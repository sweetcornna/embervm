package controlplane

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/embervm/embervm/pkg/nodeapi"
)

// schedMockAgent is a cpMockAgent whose health can be failed on demand.
type schedMockAgent struct {
	cpMockAgent
	capacity int
	cores    int
	dead     atomic.Bool
	failed   []string // drained by Healthz, like the real agent's watchdog list
}

func (m *schedMockAgent) Healthz(context.Context) (nodeapi.NodeHealth, error) {
	if m.dead.Load() {
		return nodeapi.NodeHealth{}, errors.New("connection refused")
	}
	h := nodeapi.NodeHealth{CapacityMiB: m.capacity, CPUCores: m.cores, FailedSandboxes: m.failed}
	m.failed = nil
	return h, nil
}

func newCluster(t *testing.T, capacities map[string]int) (*Store, *Scheduler, map[string]*schedMockAgent) {
	t.Helper()
	s := testStore(t)
	agents := map[string]*schedMockAgent{}
	reg := map[string]nodeapi.Agent{}
	for id, cap := range capacities {
		a := &schedMockAgent{capacity: cap}
		agents[id] = a
		reg[id] = a
	}
	sched := NewScheduler(s, NewRegistry(reg), SchedulerConfig{MissThreshold: 2})
	addrs := map[string]string{}
	caps := map[string]int{}
	for id, a := range agents {
		addrs[id] = "unix:///tmp/" + id + ".sock"
		caps[id] = a.capacity
	}
	if err := sched.RegisterNodes(context.Background(), addrs, caps); err != nil {
		t.Fatal(err)
	}
	return s, sched, agents
}

// TestSchedulerCanFit exercises the M6 resize growth admission: deltas
// against the oversold budget, shrink always fits, down nodes reject.
func TestSchedulerCanFit(t *testing.T) {
	s, sched, _ := newCluster(t, map[string]int{"n1": 1024})
	ctx := context.Background()

	// Occupy 900 of n1's 1024 MiB.
	id := pausedSandbox(t, s, "RUNNING", time.Second)
	if err := s.SetSandboxNode(ctx, id, "n1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE sandboxes SET memory_mib=900 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}

	if err := sched.CanFit(ctx, "n1", 100, 0); err != nil {
		t.Errorf("CanFit(+100 of 124 free) = %v, want nil", err)
	}
	if err := sched.CanFit(ctx, "n1", 200, 0); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("CanFit(+200 of 124 free) = %v, want ErrNoCapacity", err)
	}
	// Shrink always fits, even on a full node.
	if err := sched.CanFit(ctx, "n1", -512, -1); err != nil {
		t.Errorf("CanFit(shrink) = %v, want nil", err)
	}
	if err := sched.CanFit(ctx, "nope", 1, 0); err == nil {
		t.Error("CanFit(unknown node) = nil, want error")
	}
	// Down node rejects growth.
	if err := s.SetNodeState(ctx, "n1", "down"); err != nil {
		t.Fatal(err)
	}
	if err := sched.CanFit(ctx, "n1", 1, 0); err == nil {
		t.Error("CanFit(down node) = nil, want error")
	}
}

func TestSchedulerPlaceSticky(t *testing.T) {
	_, sched, _ := newCluster(t, map[string]int{"n1": 4096, "n2": 8192})
	ctx := context.Background()

	// n2 has more free memory, but stickiness to n1 wins when n1 fits.
	node, err := sched.Place(ctx, "n1", 1024, 1)
	if err != nil {
		t.Fatal(err)
	}
	if node != "n1" {
		t.Fatalf("Place(sticky n1) = %s, want n1", node)
	}
	// No previous node: bin-pack to the roomiest.
	node, err = sched.Place(ctx, "", 1024, 1)
	if err != nil {
		t.Fatal(err)
	}
	if node != "n2" {
		t.Fatalf("Place(fresh) = %s, want n2 (most free)", node)
	}
}

func TestSchedulerPlaceRespectsUsage(t *testing.T) {
	s, sched, _ := newCluster(t, map[string]int{"n1": 1024, "n2": 1024})
	ctx := context.Background()

	// Fill n1 with a running 900MiB sandbox.
	id := pausedSandbox(t, s, "RUNNING", time.Second)
	if err := s.SetSandboxNode(ctx, id, "n1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE sandboxes SET memory_mib=900 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}

	// 512MiB no longer fits on n1, even sticky.
	node, err := sched.Place(ctx, "n1", 512, 1)
	if err != nil {
		t.Fatal(err)
	}
	if node != "n2" {
		t.Fatalf("Place = %s, want n2 (n1 full)", node)
	}
	// And nothing fits 2048.
	if _, err := sched.Place(ctx, "", 2048, 1); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("Place(2048) = %v, want ErrNoCapacity", err)
	}
}

func TestSchedulerEvictsDeadNode(t *testing.T) {
	s, sched, agents := newCluster(t, map[string]int{"n1": 4096, "n2": 4096})
	ctx := context.Background()

	victim := pausedSandbox(t, s, "RUNNING", time.Second)
	if err := s.SetSandboxNode(ctx, victim, "n1"); err != nil {
		t.Fatal(err)
	}
	survivor := pausedSandbox(t, s, "PAUSED_WARM", time.Second)
	if err := s.SetSandboxNode(ctx, survivor, "n1"); err != nil {
		t.Fatal(err)
	}

	agents["n1"].dead.Store(true)
	// MissThreshold=2: two failing polls evict.
	for i := 0; i < 2; i++ {
		if err := sched.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}

	nodes, err := s.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, n := range nodes {
		states[n.ID] = n.State
	}
	if states["n1"] != "down" || states["n2"] != "up" {
		t.Fatalf("node states = %v, want n1 down / n2 up", states)
	}
	// The RUNNING sandbox died with its node; the WARM one lives in L1.
	sb, err := s.GetSandbox(ctx, victim)
	if err != nil {
		t.Fatal(err)
	}
	if sb.State != "FAILED" {
		t.Fatalf("victim state = %s, want FAILED", sb.State)
	}
	sb, err = s.GetSandbox(ctx, survivor)
	if err != nil {
		t.Fatal(err)
	}
	if sb.State != "PAUSED_WARM" {
		t.Fatalf("survivor state = %s, want PAUSED_WARM (untouched)", sb.State)
	}
	// Placement skips the dead node entirely.
	node, err := sched.Place(ctx, "n1", 256, 1)
	if err != nil {
		t.Fatal(err)
	}
	if node != "n2" {
		t.Fatalf("Place(sticky to dead n1) = %s, want n2", node)
	}
	// FAILED is now resumable (FAILED -> RESUMING edge) so recovery is a
	// plain resume that re-places; assert the CAS accepts it.
	if err := s.TransitionSandbox(ctx, victim, "FAILED", "RESUMING", ""); err != nil {
		t.Fatalf("FAILED->RESUMING CAS: %v", err)
	}
}

func TestSchedulerRecoversAfterHeartbeat(t *testing.T) {
	s, sched, agents := newCluster(t, map[string]int{"n1": 4096})
	ctx := context.Background()

	agents["n1"].dead.Store(true)
	for i := 0; i < 2; i++ {
		_ = sched.pollOnce(ctx)
	}
	agents["n1"].dead.Store(false)
	if err := sched.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	nodes, err := s.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].State != "up" {
		t.Fatalf("node after recovery = %+v, want up", nodes)
	}
}

func TestSchedulerWatchdogWriteThrough(t *testing.T) {
	s, sched, agents := newCluster(t, map[string]int{"n1": 4096})
	ctx := context.Background()

	victim := pausedSandbox(t, s, "RUNNING", time.Second)
	if err := s.SetSandboxNode(ctx, victim, "n1"); err != nil {
		t.Fatal(err)
	}
	// The node's watchdog reaped it; the next heartbeat carries the report
	// and the scheduler writes it through to PostgreSQL.
	agents["n1"].failed = []string{victim + ": firecracker process died"}
	if err := sched.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	sb, err := s.GetSandbox(ctx, victim)
	if err != nil {
		t.Fatal(err)
	}
	if sb.State != "FAILED" || sb.Error != "watchdog: firecracker process died" {
		t.Fatalf("victim state=%s error=%q, want FAILED / watchdog cause", sb.State, sb.Error)
	}
	// Reaped ≠ terminal: recovery is the ordinary FAILED -> RESUMING CAS.
	if err := s.TransitionSandbox(ctx, victim, "FAILED", "RESUMING", ""); err != nil {
		t.Fatalf("FAILED->RESUMING CAS: %v", err)
	}

	// A stale report must not clobber a sandbox that has since paused —
	// FailSandbox only touches active states.
	settled := pausedSandbox(t, s, "PAUSED_WARM", time.Second)
	agents["n1"].failed = []string{settled + ": uffd handler died"}
	if err := sched.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	sb, err = s.GetSandbox(ctx, settled)
	if err != nil {
		t.Fatal(err)
	}
	if sb.State != "PAUSED_WARM" {
		t.Fatalf("settled sandbox state = %s, want PAUSED_WARM (untouched)", sb.State)
	}
}

// TestSchedulerCPUOvercommit proves the vCPU budget: cores × ratio, with
// unreported cores (0) unconstrained.
func TestSchedulerCPUOvercommit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := &schedMockAgent{capacity: 8192, cores: 2}
	sched := NewScheduler(s, NewRegistry(map[string]nodeapi.Agent{"n1": a}),
		SchedulerConfig{CPUOvercommit: 3.0})
	if err := sched.RegisterNodes(ctx, map[string]string{"n1": ""}, map[string]int{"n1": 8192}); err != nil {
		t.Fatal(err)
	}
	// Heartbeat records the core count.
	if err := sched.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// Budget = 2 cores × 3.0 = 6 vCPUs. Occupy 5 of them.
	id := pausedSandbox(t, s, "RUNNING", time.Second)
	if err := s.SetSandboxNode(ctx, id, "n1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE sandboxes SET vcpus=5, memory_mib=256 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}

	// 1 more vCPU fits (5+1 <= 6); memory is plentiful.
	if node, err := sched.Place(ctx, "", 256, 1); err != nil || node != "n1" {
		t.Fatalf("Place(1 vcpu) = %s, %v; want n1", node, err)
	}
	// 2 more do not (5+2 > 6) — CPU is the binding constraint.
	if _, err := sched.Place(ctx, "", 256, 2); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("Place(2 vcpus) = %v, want ErrNoCapacity", err)
	}

	// A node that never reported cores is CPU-unconstrained.
	if _, err := s.pool.Exec(ctx, `UPDATE nodes SET cpu_cores=0 WHERE id='n1'`); err != nil {
		t.Fatal(err)
	}
	if node, err := sched.Place(ctx, "", 256, 64); err != nil || node != "n1" {
		t.Fatalf("Place(64 vcpus, no core report) = %s, %v; want n1", node, err)
	}
}
