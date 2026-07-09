package controlplane

import (
	"context"
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
}

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
		guestTransports: map[string]*http.Transport{}}
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
		if strings.Contains(route, "/proxy/") || c.Request.Body == nil {
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

	v0 := r.Group("/v0", s.tokens.Auth(), limitBodies())
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
	v0.DELETE("/sandboxes/:id", s.killSandbox)

	// M5 fork/branch/rollback (ADR-0006).
	v0.POST("/sandboxes/:id/checkpoints", s.createCheckpoint)
	v0.GET("/sandboxes/:id/checkpoints", s.listCheckpoints)
	v0.POST("/sandboxes/:id/fork", s.forkSandbox)
	v0.POST("/sandboxes/:id/rollback", s.rollbackSandbox)

	v0.GET("/sandboxes/:id/storage", s.sandboxStorage)
	v0.GET("/storage-report", s.storageReportAll)
	v0.POST("/sandboxes/:id/restore-artifacts", s.restoreArtifacts)

	v0.Any("/sandboxes/:id/proxy/:port/*path", s.proxyGuest)

	v0.POST("/sandboxes/:id/exec", s.execSandbox)
	v0.GET("/sandboxes/:id/files", s.readFile)
	v0.PUT("/sandboxes/:id/files", s.writeFile)
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
		Egress: body.Egress,
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
	c.JSON(http.StatusOK, sb)
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
