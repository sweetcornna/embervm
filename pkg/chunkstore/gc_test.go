package chunkstore

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func putChunk(t *testing.T, b Backend, hash, data string) {
	t.Helper()
	if _, err := b.Put(context.Background(), hash, bytes.NewReader([]byte(data)), int64(len(data))); err != nil {
		t.Fatal(err)
	}
}

func putManifest(t *testing.T, b Backend, key string, hashes ...string) {
	t.Helper()
	manifest := `{"layer_id":"x","chunks":[`
	for i, h := range hashes {
		if i > 0 {
			manifest += ","
		}
		manifest += fmt.Sprintf(`{"i":%d,"h":%q,"ul":16384,"cl":10,"c":"raw"}`, i, h)
	}
	manifest += `,{"i":99,"z":true,"ul":16384}]}`
	if err := b.PutObject(context.Background(), key, bytes.NewReader([]byte(manifest)), int64(len(manifest))); err != nil {
		t.Fatal(err)
	}
}

func TestDirListChunksAndKeys(t *testing.T) {
	d := newTestDir(t)
	ctx := context.Background()
	putChunk(t, d, "aa111", "x")
	putChunk(t, d, "bb222", "y")
	putManifest(t, d, "sandboxes/sb1/layer-p1.json", "aa111")
	putManifest(t, d, "templates/t1.meta", "bb222")

	chunks, err := d.ListChunks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var hashes []string
	for _, c := range chunks {
		hashes = append(hashes, c.Hash)
		if c.ModTime.IsZero() {
			t.Errorf("chunk %s has zero mtime", c.Hash)
		}
	}
	sort.Strings(hashes)
	if len(hashes) != 2 || hashes[0] != "aa111" || hashes[1] != "bb222" {
		t.Fatalf("ListChunks = %v", hashes)
	}

	keys, err := d.ListObjectKeys(ctx, "sandboxes/")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != "sandboxes/sb1/layer-p1.json" {
		t.Fatalf("ListObjectKeys(sandboxes/) = %v", keys)
	}
	all, err := d.ListObjectKeys(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("ListObjectKeys(\"\") = %v", all)
	}
}

func TestGCSweepsUnreferenced(t *testing.T) {
	d := newTestDir(t)
	ctx := context.Background()
	putChunk(t, d, "aa111", "live")
	putChunk(t, d, "bb222", "dead")
	putChunk(t, d, "cc333", "live-too")
	putManifest(t, d, "sandboxes/sb1/layer-p1.json", "aa111")
	putManifest(t, d, "sandboxes/sb2/layer-cold.json", "cc333")

	res, err := GC(ctx, d, 0) // zero grace: sweep immediately
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifests != 2 || res.LiveChunks != 2 || res.SweptChunks != 1 {
		t.Fatalf("GC result = %+v, want 2 manifests / 2 live / 1 swept", res)
	}
	for hash, want := range map[string]bool{"aa111": true, "bb222": false, "cc333": true} {
		ok, err := d.Has(ctx, hash)
		if err != nil {
			t.Fatal(err)
		}
		if ok != want {
			t.Errorf("after GC Has(%s) = %v, want %v", hash, ok, want)
		}
	}
}

func TestGCGraceProtectsFreshChunks(t *testing.T) {
	d := newTestDir(t)
	ctx := context.Background()
	// An in-flight pause: chunks uploaded, manifest not yet.
	putChunk(t, d, "dd444", "fresh-orphan")
	res, err := GC(ctx, d, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if res.SweptChunks != 0 || res.SkippedNew != 1 {
		t.Fatalf("GC result = %+v, want the fresh orphan skipped", res)
	}
	if ok, _ := d.Has(ctx, "dd444"); !ok {
		t.Fatal("grace window did not protect the fresh chunk")
	}

	// Age it past the window and it goes.
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(dirRoot(d), "objects", "dd", "dd444"), old, old); err != nil {
		t.Fatal(err)
	}
	res, err = GC(ctx, d, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if res.SweptChunks != 1 {
		t.Fatalf("aged orphan not swept: %+v", res)
	}
}

func TestGCIgnoresNonManifestObjects(t *testing.T) {
	d := newTestDir(t)
	ctx := context.Background()
	putChunk(t, d, "ee555", "x")
	// snapfiles / descriptors / ws must not confuse the marker.
	for _, key := range []string{
		"sandboxes/sb1/snapfile-p1", "sandboxes/sb1/snapshot.json", "sandboxes/sb1/ws.json",
	} {
		if err := d.PutObject(ctx, key, bytes.NewReader([]byte("not-a-manifest")), 14); err != nil {
			t.Fatal(err)
		}
	}
	res, err := GC(ctx, d, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifests != 0 || res.SweptChunks != 1 {
		t.Fatalf("GC result = %+v, want 0 manifests and the orphan swept", res)
	}
}

// dirRoot exposes the Dir root for test filesystem surgery.
func dirRoot(d *Dir) string { return d.root }
