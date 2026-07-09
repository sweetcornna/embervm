package chunkstore

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"
)

// ageChunk backdates a chunk's mtime so it falls outside any grace window.
func ageChunk(t *testing.T, d *Dir, hash string, age time.Duration) {
	t.Helper()
	path, err := d.chunkPath(hash)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-age)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

// TestGCSparesDedupTouchedChunk reproduces the pause-vs-GC race: a pause
// dedup-hits an old chunk (Copier skips the upload) and only later writes
// the manifest referencing it. The Copier's touch must pull the chunk back
// inside the grace window, or GC sweeps it and the manifest dangles.
func TestGCSparesDedupTouchedChunk(t *testing.T) {
	dst := newTestDir(t)
	src := newTestDir(t)
	ctx := context.Background()

	putChunk(t, src, "aa111", "shared-bytes")
	putChunk(t, dst, "aa111", "shared-bytes")
	ageChunk(t, dst, "aa111", 2*time.Hour) // its old referencing manifest is gone

	// The in-flight pause: chunks first (dedup hit), manifest later.
	if _, err := (Copier{Src: src, Dst: dst}).Copy(ctx, []string{"aa111"}); err != nil {
		t.Fatalf("Copy: %v", err)
	}

	res, err := GC(ctx, dst, time.Hour)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if res.SweptChunks != 0 || res.SkippedNew != 1 {
		t.Fatalf("GC swept=%d skippedNew=%d, want 0/1 (dedup touch must protect the chunk)", res.SweptChunks, res.SkippedNew)
	}
	if ok, _ := dst.Has(ctx, "aa111"); !ok {
		t.Fatal("dedup-hit chunk was swept before its manifest landed")
	}

	// The manifest lands; the next pass must keep treating it as live.
	putManifest(t, dst, "sandboxes/sb-b/layer-p1.json", "aa111")
	if _, err := GC(ctx, dst, time.Hour); err != nil {
		t.Fatal(err)
	}
	if ok, _ := dst.Has(ctx, "aa111"); !ok {
		t.Fatal("referenced chunk was swept")
	}
}

// vanishingManifests simulates a manifest recycled between ListObjectKeys
// and GetObject: reads of the named key return ErrNotFound.
type vanishingManifests struct {
	*Dir
	gone string
}

func (v *vanishingManifests) GetObject(ctx context.Context, key string) (io.ReadCloser, error) {
	if key == v.gone {
		return nil, fmt.Errorf("object %s: %w", key, ErrNotFound)
	}
	return v.Dir.GetObject(ctx, key)
}

// TestGCSkipsVanishedManifest: a manifest deleted mid-pass (recycle churn)
// must be skipped, not abort the whole run — an aborted run leaks garbage
// forever on stores with steady churn.
func TestGCSkipsVanishedManifest(t *testing.T) {
	d := newTestDir(t)
	ctx := context.Background()

	putChunk(t, d, "aa111", "live")
	putChunk(t, d, "bb222", "garbage")
	ageChunk(t, d, "aa111", 2*time.Hour)
	ageChunk(t, d, "bb222", 2*time.Hour)
	putManifest(t, d, "sandboxes/sb1/layer-p1.json", "aa111")
	putManifest(t, d, "sandboxes/sb2/layer-p1.json", "bb222")

	b := &vanishingManifests{Dir: d, gone: "sandboxes/sb2/layer-p1.json"}
	res, err := GC(ctx, b, time.Hour)
	if err != nil {
		t.Fatalf("GC aborted on a vanished manifest: %v", err)
	}
	if res.Manifests != 1 {
		t.Fatalf("Manifests = %d, want 1 (the surviving one)", res.Manifests)
	}
	if ok, _ := d.Has(ctx, "aa111"); !ok {
		t.Fatal("chunk referenced by the surviving manifest was swept")
	}
	// bb222's only manifest vanished, so it is garbage — swept.
	if ok, _ := d.Has(ctx, "bb222"); ok {
		t.Fatal("orphaned chunk survived the sweep")
	}
}

// touchAfterList simulates a pause touching a dedup-hit chunk between GC's
// ListChunks and its sweep: the listing carries a stale (old) mtime.
type touchAfterList struct {
	*Dir
	hash string
}

func (b *touchAfterList) ListChunks(ctx context.Context) ([]ChunkInfo, error) {
	chunks, err := b.Dir.ListChunks(ctx)
	if err != nil {
		return nil, err
	}
	if err := b.Dir.TouchChunk(ctx, b.hash); err != nil {
		return nil, err
	}
	return chunks, nil
}

// TestGCDeleteTimeRecheck: the sweep must re-stat each candidate — the
// listing's mtime is stale by construction, and a chunk touched after
// ListChunks must survive.
func TestGCDeleteTimeRecheck(t *testing.T) {
	d := newTestDir(t)
	ctx := context.Background()

	putChunk(t, d, "cc333", "touched-in-flight")
	ageChunk(t, d, "cc333", 2*time.Hour)

	b := &touchAfterList{Dir: d, hash: "cc333"}
	res, err := GC(ctx, b, time.Hour)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if res.SweptChunks != 0 {
		t.Fatalf("SweptChunks = %d, want 0 (delete-time re-stat must see the touch)", res.SweptChunks)
	}
	if ok, _ := d.Has(ctx, "cc333"); !ok {
		t.Fatal("chunk touched after ListChunks was swept anyway")
	}
}

// TestDurableDirWrites: the fsync variant must behave identically at the
// API level (durability itself is not testable here).
func TestDurableDirWrites(t *testing.T) {
	d, err := NewDurableDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	putChunk(t, d, "dd444", "durable")
	rc, err := d.Get(ctx, "dd444")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != "durable" {
		t.Fatalf("read back %q", data)
	}
	if err := d.PutObject(ctx, "sandboxes/x/layer-p1.json", bytesReader([]byte("{}")), 2); err != nil {
		t.Fatal(err)
	}
}

// TestTouchChunkRefreshesMtime pins the Toucher contract on the Dir backend.
func TestTouchChunkRefreshesMtime(t *testing.T) {
	d := newTestDir(t)
	ctx := context.Background()
	putChunk(t, d, "ee555", "x")
	ageChunk(t, d, "ee555", time.Hour)

	before, err := d.StatChunk(ctx, "ee555")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.TouchChunk(ctx, "ee555"); err != nil {
		t.Fatal(err)
	}
	after, err := d.StatChunk(ctx, "ee555")
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime.After(before.ModTime) {
		t.Fatalf("mtime not refreshed: %v -> %v", before.ModTime, after.ModTime)
	}
}
