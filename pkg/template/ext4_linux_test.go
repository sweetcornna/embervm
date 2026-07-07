//go:build linux

package template

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestMkext4(t *testing.T) {
	for _, tool := range []string{"mkfs.ext4", "debugfs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}

	staging := t.TempDir()
	if err := os.MkdirAll(filepath.Join(staging, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "etc", "hostname"), []byte("ember\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	img := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := mkext4(img, staging, 64); err != nil {
		t.Fatalf("mkext4: %v", err)
	}

	fi, err := os.Stat(img)
	if err != nil {
		t.Fatalf("stat image: %v", err)
	}
	if fi.Size() != 64<<20 {
		t.Errorf("image size = %d, want %d", fi.Size(), 64<<20)
	}

	// debugfs stats the file inside the image without mounting (works
	// unprivileged).
	out, err := exec.Command("debugfs", "-R", "stat /etc/hostname", img).CombinedOutput()
	if err != nil {
		t.Fatalf("debugfs stat /etc/hostname: %v: %s", err, out)
	}
}

func TestMkext4AutoSize(t *testing.T) {
	for _, tool := range []string{"mkfs.ext4"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	staging := t.TempDir()
	if err := os.WriteFile(filepath.Join(staging, "f"), make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(t.TempDir(), "auto.ext4")
	if err := mkext4(img, staging, 0); err != nil {
		t.Fatalf("mkext4 auto-size: %v", err)
	}
	fi, err := os.Stat(img)
	if err != nil {
		t.Fatal(err)
	}
	// 1MiB tree → max(2, 513) → rounded to 576MiB.
	if fi.Size() != 576<<20 {
		t.Errorf("auto size = %dMiB, want 576MiB", fi.Size()>>20)
	}
}
