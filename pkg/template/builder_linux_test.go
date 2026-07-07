//go:build linux

package template

import (
	"archive/tar"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestBuildFromTar runs the whole offline pipeline: tar → staging →
// injection → ext4, then debugfs-verifies guestd landed inside the image.
func TestBuildFromTar(t *testing.T) {
	for _, tool := range []string{"mkfs.ext4", "debugfs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}

	fs := buildTar(t, []tarEntry{
		{name: "bin/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "bin/busybox", typeflag: tar.TypeReg, mode: 0o755, body: "#!fake"},
	})
	out := filepath.Join(t.TempDir(), "rootfs.ext4")
	res, err := Build(context.Background(), BuildInput{
		TarSource:  fs,
		GuestdPath: writeFakeGuestd(t),
		OutPath:    out,
		SizeMB:     64,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.RootfsBytes != 64<<20 {
		t.Errorf("RootfsBytes = %d, want %d", res.RootfsBytes, 64<<20)
	}

	for _, path := range []string{"/usr/local/bin/guestd", "/etc/embervm/image.json", "/bin/busybox"} {
		if out, err := exec.Command("debugfs", "-R", "stat "+path, out).CombinedOutput(); err != nil {
			t.Errorf("debugfs stat %s: %v: %s", path, err, out)
		}
	}

	// Staging tree cleaned up.
	entries, err := os.ReadDir(filepath.Dir(out))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() && len(e.Name()) > 8 && e.Name()[:9] == ".staging-" {
			t.Errorf("staging dir %s left behind", e.Name())
		}
	}
}
