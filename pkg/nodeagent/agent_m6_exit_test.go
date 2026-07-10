//go:build linux

// M6 exit gates (ADR-0007): runtime CPU resize via the cgroup quota,
// engine-driven memory autoscale against real guest pressure, and an
// explicit cross-node migration of a RUNNING sandbox. The memory-resize ×
// snapshot × uffd gate is TestVirtioMemResizeKVM (agent_m6_kvm_test.go).

package nodeagent_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/embervm/embervm/pkg/controlplane"
	"github.com/embervm/embervm/pkg/nodeagent"
	"github.com/embervm/embervm/pkg/nodeapi"
)

// TestResizeCPUQuotaKVM: the VM boots with max_vcpus cores (guest nproc
// proves it), the effective compute is the cgroup cpu.max quota, and the
// resize verb moves the quota.
func TestResizeCPUQuotaKVM(t *testing.T) {
	cgroupRoot := filepath.Join(t.TempDir(), "cg")
	agent, image := kvmAgent(t, 1, func(c *nodeagent.Config) {
		c.CgroupRoot = cgroupRoot
	})
	ctx := context.Background()
	const id = "m6cpu"

	if err := agent.BuildTemplate(ctx, "tmpl-m6cpu", image); err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	if _, err := agent.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
		SandboxID: id, TemplateID: "tmpl-m6cpu",
		VCPUs: 1, MaxVCPUs: 2, MemoryMiB: 256, DataDiskGiB: 1,
	}); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	defer func() { _ = agent.StopSandbox(ctx, id) }()

	resp, err := agent.Exec(ctx, id, execReq("nproc"))
	if err != nil || resp.ExitCode != 0 {
		t.Fatalf("nproc: %v (%+v)", err, resp)
	}
	if got := strings.TrimSpace(string(resp.Stdout)); got != "2" {
		t.Fatalf("guest nproc = %s, want 2 (boot with max_vcpus)", got)
	}

	cpuMax := filepath.Join(cgroupRoot, id, "cpu.max")
	assertQuota := func(want string) {
		t.Helper()
		raw, err := os.ReadFile(cpuMax)
		if err != nil {
			// cgroup handling is best-effort by design (M1 CI lesson); a
			// host that cannot delegate +cpu skips the enforcement assert
			// but the verb/accounting asserts above still ran.
			t.Logf("cpu.max unreadable (%v) — quota enforcement not asserted on this host", err)
			return
		}
		if got := strings.TrimSpace(string(raw)); got != want {
			t.Fatalf("cpu.max = %q, want %q", got, want)
		}
	}
	assertQuota("100000 100000") // clamped to 1 vcpu at boot

	ca := agent.(*nodeagent.Agent)
	res, err := ca.ResizeSandbox(ctx, id, nodeapi.ResizeRequest{VCPUs: 2})
	if err != nil {
		t.Fatalf("cpu grow: %v", err)
	}
	if res.VCPUs != 2 {
		t.Fatalf("cpu grow achieved %d, want 2", res.VCPUs)
	}
	assertQuota("200000 100000")

	if _, err := ca.ResizeSandbox(ctx, id, nodeapi.ResizeRequest{VCPUs: 3}); err == nil {
		t.Fatal("resize above max_vcpus succeeded, want error")
	}
	if res, err = ca.ResizeSandbox(ctx, id, nodeapi.ResizeRequest{VCPUs: 1}); err != nil || res.VCPUs != 1 {
		t.Fatalf("cpu shrink: %v (%+v)", err, res)
	}
	assertQuota("100000 100000")
	t.Log("cpu quota resize ok: boot=2 cores clamped to 1, grew to 2, shrank to 1")
}

// TestAutoscaleMemoryKVM: real guest pressure drives the engine's grow, and
// releasing it drives the shrink — through PG accounting end to end.
func TestAutoscaleMemoryKVM(t *testing.T) {
	dbURL := os.Getenv("EMBERVM_TEST_DATABASE_URL")
	if os.Getenv("EMBERVM_PG_TESTS") != "1" || dbURL == "" {
		t.Skip("set EMBERVM_PG_TESTS=1 and EMBERVM_TEST_DATABASE_URL for the autoscale test")
	}
	agent, image := kvmAgent(t, 1, func(c *nodeagent.Config) {
		c.RestoreMode = "chunked"
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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
	a := &api{t: t, base: srv.URL, hc: srv.Client()}

	// Wider trigger band than production (20/65 vs 10/50): the gate proves
	// the loop, not the guest's ability to survive at 7 MiB free.
	engine := controlplane.NewEngine(store, controlplane.SingleAgent(agent), nil, nil, controlplane.EngineConfig{
		Tick: 500 * time.Millisecond, AutoscaleStepMiB: 256, AutoscaleCooldown: time.Second,
		AutoscaleGrowAvailPct: 20, AutoscaleShrinkAvailPct: 65,
	})
	go engine.Run(ctx)

	var tpl struct {
		ID string `json:"id"`
	}
	if code := a.do("POST", "/v0/templates", map[string]string{
		"name": fmt.Sprintf("m6as-%d", time.Now().UnixNano()), "image": image,
	}, &tpl); code/100 != 2 {
		t.Fatalf("create template: HTTP %d", code)
	}
	var sb struct {
		ID        string `json:"id"`
		MemoryMiB int    `json:"memory_mib"`
	}
	if code := a.do("POST", "/v0/sandboxes", map[string]any{
		"template_id": tpl.ID, "vcpus": 1, "memory_mib": 384, "data_disk_gib": 1,
		"max_memory_mib": 896, "autoscale": true,
	}, &sb); code/100 != 2 {
		t.Fatalf("create sandbox: HTTP %d", code)
	}
	defer a.do("DELETE", "/v0/sandboxes/"+sb.ID, nil, nil)

	// Hold ~270 MiB of tmpfs (default cap is 50% of RAM, so grow it first):
	// on a 384 MiB guest that pushes MemAvailable to ~11% — under the 20%
	// grow trigger with a comfortable margin above the OOM killer.
	ag := agent.(*nodeagent.Agent)
	if resp, err := ag.Exec(ctx, sb.ID, execReq("mount", "-o", "remount,size=90%", "/tmp")); err != nil || resp.ExitCode != 0 {
		t.Fatalf("remount tmpfs: %v (%+v)", err, resp)
	}
	if resp, err := ag.Exec(ctx, sb.ID, execReq("dd", "if=/dev/zero", "of=/tmp/hold", "bs=1M", "count=270")); err != nil || resp.ExitCode != 0 {
		t.Fatalf("hold memory: %v (%+v)", err, resp)
	}

	memOf := func() int {
		t.Helper()
		var got struct {
			MemoryMiB int `json:"memory_mib"`
		}
		if code := a.do("GET", "/v0/sandboxes/"+sb.ID, nil, &got); code != 200 {
			t.Fatalf("get: HTTP %d", code)
		}
		return got.MemoryMiB
	}
	waitMem := func(cond func(int) bool, what string, timeout time.Duration) int {
		t.Helper()
		deadline := time.Now().Add(timeout)
		last := -1
		for time.Now().Before(deadline) {
			if last = memOf(); cond(last) {
				return last
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatalf("sandbox never %s (last effective: %d MiB)", what, last)
		return 0
	}

	grown := waitMem(func(m int) bool { return m > 384 }, "grew under pressure", 60*time.Second)
	t.Logf("autoscale grew: 384 -> %d MiB", grown)

	if resp, err := ag.Exec(ctx, sb.ID, execReq("rm", "/tmp/hold")); err != nil || resp.ExitCode != 0 {
		t.Fatalf("release memory: %v (%+v)", err, resp)
	}
	// Shrink needs shrinkTicksNeeded quiet ticks per step; give it room.
	shrunk := waitMem(func(m int) bool { return m == 384 }, "shrank back to base", 120*time.Second)
	t.Logf("autoscale shrank back: %d -> %d MiB", grown, shrunk)
}

// TestMigrateRunningKVM: an explicit migrate moves a RUNNING sandbox to the
// other node of a 2-daemon cluster — placement pointer, guest data, and
// liveness all verified on the target.
func TestMigrateRunningKVM(t *testing.T) {
	if os.Getenv("EMBERVM_KVM_TESTS") != "1" {
		t.Skip("set EMBERVM_KVM_TESTS=1 to run KVM tests")
	}
	if os.Geteuid() != 0 {
		t.Skip("KVM tests need root")
	}
	if os.Getenv("EMBERVM_NODEAGENT_BIN") == "" {
		t.Skip("set EMBERVM_NODEAGENT_BIN to run the migrate test")
	}
	if os.Getenv("EMBERVM_JAILER_BIN") == "" {
		t.Skip("set EMBERVM_JAILER_BIN to run the migrate test (jailed daemons)")
	}
	requireL1(t)
	pool := os.Getenv("EMBERVM_ZFS_POOL")
	if pool == "" {
		t.Skip("set EMBERVM_ZFS_POOL to run the migrate test")
	}
	dbURL := os.Getenv("EMBERVM_TEST_DATABASE_URL")
	if os.Getenv("EMBERVM_PG_TESTS") != "1" || dbURL == "" {
		t.Skip("set EMBERVM_PG_TESTS=1 and EMBERVM_TEST_DATABASE_URL for the migrate test")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agents := map[string]nodeapi.Agent{}
	addrs := map[string]string{}
	caps := map[string]int{}
	for i := 1; i <= 2; i++ {
		n := startClusterNode(t, pool, 5+i) // netns range clear of other suites
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
		PollInterval: 500 * time.Millisecond, MissThreshold: 3,
	})
	if err := sched.RegisterNodes(ctx, addrs, caps); err != nil {
		t.Fatal(err)
	}
	go sched.Run(ctx)
	srv := httptest.NewServer(controlplane.NewClusterServer(
		store, registry, sched, controlplane.DevTokenStore(), nil, nil).Handler())
	t.Cleanup(srv.Close)
	a := &api{t: t, base: srv.URL, hc: srv.Client()}

	image := os.Getenv("EMBERVM_TEST_IMAGE")
	if image == "" {
		image = "alpine:3.20"
	}
	var tpl struct {
		ID string `json:"id"`
	}
	if code := a.do("POST", "/v0/templates", map[string]string{
		"name": fmt.Sprintf("m6mig-%d", time.Now().UnixNano()), "image": image,
	}, &tpl); code/100 != 2 {
		t.Fatalf("create template: HTTP %d", code)
	}
	var sb struct {
		ID     string `json:"id"`
		State  string `json:"state"`
		NodeID string `json:"node_id"`
	}
	if code := a.do("POST", "/v0/sandboxes", map[string]any{
		"template_id": tpl.ID, "vcpus": 1, "memory_mib": 256, "data_disk_gib": 1,
	}, &sb); code/100 != 2 {
		t.Fatalf("create sandbox: HTTP %d", code)
	}
	src := sb.NodeID

	// Guest state that must survive the move.
	na := agents[src]
	if err := na.WriteFile(ctx, sb.ID, "/tmp/marker", 0o644, []byte("cross-node")); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	var moved struct {
		ID     string `json:"id"`
		State  string `json:"state"`
		NodeID string `json:"node_id"`
	}
	start := time.Now()
	if code := a.do("POST", "/v0/sandboxes/"+sb.ID+"/migrate", nil, &moved); code != 200 {
		t.Fatalf("migrate: HTTP %d", code)
	}
	t.Logf("migrate took %s: %s -> %s (state %s)", time.Since(start), src, moved.NodeID, moved.State)
	if moved.NodeID == src || moved.NodeID == "" {
		t.Fatalf("migrate stayed on %q", moved.NodeID)
	}
	if moved.State != "RUNNING" {
		t.Fatalf("post-migrate state = %s, want RUNNING", moved.State)
	}

	// The guest serves from the TARGET node's agent: data intact, exec live.
	assertGuestFile(t, ctx, agents[moved.NodeID], sb.ID, "/tmp/marker", "cross-node")
	if resp, err := agents[moved.NodeID].Exec(ctx, sb.ID, execReq("echo", "alive")); err != nil || resp.ExitCode != 0 {
		t.Fatalf("exec on target node: %v (%+v)", err, resp)
	}
	// The source node no longer owns it.
	if _, err := na.Status(ctx, sb.ID); err == nil {
		t.Fatal("source node still tracks the migrated sandbox")
	}
	t.Log("migrate ok: placement, data, and liveness all on the target node")
}
