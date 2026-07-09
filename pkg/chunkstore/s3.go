package chunkstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config locates the L1 object store. In CI this is a local MinIO; in
// production Garage/SeaweedFS/Hetzner OS per docs/zh/02 §2.3.
type S3Config struct {
	Endpoint  string // host:port, no scheme
	Bucket    string
	Prefix    string // optional key prefix, e.g. "embervm"
	AccessKey string
	SecretKey string
	Secure    bool
}

// S3EnvPrefix names the environment variables S3FromEnv reads:
// EMBERVM_L1_ENDPOINT, _BUCKET, _ACCESS_KEY, _SECRET_KEY, _PREFIX, _SECURE.
const S3EnvPrefix = "EMBERVM_L1_"

// S3FromEnv builds a config from EMBERVM_L1_* variables. It returns
// (zero, false, nil) when EMBERVM_L1_ENDPOINT is unset — L1 is optional.
func S3FromEnv() (S3Config, bool, error) {
	return s3ConfigFromEnv(S3EnvPrefix)
}

func s3ConfigFromEnv(prefix string) (S3Config, bool, error) {
	endpoint := os.Getenv(prefix + "ENDPOINT")
	if endpoint == "" {
		return S3Config{}, false, nil
	}
	cfg := S3Config{
		Endpoint:  endpoint,
		Bucket:    os.Getenv(prefix + "BUCKET"),
		Prefix:    os.Getenv(prefix + "PREFIX"),
		AccessKey: os.Getenv(prefix + "ACCESS_KEY"),
		SecretKey: os.Getenv(prefix + "SECRET_KEY"),
	}
	if v := os.Getenv(prefix + "SECURE"); v != "" {
		secure, err := strconv.ParseBool(v)
		if err != nil {
			return S3Config{}, false, fmt.Errorf("chunkstore: bad %sSECURE %q: %w", prefix, v, err)
		}
		cfg.Secure = secure
	}
	if cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return S3Config{}, false, fmt.Errorf("chunkstore: %sENDPOINT is set but BUCKET/ACCESS_KEY/SECRET_KEY are incomplete", prefix)
	}
	return cfg, true, nil
}

// S3 is the L1 backend over any S3-compatible store.
type S3 struct {
	c   *minio.Client
	cfg S3Config
}

func NewS3(cfg S3Config) (*S3, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("chunkstore: S3 endpoint and bucket are required")
	}
	c, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.Secure,
	})
	if err != nil {
		return nil, fmt.Errorf("chunkstore: s3 client: %w", err)
	}
	return &S3{c: c, cfg: cfg}, nil
}

func (s *S3) chunkKey(hash string) string {
	return path.Join(s.cfg.Prefix, "objects", hash[:2], hash)
}

func (s *S3) objectKey(key string) string {
	return path.Join(s.cfg.Prefix, "meta", key)
}

func notFound(err error) bool {
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		return resp.Code == minio.NoSuchKey || resp.StatusCode == 404
	}
	return false
}

// transientS3 reports whether an error is worth retrying: transport
// failures and 5xx/429 responses. NotFound and context cancellation are
// definitive answers, not blips.
func transientS3(err error) bool {
	if err == nil || notFound(err) || errors.Is(err, ErrNotFound) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		return resp.StatusCode >= 500 || resp.StatusCode == 429
	}
	return true // connection refused/reset, unexpected EOF, DNS, ...
}

// retryS3 runs f up to 3 times with short backoff on transient errors: a
// network blip on the write-through path must not fail a whole pause. Only
// idempotent operations (and rewindable uploads) come through here.
func retryS3(ctx context.Context, f func() error) error {
	backoff := 100 * time.Millisecond
	for attempt := 1; ; attempt++ {
		err := f()
		if attempt == 3 || !transientS3(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(backoff):
		}
		backoff *= 4
	}
}

func (s *S3) Put(ctx context.Context, hash string, r io.Reader, size int64) (bool, error) {
	if len(hash) < 3 {
		return false, fmt.Errorf("chunkstore: bad chunk hash %q", hash)
	}
	ok, err := s.Has(ctx, hash)
	if err != nil {
		return false, err
	}
	if ok {
		return false, nil // dedup; a lost race means a double PUT of identical bytes
	}
	if err := s.putRetrying(ctx, s.chunkKey(hash), r, size); err != nil {
		return false, fmt.Errorf("chunk %s: put: %w", hash, err)
	}
	return true, nil
}

// putRetrying uploads with retry when the reader can be rewound (seekable:
// *os.File, *minio.Object, bytes.Reader — every pause-path source). A
// non-seekable stream gets exactly one attempt: its bytes are gone.
func (s *S3) putRetrying(ctx context.Context, key string, r io.Reader, size int64) error {
	seeker, seekable := r.(io.Seeker)
	var start int64
	if seekable {
		var err error
		if start, err = seeker.Seek(0, io.SeekCurrent); err != nil {
			seekable = false
		}
	}
	do := func() error {
		_, err := s.c.PutObject(ctx, s.cfg.Bucket, key, r, size, minio.PutObjectOptions{})
		return err
	}
	if !seekable {
		return do()
	}
	return retryS3(ctx, func() error {
		if _, err := seeker.Seek(start, io.SeekStart); err != nil {
			return err
		}
		return do()
	})
}

// TouchChunk refreshes the chunk's LastModified — its GC clock — via a
// self-copy with a metadata REPLACE directive (see Copier: dedup hits).
// A missing key surfaces as ErrNotFound: the caller decides whether that
// means "re-upload" (Copier) or a genuine hole.
func (s *S3) TouchChunk(ctx context.Context, hash string) error {
	key := s.chunkKey(hash)
	return retryS3(ctx, func() error {
		_, err := s.c.CopyObject(ctx,
			minio.CopyDestOptions{
				Bucket: s.cfg.Bucket, Object: key,
				ReplaceMetadata: true,
				UserMetadata:    map[string]string{"touched": time.Now().UTC().Format(time.RFC3339)},
			},
			minio.CopySrcOptions{Bucket: s.cfg.Bucket, Object: key})
		if err != nil {
			if notFound(err) {
				return fmt.Errorf("chunk %s: touch: %w", hash, ErrNotFound)
			}
			return fmt.Errorf("chunk %s: touch: %w", hash, err)
		}
		return nil
	})
}

// StatChunk returns a chunk's current ChunkInfo — GC's delete-time re-check.
func (s *S3) StatChunk(ctx context.Context, hash string) (ChunkInfo, error) {
	var info ChunkInfo
	err := retryS3(ctx, func() error {
		st, err := s.c.StatObject(ctx, s.cfg.Bucket, s.chunkKey(hash), minio.StatObjectOptions{})
		if err != nil {
			if notFound(err) {
				return fmt.Errorf("chunk %s: %w", hash, ErrNotFound)
			}
			return fmt.Errorf("chunk %s: stat: %w", hash, err)
		}
		info = ChunkInfo{Hash: hash, ModTime: st.LastModified}
		return nil
	})
	return info, err
}

func (s *S3) Get(ctx context.Context, hash string) (io.ReadCloser, error) {
	var rc io.ReadCloser
	err := retryS3(ctx, func() error {
		obj, err := s.c.GetObject(ctx, s.cfg.Bucket, s.chunkKey(hash), minio.GetObjectOptions{})
		if err != nil {
			return fmt.Errorf("chunk %s: get: %w", hash, err)
		}
		// GetObject is lazy; probe so missing chunks surface as ErrNotFound here.
		if _, err := obj.Stat(); err != nil {
			obj.Close()
			if notFound(err) {
				return fmt.Errorf("chunk %s: %w", hash, ErrNotFound)
			}
			return fmt.Errorf("chunk %s: stat: %w", hash, err)
		}
		rc = obj
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rc, nil
}

func (s *S3) Has(ctx context.Context, hash string) (bool, error) {
	var found bool
	err := retryS3(ctx, func() error {
		_, err := s.c.StatObject(ctx, s.cfg.Bucket, s.chunkKey(hash), minio.StatObjectOptions{})
		if err != nil {
			if notFound(err) {
				found = false
				return nil
			}
			return fmt.Errorf("chunk %s: stat: %w", hash, err)
		}
		found = true
		return nil
	})
	return found, err
}

func (s *S3) Delete(ctx context.Context, hash string) error {
	return retryS3(ctx, func() error {
		err := s.c.RemoveObject(ctx, s.cfg.Bucket, s.chunkKey(hash), minio.RemoveObjectOptions{})
		if err != nil && !notFound(err) {
			return fmt.Errorf("chunk %s: delete: %w", hash, err)
		}
		return nil
	})
}

func (s *S3) PutObject(ctx context.Context, key string, r io.Reader, size int64) error {
	if err := s.putRetrying(ctx, s.objectKey(key), r, size); err != nil {
		return fmt.Errorf("object %s: put: %w", key, err)
	}
	return nil
}

func (s *S3) GetObject(ctx context.Context, key string) (io.ReadCloser, error) {
	var rc io.ReadCloser
	err := retryS3(ctx, func() error {
		obj, err := s.c.GetObject(ctx, s.cfg.Bucket, s.objectKey(key), minio.GetObjectOptions{})
		if err != nil {
			return fmt.Errorf("object %s: get: %w", key, err)
		}
		if _, err := obj.Stat(); err != nil {
			obj.Close()
			if notFound(err) {
				return fmt.Errorf("object %s: %w", key, ErrNotFound)
			}
			return fmt.Errorf("object %s: stat: %w", key, err)
		}
		rc = obj
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rc, nil
}

func (s *S3) DeleteObject(ctx context.Context, key string) error {
	return retryS3(ctx, func() error {
		err := s.c.RemoveObject(ctx, s.cfg.Bucket, s.objectKey(key), minio.RemoveObjectOptions{})
		if err != nil && !notFound(err) {
			return fmt.Errorf("object %s: delete: %w", key, err)
		}
		return nil
	})
}

func (s *S3) HasObject(ctx context.Context, key string) (bool, error) {
	var found bool
	err := retryS3(ctx, func() error {
		_, err := s.c.StatObject(ctx, s.cfg.Bucket, s.objectKey(key), minio.StatObjectOptions{})
		if err != nil {
			if notFound(err) {
				found = false
				return nil
			}
			return fmt.Errorf("object %s: stat: %w", key, err)
		}
		found = true
		return nil
	})
	return found, err
}
