//go:build linux

package nodeagent_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/embervm/embervm/pkg/guestapi"
	"github.com/embervm/embervm/pkg/memsnap"
	"github.com/embervm/embervm/pkg/nodeagent"
	"github.com/embervm/embervm/pkg/nodeapi"
)

// storedBytes sums the stored (compressed) size of a layer's chunks.
func storedBytes(m *memsnap.Manifest) int64 {
	var n int64
	for _, c := range m.Chunks {
		n += int64(c.CLen)
	}
	return n
}

// TestChunkedLifecycleKVM drives the full M2 restore pipeline on one node:
// chunked Full pause -> WS-recording resume -> Diff pause -> WS-replay
// resume, asserting seq continuity, marker durability, diff layer size,
// resume-hook counter, and the recorded working set.
func TestChunkedLifecycleKVM(t *testing.T) {
	agent, image := kvmAgent(t, 1, func(c *nodeagent.Config) {
		c.RestoreMode = "chunked"
		c.FCVersion = os.Getenv("FC_VERSION")
		c.KernelVersion = "6.1.155"
	})
	ctx := context.Background()
	const id = "m2chunk"

	if err := agent.BuildTemplate(ctx, "tmpl-m2", image); err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	if _, err := agent.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
		SandboxID: id, TemplateID: "tmpl-m2", MemoryMiB: 256, DataDiskGiB: 1,
	}); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	defer func() { _ = agent.StopSandbox(ctx, id) }()

	if err := agent.WriteFile(ctx, id, "/tmp/marker1", 0o644, []byte("first")); err != nil {
		t.Fatalf("write marker1: %v", err)
	}
	h0, err := agent.Health(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	// --- pause 1: Full layer --------------------------------------------
	if err := agent.PauseSandbox(ctx, id); err != nil {
		t.Fatalf("pause 1: %v", err)
	}
	ca := agent.(*nodeagent.Agent)
	snapDir := filepath.Join(ca.WorkDirOf(id), "snap")
	full, err := memsnap.ReadManifest(filepath.Join(snapDir, "layer-p1.json"))
	if err != nil {
		t.Fatalf("layer-p1.json: %v", err)
	}
	if full.Kind != memsnap.KindFull {
		t.Fatalf("layer p1 kind = %s, want full", full.Kind)
	}
	if _, err := os.Stat(filepath.Join(snapDir, "memfile-p1")); !os.IsNotExist(err) {
		t.Fatal("raw memfile survived chunkify (chunk store should be the source of truth)")
	}

	// --- resume 1: records the working set -------------------------------
	if _, err := agent.ResumeSandbox(ctx, id); err != nil {
		t.Fatalf("resume 1: %v", err)
	}
	h1 := mustHealth(t, ctx, agent, id)
	// Monotone-above-pause proves the SAME process continued (a reboot
	// resets to 1). Exact +1 is racy: the resume readiness probe (and any
	// client-timed-out probe the server still counted) also increments.
	if h1.Seq <= h0.Seq {
		t.Fatalf("seq across resume 1 = %d -> %d: not monotonic, guest rebooted?", h0.Seq, h1.Seq)
	}
	if h1.Resumes != 1 {
		t.Fatalf("resumes after resume 1 = %d, want 1", h1.Resumes)
	}
	assertGuestFile(t, ctx, agent, id, "/tmp/marker1", "first")

	if err := agent.WriteFile(ctx, id, "/tmp/marker2", 0o644, []byte("second")); err != nil {
		t.Fatal(err)
	}

	// --- pause 2: Diff layer + WS persisted ------------------------------
	if err := agent.PauseSandbox(ctx, id); err != nil {
		t.Fatalf("pause 2: %v", err)
	}
	diff, err := memsnap.ReadManifest(filepath.Join(snapDir, "layer-p2.json"))
	if err != nil {
		t.Fatalf("layer-p2.json: %v", err)
	}
	if diff.Kind != memsnap.KindDiff || diff.Parent != "p1" {
		t.Fatalf("layer p2 = kind %s parent %s, want diff/p1", diff.Kind, diff.Parent)
	}
	if db, fb := storedBytes(diff), storedBytes(full); db >= fb/2 {
		t.Errorf("diff layer stored %d bytes vs full %d — diff should be far smaller", db, fb)
	}
	if _, err := os.Stat(filepath.Join(snapDir, "ws.json")); err != nil {
		t.Fatalf("working set not recorded by first resume: %v", err)
	}

	// --- resume 2: WS replay over the diff chain --------------------------
	if _, err := agent.ResumeSandbox(ctx, id); err != nil {
		t.Fatalf("resume 2: %v", err)
	}
	h2 := mustHealth(t, ctx, agent, id)
	if h2.Seq <= h1.Seq {
		t.Fatalf("seq across resume 2 = %d -> %d: not monotonic, guest rebooted?", h1.Seq, h2.Seq)
	}
	if h2.Resumes != 2 {
		t.Fatalf("resumes after resume 2 = %d, want 2", h2.Resumes)
	}
	assertGuestFile(t, ctx, agent, id, "/tmp/marker1", "first")
	assertGuestFile(t, ctx, agent, id, "/tmp/marker2", "second")
}

func mustHealth(t *testing.T, ctx context.Context, agent nodeapi.Agent, id string) *guestapi.HealthResponse {
	t.Helper()
	h, err := agent.Health(ctx, id)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	return h
}

func assertGuestFile(t *testing.T, ctx context.Context, agent nodeapi.Agent, id, path, want string) {
	t.Helper()
	got, err := agent.ReadFile(ctx, id, path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}
