package storage

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// runner runs a command and returns its trimmed stdout. It is injectable so
// ZFSBackend's command construction is unit-testable without real ZFS.
type runner func(ctx context.Context, name string, args ...string) (string, error)

func execRun(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// ZFSBackend provisions sandbox storage as ZFS datasets: templates are
// snapshotted (@final) and cloned per sandbox for O(1) copy-on-write, with
// the data disk a sparse raw file on the clone (never a zvol, docs/zh/04 §1).
type ZFSBackend struct {
	pool string
	run  runner
}

// NewZFSBackend provisions datasets under the given pool.
func NewZFSBackend(pool string) *ZFSBackend {
	return &ZFSBackend{pool: pool, run: execRun}
}

func (b *ZFSBackend) templateDS(id string) string { return b.pool + "/templates/" + id }
func (b *ZFSBackend) sandboxDS(id string) string  { return b.pool + "/sandboxes/" + id }

// mountpoint queries a dataset's mountpoint.
func (b *ZFSBackend) mountpoint(ctx context.Context, ds string) (string, error) {
	mp, err := b.run(ctx, "zfs", "get", "-H", "-o", "value", "mountpoint", ds)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(mp), nil
}

func (b *ZFSBackend) datasetExists(ctx context.Context, ds string) bool {
	_, err := b.run(ctx, "zfs", "list", "-H", "-o", "name", ds)
	return err == nil
}

// Paths implements Backend. It derives paths from the pool's conventional
// mountpoint (<pool> mounts at /<pool> by default) to stay pure (no exec).
func (b *ZFSBackend) Paths(sandboxID string) SandboxPaths {
	dir := filepath.Join("/"+b.pool, "sandboxes", sandboxID)
	return SandboxPaths{
		Dir:        dir,
		RootfsExt4: filepath.Join(dir, "rootfs.ext4"),
		DataRaw:    filepath.Join(dir, "data.raw"),
	}
}

// EnsureTemplate implements Backend.
func (b *ZFSBackend) EnsureTemplate(ctx context.Context, templateID, rootfsExt4Src string) error {
	if err := validateID("template", templateID); err != nil {
		return err
	}
	ds := b.templateDS(templateID)
	if b.datasetExists(ctx, ds+"@final") {
		return nil // already imported and cloneable
	}
	if !b.datasetExists(ctx, ds) {
		if _, err := b.run(ctx, "zfs", "create", "-p",
			"-o", "recordsize=16k",
			"-o", "primarycache=metadata",
			"-o", "compression=lz4",
			ds); err != nil {
			return err
		}
	}
	mp, err := b.mountpoint(ctx, ds)
	if err != nil {
		return err
	}
	if err := copyFile(filepath.Join(mp, "rootfs.ext4"), rootfsExt4Src, 0o644); err != nil {
		return fmt.Errorf("place template rootfs: %w", err)
	}
	if _, err := b.run(ctx, "zfs", "snapshot", ds+"@final"); err != nil {
		return err
	}
	return nil
}

// CloneSandbox implements Backend.
func (b *ZFSBackend) CloneSandbox(ctx context.Context, sandboxID, templateID string, dataDiskGiB int) (SandboxPaths, error) {
	if err := validateID("sandbox", sandboxID); err != nil {
		return SandboxPaths{}, err
	}
	if err := validateID("template", templateID); err != nil {
		return SandboxPaths{}, err
	}
	// zfs clone does not create parent datasets, so the sandboxes container
	// must exist first (create -p errors if the leaf already exists, hence
	// the existence probe).
	if parent := b.pool + "/sandboxes"; !b.datasetExists(ctx, parent) {
		if _, err := b.run(ctx, "zfs", "create", "-p", parent); err != nil {
			return SandboxPaths{}, err
		}
	}
	sds := b.sandboxDS(sandboxID)
	if _, err := b.run(ctx, "zfs", "clone", b.templateDS(templateID)+"@final", sds); err != nil {
		return SandboxPaths{}, err
	}
	mp, err := b.mountpoint(ctx, sds)
	if err != nil {
		return SandboxPaths{}, err
	}
	paths := SandboxPaths{
		Dir:        mp,
		RootfsExt4: filepath.Join(mp, "rootfs.ext4"),
		DataRaw:    filepath.Join(mp, "data.raw"),
	}
	if err := createSparse(paths.DataRaw, int64(dataDiskGiB)<<30); err != nil {
		return SandboxPaths{}, fmt.Errorf("create data disk: %w", err)
	}
	return paths, nil
}

// Snapshot implements Backend.
func (b *ZFSBackend) Snapshot(ctx context.Context, sandboxID, tag string) (string, error) {
	if err := validateID("sandbox", sandboxID); err != nil {
		return "", err
	}
	if err := validateID("tag", tag); err != nil {
		return "", err
	}
	snap := b.sandboxDS(sandboxID) + "@" + tag
	if _, err := b.run(ctx, "zfs", "snapshot", snap); err != nil {
		return "", err
	}
	return snap, nil
}

// DestroySandbox implements Backend. It is idempotent: a missing dataset is
// treated as success.
func (b *ZFSBackend) DestroySandbox(ctx context.Context, sandboxID string) error {
	if err := validateID("sandbox", sandboxID); err != nil {
		return err
	}
	out, err := b.run(ctx, "zfs", "destroy", "-r", b.sandboxDS(sandboxID))
	if err != nil && !strings.Contains(out, "does not exist") {
		return err
	}
	return nil
}
