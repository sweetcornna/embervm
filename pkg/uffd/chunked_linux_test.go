//go:build linux

package uffd

import (
	"bytes"
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"unsafe"

	"github.com/embervm/embervm/pkg/chunkstore"
	"github.com/embervm/embervm/pkg/memsnap"
	"golang.org/x/sys/unix"
)

// userfaultfd ABI bits needed only by this test (production registration is
// Firecracker's job).
const (
	uffdioAPI              = 0xc018aa3f
	uffdioRegister         = 0xc020aa00
	uffdAPIVersion         = 0xaa
	uffdRegisterModeMissng = 1
	uffdUserModeOnly       = 1
)

type uffdioAPIArg struct {
	API      uint64
	Features uint64
	Ioctls   uint64
}

type uffdioRegisterArg struct {
	Range  uffdioRange
	Mode   uint64
	Ioctls uint64
}

// newTestUffd opens a userfaultfd and registers [addr, addr+size) for
// missing-page handling, skipping the test where the kernel forbids it.
func newTestUffd(t *testing.T, addr, size uint64) int {
	t.Helper()
	fd, _, errno := unix.Syscall(unix.SYS_USERFAULTFD, unix.O_CLOEXEC|unix.O_NONBLOCK, 0, 0)
	if errno != 0 {
		fd, _, errno = unix.Syscall(unix.SYS_USERFAULTFD, unix.O_CLOEXEC|unix.O_NONBLOCK|uffdUserModeOnly, 0, 0)
	}
	if errno != 0 {
		t.Skipf("userfaultfd unavailable: %v", errno)
	}
	api := uffdioAPIArg{API: uffdAPIVersion}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, uffdioAPI, uintptr(unsafe.Pointer(&api))); errno != 0 {
		unix.Close(int(fd))
		t.Skipf("UFFDIO_API failed: %v", errno)
	}
	reg := uffdioRegisterArg{Range: uffdioRange{Start: addr, Len: size}, Mode: uffdRegisterModeMissng}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, uffdioRegister, uintptr(unsafe.Pointer(&reg))); errno != 0 {
		unix.Close(int(fd))
		t.Fatalf("UFFDIO_REGISTER: %v", errno)
	}
	t.Cleanup(func() { unix.Close(int(fd)) })
	return int(fd)
}

// TestPopulateChunkRealUffd drives the full chunked population path — layer
// write, store, resolve, fetch, decode, UFFDIO_COPY/ZEROPAGE — against a
// real userfaultfd-registered mapping and verifies the resulting memory is
// byte-identical to the source image.
func TestPopulateChunkRealUffd(t *testing.T) {
	const chunkSize = memsnap.DefaultChunkSize
	const nChunks = 8
	size := uint64(nChunks * chunkSize)

	mem, err := unix.Mmap(-1, 0, int(size), unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Munmap(mem) })
	base := uint64(uintptr(unsafe.Pointer(&mem[0])))
	fd := newTestUffd(t, base, size)

	// Image: zero chunk | compressible | random | rest compressible patterns.
	img := make([]byte, size)
	rng := rand.New(rand.NewSource(7))
	copy(img[chunkSize:], bytes.Repeat([]byte("A"), chunkSize))
	rng.Read(img[2*chunkSize : 3*chunkSize])
	for i := 3; i < nChunks; i++ {
		copy(img[i*chunkSize:], bytes.Repeat([]byte{byte('a' + i)}, chunkSize))
	}
	memPath := filepath.Join(t.TempDir(), "memfile")
	if err := os.WriteFile(memPath, img, 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := chunkstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sink := chunkstore.Bytes{Ctx: context.Background(), S: store}
	manifest, err := memsnap.WriteLayer(memPath, memsnap.WriteOptions{LayerID: "p1"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	view, err := memsnap.Resolve([]*memsnap.Manifest{manifest})
	if err != nil {
		t.Fatal(err)
	}

	h := &Handler{
		cfg:  Config{Mode: ModeChunked, View: view, Chunks: sink},
		uffd: fd,
		mappings: []GuestRegionUffdMapping{
			{BaseHostVirtAddr: base, Size: size, Offset: 0, PageSize: 4096},
		},
		quit: make(chan struct{}),
	}
	h.populated = make([]atomic.Bool, len(view.Chunks))
	h.faultBuf = make([]byte, view.ChunkSize)

	for ci := range view.Chunks {
		if err := h.populateChunk(ci, h.faultBuf, &h.stats.BytesCopiedPrefetch); err != nil {
			t.Fatalf("populateChunk(%d): %v", ci, err)
		}
	}
	if !bytes.Equal(mem, img) {
		t.Fatal("populated memory differs from source image")
	}
	// Idempotence: repopulating must not error (EEXIST tolerated).
	for ci := range view.Chunks {
		h.populated[ci].Store(false)
		if err := h.populateChunk(ci, h.faultBuf, &h.stats.BytesCopiedPrefetch); err != nil {
			t.Fatalf("repopulateChunk(%d): %v", ci, err)
		}
	}
	if !bytes.Equal(mem, img) {
		t.Fatal("repopulation corrupted memory")
	}
}

// TestPopulateChunkRegionStraddle registers two disjoint mappings whose
// offsets leave a hole (PCI-hole shape) and verifies chunks straddling the
// hole land correctly in both regions.
func TestPopulateChunkRegionStraddle(t *testing.T) {
	const chunkSize = memsnap.DefaultChunkSize
	// Snapshot offset space: region A [0, 40K), hole [40K, 56K), region B [56K, 128K).
	// Chunk 2 [32K, 48K) straddles A's end; chunk 3 [48K, 64K) straddles B's start.
	sizeA, sizeB := uint64(40<<10), uint64(72<<10)
	offB := uint64(56 << 10)
	total := int64(128 << 10)

	memA, err := unix.Mmap(-1, 0, int(sizeA), unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Munmap(memA) })
	memB, err := unix.Mmap(-1, 0, int(sizeB), unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Munmap(memB) })
	baseA := uint64(uintptr(unsafe.Pointer(&memA[0])))
	baseB := uint64(uintptr(unsafe.Pointer(&memB[0])))

	fd, _, errno := unix.Syscall(unix.SYS_USERFAULTFD, unix.O_CLOEXEC|unix.O_NONBLOCK, 0, 0)
	if errno != 0 {
		fd, _, errno = unix.Syscall(unix.SYS_USERFAULTFD, unix.O_CLOEXEC|unix.O_NONBLOCK|uffdUserModeOnly, 0, 0)
	}
	if errno != 0 {
		t.Skipf("userfaultfd unavailable: %v", errno)
	}
	t.Cleanup(func() { unix.Close(int(fd)) })
	api := uffdioAPIArg{API: uffdAPIVersion}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, fd, uffdioAPI, uintptr(unsafe.Pointer(&api))); e != 0 {
		t.Skipf("UFFDIO_API failed: %v", e)
	}
	for _, r := range []uffdioRange{{Start: baseA, Len: sizeA}, {Start: baseB, Len: sizeB}} {
		reg := uffdioRegisterArg{Range: r, Mode: uffdRegisterModeMissng}
		if _, _, e := unix.Syscall(unix.SYS_IOCTL, fd, uffdioRegister, uintptr(unsafe.Pointer(&reg))); e != 0 {
			t.Fatalf("UFFDIO_REGISTER: %v", e)
		}
	}

	img := make([]byte, total)
	for i := range img {
		img[i] = byte(i / 1024)
	}
	memPath := filepath.Join(t.TempDir(), "memfile")
	if err := os.WriteFile(memPath, img, 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := chunkstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sink := chunkstore.Bytes{Ctx: context.Background(), S: store}
	manifest, err := memsnap.WriteLayer(memPath, memsnap.WriteOptions{LayerID: "p1"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	view, err := memsnap.Resolve([]*memsnap.Manifest{manifest})
	if err != nil {
		t.Fatal(err)
	}

	h := &Handler{
		cfg:  Config{Mode: ModeChunked, View: view, Chunks: sink},
		uffd: int(fd),
		mappings: []GuestRegionUffdMapping{
			{BaseHostVirtAddr: baseA, Size: sizeA, Offset: 0, PageSize: 4096},
			{BaseHostVirtAddr: baseB, Size: sizeB, Offset: offB, PageSize: 4096},
		},
		quit: make(chan struct{}),
	}
	h.populated = make([]atomic.Bool, len(view.Chunks))
	h.faultBuf = make([]byte, view.ChunkSize)

	for ci := range view.Chunks {
		if err := h.populateChunk(ci, h.faultBuf, &h.stats.BytesCopiedPrefetch); err != nil {
			t.Fatalf("populateChunk(%d): %v", ci, err)
		}
	}
	if !bytes.Equal(memA, img[:sizeA]) {
		t.Fatal("region A content differs from source image")
	}
	if !bytes.Equal(memB, img[offB:offB+sizeB]) {
		t.Fatal("region B content differs from source image")
	}
}
