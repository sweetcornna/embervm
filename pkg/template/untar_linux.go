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

// makeDevice creates char/block/fifo nodes when running as root and skips
// them otherwise (docker-exported images rarely carry any; /dev is a
// devtmpfs mounted by guestd at boot).
func makeDevice(target string, hdr *tar.Header) error {
	if os.Geteuid() != 0 {
		return nil
	}
	mode := uint32(hdr.Mode & 0o7777)
	switch hdr.Typeflag {
	case tar.TypeChar:
		mode |= unix.S_IFCHR
	case tar.TypeBlock:
		mode |= unix.S_IFBLK
	case tar.TypeFifo:
		mode |= unix.S_IFIFO
	}
	if err := removeIfExists(target); err != nil {
		return err
	}
	dev := unix.Mkdev(uint32(hdr.Devmajor), uint32(hdr.Devminor))
	return unix.Mknod(target, mode, int(dev))
}
