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
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/embervm/embervm/pkg/controlplane"
	"github.com/embervm/embervm/pkg/guestapi"
	"github.com/embervm/embervm/pkg/nodeagent"
	"github.com/embervm/embervm/pkg/nodeapi"
	"github.com/embervm/embervm/pkg/storage"
)

// M4 exit criteria (docs/zh/03 §3): 创建 <500ms (G4), 50 并发/节点 (G4), and
// THE gate — a 3-node cluster where killing any worker leaves every sandbox
// recoverable on another node. Latencies are CI-relative (ADR-0001).

// TestFastCreateUnder500ms proves G4 创建<500ms: with a golden template
// snapshot (chunked + jailed + ZFS + L1), CreateSandbox with matching
// geometry hot-restores the golden image instead of cold-booting, and the
// median create-to-interactive is under 500ms.
func TestFastCreateUnder500ms(t *testing.T) {
	jailerBin := os.Getenv("EMBERVM_JAILER_BIN")
	if jailerBin == "" {
		t.Skip("set EMBERVM_JAILER_BIN to run the fast-create test")
	}
	requireL1(t)
	pool := os.Getenv("EMBERVM_ZFS_POOL")
	if pool == "" {
		t.Skip("set EMBERVM_ZFS_POOL to run the fast-create test")
	}
	if out, err := exec.Command("zfs", "create", "-p", pool+"/fast").CombinedOutput(); err != nil {
		t.Fatalf("zfs create: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("zfs", "destroy", "-r", pool+"/fast").Run() })

	agent, image := kvmAgent(t, 8, func(c *nodeagent.Config) {
		c.Storage = storage.NewZFSBackend(pool + "/fast")
		c.RestoreMode = "chunked"
		c.JailerBin = jailerBin
		c.JailerChrootBase = t.TempDir()
		c.FCVersion = os.Getenv("FC_VERSION")
		c.KernelVersion = "6.1.155"
		c.GoldenVCPUs = 1
		c.GoldenMemoryMiB = 256
		c.GoldenDataDiskGiB = 1
	})
	ctx := context.Background()

	if err := agent.BuildTemplate(ctx, "tmpl-fast", image); err != nil {
		t.Fatalf("BuildTemplate (with golden snapshot): %v", err)
	}

	const n = 5
	var times []time.Duration
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("fc%d", i)
		start := time.Now()
		if _, err := agent.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
			SandboxID: id, TemplateID: "tmpl-fast",
			VCPUs: 1, MemoryMiB: 256, DataDiskGiB: 1, // golden geometry → fast path
		}); err != nil {
			t.Fatalf("fast create %s: %v", id, err)
		}
		times = append(times, time.Since(start))
		t.Cleanup(func() { _ = agent.StopSandbox(context.Background(), id) })
	}
	p50 := percentile(times, 0.50)
	t.Logf("fast-create to interactive: %v (P50 %v)", times, p50)
	if p50 >= 500*time.Millisecond {
		t.Errorf("fast-create P50 = %v, want <500ms (G4 创建<500ms)", p50)
	}

	// Each clone is a live independent guest, and its snapshot chain
	// (rooted at the golden's p1) must keep working: marker + pause/resume
	// with process continuity.
	ex, err := agent.Exec(ctx, "fc0", &guestapi.ExecRequest{Cmd: "echo", Args: []string{"clone"}})
	if err != nil || string(ex.Stdout) != "clone\n" {
		t.Fatalf("exec on fast-created sandbox: %v %q", err, ex.Stdout)
	}
	if err := agent.WriteFile(ctx, "fc0", "/clonemark", 0o644, []byte("cloned")); err != nil {
		t.Fatal(err)
	}
	h0 := mustHealth(t, ctx, agent, "fc0")
	if err := agent.PauseSandbox(ctx, "fc0"); err != nil {
		t.Fatalf("pause fast-created: %v", err)
	}
	if _, err := agent.ResumeSandbox(ctx, "fc0"); err != nil {
		t.Fatalf("resume fast-created: %v", err)
	}
	if h1 := mustHealth(t, ctx, agent, "fc0"); h1.Seq <= h0.Seq {
		t.Fatalf("seq across clone resume = %d -> %d: guest rebooted?", h0.Seq, h1.Seq)
	}
	assertGuestFile(t, ctx, agent, "fc0", "/clonemark", "cloned")
}

// TestConcurrency50 proves G4 50 并发/节点: 50 × 256 MiB sandboxes on one
// agent all reach RUNNING and each guest answers an exec (logical 12.8 GiB;
// actual RSS stays far lower via zero-page lazy faulting — D5 oversell).
func TestConcurrency50(t *testing.T) {
	const n = 50
	agent, image := kvmAgent(t, n+4)
	ctx := context.Background()

	if err := agent.BuildTemplate(ctx, "t50", image); err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}

	type result struct {
		id  string
		err error
	}
	results := make(chan result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("d%d", i)
			_, err := agent.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
				SandboxID: id, TemplateID: "t50", VCPUs: 1, MemoryMiB: 256, DataDiskGiB: 1,
			})
			results <- result{id, err}
		}(i)
	}
	wg.Wait()
	close(results)

	var live []string
	for r := range results {
		if r.err != nil {
			t.Errorf("CreateSandbox %s: %v", r.id, r.err)
			continue
		}
		live = append(live, r.id)
	}
	t.Cleanup(func() {
		for _, id := range live {
			_ = agent.StopSandbox(context.Background(), id)
		}
	})
	if len(live) != n {
		t.Fatalf("only %d/%d sandboxes came up", len(live), n)
	}
	for _, id := range live {
		ex, err := agent.Exec(ctx, id, &guestapi.ExecRequest{Cmd: "echo", Args: []string{id}})
		if err != nil {
			t.Errorf("Exec %s: %v", id, err)
			continue
		}
		if got := string(ex.Stdout); got != id+"\n" {
			t.Errorf("Exec %s stdout = %q, want %q", id, got, id+"\n")
		}
	}
	t.Logf("50 concurrent sandboxes RUNNING and exec-verified")
}

// clusterNode is one spawned nodeagent daemon.
type clusterNode struct {
	id   string
	sock string
	cmd  *exec.Cmd
}

// startClusterNode execs bin/nodeagent with its own ZFS subtree, workdir,
// chunk cache, and netns range, and waits until Healthz answers.
func startClusterNode(t *testing.T, pool string, i int) *clusterNode {
	t.Helper()
	naBin := os.Getenv("EMBERVM_NODEAGENT_BIN")
	id := fmt.Sprintf("n%d", i)
	if out, err := exec.Command("zfs", "create", "-p", pool+"/"+id).CombinedOutput(); err != nil {
		t.Fatalf("zfs create %s: %v: %s", id, err, out)
	}
	t.Cleanup(func() { _ = exec.Command("zfs", "destroy", "-r", pool+"/"+id).Run() })

	// Everything the daemon touches lives OUTSIDE t.TempDir() and is
	// deliberately leaked (the CI runner is ephemeral): a SIGKILLed daemon's
	// FC/uffd children outlive it and keep writing chunk-cache files, which
	// races testing.T's TempDir RemoveAll into "directory not empty".
	dir, err := os.MkdirTemp("", id)
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, id+".sock")
	// The jail gets its own SHORT base: the jailed API socket's host path is
	// <base>/firecracker/<uuid>/root/run/firecracker.socket, and a long
	// prefix blows the 108-byte unix sun_path limit (every connect fails
	// EINVAL while FC listens fine in-chroot).
	jailBase, err := os.MkdirTemp("", "j")
	if err != nil {
		t.Fatal(err)
	}
	logf, err := os.Create(filepath.Join(dir, id+".log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(naBin,
		"--socket", sock,
		"--pool", pool+"/"+id,
		"--work-dir", filepath.Join(dir, "work"),
		"--chunk-store-dir", filepath.Join(dir, "chunks"),
		"--restore-mode", "chunked",
		"--netns-base", fmt.Sprintf("%d", 100+10*i),
		"--netns-pool", "4",
		"--script-dir", os.Getenv("EMBERVM_SCRIPT_DIR"),
		"--kernel", os.Getenv("EMBERVM_KERNEL"),
		"--fc-bin", os.Getenv("EMBERVM_FC_BIN"),
		"--uffd-handler", os.Getenv("EMBERVM_UFFD_BIN"),
		"--guestd-bin", os.Getenv("EMBERVM_GUESTD_BIN"),
		"--capacity-mib", "2048",
		"--watchdog-interval", "2s",
		"--fc-version", os.Getenv("FC_VERSION"),
		"--kernel-version", "6.1.155",
		// Jailed: chroot-relative snapshot paths are what make a snapfile
		// restorable on ANY node (D3) — and what the kill-node recovery
		// depends on when all three subtrees share the CI host.
		"--jailer-bin", os.Getenv("EMBERVM_JAILER_BIN"),
		"--jailer-chroot-base", jailBase,
	)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", id, err)
	}
	n := &clusterNode{id: id, sock: sock, cmd: cmd}
	t.Cleanup(func() {
		if n.cmd.Process != nil {
			_ = n.cmd.Process.Kill()
			_, _ = n.cmd.Process.Wait()
		}
		if !t.Failed() {
			return
		}
		if data, err := os.ReadFile(logf.Name()); err == nil {
			t.Logf("--- %s daemon log ---\n%s", id, data)
		}
		// Per-sandbox process logs (fc.log, uffd.log, jailer stderr) are the
		// only witnesses to why a microVM died inside the daemon.
		_ = filepath.WalkDir(filepath.Join(dir, "work"), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".log") {
				return nil
			}
			if data, err := os.ReadFile(p); err == nil && len(data) > 0 {
				if len(data) > 4096 {
					data = data[len(data)-4096:]
				}
				t.Logf("--- %s %s ---\n%s", id, p, data)
			}
			return nil
		})
	})

	client := nodeapi.NewClient(sock)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		hctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := client.Healthz(hctx)
		cancel()
		if err == nil {
			return n
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("node %s never became healthy", id)
	return nil
}

// TestClusterKillNode is THE M4 exit gate (docs/zh/03 §3): a 3-node cluster
// (three nodeagent daemons over unix sockets) where kill -9 of any worker
// leaves every sandbox recoverable on another node:
//
//   - placement spreads sandboxes by free memory (observed via node_id);
//   - a create on a node that never built the template receives the stream
//     from L1 on demand (GUID lineage);
//   - the scheduler's polled heartbeats evict the dead node and mark its
//     RUNNING sandboxes FAILED;
//   - a PAUSED_HOT sandbox on the dead node fails its hot resume, then
//     restores from its L1 write-through on a healthy node with continuity;
//   - a FAILED (was RUNNING) sandbox restores from its last write-through —
//     data written after that snapshot is gone, which IS the RPO contract;
//   - the recovered guest serves HTTP through the two-hop gateway proxy
//     (split mode: apiserver → node daemon UDS → netns dial).
func TestClusterKillNode(t *testing.T) {
	if os.Getenv("EMBERVM_KVM_TESTS") != "1" {
		t.Skip("set EMBERVM_KVM_TESTS=1 to run KVM tests")
	}
	if os.Geteuid() != 0 {
		t.Skip("KVM tests need root")
	}
	if os.Getenv("EMBERVM_NODEAGENT_BIN") == "" {
		t.Skip("set EMBERVM_NODEAGENT_BIN to run the cluster test")
	}
	if os.Getenv("EMBERVM_JAILER_BIN") == "" {
		t.Skip("set EMBERVM_JAILER_BIN to run the cluster test (jailed daemons)")
	}
	requireL1(t)
	pool := os.Getenv("EMBERVM_ZFS_POOL")
	if pool == "" {
		t.Skip("set EMBERVM_ZFS_POOL to run the cluster test")
	}
	dbURL := os.Getenv("EMBERVM_TEST_DATABASE_URL")
	if os.Getenv("EMBERVM_PG_TESTS") != "1" || dbURL == "" {
		t.Skip("set EMBERVM_PG_TESTS=1 and EMBERVM_TEST_DATABASE_URL for the cluster test")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- 3 worker daemons + in-proc control plane --------------------------
	nodes := map[string]*clusterNode{}
	agents := map[string]nodeapi.Agent{}
	addrs := map[string]string{}
	caps := map[string]int{}
	for i := 1; i <= 3; i++ {
		n := startClusterNode(t, pool, i)
		nodes[n.id] = n
		agents[n.id] = nodeapi.NewClient(n.sock)
		addrs[n.id] = n.sock
		caps[n.id] = 2048
	}

	store, err := controlplane.NewStore(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	registry := controlplane.NewRegistry(agents)
	sched := controlplane.NewScheduler(store, registry, controlplane.SchedulerConfig{
		PollInterval: 500 * time.Millisecond, MissThreshold: 2,
	})
	if err := sched.RegisterNodes(ctx, addrs, caps); err != nil {
		t.Fatal(err)
	}
	go sched.Run(ctx)

	srv := httptest.NewServer(controlplane.NewClusterServer(
		store, registry, sched, controlplane.DevTokenStore(), nil, nil).Handler())
	t.Cleanup(srv.Close)
	a := &api{t: t, base: srv.URL, hc: srv.Client()}

	// --- template + spread sandboxes ---------------------------------------
	image := os.Getenv("EMBERVM_TEST_IMAGE")
	if image == "" {
		image = "alpine:3.20"
	}
	var tpl struct {
		ID string `json:"id"`
	}
	if code := a.do("POST", "/v0/templates", map[string]string{
		"name": fmt.Sprintf("m4-%d", time.Now().UnixNano()), "image": image,
	}, &tpl); code/100 != 2 {
		t.Fatalf("create template: HTTP %d", code)
	}

	type sbInfo struct {
		ID     string `json:"id"`
		State  string `json:"state"`
		NodeID string `json:"node_id"`
	}
	create := func() sbInfo {
		t.Helper()
		var sb sbInfo
		if code := a.do("POST", "/v0/sandboxes", map[string]any{
			"template_id": tpl.ID, "vcpus": 1, "memory_mib": 256, "data_disk_gib": 1,
		}, &sb); code/100 != 2 {
			t.Fatalf("create sandbox: HTTP %d", code)
		}
		return sb
	}
	get := func(id string) sbInfo {
		t.Helper()
		var sb sbInfo
		if code := a.do("GET", "/v0/sandboxes/"+id, nil, &sb); code != 200 {
			t.Fatalf("get %s: HTTP %d", id, code)
		}
		return sb
	}
	putFile := func(id, path, content string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPut,
			srv.URL+"/v0/sandboxes/"+id+"/files?path="+path, strings.NewReader(content))
		req.Header.Set("Authorization", "Bearer "+controlplane.DevTokenName)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			t.Fatalf("put %s %s: HTTP %d", id, path, resp.StatusCode)
		}
	}
	readFile := func(id, path string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet,
			srv.URL+"/v0/sandboxes/"+id+"/files?path="+path, nil)
		req.Header.Set("Authorization", "Bearer "+controlplane.DevTokenName)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(data)
	}
	waitState := func(id, want string, timeout time.Duration) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		last := ""
		for time.Now().Before(deadline) {
			if sb := get(id); sb.State == want {
				return
			} else {
				last = sb.State
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatalf("sandbox %s never reached %s (last: %s)", id, want, last)
	}

	// One paused sandbox per node. Create all three FIRST — bin-packing
	// budgets only ACTIVE sandboxes, so each RUNNING create pushes the next
	// onto a different node (and any node that did not build the template
	// receives it from L1 on demand). Only then write markers and pause
	// (pause writes the snapshot through to L1).
	pausedOn := map[string]sbInfo{}
	for i := 0; i < 3; i++ {
		sb := create()
		if sb.NodeID == "" {
			t.Fatalf("sandbox %s has no node_id", sb.ID)
		}
		pausedOn[sb.NodeID] = sb
	}
	if len(pausedOn) != 3 {
		t.Fatalf("placement put %d/3 sandboxes on distinct nodes (bin-packing broken?)", len(pausedOn))
	}
	for _, sb := range pausedOn {
		putFile(sb.ID, "/marker", "survives-"+sb.ID)
		if code := a.do("POST", "/v0/sandboxes/"+sb.ID+"/pause", nil, nil); code/100 != 2 {
			t.Fatalf("pause %s: HTTP %d", sb.ID, code)
		}
	}
	t.Logf("placement observed: one paused sandbox per node (bin-pack + L1 template receive)")

	// One RUNNING sandbox with a write-through snapshot behind it: pause
	// (snapshot to L1) then resume, then write data that outlives no snapshot.
	running := create()
	putFile(running.ID, "/marker", "survives-"+running.ID)
	if code := a.do("POST", "/v0/sandboxes/"+running.ID+"/pause", nil, nil); code/100 != 2 {
		t.Fatalf("pause running: HTTP %d", code)
	}
	if code := a.do("POST", "/v0/sandboxes/"+running.ID+"/resume", nil, nil); code/100 != 2 {
		t.Fatalf("resume running: HTTP %d", code)
	}
	waitState(running.ID, "RUNNING", 15*time.Second)
	running = get(running.ID)
	putFile(running.ID, "/after-snapshot", "lost-by-design") // RPO: not in any snapshot

	victim := running.NodeID
	pausedVictim := pausedOn[victim]
	t.Logf("victim node: %s (running=%s, paused=%s)", victim, running.ID, pausedVictim.ID)

	// --- kill -9 the worker --------------------------------------------------
	if err := nodes[victim].cmd.Process.Kill(); err != nil {
		t.Fatalf("kill %s: %v", victim, err)
	}
	_, _ = nodes[victim].cmd.Process.Wait()
	t.Logf("killed %s daemon (SIGKILL)", victim)

	// Eviction: the scheduler's polled heartbeats mark the node down and its
	// RUNNING sandboxes FAILED (last write-through stays restorable).
	waitState(running.ID, "FAILED", 30*time.Second)
	t.Logf("scheduler evicted %s; RUNNING sandbox marked FAILED", victim)

	// --- recovery: paused sandbox on the dead node ---------------------------
	// The hot resume hits the dead socket and fails to FAILED; the retry takes
	// the FAILED path — restore from L1 on a healthy node.
	resumed := false
	for attempt := 0; attempt < 5 && !resumed; attempt++ {
		if code := a.do("POST", "/v0/sandboxes/"+pausedVictim.ID+"/resume", nil, nil); code/100 == 2 {
			resumed = true
		} else {
			time.Sleep(time.Second)
		}
	}
	if !resumed {
		t.Fatalf("paused sandbox %s never resumed after node death", pausedVictim.ID)
	}
	waitState(pausedVictim.ID, "RUNNING", 30*time.Second)
	if sb := get(pausedVictim.ID); sb.NodeID == victim || sb.NodeID == "" {
		t.Fatalf("paused sandbox recovered on %q, want a healthy node != %s", sb.NodeID, victim)
	}
	if code, got := readFile(pausedVictim.ID, "/marker"); code != 200 || got != "survives-"+pausedVictim.ID {
		t.Fatalf("marker after cross-node restore: HTTP %d %q", code, got)
	}
	t.Logf("paused-on-dead-node sandbox restored on another node with continuity")

	// --- recovery: FAILED (was RUNNING) sandbox -------------------------------
	if code := a.do("POST", "/v0/sandboxes/"+running.ID+"/resume", nil, nil); code/100 != 2 {
		t.Fatalf("resume FAILED sandbox: HTTP %d", code)
	}
	waitState(running.ID, "RUNNING", 30*time.Second)
	rec := get(running.ID)
	if rec.NodeID == victim || rec.NodeID == "" {
		t.Fatalf("failed sandbox recovered on %q, want a healthy node != %s", rec.NodeID, victim)
	}
	if code, got := readFile(running.ID, "/marker"); code != 200 || got != "survives-"+running.ID {
		t.Fatalf("marker after FAILED restore: HTTP %d %q", code, got)
	}
	// RPO contract: data written after the last write-through is gone.
	if code, got := readFile(running.ID, "/after-snapshot"); code == 200 {
		t.Fatalf("post-snapshot write survived node death (%q) — write-through RPO broken?", got)
	}
	t.Logf("running-on-dead-node sandbox FAILED then restored elsewhere from its last write-through (RPO verified)")

	// --- paused sandboxes on healthy nodes stay hot ---------------------------
	for nodeID, sb := range pausedOn {
		if nodeID == victim {
			continue
		}
		if code := a.do("POST", "/v0/sandboxes/"+sb.ID+"/resume", nil, nil); code/100 != 2 {
			t.Fatalf("hot resume on healthy node %s: HTTP %d", nodeID, code)
		}
		waitState(sb.ID, "RUNNING", 15*time.Second)
	}
	t.Logf("healthy nodes' paused sandboxes hot-resumed in place")

	// --- gateway proxy in-flow through both hops ------------------------------
	// Proxy an HTTP GET to a port inside the RECOVERED guest: apiserver →
	// node daemon unix socket → netns dial (split mode). The target is
	// guestd's own :7777 /healthz — present in every EmberVM guest, no
	// dependency on what the template image bundles.
	req, _ := http.NewRequest(http.MethodGet,
		srv.URL+"/v0/sandboxes/"+running.ID+"/proxy/7777/healthz", nil)
	req.Header.Set("Authorization", "Bearer "+controlplane.DevTokenName)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("gateway proxy: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(data), `"ok":true`) {
		t.Fatalf("gateway proxy = HTTP %d %q, want guestd health through both hops", resp.StatusCode, data)
	}
	t.Logf("gateway proxy verified through both hops on the recovered sandbox")
}
