package chunkstore

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 is a minimal in-memory S3 endpoint: enough of the protocol for
// minio-go PutObject/GetObject/StatObject/RemoveObject with path-style
// requests. Auth is ignored. Real MinIO coverage lands in e2e-m2 (P5).
type fakeS3 struct {
	mu      sync.Mutex
	bucket  string
	objects map[string][]byte
}

type s3Error struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Path-style: /<bucket>/<key...>
	trimmed := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(trimmed, "/")
	if bucket != f.bucket {
		writeS3Error(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	if key == "" {
		q := r.URL.Query()
		if q.Get("list-type") == "2" {
			f.serveListV2(w, q.Get("prefix"))
			return
		}
		// Bucket-level probes (location) — return something benign.
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?><LocationConstraint/>`)
		return
	}
	key, _ = url.PathUnescape(key)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		data, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
			data, err = decodeAWSChunked(data)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		f.objects[key] = data
		w.Header().Set("ETag", `"fake"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodHead:
		data, ok := f.objects[key]
		if !ok {
			// HEAD carries no body; status alone signals NoSuchKey.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", itoa(len(data)))
		w.Header().Set("ETag", `"fake"`)
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		data, ok := f.objects[key]
		if !ok {
			writeS3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("Content-Length", itoa(len(data)))
		w.Header().Set("ETag", `"fake"`)
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		_, _ = w.Write(data)
	case http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// serveListV2 implements just enough of ListObjectsV2 for minio-go.
func (f *fakeS3) serveListV2(w http.ResponseWriter, prefix string) {
	f.mu.Lock()
	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sizes := make(map[string]int, len(keys))
	for _, k := range keys {
		sizes[k] = len(f.objects[k])
	}
	f.mu.Unlock()
	sort.Strings(keys)

	type contents struct {
		Key          string    `xml:"Key"`
		Size         int       `xml:"Size"`
		LastModified time.Time `xml:"LastModified"`
		ETag         string    `xml:"ETag"`
	}
	type result struct {
		XMLName     xml.Name   `xml:"ListBucketResult"`
		Name        string     `xml:"Name"`
		Prefix      string     `xml:"Prefix"`
		KeyCount    int        `xml:"KeyCount"`
		IsTruncated bool       `xml:"IsTruncated"`
		Contents    []contents `xml:"Contents"`
	}
	res := result{Name: f.bucket, Prefix: prefix, KeyCount: len(keys)}
	for _, k := range keys {
		res.Contents = append(res.Contents, contents{
			Key: k, Size: sizes[k], LastModified: time.Now().UTC(), ETag: `"fake"`,
		})
	}
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(res)
}

func writeS3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_ = xml.NewEncoder(w).Encode(s3Error{Code: code, Message: code})
}

func itoa(n int) string { return strconv.Itoa(n) }

// decodeAWSChunked unwraps the aws-chunked payload framing minio-go uses
// for streaming-signature uploads over plain HTTP:
// "<hex-size>;chunk-signature=<sig>\r\n<data>\r\n" repeated, 0-size last.
func decodeAWSChunked(body []byte) ([]byte, error) {
	var out []byte
	rest := body
	for {
		i := strings.Index(string(rest), "\r\n")
		if i < 0 {
			return nil, io.ErrUnexpectedEOF
		}
		header := string(rest[:i])
		rest = rest[i+2:]
		sizeHex, _, _ := strings.Cut(header, ";")
		size, err := strconv.ParseInt(sizeHex, 16, 64)
		if err != nil {
			return nil, err
		}
		if size == 0 {
			return out, nil
		}
		if int64(len(rest)) < size+2 {
			return nil, io.ErrUnexpectedEOF
		}
		out = append(out, rest[:size]...)
		rest = rest[size+2:]
	}
}

func newFakeS3Backend(t *testing.T) *S3 {
	t.Helper()
	fake := &fakeS3{bucket: "embervm-test", objects: map[string][]byte{}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	s3, err := NewS3(S3Config{
		Endpoint:  strings.TrimPrefix(srv.URL, "http://"),
		Bucket:    "embervm-test",
		Prefix:    "ci",
		AccessKey: "test",
		SecretKey: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return s3
}

func TestS3BackendContract(t *testing.T) {
	backendContract(t, newFakeS3Backend(t))
}

func TestS3FromEnv(t *testing.T) {
	t.Setenv("EMBERVM_L1_ENDPOINT", "")
	if _, ok, err := S3FromEnv(); ok || err != nil {
		t.Fatalf("unset endpoint = ok=%v err=%v, want disabled", ok, err)
	}

	t.Setenv("EMBERVM_L1_ENDPOINT", "localhost:9000")
	t.Setenv("EMBERVM_L1_BUCKET", "embervm")
	t.Setenv("EMBERVM_L1_ACCESS_KEY", "ak")
	t.Setenv("EMBERVM_L1_SECRET_KEY", "sk")
	t.Setenv("EMBERVM_L1_PREFIX", "p")
	t.Setenv("EMBERVM_L1_SECURE", "false")
	cfg, ok, err := S3FromEnv()
	if err != nil || !ok {
		t.Fatal(err)
	}
	if cfg.Endpoint != "localhost:9000" || cfg.Bucket != "embervm" || cfg.Prefix != "p" || cfg.Secure {
		t.Fatalf("cfg = %+v", cfg)
	}

	t.Setenv("EMBERVM_L1_BUCKET", "")
	if _, _, err := S3FromEnv(); err == nil {
		t.Fatal("incomplete config accepted")
	}

	t.Setenv("EMBERVM_L1_BUCKET", "embervm")
	t.Setenv("EMBERVM_L1_SECURE", "not-a-bool")
	if _, _, err := S3FromEnv(); err == nil {
		t.Fatal("bad SECURE accepted")
	}
}

func TestS3ListChunksAndKeys(t *testing.T) {
	s3 := newFakeS3Backend(t)
	ctx := context.Background()
	putChunk(t, s3, "aa111", "x")
	putChunk(t, s3, "bb222", "y")
	putManifest(t, s3, "sandboxes/sb1/layer-p1.json", "aa111")

	chunks, err := s3.ListChunks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("ListChunks = %+v, want 2", chunks)
	}
	keys, err := s3.ListObjectKeys(ctx, "sandboxes/")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != "sandboxes/sb1/layer-p1.json" {
		t.Fatalf("ListObjectKeys = %v", keys)
	}

	// GC works against S3 too (grace 0).
	res, err := GC(ctx, s3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.SweptChunks != 1 || res.LiveChunks != 1 {
		t.Fatalf("S3 GC result = %+v", res)
	}
	if ok, _ := s3.Has(ctx, "bb222"); ok {
		t.Fatal("unreferenced chunk survived S3 GC")
	}
}
