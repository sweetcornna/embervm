package chunkstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/embervm/embervm/pkg/metrics"
)

// GCResult reports one mark-and-sweep pass.
type GCResult struct {
	Manifests   int // layer manifests read (the mark roots)
	LiveChunks  int // distinct hashes referenced by any manifest
	SweptChunks int // unreferenced chunks deleted
	SkippedNew  int // unreferenced but younger than the grace window
}

// layerRefs mirrors just the fields GC needs from a pkg/memsnap manifest
// (producer is the source of truth; unknown fields are ignored).
type layerRefs struct {
	Chunks []struct {
		Hash string `json:"h"`
		Zero bool   `json:"z"`
	} `json:"chunks"`
}

// GC deletes chunks no layer manifest references (mark-and-sweep over one
// backend). Roots are every `sandboxes/*/layer-*.json` object. Safety: the
// manifests are listed BEFORE the chunks, and unreferenced chunks younger
// than grace survive the sweep. An in-flight pause uploads chunks before
// its manifest, and every chunk it merely re-references (dedup hit) gets
// its mtime refreshed by the Copier — so the grace window covers BOTH the
// fresh uploads and the re-references. Because the touch can land after
// ListChunks, each sweep candidate is re-stat'ed at delete time. Run it
// after RECYCLED transitions (the engine does) or standalone.
func GC(ctx context.Context, b ListingBackend, grace time.Duration) (GCResult, error) {
	var res GCResult
	keys, err := b.ListObjectKeys(ctx, "sandboxes/")
	if err != nil {
		return res, fmt.Errorf("gc: list manifests: %w", err)
	}
	live := map[string]bool{}
	for _, key := range keys {
		base := key[strings.LastIndex(key, "/")+1:]
		if !strings.HasPrefix(base, "layer-") || !strings.HasSuffix(base, ".json") {
			continue
		}
		if err := markManifest(ctx, b, key, live); err != nil {
			if errors.Is(err, ErrNotFound) {
				// Recycled between list and read: its references die with
				// it. Aborting the pass here would let garbage pile up on
				// any store with churn.
				continue
			}
			return res, err
		}
		res.Manifests++
	}
	res.LiveChunks = len(live)

	chunks, err := b.ListChunks(ctx)
	if err != nil {
		return res, fmt.Errorf("gc: list chunks: %w", err)
	}
	stater, canStat := b.(ChunkStater)
	cutoff := time.Now().Add(-grace)
	for _, c := range chunks {
		if live[c.Hash] {
			continue
		}
		if c.ModTime.After(cutoff) {
			res.SkippedNew++
			continue
		}
		if canStat {
			// The listing's mtime is stale by construction; a pause's
			// dedup touch may have refreshed this chunk since. Only the
			// current mtime is authoritative for a destructive decision.
			cur, err := stater.StatChunk(ctx, c.Hash)
			if errors.Is(err, ErrNotFound) {
				continue // already gone
			}
			if err != nil {
				return res, fmt.Errorf("gc: recheck %s: %w", c.Hash, err)
			}
			if cur.ModTime.After(cutoff) {
				res.SkippedNew++
				continue
			}
		}
		if err := b.Delete(ctx, c.Hash); err != nil {
			return res, fmt.Errorf("gc: sweep %s: %w", c.Hash, err)
		}
		res.SweptChunks++
	}
	metrics.ChunkOps.WithLabelValues("gc_sweep").Add(float64(res.SweptChunks))
	return res, nil
}

func markManifest(ctx context.Context, b Objects, key string, live map[string]bool) error {
	rc, err := b.GetObject(ctx, key)
	if err != nil {
		return fmt.Errorf("gc: read manifest %s: %w", key, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	var refs layerRefs
	if err := json.Unmarshal(data, &refs); err != nil {
		return fmt.Errorf("gc: parse manifest %s: %w", key, err)
	}
	for _, c := range refs.Chunks {
		if !c.Zero && c.Hash != "" {
			live[c.Hash] = true
		}
	}
	return nil
}
