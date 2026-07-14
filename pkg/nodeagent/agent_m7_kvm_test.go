//go:build linux

package nodeagent_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/embervm/embervm/pkg/chunkstore"
	"github.com/embervm/embervm/pkg/guestapi"
	"github.com/embervm/embervm/pkg/nodeagent"
	"github.com/embervm/embervm/pkg/nodeapi"
	"github.com/embervm/embervm/pkg/storage"
)

// M7 gate (ADR-0008 default-elastic geometry): the ELASTIC golden slot
// fast-creates default sandboxes at M4 speed, and — the interaction no M6
// gate proved — a jailed golden CLONE (new identity, clone-restored under
// uffd) still answers PATCH /hotplug/memory, across pause/resume and a
// second resize. Plus the memmap-tax measurement the default ceiling's
// sizing rests on (ADR-0007 quotes ~1.6% of the hotplug region, resident in
// guest boot memory; nothing in code enforces it).
func TestFastCreateElasticKVM(t *testing.T) {
	jailerBin := os.Getenv("EMBERVM_JAILER_BIN")
	if jailerBin == "" {
		t.Skip("set EMBERVM_JAILER_BIN to run the elastic fast-create test")
	}
	requireL1(t)
	pool := os.Getenv("EMBERVM_ZFS_POOL")
	if pool == "" {
		t.Skip("set EMBERVM_ZFS_POOL to run the elastic fast-create test")
	}
	if out, err := exec.Command("zfs", "create", "-p", pool+"/m7fast").CombinedOutput(); err != nil {
		t.Fatalf("zfs create: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("zfs", "destroy", "-r", pool+"/m7fast").Run() })

	agent, image := kvmAgent(t, 14, func(c *nodeagent.Config) {
		c.Storage = storage.NewZFSBackend(pool + "/m7fast")
		c.RestoreMode = "chunked"
		c.JailerBin = jailerBin
		c.JailerChrootBase = t.TempDir()
		c.FCVersion = os.Getenv("FC_VERSION")
		c.KernelVersion = "6.1.155"
		c.GoldenVCPUs = 1
		c.GoldenMemoryMiB = 256
		c.GoldenDataDiskGiB = 1
		c.GoldenMaxMemoryMiB = 1024 // elastic golden slot (M7)
		c.GoldenMaxVCPUs = 2
	})
	ctx := context.Background()

	// (1) One template build produces BOTH golden slots in L1.
	if err := agent.BuildTemplate(ctx, "tmpl-m7", image); err != nil {
		t.Fatalf("BuildTemplate (with golden snapshots): %v", err)
	}
	l1, _, err := chunkstore.L1FromEnv()
	if err != nil {
		t.Fatalf("L1FromEnv: %v", err)
	}
	for _, key := range []string{"templates/tmpl-m7/golden.json", "templates/tmpl-m7/golden-elastic.json"} {
		ok, err := l1.HasObject(ctx, key)
		if err != nil || !ok {
			t.Fatalf("golden meta %s in L1 = %v, %v; want present", key, ok, err)
		}
	}

	// (2) Memmap-tax probe: a 4 GiB ceiling (no matching golden → cold
	// boot) must leave the 256 MiB base usable. ~1.6% of the 3840 MiB
	// region ≈ 61 MiB is the documented expectation; below 100 MiB
	// available the default 256/4096 pairing would be unshippable.
	if _, err := agent.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
		SandboxID: "m7tax", TemplateID: "tmpl-m7",
		VCPUs: 1, MemoryMiB: 256, DataDiskGiB: 1, MaxMemoryMiB: 4096, MaxVCPUs: 4,
	}); err != nil {
		t.Fatalf("memmap-tax probe create: %v", err)
	}
	h := mustHealth(t, ctx, agent, "m7tax")
	taxTotal, taxAvail := int(h.MemTotalKiB/1024), int(h.MemAvailableKiB/1024)
	t.Logf("memmap-tax probe (4096 MiB ceiling on 256 MiB base): MemTotal %d MiB, MemAvailable %d MiB", taxTotal, taxAvail)
	if taxAvail < 100 {
		t.Errorf("guest MemAvailable = %d MiB with a 4 GiB ceiling — memmap tax eats the base; default ceiling needs retuning", taxAvail)
	}
	_ = agent.StopSandbox(ctx, "m7tax")

	// (3) Default-elastic geometry rides the elastic golden: P50 < 500ms.
	const n = 5
	var times []time.Duration
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("m7e%d", i)
		start := time.Now()
		if _, err := agent.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
			SandboxID: id, TemplateID: "tmpl-m7",
			VCPUs: 1, MemoryMiB: 256, DataDiskGiB: 1,
			MaxMemoryMiB: 1024, MaxVCPUs: 2, // elastic golden geometry → fast path
		}); err != nil {
			t.Fatalf("elastic fast create %s: %v", id, err)
		}
		times = append(times, time.Since(start))
		t.Cleanup(func() { _ = agent.StopSandbox(context.Background(), id) })
	}
	p50 := percentile(times, 0.50)
	t.Logf("elastic fast-create to interactive: %v (P50 %v)", times, p50)
	if p50 >= 500*time.Millisecond {
		t.Errorf("elastic fast-create P50 = %v, want <500ms (default-elastic must not lose G4)", p50)
	}

	// (4) THE unproven interaction: resize a jailed golden CLONE. The
	// hotplug region config rode the golden snapfile through cloneRestore
	// into a new identity; PATCH /hotplug/memory must converge there.
	const id = "m7e0"
	res, err := agent.ResizeSandbox(ctx, id, nodeapi.ResizeRequest{MemoryMiB: 768})
	if err != nil {
		t.Fatalf("resize golden clone: %v", err)
	}
	if res.MemoryMiB != 768 {
		t.Fatalf("resize achieved %d MiB, want 768 (growth must converge)", res.MemoryMiB)
	}
	if got := guestMemTotalMiB(t, ctx, agent, id); got < 700 {
		t.Fatalf("guest MemTotal after grow = %d MiB, want ~768", got)
	}
	// The grown memory is real: fill tmpfs past the 256 MiB base. /tmp's
	// size was fixed at mount time as 50% of the BOOT 256 MiB, so grow it
	// first — the remount would fail anyway if the plugged pages weren't
	// actually online.
	if resp, err := agent.Exec(ctx, id, execReq("sh", "-c",
		"mount -o remount,size=640m /tmp && dd if=/dev/zero of=/tmp/fill bs=1M count=384 && rm /tmp/fill")); err != nil || resp.ExitCode != 0 {
		t.Fatalf("dirty hotplugged memory: %v exit %d %s", err, resp.ExitCode, resp.Stderr)
	}
	// CPU quota moves on the clone too (boot cores = elastic max 2).
	if res, err = agent.ResizeSandbox(ctx, id, nodeapi.ResizeRequest{VCPUs: 2}); err != nil || res.VCPUs != 2 {
		t.Fatalf("cpu resize on clone: %v (achieved %d)", err, res.VCPUs)
	}

	// (5) The resized clone snapshots and restores with its plug state,
	// then resizes AGAIN (post-restore hotplug PATCH on a clone identity).
	if err := agent.WriteFile(ctx, id, "/m7mark", 0o644, []byte("elastic")); err != nil {
		t.Fatal(err)
	}
	h0 := mustHealth(t, ctx, agent, id)
	if err := agent.PauseSandbox(ctx, id); err != nil {
		t.Fatalf("pause resized clone: %v", err)
	}
	if _, err := agent.ResumeSandbox(ctx, id); err != nil {
		t.Fatalf("resume resized clone: %v", err)
	}
	if h1 := mustHealth(t, ctx, agent, id); h1.Seq <= h0.Seq {
		t.Fatalf("seq across resume = %d -> %d: guest rebooted?", h0.Seq, h1.Seq)
	}
	assertGuestFile(t, ctx, agent, id, "/m7mark", "elastic")
	if got := guestMemTotalMiB(t, ctx, agent, id); got < 700 {
		t.Fatalf("guest MemTotal after resume = %d MiB, want plug state preserved (~768)", got)
	}
	if res, err = agent.ResizeSandbox(ctx, id, nodeapi.ResizeRequest{MemoryMiB: 1024}); err != nil || res.MemoryMiB != 1024 {
		t.Fatalf("post-restore resize: %v (achieved %d)", err, res.MemoryMiB)
	}
	if got := guestMemTotalMiB(t, ctx, agent, id); got < 950 {
		t.Fatalf("guest MemTotal after post-restore grow = %d MiB, want ~1024", got)
	}

	// (6) Regression: fixed geometry still rides the FIXED golden slot.
	start := time.Now()
	if _, err := agent.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
		SandboxID: "m7fixed", TemplateID: "tmpl-m7",
		VCPUs: 1, MemoryMiB: 256, DataDiskGiB: 1,
	}); err != nil {
		t.Fatalf("fixed fast create: %v", err)
	}
	t.Cleanup(func() { _ = agent.StopSandbox(context.Background(), "m7fixed") })
	fixedDur := time.Since(start)
	t.Logf("fixed fast-create alongside elastic golden: %v", fixedDur)
	if fixedDur >= time.Second {
		t.Errorf("fixed fast-create = %v — did the fixed golden slot regress to cold boot?", fixedDur)
	}
	if ex, err := agent.Exec(ctx, "m7fixed", &guestapi.ExecRequest{Cmd: "echo", Args: []string{"ok"}}); err != nil || string(ex.Stdout) != "ok\n" {
		t.Fatalf("exec on fixed clone: %v %q", err, ex.Stdout)
	}
}
