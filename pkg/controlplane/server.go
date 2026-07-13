package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/sync/errgroup"

	"github.com/embervm/embervm/pkg/chunkstore"
	"github.com/embervm/embervm/pkg/guestapi"
	"github.com/embervm/embervm/pkg/lifecycle"
	"github.com/embervm/embervm/pkg/metrics"
	"github.com/embervm/embervm/pkg/nodeagent"
	"github.com/embervm/embervm/pkg/nodeapi"
	"github.com/embervm/embervm/pkg/webui"
)

// Server is the REST control plane: it persists state in Store and drives the
// node through Agent.
type Server struct {
	store    *Store
	registry *Registry
	sched    *Scheduler
	tokens   *TokenStore
	l1       chunkstore.ListingBackend // warm object store (nil: reports/tiers degrade)
	cold     chunkstore.ListingBackend // cold object store

	// Long-lived guest-proxy transports, one per in-proc (GuestDialer) node:
	// a per-request Transport leaks its keep-alive connections and readLoop
	// goroutines (see nodeapi.NewGuestTransport). Static membership bounds
	// the map.
	gtMu            sync.Mutex
	guestTransports map[string]*http.Transport

	// Guest-health probe cache: the console polls every open workspace at
	// ~2.5s, and multiple tabs multiply that; a short TTL keeps the guest
	// probe rate bounded (each probe also bumps guestd's seq counter).
	hcMu    sync.Mutex
	hcCache map[string]cachedHealth

	// Proxy sessions: iframes and new-tab navigations into the guest proxy
	// cannot attach Authorization headers, so the console mints an HttpOnly
	// cookie session (POST /v0/proxy-session) honored ONLY on proxy routes.
	psMu          sync.Mutex
	proxySessions map[string]proxySession
}

type proxySession struct {
	info    TokenInfo
	expires time.Time
}

const (
	proxySessionCookie = "embervm_proxy"
	proxySessionTTL    = 8 * time.Hour
)

// cachedHealth is one sandbox's last successful guest-health probe.
type cachedHealth struct {
	at time.Time
	h  *guestapi.HealthResponse
}

// healthCacheTTL is deliberately ≤ the console's poll interval.
const healthCacheTTL = 2 * time.Second

// LocalNodeID names the implicit node of a single-agent deployment.
const LocalNodeID = "local"

// NewServer wires a single-node control plane (the M1-M3 shape): one agent
// registered as node "local". l1/cold may be nil: storage reports and
// selective restore then answer 503 for tiers they cannot see.
func NewServer(store *Store, agent nodeapi.Agent, tokens *TokenStore, l1, cold chunkstore.ListingBackend) *Server {
	registry := NewRegistry(map[string]nodeapi.Agent{LocalNodeID: agent})
	sched := NewScheduler(store, registry, SchedulerConfig{})
	// Single-node deployments have no separate cluster bootstrap: register
	// the implicit local node here (unlimited capacity, no liveness poll —
	// the node IS the process serving this request).
	if err := sched.RegisterNodes(context.Background(),
		map[string]string{LocalNodeID: ""}, map[string]int{LocalNodeID: 0}); err != nil {
		log.Printf("controlplane: register local node: %v", err)
	}
	return NewClusterServer(store, registry, sched, tokens, l1, cold)
}

// NewClusterServer wires a multi-node control plane (M4): agents resolve
// through the registry and placement goes through the scheduler. The caller
// runs sched.RegisterNodes + sched.Run.
func NewClusterServer(store *Store, registry *Registry, sched *Scheduler, tokens *TokenStore, l1, cold chunkstore.ListingBackend) *Server {
	return &Server{store: store, registry: registry, sched: sched, tokens: tokens, l1: l1, cold: cold,
		guestTransports: map[string]*http.Transport{}, hcCache: map[string]cachedHealth{},
		proxySessions: map[string]proxySession{}}
}

// guestTransportFor returns the shared proxy transport for one node's
// GuestDialer, creating it on first use.
func (s *Server) guestTransportFor(nodeID string, g nodeapi.GuestDialer) *http.Transport {
	s.gtMu.Lock()
	defer s.gtMu.Unlock()
	if t, ok := s.guestTransports[nodeID]; ok {
		return t
	}
	t := nodeapi.NewGuestTransport(g.DialGuest)
	s.guestTransports[nodeID] = t
	return t
}

// AgentResolver adapts the registry for the lifecycle engine (its TierAgent
// verbs are a subset of nodeapi.Agent).
func (s *Server) AgentResolver() AgentResolver {
	return func(nodeID string) (TierAgent, error) { return s.agentByID(nodeID) }
}

// CanFit exposes the scheduler's growth admission for the engine's
// autoscale loop (M6).
func (s *Server) CanFit(ctx context.Context, nodeID string, memDeltaMiB, vcpuDelta int) error {
	return s.sched.CanFit(ctx, nodeID, memDeltaMiB, vcpuDelta)
}

// agentByID resolves a node id, defaulting empty to the local node.
func (s *Server) agentByID(nodeID string) (nodeapi.Agent, error) {
	if nodeID == "" {
		nodeID = LocalNodeID
	}
	return s.registry.Agent(nodeID)
}

// agentOf resolves the agent hosting a sandbox, 503ing the request when the
// node is unknown.
func (s *Server) agentOf(c *gin.Context, sb Sandbox) (nodeapi.Agent, bool) {
	a, err := s.agentByID(sb.NodeID)
	if err != nil {
		abortErr(c, http.StatusServiceUnavailable, err)
		return nil, false
	}
	return a, true
}

// Request-body caps: a single request must not be able to pin unbounded
// memory. Handlers buffer bodies (JSON binds, writeFile's io.ReadAll), so
// the cap is the backstop. The guest proxy is exempt — it streams.
const (
	maxJSONBody = 32 << 20 // exec stdin (base64) is the largest JSON payload
	maxFileBody = 1 << 30  // PUT /files; matches the artifacts-tar cap
)

// limitBodies is Gin middleware applying the per-route body cap.
func limitBodies() gin.HandlerFunc {
	return func(c *gin.Context) {
		route := c.FullPath()
		// /term is exempt like /proxy/: both hijack the connection for
		// streaming, so a body cap is meaningless there.
		if strings.Contains(route, "/proxy/") || strings.HasSuffix(route, "/term") || c.Request.Body == nil {
			c.Next()
			return
		}
		limit := int64(maxJSONBody)
		if c.Request.Method == http.MethodPut && strings.HasSuffix(route, "/files") {
			limit = maxFileBody
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
		c.Next()
	}
}

// Handler builds the Gin router (mounted at /v0, plus an unauthenticated
// /healthz).
func (s *Server) Handler() http.Handler {
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	r.GET("/metrics", gin.WrapH(metrics.Handler()))

	v0 := r.Group("/v0", s.tokens.Auth(s.proxyCookieAuth), limitBodies())
	v0.POST("/proxy-session", s.createProxySession)
	v0.DELETE("/proxy-session", s.deleteProxySession)
	v0.GET("/events", s.listEvents)
	v0.POST("/templates", s.createTemplate)
	v0.GET("/templates", s.listTemplates)
	v0.GET("/templates/:id", s.getTemplate)
	v0.DELETE("/templates/:id", s.deleteTemplate)

	v0.POST("/sandboxes", s.createSandbox)
	v0.GET("/sandboxes", s.listSandboxes)
	v0.GET("/sandboxes/:id", s.getSandbox)
	v0.POST("/sandboxes/:id/pause", s.pauseSandbox)
	v0.POST("/sandboxes/:id/resume", s.resumeSandbox)
	v0.POST("/sandboxes/:id/snapshot", s.snapshotSandbox)
	v0.POST("/sandboxes/:id/resize", s.resizeSandbox)
	v0.POST("/sandboxes/:id/migrate", s.migrateSandbox)
	v0.DELETE("/sandboxes/:id", s.killSandbox)

	// M5 fork/branch/rollback (ADR-0006).
	v0.POST("/sandboxes/:id/checkpoints", s.createCheckpoint)
	v0.GET("/sandboxes/:id/checkpoints", s.listCheckpoints)
	v0.POST("/sandboxes/:id/fork", s.forkSandbox)
	v0.POST("/sandboxes/:id/rollback", s.rollbackSandbox)

	v0.GET("/sandboxes/:id/storage", s.sandboxStorage)
	v0.GET("/storage-report", s.storageReportAll)
	v0.GET("/nodes", s.listNodes)
	v0.POST("/sandboxes/:id/restore-artifacts", s.restoreArtifacts)

	v0.Any("/sandboxes/:id/proxy/:port/*path", s.proxyGuest)

	v0.POST("/sandboxes/:id/exec", s.execSandbox)
	v0.GET("/sandboxes/:id/files", s.readFile)
	v0.PUT("/sandboxes/:id/files", s.writeFile)
	v0.GET("/sandboxes/:id/health", s.sandboxHealth)
	v0.GET("/sandboxes/:id/term", s.termSandbox)
	v0.GET("/sandboxes/:id/events", s.sandboxEvents)

	// Everything that is not an API route is the embedded console SPA.
	// /v0 misses stay JSON errors — a client-routed HTML page answering an
	// API typo would be a debugging trap.
	console := webui.Handler()
	r.NoRoute(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/v0/") {
			abortErr(c, http.StatusNotFound, ErrNotFound)
			return
		}
		console.ServeHTTP(c.Writer, c.Request)
	})
	return r
}

func abortErr(c *gin.Context, status int, err error) {
	c.AbortWithStatusJSON(status, gin.H{"error": err.Error()})
}

// ownedSandbox fetches a sandbox and verifies the authenticated caller owns
// it. A missing sandbox and one owned by another tenant both return 404 (not
// 403) so callers cannot probe for the existence of other tenants' sandbox
// IDs. Returns ok=false after writing the response.
func (s *Server) ownedSandbox(c *gin.Context, id string) (Sandbox, bool) {
	sb, err := s.store.GetSandbox(c, id)
	if err != nil {
		abortErr(c, storeStatus(err), err)
		return Sandbox{}, false
	}
	if sb.Owner != tokenInfo(c).Owner {
		abortErr(c, http.StatusNotFound, ErrNotFound)
		return Sandbox{}, false
	}
	return sb, true
}

// stopOrphanedVM compensates the "acted on the node, then the record write
// failed" ordering: a VM the node successfully created (create/fork/restore-
// artifacts) whose control-plane row could not be updated would otherwise run
// as an invisible orphan — memory the scheduler cannot see, quota the owner
// cannot free. Best-effort, on a cancellation-immune context: the client may
// already be gone.
func (s *Server) stopOrphanedVM(c *gin.Context, agent nodeapi.Agent, id, verb string, cause error) {
	ctx := context.WithoutCancel(c.Request.Context())
	if err := agent.StopSandbox(ctx, id); err != nil {
		log.Printf("controlplane: %s %s: record write failed (%v) and the compensating stop also failed: %v — VM may be orphaned on its node", verb, id, cause, err)
	} else {
		log.Printf("controlplane: %s %s: record write failed (%v); stopped the just-created VM", verb, id, cause)
	}
	_ = s.store.SetSandboxState(ctx, id, string(lifecycle.StatePending), string(lifecycle.StateFailed), "", cause.Error())
}

// storeStatus maps store errors to HTTP status.
func storeStatus(err error) int {
	if errors.Is(err, ErrNotFound) {
		return http.StatusNotFound
	}
	if errors.Is(err, ErrConflict) {
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

// --- templates --------------------------------------------------------------

func (s *Server) createTemplate(c *gin.Context) {
	var body struct {
		Name  string `json:"name"`
		Image string `json:"image"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		abortErr(c, http.StatusBadRequest, err)
		return
	}
	if body.Name == "" || body.Image == "" {
		abortErr(c, http.StatusBadRequest, errors.New("name and image are required"))
		return
	}
	id := uuid.NewString()
	tpl, err := s.store.CreateTemplate(c, id, body.Name, body.Image)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	// Builds synchronously on one node; the template ships to every other
	// node as a zfs stream via L1 (GUID lineage) and is received on demand.
	// Every early exit must set ERROR: a row stuck BUILDING squats its
	// unique name forever.
	buildNode, err := s.sched.Place(c, "", 0, 0)
	if err != nil {
		_ = s.store.SetTemplateState(c, id, "ERROR", err.Error())
		abortErr(c, http.StatusServiceUnavailable, err)
		return
	}
	agent, err := s.agentByID(buildNode)
	if err != nil {
		_ = s.store.SetTemplateState(c, id, "ERROR", err.Error())
		abortErr(c, http.StatusServiceUnavailable, err)
		return
	}
	if err := agent.BuildTemplate(c, id, body.Image); err != nil {
		_ = s.store.SetTemplateState(c, id, "ERROR", err.Error())
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	if err := s.store.SetTemplateState(c, id, "READY", ""); err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	tpl.State = "READY"
	c.JSON(http.StatusCreated, tpl)
}

func (s *Server) listTemplates(c *gin.Context) {
	list, err := s.store.ListTemplates(c)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	c.JSON(http.StatusOK, list)
}

func (s *Server) getTemplate(c *gin.Context) {
	tpl, err := s.store.GetTemplate(c, c.Param("id"))
	if err != nil {
		abortErr(c, storeStatus(err), err)
		return
	}
	c.JSON(http.StatusOK, tpl)
}

func (s *Server) deleteTemplate(c *gin.Context) {
	if err := s.store.DeleteTemplate(c, c.Param("id")); err != nil {
		abortErr(c, storeStatus(err), err)
		return
	}
	c.Status(http.StatusNoContent)
}

// --- sandboxes --------------------------------------------------------------

func (s *Server) createSandbox(c *gin.Context) {
	owner := tokenInfo(c)
	var body struct {
		TemplateID  string `json:"template_id"`
		VCPUs       int    `json:"vcpus"`
		MemoryMiB   int    `json:"memory_mib"`
		DataDiskGiB int    `json:"data_disk_gib"`
		// ArtifactPaths are preserved when the sandbox is RECYCLED
		// (M3 selective restore); empty keeps nothing.
		ArtifactPaths []string `json:"artifact_paths"`
		Egress        string   `json:"egress"`
		// M6 runtime-resize ceilings; 0 = fixed geometry. Setting a ceiling
		// requires the corresponding base field (the node's defaults are not
		// visible here, so a bound against an unknown base is meaningless).
		MaxMemoryMiB int `json:"max_memory_mib"`
		MaxVCPUs     int `json:"max_vcpus"`
		// Autoscale opts into the engine's pressure-driven resize loop
		// within [memory_mib, max_memory_mib] / [vcpus, max_vcpus].
		Autoscale bool `json:"autoscale"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		abortErr(c, http.StatusBadRequest, err)
		return
	}
	if body.TemplateID == "" {
		abortErr(c, http.StatusBadRequest, errors.New("template_id is required"))
		return
	}
	if err := validateGeometry(body.VCPUs, body.MemoryMiB, body.DataDiskGiB); err != nil {
		abortErr(c, http.StatusBadRequest, err)
		return
	}
	if err := validateCeilings(body.VCPUs, body.MemoryMiB, body.MaxVCPUs, body.MaxMemoryMiB); err != nil {
		abortErr(c, http.StatusBadRequest, err)
		return
	}
	if body.Autoscale && body.MaxMemoryMiB == 0 && body.MaxVCPUs == 0 {
		abortErr(c, http.StatusBadRequest, errors.New("autoscale requires max_memory_mib and/or max_vcpus"))
		return
	}
	// Mirror the node's slot rounding so the stored ceiling and the VM's
	// real hotplug region agree by construction.
	if body.MaxMemoryMiB > body.MemoryMiB {
		body.MaxMemoryMiB = body.MemoryMiB + roundUpToSlot(body.MaxMemoryMiB-body.MemoryMiB)
	}
	tpl, err := s.store.GetTemplate(c, body.TemplateID)
	if err != nil {
		abortErr(c, storeStatus(err), err)
		return
	}
	if tpl.State != "READY" {
		// A BUILDING/ERROR template would fail deep inside the node agent
		// (and can orphan a half-created sandbox); refuse cleanly up front.
		abortErr(c, http.StatusConflict, fmt.Errorf("template %s is %s, want READY", tpl.ID, tpl.State))
		return
	}

	// Quota: an owner's active sandboxes must stay under max_sandboxes.
	active, err := s.store.CountActiveSandboxes(c, owner.Owner)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	if owner.MaxSandboxes > 0 && active >= owner.MaxSandboxes {
		abortErr(c, http.StatusTooManyRequests,
			errors.New("sandbox quota exceeded ("+strconv.Itoa(owner.MaxSandboxes)+")"))
		return
	}

	id := uuid.NewString()
	sb, err := s.store.CreateSandbox(c, Sandbox{
		ID: id, TemplateID: body.TemplateID, State: string(lifecycle.StatePending),
		VCPUs: body.VCPUs, MemoryMiB: body.MemoryMiB, DataDiskGiB: body.DataDiskGiB, Owner: owner.Owner,
		ArtifactPaths: body.ArtifactPaths,
		MaxMemoryMiB:  body.MaxMemoryMiB, MaxVCPUs: body.MaxVCPUs,
		BaseMemoryMiB: body.MemoryMiB, BaseVCPUs: body.VCPUs, Autoscale: body.Autoscale,
	})
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}

	nodeID, err := s.sched.Place(c, "", body.MemoryMiB, body.VCPUs)
	if err != nil {
		_ = s.store.SetSandboxState(c, id, string(lifecycle.StatePending), string(lifecycle.StateFailed), "", err.Error())
		abortErr(c, http.StatusServiceUnavailable, err)
		return
	}
	agent, err := s.agentByID(nodeID)
	if err != nil {
		// FAILED, not a stuck PENDING row that burns quota forever.
		_ = s.store.SetSandboxState(c, id, string(lifecycle.StatePending), string(lifecycle.StateFailed), "", err.Error())
		abortErr(c, http.StatusServiceUnavailable, err)
		return
	}
	if err := s.store.SetSandboxNode(c, id, nodeID); err != nil {
		_ = s.store.SetSandboxState(c, id, string(lifecycle.StatePending), string(lifecycle.StateFailed), "", err.Error())
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	sb.NodeID = nodeID
	st, err := agent.CreateSandbox(c, nodeapi.CreateSandboxRequest{
		SandboxID: id, TemplateID: body.TemplateID,
		VCPUs: body.VCPUs, MemoryMiB: body.MemoryMiB, DataDiskGiB: body.DataDiskGiB,
		Egress:       body.Egress,
		MaxMemoryMiB: body.MaxMemoryMiB, MaxVCPUs: body.MaxVCPUs,
	})
	if err != nil {
		_ = s.store.SetSandboxState(c, id, string(lifecycle.StatePending), string(lifecycle.StateFailed), "", err.Error())
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	if err := s.store.SetSandboxState(c, id, string(lifecycle.StatePending), st.State, st.Netns, ""); err != nil {
		// The VM is live but the record write failed: stop the orphan.
		s.stopOrphanedVM(c, agent, id, "create", err)
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	sb.State, sb.Netns = st.State, st.Netns
	c.JSON(http.StatusCreated, sb)
}

// validateGeometry bounds sandbox resources. Zero means "default" (the node
// agent fills them); negatives would corrupt the scheduler's budgets
// (SUM(memory_mib) usage, freeMem comparisons) and the caps keep one request
// from absorbing a whole node.
func validateGeometry(vcpus, memoryMiB, dataDiskGiB int) error {
	switch {
	case vcpus < 0 || vcpus > 64:
		return fmt.Errorf("vcpus %d out of range [0,64]", vcpus)
	case memoryMiB < 0 || memoryMiB > 1<<20:
		return fmt.Errorf("memory_mib %d out of range [0,%d]", memoryMiB, 1<<20)
	case dataDiskGiB < 0 || dataDiskGiB > 4096:
		return fmt.Errorf("data_disk_gib %d out of range [0,4096]", dataDiskGiB)
	}
	return nil
}

// validateCeilings bounds the M6 resize ceilings. A ceiling needs an
// explicit base: the node fills zero bases with defaults the control plane
// cannot see, and "max ≥ an unknown base" is not a checkable contract.
func validateCeilings(vcpus, memoryMiB, maxVCPUs, maxMemoryMiB int) error {
	switch {
	case maxMemoryMiB != 0 && memoryMiB == 0:
		return errors.New("max_memory_mib requires an explicit memory_mib")
	case maxMemoryMiB != 0 && (maxMemoryMiB < memoryMiB || maxMemoryMiB > 1<<20):
		return fmt.Errorf("max_memory_mib %d out of range [memory_mib=%d,%d]", maxMemoryMiB, memoryMiB, 1<<20)
	case maxVCPUs != 0 && vcpus == 0:
		return errors.New("max_vcpus requires an explicit vcpus")
	case maxVCPUs != 0 && (maxVCPUs < vcpus || maxVCPUs > 64):
		return fmt.Errorf("max_vcpus %d out of range [vcpus=%d,64]", maxVCPUs, vcpus)
	}
	return nil
}

// hotplugSlotMiB mirrors the node's virtio-mem slot granularity
// (pkg/nodeagent); the control plane rounds create-time ceilings the same
// way so the stored bound equals the VM's real one.
const hotplugSlotMiB = 128

func roundUpToSlot(n int) int {
	return (n + hotplugSlotMiB - 1) / hotplugSlotMiB * hotplugSlotMiB
}

func (s *Server) listSandboxes(c *gin.Context) {
	// Scope to the authenticated owner so tenants never see each other's
	// sandboxes.
	list, err := s.store.ListSandboxes(c, tokenInfo(c).Owner, c.Query("state"))
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	c.JSON(http.StatusOK, list)
}

func (s *Server) getSandbox(c *gin.Context) {
	sb, ok := s.ownedSandbox(c, c.Param("id"))
	if !ok {
		return
	}
	c.JSON(http.StatusOK, sb)
}

func (s *Server) pauseSandbox(c *gin.Context) {
	id := c.Param("id")
	sb, ok := s.ownedSandbox(c, id)
	if !ok {
		return
	}
	agent, ok := s.agentOf(c, sb)
	if !ok {
		return
	}
	if err := agent.PauseSandbox(c, id); err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	if err := s.store.SetSandboxState(c, id, sb.State, string(lifecycle.StatePausedHot), "", ""); err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	sb.State = string(lifecycle.StatePausedHot)
	c.JSON(http.StatusOK, sb)
}

// resizeSandbox retargets a RUNNING sandbox's effective geometry within its
// create-time ceilings (M6): memory via virtio-mem, CPU via the cgroup
// quota. Growth is admission-checked against the node's oversold budget and
// rejected with 409 when it does not fit (ADR-0007: no auto-migration — the
// error tells the caller their options).
func (s *Server) resizeSandbox(c *gin.Context) {
	start := time.Now()
	id := c.Param("id")
	sb, ok := s.ownedSandbox(c, id)
	if !ok {
		return
	}
	var body struct {
		MemoryMiB int `json:"memory_mib"` // 0 = unchanged
		VCPUs     int `json:"vcpus"`      // 0 = unchanged
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		abortErr(c, http.StatusBadRequest, err)
		return
	}
	if body.MemoryMiB == 0 && body.VCPUs == 0 {
		abortErr(c, http.StatusBadRequest, errors.New("nothing to resize: set memory_mib and/or vcpus"))
		return
	}
	if sb.State != string(lifecycle.StateRunning) {
		abortErr(c, http.StatusConflict, fmt.Errorf("sandbox is %s, want RUNNING", sb.State))
		return
	}
	// Ceiling bounds. The boot base lives on the node (this row's
	// memory_mib moves with resize), so below-base is the node's rejection.
	if body.MemoryMiB != 0 && (sb.MaxMemoryMiB == 0 || body.MemoryMiB < 1 || body.MemoryMiB > sb.MaxMemoryMiB) {
		abortErr(c, http.StatusConflict, fmt.Errorf(
			"memory_mib %d outside resize ceiling %d (create the sandbox with max_memory_mib to enable memory resize)",
			body.MemoryMiB, sb.MaxMemoryMiB))
		return
	}
	if body.VCPUs != 0 && (sb.MaxVCPUs == 0 || body.VCPUs < 1 || body.VCPUs > sb.MaxVCPUs) {
		abortErr(c, http.StatusConflict, fmt.Errorf(
			"vcpus %d outside resize ceiling %d (create the sandbox with max_vcpus to enable CPU resize)",
			body.VCPUs, sb.MaxVCPUs))
		return
	}
	// Growth must fit the node's oversold budget (zero targets yield
	// negative deltas, which always fit).
	if err := s.sched.CanFit(c, sb.NodeID, body.MemoryMiB-sb.MemoryMiB, body.VCPUs-sb.VCPUs); err != nil {
		if errors.Is(err, ErrNoCapacity) {
			metrics.ResizeTotal.WithLabelValues("no_capacity").Inc()
			abortErr(c, http.StatusConflict, fmt.Errorf(
				"%v — pause/resume re-places the sandbox on a roomier node, or POST /v0/sandboxes/%s/migrate", err, id))
		} else {
			abortErr(c, http.StatusInternalServerError, err)
		}
		return
	}
	agent, ok := s.agentOf(c, sb)
	if !ok {
		return
	}
	res, err := agent.ResizeSandbox(c, id, nodeapi.ResizeRequest{MemoryMiB: body.MemoryMiB, VCPUs: body.VCPUs})
	if err != nil {
		// The node may have moved partway (memory grown, cpu.max failed);
		// reconcile accounting from the node's view before erroring.
		if st, serr := agent.Status(c, id); serr == nil && st.MemoryMiB > 0 {
			_ = s.store.UpdateSandboxGeometry(c, id, st.VCPUs, st.MemoryMiB)
		}
		metrics.ResizeTotal.WithLabelValues("error").Inc()
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	if err := s.store.UpdateSandboxGeometry(c, id, res.VCPUs, res.MemoryMiB); err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	sb.VCPUs, sb.MemoryMiB = res.VCPUs, res.MemoryMiB
	metrics.ResizeTotal.WithLabelValues("ok").Inc()
	metrics.ResizeSeconds.Observe(time.Since(start).Seconds())
	c.JSON(http.StatusOK, sb)
}

func (s *Server) resumeSandbox(c *gin.Context) {
	id := c.Param("id")
	sb, ok := s.ownedSandbox(c, id)
	if !ok {
		return
	}
	// Claim the resume first (CAS via the state machine): a lifecycle-engine
	// demotion racing us loses cleanly on the from-state.
	if err := lifecycle.Validate(lifecycle.State(sb.State), lifecycle.StateResuming); err != nil {
		abortErr(c, http.StatusConflict, err)
		return
	}
	if err := s.store.TransitionSandbox(c, id, sb.State, string(lifecycle.StateResuming), ""); err != nil {
		abortErr(c, storeStatus(err), err)
		return
	}
	var st nodeapi.SandboxStatus
	var err error
	switch lifecycle.State(sb.State) {
	case lifecycle.StatePausedWarm, lifecycle.StateArchivedCold, lifecycle.StateFailed:
		// Tiered (or failed-node) restores can land anywhere: sticky to the
		// previous node, else bin-packed. FAILED here means "node died with
		// it" — its last write-through snapshot restores from L1.
		tier := "warm"
		if lifecycle.State(sb.State) == lifecycle.StateArchivedCold {
			tier = "cold"
		}
		var nodeID string
		nodeID, err = s.sched.Place(c, sb.NodeID, sb.MemoryMiB, sb.VCPUs)
		if err != nil {
			_ = s.store.SetSandboxState(c, id, string(lifecycle.StateResuming), string(lifecycle.StateFailed), "", err.Error())
			abortErr(c, http.StatusServiceUnavailable, err)
			return
		}
		var agent nodeapi.Agent
		agent, err = s.agentByID(nodeID)
		if err == nil {
			if err = s.store.SetSandboxNode(c, id, nodeID); err == nil {
				sb.NodeID = nodeID
				st, err = agent.RestoreSandbox(c, id, tier)
			}
		}
	default:
		// Hot resume runs where the local state lives.
		var agent nodeapi.Agent
		agent, err = s.agentByID(sb.NodeID)
		if err == nil {
			st, err = agent.ResumeSandbox(c, id)
		}
	}
	if err != nil {
		_ = s.store.SetSandboxState(c, id, string(lifecycle.StateResuming), string(lifecycle.StateFailed), "", err.Error())
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	if err := s.store.SetSandboxState(c, id, string(lifecycle.StateResuming), st.State, st.Netns, ""); err != nil {
		// The VM resumed but the record write failed. Unlike a fresh create,
		// stopping would discard user state — pause it back instead (the
		// write-through makes it durable again) and mark the row FAILED so
		// the resume path can recover it.
		bg := context.WithoutCancel(c.Request.Context())
		if agent, aerr := s.agentByID(sb.NodeID); aerr == nil {
			if perr := agent.PauseSandbox(bg, id); perr != nil {
				log.Printf("controlplane: resume %s: record write failed (%v) and the compensating pause also failed: %v — VM may be orphaned on its node", id, err, perr)
			}
		}
		_ = s.store.SetSandboxState(bg, id, string(lifecycle.StateResuming), string(lifecycle.StateFailed), "", err.Error())
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	sb.State = st.State
	// A restore rewinds the guest to checkpoint-time state, including the
	// virtio-mem plugged size — reconcile the accounting row with the
	// node's post-restore view (M6). Zero means an old node build that does
	// not report geometry; leave the row alone then.
	if st.MemoryMiB > 0 && (st.MemoryMiB != sb.MemoryMiB || st.VCPUs != sb.VCPUs) {
		if err := s.store.UpdateSandboxGeometry(c, id, st.VCPUs, st.MemoryMiB); err != nil {
			log.Printf("controlplane: resume %s: reconcile geometry to %d MiB/%d vcpus: %v", id, st.MemoryMiB, st.VCPUs, err)
		} else {
			sb.MemoryMiB, sb.VCPUs = st.MemoryMiB, st.VCPUs
		}
	}
	c.JSON(http.StatusOK, sb)
}

// listNodes reports the cluster's workers with their live usage — the
// console's fleet view. Nodes are cluster-wide facts, not tenant-scoped
// resources, but the route still sits behind token auth like all of /v0.
func (s *Server) listNodes(c *gin.Context) {
	nodes, err := s.store.ListNodes(c)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	usage, err := s.store.NodeUsage(c)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	type nodeView struct {
		Node
		UsedMiB   int `json:"used_mib"`
		UsedVCPUs int `json:"used_vcpus"`
		Active    int `json:"active_sandboxes"`
	}
	out := make([]nodeView, 0, len(nodes))
	for _, n := range nodes {
		u := usage[n.ID]
		out = append(out, nodeView{Node: n, UsedMiB: u.MemMiB, UsedVCPUs: u.VCPUs, Active: u.Active})
	}
	c.JSON(http.StatusOK, out)
}

// migrateSandbox moves a sandbox to another node (M6): pause (chunked
// write-through) → release local state → warm-restore on the target — the
// same machinery the M2/M4 cross-node recovery paths use, packaged as an
// explicit verb. A RUNNING sandbox ends RUNNING on the target (seconds-long
// pause in between); a PAUSED_HOT one just releases local state and moves
// its placement pointer (PAUSED_WARM — the next resume lands on the target).
func (s *Server) migrateSandbox(c *gin.Context) {
	id := c.Param("id")
	sb, ok := s.ownedSandbox(c, id)
	if !ok {
		return
	}
	var body struct {
		NodeID string `json:"node_id"` // empty: bin-pack anywhere but here
	}
	_ = c.ShouldBindJSON(&body)

	migErr := func(status int, err error) {
		metrics.Migrations.WithLabelValues("error").Inc()
		abortErr(c, status, err)
	}

	// Pick and admission-check the target before touching the sandbox.
	target := body.NodeID
	if target == "" {
		var err error
		if target, err = s.sched.PlaceExcluding(c, sb.NodeID, sb.MemoryMiB, sb.VCPUs); err != nil {
			migErr(http.StatusServiceUnavailable, err)
			return
		}
	} else {
		if target == sb.NodeID {
			migErr(http.StatusConflict, fmt.Errorf("sandbox already lives on node %s", target))
			return
		}
		if err := s.sched.CanFit(c, target, sb.MemoryMiB, sb.VCPUs); err != nil {
			migErr(http.StatusConflict, err)
			return
		}
	}
	oldAgent, err := s.agentByID(sb.NodeID)
	if err != nil {
		migErr(http.StatusServiceUnavailable, err)
		return
	}

	// A RUNNING sandbox pauses first (its own claim edge, CAS vs the
	// engine); PAUSED_HOT starts at the release step.
	state := lifecycle.State(sb.State)
	switch state {
	case lifecycle.StateRunning:
		if err := s.store.TransitionSandbox(c, id, sb.State, string(lifecycle.StatePausing), ""); err != nil {
			migErr(storeStatus(err), err)
			return
		}
		if err := oldAgent.PauseSandbox(c, id); err != nil {
			// The node's abortPause resumed the VM in place — mirror it.
			_ = s.store.SetSandboxState(c, id, string(lifecycle.StatePausing), string(lifecycle.StateRunning), "", "")
			migErr(http.StatusInternalServerError, err)
			return
		}
		if err := s.store.SetSandboxState(c, id, string(lifecycle.StatePausing), string(lifecycle.StatePausedHot), "", ""); err != nil {
			migErr(http.StatusInternalServerError, err)
			return
		}
	case lifecycle.StatePausedHot:
	default:
		migErr(http.StatusConflict, fmt.Errorf("migrate requires RUNNING or PAUSED_HOT (state %s)", sb.State))
		return
	}

	// Destructive-to-source discipline (same as the engine's HOT→WARM):
	// CAS first, release second, FAILED loudly if the release breaks.
	if err := s.store.TransitionSandbox(c, id, string(lifecycle.StatePausedHot), string(lifecycle.StatePausedWarm), ""); err != nil {
		migErr(storeStatus(err), err)
		return
	}
	if err := oldAgent.ReleaseLocal(c, id); err != nil {
		_ = s.store.SetSandboxState(c, id, string(lifecycle.StatePausedWarm), string(lifecycle.StateFailed), "", err.Error())
		migErr(http.StatusInternalServerError, err)
		return
	}
	if err := s.store.SetSandboxNode(c, id, target); err != nil {
		migErr(http.StatusInternalServerError, err)
		return
	}
	sb.NodeID = target
	sb.State = string(lifecycle.StatePausedWarm)

	// A paused sandbox is migrated at this point: it lives in L1 with its
	// placement pointing at the target.
	if state == lifecycle.StatePausedHot {
		metrics.Migrations.WithLabelValues("ok").Inc()
		c.JSON(http.StatusOK, sb)
		return
	}

	// A running one restores on the target now.
	newAgent, err := s.agentByID(target)
	if err != nil {
		migErr(http.StatusServiceUnavailable, err)
		return
	}
	if err := s.store.TransitionSandbox(c, id, string(lifecycle.StatePausedWarm), string(lifecycle.StateResuming), ""); err != nil {
		migErr(storeStatus(err), err)
		return
	}
	st, err := newAgent.RestoreSandbox(c, id, "warm")
	if err != nil {
		// Restorable from L1 on demand — the standard recovery path.
		_ = s.store.SetSandboxState(c, id, string(lifecycle.StateResuming), string(lifecycle.StateFailed), "", err.Error())
		migErr(http.StatusInternalServerError, err)
		return
	}
	if err := s.store.SetSandboxState(c, id, string(lifecycle.StateResuming), st.State, st.Netns, ""); err != nil {
		migErr(http.StatusInternalServerError, err)
		return
	}
	sb.State, sb.Netns = st.State, st.Netns
	if st.MemoryMiB > 0 && (st.MemoryMiB != sb.MemoryMiB || st.VCPUs != sb.VCPUs) {
		if err := s.store.UpdateSandboxGeometry(c, id, st.VCPUs, st.MemoryMiB); err == nil {
			sb.MemoryMiB, sb.VCPUs = st.MemoryMiB, st.VCPUs
		}
	}
	metrics.Migrations.WithLabelValues("ok").Inc()
	c.JSON(http.StatusOK, sb)
}

// --- M5 fork/branch/rollback (ADR-0006) --------------------------------------

// tagRE constrains checkpoint tags to safe dataset/path components.
var tagRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// checkpointNow drives the snapshot verb (pause → diff layer + zfs snapshot
// → resume) and records the layer it produced. An empty tag names itself
// cp<seq> after the fact.
func (s *Server) checkpointNow(c *gin.Context, sb Sandbox, tag string) (Checkpoint, error) {
	agent, err := s.agentByID(sb.NodeID)
	if err != nil {
		return Checkpoint{}, err
	}
	agentTag := tag
	if agentTag == "" {
		agentTag = "cp"
	}
	snapID, err := agent.SnapshotSandbox(c, sb.ID, agentTag)
	if err != nil {
		return Checkpoint{}, err
	}
	// Producer-defined return format: "<id>@<tag>-<seq>".
	seqStr := snapID[strings.LastIndex(snapID, "-")+1:]
	seq, err := strconv.Atoi(seqStr)
	if err != nil {
		return Checkpoint{}, fmt.Errorf("unparseable snapshot id %q", snapID)
	}
	if tag == "" {
		tag = "cp" + seqStr
	}
	return s.store.InsertCheckpoint(c, sb.ID, tag, "p"+seqStr, seq)
}

func (s *Server) createCheckpoint(c *gin.Context) {
	sb, ok := s.ownedSandbox(c, c.Param("id"))
	if !ok {
		return
	}
	var body struct {
		Tag string `json:"tag"`
	}
	_ = c.ShouldBindJSON(&body) // empty body = auto tag
	if body.Tag != "" && !tagRE.MatchString(body.Tag) {
		abortErr(c, http.StatusBadRequest, errors.New("bad checkpoint tag"))
		return
	}
	cp, err := s.checkpointNow(c, sb, body.Tag)
	if err != nil {
		abortErr(c, storeStatus(err), err)
		return
	}
	c.JSON(http.StatusCreated, cp)
}

func (s *Server) listCheckpoints(c *gin.Context) {
	sb, ok := s.ownedSandbox(c, c.Param("id"))
	if !ok {
		return
	}
	cps, err := s.store.ListCheckpoints(c, sb.ID)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	c.JSON(http.StatusOK, cps)
}

// forkSandbox branches a new sandbox off a parent checkpoint (omitted
// checkpoint = checkpoint the live parent now). The child is a full
// quota-counted sandbox on the parent's node (the chain is node-local).
func (s *Server) forkSandbox(c *gin.Context) {
	parent, ok := s.ownedSandbox(c, c.Param("id"))
	if !ok {
		return
	}
	var body struct {
		Checkpoint string `json:"checkpoint"`
	}
	_ = c.ShouldBindJSON(&body)

	owner := tokenInfo(c)
	active, err := s.store.CountActiveSandboxes(c, owner.Owner)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	if owner.MaxSandboxes > 0 && active >= owner.MaxSandboxes {
		abortErr(c, http.StatusTooManyRequests,
			errors.New("sandbox quota exceeded ("+strconv.Itoa(owner.MaxSandboxes)+")"))
		return
	}
	switch lifecycle.State(parent.State) {
	case lifecycle.StateRunning, lifecycle.StatePausedHot:
	default:
		abortErr(c, http.StatusConflict,
			fmt.Errorf("fork requires a HOT parent (state %s): resume it first", parent.State))
		return
	}

	var cp Checkpoint
	if body.Checkpoint == "" {
		cp, err = s.checkpointNow(c, parent, "")
	} else {
		cp, err = s.store.GetCheckpoint(c, parent.ID, body.Checkpoint)
	}
	if err != nil {
		abortErr(c, storeStatus(err), err)
		return
	}

	id := uuid.NewString()
	child, err := s.store.CreateSandbox(c, Sandbox{
		ID: id, TemplateID: parent.TemplateID, State: string(lifecycle.StatePending),
		VCPUs: parent.VCPUs, MemoryMiB: parent.MemoryMiB, DataDiskGiB: parent.DataDiskGiB,
		MaxMemoryMiB: parent.MaxMemoryMiB, MaxVCPUs: parent.MaxVCPUs,
		BaseMemoryMiB: parent.BaseMemoryMiB, BaseVCPUs: parent.BaseVCPUs, Autoscale: parent.Autoscale,
		Owner: owner.Owner, ArtifactPaths: parent.ArtifactPaths,
		ParentID: parent.ID, ForkedFrom: cp.Tag,
	})
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	agent, err := s.agentByID(parent.NodeID)
	if err != nil {
		_ = s.store.SetSandboxState(c, id, string(lifecycle.StatePending), string(lifecycle.StateFailed), "", err.Error())
		abortErr(c, http.StatusServiceUnavailable, err)
		return
	}
	// Record the child's node before acting. A real store error must not be
	// swallowed just because the parent sits on the implicit local node —
	// the old `err != nil && parent.NodeID != ""` guard did exactly that.
	if parent.NodeID != "" {
		if err := s.store.SetSandboxNode(c, id, parent.NodeID); err != nil {
			_ = s.store.SetSandboxState(c, id, string(lifecycle.StatePending), string(lifecycle.StateFailed), "", err.Error())
			abortErr(c, http.StatusInternalServerError, err)
			return
		}
	}
	st, err := agent.Fork(c, parent.ID, cp.Layer, id)
	if err != nil {
		_ = s.store.SetSandboxState(c, id, string(lifecycle.StatePending), string(lifecycle.StateFailed), "", err.Error())
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	if err := s.store.SetSandboxState(c, id, string(lifecycle.StatePending), st.State, st.Netns, ""); err != nil {
		s.stopOrphanedVM(c, agent, id, "fork", err)
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	child.State, child.Netns, child.NodeID = st.State, st.Netns, parent.NodeID
	// The child wakes with the CHECKPOINT's effective geometry, not the
	// parent's current one (M6) — reconcile the row like resume does.
	if st.MemoryMiB > 0 && (st.MemoryMiB != child.MemoryMiB || st.VCPUs != child.VCPUs) {
		if err := s.store.UpdateSandboxGeometry(c, id, st.VCPUs, st.MemoryMiB); err == nil {
			child.MemoryMiB, child.VCPUs = st.MemoryMiB, st.VCPUs
		}
	}
	c.JSON(http.StatusCreated, child)
}

// rollbackSandbox switches the sandbox back to a checkpoint, discarding
// everything after it — refused while newer checkpoints have live forks
// (their clone base is what `zfs rollback -r` would destroy).
func (s *Server) rollbackSandbox(c *gin.Context) {
	sb, ok := s.ownedSandbox(c, c.Param("id"))
	if !ok {
		return
	}
	var body struct {
		Checkpoint string `json:"checkpoint"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Checkpoint == "" {
		abortErr(c, http.StatusBadRequest, errors.New("checkpoint is required"))
		return
	}
	cp, err := s.store.GetCheckpoint(c, sb.ID, body.Checkpoint)
	if err != nil {
		abortErr(c, storeStatus(err), err)
		return
	}
	kids, err := s.store.LiveForkChildren(c, sb.ID, cp.Seq)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	if len(kids) > 0 {
		abortErr(c, http.StatusConflict,
			fmt.Errorf("rollback would destroy checkpoints with live forks: %s", strings.Join(kids, ", ")))
		return
	}

	// Claim the sandbox (CAS): a racing engine demotion or user verb loses
	// cleanly on the from-state. The legal claim edge depends on where the
	// sandbox is.
	claim := lifecycle.StatePausing
	if lifecycle.State(sb.State) == lifecycle.StatePausedHot {
		claim = lifecycle.StateResuming
	}
	if err := lifecycle.Validate(lifecycle.State(sb.State), claim); err != nil {
		abortErr(c, http.StatusConflict, err)
		return
	}
	if err := s.store.TransitionSandbox(c, sb.ID, sb.State, string(claim), ""); err != nil {
		abortErr(c, storeStatus(err), err)
		return
	}
	agent, err := s.agentByID(sb.NodeID)
	var st nodeapi.SandboxStatus
	if err == nil {
		st, err = agent.Rollback(c, sb.ID, cp.Layer)
	}
	if err != nil {
		_ = s.store.SetSandboxState(c, sb.ID, string(claim), string(lifecycle.StateFailed), "", err.Error())
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	if err := s.store.SetSandboxState(c, sb.ID, string(claim), st.State, st.Netns, ""); err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	if tags, err := s.store.DeleteCheckpointsAfter(c, sb.ID, cp.Seq); err != nil {
		log.Printf("controlplane: prune checkpoints after %s rollback: %v", sb.ID, err)
	} else if len(tags) > 0 {
		log.Printf("controlplane: rollback %s to %s discarded checkpoints %s", sb.ID, cp.Tag, strings.Join(tags, ", "))
	}
	sb.State, sb.Netns = st.State, st.Netns
	// Rolling back rewinds the plugged size to the checkpoint's (M6).
	if st.MemoryMiB > 0 && (st.MemoryMiB != sb.MemoryMiB || st.VCPUs != sb.VCPUs) {
		if err := s.store.UpdateSandboxGeometry(c, sb.ID, st.VCPUs, st.MemoryMiB); err == nil {
			sb.MemoryMiB, sb.VCPUs = st.MemoryMiB, st.VCPUs
		}
	}
	c.JSON(http.StatusOK, sb)
}

// stripPlatformCreds removes EmberVM's own credentials from a request before
// it is forwarded into an (untrusted) guest. The browser attaches the
// proxy-session cookie to every /v0/sandboxes/* request, and a bearer token
// may ride the Authorization header; guest-controlled code must see neither
// (a leaked proxy-session cookie is a reusable owner-scoped credential). This
// mirrors the Sec-WebSocket-Protocol bearer stripping in TokenStore.Auth.
func stripPlatformCreds(h http.Header) {
	h.Del("Cookie")
	h.Del("Authorization")
}

// proxyGuest is the M4 gateway: authenticated, owner-scoped forwarding of
// any HTTP(S)/WebSocket traffic to a guest port. In-proc agents proxy
// straight into the netns; split-mode agents add a UDS hop.
func (s *Server) proxyGuest(c *gin.Context) {
	sb, ok := s.ownedSandbox(c, c.Param("id"))
	if !ok {
		return
	}
	port, err := strconv.Atoi(c.Param("port"))
	if err != nil || port <= 0 || port > 65535 {
		abortErr(c, http.StatusBadRequest, errors.New("bad port"))
		return
	}
	agent, ok := s.agentOf(c, sb)
	if !ok {
		return
	}

	var handler http.Handler
	switch g := agent.(type) {
	case nodeapi.GuestDialer: // in-proc: dial the netns directly
		nodeID := sb.NodeID
		if nodeID == "" {
			nodeID = LocalNodeID
		}
		handler = &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.URL.Scheme = "http"
				pr.Out.URL.Host = net.JoinHostPort(sb.ID, strconv.Itoa(port))
				pr.Out.URL.Path = c.Param("path")
				stripPlatformCreds(pr.Out.Header)
			},
			Transport: s.guestTransportFor(nodeID, g),
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(err.Error()))
			},
		}
	case nodeapi.GuestProxier: // split mode: hop over the node's UDS
		inner := g.GuestProxy(sb.ID, port)
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r2 := r.Clone(r.Context())
			r2.URL.Path = c.Param("path")
			stripPlatformCreds(r2.Header)
			inner.ServeHTTP(w, r2)
		})
	default:
		abortErr(c, http.StatusNotImplemented, errors.New("node does not support guest proxying"))
		return
	}
	handler.ServeHTTP(c.Writer, c.Request)
	// A hijacked (WebSocket) or never-written response reports status 0.
	label := "unknown"
	if status := c.Writer.Status(); status >= 100 {
		label = strconv.Itoa(status/100) + "xx"
	}
	metrics.ProxyRequests.WithLabelValues(label).Inc()
}

// sandboxStorage reports one sandbox's storage footprint by tier.
func (s *Server) sandboxStorage(c *gin.Context) {
	sb, ok := s.ownedSandbox(c, c.Param("id"))
	if !ok {
		return
	}
	rep, err := s.storageReport(c, sb)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	c.JSON(http.StatusOK, rep)
}

// storageReportAll aggregates the caller's sandboxes (成本报表). Each report
// costs several object-store round-trips, so reports run concurrently with
// a bound — one request over a large fleet must neither serialize into a
// multi-minute handler nor stampede the store.
func (s *Server) storageReportAll(c *gin.Context) {
	sbs, err := s.store.ListSandboxes(c, tokenInfo(c).Owner, "")
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	rows := make([]StorageReport, len(sbs))
	g, gctx := errgroup.WithContext(c)
	g.SetLimit(8)
	for i, sb := range sbs {
		g.Go(func() error {
			rep, err := s.storageReport(gctx, sb)
			if err != nil {
				return err
			}
			rows[i] = rep
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	var (
		totalLogical, totalStored   int64
		totalChunks, totalArtifacts int64
	)
	for _, rep := range rows {
		totalLogical += rep.LogicalBytes
		totalStored += rep.StoredBytes
		totalChunks += int64(rep.ChunkCount)
		totalArtifacts += rep.ArtifactBytes
	}
	c.JSON(http.StatusOK, gin.H{
		"sandboxes":            rows,
		"total_logical_bytes":  totalLogical,
		"total_stored_bytes":   totalStored,
		"total_chunks":         totalChunks,
		"total_artifact_bytes": totalArtifacts,
	})
}

// restoreArtifacts seeds a NEW sandbox from a RECYCLED one's artifacts
// (Manus-style selective restore): fresh sandbox from the same template,
// artifacts untarred into it via guestd.
func (s *Server) restoreArtifacts(c *gin.Context) {
	id := c.Param("id")
	sb, ok := s.ownedSandbox(c, id)
	if !ok {
		return
	}
	if lifecycle.State(sb.State) != lifecycle.StateRecycled {
		abortErr(c, http.StatusConflict, fmt.Errorf("sandbox %s is %s, want RECYCLED", id, sb.State))
		return
	}
	if s.cold == nil {
		abortErr(c, http.StatusServiceUnavailable, errors.New("no cold store configured"))
		return
	}
	// Pull + decompress host-side (busybox tar in the guest lacks zstd).
	tarBytes, err := readArtifactsTar(c, s.cold, id)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}

	newID := uuid.NewString()
	row, err := s.store.CreateSandbox(c, Sandbox{
		ID: newID, TemplateID: sb.TemplateID, State: string(lifecycle.StatePending),
		VCPUs: sb.VCPUs, MemoryMiB: sb.MemoryMiB, DataDiskGiB: sb.DataDiskGiB,
		Owner: sb.Owner, ArtifactPaths: sb.ArtifactPaths,
	})
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	nodeID, err := s.sched.Place(c, sb.NodeID, sb.MemoryMiB, sb.VCPUs)
	if err != nil {
		_ = s.store.SetSandboxState(c, newID, string(lifecycle.StatePending), string(lifecycle.StateFailed), "", err.Error())
		abortErr(c, http.StatusServiceUnavailable, err)
		return
	}
	agent, err := s.agentByID(nodeID)
	if err != nil {
		_ = s.store.SetSandboxState(c, newID, string(lifecycle.StatePending), string(lifecycle.StateFailed), "", err.Error())
		abortErr(c, http.StatusServiceUnavailable, err)
		return
	}
	if err := s.store.SetSandboxNode(c, newID, nodeID); err != nil {
		_ = s.store.SetSandboxState(c, newID, string(lifecycle.StatePending), string(lifecycle.StateFailed), "", err.Error())
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	st, err := agent.CreateSandbox(c, nodeapi.CreateSandboxRequest{
		SandboxID: newID, TemplateID: sb.TemplateID,
		VCPUs: sb.VCPUs, MemoryMiB: sb.MemoryMiB, DataDiskGiB: sb.DataDiskGiB,
	})
	if err != nil {
		_ = s.store.SetSandboxState(c, newID, string(lifecycle.StatePending), string(lifecycle.StateFailed), "", err.Error())
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	if err := s.store.SetSandboxState(c, newID, string(lifecycle.StatePending), st.State, st.Netns, ""); err != nil {
		s.stopOrphanedVM(c, agent, newID, "restore-artifacts", err)
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	// From here the child is live AND recorded: any failure must stop it and
	// surface its id, or the caller has a running sandbox it cannot find.
	seedFail := func(cause error) {
		bg := context.WithoutCancel(c.Request.Context())
		if serr := agent.StopSandbox(bg, newID); serr != nil {
			log.Printf("controlplane: restore-artifacts %s: seeding failed (%v) and the compensating stop also failed: %v", newID, cause, serr)
		}
		_ = s.store.SetSandboxState(bg, newID, st.State, string(lifecycle.StateFailed), "", cause.Error())
		c.AbortWithStatusJSON(http.StatusInternalServerError,
			gin.H{"error": cause.Error(), "sandbox_id": newID})
	}
	if err := agent.WriteFile(c, newID, "/tmp/artifacts.tar", 0o600, tarBytes); err != nil {
		seedFail(fmt.Errorf("write artifacts tar: %w", err))
		return
	}
	ex, err := agent.Exec(c, newID, &guestapi.ExecRequest{
		Cmd: "sh", Args: []string{"-c", "tar -xf /tmp/artifacts.tar -C / && rm /tmp/artifacts.tar"},
	})
	if err != nil || ex.ExitCode != 0 {
		seedFail(fmt.Errorf("untar artifacts: %v (exit=%d stderr=%s)", err, exitCodeOf(ex), stderrOf(ex)))
		return
	}
	row.State = st.State
	c.JSON(http.StatusOK, gin.H{"sandbox": row, "restored_from": id})
}

func exitCodeOf(ex *guestapi.ExecResponse) int {
	if ex == nil {
		return -1
	}
	return ex.ExitCode
}

func stderrOf(ex *guestapi.ExecResponse) string {
	if ex == nil {
		return ""
	}
	return string(ex.Stderr)
}

// maxArtifactsTar caps the decompressed RECYCLED remnant: the tar is
// buffered in memory and shipped through guestd, so an oversized (or
// corrupt) cold-store object must not absorb the whole process.
const maxArtifactsTar = 1 << 30

// readArtifactsTar fetches and zstd-decompresses the RECYCLED remnant.
func readArtifactsTar(ctx context.Context, cold chunkstore.Objects, id string) ([]byte, error) {
	rc, err := cold.GetObject(ctx, nodeagent.KeyArtifacts(id))
	if err != nil {
		return nil, fmt.Errorf("artifacts for %s: %w", id, err)
	}
	defer rc.Close()
	zr, err := zstd.NewReader(rc)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	data, err := io.ReadAll(io.LimitReader(zr, maxArtifactsTar+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxArtifactsTar {
		return nil, fmt.Errorf("artifacts for %s exceed %d bytes decompressed", id, maxArtifactsTar)
	}
	return data, nil
}

func (s *Server) snapshotSandbox(c *gin.Context) {
	id := c.Param("id")
	sb, ok := s.ownedSandbox(c, id)
	if !ok {
		return
	}
	var body struct {
		Tag string `json:"tag"`
	}
	_ = c.ShouldBindJSON(&body)
	if body.Tag == "" {
		body.Tag = "snap"
	}
	agent, ok := s.agentOf(c, sb)
	if !ok {
		return
	}
	snapID, err := agent.SnapshotSandbox(c, id, body.Tag)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"snapshot_id": snapID})
}

func (s *Server) killSandbox(c *gin.Context) {
	id := c.Param("id")
	sb, ok := s.ownedSandbox(c, id)
	if !ok {
		return
	}
	// Fork lineage guard (ADR-0006 D5): children are ZFS clones of this
	// dataset's snapshots — destroying under them fails at the ZFS layer,
	// so refuse loudly first.
	if kids, err := s.store.LiveForkChildren(c, id, 0); err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	} else if len(kids) > 0 {
		abortErr(c, http.StatusConflict,
			fmt.Errorf("sandbox has live forks (%s) — destroy them first", strings.Join(kids, ", ")))
		return
	}
	agent, ok := s.agentOf(c, sb)
	if !ok {
		return
	}
	if err := agent.StopSandbox(c, id); err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	if err := s.store.SetSandboxState(c, id, sb.State, string(lifecycle.StateStopped), "", ""); err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// --- guest proxy ------------------------------------------------------------

func (s *Server) execSandbox(c *gin.Context) {
	sb, ok := s.ownedSandbox(c, c.Param("id"))
	if !ok {
		return
	}
	var req struct {
		guestapi.ExecRequest
		// Checkpoint=true snapshots BEFORE running the command (M5
		// time-travel): the step's checkpoint is the state the command saw,
		// so forking it replays the step. The tag comes back in the response.
		Checkpoint bool `json:"checkpoint"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		abortErr(c, http.StatusBadRequest, err)
		return
	}
	agent, ok := s.agentOf(c, sb)
	if !ok {
		return
	}
	var cpTag string
	if req.Checkpoint {
		cp, err := s.checkpointNow(c, sb, "")
		if err != nil {
			abortErr(c, storeStatus(err), err)
			return
		}
		cpTag = cp.Tag
	}
	resp, err := agent.Exec(c, c.Param("id"), &req.ExecRequest)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	if cpTag != "" {
		c.JSON(http.StatusOK, struct {
			*guestapi.ExecResponse
			Checkpoint string `json:"checkpoint"`
		}{resp, cpTag})
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (s *Server) readFile(c *gin.Context) {
	sb, ok := s.ownedSandbox(c, c.Param("id"))
	if !ok {
		return
	}
	path := c.Query("path")
	if path == "" {
		abortErr(c, http.StatusBadRequest, errors.New("path is required"))
		return
	}
	agent, ok := s.agentOf(c, sb)
	if !ok {
		return
	}
	if c.Query("op") == "list" {
		listing, err := agent.ListDir(c, sb.ID, path)
		if err != nil {
			abortErr(c, http.StatusInternalServerError, err)
			return
		}
		c.JSON(http.StatusOK, listing)
		return
	}
	data, err := agent.ReadFile(c, sb.ID, path)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	c.Data(http.StatusOK, "application/octet-stream", data)
}

func (s *Server) writeFile(c *gin.Context) {
	sb, ok := s.ownedSandbox(c, c.Param("id"))
	if !ok {
		return
	}
	path := c.Query("path")
	if path == "" {
		abortErr(c, http.StatusBadRequest, errors.New("path is required"))
		return
	}
	mode := fs.FileMode(0o644)
	if raw := c.Query("mode"); raw != "" {
		parsed, err := strconv.ParseUint(raw, 8, 32)
		if err != nil {
			abortErr(c, http.StatusBadRequest, errors.New("mode must be octal"))
			return
		}
		// Permission bits only: no setuid/setgid/sticky smuggled into the guest.
		mode = fs.FileMode(parsed) & fs.ModePerm
	}
	data, err := io.ReadAll(c.Request.Body)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	agent, ok := s.agentOf(c, sb)
	if !ok {
		return
	}
	if err := agent.WriteFile(c, sb.ID, path, mode, data); err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// healthView is the /health response: the sandbox's stored state plus, when
// RUNNING and reachable, the guest's live pressure numbers.
type healthView struct {
	State string `json:"state"`
	*guestapi.HealthResponse
}

// sandboxHealth proxies the guest's live health (memory, PSI pressure) to
// the console. A non-RUNNING sandbox is not an error — the console polls
// through pauses — so it answers 200 with ok:false and no probe. 502 is
// reserved for the genuinely abnormal case: the row says RUNNING but the
// guest cannot be reached (a pause race, or a wedged VM).
func (s *Server) sandboxHealth(c *gin.Context) {
	sb, ok := s.ownedSandbox(c, c.Param("id"))
	if !ok {
		return
	}
	if sb.State != string(lifecycle.StateRunning) {
		c.JSON(http.StatusOK, healthView{State: sb.State, HealthResponse: &guestapi.HealthResponse{OK: false}})
		return
	}
	if h, ok := s.cachedGuestHealth(sb.ID); ok {
		metrics.GuestHealthProbes.WithLabelValues("cached").Inc()
		c.JSON(http.StatusOK, healthView{State: sb.State, HealthResponse: h})
		return
	}
	agent, ok := s.agentOf(c, sb)
	if !ok {
		return
	}
	// A poll must not ride the nodeapi client's generous verb timeout.
	ctx, cancel := context.WithTimeout(c, 3*time.Second)
	defer cancel()
	h, err := agent.Health(ctx, sb.ID)
	if err != nil {
		metrics.GuestHealthProbes.WithLabelValues("error").Inc()
		abortErr(c, http.StatusBadGateway, err)
		return
	}
	metrics.GuestHealthProbes.WithLabelValues("ok").Inc()
	s.storeGuestHealth(sb.ID, h)
	c.JSON(http.StatusOK, healthView{State: sb.State, HealthResponse: h})
}

func (s *Server) cachedGuestHealth(id string) (*guestapi.HealthResponse, bool) {
	s.hcMu.Lock()
	defer s.hcMu.Unlock()
	e, ok := s.hcCache[id]
	if !ok || time.Since(e.at) > healthCacheTTL {
		return nil, false
	}
	return e.h, true
}

func (s *Server) storeGuestHealth(id string, h *guestapi.HealthResponse) {
	s.hcMu.Lock()
	defer s.hcMu.Unlock()
	// Entries are overwritten in place; the only unbounded growth is dead
	// sandbox ids, so a rare full purge is enough.
	if len(s.hcCache) > 4096 {
		clear(s.hcCache)
	}
	s.hcCache[id] = cachedHealth{at: time.Now(), h: h}
}

// createProxySession mints an HttpOnly cookie session for the guest proxy:
// the browser cannot attach Authorization to <iframe> or new-tab requests,
// so the console trades its bearer token for a short-lived cookie that
// proxyCookieAuth honors on /proxy/ routes only. SameSite=Strict keeps
// third-party pages from riding it.
func (s *Server) createProxySession(c *gin.Context) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	id := hex.EncodeToString(raw)
	now := time.Now()
	s.psMu.Lock()
	for k, v := range s.proxySessions { // opportunistic expiry sweep
		if now.After(v.expires) {
			delete(s.proxySessions, k)
		}
	}
	s.proxySessions[id] = proxySession{info: tokenInfo(c), expires: now.Add(proxySessionTTL)}
	s.psMu.Unlock()
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(proxySessionCookie, id, int(proxySessionTTL.Seconds()),
		"/v0/sandboxes", "", c.Request.TLS != nil, true)
	c.Status(http.StatusNoContent)
}

func (s *Server) deleteProxySession(c *gin.Context) {
	if id, err := c.Cookie(proxySessionCookie); err == nil {
		s.psMu.Lock()
		delete(s.proxySessions, id)
		s.psMu.Unlock()
	}
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(proxySessionCookie, "", -1, "/v0/sandboxes", "", c.Request.TLS != nil, true)
	c.Status(http.StatusNoContent)
}

// proxyCookieAuth is the Auth fallback for guest-proxy routes (and nothing
// else): ownership is still enforced per-sandbox by ownedSandbox downstream.
func (s *Server) proxyCookieAuth(c *gin.Context) (TokenInfo, bool) {
	if !strings.Contains(c.FullPath(), "/proxy/") {
		return TokenInfo{}, false
	}
	id, err := c.Cookie(proxySessionCookie)
	if err != nil {
		return TokenInfo{}, false
	}
	s.psMu.Lock()
	defer s.psMu.Unlock()
	ps, ok := s.proxySessions[id]
	if !ok {
		return TokenInfo{}, false
	}
	if time.Now().After(ps.expires) {
		delete(s.proxySessions, id)
		return TokenInfo{}, false
	}
	return ps.info, true
}

// eventsPage parses the shared ?before= / ?limit= cursor params.
func eventsPage(c *gin.Context) (before int64, limit int, err error) {
	limit = 100
	if raw := c.Query("limit"); raw != "" {
		n, perr := strconv.Atoi(raw)
		if perr != nil || n <= 0 {
			return 0, 0, errors.New("limit must be a positive integer")
		}
		limit = min(n, 500)
	}
	if raw := c.Query("before"); raw != "" {
		n, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil || n <= 0 {
			return 0, 0, errors.New("before must be a positive event id")
		}
		before = n
	}
	return before, limit, nil
}

func eventsBody(events []SandboxEvent, limit int) gin.H {
	if events == nil {
		events = []SandboxEvent{}
	}
	body := gin.H{"events": events}
	if len(events) == limit {
		body["next_before"] = events[len(events)-1].ID
	}
	return body
}

// sandboxEvents is one sandbox's lifecycle timeline, newest first.
func (s *Server) sandboxEvents(c *gin.Context) {
	sb, ok := s.ownedSandbox(c, c.Param("id"))
	if !ok {
		return
	}
	before, limit, err := eventsPage(c)
	if err != nil {
		abortErr(c, http.StatusBadRequest, err)
		return
	}
	events, err := s.store.ListSandboxEvents(c, sb.ID, before, limit)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	c.JSON(http.StatusOK, eventsBody(events, limit))
}

// listEvents is the owner-wide activity feed.
func (s *Server) listEvents(c *gin.Context) {
	before, limit, err := eventsPage(c)
	if err != nil {
		abortErr(c, http.StatusBadRequest, err)
		return
	}
	events, err := s.store.ListOwnerEvents(c, tokenInfo(c).Owner, before, limit)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	c.JSON(http.StatusOK, eventsBody(events, limit))
}

// termSandbox is the interactive terminal: an authenticated, owner-scoped
// WebSocket that tunnels to guestd's /term (which owns the PTY). The data
// path is exactly the guest proxy's — httputil.ReverseProxy passes the
// Upgrade through — so split-mode needs no new nodeapi verb. Auth arrives
// via a Sec-WebSocket-Protocol bearer entry that Auth() already stripped, so
// the guest never sees the credential.
func (s *Server) termSandbox(c *gin.Context) {
	sb, ok := s.ownedSandbox(c, c.Param("id"))
	if !ok {
		return
	}
	if sb.State != string(lifecycle.StateRunning) {
		metrics.TermSessions.WithLabelValues("denied").Inc()
		abortErr(c, http.StatusConflict, fmt.Errorf("sandbox is %s, terminals need RUNNING", sb.State))
		return
	}
	if !strings.EqualFold(c.GetHeader("Upgrade"), "websocket") {
		abortErr(c, http.StatusBadRequest, errors.New("websocket upgrade required"))
		return
	}
	agent, ok := s.agentOf(c, sb)
	if !ok {
		return
	}

	var handler http.Handler
	switch g := agent.(type) {
	case nodeapi.GuestDialer: // in-proc: dial the netns directly
		nodeID := sb.NodeID
		if nodeID == "" {
			nodeID = LocalNodeID
		}
		handler = &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.URL.Scheme = "http"
				pr.Out.URL.Host = net.JoinHostPort(sb.ID, strconv.Itoa(guestapi.Port))
				pr.Out.URL.Path = "/term" // query (?cols=&rows=) rides along
				// The Sec-WebSocket-Protocol bearer entry is already stripped
				// by Auth(); drop the cookie/Authorization too so no platform
				// credential reaches guestd.
				stripPlatformCreds(pr.Out.Header)
			},
			Transport: s.guestTransportFor(nodeID, g),
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(err.Error()))
			},
		}
	case nodeapi.GuestProxier: // split mode: hop over the node's UDS
		inner := g.GuestProxy(sb.ID, guestapi.Port)
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/term"
			stripPlatformCreds(r2.Header)
			inner.ServeHTTP(w, r2)
		})
	default:
		abortErr(c, http.StatusNotImplemented, errors.New("node does not support guest proxying"))
		return
	}

	metrics.TermSessionsActive.Inc()
	defer metrics.TermSessionsActive.Dec()
	handler.ServeHTTP(c.Writer, c.Request) // blocks for the whole session
	// A hijacked (upgraded) session reports status 0; anything else means
	// the handshake failed before streaming started.
	result := "error"
	if status := c.Writer.Status(); status == 0 || status == http.StatusSwitchingProtocols {
		result = "ok"
	}
	metrics.TermSessions.WithLabelValues(result).Inc()
}
