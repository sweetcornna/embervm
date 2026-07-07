package template

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// BuildInput describes one template build. Exactly one of Image (registry
// pull) or TarSource (pre-flattened filesystem tar — tests, air-gapped
// hosts) must be set.
type BuildInput struct {
	Image      string
	TarSource  io.Reader
	GuestdPath string // host path to the static guestd binary to inject
	OutPath    string // destination rootfs.ext4
	SizeMB     int    // 0 → auto (max(2×tree, tree+512MiB), 64MiB-rounded)
}

// BuildResult reports what was built. Config is zero-valued for TarSource
// builds (no OCI config to read).
type BuildResult struct {
	Config      ImageConfig
	RootfsBytes int64
}

// Build turns a Docker/OCI image into a bootable EmberVM rootfs.ext4 with
// guestd installed as the guest's PID 1 (init=/usr/local/bin/guestd). The
// staging tree lives next to OutPath so large images don't land in /tmp.
func Build(ctx context.Context, in BuildInput) (*BuildResult, error) {
	if (in.Image == "") == (in.TarSource == nil) {
		return nil, errors.New("exactly one of Image or TarSource must be set")
	}
	if in.GuestdPath == "" || in.OutPath == "" {
		return nil, errors.New("GuestdPath and OutPath are required")
	}

	var (
		src io.Reader
		cfg ImageConfig
	)
	if in.Image != "" {
		rc, imgCfg, err := flattenImage(ctx, in.Image)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		src, cfg = rc, imgCfg
	} else {
		src = in.TarSource
	}

	if err := os.MkdirAll(filepath.Dir(in.OutPath), 0o755); err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp(filepath.Dir(in.OutPath), ".staging-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(staging)

	if err := Untar(staging, src); err != nil {
		return nil, fmt.Errorf("extract image filesystem: %w", err)
	}
	if err := injectRuntime(staging, in.GuestdPath, cfg); err != nil {
		return nil, err
	}
	if err := mkext4(in.OutPath, staging, in.SizeMB); err != nil {
		return nil, err
	}

	fi, err := os.Stat(in.OutPath)
	if err != nil {
		return nil, err
	}
	return &BuildResult{Config: cfg, RootfsBytes: fi.Size()}, nil
}
