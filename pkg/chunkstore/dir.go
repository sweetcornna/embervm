package chunkstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// Dir is the node-local backend: chunks under objects/<hh>/<hash>, named
// blobs under meta/<key>. Writes are tmp+rename so concurrent writers of
// the same chunk are safe (identical content, last rename wins).
type Dir struct {
	root string
}

func NewDir(root string) (*Dir, error) {
	for _, sub := range []string{"objects", "meta"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			return nil, err
		}
	}
	return &Dir{root: root}, nil
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

func writeAtomic(path string, r io.Reader, size int64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if size >= 0 && n != size {
		return fmt.Errorf("chunkstore: wrote %d bytes, expected %d", n, size)
	}
	return os.Rename(tmp.Name(), path)
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
	if err := writeAtomic(path, r, size); err != nil {
		return false, err
	}
	return true, nil
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
	return writeAtomic(path, r, size)
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
