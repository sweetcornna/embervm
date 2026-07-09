//go:build linux

package nodeagent_test

// Manual stress harness (EMBERVM_STRESS=1): find the HOST's practical
// limits — concurrent sandboxes, fork fan-out, resume latency. Not a CI
// gate: numbers are host-relative, and the polite guards (MemAvailable /
// disk floor) stop the ramp before the host OOMs, so "LIMIT" means "this
// host, this politeness", not a product ceiling.

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/embervm/embervm/pkg/guestapi"
	"github.com/embervm/embervm/pkg/nodeagent"
	"github.com/embervm/embervm/pkg/nodeapi"
	"github.com/embervm/embervm/pkg/storage"
	"golang.org/x/sys/unix"
)

func stressEnv(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// memAvailableMiB reads MemAvailable from /proc/meminfo.
func memAvailableMiB(t *testing.T) int {
	t.Helper()
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if kb, ok := strings.CutPrefix(sc.Text(), "MemAvailable:"); ok {
			n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(kb), " kB"))
			return n / 1024
		}
	}
	return 0
}

func diskFreeGiB(path string) float64 {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0
	}
	return float64(st.Bavail) * float64(st.Bsize) / (1 << 30)
}

// TestStressConcurrencyRamp creates sandboxes in batches until the target,
// a failure, or the politeness floors (1 GiB RAM / 3 GiB disk left).
func TestStressConcurrencyRamp(t *testing.T) {
	if os.Getenv("EMBERVM_STRESS") != "1" {
		t.Skip("set EMBERVM_STRESS=1 for manual stress runs")
	}
	target := stressEnv("EMBERVM_STRESS_MAX", 100)
	const batch = 10
	// The plain dev backend copies the whole rootfs per sandbox (~15
	// sandboxes empty a 20GiB host). EMBERVM_STRESS_ZFS=1 runs the ramp on
	// the production storage instead: clones are O(1) and copy nothing.
	var opts []func(*nodeagent.Config)
	if os.Getenv("EMBERVM_STRESS_ZFS") == "1" {
		pool := os.Getenv("EMBERVM_ZFS_POOL")
		if pool == "" {
			t.Skip("EMBERVM_STRESS_ZFS=1 needs EMBERVM_ZFS_POOL")
		}
		if out, err := exec.Command("zfs", "create", "-p", pool+"/stressc").CombinedOutput(); err != nil {
			t.Fatalf("zfs create: %v: %s", err, out)
		}
		t.Cleanup(func() { _ = exec.Command("zfs", "destroy", "-r", pool+"/stressc").Run() })
		opts = append(opts, func(c *nodeagent.Config) {
			c.Storage = storage.NewZFSBackend(pool + "/stressc")
		})
	}
	agent, image := kvmAgent(t, target+4, opts...)
	ctx := context.Background()

	if err := agent.BuildTemplate(ctx, "stress", image); err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}

	var live []string
	t.Cleanup(func() {
		for _, id := range live {
			_ = agent.StopSandbox(context.Background(), id)
		}
	})

	stopReason := "reached target"
	for len(live) < target {
		if m := memAvailableMiB(t); m < 1024 {
			stopReason = fmt.Sprintf("MemAvailable %dMiB < 1GiB floor", m)
			break
		}
		if d := diskFreeGiB(os.TempDir()); d < 3 {
			stopReason = fmt.Sprintf("disk free %.1fGiB < 3GiB floor", d)
			break
		}
		n := batch
		if r := target - len(live); r < n {
			n = r
		}
		start := time.Now()
		errs := make(chan error, n)
		ids := make(chan string, n)
		// Cold boots are CPU-heavy (full kernel init): a simultaneous burst
		// stampedes past the 30s guest budget on few cores. The question
		// here is how many sandboxes can LIVE together, not be born
		// together — cap birth concurrency at ~core count.
		sem := make(chan struct{}, 4)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				id := fmt.Sprintf("s%d", len(live)+i)
				if _, err := agent.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
					SandboxID: id, TemplateID: "stress", VCPUs: 1, MemoryMiB: 256, DataDiskGiB: 1,
				}); err != nil {
					errs <- fmt.Errorf("%s: %w", id, err)
					return
				}
				ids <- id
			}(i)
		}
		wg.Wait()
		close(errs)
		close(ids)
		for id := range ids {
			live = append(live, id)
		}
		if err := <-errs; err != nil {
			stopReason = fmt.Sprintf("create failed at %d: %v", len(live), err)
			t.Logf("%s", stopReason)
			break
		}
		// Every guest in the batch answers an exec — RUNNING is not enough.
		ok := 0
		for _, id := range live[len(live)-n:] {
			if ex, err := agent.Exec(ctx, id, &guestapi.ExecRequest{Cmd: "echo", Args: []string{"ok"}}); err == nil && ex.ExitCode == 0 {
				ok++
			}
		}
		t.Logf("ramp: %3d live (batch of %d in %6.1fs, %d/%d exec-ok, MemAvailable %dMiB, disk %.1fGiB)",
			len(live), n, time.Since(start).Seconds(), ok, n, memAvailableMiB(t), diskFreeGiB(os.TempDir()))
		if ok < n {
			stopReason = fmt.Sprintf("exec verification dropped to %d/%d at %d live", ok, n, len(live))
			break
		}
	}

	// The whole fleet must still be alive at the end, not just the last batch.
	alive := 0
	for _, id := range live {
		if ex, err := agent.Exec(ctx, id, &guestapi.ExecRequest{Cmd: "echo", Args: []string{"ok"}}); err == nil && ex.ExitCode == 0 {
			alive++
		}
	}
	t.Logf("LIMIT: %d concurrent sandboxes (%d/%d alive on final sweep) — stop: %s", len(live), alive, len(live), stopReason)
}

// TestStressForkFanout checkpoints one parent and forks as many parallel
// branches as the host allows, reporting the latency distribution.
func TestStressForkFanout(t *testing.T) {
	if os.Getenv("EMBERVM_STRESS") != "1" {
		t.Skip("set EMBERVM_STRESS=1 for manual stress runs")
	}
	forks := stressEnv("EMBERVM_STRESS_FORKS", 30)
	agent := m5Agent(t, "stressf", forks+4)
	ctx := context.Background()

	if _, err := agent.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
		SandboxID: "fparent", TemplateID: "tmpl-stressf", MemoryMiB: 256, DataDiskGiB: 1,
	}); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	defer func() { _ = agent.StopSandbox(ctx, "fparent") }()
	if err := agent.WriteFile(ctx, "fparent", "/origin", 0o644, []byte("stress")); err != nil {
		t.Fatal(err)
	}
	layer := checkpoint(t, ctx, agent, "fparent", "fan")

	type res struct {
		id  string
		dur time.Duration
		err error
	}
	results := make(chan res, forks)
	var wg sync.WaitGroup
	wallStart := time.Now()
	for i := 0; i < forks; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("fk%d", i)
			s := time.Now()
			_, err := agent.Fork(ctx, "fparent", layer, id)
			results <- res{id, time.Since(s), err}
		}(i)
	}
	wg.Wait()
	close(results)
	wall := time.Since(wallStart)

	var durs []time.Duration
	var kids []string
	failed := 0
	for r := range results {
		if r.err != nil {
			failed++
			t.Logf("fork %s failed: %v", r.id, r.err)
			continue
		}
		kids = append(kids, r.id)
		durs = append(durs, r.dur)
	}
	t.Cleanup(func() {
		for _, id := range kids {
			_ = agent.StopSandbox(context.Background(), id)
		}
	})
	verified := 0
	for _, id := range kids {
		if ex, err := agent.Exec(ctx, id, &guestapi.ExecRequest{Cmd: "cat", Args: []string{"/origin"}}); err == nil && string(ex.Stdout) == "stress" {
			verified++
		}
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	t.Logf("FORK FANOUT: %d requested, %d ok (%d failed), %d exec+state verified; wall %.1fs; per-fork P50 %v P95 %v max %v; MemAvailable %dMiB",
		forks, len(kids), failed, verified, wall.Seconds(),
		percentile(durs, 0.50), percentile(durs, 0.95), durs[len(durs)-1], memAvailableMiB(t))
	if verified == 0 {
		t.Fatal("no fork survived verification")
	}
}

// TestStressResumeLatency runs pause/resume cycles on one sandbox and
// reports the hot-resume distribution for this host.
func TestStressResumeLatency(t *testing.T) {
	if os.Getenv("EMBERVM_STRESS") != "1" {
		t.Skip("set EMBERVM_STRESS=1 for manual stress runs")
	}
	cycles := stressEnv("EMBERVM_STRESS_CYCLES", 20)
	agent := m5Agent(t, "stressr", 2) // chunked+jailed+ZFS: the production path
	ctx := context.Background()

	if _, err := agent.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
		SandboxID: "r1", TemplateID: "tmpl-stressr", MemoryMiB: 256, DataDiskGiB: 1,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = agent.StopSandbox(ctx, "r1") }()

	var pauses, resumes []time.Duration
	for i := 0; i < cycles; i++ {
		s := time.Now()
		if err := agent.PauseSandbox(ctx, "r1"); err != nil {
			t.Fatalf("pause %d: %v", i, err)
		}
		pauses = append(pauses, time.Since(s))
		s = time.Now()
		if _, err := agent.ResumeSandbox(ctx, "r1"); err != nil {
			t.Fatalf("resume %d: %v", i, err)
		}
		resumes = append(resumes, time.Since(s))
	}
	sort.Slice(pauses, func(i, j int) bool { return pauses[i] < pauses[j] })
	sort.Slice(resumes, func(i, j int) bool { return resumes[i] < resumes[j] })
	t.Logf("RESUME LATENCY over %d cycles (chunked+jailed): resume P50 %v P95 %v max %v; pause P50 %v P95 %v",
		cycles, percentile(resumes, 0.50), percentile(resumes, 0.95), resumes[len(resumes)-1],
		percentile(pauses, 0.50), percentile(pauses, 0.95))
}
