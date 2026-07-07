package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// PlainBackend provisions sandbox storage with ordinary filesystem
// operations: it copies rootfs images and creates sparse data disks. It has
// no copy-on-write, so template rootfs bytes are duplicated per sandbox —
// fine for `embervm dev` and tests where correctness, not density, matters.
type PlainBackend struct {
	root string
}

// NewPlainBackend roots all storage under dir.
func NewPlainBackend(root string) *PlainBackend {
	return &PlainBackend{root: root}
}

func (b *PlainBackend) templateRootfs(id string) string {
	return filepath.Join(b.root, "templates", id, "rootfs.ext4")
}

func (b *PlainBackend) sandboxDir(id string) string {
	return filepath.Join(b.root, "sandboxes", id)
}

// Paths implements Backend.
func (b *PlainBackend) Paths(sandboxID string) SandboxPaths {
	dir := b.sandboxDir(sandboxID)
	return SandboxPaths{
		Dir:        dir,
		RootfsExt4: filepath.Join(dir, "rootfs.ext4"),
		DataRaw:    filepath.Join(dir, "data.raw"),
	}
}

// EnsureTemplate implements Backend.
func (b *PlainBackend) EnsureTemplate(_ context.Context, templateID, rootfsExt4Src string) error {
	if err := validateID("template", templateID); err != nil {
		return err
	}
	dst := b.templateRootfs(templateID)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return copyFile(dst, rootfsExt4Src, 0o644)
}

// CloneSandbox implements Backend.
func (b *PlainBackend) CloneSandbox(_ context.Context, sandboxID, templateID string, dataDiskGiB int) (SandboxPaths, error) {
	if err := validateID("sandbox", sandboxID); err != nil {
		return SandboxPaths{}, err
	}
	if err := validateID("template", templateID); err != nil {
		return SandboxPaths{}, err
	}
	src := b.templateRootfs(templateID)
	if _, err := os.Stat(src); err != nil {
		return SandboxPaths{}, fmt.Errorf("template %q not found: %w", templateID, err)
	}
	paths := b.Paths(sandboxID)
	if err := os.MkdirAll(paths.Dir, 0o755); err != nil {
		return SandboxPaths{}, err
	}
	if err := copyFile(paths.RootfsExt4, src, 0o644); err != nil {
		return SandboxPaths{}, fmt.Errorf("clone rootfs: %w", err)
	}
	if err := createSparse(paths.DataRaw, int64(dataDiskGiB)<<30); err != nil {
		return SandboxPaths{}, fmt.Errorf("create data disk: %w", err)
	}
	return paths, nil
}

// Snapshot implements Backend by copying the sandbox files under .snap/<tag>.
func (b *PlainBackend) Snapshot(_ context.Context, sandboxID, tag string) (string, error) {
	if err := validateID("sandbox", sandboxID); err != nil {
		return "", err
	}
	if err := validateID("tag", tag); err != nil {
		return "", err
	}
	dir := b.sandboxDir(sandboxID)
	snapDir := filepath.Join(dir, ".snap", tag)
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		return "", err
	}
	// Copy the writable rootfs so the marker is a real point-in-time copy;
	// data.raw is deliberately left out (large, and M1 does not roll it back).
	if err := copyFile(filepath.Join(snapDir, "rootfs.ext4"), filepath.Join(dir, "rootfs.ext4"), 0o644); err != nil {
		return "", err
	}
	return snapDir, nil
}

// DestroySandbox implements Backend.
func (b *PlainBackend) DestroySandbox(_ context.Context, sandboxID string) error {
	if err := validateID("sandbox", sandboxID); err != nil {
		return err
	}
	err := os.RemoveAll(b.sandboxDir(sandboxID))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// copyFile copies src to dst, creating/truncating dst with mode.
func copyFile(dst, src string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

// createSparse makes a sparse file of exactly size bytes (no blocks
// allocated until written).
func createSparse(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
