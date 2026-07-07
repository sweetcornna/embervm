//go:build linux

package template

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestBuildFromRegistry pulls a real image (network) and builds a template.
// Gated behind EMBERVM_NET_TESTS=1; CI runs it in lint-unit (no KVM needed).
func TestBuildFromRegistry(t *testing.T) {
	if os.Getenv("EMBERVM_NET_TESTS") != "1" {
		t.Skip("set EMBERVM_NET_TESTS=1 to run registry pull tests")
	}
	for _, tool := range []string{"mkfs.ext4", "debugfs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	out := filepath.Join(t.TempDir(), "alpine.ext4")
	res, err := Build(ctx, BuildInput{
		Image:      "alpine:3.20",
		GuestdPath: writeFakeGuestd(t),
		OutPath:    out,
	})
	if err != nil {
		t.Fatalf("Build(alpine:3.20): %v", err)
	}
	if len(res.Config.Cmd) == 0 {
		t.Errorf("alpine config Cmd empty, want /bin/sh: %+v", res.Config)
	}

	for _, path := range []string{"/usr/local/bin/guestd", "/etc/embervm/image.json", "/bin/busybox", "/etc/alpine-release"} {
		if out, err := exec.Command("debugfs", "-R", "stat "+path, out).CombinedOutput(); err != nil {
			t.Errorf("debugfs stat %s: %v: %s", path, err, out)
		}
	}
}
