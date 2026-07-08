//go:build linux

package nodeagent_test

import (
	"context"
	"os"
	"testing"

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
