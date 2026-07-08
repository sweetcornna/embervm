package chunkstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"

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
	if _, err := s.c.PutObject(ctx, s.cfg.Bucket, s.chunkKey(hash), r, size,
		minio.PutObjectOptions{}); err != nil {
		return false, fmt.Errorf("chunk %s: put: %w", hash, err)
	}
	return true, nil
}

func (s *S3) Get(ctx context.Context, hash string) (io.ReadCloser, error) {
	obj, err := s.c.GetObject(ctx, s.cfg.Bucket, s.chunkKey(hash), minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("chunk %s: get: %w", hash, err)
	}
	// GetObject is lazy; probe so missing chunks surface as ErrNotFound here.
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		if notFound(err) {
			return nil, fmt.Errorf("chunk %s: %w", hash, ErrNotFound)
		}
		return nil, fmt.Errorf("chunk %s: stat: %w", hash, err)
	}
	return obj, nil
}

func (s *S3) Has(ctx context.Context, hash string) (bool, error) {
	_, err := s.c.StatObject(ctx, s.cfg.Bucket, s.chunkKey(hash), minio.StatObjectOptions{})
	if err != nil {
		if notFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("chunk %s: stat: %w", hash, err)
	}
	return true, nil
}

func (s *S3) Delete(ctx context.Context, hash string) error {
	err := s.c.RemoveObject(ctx, s.cfg.Bucket, s.chunkKey(hash), minio.RemoveObjectOptions{})
	if err != nil && !notFound(err) {
		return fmt.Errorf("chunk %s: delete: %w", hash, err)
	}
	return nil
}

func (s *S3) PutObject(ctx context.Context, key string, r io.Reader, size int64) error {
	if _, err := s.c.PutObject(ctx, s.cfg.Bucket, s.objectKey(key), r, size,
		minio.PutObjectOptions{}); err != nil {
		return fmt.Errorf("object %s: put: %w", key, err)
	}
	return nil
}

func (s *S3) GetObject(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.c.GetObject(ctx, s.cfg.Bucket, s.objectKey(key), minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("object %s: get: %w", key, err)
	}
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		if notFound(err) {
			return nil, fmt.Errorf("object %s: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("object %s: stat: %w", key, err)
	}
	return obj, nil
}

func (s *S3) HasObject(ctx context.Context, key string) (bool, error) {
	_, err := s.c.StatObject(ctx, s.cfg.Bucket, s.objectKey(key), minio.StatObjectOptions{})
	if err != nil {
		if notFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("object %s: stat: %w", key, err)
	}
	return true, nil
}
