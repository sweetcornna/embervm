//go:build linux

// Command uffdiocopy benchmarks userfaultfd page-population throughput
// (UFFDIO_COPY and UFFDIO_ZEROPAGE) across thread counts and chunk sizes.
//
// It fills a documented data gap (docs/zh/04-创新与最佳实践.md §9): no
// authoritative multi-thread UFFDIO_COPY throughput table exists, yet that
// number upper-bounds how fast a userfaultfd-backed restore path can page a
// microVM snapshot back into guest memory.
//
// Design: each worker thread (runtime.LockOSThread) owns its own userfaultfd,
// its own MAP_PRIVATE|MAP_ANONYMOUS region registered with
// UFFDIO_REGISTER_MODE_MISSING, and its own random source buffer. The worker
// walks the region in chunk-sized UFFDIO_COPY steps; when it reaches the end
// it depopulates the region with madvise(MADV_DONTNEED) and starts over, which
// keeps peak RSS bounded to roughly threads * region-mb.
//
// This tool is deliberately self-contained: it defines its own userfaultfd
// ioctl constants and argument structs instead of importing pkg/uffd, so it
// can be copied to a bare host and built with only golang.org/x/sys.
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const mib = 1 << 20

// userfaultfd ABI. The ioctl request encodings are identical on x86_64 and
// aarch64 (_IOWR('\xaa', nr, struct ...)).
const (
	uffdAPIVersion = 0xAA

	uffdioAPIIoctl      = 0xc018aa3f // _IOWR(UFFDIO, 0x3F, struct uffdio_api)
	uffdioRegisterIoctl = 0xc020aa00 // _IOWR(UFFDIO, 0x00, struct uffdio_register)
	uffdioCopyIoctl     = 0xc028aa03 // _IOWR(UFFDIO, 0x03, struct uffdio_copy)
	uffdioZeropageIoctl = 0xc020aa04 // _IOWR(UFFDIO, 0x04, struct uffdio_zeropage)

	uffdioRegisterModeMissing = 1
)

// uffdioAPI mirrors struct uffdio_api.
type uffdioAPI struct {
	api      uint64
	features uint64
	ioctls   uint64
}

// uffdioRange mirrors struct uffdio_range.
type uffdioRange struct {
	start uint64
	len   uint64
}

// uffdioRegister mirrors struct uffdio_register.
type uffdioRegister struct {
	rng    uffdioRange
	mode   uint64
	ioctls uint64
}

// uffdioCopy mirrors struct uffdio_copy.
type uffdioCopy struct {
	dst  uint64
	src  uint64
	len  uint64
	mode uint64
	copy int64
}

// uffdioZeropage mirrors struct uffdio_zeropage.
type uffdioZeropage struct {
	rng      uffdioRange
	mode     uint64
	zeropage int64
}

const (
	opCopy     = "copy"
	opZeropage = "zeropage"
	opBoth     = "both"
)

type cellResult struct {
	Op         string  `json:"op"`
	Threads    int     `json:"threads"`
	ChunkBytes uint64  `json:"chunk_bytes"`
	TotalMB    float64 `json:"total_mb"`
	MBPerS     float64 `json:"mb_per_s"`
}

type report struct {
	PageSize int          `json:"page_size"`
	RegionMB uint64       `json:"region_mb"`
	Seconds  float64      `json:"seconds"`
	Cells    []cellResult `json:"cells"`
}

func main() {
	var (
		threadsFlag = flag.String("threads", "1,2,4,8", "comma-separated worker thread counts")
		chunksFlag  = flag.String("chunks", "4096,65536,2097152", "comma-separated chunk sizes in bytes (page-size multiples)")
		regionMB    = flag.Uint64("region-mb", 512, "per-thread target region size in MiB")
		seconds     = flag.Float64("seconds", 3, "wall seconds per benchmark cell")
		opFlag      = flag.String("op", opCopy, "operation to benchmark: copy|zeropage|both")
		outFlag     = flag.String("out", "results/uffdio-copy.json", "JSON output path")
	)
	flag.Parse()

	if err := run(*threadsFlag, *chunksFlag, *regionMB, *seconds, *opFlag, *outFlag); err != nil {
		fmt.Fprintf(os.Stderr, "uffdiocopy: %v\n", err)
		os.Exit(1)
	}
}

func run(threadsFlag, chunksFlag string, regionMB uint64, seconds float64, opFlag, outPath string) error {
	pageSize := uint64(os.Getpagesize())

	threadsList, err := parseIntList(threadsFlag)
	if err != nil {
		return fmt.Errorf("--threads: %w", err)
	}
	chunkList, err := parseUint64List(chunksFlag)
	if err != nil {
		return fmt.Errorf("--chunks: %w", err)
	}
	if seconds <= 0 {
		return fmt.Errorf("--seconds must be > 0, got %g", seconds)
	}
	if regionMB == 0 {
		return fmt.Errorf("--region-mb must be > 0")
	}

	var ops []string
	switch opFlag {
	case opCopy, opZeropage:
		ops = []string{opFlag}
	case opBoth:
		ops = []string{opCopy, opZeropage}
	default:
		return fmt.Errorf("--op must be copy, zeropage or both, got %q", opFlag)
	}

	maxThreads := 0
	for _, t := range threadsList {
		if t < 1 {
			return fmt.Errorf("--threads entries must be >= 1, got %d", t)
		}
		if t > maxThreads {
			maxThreads = t
		}
	}
	var maxChunk uint64
	for _, c := range chunkList {
		if c == 0 || c%pageSize != 0 {
			return fmt.Errorf("--chunks entries must be non-zero multiples of the page size (%d), got %d", pageSize, c)
		}
		if c > maxChunk {
			maxChunk = c
		}
	}

	regionMB = capRegionToMemAvailable(regionMB, maxThreads, maxChunk)
	regionLen := regionMB * mib
	for _, c := range chunkList {
		if c > regionLen {
			return fmt.Errorf("chunk %d exceeds per-thread region of %d MiB", c, regionMB)
		}
	}

	rep := report{
		PageSize: int(pageSize),
		RegionMB: regionMB,
		Seconds:  seconds,
	}
	duration := time.Duration(seconds * float64(time.Second))

	for _, op := range ops {
		for _, threads := range threadsList {
			for _, chunk := range chunkList {
				fmt.Fprintf(os.Stderr, "running op=%s threads=%d chunk=%d region-mb=%d seconds=%g ...\n",
					op, threads, chunk, regionMB, seconds)
				cell, err := runCell(op, threads, chunk, regionLen, duration)
				if err != nil {
					return fmt.Errorf("cell op=%s threads=%d chunk=%d: %w", op, threads, chunk, err)
				}
				rep.Cells = append(rep.Cells, cell)
			}
		}
	}

	printTable(rep)
	if err := writeJSON(outPath, rep); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", outPath)
	return nil
}

// capRegionToMemAvailable warns and shrinks the per-thread region if the
// worst-case populated footprint (maxThreads * region) would exceed 50% of
// MemAvailable. MADV_DONTNEED resets already bound steady-state RSS, but the
// instantaneous peak right before a reset is the full footprint.
func capRegionToMemAvailable(regionMB uint64, maxThreads int, maxChunk uint64) uint64 {
	avail, err := memAvailableBytes()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot read MemAvailable (%v); skipping memory budget check\n", err)
		return regionMB
	}
	budget := avail / 2
	need := uint64(maxThreads) * regionMB * mib
	if need <= budget {
		return regionMB
	}
	shrunk := budget / uint64(maxThreads) / mib
	minMB := (maxChunk + mib - 1) / mib
	if minMB == 0 {
		minMB = 1
	}
	if shrunk < minMB {
		shrunk = minMB
	}
	fmt.Fprintf(os.Stderr,
		"warning: %d threads x %d MiB = %d MiB exceeds 50%% of MemAvailable (%d MiB); shrinking region-mb to %d\n",
		maxThreads, regionMB, uint64(maxThreads)*regionMB, budget/mib, shrunk)
	return shrunk
}

func memAvailableBytes() (uint64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "MemAvailable:" {
			kb, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parse MemAvailable: %w", err)
			}
			return kb * 1024, nil
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("MemAvailable not found in /proc/meminfo")
}

// runCell runs one (op, threads, chunk) benchmark cell and returns the
// aggregate result across all workers.
func runCell(op string, threads int, chunk, regionLen uint64, duration time.Duration) (cellResult, error) {
	workers := make([]*worker, 0, threads)
	defer func() {
		for _, w := range workers {
			w.close()
		}
	}()
	for i := 0; i < threads; i++ {
		w, err := newWorker(regionLen, chunk)
		if err != nil {
			return cellResult{}, fmt.Errorf("worker %d setup: %w", i, err)
		}
		workers = append(workers, w)
	}

	type workerResult struct {
		bytes uint64
		err   error
	}
	results := make([]workerResult, threads)

	var wg sync.WaitGroup
	begin := time.Now()
	deadline := begin.Add(duration)
	for i, w := range workers {
		wg.Add(1)
		go func(i int, w *worker) {
			defer wg.Done()
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			b, err := w.run(op, chunk, deadline)
			results[i] = workerResult{bytes: b, err: err}
		}(i, w)
	}
	wg.Wait()
	elapsed := time.Since(begin)

	var total uint64
	for i, r := range results {
		if r.err != nil {
			return cellResult{}, fmt.Errorf("worker %d: %w", i, r.err)
		}
		total += r.bytes
	}

	totalMB := float64(total) / mib
	return cellResult{
		Op:         op,
		Threads:    threads,
		ChunkBytes: chunk,
		TotalMB:    round1(totalMB),
		MBPerS:     round1(totalMB / elapsed.Seconds()),
	}, nil
}

// worker owns one userfaultfd, one registered anonymous region and one source
// buffer. Nothing is shared between workers.
type worker struct {
	fd     uintptr
	region []byte
	src    []byte
}

func newWorker(regionLen, chunk uint64) (*worker, error) {
	fd, err := createUserfaultfd()
	if err != nil {
		return nil, err
	}
	w := &worker{fd: fd}

	api := uffdioAPI{api: uffdAPIVersion}
	if errno := ioctl(fd, uffdioAPIIoctl, unsafe.Pointer(&api)); errno != 0 {
		w.close()
		return nil, fmt.Errorf("ioctl(UFFDIO_API): %w", errno)
	}

	region, err := unix.Mmap(-1, 0, int(regionLen),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		w.close()
		return nil, fmt.Errorf("mmap %d bytes: %w", regionLen, err)
	}
	w.region = region

	reg := uffdioRegister{
		rng: uffdioRange{
			start: uint64(uintptr(unsafe.Pointer(&region[0]))),
			len:   regionLen,
		},
		mode: uffdioRegisterModeMissing,
	}
	if errno := ioctl(fd, uffdioRegisterIoctl, unsafe.Pointer(&reg)); errno != 0 {
		w.close()
		return nil, fmt.Errorf("ioctl(UFFDIO_REGISTER): %w", errno)
	}

	w.src = make([]byte, chunk)
	if _, err := rand.Read(w.src); err != nil {
		w.close()
		return nil, fmt.Errorf("fill source buffer: %w", err)
	}
	return w, nil
}

func createUserfaultfd() (uintptr, error) {
	fd, _, errno := unix.Syscall(unix.SYS_USERFAULTFD,
		uintptr(unix.O_CLOEXEC|unix.O_NONBLOCK), 0, 0)
	if errno != 0 {
		switch errno {
		case unix.EPERM:
			return 0, fmt.Errorf("userfaultfd(2): %w; run: sudo sysctl -w vm.unprivileged_userfaultfd=1 (or run with CAP_SYS_PTRACE)", errno)
		case unix.ENOSYS:
			return 0, fmt.Errorf("userfaultfd(2): %w; kernel built without CONFIG_USERFAULTFD", errno)
		default:
			return 0, fmt.Errorf("userfaultfd(2): %w", errno)
		}
	}
	return fd, nil
}

func (w *worker) close() {
	if w.region != nil {
		_ = unix.Munmap(w.region)
		w.region = nil
	}
	if w.fd != 0 {
		_ = unix.Close(int(w.fd))
		w.fd = 0
	}
}

// run walks the region in chunk-sized steps until the deadline, populating
// pages with UFFDIO_COPY or UFFDIO_ZEROPAGE, and returns total bytes
// populated. When the walker reaches the region end, the region is
// depopulated with MADV_DONTNEED and the walker resets.
func (w *worker) run(op string, chunk uint64, deadline time.Time) (uint64, error) {
	base := uint64(uintptr(unsafe.Pointer(&w.region[0])))
	src := uint64(uintptr(unsafe.Pointer(&w.src[0])))
	regionLen := uint64(len(w.region))

	var total, off uint64
	for iter := 0; ; iter++ {
		// Amortize the clock read; overshoot is fine because throughput is
		// computed from measured wall time, not the requested duration.
		if iter&63 == 0 && !time.Now().Before(deadline) {
			break
		}
		if off+chunk > regionLen {
			if err := unix.Madvise(w.region, unix.MADV_DONTNEED); err != nil {
				return total, fmt.Errorf("madvise(MADV_DONTNEED): %w", err)
			}
			off = 0
		}

		var (
			errno unix.Errno
			done  int64
		)
		if op == opCopy {
			arg := uffdioCopy{dst: base + off, src: src, len: chunk}
			errno = ioctl(w.fd, uffdioCopyIoctl, unsafe.Pointer(&arg))
			done = arg.copy
		} else {
			arg := uffdioZeropage{rng: uffdioRange{start: base + off, len: chunk}}
			errno = ioctl(w.fd, uffdioZeropageIoctl, unsafe.Pointer(&arg))
			done = arg.zeropage
		}

		switch errno {
		case 0:
			// Full-length success: the kernel reports len in the result
			// field; count the whole chunk.
			total += chunk
			off += chunk
		case unix.EAGAIN:
			// The kernel reports partial progress through the result field.
			// Account for it and retry from the first unpopulated byte
			// (retrying the already-populated prefix would raise EEXIST).
			if done > 0 {
				total += uint64(done)
				off += uint64(done)
			}
		case unix.EEXIST:
			// Should not happen right after an MADV_DONTNEED reset; skip the
			// already-populated chunk and advance.
			off += chunk
		default:
			return total, fmt.Errorf("ioctl(%s) at offset %#x (chunk %d): %w",
				ioctlName(op), off, chunk, errno)
		}
	}
	runtime.KeepAlive(w.src)
	runtime.KeepAlive(w.region)
	return total, nil
}

func ioctlName(op string) string {
	if op == opZeropage {
		return "UFFDIO_ZEROPAGE"
	}
	return "UFFDIO_COPY"
}

func ioctl(fd, req uintptr, arg unsafe.Pointer) unix.Errno {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, req, uintptr(arg))
	return errno
}

func printTable(rep report) {
	fmt.Printf("page_size=%d region_mb=%d seconds=%g\n", rep.PageSize, rep.RegionMB, rep.Seconds)
	fmt.Printf("%-10s %8s %12s %12s %12s\n", "OP", "THREADS", "CHUNK_BYTES", "TOTAL_MB", "MB_PER_S")
	for _, c := range rep.Cells {
		fmt.Printf("%-10s %8d %12d %12.1f %12.1f\n", c.Op, c.Threads, c.ChunkBytes, c.TotalMB, c.MBPerS)
	}
}

func writeJSON(path string, rep report) error {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create output dir: %w", err)
		}
	}
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func round1(x float64) float64 {
	return math.Round(x*10) / 10
}

func parseIntList(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		v, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("invalid entry %q: %w", part, err)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty list")
	}
	return out, nil
}

func parseUint64List(s string) ([]uint64, error) {
	var out []uint64
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		v, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid entry %q: %w", part, err)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty list")
	}
	return out, nil
}
