//go:build linux

package nodeagent_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/embervm/embervm/pkg/nodeagent"
	"github.com/embervm/embervm/pkg/nodeapi"
)

// TestResumeFailureRollback proves a failed resume neither leaks processes
// nor wedges the state machine: the sandbox lands in FAILED (with FC and the
// uffd handler killed) and a later resume recovers it. Regression: resume()
// used to return on error with both children alive and the machine stuck in
// RESUMING — unrecoverable and invisible to the watchdog.
func TestResumeFailureRollback(t *testing.T) {
	agent, image := kvmAgent(t, 1, func(c *nodeagent.Config) {
		c.RestoreMode = "chunked"
		c.FCVersion = os.Getenv("FC_VERSION")
		c.KernelVersion = "6.1.155"
	})
	ctx := context.Background()
	const id = "resumefail"

	if err := agent.BuildTemplate(ctx, "tmpl-rf", image); err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	if _, err := agent.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
		SandboxID: id, TemplateID: "tmpl-rf", MemoryMiB: 256, DataDiskGiB: 1,
	}); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	defer func() { _ = agent.StopSandbox(ctx, id) }()
	if err := agent.PauseSandbox(ctx, id); err != nil {
		t.Fatalf("pause: %v", err)
	}

	ca := agent.(*nodeagent.Agent)
	snapfile := filepath.Join(ca.WorkDirOf(id), "snap", "snapfile-p1")
	hidden := snapfile + ".hidden"
	if err := os.Rename(snapfile, hidden); err != nil {
		t.Fatalf("hide snapfile: %v", err)
	}

	if _, err := agent.ResumeSandbox(ctx, id); err == nil {
		t.Fatal("resume without a snapfile succeeded — expected failure")
	}
	if fcPid, uffdPid := ca.PidsOf(id); fcPid != 0 || uffdPid != 0 {
		t.Fatalf("failed resume leaked processes: fc=%d uffd=%d", fcPid, uffdPid)
	}
	st, err := agent.Status(ctx, id)
	if err != nil {
		t.Fatalf("status after failed resume: %v", err)
	}
	if st.State != "FAILED" {
		t.Fatalf("state after failed resume = %s, want FAILED", st.State)
	}

	// The whole point of FAILED over a wedged RESUMING: recovery is legal.
	if err := os.Rename(hidden, snapfile); err != nil {
		t.Fatalf("restore snapfile: %v", err)
	}
	if _, err := agent.ResumeSandbox(ctx, id); err != nil {
		t.Fatalf("recovery resume after FAILED: %v", err)
	}
	if _, err := agent.Health(ctx, id); err != nil {
		t.Fatalf("guest health after recovery: %v", err)
	}
}

// TestReleaseResumeRaceLocal proves the same-node release-vs-resume exclusion:
// whichever of ReleaseLocal / ResumeSandbox wins the lifecycle CAS proceeds,
// the other fails — never both. Regression: ReleaseLocal used to tear the
// sandbox down without claiming the machine, so a concurrent local resume
// could boot Firecracker against a dataset mid-destroy.
func TestReleaseResumeRaceLocal(t *testing.T) {
	agent, image := kvmAgent(t, 1, func(c *nodeagent.Config) {
		c.RestoreMode = "chunked"
		c.FCVersion = os.Getenv("FC_VERSION")
		c.KernelVersion = "6.1.155"
	})
	ctx := context.Background()
	ca := agent.(*nodeagent.Agent)
	const id = "relrace"

	if err := agent.BuildTemplate(ctx, "tmpl-rr", image); err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	if _, err := agent.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
		SandboxID: id, TemplateID: "tmpl-rr", MemoryMiB: 256, DataDiskGiB: 1,
	}); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	defer func() { _ = agent.StopSandbox(ctx, id) }()

	// Repeat the race until the release wins once (or the rounds run out —
	// each round is still a valid exclusion check when the resume wins).
	for round := 1; round <= 6; round++ {
		if err := agent.PauseSandbox(ctx, id); err != nil {
			t.Fatalf("round %d pause: %v", round, err)
		}

		var wg sync.WaitGroup
		var releaseErr, resumeErr error
		wg.Add(2)
		go func() { defer wg.Done(); releaseErr = ca.ReleaseLocal(ctx, id) }()
		go func() { defer wg.Done(); _, resumeErr = agent.ResumeSandbox(ctx, id) }()
		wg.Wait()

		if releaseErr == nil && resumeErr == nil {
			t.Fatalf("round %d: release AND resume both succeeded — exclusion broken", round)
		}
		if releaseErr != nil && resumeErr != nil {
			t.Fatalf("round %d: both failed: release=%v resume=%v", round, releaseErr, resumeErr)
		}
		if releaseErr == nil {
			// Release won: local state must be fully gone.
			if _, err := agent.Status(ctx, id); err == nil {
				t.Fatal("release won but the sandbox still resolves locally")
			}
			return
		}
		// Resume won: the sandbox must be intact and interactive.
		if _, err := agent.Health(ctx, id); err != nil {
			t.Fatalf("round %d: resume won but guest is unhealthy: %v", round, err)
		}
	}
	t.Log("release never won a round; exclusion held in every resume-won round")
}
