package controlplane

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/embervm/embervm/pkg/guestapi"
	"github.com/embervm/embervm/pkg/lifecycle"
	"github.com/embervm/embervm/pkg/nodeapi"
)

// Server is the REST control plane: it persists state in Store and drives the
// node through Agent.
type Server struct {
	store  *Store
	agent  nodeapi.Agent
	tokens *TokenStore
}

// NewServer wires a control-plane server.
func NewServer(store *Store, agent nodeapi.Agent, tokens *TokenStore) *Server {
	return &Server{store: store, agent: agent, tokens: tokens}
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

	v0.POST("/sandboxes/:id/exec", s.execSandbox)
	v0.GET("/sandboxes/:id/files", s.readFile)
	v0.PUT("/sandboxes/:id/files", s.writeFile)
	return r
}

func abortErr(c *gin.Context, status int, err error) {
	c.AbortWithStatusJSON(status, gin.H{"error": err.Error()})
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
	list, err := s.store.ListSandboxes(c, c.Query("state"))
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	c.JSON(http.StatusOK, list)
}

func (s *Server) getSandbox(c *gin.Context) {
	sb, err := s.store.GetSandbox(c, c.Param("id"))
	if err != nil {
		abortErr(c, storeStatus(err), err)
		return
	}
	c.JSON(http.StatusOK, sb)
}

func (s *Server) pauseSandbox(c *gin.Context) {
	id := c.Param("id")
	sb, err := s.store.GetSandbox(c, id)
	if err != nil {
		abortErr(c, storeStatus(err), err)
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
	sb, err := s.store.GetSandbox(c, id)
	if err != nil {
		abortErr(c, storeStatus(err), err)
		return
	}
	st, err := s.agent.ResumeSandbox(c, id)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	if err := s.store.SetSandboxState(c, id, sb.State, st.State, st.Netns, ""); err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	sb.State = st.State
	c.JSON(http.StatusOK, sb)
}

func (s *Server) snapshotSandbox(c *gin.Context) {
	id := c.Param("id")
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
	sb, err := s.store.GetSandbox(c, id)
	if err != nil {
		abortErr(c, storeStatus(err), err)
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
