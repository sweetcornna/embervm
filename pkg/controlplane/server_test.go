package controlplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"

	"github.com/embervm/embervm/pkg/guestapi"
	"github.com/embervm/embervm/pkg/nodeapi"
)

// cpMockAgent is a nodeapi.Agent that records calls for the handler tests.
type cpMockAgent struct {
	buildErr    error
	createErr   error
	healthErr   error
	healthCalls atomic.Int64
	snapSeq     int
	lastFork    struct{ parent, layer, newID string }
	lastRB      struct{ id, layer string }
	lastResize  nodeapi.ResizeRequest
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
func (m *cpMockAgent) ResizeSandbox(_ context.Context, id string, req nodeapi.ResizeRequest) (nodeapi.ResizeResult, error) {
	m.lastResize = req
	return nodeapi.ResizeResult{MemoryMiB: req.MemoryMiB, VCPUs: req.VCPUs}, nil
}
func (m *cpMockAgent) Fork(_ context.Context, parentID, layer, newID string) (nodeapi.SandboxStatus, error) {
	m.lastFork = struct{ parent, layer, newID string }{parentID, layer, newID}
	return nodeapi.SandboxStatus{SandboxID: newID, State: "RUNNING", Netns: "ember1"}, nil
}
func (m *cpMockAgent) Rollback(_ context.Context, id, layer string) (nodeapi.SandboxStatus, error) {
	m.lastRB = struct{ id, layer string }{id, layer}
	return nodeapi.SandboxStatus{SandboxID: id, State: "RUNNING", Netns: "ember0"}, nil
}
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
	// The producer-defined return format the checkpoint handler parses.
	m.snapSeq++
	return fmt.Sprintf("%s@%s-%d", id, tag, m.snapSeq), nil
}
func (m *cpMockAgent) Status(_ context.Context, id string) (nodeapi.SandboxStatus, error) {
	return nodeapi.SandboxStatus{SandboxID: id, State: "RUNNING"}, nil
}
func (m *cpMockAgent) Exec(_ context.Context, id string, req *guestapi.ExecRequest) (*guestapi.ExecResponse, error) {
	return &guestapi.ExecResponse{ExitCode: 0, Stdout: []byte("out:" + req.Cmd)}, nil
}
func (m *cpMockAgent) Health(_ context.Context, id string) (*guestapi.HealthResponse, error) {
	m.healthCalls.Add(1)
	if m.healthErr != nil {
		return nil, m.healthErr
	}
	return &guestapi.HealthResponse{OK: true, Seq: 1, MemTotalKiB: 1 << 20, MemAvailableKiB: 1 << 19, PSIMemSome10: 0.5}, nil
}
func (m *cpMockAgent) ReadFile(_ context.Context, id, path string) ([]byte, error) {
	return []byte("data:" + path), nil
}
func (m *cpMockAgent) WriteFile(context.Context, string, string, fs.FileMode, []byte) error {
	return nil
}
func (m *cpMockAgent) ListDir(_ context.Context, id, path string) (*guestapi.ListDirResponse, error) {
	return &guestapi.ListDirResponse{Path: path, Entries: []guestapi.DirEntry{
		{Name: "etc", IsDir: true, Mode: "drwxr-xr-x"},
		{Name: "data.txt", Size: 4, Mode: "-rw-r--r--"},
	}}, nil
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

// TestServerResize walks the M6 resize verb: ceilings enforced at create,
// happy path updates accounting, state and bound violations 409, empty body
// 400, fixed-geometry sandboxes are not resizable.
func TestServerResize(t *testing.T) {
	agent := &cpMockAgent{}
	h := newTestServer(t, agent)

	w := call(h, http.MethodPost, "/v0/templates", map[string]string{"name": "web", "image": "alpine:3.20"})
	var tpl Template
	_ = json.Unmarshal(w.Body.Bytes(), &tpl)

	// Resizable sandbox: 256..1000 MiB (rounds up to 256+768=1024), 1..4 cpus.
	w = call(h, http.MethodPost, "/v0/sandboxes", map[string]any{
		"template_id": tpl.ID, "vcpus": 1, "memory_mib": 256, "data_disk_gib": 1,
		"max_memory_mib": 1000, "max_vcpus": 4,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body)
	}
	var sb Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &sb)
	if sb.MaxMemoryMiB != 1024 {
		t.Errorf("stored ceiling = %d, want 1024 (slot-rounded from 1000)", sb.MaxMemoryMiB)
	}

	// Happy path.
	w = call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/resize", map[string]any{"memory_mib": 768, "vcpus": 2})
	if w.Code != http.StatusOK {
		t.Fatalf("resize = %d: %s", w.Code, w.Body)
	}
	var got Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.MemoryMiB != 768 || got.VCPUs != 2 {
		t.Errorf("resized to %d MiB / %d cpus, want 768/2", got.MemoryMiB, got.VCPUs)
	}
	if agent.lastResize.MemoryMiB != 768 || agent.lastResize.VCPUs != 2 {
		t.Errorf("agent saw %+v", agent.lastResize)
	}
	// Accounting persisted.
	w = call(h, http.MethodGet, "/v0/sandboxes/"+sb.ID, nil)
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.MemoryMiB != 768 {
		t.Errorf("persisted memory = %d, want 768", got.MemoryMiB)
	}

	// Over the ceiling → 409.
	if w := call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/resize", map[string]any{"memory_mib": 2048}); w.Code != http.StatusConflict {
		t.Errorf("over-ceiling resize = %d, want 409: %s", w.Code, w.Body)
	}
	if w := call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/resize", map[string]any{"vcpus": 8}); w.Code != http.StatusConflict {
		t.Errorf("over-ceiling cpu resize = %d, want 409: %s", w.Code, w.Body)
	}
	// Nothing to do → 400.
	if w := call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/resize", map[string]any{}); w.Code != http.StatusBadRequest {
		t.Errorf("empty resize = %d, want 400: %s", w.Code, w.Body)
	}
	// Wrong state → 409.
	if w := call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/pause", nil); w.Code != http.StatusOK {
		t.Fatalf("pause = %d: %s", w.Code, w.Body)
	}
	if w := call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/resize", map[string]any{"memory_mib": 512}); w.Code != http.StatusConflict {
		t.Errorf("paused resize = %d, want 409: %s", w.Code, w.Body)
	}

	// Fixed-geometry sandbox cannot resize.
	w = call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl.ID, "vcpus": 1, "memory_mib": 256, "data_disk_gib": 1})
	var fixed Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &fixed)
	if w := call(h, http.MethodPost, "/v0/sandboxes/"+fixed.ID+"/resize", map[string]any{"memory_mib": 512}); w.Code != http.StatusConflict {
		t.Errorf("fixed-geometry resize = %d, want 409: %s", w.Code, w.Body)
	}

	// Ceiling without a base is a 400 at create.
	if w := call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl.ID, "max_memory_mib": 1024}); w.Code != http.StatusBadRequest {
		t.Errorf("ceiling-without-base create = %d, want 400: %s", w.Code, w.Body)
	}
}

// TestServerMigrate walks the M6 migrate verb on a two-node cluster: a
// RUNNING sandbox moves and ends RUNNING on the target; explicit same-node
// targets 409; a PAUSED_HOT sandbox just moves its placement (PAUSED_WARM).
func TestServerMigrate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := testStore(t)
	agents := map[string]nodeapi.Agent{"n1": &cpMockAgent{}, "n2": &cpMockAgent{}}
	registry := NewRegistry(agents)
	sched := NewScheduler(store, registry, SchedulerConfig{})
	if err := sched.RegisterNodes(context.Background(),
		map[string]string{"n1": "", "n2": ""}, map[string]int{"n1": 0, "n2": 0}); err != nil {
		t.Fatal(err)
	}
	tokens := NewTokenStore(map[string]TokenInfo{"tok": {Owner: "alice", MaxSandboxes: 5}})
	h := NewClusterServer(store, registry, sched, tokens, nil, nil).Handler()

	w := call(h, http.MethodPost, "/v0/templates", map[string]string{"name": "web", "image": "alpine:3.20"})
	var tpl Template
	_ = json.Unmarshal(w.Body.Bytes(), &tpl)
	w = call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl.ID, "vcpus": 1, "memory_mib": 256, "data_disk_gib": 1})
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body)
	}
	var sb Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &sb)
	src := sb.NodeID
	if src == "" {
		t.Fatal("sandbox has no placement")
	}

	// Explicit same-node target refuses.
	if w := call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/migrate", map[string]string{"node_id": src}); w.Code != http.StatusConflict {
		t.Errorf("same-node migrate = %d, want 409: %s", w.Code, w.Body)
	}

	// RUNNING migrate lands RUNNING elsewhere.
	w = call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/migrate", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("migrate = %d: %s", w.Code, w.Body)
	}
	var moved Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &moved)
	if moved.NodeID == src || moved.NodeID == "" {
		t.Errorf("migrate stayed on %q", moved.NodeID)
	}
	if moved.State != "RUNNING" {
		t.Errorf("post-migrate state = %s, want RUNNING", moved.State)
	}

	// PAUSED_HOT migrate only moves the pointer (PAUSED_WARM).
	if w := call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/pause", nil); w.Code != http.StatusOK {
		t.Fatalf("pause = %d: %s", w.Code, w.Body)
	}
	w = call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/migrate", map[string]string{"node_id": src})
	if w.Code != http.StatusOK {
		t.Fatalf("paused migrate = %d: %s", w.Code, w.Body)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &moved)
	if moved.NodeID != src || moved.State != "PAUSED_WARM" {
		t.Errorf("paused migrate = node %s state %s, want %s/PAUSED_WARM", moved.NodeID, moved.State, src)
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

// TestSandboxHealthEndpoint pins /health semantics: RUNNING probes the
// guest (with a short-TTL cache), non-RUNNING answers ok:false WITHOUT
// touching the node, probe failure on a RUNNING row is 502, ownership 404s.
func TestSandboxHealthEndpoint(t *testing.T) {
	agent := &cpMockAgent{}
	h := newTestServer(t, agent)

	w := call(h, http.MethodPost, "/v0/templates", map[string]string{"name": "web", "image": "img"})
	var tpl Template
	_ = json.Unmarshal(w.Body.Bytes(), &tpl)
	w = call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl.ID})
	var sb Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &sb)

	var hv struct {
		State       string `json:"state"`
		OK          bool   `json:"ok"`
		MemTotalKiB uint64 `json:"mem_total_kib"`
	}
	w = call(h, http.MethodGet, "/v0/sandboxes/"+sb.ID+"/health", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("health = %d: %s", w.Code, w.Body)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &hv)
	if hv.State != "RUNNING" || !hv.OK || hv.MemTotalKiB != 1<<20 {
		t.Errorf("health body = %+v: %s", hv, w.Body)
	}
	if n := agent.healthCalls.Load(); n != 1 {
		t.Errorf("probes = %d, want 1", n)
	}

	// Within the TTL the cache answers; the guest is not re-probed.
	if w := call(h, http.MethodGet, "/v0/sandboxes/"+sb.ID+"/health", nil); w.Code != http.StatusOK {
		t.Errorf("cached health = %d", w.Code)
	}
	if n := agent.healthCalls.Load(); n != 1 {
		t.Errorf("probes after cached read = %d, want 1", n)
	}

	// Paused: 200 ok:false, node untouched.
	call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/pause", nil)
	w = call(h, http.MethodGet, "/v0/sandboxes/"+sb.ID+"/health", nil)
	_ = json.Unmarshal(w.Body.Bytes(), &hv)
	if w.Code != http.StatusOK || hv.OK || hv.State != "PAUSED_HOT" {
		t.Errorf("paused health = %d %+v", w.Code, hv)
	}
	if n := agent.healthCalls.Load(); n != 1 {
		t.Errorf("paused sandbox probed the guest (%d probes)", n)
	}

	// Ownership: other tenants get 404, not 401/403 (no probing).
	if w := callAs(h, "tok2", http.MethodGet, "/v0/sandboxes/"+sb.ID+"/health", nil); w.Code != http.StatusNotFound {
		t.Errorf("cross-tenant health = %d, want 404", w.Code)
	}

	// A RUNNING row whose guest cannot be reached is genuinely abnormal: 502.
	agent.healthErr = fmt.Errorf("guest unreachable")
	w = call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl.ID})
	var sb2 Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &sb2)
	if w := call(h, http.MethodGet, "/v0/sandboxes/"+sb2.ID+"/health", nil); w.Code != http.StatusBadGateway {
		t.Errorf("unreachable guest = %d, want 502: %s", w.Code, w.Body)
	}
}

// TestSandboxTermEndpoint drives the terminal path end to end below the
// PTY: browser-style subprotocol auth → gin → ReverseProxy upgrade → stub
// guest /term. Also pins the state gate (409), the upgrade requirement
// (400), ownership (404), and that the credential never reaches the guest.
func TestSandboxTermEndpoint(t *testing.T) {
	var guestSaw struct {
		path, query, protocols string
	}
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		guestSaw.path, guestSaw.query = r.URL.Path, r.URL.RawQuery
		guestSaw.protocols = r.Header.Get("Sec-WebSocket-Protocol")
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{guestapi.TermSubprotocol}})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		_ = conn.Write(ctx, typ, data)
		conn.Close(websocket.StatusNormalClosure, "")
	}))
	defer guest.Close()

	h := newTestServer(t, &cpDialerAgent{addr: strings.TrimPrefix(guest.URL, "http://")})
	front := httptest.NewServer(h)
	defer front.Close()

	w := call(h, http.MethodPost, "/v0/templates", map[string]string{"name": "web", "image": "img"})
	var tpl Template
	_ = json.Unmarshal(w.Body.Bytes(), &tpl)
	w = call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl.ID})
	var sb Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &sb)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx,
		"ws"+strings.TrimPrefix(front.URL, "http")+"/v0/sandboxes/"+sb.ID+"/term?cols=120&rows=40",
		&websocket.DialOptions{Subprotocols: []string{
			"bearer." + base64.RawURLEncoding.EncodeToString([]byte("tok")),
			guestapi.TermSubprotocol,
		}})
	if err != nil {
		t.Fatalf("ws dial /term: %v", err)
	}
	defer conn.CloseNow()
	if got := conn.Subprotocol(); got != guestapi.TermSubprotocol {
		t.Errorf("negotiated subprotocol = %q", got)
	}
	if err := conn.Write(ctx, websocket.MessageBinary, []byte("ls\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	typ, data, err := conn.Read(ctx)
	if err != nil || typ != websocket.MessageBinary || string(data) != "ls\n" {
		t.Errorf("echo = %v %q (%v)", typ, data, err)
	}
	if guestSaw.path != "/term" || guestSaw.query != "cols=120&rows=40" {
		t.Errorf("guest saw %s?%s, want /term?cols=120&rows=40", guestSaw.path, guestSaw.query)
	}
	if strings.Contains(guestSaw.protocols, "bearer.") {
		t.Errorf("credential leaked to the guest: %q", guestSaw.protocols)
	}

	// Non-upgrade GET on a RUNNING sandbox: 400.
	if w := call(h, http.MethodGet, "/v0/sandboxes/"+sb.ID+"/term", nil); w.Code != http.StatusBadRequest {
		t.Errorf("plain GET /term = %d, want 400: %s", w.Code, w.Body)
	}
	// Ownership: 404 before any state leak.
	if w := callAs(h, "tok2", http.MethodGet, "/v0/sandboxes/"+sb.ID+"/term", nil); w.Code != http.StatusNotFound {
		t.Errorf("cross-tenant /term = %d, want 404", w.Code)
	}
	// State gate: a paused sandbox refuses loudly.
	call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/pause", nil)
	if w := call(h, http.MethodGet, "/v0/sandboxes/"+sb.ID+"/term", nil); w.Code != http.StatusConflict {
		t.Errorf("paused /term = %d, want 409: %s", w.Code, w.Body)
	}
}

// TestServerListDir pins the ?op=list branch of GET /files.
func TestServerListDir(t *testing.T) {
	h := newTestServer(t, &cpMockAgent{})

	w := call(h, http.MethodPost, "/v0/templates", map[string]string{"name": "web", "image": "img"})
	var tpl Template
	_ = json.Unmarshal(w.Body.Bytes(), &tpl)
	w = call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl.ID})
	var sb Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &sb)

	w = call(h, http.MethodGet, "/v0/sandboxes/"+sb.ID+"/files?op=list&path=/opt", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", w.Code, w.Body)
	}
	var listing guestapi.ListDirResponse
	_ = json.Unmarshal(w.Body.Bytes(), &listing)
	if listing.Path != "/opt" || len(listing.Entries) != 2 || !listing.Entries[0].IsDir {
		t.Errorf("listing = %+v", listing)
	}

	// The op parameter must not change plain reads.
	if w := call(h, http.MethodGet, "/v0/sandboxes/"+sb.ID+"/files?path=/etc/hosts", nil); w.Code != http.StatusOK || w.Body.String() != "data:/etc/hosts" {
		t.Errorf("plain read = %d %q", w.Code, w.Body)
	}
	if w := callAs(h, "tok2", http.MethodGet, "/v0/sandboxes/"+sb.ID+"/files?op=list&path=/", nil); w.Code != http.StatusNotFound {
		t.Errorf("cross-tenant list = %d, want 404", w.Code)
	}
}

// TestSandboxEventsEndpoint pins the lifecycle timeline: newest-first
// ordering, the id cursor, owner scoping, and error detail on failures.
func TestSandboxEventsEndpoint(t *testing.T) {
	h := newTestServer(t, &cpMockAgent{})

	w := call(h, http.MethodPost, "/v0/templates", map[string]string{"name": "web", "image": "img"})
	var tpl Template
	_ = json.Unmarshal(w.Body.Bytes(), &tpl)
	w = call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl.ID})
	var sb Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &sb)
	call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/pause", nil)
	call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/resume", nil)

	type page struct {
		Events []struct {
			ID        int64          `json:"id"`
			SandboxID string         `json:"sandbox_id"`
			FromState string         `json:"from_state"`
			ToState   string         `json:"to_state"`
			Detail    map[string]any `json:"detail"`
		} `json:"events"`
		NextBefore int64 `json:"next_before"`
	}
	get := func(path string) page {
		t.Helper()
		w := call(h, http.MethodGet, path, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, w.Code, w.Body)
		}
		var p page
		_ = json.Unmarshal(w.Body.Bytes(), &p)
		return p
	}

	full := get("/v0/sandboxes/" + sb.ID + "/events")
	// create + pause + resume leave at least 4 transitions behind.
	if len(full.Events) < 4 {
		t.Fatalf("events = %d, want ≥4", len(full.Events))
	}
	if full.Events[0].ToState != "RUNNING" || full.Events[1].ToState != "RESUMING" {
		t.Errorf("newest-first violated: %v %v", full.Events[0].ToState, full.Events[1].ToState)
	}
	for i := 1; i < len(full.Events); i++ {
		if full.Events[i].ID >= full.Events[i-1].ID {
			t.Fatalf("ids not strictly descending at %d", i)
		}
	}

	// Cursor: walking limit=1 pages reproduces the full list.
	var walked []int64
	next := int64(0)
	for range len(full.Events) {
		path := "/v0/sandboxes/" + sb.ID + "/events?limit=1"
		if next > 0 {
			path += "&before=" + strconv.FormatInt(next, 10)
		}
		p := get(path)
		if len(p.Events) != 1 {
			break
		}
		walked = append(walked, p.Events[0].ID)
		if p.NextBefore == 0 {
			break
		}
		next = p.NextBefore
	}
	if len(walked) != len(full.Events) {
		t.Errorf("cursor walk saw %d events, want %d", len(walked), len(full.Events))
	}

	// Bad params 400; ownership 404; the fleet feed is owner-scoped.
	if w := call(h, http.MethodGet, "/v0/sandboxes/"+sb.ID+"/events?limit=zero", nil); w.Code != http.StatusBadRequest {
		t.Errorf("bad limit = %d, want 400", w.Code)
	}
	if w := callAs(h, "tok2", http.MethodGet, "/v0/sandboxes/"+sb.ID+"/events", nil); w.Code != http.StatusNotFound {
		t.Errorf("cross-tenant events = %d, want 404", w.Code)
	}
	wf := callAs(h, "tok2", http.MethodGet, "/v0/events", nil)
	var bobFeed page
	_ = json.Unmarshal(wf.Body.Bytes(), &bobFeed)
	if len(bobFeed.Events) != 0 {
		t.Errorf("bob's fleet feed sees alice's events: %d", len(bobFeed.Events))
	}
	aliceFeed := get("/v0/events")
	if len(aliceFeed.Events) != len(full.Events) {
		t.Errorf("alice fleet feed = %d, want %d", len(aliceFeed.Events), len(full.Events))
	}

	// Failed transitions carry their cause in detail.error.
	failing := &cpMockAgent{createErr: fmt.Errorf("no kvm today")}
	h2 := newTestServer(t, failing)
	w = call(h2, http.MethodPost, "/v0/templates", map[string]string{"name": "web", "image": "img"})
	_ = json.Unmarshal(w.Body.Bytes(), &tpl)
	w = call(h2, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl.ID})
	var failed struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &failed)
	if failed.ID != "" {
		w = call(h2, http.MethodGet, "/v0/sandboxes/"+failed.ID+"/events", nil)
		var p page
		_ = json.Unmarshal(w.Body.Bytes(), &p)
		found := false
		for _, e := range p.Events {
			if e.ToState == "FAILED" && e.Detail != nil && e.Detail["error"] != nil {
				found = true
			}
		}
		if !found {
			t.Errorf("no FAILED event with detail.error: %s", w.Body)
		}
	}
}

// TestProxySessionCookie pins the iframe auth path: the cookie works on
// proxy routes only, ownership still applies, and revocation sticks.
func TestProxySessionCookie(t *testing.T) {
	var guestSaw struct{ cookie, authz string }
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The guest is untrusted: no platform credential may reach it.
		guestSaw.cookie = r.Header.Get("Cookie")
		guestSaw.authz = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("guest-ok"))
	}))
	defer guest.Close()
	h := newTestServer(t, &cpDialerAgent{addr: strings.TrimPrefix(guest.URL, "http://")})

	w := call(h, http.MethodPost, "/v0/templates", map[string]string{"name": "web", "image": "img"})
	var tpl Template
	_ = json.Unmarshal(w.Body.Bytes(), &tpl)
	w = call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl.ID})
	var sb Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &sb)

	// Mint a session as alice.
	w = call(h, http.MethodPost, "/v0/proxy-session", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("mint = %d: %s", w.Code, w.Body)
	}
	var cookie string
	for _, sc := range w.Result().Cookies() {
		if sc.Name == "embervm_proxy" {
			cookie = sc.Value
			if !sc.HttpOnly || sc.SameSite != http.SameSiteStrictMode {
				t.Errorf("cookie flags: httponly=%v samesite=%v", sc.HttpOnly, sc.SameSite)
			}
		}
	}
	if cookie == "" {
		t.Fatal("no embervm_proxy cookie set")
	}

	// Cookie-only request (no Authorization header — the iframe case).
	cookieCall := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		ctx, cancel := context.WithCancel(req.Context())
		defer cancel()
		req = req.WithContext(ctx)
		req.AddCookie(&http.Cookie{Name: "embervm_proxy", Value: cookie})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if w := cookieCall(http.MethodGet, "/v0/sandboxes/"+sb.ID+"/proxy/8080/"); w.Code != http.StatusOK || w.Body.String() != "guest-ok" {
		t.Errorf("cookie proxy = %d %q, want 200 guest-ok", w.Code, w.Body)
	}
	// The proxy-session cookie authenticated the request but must NOT be
	// forwarded into the untrusted guest (it is a reusable owner credential).
	if guestSaw.cookie != "" {
		t.Errorf("guest received cookie header %q — platform credential leaked", guestSaw.cookie)
	}
	// A bearer-authenticated proxy request likewise must not forward the token.
	call(h, http.MethodGet, "/v0/sandboxes/"+sb.ID+"/proxy/8080/", nil)
	if guestSaw.authz != "" {
		t.Errorf("guest received Authorization %q — bearer leaked", guestSaw.authz)
	}
	// The cookie must NOT authorize anything outside /proxy/.
	if w := cookieCall(http.MethodGet, "/v0/sandboxes"); w.Code != http.StatusUnauthorized {
		t.Errorf("cookie on API route = %d, want 401", w.Code)
	}
	if w := cookieCall(http.MethodGet, "/v0/sandboxes/"+sb.ID+"/files?path=/etc/hosts"); w.Code != http.StatusUnauthorized {
		t.Errorf("cookie on files route = %d, want 401", w.Code)
	}
	// Ownership still enforced through the cookie: bob's session, alice's box.
	w = callAs(h, "tok2", http.MethodPost, "/v0/proxy-session", nil)
	var bobCookie string
	for _, sc := range w.Result().Cookies() {
		if sc.Name == "embervm_proxy" {
			bobCookie = sc.Value
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/v0/sandboxes/"+sb.ID+"/proxy/8080/", nil)
	req.AddCookie(&http.Cookie{Name: "embervm_proxy", Value: bobCookie})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("bob's cookie on alice's sandbox = %d, want 404", rec.Code)
	}

	// Revocation.
	reqDel := httptest.NewRequest(http.MethodDelete, "/v0/proxy-session", nil)
	reqDel.Header.Set("Authorization", "Bearer tok")
	reqDel.AddCookie(&http.Cookie{Name: "embervm_proxy", Value: cookie})
	h.ServeHTTP(httptest.NewRecorder(), reqDel)
	if w := cookieCall(http.MethodGet, "/v0/sandboxes/"+sb.ID+"/proxy/8080/"); w.Code != http.StatusUnauthorized {
		t.Errorf("revoked cookie = %d, want 401", w.Code)
	}
}

// TestServerForkFlow drives the M5 branch API end to end against PG:
// checkpoint → list → fork-by-tag → lineage on the child → destroy guards.
func TestServerForkFlow(t *testing.T) {
	agent := &cpMockAgent{}
	h := newTestServer(t, agent)

	w := call(h, http.MethodPost, "/v0/templates", map[string]string{"name": "web", "image": "img"})
	var tpl Template
	_ = json.Unmarshal(w.Body.Bytes(), &tpl)
	w = call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl.ID})
	var parent Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &parent)

	// Checkpoint with a user tag.
	w = call(h, http.MethodPost, "/v0/sandboxes/"+parent.ID+"/checkpoints", map[string]string{"tag": "step-1"})
	if w.Code != http.StatusCreated {
		t.Fatalf("checkpoint = %d: %s", w.Code, w.Body)
	}
	var cp Checkpoint
	_ = json.Unmarshal(w.Body.Bytes(), &cp)
	if cp.Tag != "step-1" || cp.Layer != "p1" || cp.Seq != 1 {
		t.Fatalf("checkpoint = %+v", cp)
	}
	// Duplicate tag → 409; bad tag → 400.
	if w := call(h, http.MethodPost, "/v0/sandboxes/"+parent.ID+"/checkpoints", map[string]string{"tag": "step-1"}); w.Code != http.StatusConflict {
		t.Errorf("duplicate tag = %d, want 409", w.Code)
	}
	if w := call(h, http.MethodPost, "/v0/sandboxes/"+parent.ID+"/checkpoints", map[string]string{"tag": "../evil"}); w.Code != http.StatusBadRequest {
		t.Errorf("bad tag = %d, want 400", w.Code)
	}
	w = call(h, http.MethodGet, "/v0/sandboxes/"+parent.ID+"/checkpoints", nil)
	var cps []Checkpoint
	_ = json.Unmarshal(w.Body.Bytes(), &cps)
	if len(cps) != 1 {
		t.Fatalf("checkpoints = %+v", cps)
	}

	// Fork from the tag: child carries lineage, node, geometry; the agent
	// saw the parent's layer.
	w = call(h, http.MethodPost, "/v0/sandboxes/"+parent.ID+"/fork", map[string]string{"checkpoint": "step-1"})
	if w.Code != http.StatusCreated {
		t.Fatalf("fork = %d: %s", w.Code, w.Body)
	}
	var child Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &child)
	if child.ParentID != parent.ID || child.ForkedFrom != "step-1" || child.State != "RUNNING" {
		t.Fatalf("child = %+v", child)
	}
	if child.NodeID != parent.NodeID || child.MemoryMiB != parent.MemoryMiB {
		t.Fatalf("child placement/geometry = %+v (parent %+v)", child, parent)
	}
	if agent.lastFork.parent != parent.ID || agent.lastFork.layer != "p1" || agent.lastFork.newID != child.ID {
		t.Fatalf("agent saw fork %+v", agent.lastFork)
	}

	// D5 destroy guard: parent with a live fork refuses DELETE.
	if w := call(h, http.MethodDelete, "/v0/sandboxes/"+parent.ID, nil); w.Code != http.StatusConflict {
		t.Fatalf("delete forked parent = %d, want 409: %s", w.Code, w.Body)
	}
	if w := call(h, http.MethodDelete, "/v0/sandboxes/"+child.ID, nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete child = %d", w.Code)
	}
	if w := call(h, http.MethodDelete, "/v0/sandboxes/"+parent.ID, nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete parent after child gone = %d: %s", w.Code, w.Body)
	}
}

// TestServerForkAutoCheckpointAndQuota pins the branch-now UX (fork with no
// checkpoint makes one) and that forks are quota-counted.
func TestServerForkAutoCheckpointAndQuota(t *testing.T) {
	agent := &cpMockAgent{}
	h := newTestServer(t, agent)

	w := call(h, http.MethodPost, "/v0/templates", map[string]string{"name": "web", "image": "img"})
	var tpl Template
	_ = json.Unmarshal(w.Body.Bytes(), &tpl)
	w = call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl.ID})
	var parent Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &parent)

	w = call(h, http.MethodPost, "/v0/sandboxes/"+parent.ID+"/fork", nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("auto-checkpoint fork = %d: %s", w.Code, w.Body)
	}
	var child Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &child)
	if child.ForkedFrom != "cp1" {
		t.Fatalf("auto checkpoint tag = %q, want cp1", child.ForkedFrom)
	}

	// max_sandboxes is 2: parent + child fill the quota; a second fork 429s.
	if w := call(h, http.MethodPost, "/v0/sandboxes/"+parent.ID+"/fork", nil); w.Code != http.StatusTooManyRequests {
		t.Fatalf("over-quota fork = %d, want 429: %s", w.Code, w.Body)
	}

	// Cross-tenant: bob cannot see alice's sandbox to fork it.
	if w := callAs(h, "tok2", http.MethodPost, "/v0/sandboxes/"+parent.ID+"/fork", nil); w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant fork = %d, want 404", w.Code)
	}
}

// TestServerRollback pins the rollback flow: claim, layer switch, checkpoint
// pruning, and the live-fork 409 guard.
func TestServerRollback(t *testing.T) {
	agent := &cpMockAgent{}
	h := newTestServer(t, agent)

	w := call(h, http.MethodPost, "/v0/templates", map[string]string{"name": "web", "image": "img"})
	var tpl Template
	_ = json.Unmarshal(w.Body.Bytes(), &tpl)
	w = call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl.ID})
	var sb Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &sb)

	for _, tag := range []string{"a", "b"} {
		if w := call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/checkpoints", map[string]string{"tag": tag}); w.Code != http.StatusCreated {
			t.Fatalf("checkpoint %s = %d", tag, w.Code)
		}
	}

	// A fork off the NEWER checkpoint blocks rollback to the older one.
	w = call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/fork", map[string]string{"checkpoint": "b"})
	var child Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &child)
	if w := call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/rollback", map[string]string{"checkpoint": "a"}); w.Code != http.StatusConflict {
		t.Fatalf("rollback with live fork off newer checkpoint = %d, want 409: %s", w.Code, w.Body)
	}
	if w := call(h, http.MethodDelete, "/v0/sandboxes/"+child.ID, nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete child = %d", w.Code)
	}

	// Now the rollback goes through, prunes b, and the agent saw the layer.
	w = call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/rollback", map[string]string{"checkpoint": "a"})
	if w.Code != http.StatusOK {
		t.Fatalf("rollback = %d: %s", w.Code, w.Body)
	}
	if agent.lastRB.id != sb.ID || agent.lastRB.layer != "p1" {
		t.Fatalf("agent saw rollback %+v", agent.lastRB)
	}
	var after Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &after)
	if after.State != "RUNNING" {
		t.Fatalf("state after rollback = %s", after.State)
	}
	w = call(h, http.MethodGet, "/v0/sandboxes/"+sb.ID+"/checkpoints", nil)
	var cps []Checkpoint
	_ = json.Unmarshal(w.Body.Bytes(), &cps)
	if len(cps) != 1 || cps[0].Tag != "a" {
		t.Fatalf("checkpoints after rollback = %+v, want only a", cps)
	}
	// Rollback to an unknown checkpoint → 404.
	if w := call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/rollback", map[string]string{"checkpoint": "ghost"}); w.Code != http.StatusNotFound {
		t.Fatalf("rollback to ghost = %d, want 404", w.Code)
	}
}

// TestServerExecCheckpoint pins the time-travel primitive: exec with
// checkpoint:true snapshots first and returns the step's tag.
func TestServerExecCheckpoint(t *testing.T) {
	agent := &cpMockAgent{}
	h := newTestServer(t, agent)

	w := call(h, http.MethodPost, "/v0/templates", map[string]string{"name": "web", "image": "img"})
	var tpl Template
	_ = json.Unmarshal(w.Body.Bytes(), &tpl)
	w = call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl.ID})
	var sb Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &sb)

	w = call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/exec", map[string]any{
		"cmd": "echo", "checkpoint": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("exec+checkpoint = %d: %s", w.Code, w.Body)
	}
	var resp struct {
		Stdout     []byte `json:"stdout"`
		Checkpoint string `json:"checkpoint"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Checkpoint != "cp1" || string(resp.Stdout) != "out:echo" {
		t.Fatalf("exec+checkpoint resp = %+v (%s)", resp, w.Body)
	}
	w = call(h, http.MethodGet, "/v0/sandboxes/"+sb.ID+"/checkpoints", nil)
	var cps []Checkpoint
	_ = json.Unmarshal(w.Body.Bytes(), &cps)
	if len(cps) != 1 || cps[0].Tag != "cp1" {
		t.Fatalf("checkpoints = %+v", cps)
	}
	// Plain exec is unchanged: no checkpoint field appears.
	w = call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/exec", map[string]any{"cmd": "echo"})
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"checkpoint"`) {
		t.Fatalf("plain exec = %d: %s", w.Code, w.Body)
	}
}
