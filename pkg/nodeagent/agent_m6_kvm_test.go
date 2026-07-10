//go:build linux

package nodeagent_test

import (
	"bufio"
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/embervm/embervm/pkg/guestapi"
	"github.com/embervm/embervm/pkg/nodeagent"
	"github.com/embervm/embervm/pkg/nodeapi"
)

// guestMemTotalMiB reads MemTotal from the guest's /proc/meminfo. Exec, not
// ReadFile: guestd's file reads honor the stat size, and procfs stats 0.
func guestMemTotalMiB(t *testing.T, ctx context.Context, agent nodeapi.Agent, id string) int {
	t.Helper()
	resp, err := agent.Exec(ctx, id, execReq("cat", "/proc/meminfo"))
	if err != nil {
		t.Fatalf("cat /proc/meminfo: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("cat /proc/meminfo: exit %d: %s", resp.ExitCode, resp.Stderr)
	}
	sc := bufio.NewScanner(strings.NewReader(string(resp.Stdout)))
	for sc.Scan() {
		if kb, ok := strings.CutPrefix(sc.Text(), "MemTotal:"); ok {
			n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(kb), " kB"))
			if err != nil {
				t.Fatalf("parse MemTotal %q: %v", sc.Text(), err)
			}
			return n / 1024
		}
	}
	t.Fatal("MemTotal not found in guest /proc/meminfo")
	return 0
}

// TestVirtioMemResizeKVM is the M6 Phase-0 go/no-go experiment AND the
// standing resize gate (ADR-0007): it exercises the one interaction the
// Firecracker docs leave unstated — whether PATCH /hotplug/memory still
// works on a snapshot-RESTORED VM — plus the full grow → dirty → shrink →
// chunked pause → uffd restore → resize-again loop. Failure of step 6 means
// the milestone falls back to the balloon-headroom design.
func TestVirtioMemResizeKVM(t *testing.T) {
	agent, image := kvmAgent(t, 1, func(c *nodeagent.Config) {
		c.RestoreMode = "chunked"
	})
	ctx := context.Background()
	const id = "m6resize"

	if err := agent.BuildTemplate(ctx, "tmpl-m6", image); err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	// 256 MiB base + a hotplug region up to 1 GiB.
	if _, err := agent.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
		SandboxID: id, TemplateID: "tmpl-m6",
		MemoryMiB: 256, MaxMemoryMiB: 1024, DataDiskGiB: 1,
	}); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	defer func() { _ = agent.StopSandbox(ctx, id) }()
	ca := agent.(*nodeagent.Agent)

	base := guestMemTotalMiB(t, ctx, agent, id)
	if base > 300 {
		t.Fatalf("baseline MemTotal = %d MiB: hotplug region appears pre-plugged (want ~256)", base)
	}
	if err := agent.WriteFile(ctx, id, "/tmp/marker", 0o644, []byte("before-resize")); err != nil {
		t.Fatal(err)
	}

	// --- 1. grow 256 -> 768 ------------------------------------------------
	res, err := ca.ResizeSandbox(ctx, id, nodeapi.ResizeRequest{MemoryMiB: 768})
	if err != nil {
		t.Fatalf("grow to 768: %v", err)
	}
	if res.MemoryMiB != 768 {
		t.Fatalf("grow achieved %d MiB, want 768", res.MemoryMiB)
	}
	grown := guestMemTotalMiB(t, ctx, agent, id)
	if grown < base+384 {
		t.Fatalf("MemTotal after grow = %d MiB (baseline %d): hotplugged memory not onlined", grown, base)
	}
	t.Logf("grow ok: MemTotal %d -> %d MiB", base, grown)

	// --- 2. the new memory is actually usable: dirty ~400 MiB of tmpfs,
	// far beyond the 256 MiB base ------------------------------------------
	if _, err := agent.Exec(ctx, id, execReq("dd", "if=/dev/zero", "of=/tmp/big", "bs=1M", "count=400")); err != nil {
		t.Fatalf("dirtying hotplugged memory: %v", err)
	}
	if _, err := agent.Exec(ctx, id, execReq("rm", "/tmp/big")); err != nil {
		t.Fatal(err)
	}

	// --- 3. shrink 768 -> 384 (cooperative; partial is legal, progress is
	// not optional) ----------------------------------------------------------
	res, err = ca.ResizeSandbox(ctx, id, nodeapi.ResizeRequest{MemoryMiB: 384})
	if err != nil {
		t.Fatalf("shrink to 384: %v", err)
	}
	if res.MemoryMiB >= 768 {
		t.Fatalf("shrink achieved nothing: still %d MiB", res.MemoryMiB)
	}
	t.Logf("shrink ok: effective %d MiB (asked 384)", res.MemoryMiB)

	// --- 4. chunked pause + uffd restore ------------------------------------
	h0 := mustHealth(t, ctx, agent, id)
	if err := agent.PauseSandbox(ctx, id); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := agent.ResumeSandbox(ctx, id); err != nil {
		t.Fatalf("resume: %v", err)
	}
	h1 := mustHealth(t, ctx, agent, id)
	if h1.Seq <= h0.Seq {
		t.Fatalf("seq %d -> %d: guest rebooted across pause/resume", h0.Seq, h1.Seq)
	}
	assertGuestFile(t, ctx, agent, id, "/tmp/marker", "before-resize")

	// --- 5. MemTotal survived the restore ------------------------------------
	restored := guestMemTotalMiB(t, ctx, agent, id)
	if restored < res.MemoryMiB-64 || restored > res.MemoryMiB+64 {
		t.Fatalf("MemTotal after restore = %d MiB, want ~%d (plug state should ride the snapshot)", restored, res.MemoryMiB)
	}

	// --- 6. THE go/no-go: resize again on the restored VM --------------------
	res2, err := ca.ResizeSandbox(ctx, id, nodeapi.ResizeRequest{MemoryMiB: 896})
	if err != nil {
		t.Fatalf("NO-GO: resize after uffd restore failed (balloon-headroom fallback required): %v", err)
	}
	if res2.MemoryMiB != 896 {
		t.Fatalf("post-restore grow achieved %d MiB, want 896", res2.MemoryMiB)
	}
	regrown := guestMemTotalMiB(t, ctx, agent, id)
	if regrown < restored+256 {
		t.Fatalf("MemTotal after post-restore grow = %d MiB (was %d): not onlined", regrown, restored)
	}
	t.Logf("post-restore grow ok: MemTotal %d -> %d MiB", restored, regrown)

	// --- 7. hot-UNplug under the live uffd handler (EVENT_REMOVE path),
	// then one more pause/resume so the removed ranges round-trip a snapshot -
	if _, err = ca.ResizeSandbox(ctx, id, nodeapi.ResizeRequest{MemoryMiB: 512}); err != nil {
		t.Fatalf("post-restore shrink: %v", err)
	}
	if err := agent.PauseSandbox(ctx, id); err != nil {
		t.Fatalf("pause 2: %v", err)
	}
	if _, err := agent.ResumeSandbox(ctx, id); err != nil {
		t.Fatalf("resume 2: %v", err)
	}
	h2 := mustHealth(t, ctx, agent, id)
	if h2.Seq <= h1.Seq {
		t.Fatalf("seq %d -> %d: guest rebooted across second pause/resume", h1.Seq, h2.Seq)
	}
	assertGuestFile(t, ctx, agent, id, "/tmp/marker", "before-resize")
	t.Log("GO: virtio-mem resize survives chunked pause / uffd restore, both directions")
}

func execReq(cmd string, args ...string) *guestapi.ExecRequest {
	return &guestapi.ExecRequest{Cmd: cmd, Args: args}
}
