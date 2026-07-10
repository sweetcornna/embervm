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
	buildErr   error
	createErr  error
	snapSeq    int
	lastFork   struct{ parent, layer, newID string }
	lastRB     struct{ id, layer string }
	lastResize nodeapi.ResizeRequest
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
