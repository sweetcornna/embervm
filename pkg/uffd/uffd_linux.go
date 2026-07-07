//go:build linux

package uffd

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// userfaultfd ioctl request codes (identical on x86_64 and aarch64) and
// event codes from <linux/userfaultfd.h>.
const (
	uffdioCopy     = 0xc028aa03 // _IOWR(UFFDIO, 0x03, struct uffdio_copy)
	uffdioZeropage = 0xc020aa04 // _IOWR(UFFDIO, 0x04, struct uffdio_zeropage)

	uffdEventPagefault = 0x12
	uffdEventFork      = 0x13
	uffdEventRemap     = 0x14
	uffdEventRemove    = 0x15
	uffdEventUnmap     = 0x16

	// sizeof(struct uffd_msg): 8-byte packed header + 24-byte union.
	uffdMsgSize = 32
)

type uffdioCopyArg struct {
	Dst  uint64
	Src  uint64
	Len  uint64
	Mode uint64
	Copy int64
}

type uffdioRange struct {
	Start uint64
	Len   uint64
}

type uffdioZeropageArg struct {
	Range    uffdioRange
	Mode     uint64
	Zeropage int64
}

// ioctlCopy populates [dst, dst+length) in the registered address space from
// src (a pointer into this process's memory). Returns bytes copied so far
// (meaningful on EAGAIN) and the errno, if any.
func ioctlCopy(uffd int, dst, src, length uint64) (int64, error) {
	arg := uffdioCopyArg{Dst: dst, Src: src, Len: length}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(uffd), uffdioCopy, uintptr(unsafe.Pointer(&arg)))
	if errno != 0 {
		return arg.Copy, errno
	}
	return arg.Copy, nil
}

// ioctlZeropage maps zero pages over [start, start+length).
func ioctlZeropage(uffd int, start, length uint64) (int64, error) {
	arg := uffdioZeropageArg{Range: uffdioRange{Start: start, Len: length}}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(uffd), uffdioZeropage, uintptr(unsafe.Pointer(&arg)))
	if errno != 0 {
		return arg.Zeropage, errno
	}
	return arg.Zeropage, nil
}

// sliceAddr returns the address of b[off] for use as a UFFDIO_COPY source.
func sliceAddr(b []byte, off uint64) uintptr {
	return uintptr(unsafe.Pointer(&b[off]))
}
