//go:build linux

package nodeagent_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/embervm/embervm/pkg/guestapi"
	"github.com/embervm/embervm/pkg/memsnap"
	"github.com/embervm/embervm/pkg/netns"
	"github.com/embervm/embervm/pkg/nodeagent"
	"github.com/embervm/embervm/pkg/nodeapi"
	"github.com/embervm/embervm/pkg/storage"
)

// M2 exit criteria (docs/zh/03 §3): pause→上传→异机 resume 全链路;
// 热恢复 P50 < 500ms; 温恢复 P99 < 3s. Latencies are CI-relative (ADR-0001).

func requireL1(t *testing.T) {
	t.Helper()
	if os.Getenv("EMBERVM_L1_DIR") == "" && os.Getenv("EMBERVM_L1_ENDPOINT") == "" {
		t.Skip("set EMBERVM_L1_DIR or EMBERVM_L1_ENDPOINT to run L1-backed tests")
	}
}

func percentile(durs []time.Duration, p float64) time.Duration {
	sorted := append([]time.Duration(nil), durs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(p*float64(len(sorted))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// dirtyGuestMemory makes the guest touch/dirty memory and disk so diffs and
// working sets are non-trivial.
func dirtyGuestMemory(t *testing.T, ctx context.Context, agent nodeapi.Agent, id string, mib int) {
	t.Helper()
	ex, err := agent.Exec(ctx, id, &guestapi.ExecRequest{
		Cmd:      "sh",
		Args:     []string{"-c", fmt.Sprintf("dd if=/dev/urandom of=/dirty.bin bs=1M count=%d 2>/dev/null && sync", mib)},
		TimeoutS: 120,
	})
	if err != nil {
		t.Fatalf("dirty exec: %v", err)
	}
	if ex.ExitCode != 0 {
		t.Fatalf("dirty exec exit=%d stderr=%s", ex.ExitCode, ex.Stderr)
	}
}

// TestHotRestoreP50 proves 热恢复 P50 < 500ms with the full M2 pipeline
// (chunked + WS prefetch + diff chain), 1 GiB guest, 15 GiB sparse data disk.
func TestHotRestoreP50(t *testing.T) {
	agent, image := kvmAgent(t, 2, func(c *nodeagent.Config) {
		c.RestoreMode = "chunked"
	})
	ctx := context.Background()
	const id = "hot1"

	if err := agent.BuildTemplate(ctx, "t-hot", image); err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	if _, err := agent.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
		SandboxID: id, TemplateID: "t-hot", VCPUs: 1, MemoryMiB: 1024, DataDiskGiB: 15,
	}); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	t.Cleanup(func() { _ = agent.StopSandbox(context.Background(), id) })
	dirtyGuestMemory(t, ctx, agent, id, 64)

	const iters = 15
	var durs []time.Duration
	for i := 0; i < iters; i++ {
		if err := agent.PauseSandbox(ctx, id); err != nil {
			t.Fatalf("pause %d: %v", i, err)
		}
		start := time.Now()
		if _, err := agent.ResumeSandbox(ctx, id); err != nil {
			t.Fatalf("resume %d: %v", i, err)
		}
		durs = append(durs, time.Since(start))
	}
	p50 := percentile(durs, 0.50)
	t.Logf("hot restore over %d iters (chunked+WS, 1GiB mem, 15GiB data): P50=%v P90=%v min=%v max=%v",
		iters, p50, percentile(durs, 0.90), durs[0], percentile(durs, 1.0))
	if p50 >= 500*time.Millisecond {
		t.Errorf("hot restore P50 = %v, want <500ms (M2 exit criterion)", p50)
	}
}

// TestWarmRestoreP99 proves 温恢复 P99 < 3s: every cycle wipes the node-local
// chunk cache after the write-through pause, so the resume pulls the working
// set and all faulted chunks from L1.
func TestWarmRestoreP99(t *testing.T) {
	requireL1(t)
	chunkDir := filepath.Join(t.TempDir(), "chunks")
	agent, image := kvmAgent(t, 2, func(c *nodeagent.Config) {
		c.RestoreMode = "chunked"
		c.ChunkStoreDir = chunkDir
	})
	ctx := context.Background()
	const id = "warm1"

	if err := agent.BuildTemplate(ctx, "t-warm", image); err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	if _, err := agent.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
		SandboxID: id, TemplateID: "t-warm", VCPUs: 1, MemoryMiB: 1024, DataDiskGiB: 15,
	}); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	t.Cleanup(func() { _ = agent.StopSandbox(context.Background(), id) })
	dirtyGuestMemory(t, ctx, agent, id, 64)

	const iters = 10
	var durs []time.Duration
	for i := 0; i < iters; i++ {
		if err := agent.PauseSandbox(ctx, id); err != nil {
			t.Fatalf("pause %d: %v", i, err)
		}
		// Simulate a cold node-local cache: memory must come from L1.
		if err := os.RemoveAll(filepath.Join(chunkDir, "objects")); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if _, err := agent.ResumeSandbox(ctx, id); err != nil {
			t.Fatalf("resume %d: %v", i, err)
		}
		durs = append(durs, time.Since(start))
	}
	p99 := percentile(durs, 0.99)
	t.Logf("warm restore over %d iters (chunk cache wiped, WS+faults from L1): P50=%v P99=%v",
		iters, percentile(durs, 0.50), p99)
	if p99 >= 3*time.Second {
		t.Errorf("warm restore P99 = %v, want <3s (M2 exit criterion)", p99)
	}
}

// TestCrossNodeRestore proves the pause→上传→异机 resume 全链路: node A
// pauses (write-through L1) and dies; node B — separate ZFS subtree, work
// dir, and empty chunk cache — rebuilds the sandbox from L1 alone and the
// SAME guest process continues (seq+1, markers intact, resumes+1).
func TestCrossNodeRestore(t *testing.T) {
	requireL1(t)
	pool := os.Getenv("EMBERVM_ZFS_POOL")
	if pool == "" {
		t.Skip("set EMBERVM_ZFS_POOL to run the cross-node restore test")
	}
	if os.Getenv("EMBERVM_KVM_TESTS") != "1" {
		t.Skip("set EMBERVM_KVM_TESTS=1 to run KVM tests")
	}
	for _, sub := range []string{pool + "/n1", pool + "/n2"} {
		if out, err := exec.Command("zfs", "create", "-p", sub).CombinedOutput(); err != nil {
			t.Fatalf("zfs create %s: %v: %s", sub, err, out)
		}
	}
	t.Cleanup(func() {
		_ = exec.Command("zfs", "destroy", "-r", pool+"/n1").Run()
		_ = exec.Command("zfs", "destroy", "-r", pool+"/n2").Run()
	})

	// One shared netns pool (lease names are node-global on a single host).
	scriptDir := os.Getenv("EMBERVM_SCRIPT_DIR")
	npool := netns.NewPool(scriptDir, 2)
	if err := npool.Setup(context.Background()); err != nil {
		t.Fatalf("netns pool: %v", err)
	}
	t.Cleanup(func() { _ = npool.Teardown(context.Background()) })

	newNode := func(sub string) nodeapi.Agent {
		agent, err := nodeagent.New(nodeagent.Config{
			Storage:        storage.NewZFSBackend(pool + "/" + sub),
			Pool:           npool,
			WorkDir:        t.TempDir(),
			ChunkStoreDir:  filepath.Join(t.TempDir(), "chunks-"+sub),
			KernelPath:     os.Getenv("EMBERVM_KERNEL"),
			FCBin:          os.Getenv("EMBERVM_FC_BIN"),
			UffdHandlerBin: os.Getenv("EMBERVM_UFFD_BIN"),
			GuestdBin:      os.Getenv("EMBERVM_GUESTD_BIN"),
			RestoreMode:    "chunked",
			FCVersion:      os.Getenv("FC_VERSION"),
			KernelVersion:  "6.1.155",
		})
		if err != nil {
			t.Fatalf("node %s: %v", sub, err)
		}
		return agent
	}
	nodeA, nodeB := newNode("n1"), newNode("n2")
	ctx := context.Background()
	const id = "xnode1"
	image := os.Getenv("EMBERVM_TEST_IMAGE")
	if image == "" {
		image = "alpine:3.20"
	}

	// --- node A: create, dirty, pause (write-through), die ----------------
	if err := nodeA.BuildTemplate(ctx, "t-x", image); err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	if _, err := nodeA.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
		SandboxID: id, TemplateID: "t-x", VCPUs: 1, MemoryMiB: 512, DataDiskGiB: 2,
	}); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if err := nodeA.WriteFile(ctx, id, "/xmarker", 0o644, []byte("cross-node")); err != nil {
		t.Fatal(err)
	}
	dirtyGuestMemory(t, ctx, nodeA, id, 32)
	hA, err := nodeA.Health(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := nodeA.PauseSandbox(ctx, id); err != nil {
		t.Fatalf("pause on node A: %v", err)
	}
	// Node A dies: its sandbox state, dataset, and chunk cache disappear.
	if err := nodeA.StopSandbox(ctx, id); err != nil {
		t.Fatalf("stop on node A: %v", err)
	}

	// --- node B: rebuild from L1 alone ------------------------------------
	start := time.Now()
	st, err := nodeB.(*nodeagent.Agent).RestoreSandbox(ctx, id, "warm")
	if err != nil {
		t.Fatalf("RestoreSandbox on node B: %v", err)
	}
	t.Cleanup(func() { _ = nodeB.StopSandbox(context.Background(), id) })
	t.Logf("cross-node restore (L1-only, cold cache): %v to %s", time.Since(start), st.State)

	hB, err := nodeB.Health(ctx, id)
	if err != nil {
		t.Fatalf("health on node B: %v", err)
	}
	if hB.Seq <= hA.Seq {
		t.Errorf("seq across nodes = %d -> %d: not monotonic, guest rebooted?", hA.Seq, hB.Seq)
	}
	if hB.Resumes != hA.Resumes+1 {
		t.Errorf("resumes across nodes = %d -> %d, want +1", hA.Resumes, hB.Resumes)
	}
	assertGuestFile(t, ctx, nodeB, id, "/xmarker", "cross-node")
	// The dirty file lives on the replicated data path — prove disk state
	// followed too.
	ex, err := nodeB.Exec(ctx, id, &guestapi.ExecRequest{Cmd: "ls", Args: []string{"-l", "/dirty.bin"}})
	if err != nil || ex.ExitCode != 0 {
		t.Errorf("dirty.bin missing after cross-node restore: %v (exit=%d)", err, ex.ExitCode)
	}
	t.Logf("pause→upload→异机 resume chain verified: seq %d→%d, resumes %d→%d",
		hA.Seq, hB.Seq, hA.Resumes, hB.Resumes)
}

// TestDiffChain proves layered snapshots stay correct and small: markers
// from every layer survive a 3-layer restore, and a diff layer stores far
// less than the full root.
func TestDiffChain(t *testing.T) {
	agent, image := kvmAgent(t, 2, func(c *nodeagent.Config) {
		c.RestoreMode = "chunked"
	})
	ctx := context.Background()
	const id = "diff1"

	if err := agent.BuildTemplate(ctx, "t-diff", image); err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	if _, err := agent.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
		SandboxID: id, TemplateID: "t-diff", VCPUs: 1, MemoryMiB: 512, DataDiskGiB: 2,
	}); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	t.Cleanup(func() { _ = agent.StopSandbox(context.Background(), id) })

	dirtyGuestMemory(t, ctx, agent, id, 32)
	writeMarker := func(n int) {
		if err := agent.WriteFile(ctx, id, fmt.Sprintf("/marker%d", n), 0o644, []byte(fmt.Sprintf("layer%d", n))); err != nil {
			t.Fatalf("marker%d: %v", n, err)
		}
	}
	cycle := func(n int) {
		if err := agent.PauseSandbox(ctx, id); err != nil {
			t.Fatalf("pause %d: %v", n, err)
		}
		if _, err := agent.ResumeSandbox(ctx, id); err != nil {
			t.Fatalf("resume %d: %v", n, err)
		}
	}

	writeMarker(1)
	cycle(1) // p1: Full
	writeMarker(2)
	dirtyGuestMemory(t, ctx, agent, id, 16)
	cycle(2) // p2: Diff
	writeMarker(3)
	cycle(3) // p3: Diff — restore resolves the whole chain

	for n := 1; n <= 3; n++ {
		assertGuestFile(t, ctx, agent, id, fmt.Sprintf("/marker%d", n), fmt.Sprintf("layer%d", n))
	}

	snapDir := filepath.Join(agent.(*nodeagent.Agent).WorkDirOf(id), "snap")
	full, err := memsnap.ReadManifest(filepath.Join(snapDir, "layer-p1.json"))
	if err != nil {
		t.Fatal(err)
	}
	readDiff := func(layer string) *memsnap.Manifest {
		m, err := memsnap.ReadManifest(filepath.Join(snapDir, "layer-"+layer+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if m.Kind != memsnap.KindDiff {
			t.Errorf("%s kind = %s, want diff", layer, m.Kind)
		}
		t.Logf("layer %s: %d chunks, %d stored bytes (full root: %d)",
			layer, len(m.Chunks), storedBytes(m), storedBytes(full))
		return m
	}
	p2, p3 := readDiff("p2"), readDiff("p3")
	fb := storedBytes(full)

	// The claim under test: diff layers scale with what the guest DIRTIED,
	// not with memory size. Broken dirty tracking would make every diff
	// restate the whole footprint (≈100% of the full root's stored bytes).
	// p2 deliberately dirtied 16 MiB of incompressible data, so it must
	// carry at least that much — but still be well below the full root.
	if db := storedBytes(p2); db < 16<<20 || db*4 >= fb*3 {
		t.Errorf("p2 stored %d bytes; want >=16MiB of captured dirty data and <75%% of full (%d) — dirty-page diffing broken", db, fb)
	}
	// p3 only wrote a marker file: ambient churn only, far below the root.
	if db := storedBytes(p3); db*4 >= fb {
		t.Errorf("p3 stored %d bytes >= 25%% of full (%d) — a no-dirty cycle must produce a small diff", db, fb)
	}
}

// TestDedupReport measures (report-only) content dedup between two sandboxes
// of the same template — the chunk repo's 同 base 去重 promise.
func TestDedupReport(t *testing.T) {
	agent, image := kvmAgent(t, 3, func(c *nodeagent.Config) {
		c.RestoreMode = "chunked"
	})
	ctx := context.Background()

	if err := agent.BuildTemplate(ctx, "t-dedup", image); err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	manifests := map[string]*memsnap.Manifest{}
	for _, id := range []string{"dd1", "dd2"} {
		if _, err := agent.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
			SandboxID: id, TemplateID: "t-dedup", VCPUs: 1, MemoryMiB: 256, DataDiskGiB: 1,
		}); err != nil {
			t.Fatalf("CreateSandbox %s: %v", id, err)
		}
		t.Cleanup(func() { _ = agent.StopSandbox(context.Background(), id) })
		if err := agent.PauseSandbox(ctx, id); err != nil {
			t.Fatalf("pause %s: %v", id, err)
		}
		m, err := memsnap.ReadManifest(filepath.Join(agent.(*nodeagent.Agent).WorkDirOf(id), "snap", "layer-p1.json"))
		if err != nil {
			t.Fatal(err)
		}
		manifests[id] = m
	}

	seen := map[string]bool{}
	var total, shared, zero int
	for _, c := range manifests["dd1"].Chunks {
		if c.Zero {
			continue
		}
		seen[c.Hash] = true
	}
	for _, c := range manifests["dd2"].Chunks {
		if c.Zero {
			zero++
			continue
		}
		total++
		if seen[c.Hash] {
			shared++
		}
	}
	t.Logf("dedup report: sandbox dd2 has %d non-zero chunks; %d (%.1f%%) already stored by dd1; %d zero chunks skipped entirely",
		total, shared, 100*float64(shared)/float64(max(total, 1)), zero)
}
