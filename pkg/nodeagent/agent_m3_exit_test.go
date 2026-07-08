//go:build linux

package nodeagent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/embervm/embervm/pkg/chunkstore"
	"github.com/embervm/embervm/pkg/controlplane"
	"github.com/embervm/embervm/pkg/netns"
	"github.com/embervm/embervm/pkg/nodeagent"
	"github.com/embervm/embervm/pkg/storage"
)

// M3 exit criteria (docs/zh/03 §3): 冷归档恢复 < 10s 可交互; 归档成本达标.
// Latencies are CI-relative (ADR-0001). This is a full-stack test: real
// REST control plane + lifecycle engine + ZFS + warm/cold object stores.

// m3Env gathers every prerequisite or skips.
func m3Env(t *testing.T) (pool, dbURL string) {
	t.Helper()
	if os.Getenv("EMBERVM_KVM_TESTS") != "1" {
		t.Skip("set EMBERVM_KVM_TESTS=1 to run KVM tests")
	}
	requireL1(t)
	if os.Getenv("EMBERVM_COLD_DIR") == "" && os.Getenv("EMBERVM_COLD_ENDPOINT") == "" {
		t.Skip("set EMBERVM_COLD_DIR or EMBERVM_COLD_ENDPOINT to run cold-tier tests")
	}
	pool = os.Getenv("EMBERVM_ZFS_POOL")
	if pool == "" {
		t.Skip("set EMBERVM_ZFS_POOL to run the lifecycle flow test")
	}
	dbURL = os.Getenv("EMBERVM_TEST_DATABASE_URL")
	if os.Getenv("EMBERVM_PG_TESTS") != "1" || dbURL == "" {
		t.Skip("set EMBERVM_PG_TESTS=1 and EMBERVM_TEST_DATABASE_URL for the lifecycle flow test")
	}
	return pool, dbURL
}

// api is a minimal REST client against the in-process control plane.
type api struct {
	t    *testing.T
	base string
	hc   *http.Client
}

func (a *api) do(method, path string, body any, out any) int {
	a.t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			a.t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, a.base+path, rdr)
	if err != nil {
		a.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+controlplane.DevTokenName)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode/100 == 2 {
		if err := json.Unmarshal(data, out); err != nil {
			a.t.Fatalf("%s %s: decode %q: %v", method, path, data, err)
		}
	}
	if resp.StatusCode/100 != 2 {
		a.t.Logf("%s %s -> %d: %s", method, path, resp.StatusCode, data)
	}
	return resp.StatusCode
}

func (a *api) waitState(id, want string, timeout time.Duration) {
	a.t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		var sb struct {
			State string `json:"state"`
		}
		if code := a.do("GET", "/v0/sandboxes/"+id, nil, &sb); code == 200 {
			last = sb.State
			if sb.State == want {
				return
			}
			if sb.State == "FAILED" {
				a.t.Fatalf("sandbox %s FAILED while waiting for %s", id, want)
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	a.t.Fatalf("sandbox %s never reached %s (last: %s)", id, want, last)
}

// TestLifecycleTTLFlow drives the whole M3 tier chain through the real REST
// surface with a running lifecycle engine: HOT→WARM→COLD on TTLs, resume
// from WARM and (timed, <10s) from COLD with process continuity, archive
// cost report, RECYCLE to artifacts, and Manus-style selective restore.
func TestLifecycleTTLFlow(t *testing.T) {
	pool, dbURL := m3Env(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if out, err := exec.Command("zfs", "create", "-p", pool+"/m3").CombinedOutput(); err != nil {
		t.Fatalf("zfs create: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("zfs", "destroy", "-r", pool+"/m3").Run() })

	npool := netns.NewPool(os.Getenv("EMBERVM_SCRIPT_DIR"), 2)
	if err := npool.Setup(ctx); err != nil {
		t.Fatalf("netns pool: %v", err)
	}
	t.Cleanup(func() { _ = npool.Teardown(context.Background()) })

	agent, err := nodeagent.New(nodeagent.Config{
		Storage:        storage.NewZFSBackend(pool + "/m3"),
		Pool:           npool,
		WorkDir:        t.TempDir(),
		ChunkStoreDir:  filepath.Join(t.TempDir(), "chunks"),
		KernelPath:     os.Getenv("EMBERVM_KERNEL"),
		FCBin:          os.Getenv("EMBERVM_FC_BIN"),
		UffdHandlerBin: os.Getenv("EMBERVM_UFFD_BIN"),
		GuestdBin:      os.Getenv("EMBERVM_GUESTD_BIN"),
		RestoreMode:    "chunked",
		FCVersion:      os.Getenv("FC_VERSION"),
		KernelVersion:  "6.1.155",
	})
	if err != nil {
		t.Fatal(err)
	}

	store, err := controlplane.NewStore(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	l1, ok, err := chunkstore.L1FromEnv()
	if err != nil || !ok {
		t.Fatalf("L1 store: ok=%v err=%v", ok, err)
	}
	cold, ok, err := chunkstore.ColdFromEnv()
	if err != nil || !ok {
		t.Fatalf("cold store: ok=%v err=%v", ok, err)
	}

	engine := controlplane.NewEngine(store, agent.(*nodeagent.Agent), l1, cold, controlplane.EngineConfig{
		Tick:       300 * time.Millisecond,
		TTLWarm:    2 * time.Second,
		TTLCold:    3 * time.Second,
		TTLRecycle: 6 * time.Second,
		GCGrace:    time.Nanosecond,
	})
	go engine.Run(ctx)

	srv := httptest.NewServer(controlplane.NewServer(store, agent, controlplane.DevTokenStore(), l1, cold).Handler())
	t.Cleanup(srv.Close)
	a := &api{t: t, base: srv.URL, hc: srv.Client()}

	// --- build + create ----------------------------------------------------
	image := os.Getenv("EMBERVM_TEST_IMAGE")
	if image == "" {
		image = "alpine:3.20"
	}
	var tpl struct {
		ID string `json:"id"`
	}
	if code := a.do("POST", "/v0/templates", map[string]string{
		"name": fmt.Sprintf("m3-%d", time.Now().UnixNano()), "image": image,
	}, &tpl); code/100 != 2 {
		t.Fatalf("create template: HTTP %d", code)
	}
	var sb struct {
		ID string `json:"id"`
	}
	if code := a.do("POST", "/v0/sandboxes", map[string]any{
		"template_id": tpl.ID, "vcpus": 1, "memory_mib": 512, "data_disk_gib": 2,
		"artifact_paths": []string{"/artifact.txt"},
	}, &sb); code/100 != 2 {
		t.Fatalf("create sandbox: HTTP %d", code)
	}
	id := sb.ID
	t.Cleanup(func() { a.do("DELETE", "/v0/sandboxes/"+id, nil, nil) })

	putFile := func(path, content string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPut,
			srv.URL+"/v0/sandboxes/"+id+"/files?path="+path, strings.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+controlplane.DevTokenName)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			t.Fatalf("put %s: HTTP %d", path, resp.StatusCode)
		}
	}
	readFile := func(path string) string {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet,
			srv.URL+"/v0/sandboxes/"+id+"/files?path="+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+controlplane.DevTokenName)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode/100 != 2 {
			t.Fatalf("read %s: HTTP %d: %s", path, resp.StatusCode, data)
		}
		return string(data)
	}
	execIn := func(cmd string) {
		t.Helper()
		var out struct {
			ExitCode int `json:"exit_code"`
		}
		if code := a.do("POST", "/v0/sandboxes/"+id+"/exec", map[string]any{
			"cmd": "sh", "args": []string{"-c", cmd}, "timeout_s": 120,
		}, &out); code/100 != 2 || out.ExitCode != 0 {
			t.Fatalf("exec %q: HTTP %d exit %d", cmd, code, out.ExitCode)
		}
	}

	putFile("/artifact.txt", "precious result")
	execIn("dd if=/dev/urandom of=/dirty.bin bs=1M count=16 2>/dev/null && sync")

	// --- HOT → WARM (engine) → resume from WARM -----------------------------
	if code := a.do("POST", "/v0/sandboxes/"+id+"/pause", nil, nil); code/100 != 2 {
		t.Fatalf("pause 1: HTTP %d", code)
	}
	a.waitState(id, "PAUSED_WARM", 30*time.Second)
	t.Logf("engine demoted HOT→WARM (node-local state released)")

	if code := a.do("POST", "/v0/sandboxes/"+id+"/resume", nil, nil); code/100 != 2 {
		t.Fatalf("resume from WARM: HTTP %d", code)
	}
	a.waitState(id, "RUNNING", 15*time.Second)
	if got := readFile("/artifact.txt"); got != "precious result" {
		t.Fatalf("artifact after WARM resume = %q", got)
	}

	// --- WARM → COLD (synthetic full) → cost report → timed cold resume ----
	if code := a.do("POST", "/v0/sandboxes/"+id+"/pause", nil, nil); code/100 != 2 {
		t.Fatalf("pause 2: HTTP %d", code)
	}
	a.waitState(id, "ARCHIVED_COLD", 45*time.Second)
	t.Logf("engine archived WARM→COLD (synthetic full in cold store)")

	var rep struct {
		Tier         string  `json:"tier"`
		LogicalBytes int64   `json:"logical_bytes"`
		StoredBytes  int64   `json:"stored_bytes"`
		ChunkCount   int     `json:"chunk_count"`
		StoredRatio  float64 `json:"stored_ratio"`
		Layers       int     `json:"layers"`
	}
	if code := a.do("GET", "/v0/sandboxes/"+id+"/storage", nil, &rep); code != 200 {
		t.Fatalf("storage report: HTTP %d", code)
	}
	t.Logf("archive cost report: tier=%s layers=%d logical=%d stored=%d chunks=%d ratio=%.3f",
		rep.Tier, rep.Layers, rep.LogicalBytes, rep.StoredBytes, rep.ChunkCount, rep.StoredRatio)
	if rep.Tier != "cold" || rep.Layers != 1 {
		t.Errorf("cold report tier/layers = %s/%d, want cold/1 (synthetic full)", rep.Tier, rep.Layers)
	}
	if rep.StoredBytes <= 0 || rep.StoredRatio > 0.6 {
		t.Errorf("归档成本 gate: stored=%d ratio=%.3f, want >0 and <=0.6 of logical", rep.StoredBytes, rep.StoredRatio)
	}

	start := time.Now()
	if code := a.do("POST", "/v0/sandboxes/"+id+"/resume", nil, nil); code/100 != 2 {
		t.Fatalf("resume from COLD: HTTP %d", code)
	}
	a.waitState(id, "RUNNING", 15*time.Second)
	coldRestore := time.Since(start)
	t.Logf("cold-archive restore to interactive: %v", coldRestore)
	if coldRestore >= 10*time.Second {
		t.Errorf("cold restore took %v, want <10s (M3 exit criterion)", coldRestore)
	}
	if got := readFile("/artifact.txt"); got != "precious result" {
		t.Fatalf("artifact after COLD resume = %q", got)
	}
	execIn("test -s /dirty.bin") // disk state followed through the chain

	// --- chain restart: hot resume after the post-cold-restore pause -------
	// The pause after a cold restore roots a fresh Full chain; the resume
	// must see ONLY the new chain (a stale synthetic layer-cold.json in the
	// snap dir would make the manifest chain unresolvable).
	if code := a.do("POST", "/v0/sandboxes/"+id+"/pause", nil, nil); code/100 != 2 {
		t.Fatalf("pause 3: HTTP %d", code)
	}
	resumed := false
	for attempt := 0; attempt < 3 && !resumed; attempt++ {
		// The engine may demote to WARM under us; both tiers must resume.
		if code := a.do("POST", "/v0/sandboxes/"+id+"/resume", nil, nil); code/100 == 2 {
			resumed = true
		} else {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if !resumed {
		t.Fatal("resume after chain restart failed")
	}
	a.waitState(id, "RUNNING", 15*time.Second)
	if got := readFile("/artifact.txt"); got != "precious result" {
		t.Fatalf("artifact after chain-restart resume = %q", got)
	}
	t.Logf("chain-restart resume verified (fresh Full chain after cold restore)")

	// --- COLD → RECYCLED → selective restore --------------------------------
	if code := a.do("POST", "/v0/sandboxes/"+id+"/pause", nil, nil); code/100 != 2 {
		t.Fatalf("pause 4: HTTP %d", code)
	}
	a.waitState(id, "RECYCLED", 90*time.Second)
	t.Logf("engine recycled the sandbox (artifacts only)")

	if ok, err := cold.HasObject(ctx, nodeagent.KeyArtifacts(id)); err != nil || !ok {
		t.Fatalf("artifacts.tar.zst missing from cold store: ok=%v err=%v", ok, err)
	}
	if ok, _ := cold.HasObject(ctx, nodeagent.KeySnapshotJSON(id)); ok {
		t.Fatal("snapshot descriptor survived recycle")
	}

	var restored struct {
		Sandbox struct {
			ID string `json:"id"`
		} `json:"sandbox"`
	}
	if code := a.do("POST", "/v0/sandboxes/"+id+"/restore-artifacts", nil, &restored); code != 200 {
		t.Fatalf("restore-artifacts: HTTP %d", code)
	}
	newID := restored.Sandbox.ID
	t.Cleanup(func() { a.do("DELETE", "/v0/sandboxes/"+newID, nil, nil) })

	req, err := http.NewRequest(http.MethodGet,
		srv.URL+"/v0/sandboxes/"+newID+"/files?path=/artifact.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+controlplane.DevTokenName)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(data) != "precious result" {
		t.Fatalf("selective restore: HTTP %d, artifact = %q", resp.StatusCode, data)
	}
	t.Logf("selective restore verified: new sandbox %s carries the artifact", newID)
}
