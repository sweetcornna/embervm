package uffd

import (
	"encoding/json"
	"fmt"
	"os"
)

// WSTraceFormatVersion versions the ws.json wire format.
const WSTraceFormatVersion = 1

// WSTrace is the recorded working set: chunk indices in first-touch fault
// order (REAP-style). Recorded on the first chunked resume, consumed as the
// eager prefetch order by every later resume.
type WSTrace struct {
	FormatVersion int   `json:"format_version"`
	ChunkSize     int   `json:"chunk_size"`
	Chunks        []int `json:"chunks"`
}

// ReadWSTrace loads a trace; a missing file returns (nil, nil) — the caller
// treats that as "first resume, record".
func ReadWSTrace(path string) (*WSTrace, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var t WSTrace
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("parse ws trace %s: %w", path, err)
	}
	if t.FormatVersion != WSTraceFormatVersion {
		return nil, fmt.Errorf("ws trace %s: unsupported format_version %d", path, t.FormatVersion)
	}
	return &t, nil
}

// WriteFile atomically persists the trace.
func (t *WSTrace) WriteFile(path string) error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// wsRecorder accumulates first-touch chunk order. Only the fault loop
// goroutine touches it — no locking.
type wsRecorder struct {
	chunkSize int
	seen      map[int]bool
	order     []int
}

func newWSRecorder(chunkSize int) *wsRecorder {
	return &wsRecorder{chunkSize: chunkSize, seen: make(map[int]bool)}
}

func (r *wsRecorder) touch(ci int) {
	if r.seen[ci] {
		return
	}
	r.seen[ci] = true
	r.order = append(r.order, ci)
}

func (r *wsRecorder) trace() *WSTrace {
	return &WSTrace{FormatVersion: WSTraceFormatVersion, ChunkSize: r.chunkSize, Chunks: r.order}
}

// ChunkGetter fetches a chunk's stored bytes by content address.
// chunkstore.Bytes satisfies it (local dir or Tiered local+L1).
type ChunkGetter interface {
	Get(hash string) ([]byte, error)
}
