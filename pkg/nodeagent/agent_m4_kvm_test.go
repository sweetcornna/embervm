//go:build linux

package nodeagent_test

import (
	"context"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/embervm/embervm/pkg/nodeagent"
	"github.com/embervm/embervm/pkg/nodeapi"
)

// TestJailedLifecycleKVM proves the full chunked pause/resume cycle under
// jailer hardening (chroot + per-VM uid/gid + netns + default seccomp):
// snapshot paths are chroot-relative and the whole M2 pipeline still holds.
func TestJailedLifecycleKVM(t *testing.T) {
	jailerBin := os.Getenv("EMBERVM_JAILER_BIN")
	if jailerBin == "" {
		t.Skip("set EMBERVM_JAILER_BIN to run the jailed lifecycle test")
	}
	agent, image := kvmAgent(t, 1, func(c *nodeagent.Config) {
		c.RestoreMode = "chunked"
		c.JailerBin = jailerBin
		c.JailerChrootBase = t.TempDir()
		c.FCVersion = os.Getenv("FC_VERSION")
		c.KernelVersion = "6.1.155"
	})
	ctx := context.Background()
	const id = "jail1"

	if err := agent.BuildTemplate(ctx, "tmpl-jail", image); err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	if _, err := agent.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
		SandboxID: id, TemplateID: "tmpl-jail", MemoryMiB: 256, DataDiskGiB: 1,
	}); err != nil {
		t.Fatalf("CreateSandbox (jailed): %v", err)
	}
	defer func() { _ = agent.StopSandbox(ctx, id) }()

	if err := agent.WriteFile(ctx, id, "/jailmarker", 0o644, []byte("hardened")); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	h0 := mustHealth(t, ctx, agent, id)

	// Full pause -> resume under the jailer (chroot-relative snapfile).
	if err := agent.PauseSandbox(ctx, id); err != nil {
		t.Fatalf("pause 1: %v", err)
	}
	if _, err := agent.ResumeSandbox(ctx, id); err != nil {
		t.Fatalf("resume 1 (jailed): %v", err)
	}
	h1 := mustHealth(t, ctx, agent, id)
	if h1.Seq <= h0.Seq {
		t.Fatalf("seq across jailed resume = %d -> %d: guest rebooted?", h0.Seq, h1.Seq)
	}
	assertGuestFile(t, ctx, agent, id, "/jailmarker", "hardened")

	// Diff pause -> resume: the whole layered pipeline inside the chroot.
	if err := agent.WriteFile(ctx, id, "/jailmarker2", 0o644, []byte("layer2")); err != nil {
		t.Fatal(err)
	}
	if err := agent.PauseSandbox(ctx, id); err != nil {
		t.Fatalf("pause 2: %v", err)
	}
	if _, err := agent.ResumeSandbox(ctx, id); err != nil {
		t.Fatalf("resume 2 (jailed diff): %v", err)
	}
	assertGuestFile(t, ctx, agent, id, "/jailmarker", "hardened")
	assertGuestFile(t, ctx, agent, id, "/jailmarker2", "layer2")
}

// TestWatchdogReapsZombiesKVM proves the G5 reaper end to end: a Firecracker
// (or uffd handler) that dies behind the agent's back is force-FAILED, its
// resources are released, and the failure is reported through Healthz. The
// netns pool has exactly ONE slot, so the second CreateSandbox only succeeds
// if the first reap really released the lease.
func TestWatchdogReapsZombiesKVM(t *testing.T) {
	agent, image := kvmAgent(t, 1, func(c *nodeagent.Config) {
		c.RestoreMode = "chunked"
		c.WatchdogInterval = 200 * time.Millisecond
	})
	ca := agent.(*nodeagent.Agent)
	ctx := context.Background()

	if err := agent.BuildTemplate(ctx, "tmpl-wd", image); err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}

	// Zombie 1: Firecracker SIGKILLed under a RUNNING sandbox.
	if _, err := agent.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
		SandboxID: "wd1", TemplateID: "tmpl-wd", MemoryMiB: 256, DataDiskGiB: 1,
	}); err != nil {
		t.Fatalf("CreateSandbox wd1: %v", err)
	}
	// Balloon plumbing rides along on the live VM: a retarget must reach
	// the device bootFresh attached (M4 memory oversell).
	if err := agent.SetBalloon(ctx, "wd1", 64); err != nil {
		t.Fatalf("SetBalloon on RUNNING: %v", err)
	}
	fcPid, _ := ca.PidsOf("wd1")
	if fcPid == 0 {
		t.Fatal("no firecracker pid for wd1")
	}
	if err := syscall.Kill(fcPid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill firecracker: %v", err)
	}
	if got := awaitReap(t, ctx, agent, "wd1"); !strings.Contains(got, "firecracker process died") {
		t.Fatalf("wd1 reap cause = %q, want firecracker process died", got)
	}

	// Zombie 2: the uffd handler dies after a resume — every future page
	// fault would hang the vCPU forever (docs/zh/04 §6). Only creatable at
	// all because wd1's reap released the single netns lease.
	if _, err := agent.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
		SandboxID: "wd2", TemplateID: "tmpl-wd", MemoryMiB: 256, DataDiskGiB: 1,
	}); err != nil {
		t.Fatalf("CreateSandbox wd2 (lease not released by reap?): %v", err)
	}
	if err := agent.PauseSandbox(ctx, "wd2"); err != nil {
		t.Fatalf("pause wd2: %v", err)
	}
	if err := agent.SetBalloon(ctx, "wd2", 64); err == nil {
		t.Fatal("SetBalloon on PAUSED_HOT: want error, got nil")
	}
	if _, err := agent.ResumeSandbox(ctx, "wd2"); err != nil {
		t.Fatalf("resume wd2: %v", err)
	}
	_, uffdPid := ca.PidsOf("wd2")
	if uffdPid == 0 {
		t.Fatal("no uffd handler pid for resumed wd2")
	}
	if err := syscall.Kill(uffdPid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill uffd handler: %v", err)
	}
	if got := awaitReap(t, ctx, agent, "wd2"); !strings.Contains(got, "uffd handler died") {
		t.Fatalf("wd2 reap cause = %q, want uffd handler died", got)
	}
}

// awaitReap polls Healthz (which drains the watchdog's failure reports)
// until one for id shows up, and asserts the sandbox left the roster.
func awaitReap(t *testing.T, ctx context.Context, agent nodeapi.Agent, id string) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var seen []string
	for time.Now().Before(deadline) {
		h, err := agent.Healthz(ctx)
		if err != nil {
			t.Fatalf("Healthz: %v", err)
		}
		seen = append(seen, h.FailedSandboxes...)
		for _, r := range seen {
			if strings.HasPrefix(r, id+": ") {
				if h.Sandboxes != 0 {
					t.Fatalf("reaped %s but roster still has %d sandboxes", id, h.Sandboxes)
				}
				return r
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("watchdog never reaped %s (reports: %v)", id, seen)
	return ""
}
