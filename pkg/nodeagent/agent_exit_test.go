//go:build linux

package nodeagent_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/embervm/embervm/pkg/guestapi"
	"github.com/embervm/embervm/pkg/nodeapi"
)

// TestConcurrency20 proves the M1 exit criterion "单机 20 并发": 20 sandboxes
// created concurrently all reach RUNNING and each guest answers an exec.
func TestConcurrency20(t *testing.T) {
	const n = 20
	agent, image := kvmAgent(t, n+4) // headroom above the 20 concurrent slots
	ctx := context.Background()

	if err := agent.BuildTemplate(ctx, "t1", image); err != nil {
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
			id := fmt.Sprintf("c%d", i)
			_, err := agent.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
				SandboxID: id, TemplateID: "t1", VCPUs: 1, MemoryMiB: 256, DataDiskGiB: 1,
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

	// Every guest must be independently reachable and runnable.
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
	t.Logf("20 concurrent sandboxes RUNNING and exec-verified")
}

// TestHotResumeUnder1s15GiB proves the M1 exit criterion "热恢复 <1s（含 15GB
// 数据盘）": with a 15 GiB (sparse) data disk attached, resuming a paused
// sandbox to an interactive guest takes under one second. The data disk is a
// sparse raw file re-attached from the snapshot, so its size never enters the
// resume critical path (docs/zh/02 §1) — this test guards that property.
func TestHotResumeUnder1s15GiB(t *testing.T) {
	agent, image := kvmAgent(t, 2)
	ctx := context.Background()

	if err := agent.BuildTemplate(ctx, "t1", image); err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	if _, err := agent.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
		SandboxID: "r1", TemplateID: "t1", VCPUs: 1, MemoryMiB: 256, DataDiskGiB: 15,
	}); err != nil {
		t.Fatalf("CreateSandbox (15GiB data): %v", err)
	}
	t.Cleanup(func() { _ = agent.StopSandbox(context.Background(), "r1") })

	if err := agent.PauseSandbox(ctx, "r1"); err != nil {
		t.Fatalf("PauseSandbox: %v", err)
	}

	start := time.Now()
	if _, err := agent.ResumeSandbox(ctx, "r1"); err != nil {
		t.Fatalf("ResumeSandbox: %v", err)
	}
	elapsed := time.Since(start)
	t.Logf("hot resume with 15GiB data disk (uffd load -> interactive): %v", elapsed)

	if elapsed >= time.Second {
		t.Errorf("hot resume took %v, want <1s (exit criterion)", elapsed)
	}
	// Confirm the guest is actually interactive after that time.
	if _, err := agent.Health(ctx, "r1"); err != nil {
		t.Errorf("guest not interactive after resume: %v", err)
	}
}
