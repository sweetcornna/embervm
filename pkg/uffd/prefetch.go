//go:build linux

package uffd

import (
	"time"

	"golang.org/x/sys/unix"
)

// prefetch streams every announced region into the guest address space with
// large sequential UFFDIO_COPY calls while the fault loop keeps serving
// demand faults concurrently. Pages the faulter got to first surface as
// EEXIST, in which case we fall back to per-page copies across that block.
//
// This is the FaaSnap-style "concurrent paging" M0 baseline; M2 replaces the
// blind sequential order with the recorded working-set order (REAP).
func (h *Handler) prefetch() {
	start := time.Now()
	block := h.cfg.PrefetchBlock
	for i := range h.mappings {
		m := &h.mappings[i]
		// Round the block down to this region's page size.
		step := block &^ (m.PageSize - 1)
		if step == 0 {
			step = m.PageSize
		}
		for off := uint64(0); off < m.Size; off += step {
			if h.stopped() {
				return
			}
			n := step
			if off+n > m.Size {
				n = m.Size - off
			}
			dst := m.BaseHostVirtAddr + off
			if h.removed.intersects(dst, dst+n) {
				h.prefetchPagewise(m, off, n)
				continue
			}
			if err := h.prefetchBlock(m, off, n); err != nil {
				h.logf("prefetch: block dst=%#x len=%d: %v; falling back to per-page", dst, n, err)
				h.prefetchPagewise(m, off, n)
			}
		}
	}
	h.logf("prefetch finished in %s (%d bytes)", time.Since(start), h.stats.BytesCopiedPrefetch.Load())
}

func (h *Handler) prefetchBlock(m *GuestRegionUffdMapping, off, length uint64) error {
	dst := m.BaseHostVirtAddr + off
	src := m.Offset + off
	for retries := 0; ; retries++ {
		copied, err := ioctlCopy(h.uffd, dst, uint64(sliceAddr(h.backing, src)), length)
		switch {
		case err == nil:
			h.stats.BytesCopiedPrefetch.Add(length)
			return nil
		case err == unix.EAGAIN && retries < 64:
			// On EAGAIN the kernel reports partial progress in `copy`.
			if copied > 0 {
				h.stats.BytesCopiedPrefetch.Add(uint64(copied))
				dst += uint64(copied)
				src += uint64(copied)
				length -= uint64(copied)
				if length == 0 {
					return nil
				}
			}
			continue
		default:
			return err
		}
	}
}

// prefetchPagewise copies one page at a time, silently skipping pages that
// are already populated or have been discarded by the guest.
func (h *Handler) prefetchPagewise(m *GuestRegionUffdMapping, off, length uint64) {
	for p := off; p < off+length; p += m.PageSize {
		if h.stopped() {
			return
		}
		dst := m.BaseHostVirtAddr + p
		if h.removed.contains(dst) {
			continue
		}
		for retries := 0; ; retries++ {
			_, err := ioctlCopy(h.uffd, dst, uint64(sliceAddr(h.backing, m.Offset+p)), m.PageSize)
			if err == nil {
				h.stats.BytesCopiedPrefetch.Add(m.PageSize)
				break
			}
			if err == unix.EEXIST {
				break
			}
			if err == unix.EAGAIN && retries < 64 {
				continue
			}
			h.logf("prefetch: page dst=%#x: %v (skipping)", dst, err)
			break
		}
	}
}
