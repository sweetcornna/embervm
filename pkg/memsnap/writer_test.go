package memsnap

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// memSink is the reference Sink+Getter: content-addressed map with dedup.
type memSink struct {
	objects map[string][]byte
	puts    int
	dedup   int
}

func newMemSink() *memSink { return &memSink{objects: map[string][]byte{}} }

func (s *memSink) Put(hash string, stored []byte) (bool, error) {
	s.puts++
	if _, ok := s.objects[hash]; ok {
		s.dedup++
		return false, nil
	}
	s.objects[hash] = append([]byte(nil), stored...)
	return true, nil
}

func (s *memSink) Get(hash string) ([]byte, error) {
	stored, ok := s.objects[hash]
	if !ok {
		return nil, fmt.Errorf("no object %s", hash)
	}
	return stored, nil
}

// reconstruct rebuilds the full memory image from a resolved view + sink.
func reconstruct(t *testing.T, v *View, s *memSink) []byte {
	t.Helper()
	out := make([]byte, v.MemSizeBytes)
	for _, ref := range v.Chunks {
		if ref.Zero {
			continue
		}
		stored, ok := s.objects[ref.Hash]
		if !ok {
			t.Fatalf("chunk %d hash %s not in sink", ref.Index, ref.Hash)
		}
		data, err := Decode(ref, stored)
		if err != nil {
			t.Fatal(err)
		}
		copy(out[int64(ref.Index)*int64(v.ChunkSize):], data)
	}
	return out
}

const (
	testChunk = 16 * 1024
	testPage  = 4 * 1024
)

// buildTestImage: chunk0 zeros | chunk1 compressible | chunk2 random |
// trailing partial chunk (1000 bytes, compressible).
func buildTestImage(t *testing.T) []byte {
	t.Helper()
	rng := rand.New(rand.NewSource(42))
	img := make([]byte, 3*testChunk+1000)
	copy(img[testChunk:], bytes.Repeat([]byte("A"), testChunk))
	rng.Read(img[2*testChunk : 3*testChunk])
	copy(img[3*testChunk:], bytes.Repeat([]byte("B"), 1000))
	return img
}

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "memfile")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeSparse creates a sparse file of the given size with the given
// (offset, bytes) writes — the shape Firecracker gives a diff memfile.
func writeSparse(t *testing.T, size int64, writes map[int64][]byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "diff-memfile")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	for off, data := range writes {
		if _, err := f.WriteAt(data, off); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// needsSparse skips tests that require page-granular SEEK_DATA (linux-only;
// APFS reports materialized whole-file extents). CI lint-unit runs them.
func needsSparse(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("diff extraction requires linux SEEK_DATA semantics (GOOS=%s)", runtime.GOOS)
	}
}

func TestWriteLayerFull(t *testing.T) {
	img := buildTestImage(t)
	sink := newMemSink()
	m, err := WriteLayer(writeTemp(t, img), WriteOptions{
		LayerID: "p1", FCVersion: "v1.16.1", KernelVersion: "6.1.155",
	}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Chunks) != 4 {
		t.Fatalf("chunks = %d, want 4", len(m.Chunks))
	}
	c := m.Chunks
	if !c[0].Zero || c[0].ULen != testChunk {
		t.Errorf("chunk0 = %+v, want zero full-size", c[0])
	}
	if c[1].Zero || c[1].Codec != CodecLZ4 || c[1].CLen >= c[1].ULen {
		t.Errorf("chunk1 = %+v, want lz4-compressed", c[1])
	}
	if c[2].Codec != CodecRaw || c[2].CLen != testChunk {
		t.Errorf("chunk2 = %+v, want raw (incompressible)", c[2])
	}
	if c[3].ULen != 1000 || c[3].Codec != CodecLZ4 {
		t.Errorf("chunk3 = %+v, want 1000-byte lz4 partial", c[3])
	}
	for _, ref := range c[1:] {
		if got := len(sink.objects[ref.Hash]); got != ref.CLen {
			t.Errorf("chunk%d stored %d bytes, manifest clen %d", ref.Index, got, ref.CLen)
		}
	}
	v, err := Resolve([]*Manifest{m})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reconstruct(t, v, sink), img) {
		t.Fatal("full-layer reconstruction differs from source image")
	}
}

func TestWriteLayerRejectsParent(t *testing.T) {
	img := make([]byte, testChunk)
	_, err := WriteLayer(writeTemp(t, img), WriteOptions{LayerID: "p2", Parent: "p1"}, newMemSink())
	if err == nil {
		t.Fatal("WriteLayer accepted a parent (must use WriteDiffLayer)")
	}
}

func TestWriteLayerDedup(t *testing.T) {
	img := make([]byte, 2*testChunk)
	copy(img, bytes.Repeat([]byte("D"), testChunk))
	copy(img[testChunk:], bytes.Repeat([]byte("D"), testChunk))
	sink := newMemSink()
	m, err := WriteLayer(writeTemp(t, img), WriteOptions{LayerID: "p1"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if m.Chunks[0].Hash != m.Chunks[1].Hash {
		t.Fatal("identical chunks got different hashes")
	}
	if sink.puts != 2 || sink.dedup != 1 || len(sink.objects) != 1 {
		t.Fatalf("puts=%d dedup=%d objects=%d, want 2/1/1", sink.puts, sink.dedup, len(sink.objects))
	}
}

func TestWriteLayerSizeMismatch(t *testing.T) {
	img := make([]byte, testChunk)
	_, err := WriteLayer(writeTemp(t, img), WriteOptions{
		LayerID: "p1", MemSizeBytes: 2 * testChunk,
	}, newMemSink())
	if err == nil {
		t.Fatal("accepted memfile whose size differs from MemSizeBytes")
	}
}

func TestWriteDiffLayerWholeChunks(t *testing.T) {
	needsSparse(t)
	img := buildTestImage(t)
	sink := newMemSink()
	full, err := WriteLayer(writeTemp(t, img), WriteOptions{LayerID: "p1"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := Resolve([]*Manifest{full})
	if err != nil {
		t.Fatal(err)
	}

	// Whole-chunk dirty writes: data at chunk 1, explicit zeros at chunk 2
	// (a chunk dirtied back to zero must still override the parent).
	dirty1 := bytes.Repeat([]byte("X"), testChunk)
	diffPath := writeSparse(t, int64(len(img)), map[int64][]byte{
		1 * testChunk: dirty1,
		2 * testChunk: make([]byte, testChunk),
	})
	diff, err := WriteDiffLayer(diffPath, WriteOptions{LayerID: "p2", Parent: "p1"}, parent, sink, sink)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]ChunkRef{}
	for _, c := range diff.Chunks {
		got[c.Index] = c
	}
	if _, ok := got[1]; !ok {
		t.Fatalf("dirty chunk 1 missing from diff layer %v", diff.Chunks)
	}
	if !got[2].Zero {
		t.Errorf("zero-dirtied chunk 2 = %+v, want zero", got[2])
	}
	if _, ok := got[0]; ok {
		t.Errorf("clean chunk 0 leaked into diff layer")
	}

	want := append([]byte(nil), img...)
	copy(want[testChunk:], dirty1)
	copy(want[2*testChunk:3*testChunk], make([]byte, testChunk))
	v, err := Resolve([]*Manifest{diff, full}) // order must not matter
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reconstruct(t, v, sink), want) {
		t.Fatal("layered reconstruction differs from modified image")
	}
}

// A single dirty 4 KiB page inside a 16 KiB chunk must be merged with the
// parent chunk's content — the clean pages must NOT become zeros.
func TestWriteDiffLayerPartialDirtyMerge(t *testing.T) {
	needsSparse(t)
	img := buildTestImage(t)
	sink := newMemSink()
	full, err := WriteLayer(writeTemp(t, img), WriteOptions{LayerID: "p1"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := Resolve([]*Manifest{full})
	if err != nil {
		t.Fatal(err)
	}

	// Dirty only page 2 of chunk 2 (the random, raw-coded chunk).
	page := bytes.Repeat([]byte("P"), testPage)
	diffPath := writeSparse(t, int64(len(img)), map[int64][]byte{
		2*testChunk + 2*testPage: page,
	})
	diff, err := WriteDiffLayer(diffPath, WriteOptions{LayerID: "p2", Parent: "p1"}, parent, sink, sink)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Chunks) != 1 || diff.Chunks[0].Index != 2 {
		t.Fatalf("diff chunks = %+v, want exactly chunk 2", diff.Chunks)
	}

	want := append([]byte(nil), img...)
	copy(want[2*testChunk+2*testPage:], page)
	v, err := Resolve([]*Manifest{full, diff})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reconstruct(t, v, sink), want) {
		t.Fatal("partial-dirty merge lost parent content")
	}
}

// A diff on top of a diff must merge against the RESOLVED chain view, not
// just the root layer.
func TestWriteDiffLayerChainMerge(t *testing.T) {
	needsSparse(t)
	img := buildTestImage(t)
	sink := newMemSink()
	full, err := WriteLayer(writeTemp(t, img), WriteOptions{LayerID: "p1"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	v1, err := Resolve([]*Manifest{full})
	if err != nil {
		t.Fatal(err)
	}

	pageA := bytes.Repeat([]byte("1"), testPage)
	d1Path := writeSparse(t, int64(len(img)), map[int64][]byte{2 * testChunk: pageA})
	d1, err := WriteDiffLayer(d1Path, WriteOptions{LayerID: "p2", Parent: "p1"}, v1, sink, sink)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := Resolve([]*Manifest{full, d1})
	if err != nil {
		t.Fatal(err)
	}

	pageB := bytes.Repeat([]byte("2"), testPage)
	d2Path := writeSparse(t, int64(len(img)), map[int64][]byte{2*testChunk + 3*testPage: pageB})
	d2, err := WriteDiffLayer(d2Path, WriteOptions{LayerID: "p3", Parent: "p2"}, v2, sink, sink)
	if err != nil {
		t.Fatal(err)
	}

	want := append([]byte(nil), img...)
	copy(want[2*testChunk:], pageA)
	copy(want[2*testChunk+3*testPage:], pageB)
	v3, err := Resolve([]*Manifest{d2, full, d1})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reconstruct(t, v3, sink), want) {
		t.Fatal("chain merge lost an intermediate layer's dirty page")
	}
}

func TestWriteDiffLayerRequiresParent(t *testing.T) {
	img := make([]byte, testChunk)
	path := writeTemp(t, img)
	if _, err := WriteDiffLayer(path, WriteOptions{LayerID: "p2"}, nil, nil, newMemSink()); err == nil {
		t.Fatal("WriteDiffLayer accepted empty Parent")
	}
	if _, err := WriteDiffLayer(path, WriteOptions{LayerID: "p2", Parent: "p1"}, nil, nil, newMemSink()); err == nil {
		t.Fatal("WriteDiffLayer accepted nil parent view/getter")
	}
}
