// Package chunkstore stores snapshot chunks content-addressed by the
// SHA-256 of their uncompressed bytes (the address pkg/memsnap computes),
// plus named metadata objects (manifests, snapfiles, WS traces, disk
// streams). Two backends: a local directory (node-local cache, L0) and an
// S3-compatible object store (L1, e.g. MinIO/Garage/Hetzner OS).
package chunkstore

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is returned by Get/GetObject when the object does not exist.
var ErrNotFound = errors.New("chunkstore: object not found")

// Store is a content-addressed chunk store. Stored bytes are the chunk's
// wire form (lz4 or raw, per its manifest ChunkRef); the hash is over the
// UNCOMPRESSED bytes, so a Store cannot re-verify content by itself —
// integrity is checked at decode time against the manifest.
type Store interface {
	// Put stores the chunk unless it is already present. written=false
	// means the chunk was deduplicated.
	Put(ctx context.Context, hash string, r io.Reader, size int64) (written bool, err error)
	Get(ctx context.Context, hash string) (io.ReadCloser, error)
	Has(ctx context.Context, hash string) (bool, error)
	Delete(ctx context.Context, hash string) error
}

// Objects stores named (non-content-addressed) blobs: layer manifests,
// Firecracker snapfiles, WS traces, zfs send streams. Keys are
// caller-scoped paths like "sandboxes/<id>/layer-p1.json".
type Objects interface {
	PutObject(ctx context.Context, key string, r io.Reader, size int64) error
	GetObject(ctx context.Context, key string) (io.ReadCloser, error)
	HasObject(ctx context.Context, key string) (bool, error)
	// DeleteObject removes a named blob; deleting an absent key succeeds.
	DeleteObject(ctx context.Context, key string) error
}

// Backend is what both the local dir and S3 implementations provide.
type Backend interface {
	Store
	Objects
}

// Bytes adapts a Store to pkg/memsnap's byte-slice Sink/Getter interfaces.
type Bytes struct {
	Ctx context.Context
	S   Store
}

func (b Bytes) Put(hash string, stored []byte) (bool, error) {
	return b.S.Put(b.Ctx, hash, bytesReader(stored), int64(len(stored)))
}

func (b Bytes) Get(hash string) ([]byte, error) {
	rc, err := b.S.Get(b.Ctx, hash)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}
