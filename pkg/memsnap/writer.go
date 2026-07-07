package memsnap

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/pierrec/lz4/v4"
)

// Sink receives chunk bytes in their stored form (lz4 or raw). Implemented
// by pkg/chunkstore; kept minimal here to avoid an import cycle. Put must be
// a no-op returning written=false when the hash is already present.
type Sink interface {
	Put(hash string, stored []byte) (written bool, err error)
}

// Getter fetches a chunk's stored bytes by content address (the read side
// of Sink). WriteDiffLayer needs it to merge partially-dirty chunks with
// their parent content.
type Getter interface {
	Get(hash string) ([]byte, error)
}

type extent struct {
	off, len int64
}

// WriteOptions parameterizes one snapshot layer.
type WriteOptions struct {
	LayerID       string
	Parent        string // required for WriteDiffLayer, empty for WriteLayer
	FCVersion     string
	KernelVersion string
	MemSizeBytes  int64 // guest memory size; defaults to the memfile size
	ChunkSize     int   // defaults to DefaultChunkSize
	CreatedAt     time.Time
}

// layerWriter carries the shared per-layer chunkify state.
type layerWriter struct {
	f     *os.File
	m     *Manifest
	sink  Sink
	buf   []byte // chunk read buffer
	comp  []byte // lz4 output buffer
	zeros []byte
}

func newLayerWriter(memfilePath string, opts WriteOptions, kind string, sink Sink) (*layerWriter, error) {
	f, err := os.Open(memfilePath)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	chunkSize := opts.ChunkSize
	if chunkSize == 0 {
		chunkSize = DefaultChunkSize
	}
	memSize := opts.MemSizeBytes
	if memSize == 0 {
		memSize = st.Size()
	}
	if st.Size() != memSize {
		f.Close()
		return nil, fmt.Errorf("write layer %q: memfile is %d bytes, expected %d", opts.LayerID, st.Size(), memSize)
	}
	createdAt := opts.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	return &layerWriter{
		f:    f,
		sink: sink,
		m: &Manifest{
			FormatVersion: FormatVersion,
			LayerID:       opts.LayerID,
			Parent:        opts.Parent,
			Kind:          kind,
			FCVersion:     opts.FCVersion,
			KernelVersion: opts.KernelVersion,
			MemSizeBytes:  memSize,
			ChunkSize:     chunkSize,
			CreatedAt:     createdAt,
		},
		buf:   make([]byte, chunkSize),
		comp:  make([]byte, lz4.CompressBlockBound(chunkSize)),
		zeros: make([]byte, chunkSize),
	}, nil
}

// chunkSpan returns a chunk's offset and its uncompressed length.
func (w *layerWriter) chunkSpan(i int) (off int64, ulen int) {
	off = int64(i) * int64(w.m.ChunkSize)
	ulen = w.m.ChunkSize
	if rem := w.m.MemSizeBytes - off; int64(ulen) > rem {
		ulen = int(rem)
	}
	return off, ulen
}

// emit hashes, compresses, stores, and records one chunk's final content.
func (w *layerWriter) emit(i int, content []byte) error {
	ulen := len(content)
	if bytes.Equal(content, w.zeros[:ulen]) {
		w.m.Chunks = append(w.m.Chunks, ChunkRef{Index: i, Zero: true, ULen: ulen})
		return nil
	}
	ref := ChunkRef{Index: i, Hash: HashChunk(content), ULen: ulen}
	clen, err := lz4.CompressBlock(content, w.comp, nil)
	if err != nil {
		return fmt.Errorf("write layer %q: lz4 chunk %d: %w", w.m.LayerID, i, err)
	}
	var stored []byte
	if clen > 0 && clen < ulen {
		ref.Codec, ref.CLen, stored = CodecLZ4, clen, w.comp[:clen]
	} else {
		ref.Codec, ref.CLen, stored = CodecRaw, ulen, content
	}
	if _, err := w.sink.Put(ref.Hash, stored); err != nil {
		return fmt.Errorf("write layer %q: store chunk %d: %w", w.m.LayerID, i, err)
	}
	w.m.Chunks = append(w.m.Chunks, ref)
	return nil
}

func (w *layerWriter) readChunk(i int) ([]byte, error) {
	off, ulen := w.chunkSpan(i)
	src := w.buf[:ulen]
	if _, err := io.ReadFull(io.NewSectionReader(w.f, off, int64(ulen)), src); err != nil {
		return nil, fmt.Errorf("write layer %q: read chunk %d: %w", w.m.LayerID, i, err)
	}
	return src, nil
}

func (w *layerWriter) finish() (*Manifest, error) {
	if err := w.m.Validate(); err != nil {
		return nil, err
	}
	return w.m, nil
}

// WriteLayer chunkifies a full Firecracker memory file into sink and returns
// the layer manifest.
func WriteLayer(memfilePath string, opts WriteOptions, sink Sink) (*Manifest, error) {
	if opts.Parent != "" {
		return nil, fmt.Errorf("write layer %q: full layer must not have a parent (use WriteDiffLayer)", opts.LayerID)
	}
	w, err := newLayerWriter(memfilePath, opts, KindFull, sink)
	if err != nil {
		return nil, err
	}
	defer w.f.Close()
	for i := 0; i < ChunkCount(w.m.MemSizeBytes, w.m.ChunkSize); i++ {
		src, err := w.readChunk(i)
		if err != nil {
			return nil, err
		}
		if err := w.emit(i, src); err != nil {
			return nil, err
		}
	}
	return w.finish()
}

// WriteDiffLayer chunkifies a sparse diff memory file (snapshot_type=Diff:
// Firecracker writes exactly the dirty 4 KiB pages, clean pages stay holes).
// Chunks are 4 pages, so a partially-dirty chunk is MERGED with its parent
// content at write time — dirty extents overlaid onto the parent chunk —
// and the manifest records whole self-contained chunks; restore stays a
// simple newest-wins lookup. parent is the resolved view of the chain this
// layer extends, get fetches parent chunk bytes (both required).
func WriteDiffLayer(memfilePath string, opts WriteOptions, parent *View, get Getter, sink Sink) (*Manifest, error) {
	if opts.Parent == "" {
		return nil, fmt.Errorf("write diff layer %q: Parent is required", opts.LayerID)
	}
	if parent == nil || get == nil {
		return nil, fmt.Errorf("write diff layer %q: parent view and getter are required", opts.LayerID)
	}
	w, err := newLayerWriter(memfilePath, opts, KindDiff, sink)
	if err != nil {
		return nil, err
	}
	defer w.f.Close()
	if parent.MemSizeBytes != w.m.MemSizeBytes || parent.ChunkSize != w.m.ChunkSize {
		return nil, fmt.Errorf("write diff layer %q: parent geometry (%d/%d) differs from diff (%d/%d)",
			opts.LayerID, parent.MemSizeBytes, parent.ChunkSize, w.m.MemSizeBytes, w.m.ChunkSize)
	}

	exts, err := dataExtents(w.f)
	if err != nil {
		return nil, fmt.Errorf("write diff layer %q: %w", opts.LayerID, err)
	}
	n := ChunkCount(w.m.MemSizeBytes, w.m.ChunkSize)
	dirty := make(map[int][]extent) // chunk index -> dirty ranges within the chunk
	for _, e := range exts {
		first := int(e.off / int64(w.m.ChunkSize))
		last := int((e.off + e.len - 1) / int64(w.m.ChunkSize))
		for i := first; i <= last && i < n; i++ {
			off, ulen := w.chunkSpan(i)
			lo, hi := e.off, e.off+e.len
			if lo < off {
				lo = off
			}
			if end := off + int64(ulen); hi > end {
				hi = end
			}
			if hi > lo {
				dirty[i] = append(dirty[i], extent{lo - off, hi - lo})
			}
		}
	}
	indices := make([]int, 0, len(dirty))
	for i := range dirty {
		indices = append(indices, i)
	}
	sort.Ints(indices)

	for _, i := range indices {
		_, ulen := w.chunkSpan(i)
		src, err := w.readChunk(i)
		if err != nil {
			return nil, err
		}
		ranges := dirty[i]
		wholeDirty := len(ranges) == 1 && ranges[0].off == 0 && ranges[0].len == int64(ulen)
		var content []byte
		if wholeDirty {
			content = src
		} else {
			base, err := parentChunk(parent, get, i, ulen)
			if err != nil {
				return nil, fmt.Errorf("write diff layer %q: %w", opts.LayerID, err)
			}
			for _, r := range ranges {
				copy(base[r.off:r.off+r.len], src[r.off:r.off+r.len])
			}
			content = base
		}
		if err := w.emit(i, content); err != nil {
			return nil, err
		}
	}
	return w.finish()
}

// parentChunk materializes chunk i of the parent view as mutable bytes.
func parentChunk(parent *View, get Getter, i, ulen int) ([]byte, error) {
	ref := parent.Chunks[i]
	if ref.Zero || ref.Hash == "" {
		return make([]byte, ulen), nil
	}
	stored, err := get.Get(ref.Hash)
	if err != nil {
		return nil, fmt.Errorf("parent chunk %d (%s): %w", i, ref.Hash, err)
	}
	data, err := Decode(ref, stored)
	if err != nil {
		return nil, fmt.Errorf("parent chunk %d: %w", i, err)
	}
	if len(data) != ulen {
		return nil, fmt.Errorf("parent chunk %d: %d bytes, want %d", i, len(data), ulen)
	}
	return append([]byte(nil), data...), nil
}
