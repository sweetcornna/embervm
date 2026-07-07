//go:build !linux

package template

import "errors"

// mkfs.ext4 -d only behaves correctly on Linux (mirrors the guard in
// scripts/build-rootfs.sh); development hosts stop at the staging tree.
func mkext4(img, stagingRoot string, sizeMB int) error {
	return errors.New("ext4 image assembly is linux-only")
}
