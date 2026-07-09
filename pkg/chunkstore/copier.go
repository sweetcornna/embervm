package chunkstore

import (
	"context"
	"errors"
	"fmt"
	"io"

	"golang.org/x/sync/errgroup"

	"github.com/embervm/embervm/pkg/metrics"
)

// DefaultParallel bounds concurrent chunk transfers (并行上传下载,
// docs/zh/03 §3 M2).
const DefaultParallel = 16

// Copier moves chunks between stores (write-through to L1 on pause,
// backfill from L1 on restore).
type Copier struct {
	Src      Store
	Dst      Store
	Parallel int // <= 0 means DefaultParallel
}

// Copy transfers the given chunks, skipping ones Dst already has.
// It returns how many chunks were actually written (the rest were dedup
// hits) and fails fast on the first error.
func (c Copier) Copy(ctx context.Context, hashes []string) (int, error) {
	par := c.Parallel
	if par <= 0 {
		par = DefaultParallel
	}
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(par)
	copied := make(chan int, len(hashes))
	// A manifest can reference one content hash at MANY chunk indices
	// (repeated memory pages), so the list carries duplicates. Launching
	// them all races the same key against itself — concurrent Put+Touch of
	// one object makes MinIO's self-copy transiently observe NoSuchKey —
	// and is wasted round-trips besides. One transfer per distinct hash.
	seen := make(map[string]struct{}, len(hashes))
	unique := 0
	for _, hash := range hashes {
		if _, dup := seen[hash]; dup {
			continue
		}
		seen[hash] = struct{}{}
		unique++
		g.Go(func() error {
			ok, err := c.Dst.Has(ctx, hash)
			if err != nil {
				return err
			}
			if ok {
				// A dedup hit re-references an existing chunk whose mtime
				// may be ancient, and the manifest that will reference it
				// lands only after Copy returns. Refresh the chunk's GC
				// clock or a concurrent sweep could delete it before the
				// manifest becomes a mark root (gc.go safety argument).
				toucher, can := c.Dst.(Toucher)
				if !can {
					return nil
				}
				err := toucher.TouchChunk(ctx, hash)
				if err == nil {
					return nil
				}
				if !errors.Is(err, ErrNotFound) {
					return fmt.Errorf("touch chunk %s: %w", hash, err)
				}
				// Gone between Has and Touch: a GC sweep we lost to, or a
				// concurrent writer overwriting the same immutable content
				// (another sandbox's pause). Fall through and upload — a
				// double PUT of identical bytes is safe and leaves exactly
				// the fresh mtime the touch wanted.
			}
			rc, err := c.Src.Get(ctx, hash)
			if err != nil {
				return err
			}
			defer rc.Close()
			// Size -1: Dir streams any length; S3 falls back to a
			// multipart-capable upload for unknown sizes.
			written, err := c.Dst.Put(ctx, hash, rc, sizeOf(rc))
			if err != nil {
				return fmt.Errorf("copy chunk %s: %w", hash, err)
			}
			if written {
				copied <- 1
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return 0, err
	}
	close(copied)
	n := 0
	for range copied {
		n++
	}
	metrics.ChunkOps.WithLabelValues("put").Add(float64(n))
	metrics.ChunkOps.WithLabelValues("dedup_hit").Add(float64(unique - n))
	return n, nil
}

// Missing returns the subset of hashes the store does not have, preserving
// order (restore uses it to plan WS-first backfill).
func Missing(ctx context.Context, s Store, hashes []string) ([]string, error) {
	var out []string
	for _, h := range hashes {
		ok, err := s.Has(ctx, h)
		if err != nil {
			return nil, err
		}
		if !ok {
			out = append(out, h)
		}
	}
	return out, nil
}

// sizeOf recovers the exact size from seekable readers (both backends
// return them: *os.File and *minio.Object); otherwise -1, which Put
// handles with a streaming upload.
func sizeOf(r io.Reader) int64 {
	s, ok := r.(io.Seeker)
	if !ok {
		return -1
	}
	end, err := s.Seek(0, io.SeekEnd)
	if err != nil {
		return -1
	}
	if _, err := s.Seek(0, io.SeekStart); err != nil {
		return -1
	}
	return end
}
