package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeTemplateSrc(t *testing.T, content string) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		t.Fatalf("write template src: %v", err)
	}
	return src
}

func TestPlainRoundtrip(t *testing.T) {
	ctx := context.Background()
	be := NewPlainBackend(t.TempDir())
	src := writeTemplateSrc(t, "rootfs-image-bytes")

	if err := be.EnsureTemplate(ctx, "tpl1", src); err != nil {
		t.Fatalf("EnsureTemplate: %v", err)
	}
	// Idempotent.
	if err := be.EnsureTemplate(ctx, "tpl1", src); err != nil {
		t.Fatalf("EnsureTemplate (second): %v", err)
	}

	paths, err := be.CloneSandbox(ctx, "sbx1", "tpl1", 15)
	if err != nil {
		t.Fatalf("CloneSandbox: %v", err)
	}

	// Rootfs clone carries the template bytes.
	got, err := os.ReadFile(paths.RootfsExt4)
	if err != nil {
		t.Fatalf("read cloned rootfs: %v", err)
	}
	if string(got) != "rootfs-image-bytes" {
		t.Errorf("cloned rootfs = %q, want template bytes", got)
	}

	// data.raw is a 15GiB sparse file.
	fi, err := os.Stat(paths.DataRaw)
	if err != nil {
		t.Fatalf("stat data.raw: %v", err)
	}
	if fi.Size() != 15<<30 {
		t.Errorf("data.raw size = %d, want %d", fi.Size(), int64(15)<<30)
	}
	if kib := onDiskKiB(t, paths.DataRaw); kib >= 0 && kib > 1024 {
		t.Errorf("data.raw occupies %d KiB on disk, want ~0 (sparse)", kib)
	}

	// Paths is pure.
	if be.Paths("sbx1") != paths {
		t.Errorf("Paths(sbx1) = %+v, want %+v", be.Paths("sbx1"), paths)
	}

	snap, err := be.Snapshot(ctx, "sbx1", "p1")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap == "" {
		t.Error("Snapshot returned empty id")
	}

	if err := be.DestroySandbox(ctx, "sbx1"); err != nil {
		t.Fatalf("DestroySandbox: %v", err)
	}
	if _, err := os.Stat(paths.Dir); !os.IsNotExist(err) {
		t.Errorf("sandbox dir still present after destroy: %v", err)
	}
	// Idempotent destroy.
	if err := be.DestroySandbox(ctx, "sbx1"); err != nil {
		t.Errorf("second DestroySandbox: %v", err)
	}
}

func TestPlainErrors(t *testing.T) {
	ctx := context.Background()
	be := NewPlainBackend(t.TempDir())

	if _, err := be.CloneSandbox(ctx, "sbx", "missing-tpl", 1); err == nil {
		t.Error("CloneSandbox with unknown template: want error")
	}
	if err := be.EnsureTemplate(ctx, "bad id!", writeTemplateSrc(t, "x")); err == nil {
		t.Error("EnsureTemplate with invalid id: want error")
	}
	if _, err := be.CloneSandbox(ctx, "../escape", "tpl", 1); err == nil {
		t.Error("CloneSandbox with invalid id: want error")
	}
}

// onDiskKiB returns the actual allocated size of path in KiB.
func onDiskKiB(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return blocks512(fi) / 2
}
