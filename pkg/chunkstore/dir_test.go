package chunkstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
)

const testHash = "ab1234567890deadbeef"

func newTestDir(t *testing.T) *Dir {
	t.Helper()
	d, err := NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// backendContract runs the shared Store+Objects behavior tests.
func backendContract(t *testing.T, b Backend) {
	t.Helper()
	ctx := context.Background()
	data := []byte("stored chunk bytes")

	written, err := b.Put(ctx, testHash, bytes.NewReader(data), int64(len(data)))
	if err != nil || !written {
		t.Fatalf("first Put = %v, %v; want written", written, err)
	}
	written, err = b.Put(ctx, testHash, bytes.NewReader(data), int64(len(data)))
	if err != nil || written {
		t.Fatalf("second Put = %v, %v; want dedup", written, err)
	}
	ok, err := b.Has(ctx, testHash)
	if err != nil || !ok {
		t.Fatalf("Has = %v, %v; want true", ok, err)
	}
	rc, err := b.Get(ctx, testHash)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Get = %q, %v", got, err)
	}

	if _, err := b.Get(ctx, "ffdoesnotexist"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}
	ok, err = b.Has(ctx, "ffdoesnotexist")
	if err != nil || ok {
		t.Fatalf("Has missing = %v, %v; want false", ok, err)
	}

	if err := b.Delete(ctx, testHash); err != nil {
		t.Fatal(err)
	}
	if ok, _ := b.Has(ctx, testHash); ok {
		t.Fatal("chunk still present after Delete")
	}
	if err := b.Delete(ctx, testHash); err != nil {
		t.Fatalf("Delete of missing chunk = %v, want nil", err)
	}

	// Named objects.
	meta := []byte(`{"layer_id":"p1"}`)
	if err := b.PutObject(ctx, "sandboxes/sb1/layer-p1.json", bytes.NewReader(meta), int64(len(meta))); err != nil {
		t.Fatal(err)
	}
	ok, err = b.HasObject(ctx, "sandboxes/sb1/layer-p1.json")
	if err != nil || !ok {
		t.Fatalf("HasObject = %v, %v; want true", ok, err)
	}
	rc, err = b.GetObject(ctx, "sandboxes/sb1/layer-p1.json")
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, meta) {
		t.Fatalf("GetObject = %q", got)
	}
	if _, err := b.GetObject(ctx, "sandboxes/sb1/absent.json"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetObject missing = %v, want ErrNotFound", err)
	}
	if err := b.DeleteObject(ctx, "sandboxes/sb1/layer-p1.json"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := b.HasObject(ctx, "sandboxes/sb1/layer-p1.json"); ok {
		t.Fatal("object still present after DeleteObject")
	}
	if err := b.DeleteObject(ctx, "sandboxes/sb1/layer-p1.json"); err != nil {
		t.Fatalf("DeleteObject of absent key = %v, want nil", err)
	}
	if err := b.PutObject(ctx, "sandboxes/sb1/layer-p1.json", bytes.NewReader(meta), int64(len(meta))); err != nil {
		t.Fatal(err)
	}

	// Overwrite is allowed for named objects (manifests are immutable by
	// convention, ws.json is rolling-updated).
	meta2 := []byte(`{"layer_id":"p1","v":2}`)
	if err := b.PutObject(ctx, "sandboxes/sb1/layer-p1.json", bytes.NewReader(meta2), int64(len(meta2))); err != nil {
		t.Fatal(err)
	}
	rc, _ = b.GetObject(ctx, "sandboxes/sb1/layer-p1.json")
	got, _ = io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, meta2) {
		t.Fatalf("overwritten GetObject = %q", got)
	}
}

func TestDirBackendContract(t *testing.T) {
	backendContract(t, newTestDir(t))
}

func TestDirRejectsBadNames(t *testing.T) {
	d := newTestDir(t)
	ctx := context.Background()
	for _, h := range []string{"", "ab", "../escape", "a/b"} {
		if _, err := d.Put(ctx, h, bytes.NewReader(nil), 0); err == nil {
			t.Errorf("Put accepted bad hash %q", h)
		}
	}
	for _, k := range []string{"", "../escape", "/abs"} {
		if err := d.PutObject(ctx, k, bytes.NewReader(nil), 0); err == nil {
			t.Errorf("PutObject accepted bad key %q", k)
		}
	}
}

func TestDirPutSizeMismatch(t *testing.T) {
	d := newTestDir(t)
	_, err := d.Put(context.Background(), testHash, bytes.NewReader([]byte("abc")), 99)
	if err == nil {
		t.Fatal("Put accepted size mismatch")
	}
	if ok, _ := d.Has(context.Background(), testHash); ok {
		t.Fatal("failed Put left a chunk behind")
	}
}

func TestDirConcurrentPutSameHash(t *testing.T) {
	d := newTestDir(t)
	ctx := context.Background()
	data := bytes.Repeat([]byte("C"), 1024)
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := d.Put(ctx, testHash, bytes.NewReader(data), int64(len(data))); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	rc, err := d.Get(ctx, testHash)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("concurrent Put corrupted the chunk")
	}
}

func TestDirCanceledContext(t *testing.T) {
	d := newTestDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Put(ctx, testHash, bytes.NewReader(nil), 0); err == nil {
		t.Fatal("Put ignored canceled context")
	}
	if _, err := d.Get(ctx, testHash); err == nil {
		t.Fatal("Get ignored canceled context")
	}
}

func TestBytesAdapter(t *testing.T) {
	d := newTestDir(t)
	b := Bytes{Ctx: context.Background(), S: d}
	written, err := b.Put(testHash, []byte("chunk"))
	if err != nil || !written {
		t.Fatalf("Bytes.Put = %v, %v", written, err)
	}
	got, err := b.Get(testHash)
	if err != nil || string(got) != "chunk" {
		t.Fatalf("Bytes.Get = %q, %v", got, err)
	}
	if _, err := b.Get("ffmissing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Bytes.Get missing = %v", err)
	}
}

// fakeStore lets copier tests inject failures.
type fakeStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	failGet map[string]bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: map[string][]byte{}, failGet: map[string]bool{}}
}

func (f *fakeStore) Put(ctx context.Context, hash string, r io.Reader, size int64) (bool, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.objects[hash]; ok {
		return false, nil
	}
	f.objects[hash] = data
	return true, nil
}

func (f *fakeStore) Get(ctx context.Context, hash string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failGet[hash] {
		return nil, fmt.Errorf("chunk %s: injected failure", hash)
	}
	data, ok := f.objects[hash]
	if !ok {
		return nil, fmt.Errorf("chunk %s: %w", hash, ErrNotFound)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *fakeStore) Has(ctx context.Context, hash string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[hash]
	return ok, nil
}

func (f *fakeStore) Delete(ctx context.Context, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, hash)
	return nil
}
