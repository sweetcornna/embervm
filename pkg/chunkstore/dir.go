package chunkstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// Dir is the node-local backend: chunks under objects/<hh>/<hash>, named
// blobs under meta/<key>. Writes are tmp+rename so concurrent writers of
// the same chunk are safe (identical content, last rename wins).
type Dir struct {
	root    string
	durable bool // fsync file + parent dir on every write
}

func NewDir(root string) (*Dir, error) {
	for _, sub := range []string{"objects", "meta"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			return nil, err
		}
	}
	return &Dir{root: root}, nil
}

// NewDurableDir is a Dir that fsyncs the file and its parent directory on
// every write, so a rename cannot outlive its data across power loss (a
// valid-named but empty chunk would surface as a restore-time hash
// mismatch). Use it for L1/cold roles — the write-through RPO target;
// the node-local cache keeps the fast path since its contents are
// re-fetchable.
func NewDurableDir(root string) (*Dir, error) {
	d, err := NewDir(root)
	if err != nil {
		return nil, err
	}
	d.durable = true
	return d, nil
}

func (d *Dir) chunkPath(hash string) (string, error) {
	if len(hash) < 3 || strings.ContainsAny(hash, "/.") {
		return "", fmt.Errorf("chunkstore: bad chunk hash %q", hash)
	}
	return filepath.Join(d.root, "objects", hash[:2], hash), nil
}

func (d *Dir) objectPath(key string) (string, error) {
	if key == "" || strings.Contains(key, "..") || strings.HasPrefix(key, "/") {
		return "", fmt.Errorf("chunkstore: bad object key %q", key)
	}
	return filepath.Join(d.root, "meta", filepath.FromSlash(key)), nil
}

func writeAtomic(path string, r io.Reader, size int64, durable bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, r)
	if err == nil && durable {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	// size < 0 means "unknown, stream to EOF" (pipes): the length check is
	// then unavailable and truncation detection falls to the reader side.
	if size >= 0 && n != size {
		return fmt.Errorf("chunkstore: wrote %d bytes, expected %d", n, size)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if durable {
		return syncDir(filepath.Dir(path))
	}
	return nil
}

// syncDir fsyncs a directory so a completed rename survives power loss.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (d *Dir) Put(ctx context.Context, hash string, r io.Reader, size int64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	path, err := d.chunkPath(hash)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(path); err == nil {
		return false, nil // dedup
	}
	if err := writeAtomic(path, r, size, d.durable); err != nil {
		return false, err
	}
	return true, nil
}

// TouchChunk refreshes a chunk's modification time — its GC clock. The
// pause write-through touches dedup hits so the GC grace window covers
// chunks a new manifest re-references, not only chunks it uploads (the
// gc.go safety argument). A missing chunk surfaces as ErrNotFound.
func (d *Dir) TouchChunk(ctx context.Context, hash string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := d.chunkPath(hash)
	if err != nil {
		return err
	}
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("chunk %s: touch: %w", hash, ErrNotFound)
		}
		return err
	}
	return nil
}

// StatChunk returns a chunk's current ChunkInfo — GC's delete-time re-check.
func (d *Dir) StatChunk(ctx context.Context, hash string) (ChunkInfo, error) {
	if err := ctx.Err(); err != nil {
		return ChunkInfo{}, err
	}
	path, err := d.chunkPath(hash)
	if err != nil {
		return ChunkInfo{}, err
	}
	fi, err := os.Stat(path)
	if os.IsNotExist(err) {
		return ChunkInfo{}, fmt.Errorf("chunk %s: %w", hash, ErrNotFound)
	}
	if err != nil {
		return ChunkInfo{}, err
	}
	return ChunkInfo{Hash: hash, ModTime: fi.ModTime()}, nil
}

func (d *Dir) Get(ctx context.Context, hash string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := d.chunkPath(hash)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("chunk %s: %w", hash, ErrNotFound)
	}
	return f, err
}

func (d *Dir) Has(ctx context.Context, hash string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	path, err := d.chunkPath(hash)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func (d *Dir) Delete(ctx context.Context, hash string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := d.chunkPath(hash)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (d *Dir) PutObject(ctx context.Context, key string, r io.Reader, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := d.objectPath(key)
	if err != nil {
		return err
	}
	return writeAtomic(path, r, size, d.durable)
}

func (d *Dir) GetObject(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := d.objectPath(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("object %s: %w", key, ErrNotFound)
	}
	return f, err
}

func (d *Dir) DeleteObject(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := d.objectPath(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (d *Dir) HasObject(ctx context.Context, key string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	path, err := d.objectPath(key)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}
