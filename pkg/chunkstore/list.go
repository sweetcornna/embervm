package chunkstore

import (
	"context"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
)

// ChunkInfo describes one stored chunk for GC decisions.
type ChunkInfo struct {
	Hash    string
	ModTime time.Time
}

// Lister enumerates a backend's contents. Both backends implement it; the
// chunk GC (gc.go) is the consumer.
type Lister interface {
	// ListChunks enumerates every stored chunk.
	ListChunks(ctx context.Context) ([]ChunkInfo, error)
	// ListObjectKeys enumerates named-object keys under prefix (caller
	// namespace, e.g. "sandboxes/"); "" lists everything.
	ListObjectKeys(ctx context.Context, prefix string) ([]string, error)
}

// ListingBackend is a Backend that can also be enumerated (GC target).
type ListingBackend interface {
	Backend
	Lister
}

var (
	_ ListingBackend = (*Dir)(nil)
	_ ListingBackend = (*S3)(nil)
)

// --- Dir ---------------------------------------------------------------------

func (d *Dir) ListChunks(ctx context.Context) ([]ChunkInfo, error) {
	var out []ChunkInfo
	root := filepath.Join(d.root, "objects")
	err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if strings.HasPrefix(e.Name(), ".tmp-") {
			return nil // in-flight write
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		out = append(out, ChunkInfo{Hash: e.Name(), ModTime: info.ModTime()})
		return nil
	})
	return out, err
}

func (d *Dir) ListObjectKeys(ctx context.Context, prefix string) ([]string, error) {
	var out []string
	root := filepath.Join(d.root, "meta")
	err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if strings.HasPrefix(e.Name(), ".tmp-") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if strings.HasPrefix(key, prefix) {
			out = append(out, key)
		}
		return nil
	})
	return out, err
}

// --- S3 ----------------------------------------------------------------------

func (s *S3) ListChunks(ctx context.Context) ([]ChunkInfo, error) {
	base := path.Join(s.cfg.Prefix, "objects") + "/"
	var out []ChunkInfo
	for obj := range s.c.ListObjects(ctx, s.cfg.Bucket, minio.ListObjectsOptions{
		Prefix: base, Recursive: true,
	}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		out = append(out, ChunkInfo{Hash: path.Base(obj.Key), ModTime: obj.LastModified})
	}
	return out, nil
}

func (s *S3) ListObjectKeys(ctx context.Context, prefix string) ([]string, error) {
	base := path.Join(s.cfg.Prefix, "meta") + "/"
	var out []string
	for obj := range s.c.ListObjects(ctx, s.cfg.Bucket, minio.ListObjectsOptions{
		Prefix: base + prefix, Recursive: true,
	}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		out = append(out, strings.TrimPrefix(obj.Key, base))
	}
	return out, nil
}
