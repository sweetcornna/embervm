package controlplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"

	"github.com/embervm/embervm/pkg/chunkstore"
	"github.com/embervm/embervm/pkg/guestapi"
	"github.com/embervm/embervm/pkg/lifecycle"
	"github.com/embervm/embervm/pkg/nodeagent"
	"github.com/embervm/embervm/pkg/nodeapi"
)

// Server is the REST control plane: it persists state in Store and drives the
// node through Agent.
type Server struct {
	store  *Store
	agent  nodeapi.Agent
	tokens *TokenStore
	l1     chunkstore.ListingBackend // warm object store (nil: reports/tiers degrade)
	cold   chunkstore.ListingBackend // cold object store
}

// NewServer wires a control-plane server. l1/cold may be nil: storage
// reports and selective restore then answer 503 for tiers they cannot see.
func NewServer(store *Store, agent nodeapi.Agent, tokens *TokenStore, l1, cold chunkstore.ListingBackend) *Server {
	return &Server{store: store, agent: agent, tokens: tokens, l1: l1, cold: cold}
}

// Handler builds the Gin router (mounted at /v0, plus an unauthenticated
// /healthz).
func (s *Server) Handler() http.Handler {
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	v0 := r.Group("/v0", s.tokens.Auth())
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

	v0.GET("/sandboxes/:id/storage", s.sandboxStorage)
	v0.GET("/storage-report", s.storageReportAll)
	v0.POST("/sandboxes/:id/restore-artifacts", s.restoreArtifacts)

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

// storeStatus maps store errors to HTTP status.
func storeStatus(err error) int {
	if errors.Is(err, ErrNotFound) {
		return http.StatusNotFound
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
	// M1 builds synchronously; the row starts BUILDING and settles here.
	if err := s.agent.BuildTemplate(c, id, body.Image); err != nil {
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
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		abortErr(c, http.StatusBadRequest, err)
		return
	}
	if body.TemplateID == "" {
		abortErr(c, http.StatusBadRequest, errors.New("template_id is required"))
		return
	}
	if _, err := s.store.GetTemplate(c, body.TemplateID); err != nil {
		abortErr(c, storeStatus(err), err)
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

	st, err := s.agent.CreateSandbox(c, nodeapi.CreateSandboxRequest{
		SandboxID: id, TemplateID: body.TemplateID,
		VCPUs: body.VCPUs, MemoryMiB: body.MemoryMiB, DataDiskGiB: body.DataDiskGiB,
	})
	if err != nil {
		_ = s.store.SetSandboxState(c, id, string(lifecycle.StatePending), string(lifecycle.StateFailed), "", err.Error())
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	if err := s.store.SetSandboxState(c, id, string(lifecycle.StatePending), st.State, st.Netns, ""); err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	sb.State, sb.Netns = st.State, st.Netns
	c.JSON(http.StatusCreated, sb)
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
	if err := s.agent.PauseSandbox(c, id); err != nil {
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
	case lifecycle.StatePausedWarm:
		st, err = s.agent.RestoreSandbox(c, id, "warm")
	case lifecycle.StateArchivedCold:
		st, err = s.agent.RestoreSandbox(c, id, "cold")
	default:
		st, err = s.agent.ResumeSandbox(c, id)
	}
	if err != nil {
		_ = s.store.SetSandboxState(c, id, string(lifecycle.StateResuming), string(lifecycle.StateFailed), "", err.Error())
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	if err := s.store.SetSandboxState(c, id, string(lifecycle.StateResuming), st.State, st.Netns, ""); err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	sb.State = st.State
	c.JSON(http.StatusOK, sb)
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

// storageReportAll aggregates the caller's sandboxes (成本报表).
func (s *Server) storageReportAll(c *gin.Context) {
	sbs, err := s.store.ListSandboxes(c, tokenInfo(c).Owner, "")
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	var (
		rows                        []StorageReport
		totalLogical, totalStored   int64
		totalChunks, totalArtifacts int64
	)
	for _, sb := range sbs {
		rep, err := s.storageReport(c, sb)
		if err != nil {
			abortErr(c, http.StatusInternalServerError, err)
			return
		}
		rows = append(rows, rep)
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
	st, err := s.agent.CreateSandbox(c, nodeapi.CreateSandboxRequest{
		SandboxID: newID, TemplateID: sb.TemplateID,
		VCPUs: sb.VCPUs, MemoryMiB: sb.MemoryMiB, DataDiskGiB: sb.DataDiskGiB,
	})
	if err != nil {
		_ = s.store.SetSandboxState(c, newID, string(lifecycle.StatePending), string(lifecycle.StateFailed), "", err.Error())
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	if err := s.store.SetSandboxState(c, newID, string(lifecycle.StatePending), st.State, st.Netns, ""); err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	if err := s.agent.WriteFile(c, newID, "/tmp/artifacts.tar", 0o600, tarBytes); err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	ex, err := s.agent.Exec(c, newID, &guestapi.ExecRequest{
		Cmd: "sh", Args: []string{"-c", "tar -xf /tmp/artifacts.tar -C / && rm /tmp/artifacts.tar"},
	})
	if err != nil || ex.ExitCode != 0 {
		abortErr(c, http.StatusInternalServerError,
			fmt.Errorf("untar artifacts: %v (exit=%d stderr=%s)", err, exitCodeOf(ex), stderrOf(ex)))
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
	return io.ReadAll(zr)
}

func (s *Server) snapshotSandbox(c *gin.Context) {
	id := c.Param("id")
	if _, ok := s.ownedSandbox(c, id); !ok {
		return
	}
	var body struct {
		Tag string `json:"tag"`
	}
	_ = c.ShouldBindJSON(&body)
	if body.Tag == "" {
		body.Tag = "snap"
	}
	snapID, err := s.agent.SnapshotSandbox(c, id, body.Tag)
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
	if err := s.agent.StopSandbox(c, id); err != nil {
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
	if _, ok := s.ownedSandbox(c, c.Param("id")); !ok {
		return
	}
	var req guestapi.ExecRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abortErr(c, http.StatusBadRequest, err)
		return
	}
	resp, err := s.agent.Exec(c, c.Param("id"), &req)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (s *Server) readFile(c *gin.Context) {
	if _, ok := s.ownedSandbox(c, c.Param("id")); !ok {
		return
	}
	path := c.Query("path")
	if path == "" {
		abortErr(c, http.StatusBadRequest, errors.New("path is required"))
		return
	}
	data, err := s.agent.ReadFile(c, c.Param("id"), path)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	c.Data(http.StatusOK, "application/octet-stream", data)
}

func (s *Server) writeFile(c *gin.Context) {
	if _, ok := s.ownedSandbox(c, c.Param("id")); !ok {
		return
	}
	path := c.Query("path")
	if path == "" {
		abortErr(c, http.StatusBadRequest, errors.New("path is required"))
		return
	}
	mode := uint64(0o644)
	if raw := c.Query("mode"); raw != "" {
		parsed, err := strconv.ParseUint(raw, 8, 32)
		if err != nil {
			abortErr(c, http.StatusBadRequest, errors.New("mode must be octal"))
			return
		}
		mode = parsed
	}
	data, err := io.ReadAll(c.Request.Body)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	if err := s.agent.WriteFile(c, c.Param("id"), path, fs.FileMode(mode), data); err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	c.Status(http.StatusNoContent)
}
