//go:build linux

package template

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
)

// mkext4 builds img from the staging tree via mkfs.ext4 -d (no mount, no
// root needed), atomically (tmp + rename, matching scripts/build-rootfs.sh).
// sizeMB 0 → max(2×tree, tree+512MiB), rounded up to 64MiB.
func mkext4(img, stagingRoot string, sizeMB int) error {
	if sizeMB <= 0 {
		treeMB, err := treeSizeMB(stagingRoot)
		if err != nil {
			return fmt.Errorf("size staging tree: %w", err)
		}
		sizeMB = max(2*treeMB, treeMB+512)
		sizeMB = (sizeMB + 63) / 64 * 64
	}

	if err := os.MkdirAll(filepath.Dir(img), 0o755); err != nil {
		return err
	}
	tmp := img + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := f.Truncate(int64(sizeMB) << 20); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("truncate %s to %dMiB: %w", tmp, sizeMB, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}

	cmd := exec.Command("mkfs.ext4", "-F", "-q", "-d", stagingRoot, tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("mkfs.ext4: %w: %s", err, out)
	}
	return os.Rename(tmp, img)
}

// treeSizeMB sums apparent file sizes under root, in MiB rounded up.
func treeSizeMB(root string) (int, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			fi, err := d.Info()
			if err != nil {
				return err
			}
			total += fi.Size()
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return int((total + (1 << 20) - 1) >> 20), nil
}
