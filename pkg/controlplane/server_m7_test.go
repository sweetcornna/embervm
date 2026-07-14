package controlplane

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"

	"github.com/embervm/embervm/pkg/chunkstore"
	"github.com/embervm/embervm/pkg/nodeagent"
)

// mkTemplate creates a READY template and returns its id.
func mkTemplate(t *testing.T, h http.Handler) string {
	t.Helper()
	w := call(h, http.MethodPost, "/v0/templates", map[string]string{"name": "web", "image": "alpine:3.20"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create template = %d: %s", w.Code, w.Body)
	}
	var tpl Template
	_ = json.Unmarshal(w.Body.Bytes(), &tpl)
	return tpl.ID
}

// TestServerCreateDefaultElastic pins the M7 create contract: a no-geometry
// create is elastic with the platform defaults, explicit geometry keeps its
// M6 meaning, and the node request mirrors the resolved row.
func TestServerCreateDefaultElastic(t *testing.T) {
	mock := &cpMockAgent{}
	h := newTestServer(t, mock)
	tpl := mkTemplate(t, h)

	// Bare create → default elastic, autoscale on.
	w := call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl})
	if w.Code != http.StatusCreated {
		t.Fatalf("bare create = %d: %s", w.Code, w.Body)
	}
	var sb Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &sb)
	if sb.VCPUs != 1 || sb.MemoryMiB != 256 || sb.DataDiskGiB != 15 {
		t.Errorf("base = %d/%dMiB disk %d, want 1/256 disk 15", sb.VCPUs, sb.MemoryMiB, sb.DataDiskGiB)
	}
	if sb.MaxVCPUs != 4 || sb.MaxMemoryMiB != 4096 {
		t.Errorf("ceiling = %d/%dMiB, want 4/4096", sb.MaxVCPUs, sb.MaxMemoryMiB)
	}
	if sb.BaseVCPUs != 1 || sb.BaseMemoryMiB != 256 {
		t.Errorf("floors = %d/%dMiB, want 1/256", sb.BaseVCPUs, sb.BaseMemoryMiB)
	}
	if !sb.Autoscale {
		t.Error("bare create should have autoscale on")
	}
	// The node saw the same resolved geometry (no zeros for its own fills).
	if mock.lastCreate.VCPUs != 1 || mock.lastCreate.MemoryMiB != 256 ||
		mock.lastCreate.MaxMemoryMiB != 4096 || mock.lastCreate.MaxVCPUs != 4 {
		t.Errorf("node request = %+v, want resolved 1/256 max 4/4096", mock.lastCreate)
	}

	// Explicit base, no max → fixed geometry exactly as M6.
	w = call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl, "vcpus": 2, "memory_mib": 512})
	if w.Code != http.StatusCreated {
		t.Fatalf("fixed create = %d: %s", w.Code, w.Body)
	}
	var fixed Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &fixed)
	if fixed.MaxMemoryMiB != 0 || fixed.MaxVCPUs != 0 || fixed.Autoscale {
		t.Errorf("explicit base should stay fixed: max %d/%d autoscale %v",
			fixed.MaxVCPUs, fixed.MaxMemoryMiB, fixed.Autoscale)
	}
	if fixed.VCPUs != 2 || fixed.MemoryMiB != 512 || fixed.DataDiskGiB != 15 {
		t.Errorf("fixed base = %d/%dMiB disk %d, want 2/512 disk 15", fixed.VCPUs, fixed.MemoryMiB, fixed.DataDiskGiB)
	}

	// Explicit base + autoscale without a ceiling → 400.
	w = call(h, http.MethodPost, "/v0/sandboxes",
		map[string]any{"template_id": tpl, "memory_mib": 512, "autoscale": true})
	if w.Code != http.StatusBadRequest {
		t.Errorf("base+autoscale, no max = %d, want 400: %s", w.Code, w.Body)
	}
}

// TestServerCreateCeilingOnly covers the console's preset shape: a ceiling
// with no base gets the default base and autoscale.
func TestServerCreateCeilingOnly(t *testing.T) {
	mock := &cpMockAgent{}
	h := newTestServer(t, mock)
	tpl := mkTemplate(t, h)

	w := call(h, http.MethodPost, "/v0/sandboxes",
		map[string]any{"template_id": tpl, "max_memory_mib": 8000}) // not slot-aligned
	if w.Code != http.StatusCreated {
		t.Fatalf("ceiling-only create = %d: %s", w.Code, w.Body)
	}
	var sb Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &sb)
	if sb.VCPUs != 1 || sb.MemoryMiB != 256 {
		t.Errorf("base = %d/%dMiB, want default 1/256", sb.VCPUs, sb.MemoryMiB)
	}
	// 256 + roundUpToSlot(8000-256) = 256 + 7808 = 8064.
	if sb.MaxMemoryMiB != 8064 {
		t.Errorf("ceiling = %d, want slot-rounded 8064", sb.MaxMemoryMiB)
	}
	if sb.MaxVCPUs != 0 {
		t.Errorf("vcpu ceiling = %d, want 0 (memory-only elastic)", sb.MaxVCPUs)
	}
	if !sb.Autoscale {
		t.Error("ceiling-only create should default autoscale on")
	}

	// autoscale:false is respected alongside a ceiling.
	w = call(h, http.MethodPost, "/v0/sandboxes",
		map[string]any{"template_id": tpl, "max_memory_mib": 1024, "autoscale": false})
	if w.Code != http.StatusCreated {
		t.Fatalf("ceiling-only autoscale:false = %d: %s", w.Code, w.Body)
	}
	var manual Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &manual)
	if manual.Autoscale {
		t.Error("autoscale:false must win over the default")
	}
	if manual.MaxMemoryMiB != 1024 {
		t.Errorf("ceiling = %d, want 1024", manual.MaxMemoryMiB)
	}
}

// TestServerCreateElasticDisabled pins the escape hatch: Disabled restores
// the pre-M7 wire behavior byte for byte (zeros stored, node fills).
func TestServerCreateElasticDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := testStore(t)
	tokens := NewTokenStore(map[string]TokenInfo{"tok": {Owner: "alice", MaxSandboxes: 2}})
	srv := NewServer(store, &cpMockAgent{}, tokens, nil, nil)
	srv.Elastic = ElasticDefaults{Disabled: true}
	h := srv.Handler()
	tpl := mkTemplate(t, h)

	w := call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl})
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body)
	}
	var sb Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &sb)
	if sb.VCPUs != 0 || sb.MemoryMiB != 0 || sb.MaxMemoryMiB != 0 || sb.Autoscale {
		t.Errorf("Disabled create should store legacy zeros, got %+v", sb)
	}

	// The legacy guard still rejects autoscale without a ceiling.
	w = call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl, "autoscale": true})
	if w.Code != http.StatusBadRequest {
		t.Errorf("Disabled autoscale-no-ceiling = %d, want 400: %s", w.Code, w.Body)
	}
	// ...and a ceiling without a base (the pre-M7 validateCeilings arm).
	w = call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl, "max_memory_mib": 1024})
	if w.Code != http.StatusBadRequest {
		t.Errorf("Disabled ceiling-without-base = %d, want 400: %s", w.Code, w.Body)
	}
}

// TestServerSetAutoscale covers the M7 runtime toggle and its audit trail.
func TestServerSetAutoscale(t *testing.T) {
	h := newTestServer(t, &cpMockAgent{})
	tpl := mkTemplate(t, h)

	w := call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl})
	var sb Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &sb)
	if !sb.Autoscale {
		t.Fatal("default create should start with autoscale on")
	}

	// Toggle off.
	w = call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/autoscale", map[string]any{"autoscale": false})
	if w.Code != http.StatusOK {
		t.Fatalf("toggle off = %d: %s", w.Code, w.Body)
	}
	var got Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Autoscale {
		t.Error("toggle off did not stick in the response")
	}
	w = call(h, http.MethodGet, "/v0/sandboxes/"+sb.ID, nil)
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Autoscale {
		t.Error("toggle off did not persist")
	}

	// Idempotent repeat, then back on.
	if w = call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/autoscale", map[string]any{"autoscale": false}); w.Code != http.StatusOK {
		t.Fatalf("idempotent toggle = %d: %s", w.Code, w.Body)
	}
	if w = call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/autoscale", map[string]any{"autoscale": true}); w.Code != http.StatusOK {
		t.Fatalf("toggle on = %d: %s", w.Code, w.Body)
	}

	// The toggles are on the timeline (idempotent repeat writes nothing).
	events := sandboxEventDetails(t, h, sb.ID, "autoscale_config")
	if len(events) != 2 {
		t.Errorf("autoscale_config events = %d, want 2", len(events))
	}

	// A fixed-geometry sandbox cannot enable it.
	w = call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl, "vcpus": 1, "memory_mib": 256})
	var fixed Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &fixed)
	w = call(h, http.MethodPost, "/v0/sandboxes/"+fixed.ID+"/autoscale", map[string]any{"autoscale": true})
	if w.Code != http.StatusConflict {
		t.Errorf("enable on fixed geometry = %d, want 409: %s", w.Code, w.Body)
	}
}

// TestServerResizeEvent asserts the resize verb records a typed timeline row.
func TestServerResizeEvent(t *testing.T) {
	h := newTestServer(t, &cpMockAgent{})
	tpl := mkTemplate(t, h)

	w := call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl})
	var sb Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &sb)

	if w = call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/resize", map[string]any{"memory_mib": 512}); w.Code != http.StatusOK {
		t.Fatalf("resize = %d: %s", w.Code, w.Body)
	}
	events := sandboxEventDetails(t, h, sb.ID, "resize")
	if len(events) != 1 {
		t.Fatalf("resize events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Actor != "user" || ev.Reason != "manual" {
		t.Errorf("resize event actor/reason = %s/%s, want user/manual", ev.Actor, ev.Reason)
	}
	if ev.MemoryMiB == nil || ev.MemoryMiB[0] != 256 || ev.MemoryMiB[1] != 512 {
		t.Errorf("resize event memory = %v, want [256 512]", ev.MemoryMiB)
	}
	if ev.VCPUs != nil {
		t.Errorf("vcpus did not move but event has %v", ev.VCPUs)
	}
}

// TestServerListNodesOversell asserts the M7 nodes fields sum bases and
// ceilings (fixed rows count effective on both ends).
func TestServerListNodesOversell(t *testing.T) {
	h := newTestServer(t, &cpMockAgent{})
	tpl := mkTemplate(t, h)

	// One default-elastic (256 base / 4096 ceiling) + one fixed 512.
	if w := call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl}); w.Code != http.StatusCreated {
		t.Fatalf("elastic create = %d: %s", w.Code, w.Body)
	}
	if w := call(h, http.MethodPost, "/v0/sandboxes", map[string]any{"template_id": tpl, "vcpus": 1, "memory_mib": 512}); w.Code != http.StatusCreated {
		t.Fatalf("fixed create = %d: %s", w.Code, w.Body)
	}

	w := call(h, http.MethodGet, "/v0/nodes", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list nodes = %d: %s", w.Code, w.Body)
	}
	var nodes []struct {
		ID           string `json:"id"`
		UsedMiB      int    `json:"used_mib"`
		BaseMiB      int    `json:"base_mib"`
		CeilingMiB   int    `json:"ceiling_mib"`
		CeilingVCPUs int    `json:"ceiling_vcpus"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &nodes)
	if len(nodes) != 1 {
		t.Fatalf("nodes = %d, want 1 (local)", len(nodes))
	}
	n := nodes[0]
	if n.UsedMiB != 256+512 || n.BaseMiB != 256+512 {
		t.Errorf("used/base = %d/%d, want 768/768", n.UsedMiB, n.BaseMiB)
	}
	if n.CeilingMiB != 4096+512 {
		t.Errorf("ceiling_mib = %d, want 4608 (elastic max + fixed effective)", n.CeilingMiB)
	}
	if n.CeilingVCPUs != 4+1 {
		t.Errorf("ceiling_vcpus = %d, want 5", n.CeilingVCPUs)
	}
}

// TestServerRestoreArtifactsKeepsElastic pins the M7 audit fix: a RECYCLED
// default-elastic sandbox restored via restore-artifacts keeps its ceilings
// and autoscale, and cold-boots at the BASE floor (not the last effective
// geometry) — dropping either would silently turn it fixed.
func TestServerRestoreArtifactsKeepsElastic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := testStore(t)
	tokens := NewTokenStore(map[string]TokenInfo{"tok": {Owner: "alice", MaxSandboxes: 5}})
	cold, err := chunkstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mock := &cpMockAgent{}
	h := NewServer(store, mock, tokens, nil, cold).Handler()
	tpl := mkTemplate(t, h)

	w := call(h, http.MethodPost, "/v0/sandboxes",
		map[string]any{"template_id": tpl, "artifact_paths": []string{"/data"}})
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body)
	}
	var sb Sandbox
	_ = json.Unmarshal(w.Body.Bytes(), &sb)
	// Grow so the effective geometry sits above the base floor.
	if w = call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/resize", map[string]any{"memory_mib": 512}); w.Code != http.StatusOK {
		t.Fatalf("resize = %d: %s", w.Code, w.Body)
	}
	ctx := context.Background()
	if err := store.SetSandboxState(ctx, sb.ID, "RUNNING", "RECYCLED", "", ""); err != nil {
		t.Fatal(err)
	}

	// A minimal artifacts tarball in the cold store.
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	if err := tw.WriteHeader(&tar.Header{Name: "data/x", Mode: 0o644, Size: 2}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte("ok"))
	_ = tw.Close()
	var zBuf bytes.Buffer
	zw, _ := zstd.NewWriter(&zBuf)
	_, _ = zw.Write(tarBuf.Bytes())
	_ = zw.Close()
	if err := cold.PutObject(ctx, nodeagent.KeyArtifacts(sb.ID), bytes.NewReader(zBuf.Bytes()), int64(zBuf.Len())); err != nil {
		t.Fatal(err)
	}

	w = call(h, http.MethodPost, "/v0/sandboxes/"+sb.ID+"/restore-artifacts", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("restore-artifacts = %d: %s", w.Code, w.Body)
	}
	var out struct {
		Sandbox Sandbox `json:"sandbox"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	got := out.Sandbox
	if got.MaxMemoryMiB != 4096 || got.MaxVCPUs != 4 || !got.Autoscale {
		t.Errorf("restored sandbox lost elasticity: max %d/%dMiB autoscale %v",
			got.MaxVCPUs, got.MaxMemoryMiB, got.Autoscale)
	}
	if got.MemoryMiB != 256 || got.BaseMemoryMiB != 256 {
		t.Errorf("restored boot geometry = %dMiB (base %d), want the 256 base floor", got.MemoryMiB, got.BaseMemoryMiB)
	}
	if mock.lastCreate.MaxMemoryMiB != 4096 || mock.lastCreate.MaxVCPUs != 4 ||
		mock.lastCreate.MemoryMiB != 256 {
		t.Errorf("node request lost the elastic geometry: %+v", mock.lastCreate)
	}
}

// sandboxEventDetails fetches one sandbox's timeline and returns the parsed
// resource details of the given kind, oldest first.
func sandboxEventDetails(t *testing.T, h http.Handler, id, kind string) []ResourceEventDetail {
	t.Helper()
	w := call(h, http.MethodGet, "/v0/sandboxes/"+id+"/events", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("events = %d: %s", w.Code, w.Body)
	}
	var body struct {
		Events []SandboxEvent `json:"events"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	var out []ResourceEventDetail
	for i := len(body.Events) - 1; i >= 0; i-- { // newest-first → oldest first
		e := body.Events[i]
		if len(e.Detail) == 0 {
			continue
		}
		var d ResourceEventDetail
		if err := json.Unmarshal(e.Detail, &d); err != nil || d.Kind != kind {
			continue
		}
		out = append(out, d)
	}
	return out
}
