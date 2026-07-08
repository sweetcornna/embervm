package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/embervm/embervm/pkg/guestapi"
	"github.com/embervm/embervm/pkg/nodeapi"
)

// cpMockAgent is a nodeapi.Agent that records calls for the handler tests.
type cpMockAgent struct {
	buildErr  error
	createErr error
}

func (m *cpMockAgent) BuildTemplate(_ context.Context, id, image string) error { return m.buildErr }
func (m *cpMockAgent) Healthz(context.Context) (nodeapi.NodeHealth, error) {
	return nodeapi.NodeHealth{CapacityMiB: 8192}, nil
}
func (m *cpMockAgent) ReleaseLocal(context.Context, string) error { return nil }
func (m *cpMockAgent) RestoreSandbox(_ context.Context, id, _ string) (nodeapi.SandboxStatus, error) {
	return nodeapi.SandboxStatus{SandboxID: id, State: "RUNNING"}, nil
}
func (m *cpMockAgent) ExtractArtifacts(context.Context, string, []string) error { return nil }
func (m *cpMockAgent) Prewarm(context.Context, string, string) error            { return nil }
func (m *cpMockAgent) SetBalloon(context.Context, string, int) error            { return nil }
func (m *cpMockAgent) CreateSandbox(_ context.Context, req nodeapi.CreateSandboxRequest) (nodeapi.SandboxStatus, error) {
	if m.createErr != nil {
		return nodeapi.SandboxStatus{}, m.createErr
	}
	return nodeapi.SandboxStatus{SandboxID: req.SandboxID, State: "RUNNING", Netns: "ember0", GuestAddr: "172.16.0.2:7777"}, nil
}
func (m *cpMockAgent) StopSandbox(context.Context, string) error { return nil }
func (m *cpMockAgent) PauseSandbox(context.Context, string) error {
	return nil
}
func (m *cpMockAgent) ResumeSandbox(_ context.Context, id string) (nodeapi.SandboxStatus, error) {
	return nodeapi.SandboxStatus{SandboxID: id, State: "RUNNING", Netns: "ember0"}, nil
}
func (m *cpMockAgent) SnapshotSandbox(_ context.Context, id, tag string) (string, error) {
	return "snap-" + tag, nil
}
func (m *cpMockAgent) Status(_ context.Context, id string) (nodeapi.SandboxStatus, error) {
	return nodeapi.SandboxStatus{SandboxID: id, State: "RUNNING"}, nil
}
func (m *cpMockAgent) Exec(_ context.Context, id string, req *guestapi.ExecRequest) (*guestapi.ExecResponse, error) {
	return &guestapi.ExecResponse{ExitCode: 0, Stdout: []byte("out:" + req.Cmd)}, nil
}
func (m *cpMockAgent) Health(_ context.Context, id string) (*guestapi.HealthResponse, error) {
	return &guestapi.HealthResponse{OK: true, Seq: 1}, nil
}
func (m *cpMockAgent) ReadFile(_ context.Context, id, path string) ([]byte, error) {
	return []byte("data:" + path), nil
}
func (m *cpMockAgent) WriteFile(context.Context, string, string, fs.FileMode, []byte) error {
	return nil
}

func newTestServer(t *testing.T, agent nodeapi.Agent) http.Handler {
	t.Helper()
	gin.SetMode(gin.TestMode)
	store := testStore(t) // skips without PG
	tokens := NewTokenStore(map[string]TokenInfo{
		"tok":  {Owner: "alice", MaxSandboxes: 2},
		"tok2": {Owner: "bob", MaxSandboxes: 2},
	})
	return NewServer(store, agent, tokens, nil, nil).Handler()
}

// callAs issues a request authenticated as the given bearer token.
func callAs(h http.Handler, token, method, path string, body any) *httptest.ResponseRecorder {
	var rdr *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	// A real server request always has a cancelable context. Without one,
	// httputil.ReverseProxy (the guest proxy) falls back to CloseNotifier,
	// which gin's writer claims but the recorder cannot back — a panic.
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	req = req.WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// call issues a request as the default alice token.
func call(h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	return callAs(h, "tok", method, path, body)
}

func TestServerFullLifecycle(t *testing.T) {
	h := newTestServer(t, &cpMockAgent{})

	// Create template.
	w := call(h, http.MethodPost, "/v0/templates", map[string]string{"name": "web", "image": "alpine:3.20"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create template = %d: %s", w.Code, w.Body)
	}
	var tpl Template
	_ = json.Unmarshal(w.Body.Bytes(), &tpl)
	if tpl.State != "READY" {
		t.Errorf("template state = %q, want READY", tpl.State)
	}

	// Create sandbox.
	w = call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl.ID, "vcpus": 1, "memory_mib": 256, "data_disk_gib": 15})
	if w.Code != http.StatusCreated {
		t.Fatalf("create sandbox = %d: %s", w.Code, w.Body)
	}
	var sb Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &sb)
	if sb.State != "RUNNING" {
		t.Errorf("sandbox state = %q, want RUNNING", sb.State)
	}

	// Lifecycle transitions.
	for _, path := range []string{"/v0/sandboxes/" + sb.ID + "/pause", "/v0/sandboxes/" + sb.ID + "/resume"} {
		if w := call(h, http.MethodPost, path, nil); w.Code != http.StatusOK {
			t.Errorf("%s = %d: %s", path, w.Code, w.Body)
		}
	}
	if w := call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/snapshot", map[string]string{"tag": "s1"}); w.Code != http.StatusOK {
		t.Errorf("snapshot = %d: %s", w.Code, w.Body)
	}

	// Guest proxy.
	w = call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/exec", guestapi.ExecRequest{Cmd: "echo"})
	if w.Code != http.StatusOK {
		t.Fatalf("exec = %d: %s", w.Code, w.Body)
	}
	var ex guestapi.ExecResponse
	_ = json.Unmarshal(w.Body.Bytes(), &ex)
	if string(ex.Stdout) != "out:echo" {
		t.Errorf("exec stdout = %q", ex.Stdout)
	}

	// Kill.
	if w := call(h, http.MethodDelete, "/v0/sandboxes/"+sb.ID, nil); w.Code != http.StatusNoContent {
		t.Errorf("kill = %d: %s", w.Code, w.Body)
	}
}

func TestServerQuota(t *testing.T) {
	h := newTestServer(t, &cpMockAgent{})
	w := call(h, http.MethodPost, "/v0/templates", map[string]string{"name": "web", "image": "img"})
	var tpl Template
	_ = json.Unmarshal(w.Body.Bytes(), &tpl)

	// max_sandboxes is 2.
	for i := 0; i < 2; i++ {
		if w := call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl.ID}); w.Code != http.StatusCreated {
			t.Fatalf("sandbox %d = %d: %s", i, w.Code, w.Body)
		}
	}
	if w := call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl.ID}); w.Code != http.StatusTooManyRequests {
		t.Errorf("3rd sandbox = %d, want 429: %s", w.Code, w.Body)
	}
}

// TestServerSandboxOwnership is the regression for the multi-tenant IDOR:
// bob must not be able to see or touch alice's sandbox, on any verb.
func TestServerSandboxOwnership(t *testing.T) {
	h := newTestServer(t, &cpMockAgent{})

	w := call(h, http.MethodPost, "/v0/templates", map[string]string{"name": "web", "image": "img"})
	var tpl Template
	_ = json.Unmarshal(w.Body.Bytes(), &tpl)

	// alice creates a sandbox.
	w = call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl.ID})
	var sb Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &sb)
	if sb.ID == "" {
		t.Fatalf("no sandbox id: %s", w.Body)
	}

	// bob must get 404 on every sandbox verb targeting alice's sandbox.
	base := "/v0/sandboxes/" + sb.ID
	probes := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, base, nil},
		{http.MethodPost, base + "/pause", nil},
		{http.MethodPost, base + "/resume", nil},
		{http.MethodPost, base + "/snapshot", map[string]string{"tag": "x"}},
		{http.MethodPost, base + "/exec", guestapi.ExecRequest{Cmd: "echo"}},
		{http.MethodGet, base + "/files?path=/etc/hostname", nil},
		{http.MethodPut, base + "/files?path=/tmp/x", nil},
		{http.MethodDelete, base, nil},
	}
	for _, p := range probes {
		if w := callAs(h, "tok2", p.method, p.path, p.body); w.Code != http.StatusNotFound {
			t.Errorf("bob %s %s = %d, want 404 (cross-tenant access must be denied)", p.method, p.path, w.Code)
		}
	}

	// bob's own list must not include alice's sandbox.
	w = callAs(h, "tok2", http.MethodGet, "/v0/sandboxes", nil)
	var bobList []Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &bobList)
	if len(bobList) != 0 {
		t.Errorf("bob sees %d sandboxes, want 0 (owner scoping)", len(bobList))
	}

	// alice still has full access.
	if w := call(h, http.MethodGet, base, nil); w.Code != http.StatusOK {
		t.Errorf("alice GET own sandbox = %d, want 200", w.Code)
	}
}

func TestServerAuthRequired(t *testing.T) {
	h := newTestServer(t, &cpMockAgent{})
	req := httptest.NewRequest(http.MethodGet, "/v0/templates", nil) // no auth header
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated request = %d, want 401", w.Code)
	}
	// healthz is open.
	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("healthz = %d, want 200", w.Code)
	}
}

// cpDialerAgent adds the GuestDialer data path to cpMockAgent: the M4
// gateway's in-proc branch (embervm dev) dials the guest directly.
type cpDialerAgent struct {
	cpMockAgent
	addr string // stand-in guest listener host:port
}

func (d *cpDialerAgent) DialGuest(_ context.Context, _ string, _ int) (net.Conn, error) {
	return net.Dial("tcp", d.addr)
}

func TestServerGuestProxy(t *testing.T) {
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "guest %s %s", r.Method, r.URL.Path)
	}))
	defer guest.Close()
	h := newTestServer(t, &cpDialerAgent{addr: strings.TrimPrefix(guest.URL, "http://")})

	w := call(h, http.MethodPost, "/v0/templates", map[string]string{"name": "web", "image": "img"})
	var tpl Template
	_ = json.Unmarshal(w.Body.Bytes(), &tpl)
	w = call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl.ID})
	var sb Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &sb)
	if sb.ID == "" {
		t.Fatalf("no sandbox id: %s", w.Body)
	}
	base := "/v0/sandboxes/" + sb.ID + "/proxy/8080"

	// Any method, any subpath, owner-scoped.
	if w := call(h, http.MethodPost, base+"/api/run", nil); w.Code != http.StatusOK || w.Body.String() != "guest POST /api/run" {
		t.Errorf("proxy = %d %q", w.Code, w.Body)
	}
	if w := callAs(h, "tok2", http.MethodGet, base+"/x", nil); w.Code != http.StatusNotFound {
		t.Errorf("cross-tenant proxy = %d, want 404", w.Code)
	}
	if w := call(h, http.MethodGet, "/v0/sandboxes/"+sb.ID+"/proxy/99999/x", nil); w.Code != http.StatusBadRequest {
		t.Errorf("bad port = %d, want 400", w.Code)
	}
}

// TestServerGuestProxyUnsupportedAgent pins the capability gate: an agent
// with neither GuestDialer nor GuestProxier yields 501, not a panic.
func TestServerGuestProxyUnsupportedAgent(t *testing.T) {
	h := newTestServer(t, &cpMockAgent{})
	w := call(h, http.MethodPost, "/v0/templates", map[string]string{"name": "web", "image": "img"})
	var tpl Template
	_ = json.Unmarshal(w.Body.Bytes(), &tpl)
	w = call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl.ID})
	var sb Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &sb)

	if w := call(h, http.MethodGet, "/v0/sandboxes/"+sb.ID+"/proxy/8080/x", nil); w.Code != http.StatusNotImplemented {
		t.Errorf("proxy without dialer = %d, want 501: %s", w.Code, w.Body)
	}
}
