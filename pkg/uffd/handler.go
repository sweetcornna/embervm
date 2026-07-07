//go:build linux

package uffd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/embervm/embervm/pkg/memsnap"
	"golang.org/x/sys/unix"
)

// Mode selects how snapshot memory is brought back.
type Mode string

const (
	// ModeLazy serves pages strictly on demand (pure lazy loading). This is
	// the known-slow baseline: sequential single-page faulting tops out far
	// below prefetch throughput (REAP: 43 MB/s vs 533 MB/s).
	ModeLazy Mode = "lazy"
	// ModePrefetch additionally streams the whole memory file into the guest
	// address space in the background, sequentially, while faults are served
	// concurrently (FaaSnap-style concurrent paging; M0 approximation of the
	// working-set prefetch that ModeChunked implements).
	ModePrefetch Mode = "prefetch"
	// ModeChunked serves memory from a chunked, compressed, content-addressed
	// snapshot (pkg/memsnap manifests + a chunk store) instead of a raw
	// memfile: faults populate whole 16 KiB chunks, the recorded working set
	// is prefetched eagerly in trace order, and the rest backfills in the
	// background (M2 restore pipeline: REAP + FaaSnap).
	ModeChunked Mode = "chunked"
)

const defaultPrefetchBlock = 4 << 20

// Config configures a Handler.
type Config struct {
	// SocketPath is the unix socket Firecracker connects to; created by New,
	// removed on Close.
	SocketPath string
	// MemfilePath is the snapshot memory file to serve pages from
	// (ModeLazy/ModePrefetch; unused in ModeChunked).
	MemfilePath string
	Mode        Mode
	// PrefetchBlock is the bytes per background UFFDIO_COPY in ModePrefetch
	// (default 4 MiB, rounded to page size).
	PrefetchBlock uint64
	// HandshakeTimeout bounds the wait for Firecracker to connect
	// (default 120s).
	HandshakeTimeout time.Duration
	Verbose          bool

	// ModeChunked inputs: the resolved layer-chain view and a chunk fetcher
	// (local store, optionally tiered over L1).
	View   *memsnap.View
	Chunks ChunkGetter
	// WSPath, when set, enables working-set handling: replayed as the eager
	// prefetch order when the file exists, recorded from first-touch fault
	// order when it does not.
	WSPath string
}

// Handler owns one VM's restore: one listening socket, one userfaultfd, one
// memory file. Run one Handler process per VM (a crashed handler means the
// VM hangs on its next fault, so isolation and an external watchdog matter;
// see docs/zh/04 §6).
type Handler struct {
	cfg      Config
	listener *net.UnixListener
	conn     *net.UnixConn
	uffd     int
	mappings []GuestRegionUffdMapping
	backing  []byte
	removed  rangeSet
	stats    Stats

	// ModeChunked state.
	populated []atomic.Bool // per-chunk fast path around EEXIST round trips
	faultBuf  []byte        // fault-loop scratch (prefetch has its own)
	wsRec     *wsRecorder   // non-nil only while recording a first-resume WS

	quit     chan struct{}
	quitOnce sync.Once
}

// New creates the listening socket so that the caller (and Firecracker) can
// rely on its existence before PUT /snapshot/load is issued.
func New(cfg Config) (*Handler, error) {
	if cfg.SocketPath == "" {
		return nil, errors.New("uffd: SocketPath is required")
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeLazy
	}
	switch cfg.Mode {
	case ModeLazy, ModePrefetch:
		if cfg.MemfilePath == "" {
			return nil, errors.New("uffd: MemfilePath is required in lazy/prefetch modes")
		}
	case ModeChunked:
		if cfg.View == nil || cfg.Chunks == nil {
			return nil, errors.New("uffd: View and Chunks are required in chunked mode")
		}
	default:
		return nil, fmt.Errorf("uffd: unknown mode %q", cfg.Mode)
	}
	if cfg.PrefetchBlock == 0 {
		cfg.PrefetchBlock = defaultPrefetchBlock
	}
	if cfg.HandshakeTimeout == 0 {
		cfg.HandshakeTimeout = 120 * time.Second
	}
	_ = os.Remove(cfg.SocketPath)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: cfg.SocketPath, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("uffd: listen %s: %w", cfg.SocketPath, err)
	}
	return &Handler{cfg: cfg, listener: l, uffd: -1, quit: make(chan struct{})}, nil
}

// Stats returns the handler's counters (safe to read at any time).
func (h *Handler) Stats() *Stats { return &h.stats }

// Stop asks Serve to wind down. Safe to call from signal handlers and
// multiple times.
func (h *Handler) Stop() {
	h.quitOnce.Do(func() {
		close(h.quit)
		// Unblock a pending Accept.
		_ = h.listener.SetDeadline(time.Now())
	})
}

func (h *Handler) stopped() bool {
	select {
	case <-h.quit:
		return true
	default:
		return false
	}
}

// Serve performs the handshake and then serves page faults until the VM goes
// away (peer socket EOF), Stop is called, or a fatal error occurs.
func (h *Handler) Serve() error {
	defer h.cleanup()

	mappings, fd, conn, err := Handshake(h.listener, h.cfg.HandshakeTimeout)
	if err != nil {
		if h.stopped() {
			return nil
		}
		return err
	}
	h.mappings, h.uffd, h.conn = mappings, fd, conn
	h.stats.HandshakeUnixNs.Store(time.Now().UnixNano())
	h.stats.Regions.Store(int64(len(mappings)))

	var ws *WSTrace
	if h.cfg.Mode == ModeChunked {
		if ws, err = h.initChunked(); err != nil {
			return err
		}
		defer h.finishChunked()
	} else if err := h.mapBacking(); err != nil {
		return err
	}
	h.logf("handshake done: %d region(s), %d bytes guest memory, mode=%s",
		len(h.mappings), h.stats.MemTotalBytes.Load(), h.cfg.Mode)

	// Firecracker holds the peer end open for the VM's lifetime; EOF here
	// means the VM exited and we should too.
	go h.watchPeer()

	switch h.cfg.Mode {
	case ModePrefetch:
		go h.prefetch()
	case ModeChunked:
		go h.chunkedPrefetch(ws)
	}

	return h.faultLoop()
}

func (h *Handler) mapBacking() error {
	f, err := os.Open(h.cfg.MemfilePath)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	var total uint64
	for _, m := range h.mappings {
		if m.Offset+m.Size > uint64(st.Size()) {
			return fmt.Errorf("uffd: region [offset=%d size=%d] exceeds memfile size %d",
				m.Offset, m.Size, st.Size())
		}
		total += m.Size
	}
	h.stats.MemTotalBytes.Store(total)
	b, err := unix.Mmap(int(f.Fd()), 0, int(st.Size()), unix.PROT_READ, unix.MAP_PRIVATE)
	if err != nil {
		return fmt.Errorf("uffd: mmap memfile: %w", err)
	}
	h.backing = b
	return nil
}

func (h *Handler) watchPeer() {
	buf := make([]byte, 1)
	for {
		if _, err := h.conn.Read(buf); err != nil {
			h.logf("peer socket closed (%v), shutting down", err)
			h.Stop()
			return
		}
	}
}

func (h *Handler) faultLoop() error {
	buf := make([]byte, uffdMsgSize*64)
	fds := []unix.PollFd{{Fd: int32(h.uffd), Events: unix.POLLIN}}
	for {
		if h.stopped() {
			return nil
		}
		fds[0].Revents = 0
		n, err := unix.Poll(fds, 500)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return fmt.Errorf("uffd: poll: %w", err)
		}
		if n == 0 {
			continue
		}
		if fds[0].Revents&(unix.POLLERR|unix.POLLHUP) != 0 {
			h.logf("uffd polled ERR/HUP, VM is gone")
			return nil
		}
		nr, err := unix.Read(h.uffd, buf)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EINTR {
				continue
			}
			if h.stopped() {
				return nil
			}
			return fmt.Errorf("uffd: read: %w", err)
		}
		for off := 0; off+uffdMsgSize <= nr; off += uffdMsgSize {
			if err := h.handleMsg(buf[off : off+uffdMsgSize]); err != nil {
				return err
			}
		}
	}
}

func (h *Handler) handleMsg(msg []byte) error {
	event := msg[0]
	a := binary.LittleEndian.Uint64(msg[8:16])
	b := binary.LittleEndian.Uint64(msg[16:24])
	switch event {
	case uffdEventPagefault:
		// a=flags, b=address
		return h.servePage(b)
	case uffdEventRemove, uffdEventUnmap:
		// a=start, b=end. The kernel already zapped these pages (balloon
		// madvise); snapshot contents are stale for this range, so future
		// faults must see zeros. Note: a background prefetch COPY that races
		// this event could re-populate stale bytes; M0 VMs carry no balloon
		// device so the event is not expected — we track it defensively and
		// count it so the race would at least be visible in metrics.
		h.removed.add(a, b)
		h.stats.RemoveEvents.Add(1)
		h.logf("remove/unmap event: [%#x, %#x)", a, b)
	case uffdEventFork, uffdEventRemap:
		h.logf("unexpected uffd event %#x (fork/remap); ignoring", event)
	default:
		h.logf("unknown uffd event %#x; ignoring", event)
	}
	return nil
}

func (h *Handler) servePage(addr uint64) error {
	if h.cfg.Mode == ModeChunked {
		return h.serveChunkFault(addr)
	}
	m := h.regionFor(addr)
	if m == nil {
		// Nothing we can do: not our region. The faulting vCPU would hang,
		// but this indicates a serious layout mismatch — surface it.
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

	src := m.Offset + (page - m.BaseHostVirtAddr)
	if err := h.copyPage(page, src, m.PageSize); err != nil {
		return err
	}
	h.stats.FaultsServed.Add(1)
	return nil
}

func (h *Handler) copyPage(dst, srcOff, length uint64) error {
	for retries := 0; ; retries++ {
		_, err := ioctlCopy(h.uffd, dst, uint64(sliceAddr(h.backing, srcOff)), length)
		switch {
		case err == nil:
			h.stats.BytesCopiedFault.Add(length)
			return nil
		case err == unix.EEXIST:
			// Already populated (prefetch won the race). The earlier COPY's
			// wake covers our faulter — nothing else to do.
			return nil
		case err == unix.EAGAIN && retries < 64:
			// The VM's memory layout is changing under us; retry.
			continue
		default:
			return fmt.Errorf("uffd: UFFDIO_COPY dst=%#x len=%d: %w", dst, length, err)
		}
	}
}

func (h *Handler) zeroPage(dst, length uint64) error {
	for retries := 0; ; retries++ {
		_, err := ioctlZeropage(h.uffd, dst, length)
		switch {
		case err == nil || err == unix.EEXIST:
			return nil
		case err == unix.EAGAIN && retries < 64:
			continue
		default:
			return fmt.Errorf("uffd: UFFDIO_ZEROPAGE dst=%#x len=%d: %w", dst, length, err)
		}
	}
}

func (h *Handler) regionFor(addr uint64) *GuestRegionUffdMapping {
	for i := range h.mappings {
		m := &h.mappings[i]
		if addr >= m.BaseHostVirtAddr && addr < m.End() {
			return m
		}
	}
	return nil
}

func (h *Handler) cleanup() {
	if h.conn != nil {
		h.conn.Close()
	}
	if h.uffd >= 0 {
		unix.Close(h.uffd)
	}
	if h.backing != nil {
		_ = unix.Munmap(h.backing)
	}
	h.listener.Close()
	_ = os.Remove(h.cfg.SocketPath)
}

func (h *Handler) logf(format string, args ...any) {
	if h.cfg.Verbose {
		log.Printf(format, args...)
	}
}
