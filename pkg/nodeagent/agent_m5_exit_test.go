//go:build linux

package nodeagent_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/embervm/embervm/pkg/controlplane"
	"github.com/embervm/embervm/pkg/netns"
	"github.com/embervm/embervm/pkg/nodeagent"
	"github.com/embervm/embervm/pkg/storage"
)

// M5 exit criteria (docs/zh/03 §3): 单沙箱 fork 出 10 分支并行执行,父实例
// 不停顿; time-travel 每步自动快照 + 任意步 fork 重放. Full-stack: real REST
// control plane + PG + jailed/chunked agent + ZFS + L1. Latencies are
// CI-relative (ADR-0001).

// m5Stack boots the REST stack for the M5 gates or skips.
func m5Stack(t *testing.T, subtree string, poolSize int) *api {
	t.Helper()
	if os.Getenv("EMBERVM_KVM_TESTS") != "1" {
		t.Skip("set EMBERVM_KVM_TESTS=1 to run KVM tests")
	}
	if os.Geteuid() != 0 {
		t.Skip("KVM tests need root")
	}
	jailerBin := os.Getenv("EMBERVM_JAILER_BIN")
	if jailerBin == "" {
		t.Skip("set EMBERVM_JAILER_BIN to run the M5 gates")
	}
	requireL1(t)
	pool := os.Getenv("EMBERVM_ZFS_POOL")
	if pool == "" {
		t.Skip("set EMBERVM_ZFS_POOL to run the M5 gates")
	}
	dbURL := os.Getenv("EMBERVM_TEST_DATABASE_URL")
	if os.Getenv("EMBERVM_PG_TESTS") != "1" || dbURL == "" {
		t.Skip("set EMBERVM_PG_TESTS=1 and EMBERVM_TEST_DATABASE_URL for the M5 gates")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if out, err := exec.Command("zfs", "create", "-p", pool+"/"+subtree).CombinedOutput(); err != nil {
		t.Fatalf("zfs create: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("zfs", "destroy", "-r", pool+"/"+subtree).Run() })

	npool := netns.NewPool(os.Getenv("EMBERVM_SCRIPT_DIR"), poolSize)
	if err := npool.Setup(ctx); err != nil {
		t.Fatalf("netns pool: %v", err)
	}
	t.Cleanup(func() { _ = npool.Teardown(context.Background()) })

	agent, err := nodeagent.New(nodeagent.Config{
		Storage:          storage.NewZFSBackend(pool + "/" + subtree),
		Pool:             npool,
		WorkDir:          t.TempDir(),
		ChunkStoreDir:    t.TempDir() + "/chunks",
		KernelPath:       os.Getenv("EMBERVM_KERNEL"),
		FCBin:            os.Getenv("EMBERVM_FC_BIN"),
		UffdHandlerBin:   os.Getenv("EMBERVM_UFFD_BIN"),
		GuestdBin:        os.Getenv("EMBERVM_GUESTD_BIN"),
		RestoreMode:      "chunked",
		JailerBin:        jailerBin,
		JailerChrootBase: t.TempDir(),
		FCVersion:        os.Getenv("FC_VERSION"),
		KernelVersion:    "6.1.155",
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
	srv := httptest.NewServer(controlplane.NewServer(store, agent, controlplane.DevTokenStore(), nil, nil).Handler())
	t.Cleanup(srv.Close)
	return &api{t: t, base: srv.URL, hc: srv.Client()}
}

// m5Sandbox builds a template and creates one sandbox through the REST API.
func m5Sandbox(t *testing.T, a *api, name string) string {
	t.Helper()
	image := os.Getenv("EMBERVM_TEST_IMAGE")
	if image == "" {
		image = "alpine:3.20"
	}
	var tpl struct {
		ID string `json:"id"`
	}
	if code := a.do("POST", "/v0/templates", map[string]string{
		"name": fmt.Sprintf("%s-%d", name, time.Now().UnixNano()), "image": image,
	}, &tpl); code/100 != 2 {
		t.Fatalf("create template: HTTP %d", code)
	}
	var sb struct {
		ID string `json:"id"`
	}
	if code := a.do("POST", "/v0/sandboxes", map[string]any{
		"template_id": tpl.ID, "vcpus": 1, "memory_mib": 256, "data_disk_gib": 1,
	}, &sb); code/100 != 2 {
		t.Fatalf("create sandbox: HTTP %d", code)
	}
	t.Cleanup(func() { a.do("DELETE", "/v0/sandboxes/"+sb.ID, nil, nil) })
	return sb.ID
}

func (a *api) execIn(id, cmd string) int {
	a.t.Helper()
	var out struct {
		ExitCode int `json:"exit_code"`
	}
	if code := a.do("POST", "/v0/sandboxes/"+id+"/exec", map[string]any{
		"cmd": "sh", "args": []string{"-c", cmd}, "timeout_s": 60,
	}, &out); code/100 != 2 {
		a.t.Fatalf("exec %q in %s: HTTP %d", cmd, id, code)
	}
	return out.ExitCode
}

func (a *api) fileIn(id, path string) (int, string) {
	a.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, a.base+"/v0/sandboxes/"+id+"/files?path="+path, nil)
	req.Header.Set("Authorization", "Bearer "+controlplane.DevTokenName)
	resp, err := a.hc.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

// counterLines samples the parent's background counter.
func (a *api) counterLines(id string) int {
	a.t.Helper()
	code, body := a.fileIn(id, "/count")
	if code != 200 {
		a.t.Fatalf("read /count: HTTP %d", code)
	}
	return strings.Count(body, "\n")
}

// TestFork10Branches is THE M5 exit gate (退出标准 verbatim): one sandbox
// forks 10 branches that run in parallel while the parent keeps executing —
// its background counter grows monotonically across the whole fork window
// and its state never leaves RUNNING.
func TestFork10Branches(t *testing.T) {
	a := m5Stack(t, "m5e", 14)
	parent := m5Sandbox(t, a, "m5-fork10")

	// A background pulse in the parent: liveness we can sample.
	if rc := a.execIn(parent, "sh -c 'while :; do date +%s%N >> /count; sleep 0.2; done' >/dev/null 2>&1 & echo started"); rc != 0 {
		t.Fatalf("start counter: exit %d", rc)
	}
	if rc := a.execIn(parent, "echo parent-state > /origin && sync"); rc != 0 {
		t.Fatalf("write origin: exit %d", rc)
	}

	var cp struct {
		Tag   string `json:"tag"`
		Layer string `json:"layer"`
	}
	cpStart := time.Now()
	if code := a.do("POST", "/v0/sandboxes/"+parent+"/checkpoints", map[string]string{"tag": "branch"}, &cp); code != 201 {
		t.Fatalf("checkpoint: HTTP %d", code)
	}
	t.Logf("checkpoint %s (%s) in %v", cp.Tag, cp.Layer, time.Since(cpStart))

	n0 := a.counterLines(parent)

	// 10 parallel forks off the one checkpoint.
	type forkRes struct {
		id  string
		dur time.Duration
		err error
	}
	results := make(chan forkRes, 10)
	var wg sync.WaitGroup
	forkStart := time.Now()
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var child struct {
				ID    string `json:"id"`
				State string `json:"state"`
			}
			start := time.Now()
			if code := a.do("POST", "/v0/sandboxes/"+parent+"/fork", map[string]string{"checkpoint": "branch"}, &child); code != 201 {
				results <- forkRes{err: fmt.Errorf("fork: HTTP %d", code)}
				return
			}
			results <- forkRes{id: child.ID, dur: time.Since(start)}
		}()
	}
	wg.Wait()
	close(results)
	var kids []string
	var durs []time.Duration
	for r := range results {
		if r.err != nil {
			t.Fatal(r.err)
		}
		kids = append(kids, r.id)
		durs = append(durs, r.dur)
	}
	t.Cleanup(func() {
		for _, id := range kids {
			a.do("DELETE", "/v0/sandboxes/"+id, nil, nil)
		}
	})
	if len(kids) != 10 {
		t.Fatalf("only %d/10 forks", len(kids))
	}
	t.Logf("10 forks in %v total, per-fork %v (P50 %v)", time.Since(forkStart), durs, percentile(durs, 0.5))

	// 父实例不停顿: the counter kept growing across the fork window and the
	// parent never left RUNNING.
	n1 := a.counterLines(parent)
	if n1 <= n0 {
		t.Fatalf("parent counter stalled across forks: %d -> %d", n0, n1)
	}
	var psb struct {
		State string `json:"state"`
	}
	if code := a.do("GET", "/v0/sandboxes/"+parent, nil, &psb); code != 200 || psb.State != "RUNNING" {
		t.Fatalf("parent state = %s (HTTP %d)", psb.State, code)
	}

	// Every branch executes in parallel: exec-verified, sees the checkpoint
	// state, and diverges without touching parent or siblings.
	var vwg sync.WaitGroup
	errs := make(chan error, 10)
	for i, id := range kids {
		vwg.Add(1)
		go func(i int, id string) {
			defer vwg.Done()
			if code, got := a.fileIn(id, "/origin"); code != 200 || strings.TrimSpace(got) != "parent-state" {
				errs <- fmt.Errorf("child %s /origin = %d %q", id, code, got)
				return
			}
			if rc := a.execIn(id, fmt.Sprintf("echo %d > /branch && sync", i)); rc != 0 {
				errs <- fmt.Errorf("child %s exec exit %d", id, rc)
				return
			}
			errs <- nil
		}(i, id)
	}
	vwg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if code, _ := a.fileIn(parent, "/branch"); code == 200 {
		t.Fatal("parent sees a child's /branch write")
	}
	if code, got := a.fileIn(kids[0], "/branch"); code != 200 || strings.TrimSpace(got) != "0" {
		t.Fatalf("child 0 /branch = %q (siblings bleeding?)", got)
	}
	n2 := a.counterLines(parent)
	if n2 <= n1 {
		t.Fatalf("parent counter stalled during branch execution: %d -> %d", n1, n2)
	}
	t.Logf("10 branches parallel-verified; parent counter %d -> %d -> %d (never stalled)", n0, n1, n2)
}

// TestRollbackCheckpoint drives rollback through REST: discard past the
// checkpoint, guard against live forks off newer checkpoints, keep working
// afterwards.
func TestRollbackCheckpoint(t *testing.T) {
	a := m5Stack(t, "m5r2", 4)
	sb := m5Sandbox(t, a, "m5-rollback")

	if rc := a.execIn(sb, "echo keep > /a && sync"); rc != 0 {
		t.Fatal("write /a")
	}
	if code := a.do("POST", "/v0/sandboxes/"+sb+"/checkpoints", map[string]string{"tag": "keep"}, nil); code != 201 {
		t.Fatalf("checkpoint keep: HTTP %d", code)
	}
	if rc := a.execIn(sb, "echo discard > /b && sync"); rc != 0 {
		t.Fatal("write /b")
	}
	if code := a.do("POST", "/v0/sandboxes/"+sb+"/checkpoints", map[string]string{"tag": "later"}, nil); code != 201 {
		t.Fatalf("checkpoint later: HTTP %d", code)
	}

	// A live fork off the newer checkpoint blocks the rollback (409).
	var child struct {
		ID string `json:"id"`
	}
	if code := a.do("POST", "/v0/sandboxes/"+sb+"/fork", map[string]string{"checkpoint": "later"}, &child); code != 201 {
		t.Fatalf("fork later: HTTP %d", code)
	}
	if code := a.do("POST", "/v0/sandboxes/"+sb+"/rollback", map[string]string{"checkpoint": "keep"}, nil); code != http.StatusConflict {
		t.Fatalf("rollback with live fork = HTTP %d, want 409", code)
	}
	if code := a.do("DELETE", "/v0/sandboxes/"+child.ID, nil, nil); code != 204 {
		t.Fatalf("delete child: HTTP %d", code)
	}

	rbStart := time.Now()
	if code := a.do("POST", "/v0/sandboxes/"+sb+"/rollback", map[string]string{"checkpoint": "keep"}, nil); code != 200 {
		t.Fatalf("rollback: HTTP %d", code)
	}
	t.Logf("rollback to interactive in %v", time.Since(rbStart))
	if code, got := a.fileIn(sb, "/a"); code != 200 || strings.TrimSpace(got) != "keep" {
		t.Fatalf("/a after rollback = %d %q", code, got)
	}
	if code, _ := a.fileIn(sb, "/b"); code == 200 {
		t.Fatal("post-checkpoint /b survived rollback")
	}
	// The discarded checkpoint is pruned; the chain keeps working.
	var cps []struct {
		Tag string `json:"tag"`
	}
	if code := a.do("GET", "/v0/sandboxes/"+sb+"/checkpoints", nil, &cps); code != 200 || len(cps) != 1 || cps[0].Tag != "keep" {
		t.Fatalf("checkpoints after rollback = %+v", cps)
	}
	if code := a.do("POST", "/v0/sandboxes/"+sb+"/checkpoints", map[string]string{"tag": "again"}, nil); code != 201 {
		t.Fatalf("re-checkpoint after rollback: HTTP %d", code)
	}
	if rc := a.execIn(sb, "test -f /a"); rc != 0 {
		t.Fatal("sandbox unhealthy after rollback + re-checkpoint")
	}
}

// TestTimeTravelReplay drives 时光回溯: every exec step auto-checkpoints
// (state BEFORE the command), and forking any step's tag replays it.
func TestTimeTravelReplay(t *testing.T) {
	a := m5Stack(t, "m5t", 4)
	sb := m5Sandbox(t, a, "m5-ttravel")

	step := func(cmd string) string {
		t.Helper()
		var out struct {
			ExitCode   int    `json:"exit_code"`
			Checkpoint string `json:"checkpoint"`
		}
		if code := a.do("POST", "/v0/sandboxes/"+sb+"/exec", map[string]any{
			"cmd": "sh", "args": []string{"-c", cmd}, "timeout_s": 60, "checkpoint": true,
		}, &out); code/100 != 2 || out.ExitCode != 0 {
			t.Fatalf("step %q: HTTP %d exit %d", cmd, code, out.ExitCode)
		}
		if out.Checkpoint == "" {
			t.Fatalf("step %q returned no checkpoint tag", cmd)
		}
		return out.Checkpoint
	}

	_ = step("echo one >> /log && sync")
	t2 := step("echo two >> /log && sync")
	_ = step("echo three >> /log && sync")
	if code, got := a.fileIn(sb, "/log"); code != 200 || got != "one\ntwo\nthree\n" {
		t.Fatalf("/log = %q", got)
	}

	// Fork step 2's checkpoint: the branch sees the state BEFORE step 2 and
	// replays history differently; the parent's timeline is untouched.
	var child struct {
		ID string `json:"id"`
	}
	if code := a.do("POST", "/v0/sandboxes/"+sb+"/fork", map[string]string{"checkpoint": t2}, &child); code != 201 {
		t.Fatalf("fork %s: HTTP %d", t2, code)
	}
	t.Cleanup(func() { a.do("DELETE", "/v0/sandboxes/"+child.ID, nil, nil) })
	if code, got := a.fileIn(child.ID, "/log"); code != 200 || got != "one\n" {
		t.Fatalf("child /log at %s = %q, want pre-step-2 state", t2, got)
	}
	if rc := a.execIn(child.ID, "echo TWO >> /log && sync"); rc != 0 {
		t.Fatal("replay step 2 in child")
	}
	if code, got := a.fileIn(child.ID, "/log"); code != 200 || got != "one\nTWO\n" {
		t.Fatalf("child replayed /log = %q", got)
	}
	if code, got := a.fileIn(sb, "/log"); code != 200 || got != "one\ntwo\nthree\n" {
		t.Fatalf("parent /log diverged: %q", got)
	}
	t.Logf("time-travel verified: fork %s replayed step 2 divergently (%s)", t2, child.ID)
}
