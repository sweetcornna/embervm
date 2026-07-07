// Package uffd implements the userspace side of Firecracker's
// snapshot-restore page-fault handling: it receives the guest memory layout
// and a userfaultfd from Firecracker over a unix domain socket, then serves
// missing pages from the snapshot memory file (lazily on fault, optionally
// with a sequential background prefetch).
//
// Wire protocol: after `PUT /snapshot/load` with
// `mem_backend.backend_type == "Uffd"`, Firecracker connects to the handler's
// listening socket and sends — in a single message — a JSON array of
// GuestRegionUffdMapping plus the userfaultfd as SCM_RIGHTS ancillary data.
// This package is an independent Go implementation informed by Firecracker's
// example handlers (Apache-2.0, src/firecracker/examples/uffd/); see
// THIRD_PARTY_NOTICES.md.
package uffd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

// GuestRegionUffdMapping describes one guest memory region as announced by
// Firecracker during the uffd handshake. Field names follow the wire format
// of Firecracker v1.16 (`page_size`); the pre-v1.10 alias `page_size_kib`
// (which, despite its name, always carried bytes) is accepted for
// compatibility with the N-1 CI matrix.
type GuestRegionUffdMapping struct {
	BaseHostVirtAddr uint64 `json:"base_host_virt_addr"`
	Size             uint64 `json:"size"`
	Offset           uint64 `json:"offset"`
	PageSize         uint64 `json:"page_size"`
}

func (m *GuestRegionUffdMapping) UnmarshalJSON(b []byte) error {
	var wire struct {
		BaseHostVirtAddr uint64 `json:"base_host_virt_addr"`
		Size             uint64 `json:"size"`
		Offset           uint64 `json:"offset"`
		PageSize         uint64 `json:"page_size"`
		PageSizeKiB      uint64 `json:"page_size_kib"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	m.BaseHostVirtAddr = wire.BaseHostVirtAddr
	m.Size = wire.Size
	m.Offset = wire.Offset
	m.PageSize = wire.PageSize
	if m.PageSize == 0 {
		m.PageSize = wire.PageSizeKiB
	}
	if m.PageSize == 0 {
		m.PageSize = 4096
	}
	return nil
}

// End returns the exclusive upper bound of the region in host virtual
// address space.
func (m *GuestRegionUffdMapping) End() uint64 {
	return m.BaseHostVirtAddr + m.Size
}

func (m *GuestRegionUffdMapping) validate() error {
	if m.Size == 0 {
		return errors.New("uffd: mapping with zero size")
	}
	if m.PageSize == 0 || m.PageSize&(m.PageSize-1) != 0 {
		return fmt.Errorf("uffd: page size %d is not a power of two", m.PageSize)
	}
	if m.BaseHostVirtAddr%m.PageSize != 0 {
		return fmt.Errorf("uffd: base address %#x not aligned to page size %d", m.BaseHostVirtAddr, m.PageSize)
	}
	return nil
}

// Handshake accepts a single connection on l and receives the region
// mappings plus the userfaultfd. It returns the parsed mappings, the
// received file descriptor and the accepted connection. The caller must
// keep the connection open: Firecracker holds the peer end for the lifetime
// of the VM, so EOF on it signals that the VM is gone.
func Handshake(l *net.UnixListener, timeout time.Duration) ([]GuestRegionUffdMapping, int, *net.UnixConn, error) {
	deadline := time.Now().Add(timeout)
	if err := l.SetDeadline(deadline); err != nil {
		return nil, -1, nil, err
	}
	conn, err := l.AcceptUnix()
	if err != nil {
		return nil, -1, nil, fmt.Errorf("uffd: accept: %w", err)
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		conn.Close()
		return nil, -1, nil, err
	}

	// Firecracker sends everything in one sendmsg, but be tolerant of
	// fragmented delivery: keep reading until the JSON payload parses and a
	// file descriptor has arrived.
	buf := make([]byte, 1<<20)
	oob := make([]byte, 4096)
	var (
		data     []byte
		fd       = -1
		mappings []GuestRegionUffdMapping
	)
	for {
		n, oobn, _, _, rerr := conn.ReadMsgUnix(buf, oob)
		if n > 0 {
			data = append(data, buf[:n]...)
		}
		if oobn > 0 && fd < 0 {
			fd = parseSCMRightsFd(oob[:oobn])
		}
		if fd >= 0 && len(data) > 0 && json.Valid(data) {
			if err := json.Unmarshal(data, &mappings); err == nil {
				break
			}
		}
		if rerr != nil {
			conn.Close()
			if fd >= 0 {
				unix.Close(fd)
			}
			return nil, -1, nil, fmt.Errorf("uffd: handshake read (got %d bytes, fd=%v): %w", len(data), fd >= 0, rerr)
		}
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		conn.Close()
		unix.Close(fd)
		return nil, -1, nil, err
	}
	if len(mappings) == 0 {
		conn.Close()
		unix.Close(fd)
		return nil, -1, nil, errors.New("uffd: handshake carried no memory regions")
	}
	for i := range mappings {
		if err := mappings[i].validate(); err != nil {
			conn.Close()
			unix.Close(fd)
			return nil, -1, nil, err
		}
	}
	return mappings, fd, conn, nil
}

func parseSCMRightsFd(oob []byte) int {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return -1
	}
	for i := range msgs {
		fds, err := unix.ParseUnixRights(&msgs[i])
		if err == nil && len(fds) > 0 {
			// Only the first fd is meaningful; close any extras.
			for _, extra := range fds[1:] {
				unix.Close(extra)
			}
			return fds[0]
		}
	}
	return -1
}
