package nodeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/embervm/embervm/pkg/guestapi"
)

// --- server -----------------------------------------------------------------

// NewServer returns an HTTP handler that serves an Agent. A node daemon
// listens with it on a unix socket; the control plane reaches it via Client.
func NewServer(a Agent) http.Handler {
	s := &server{agent: a}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("POST /templates/{id}/build", s.buildTemplate)
	mux.HandleFunc("POST /sandboxes", s.createSandbox)
	mux.HandleFunc("GET /sandboxes/{id}", s.status)
	mux.HandleFunc("POST /sandboxes/{id}/stop", s.stopSandbox)
	mux.HandleFunc("POST /sandboxes/{id}/pause", s.pauseSandbox)
	mux.HandleFunc("POST /sandboxes/{id}/resume", s.resumeSandbox)
	mux.HandleFunc("POST /sandboxes/{id}/snapshot", s.snapshotSandbox)
	mux.HandleFunc("POST /sandboxes/{id}/release", s.releaseLocal)
	mux.HandleFunc("POST /sandboxes/{id}/restore", s.restoreSandbox)
	mux.HandleFunc("POST /sandboxes/{id}/extract-artifacts", s.extractArtifacts)
	mux.HandleFunc("POST /sandboxes/{id}/prewarm", s.prewarm)
	mux.HandleFunc("POST /sandboxes/{id}/balloon", s.setBalloon)
	mux.HandleFunc("POST /sandboxes/{id}/resize", s.resizeSandbox)
	mux.HandleFunc("POST /sandboxes/{id}/fork", s.fork)
	mux.HandleFunc("POST /sandboxes/{id}/rollback", s.rollback)
	mux.HandleFunc("POST /sandboxes/{id}/exec", s.exec)
	mux.HandleFunc("GET /sandboxes/{id}/health", s.health)
	mux.HandleFunc("GET /sandboxes/{id}/files", s.readFile)
	mux.HandleFunc("PUT /sandboxes/{id}/files", s.writeFile)
	mux.HandleFunc("GET /sandboxes/{id}/files/list", s.listDir)
	if gd, ok := a.(GuestDialer); ok {
		// Gateway data path: any method, any subpath, WebSocket-transparent
		// (httputil.ReverseProxy passes Upgrade through).
		mux.Handle("/sandboxes/{id}/guest/{port}/",
			&guestProxy{transport: NewGuestTransport(gd.DialGuest)})
	}
	return mux
}

// NewGuestTransport builds a long-lived Transport that routes each request
// to the sandbox and port encoded in the URL host ("<sandboxID>:<port>").
// Guest proxies MUST share one of these per dialer: a per-request Transport
// strands every keep-alive connection (plus its readLoop goroutine) in a
// pool nothing ever reuses or closes — a connection leak per request — and
// defeats connection reuse entirely.
func NewGuestTransport(dial func(ctx context.Context, sandboxID string, port int) (net.Conn, error)) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			host, portStr, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("guest proxy: bad target %q: %w", addr, err)
			}
			port, err := strconv.Atoi(portStr)
			if err != nil {
				return nil, fmt.Errorf("guest proxy: bad port %q", portStr)
			}
			return dial(ctx, host, port)
		},
		MaxIdleConnsPerHost: 4, // pooled per sandbox:port (the URL host)
		IdleConnTimeout:     90 * time.Second,
	}
}

// guestProxy reverse-proxies into the sandbox netns.
type guestProxy struct {
	transport *http.Transport
}

func (g *guestProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port <= 0 || port > 65535 {
		writeJSON(w, http.StatusBadRequest, guestapi.ErrorResponse{Error: "bad port"})
		return
	}
	prefix := "/sandboxes/" + id + "/guest/" + r.PathValue("port")
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = net.JoinHostPort(id, strconv.Itoa(port))
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, prefix)
			if pr.Out.URL.Path == "" {
				pr.Out.URL.Path = "/"
			}
		},
		Transport: g.transport,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeJSON(w, http.StatusBadGateway, guestapi.ErrorResponse{Error: err.Error()})
		},
	}
	proxy.ServeHTTP(w, r)
}

type server struct{ agent Agent }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func fail(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusInternalServerError, guestapi.ErrorResponse{Error: err.Error()})
}

func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	h, err := s.agent.Healthz(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *server) buildTemplate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Image string `json:"image"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, guestapi.ErrorResponse{Error: err.Error()})
		return
	}
	if err := s.agent.BuildTemplate(r.Context(), r.PathValue("id"), body.Image); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (s *server) createSandbox(w http.ResponseWriter, r *http.Request) {
	var req CreateSandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, guestapi.ErrorResponse{Error: err.Error()})
		return
	}
	st, err := s.agent.CreateSandbox(r.Context(), req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	st, err := s.agent.Status(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *server) stopSandbox(w http.ResponseWriter, r *http.Request) {
	if err := s.agent.StopSandbox(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (s *server) pauseSandbox(w http.ResponseWriter, r *http.Request) {
	if err := s.agent.PauseSandbox(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (s *server) resumeSandbox(w http.ResponseWriter, r *http.Request) {
	st, err := s.agent.ResumeSandbox(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *server) snapshotSandbox(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Tag string `json:"tag"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, guestapi.ErrorResponse{Error: err.Error()})
		return
	}
	id, err := s.agent.SnapshotSandbox(r.Context(), r.PathValue("id"), body.Tag)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"snapshot_id": id})
}

func (s *server) releaseLocal(w http.ResponseWriter, r *http.Request) {
	if err := s.agent.ReleaseLocal(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (s *server) restoreSandbox(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Tier string `json:"tier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, guestapi.ErrorResponse{Error: err.Error()})
		return
	}
	st, err := s.agent.RestoreSandbox(r.Context(), r.PathValue("id"), body.Tier)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *server) extractArtifacts(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, guestapi.ErrorResponse{Error: err.Error()})
		return
	}
	if err := s.agent.ExtractArtifacts(r.Context(), r.PathValue("id"), body.Paths); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (s *server) prewarm(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Tier string `json:"tier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, guestapi.ErrorResponse{Error: err.Error()})
		return
	}
	if err := s.agent.Prewarm(r.Context(), r.PathValue("id"), body.Tier); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (s *server) setBalloon(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TargetMiB int `json:"target_mib"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, guestapi.ErrorResponse{Error: err.Error()})
		return
	}
	if err := s.agent.SetBalloon(r.Context(), r.PathValue("id"), body.TargetMiB); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (s *server) resizeSandbox(w http.ResponseWriter, r *http.Request) {
	var req ResizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, guestapi.ErrorResponse{Error: err.Error()})
		return
	}
	res, err := s.agent.ResizeSandbox(r.Context(), r.PathValue("id"), req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) fork(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Layer string `json:"layer"`
		NewID string `json:"new_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, guestapi.ErrorResponse{Error: err.Error()})
		return
	}
	st, err := s.agent.Fork(r.Context(), r.PathValue("id"), body.Layer, body.NewID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *server) rollback(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Layer string `json:"layer"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, guestapi.ErrorResponse{Error: err.Error()})
		return
	}
	st, err := s.agent.Rollback(r.Context(), r.PathValue("id"), body.Layer)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *server) exec(w http.ResponseWriter, r *http.Request) {
	var req guestapi.ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, guestapi.ErrorResponse{Error: err.Error()})
		return
	}
	resp, err := s.agent.Exec(r.Context(), r.PathValue("id"), &req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	h, err := s.agent.Health(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *server) listDir(w http.ResponseWriter, r *http.Request) {
	resp, err := s.agent.ListDir(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) readFile(w http.ResponseWriter, r *http.Request) {
	data, err := s.agent.ReadFile(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(data)
}

// maxWriteFileBody bounds PUT /files bodies (the largest legitimate payload
// is a restored artifacts tar, capped at 1 GiB by the control plane).
const maxWriteFileBody = 1 << 30

func (s *server) writeFile(w http.ResponseWriter, r *http.Request) {
	mode := fs.FileMode(0o644)
	if raw := r.URL.Query().Get("mode"); raw != "" {
		parsed, err := strconv.ParseUint(raw, 8, 32)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, guestapi.ErrorResponse{Error: "mode must be octal"})
			return
		}
		// Permission bits only: no setuid/setgid/sticky smuggled into the guest.
		mode = fs.FileMode(parsed) & fs.ModePerm
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWriteFileBody))
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.agent.WriteFile(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"), mode, data); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// --- client -----------------------------------------------------------------

// Client is an Agent backed by a node daemon reached over a unix socket. It
// implements the Agent interface so callers cannot tell it from an in-proc
// agent.
type Client struct {
	hc   *http.Client
	base string
}

var _ Agent = (*Client)(nil)

// NewClient dials the node daemon listening on the unix socket at
// socketPath.
func NewClient(socketPath string) *Client {
	return &Client{
		base: "http://node",
		hc: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
				},
				// A wedged node daemon must not pin a control-plane handler
				// goroutine forever. Header-level only: bodies stream for as
				// long as they need (guest proxy, template builds), and slow
				// verbs (a long Exec answers with headers only when the
				// command finishes) get a deliberately generous bound.
				ResponseHeaderTimeout: 15 * time.Minute,
				IdleConnTimeout:       90 * time.Second,
			},
		},
	}
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, reqBody, respBody any) error {
	var rdr io.Reader
	if reqBody != nil {
		raw, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var e guestapi.ErrorResponse
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return fmt.Errorf("nodeagent %s %s: %s", method, path, e.Error)
		}
		return fmt.Errorf("nodeagent %s %s: HTTP %d", method, path, resp.StatusCode)
	}
	if respBody != nil {
		return json.NewDecoder(resp.Body).Decode(respBody)
	}
	return nil
}

func (c *Client) Healthz(ctx context.Context) (NodeHealth, error) {
	var h NodeHealth
	err := c.do(ctx, http.MethodGet, "/healthz", nil, nil, &h)
	return h, err
}

func (c *Client) BuildTemplate(ctx context.Context, templateID, image string) error {
	return c.do(ctx, http.MethodPost, "/templates/"+templateID+"/build", nil,
		map[string]string{"image": image}, nil)
}

func (c *Client) CreateSandbox(ctx context.Context, req CreateSandboxRequest) (SandboxStatus, error) {
	var st SandboxStatus
	err := c.do(ctx, http.MethodPost, "/sandboxes", nil, req, &st)
	return st, err
}

func (c *Client) Status(ctx context.Context, sandboxID string) (SandboxStatus, error) {
	var st SandboxStatus
	err := c.do(ctx, http.MethodGet, "/sandboxes/"+sandboxID, nil, nil, &st)
	return st, err
}

func (c *Client) StopSandbox(ctx context.Context, sandboxID string) error {
	return c.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/stop", nil, nil, nil)
}

func (c *Client) PauseSandbox(ctx context.Context, sandboxID string) error {
	return c.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/pause", nil, nil, nil)
}

func (c *Client) ResumeSandbox(ctx context.Context, sandboxID string) (SandboxStatus, error) {
	var st SandboxStatus
	err := c.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/resume", nil, nil, &st)
	return st, err
}

func (c *Client) SnapshotSandbox(ctx context.Context, sandboxID, tag string) (string, error) {
	var out struct {
		SnapshotID string `json:"snapshot_id"`
	}
	err := c.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/snapshot", nil,
		map[string]string{"tag": tag}, &out)
	return out.SnapshotID, err
}

func (c *Client) ReleaseLocal(ctx context.Context, sandboxID string) error {
	return c.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/release", nil, nil, nil)
}

func (c *Client) RestoreSandbox(ctx context.Context, sandboxID, tier string) (SandboxStatus, error) {
	var st SandboxStatus
	err := c.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/restore", nil,
		map[string]string{"tier": tier}, &st)
	return st, err
}

func (c *Client) ExtractArtifacts(ctx context.Context, sandboxID string, paths []string) error {
	return c.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/extract-artifacts", nil,
		map[string][]string{"paths": paths}, nil)
}

func (c *Client) Prewarm(ctx context.Context, sandboxID, tier string) error {
	return c.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/prewarm", nil,
		map[string]string{"tier": tier}, nil)
}

// GuestProxy forwards guest-port traffic to the node daemon's proxy hop
// over the unix socket (the second hop dials the netns there).
func (c *Client) GuestProxy(sandboxID string, port int) http.Handler {
	prefix := "/sandboxes/" + sandboxID + "/guest/" + strconv.Itoa(port)
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "node"
			pr.Out.URL.Path = prefix + pr.In.URL.Path
		},
		Transport: c.hc.Transport,
	}
}

var _ GuestProxier = (*Client)(nil)

func (c *Client) SetBalloon(ctx context.Context, sandboxID string, targetMiB int) error {
	return c.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/balloon", nil,
		map[string]int{"target_mib": targetMiB}, nil)
}

func (c *Client) ResizeSandbox(ctx context.Context, sandboxID string, req ResizeRequest) (ResizeResult, error) {
	var res ResizeResult
	err := c.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/resize", nil, req, &res)
	return res, err
}

func (c *Client) Fork(ctx context.Context, parentID, layer, newID string) (SandboxStatus, error) {
	var st SandboxStatus
	err := c.do(ctx, http.MethodPost, "/sandboxes/"+parentID+"/fork", nil,
		map[string]string{"layer": layer, "new_id": newID}, &st)
	return st, err
}

func (c *Client) Rollback(ctx context.Context, sandboxID, layer string) (SandboxStatus, error) {
	var st SandboxStatus
	err := c.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/rollback", nil,
		map[string]string{"layer": layer}, &st)
	return st, err
}

func (c *Client) Exec(ctx context.Context, sandboxID string, req *guestapi.ExecRequest) (*guestapi.ExecResponse, error) {
	var resp guestapi.ExecResponse
	err := c.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/exec", nil, req, &resp)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) Health(ctx context.Context, sandboxID string) (*guestapi.HealthResponse, error) {
	var h guestapi.HealthResponse
	err := c.do(ctx, http.MethodGet, "/sandboxes/"+sandboxID+"/health", nil, nil, &h)
	if err != nil {
		return nil, err
	}
	return &h, nil
}

func (c *Client) ReadFile(ctx context.Context, sandboxID, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+"/sandboxes/"+sandboxID+"/files?"+url.Values{"path": {path}}.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("nodeagent read file: HTTP %d: %s", resp.StatusCode, raw)
	}
	return io.ReadAll(resp.Body)
}

func (c *Client) ListDir(ctx context.Context, sandboxID, path string) (*guestapi.ListDirResponse, error) {
	var resp guestapi.ListDirResponse
	err := c.do(ctx, http.MethodGet, "/sandboxes/"+sandboxID+"/files/list",
		url.Values{"path": {path}}, nil, &resp)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) WriteFile(ctx context.Context, sandboxID, path string, mode fs.FileMode, data []byte) error {
	q := url.Values{"path": {path}, "mode": {"0" + strconv.FormatUint(uint64(mode.Perm()), 8)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.base+"/sandboxes/"+sandboxID+"/files?"+q.Encode(), bytes.NewReader(data))
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("nodeagent write file: HTTP %d: %s", resp.StatusCode, raw)
	}
	return nil
}
