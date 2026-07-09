//go:build linux

package template

import (
	"archive/tar"
	"os"

	"golang.org/x/sys/unix"
)

// lchownAsRoot applies tar ownership when running as root; unprivileged
// extraction (unit tests) leaves files owned by the caller.
func lchownAsRoot(target string, uid, gid int) error {
	if os.Geteuid() != 0 {
		return nil
	}
	return os.Lchown(target, uid, gid)
}

// makeDevice materializes FIFO entries (root only, matching ownership
// handling). Char/block device entries are deliberately NOT created:
// extraction runs as root on the HOST filesystem, so a malicious image
// entry ("c 1 1" → /dev/mem, or a block node aliasing a host disk) would
// put a real, openable device node in the staging tree. The guest never
// needs them — /dev is a devtmpfs mounted by guestd at boot.
func makeDevice(target string, hdr *tar.Header) error {
	if hdr.Typeflag != tar.TypeFifo {
		return nil
	}
	if os.Geteuid() != 0 {
		return nil
	}
	if err := removeIfExists(target); err != nil {
		return err
	}
	return unix.Mknod(target, unix.S_IFIFO|uint32(hdr.Mode&0o7777), 0)
}
