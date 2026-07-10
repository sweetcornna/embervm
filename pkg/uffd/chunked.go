//go:build linux

package uffd

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/embervm/embervm/pkg/memsnap"
	"golang.org/x/sys/unix"
)

const chunkFetchRetries = 3

// serveChunkFault handles one page fault in chunked mode by populating the
// whole chunk containing the page (4 pages per fetch: fewer round trips to
// the store, and the fetch+decode cost is already paid).
func (h *Handler) serveChunkFault(addr uint64) error {
	m := h.regionFor(addr)
	if m == nil {
		return fmt.Errorf("uffd: page fault at %#x outside all announced regions", addr)
	}
	page := addr &^ (m.PageSize - 1)
	h.stats.FirstFaultUnixNs.CompareAndSwap(0, time.Now().UnixNano())

	if h.removed.contains(page) {
		if err := h.zeroPage(page, m.PageSize); err != nil {
			return err
		}
		h.stats.Zeropages.Add(1)
		return nil
	}

	ci := chunkIndexForFault(m, page, h.cfg.View.ChunkSize)
	if h.wsRec != nil {
		h.wsRec.touch(ci)
	}
	if err := h.populateChunk(ci, h.faultBuf, &h.stats.BytesCopiedFault); err != nil {
		return err
	}
	h.stats.ChunksServedFault.Add(1)
	return nil
}

// populateChunk fetches, decodes, and maps one chunk, accounting copied
// bytes to ctr (fault vs prefetch). Safe to call from the fault loop and the
// prefetch goroutine concurrently as long as each passes its own scratch
// buffer; overlapping populations resolve via EEXIST.
func (h *Handler) populateChunk(ci int, scratch []byte, ctr *atomic.Uint64) error {
	if ci < 0 || ci >= len(h.cfg.View.Chunks) {
		return fmt.Errorf("uffd: chunk index %d out of range", ci)
	}
	if h.populated[ci].Load() {
		return nil
	}
	ref := h.cfg.View.Chunks[ci]
	chunkOff := uint64(ci) * uint64(h.cfg.View.ChunkSize)
	pieces := chunkPieces(h.mappings, chunkOff, ref.ULen)
	if len(pieces) == 0 {
		h.populated[ci].Store(true)
		return nil // chunk lives entirely in an unannounced hole
	}

	if ref.Zero {
		for _, p := range pieces {
			if err := h.zeroRange(p.hostAddr, p.length); err != nil {
				return err
			}
		}
		h.populated[ci].Store(true)
		return nil
	}

	data, err := h.fetchChunk(ref, scratch)
	if err != nil {
		return err
	}
	for _, p := range pieces {
		if h.removed.intersects(p.hostAddr, p.hostAddr+p.length) {
			h.copyPiecePagewise(p, data)
			continue
		}
		if err := h.copyRange(p.hostAddr, data[p.dataOff:int(p.dataOff)+int(p.length)], ctr); err != nil {
			if err == unix.EEXIST {
				h.copyPiecePagewise(p, data)
				continue
			}
			return err
		}
	}
	// A balloon REMOVE that landed while we copied zapped part of this
	// range after (or before) our COPY — which pages hold snapshot bytes
	// and which are gone is unknowable now. Do NOT mark the chunk
	// populated: future faults must keep flowing to the removed-aware
	// path (zeros for zapped pages) instead of being silently skipped.
	for _, p := range pieces {
		if h.removed.intersects(p.hostAddr, p.hostAddr+p.length) {
			h.stats.RemoveRaces.Add(1)
			h.logf("chunked: REMOVE raced populate of chunk %d; left unpopulated", ci)
			return nil
		}
	}
	h.populated[ci].Store(true)
	return nil
}

// fetchChunk retrieves and decodes a chunk's bytes, retrying transient store
// failures. A permanent failure is fatal to the handler — a fault we cannot
// serve leaves the vCPU hung, and exiting loudly beats hanging silently.
// Content is trusted from the content-addressed store (no per-fetch hash
// verification on the fault path — lz4/length mismatches still surface);
// end-to-end integrity is asserted by the e2e restore-continuity gates.
func (h *Handler) fetchChunk(ref memsnap.ChunkRef, scratch []byte) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < chunkFetchRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
		}
		stored, err := h.cfg.Chunks.Get(ref.Hash)
		if err != nil {
			lastErr = err
			h.stats.ChunkFetchErrors.Add(1)
			continue
		}
		data, err := memsnap.Decode(ref, stored)
		if err != nil {
			lastErr = err
			h.stats.ChunkFetchErrors.Add(1)
			continue
		}
		copy(scratch, data)
		return scratch[:ref.ULen], nil
	}
	return nil, fmt.Errorf("uffd: chunk %d (%s) unavailable after %d attempts: %w",
		ref.Index, ref.Hash, chunkFetchRetries, lastErr)
}

// copyRange COPYs a whole span, accounting to ctr; the caller handles EEXIST.
func (h *Handler) copyRange(dst uint64, data []byte, ctr *atomic.Uint64) error {
	length := uint64(len(data))
	src := uint64(sliceAddr(data, 0))
	for retries := 0; ; retries++ {
		copied, err := ioctlCopy(h.uffd, dst, src, length)
		switch {
		case err == nil:
			ctr.Add(length)
			return nil
		case err == unix.EAGAIN && retries < 64:
			if copied > 0 {
				ctr.Add(uint64(copied))
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

// copyPiecePagewise populates a piece page by page, skipping populated and
// removed pages.
func (h *Handler) copyPiecePagewise(p chunkPiece, data []byte) {
	pageSize := uint64(4096)
	if m := h.regionFor(p.hostAddr); m != nil {
		pageSize = m.PageSize
	}
	for off := uint64(0); off < p.length; off += pageSize {
		dst := p.hostAddr + off
		if h.removed.contains(dst) {
			continue
		}
		n := pageSize
		if off+n > p.length {
			n = p.length - off
		}
		for retries := 0; ; retries++ {
			_, err := ioctlCopy(h.uffd, dst, uint64(sliceAddr(data, uint64(p.dataOff)+off)), n)
			if err == nil {
				h.stats.BytesCopiedFault.Add(n)
				break
			}
			if err == unix.EEXIST {
				break
			}
			if err == unix.EAGAIN && retries < 64 {
				continue
			}
			h.logf("chunked: page dst=%#x: %v (skipping)", dst, err)
			break
		}
	}
}

// zeroRunMax caps a single coalesced ZEROPAGE ioctl: demand faults must not
// stall behind one giant mmap-lock hold.
const zeroRunMax = 64 << 20

// populateZeroRun zero-fills chunks [from..to] with span-sized ZEROPAGE
// calls instead of per-chunk ones. Zeros are zeros even for pages a
// concurrent balloon/hotplug REMOVE zapped, so the remove-race discipline
// that keeps populateChunk from marking raced chunks does not apply here.
func (h *Handler) populateZeroRun(from, to int) error {
	cs := uint64(h.cfg.View.ChunkSize)
	runOff := uint64(from) * cs
	runLen := (to-from)*h.cfg.View.ChunkSize + h.cfg.View.Chunks[to].ULen
	for _, p := range chunkPieces(h.mappings, runOff, runLen) {
		for off := uint64(0); off < p.length; off += zeroRunMax {
			n := min(uint64(zeroRunMax), p.length-off)
			if err := h.zeroRange(p.hostAddr+off, n); err != nil {
				return err
			}
		}
	}
	for ci := from; ci <= to; ci++ {
		h.populated[ci].Store(true)
	}
	return nil
}

// zeroRange maps zeros over a span, falling back pagewise on EEXIST.
func (h *Handler) zeroRange(dst uint64, length uint64) error {
	_, err := ioctlZeropage(h.uffd, dst, length)
	if err == nil || err == unix.EEXIST {
		h.stats.Zeropages.Add(length / 4096)
		return nil
	}
	if err == unix.EAGAIN {
		pageSize := uint64(4096)
		if m := h.regionFor(dst); m != nil {
			pageSize = m.PageSize
		}
		for off := uint64(0); off < length; off += pageSize {
			if zerr := h.zeroPage(dst+off, pageSize); zerr != nil {
				return zerr
			}
		}
		return nil
	}
	return fmt.Errorf("uffd: UFFDIO_ZEROPAGE dst=%#x len=%d: %w", dst, length, err)
}

// chunkedPrefetch is the restore pipeline's engine: eagerly load the
// recorded working set in trace order (REAP), then backfill every remaining
// chunk sequentially while faults keep being served with priority
// (FaaSnap-style concurrent paging). Prefetch failures are non-fatal — the
// fault path retries on its own.
func (h *Handler) chunkedPrefetch(ws *WSTrace) {
	start := time.Now()
	scratch := make([]byte, h.cfg.View.ChunkSize)
	if ws != nil {
		for _, ci := range ws.Chunks {
			if h.stopped() {
				return
			}
			if ci < 0 || ci >= len(h.populated) || h.populated[ci].Load() {
				continue
			}
			if err := h.populateChunk(ci, scratch, &h.stats.BytesCopiedPrefetch); err != nil {
				h.logf("ws prefetch: chunk %d: %v", ci, err)
				continue
			}
			h.stats.ChunksPrefetchedWS.Add(1)
		}
		h.logf("ws prefetch done: %d chunks in %s", h.stats.ChunksPrefetchedWS.Load(), time.Since(start))
	}
	for ci := 0; ci < len(h.cfg.View.Chunks); ci++ {
		if h.stopped() {
			return
		}
		if h.populated[ci].Load() {
			continue
		}
		// Zero chunks come in huge contiguous runs (never-plugged hotplug
		// ranges, ballooned-out regions — M6): one ZEROPAGE per run instead
		// of one ioctl per 8-16KiB chunk.
		if h.cfg.View.Chunks[ci].Zero {
			end := ci
			for end+1 < len(h.cfg.View.Chunks) && h.cfg.View.Chunks[end+1].Zero && !h.populated[end+1].Load() {
				end++
			}
			if err := h.populateZeroRun(ci, end); err != nil {
				h.logf("backfill: zero run %d-%d: %v", ci, end, err)
			} else {
				h.stats.ChunksBackfilled.Add(uint64(end - ci + 1))
			}
			ci = end
			continue
		}
		if err := h.populateChunk(ci, scratch, &h.stats.BytesCopiedPrefetch); err != nil {
			h.logf("backfill: chunk %d: %v", ci, err)
			continue
		}
		h.stats.ChunksBackfilled.Add(1)
	}
	h.logf("backfill finished in %s (%d ws + %d backfilled chunks)",
		time.Since(start), h.stats.ChunksPrefetchedWS.Load(), h.stats.ChunksBackfilled.Load())
}

// initChunked prepares chunked-mode state after the handshake announced the
// regions: per-chunk populated flags, scratch buffer, WS recording/replay.
func (h *Handler) initChunked() (*WSTrace, error) {
	v := h.cfg.View
	for i := range h.mappings {
		m := &h.mappings[i]
		if m.Offset+m.Size > uint64(v.MemSizeBytes) {
			return nil, fmt.Errorf("uffd: region [offset=%d size=%d] exceeds snapshot memory size %d",
				m.Offset, m.Size, v.MemSizeBytes)
		}
	}
	var total uint64
	for i := range h.mappings {
		total += h.mappings[i].Size
	}
	h.stats.MemTotalBytes.Store(total)
	h.populated = make([]atomic.Bool, len(v.Chunks))
	h.faultBuf = make([]byte, v.ChunkSize)

	var ws *WSTrace
	if h.cfg.WSPath != "" {
		var err error
		ws, err = ReadWSTrace(h.cfg.WSPath)
		if err != nil {
			return nil, err
		}
		if ws == nil {
			h.wsRec = newWSRecorder(v.ChunkSize) // first resume: record
		} else {
			h.stats.WSChunksLoaded.Store(uint64(len(ws.Chunks)))
		}
	}
	return ws, nil
}

// finishChunked persists a newly recorded working set on the way out.
func (h *Handler) finishChunked() {
	if h.wsRec == nil || h.cfg.WSPath == "" {
		return
	}
	t := h.wsRec.trace()
	if len(t.Chunks) == 0 {
		return // nothing observed (e.g. handshake failed); keep no empty trace
	}
	if err := t.WriteFile(h.cfg.WSPath); err != nil {
		h.logf("writing ws trace: %v", err)
		return
	}
	h.logf("recorded working set: %d chunks -> %s", len(t.Chunks), h.cfg.WSPath)
}
