// Package memsnap defines EmberVM's chunked snapshot-memory format (M2).
//
// A guest memory file is split into fixed-size chunks (16 KiB = 4 guest
// pages). Each chunk is content-addressed by the SHA-256 of its uncompressed
// bytes, compressed independently with lz4 (kept raw when incompressible),
// and all-zero chunks are recorded but never stored. A snapshot is a chain
// of layers: one "full" root plus zero or more "diff" layers, each described
// by a Manifest. Fixed-size (not content-defined) chunking is deliberate: a
// page fault must map a memory offset to a chunk in O(1).
package memsnap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pierrec/lz4/v4"
)

const (
	// FormatVersion is snapshot_format_version for manifests this package writes.
	FormatVersion = 1
	// DefaultChunkSize is 4 guest pages; docs/zh/03 §3 mandates 8-16 KiB.
	DefaultChunkSize = 16 * 1024

	KindFull = "full"
	KindDiff = "diff"

	CodecLZ4 = "lz4"
	CodecRaw = "raw"
)

// ChunkRef locates and describes one chunk of guest memory.
// Field names are the wire format; consumers mirror the producer exactly.
type ChunkRef struct {
	Index int    `json:"i"`
	Hash  string `json:"h,omitempty"` // sha256 hex of the UNCOMPRESSED bytes; empty for zero chunks
	Zero  bool   `json:"z,omitempty"`
	Codec string `json:"c,omitempty"`  // "lz4" | "raw"; empty for zero chunks
	ULen  int    `json:"ul"`           // uncompressed length (== chunk size except the last chunk)
	CLen  int    `json:"cl,omitempty"` // stored length (== ULen when raw, 0 when zero)
}

// Manifest describes one snapshot layer.
type Manifest struct {
	FormatVersion int        `json:"format_version"`
	LayerID       string     `json:"layer_id"`
	Parent        string     `json:"parent,omitempty"` // empty on the full root layer
	Kind          string     `json:"kind"`             // "full" | "diff"
	FCVersion     string     `json:"fc_version"`
	KernelVersion string     `json:"kernel_version"`
	MemSizeBytes  int64      `json:"mem_size_bytes"`
	ChunkSize     int        `json:"chunk_size"`
	CreatedAt     time.Time  `json:"created_at"`
	Chunks        []ChunkRef `json:"chunks"`
}

// ChunkCount returns how many chunks cover memSize bytes.
func ChunkCount(memSize int64, chunkSize int) int {
	if memSize <= 0 || chunkSize <= 0 {
		return 0
	}
	return int((memSize + int64(chunkSize) - 1) / int64(chunkSize))
}

// HashChunk returns the content address of uncompressed chunk bytes.
func HashChunk(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Validate checks internal consistency of a single layer manifest.
func (m *Manifest) Validate() error {
	if m.FormatVersion != FormatVersion {
		return fmt.Errorf("manifest %q: unsupported format_version %d", m.LayerID, m.FormatVersion)
	}
	if m.Kind != KindFull && m.Kind != KindDiff {
		return fmt.Errorf("manifest %q: bad kind %q", m.LayerID, m.Kind)
	}
	if m.Kind == KindFull && m.Parent != "" {
		return fmt.Errorf("manifest %q: full layer must not have a parent", m.LayerID)
	}
	if m.Kind == KindDiff && m.Parent == "" {
		return fmt.Errorf("manifest %q: diff layer requires a parent", m.LayerID)
	}
	if m.LayerID == "" {
		return fmt.Errorf("manifest: empty layer_id")
	}
	if m.ChunkSize <= 0 || m.MemSizeBytes <= 0 {
		return fmt.Errorf("manifest %q: chunk_size/mem_size_bytes must be positive", m.LayerID)
	}
	n := ChunkCount(m.MemSizeBytes, m.ChunkSize)
	seen := make(map[int]bool, len(m.Chunks))
	for _, c := range m.Chunks {
		if c.Index < 0 || c.Index >= n {
			return fmt.Errorf("manifest %q: chunk index %d out of range [0,%d)", m.LayerID, c.Index, n)
		}
		if seen[c.Index] {
			return fmt.Errorf("manifest %q: duplicate chunk index %d", m.LayerID, c.Index)
		}
		seen[c.Index] = true
		if c.Zero {
			if c.Hash != "" || c.Codec != "" || c.CLen != 0 {
				return fmt.Errorf("manifest %q: zero chunk %d carries hash/codec/clen", m.LayerID, c.Index)
			}
		} else {
			if c.Hash == "" {
				return fmt.Errorf("manifest %q: chunk %d missing hash", m.LayerID, c.Index)
			}
			if c.Codec != CodecLZ4 && c.Codec != CodecRaw {
				return fmt.Errorf("manifest %q: chunk %d bad codec %q", m.LayerID, c.Index, c.Codec)
			}
			if c.CLen <= 0 {
				return fmt.Errorf("manifest %q: chunk %d bad clen %d", m.LayerID, c.Index, c.CLen)
			}
		}
		if c.ULen <= 0 || c.ULen > m.ChunkSize {
			return fmt.Errorf("manifest %q: chunk %d bad ulen %d", m.LayerID, c.Index, c.ULen)
		}
	}
	if m.Kind == KindFull && len(m.Chunks) != n {
		return fmt.Errorf("manifest %q: full layer has %d chunks, want %d", m.LayerID, len(m.Chunks), n)
	}
	return nil
}

// WriteFile atomically persists the manifest as JSON.
func (m *Manifest) WriteFile(path string) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal manifest %q: %w", m.LayerID, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ParseManifest decodes and validates a manifest from raw JSON (object
// stores hand back bytes, not paths).
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// ReadManifest loads and validates a layer manifest.
func ReadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", filepath.Base(path), err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Decode turns a chunk's stored bytes back into uncompressed memory bytes.
// Zero chunks have no stored bytes; callers handle them without Decode.
func Decode(ref ChunkRef, stored []byte) ([]byte, error) {
	if ref.Zero {
		return nil, fmt.Errorf("chunk %d: Decode called on a zero chunk", ref.Index)
	}
	if len(stored) != ref.CLen {
		return nil, fmt.Errorf("chunk %d: stored %d bytes, manifest says %d", ref.Index, len(stored), ref.CLen)
	}
	switch ref.Codec {
	case CodecRaw:
		return stored, nil
	case CodecLZ4:
		out := make([]byte, ref.ULen)
		n, err := lz4.UncompressBlock(stored, out)
		if err != nil {
			return nil, fmt.Errorf("chunk %d: lz4 decode: %w", ref.Index, err)
		}
		if n != ref.ULen {
			return nil, fmt.Errorf("chunk %d: lz4 decoded %d bytes, want %d", ref.Index, n, ref.ULen)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("chunk %d: unknown codec %q", ref.Index, ref.Codec)
	}
}
