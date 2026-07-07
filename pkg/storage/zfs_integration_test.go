//go:build linux

package storage

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestZFSIntegration exercises the real zfs CLI against a throwaway
// loop-file pool. It is gated behind EMBERVM_ZFS_TESTS=1 and needs root +
// zfs userland; missing prerequisites SKIP (non-blocking) rather than fail,
// matching the M0 ZFS policy (test/bench/zfs-compare.sh).
func TestZFSIntegration(t *testing.T) {
	if os.Getenv("EMBERVM_ZFS_TESTS") != "1" {
		t.Skip("set EMBERVM_ZFS_TESTS=1 to run the real-ZFS integration test")
	}
	if os.Geteuid() != 0 {
		t.Skip("real-ZFS integration test needs root")
	}
	for _, tool := range []string{"zpool", "zfs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s userland not installed", tool)
		}
	}

	ctx := context.Background()
	pool := "embervm-it"
	img := filepath.Join(t.TempDir(), "pool.img")

	// A 1 GiB loop file is plenty; data.raw is sparse.
	if err := os.Truncate(mustCreate(t, img), 1<<30); err != nil {
		t.Fatalf("size pool image: %v", err)
	}
	mnt := t.TempDir()
	if out, err := exec.CommandContext(ctx, "zpool", "create", "-f",
		"-m", mnt, pool, img).CombinedOutput(); err != nil {
		t.Skipf("zpool create failed (kernel module?): %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("zpool", "destroy", "-f", pool).Run() })

	// Point the backend at this pool; override Paths' /<pool> assumption by
	// using the mountpoint the CLI reports (the backend reads it back).
	be := &ZFSBackend{pool: pool, run: execRun}

	src := writeTemplateSrc(t, "integration-rootfs-bytes")
	if err := be.EnsureTemplate(ctx, "tpl1", src); err != nil {
		t.Fatalf("EnsureTemplate: %v", err)
	}
	// Idempotent second call.
	if err := be.EnsureTemplate(ctx, "tpl1", src); err != nil {
		t.Fatalf("EnsureTemplate (idempotent): %v", err)
	}

	paths, err := be.CloneSandbox(ctx, "sbx1", "tpl1", 15)
	if err != nil {
		t.Fatalf("CloneSandbox: %v", err)
	}
	got, err := os.ReadFile(paths.RootfsExt4)
	if err != nil || string(got) != "integration-rootfs-bytes" {
		t.Fatalf("cloned rootfs = %q, err=%v", got, err)
	}
	fi, err := os.Stat(paths.DataRaw)
	if err != nil || fi.Size() != 15<<30 {
		t.Fatalf("data.raw size = %v (err=%v), want 15GiB", fi, err)
	}
	if kib := blocks512(fi) / 2; kib > 4096 {
		t.Errorf("15GiB data.raw allocated %d KiB, want sparse (~0)", kib)
	}

	snap, err := be.Snapshot(ctx, "sbx1", "p1")
	if err != nil || !strings.HasSuffix(snap, "@p1") {
		t.Fatalf("Snapshot = %q, err=%v", snap, err)
	}

	if err := be.DestroySandbox(ctx, "sbx1"); err != nil {
		t.Fatalf("DestroySandbox: %v", err)
	}
	if err := be.DestroySandbox(ctx, "sbx1"); err != nil {
		t.Errorf("idempotent DestroySandbox: %v", err)
	}
}

func mustCreate(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	f.Close()
	return path
}
