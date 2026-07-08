//go:build linux

package nodeagent_test

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/embervm/embervm/pkg/nodeagent"
	"github.com/embervm/embervm/pkg/nodeapi"
	"github.com/embervm/embervm/pkg/storage"
)

// M5 fork/rollback verbs (master-spec 2026-07-09). Both need chunked +
// jailed + ZFS — the golden fast-create conditions — so the fixture mirrors
// TestFastCreateUnder500ms minus the golden config.

// m5Agent builds a jailed+chunked+ZFS agent on a fresh pool subtree.
func m5Agent(t *testing.T, subtree string, poolSize int) nodeapi.Agent {
	t.Helper()
	jailerBin := os.Getenv("EMBERVM_JAILER_BIN")
	if jailerBin == "" {
		t.Skip("set EMBERVM_JAILER_BIN to run fork/rollback tests")
	}
	pool := os.Getenv("EMBERVM_ZFS_POOL")
	if pool == "" {
		t.Skip("set EMBERVM_ZFS_POOL to run fork/rollback tests")
	}
	if out, err := exec.Command("zfs", "create", "-p", pool+"/"+subtree).CombinedOutput(); err != nil {
		t.Fatalf("zfs create: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("zfs", "destroy", "-r", pool+"/"+subtree).Run() })

	agent, image := kvmAgent(t, poolSize, func(c *nodeagent.Config) {
		c.Storage = storage.NewZFSBackend(pool + "/" + subtree)
		c.RestoreMode = "chunked"
		c.JailerBin = jailerBin
		c.JailerChrootBase = t.TempDir()
		c.FCVersion = os.Getenv("FC_VERSION")
		c.KernelVersion = "6.1.155"
	})
	if err := agent.BuildTemplate(context.Background(), "tmpl-"+subtree, image); err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	return agent
}

// checkpoint drives the existing snapshot verb and returns the layer name
// ("p<N>") it produced — the same parse the control plane does (the verb's
// return format "<id>@<tag>-<N>" is producer-defined).
func checkpoint(t *testing.T, ctx context.Context, agent nodeapi.Agent, id, tag string) string {
	t.Helper()
	snapID, err := agent.SnapshotSandbox(ctx, id, tag)
	if err != nil {
		t.Fatalf("checkpoint %s: %v", tag, err)
	}
	seq := snapID[strings.LastIndex(snapID, "-")+1:]
	if _, err := strconv.Atoi(seq); err != nil {
		t.Fatalf("unparseable snapshot id %q", snapID)
	}
	return "p" + seq
}

// TestForkKVM proves the fork core: a child born from a parent's checkpoint
// sees exactly the checkpointed state, runs independently, and never
// disturbs the parent (whose machine never leaves RUNNING).
func TestForkKVM(t *testing.T) {
	agent := m5Agent(t, "m5f", 3)
	ctx := context.Background()

	if _, err := agent.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
		SandboxID: "par1", TemplateID: "tmpl-m5f", MemoryMiB: 256, DataDiskGiB: 1,
	}); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	defer func() { _ = agent.StopSandbox(ctx, "par1") }()

	if err := agent.WriteFile(ctx, "par1", "/state", 0o644, []byte("base")); err != nil {
		t.Fatal(err)
	}
	layer := checkpoint(t, ctx, agent, "par1", "cp1")

	// Post-checkpoint parent state must NOT reach the child.
	if err := agent.WriteFile(ctx, "par1", "/post", 0o644, []byte("after")); err != nil {
		t.Fatal(err)
	}

	st, err := agent.Fork(ctx, "par1", layer, "child1")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	defer func() { _ = agent.StopSandbox(ctx, "child1") }()
	if st.State != "RUNNING" {
		t.Fatalf("child state = %s", st.State)
	}

	assertGuestFile(t, ctx, agent, "child1", "/state", "base")
	if _, err := agent.ReadFile(ctx, "child1", "/post"); err == nil {
		t.Fatal("child sees post-checkpoint parent state (/post)")
	}
	// Divergence: the child's writes stay its own.
	if err := agent.WriteFile(ctx, "child1", "/branch", 0o644, []byte("child-only")); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.ReadFile(ctx, "par1", "/branch"); err == nil {
		t.Fatal("parent sees the child's write (/branch)")
	}
	// The parent never stopped: still RUNNING, still serving, /post intact.
	pst, err := agent.Status(ctx, "par1")
	if err != nil || pst.State != "RUNNING" {
		t.Fatalf("parent status = %+v err=%v", pst, err)
	}
	assertGuestFile(t, ctx, agent, "par1", "/post", "after")
	t.Logf("fork verified: child at checkpoint state, parent untouched, branches diverge")
}

// TestRollbackKVM proves the layer switch: rollback discards everything
// after the target checkpoint — guest memory, disk writes, and the later
// checkpoint's layers — and the chain keeps working afterwards (monotone
// tags, next checkpoint, resume).
func TestRollbackKVM(t *testing.T) {
	agent := m5Agent(t, "m5r", 2)
	ctx := context.Background()

	if _, err := agent.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
		SandboxID: "rb1", TemplateID: "tmpl-m5r", MemoryMiB: 256, DataDiskGiB: 1,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = agent.StopSandbox(ctx, "rb1") }()

	if err := agent.WriteFile(ctx, "rb1", "/a", 0o644, []byte("keep")); err != nil {
		t.Fatal(err)
	}
	target := checkpoint(t, ctx, agent, "rb1", "cp1")

	if err := agent.WriteFile(ctx, "rb1", "/b", 0o644, []byte("discard")); err != nil {
		t.Fatal(err)
	}
	_ = checkpoint(t, ctx, agent, "rb1", "cp2") // a LATER checkpoint the rollback must discard

	st, err := agent.Rollback(ctx, "rb1", target)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if st.State != "RUNNING" {
		t.Fatalf("state after rollback = %s", st.State)
	}
	assertGuestFile(t, ctx, agent, "rb1", "/a", "keep")
	if _, err := agent.ReadFile(ctx, "rb1", "/b"); err == nil {
		t.Fatal("post-checkpoint write survived rollback (/b)")
	}

	// The chain continues: a fresh checkpoint after rollback gets a HIGHER
	// tag than anything discarded (monotone seq contract), and the sandbox
	// keeps full pause/resume health.
	next := checkpoint(t, ctx, agent, "rb1", "cp3")
	tn, _ := strconv.Atoi(strings.TrimPrefix(next, "p"))
	tt, _ := strconv.Atoi(strings.TrimPrefix(target, "p"))
	if tn <= tt+1 {
		t.Fatalf("post-rollback checkpoint %s not above the discarded range (target %s)", next, target)
	}
	assertGuestFile(t, ctx, agent, "rb1", "/a", "keep")
	t.Logf("rollback verified: %s -> %s discarded later state, chain resumed at %s", target, next, next)
}
