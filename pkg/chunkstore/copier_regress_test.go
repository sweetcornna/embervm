package chunkstore

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
)

// countingStore wraps a Dir and counts Has/Put/Touch calls per hash.
type countingStore struct {
	*Dir
	has, put, touch atomic.Int64
	touchNotFound   bool // force TouchChunk to report ErrNotFound
}

func (c *countingStore) Has(ctx context.Context, hash string) (bool, error) {
	c.has.Add(1)
	return c.Dir.Has(ctx, hash)
}

func (c *countingStore) Put(ctx context.Context, hash string, r io.Reader, size int64) (bool, error) {
	c.put.Add(1)
	return c.Dir.Put(ctx, hash, r, size)
}

func (c *countingStore) TouchChunk(ctx context.Context, hash string) error {
	c.touch.Add(1)
	if c.touchNotFound {
		return fmt.Errorf("chunk %s: touch: %w", hash, ErrNotFound)
	}
	return c.Dir.TouchChunk(ctx, hash)
}

// TestCopierDeduplicatesBatch: a manifest referencing one content hash at
// many chunk indices hands Copy a list full of duplicates. They must
// collapse to ONE transfer — concurrent Put+Touch of the same key raced
// MinIO's self-copy into transient NoSuchKey (the e2e-m2 write-through
// failure) and wasted round-trips.
func TestCopierDeduplicatesBatch(t *testing.T) {
	src := newTestDir(t)
	dst := &countingStore{Dir: newTestDir(t)}
	ctx := context.Background()
	putChunk(t, src, "ff001", "repeated-content")

	hashes := make([]string, 32)
	for i := range hashes {
		hashes[i] = "ff001"
	}
	n, err := (Copier{Src: src, Dst: dst}).Copy(ctx, hashes)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if n != 1 {
		t.Fatalf("written = %d, want 1", n)
	}
	if got := dst.has.Load(); got != 1 {
		t.Fatalf("Has calls = %d, want 1 (duplicates must collapse)", got)
	}
	if got := dst.put.Load(); got != 1 {
		t.Fatalf("Put calls = %d, want 1", got)
	}
}

// TestCopierTouchNotFoundFallsBackToUpload: a chunk that vanishes between
// Has and Touch (GC sweep lost race, or a concurrent writer overwriting the
// same key) must be re-uploaded, not fail the pause and not be skipped —
// skipping would hand the manifest a dangling reference.
func TestCopierTouchNotFoundFallsBackToUpload(t *testing.T) {
	src := newTestDir(t)
	dst := &countingStore{Dir: newTestDir(t), touchNotFound: true}
	ctx := context.Background()
	putChunk(t, src, "ff002", "content")
	putChunk(t, dst.Dir, "ff002", "content") // Has sees it → dedup-hit path

	n, err := (Copier{Src: src, Dst: dst}).Copy(ctx, []string{"ff002"})
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if got := dst.touch.Load(); got != 1 {
		t.Fatalf("Touch calls = %d, want 1", got)
	}
	// The fallback re-put: Dir.Put dedups on the existing file, so written
	// stays 0, but the Put attempt must have happened.
	if got := dst.put.Load(); got != 1 {
		t.Fatalf("Put calls = %d, want 1 (fallback upload after touch NotFound)", got)
	}
	_ = n
}
