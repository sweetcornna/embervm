package uffd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestWSTraceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ws.json")
	trace := &WSTrace{FormatVersion: WSTraceFormatVersion, ChunkSize: 16384, Chunks: []int{7, 2, 9}}
	if err := trace.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	got, err := ReadWSTrace(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(trace, got) {
		t.Fatalf("round trip: wrote %+v, read %+v", trace, got)
	}
}

func TestWSTraceMissingFileMeansRecord(t *testing.T) {
	got, err := ReadWSTrace(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil || got != nil {
		t.Fatalf("missing file = %+v, %v; want nil, nil", got, err)
	}
}

func TestWSTraceRejectsUnknownVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ws.json")
	if err := os.WriteFile(path, []byte(`{"format_version":99,"chunks":[1]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadWSTrace(path); err == nil {
		t.Fatal("accepted unknown format_version")
	}
}

func TestWSTraceWireFieldNames(t *testing.T) {
	data, err := json.Marshal(&WSTrace{FormatVersion: 1, ChunkSize: 16384, Chunks: []int{1}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, key := range []string{`"format_version":1`, `"chunk_size":16384`, `"chunks":[1]`} {
		if !strings.Contains(s, key) {
			t.Errorf("wire JSON missing %s in %s", key, s)
		}
	}
}

func TestWSRecorderFirstTouchOrder(t *testing.T) {
	r := newWSRecorder(16384)
	for _, ci := range []int{5, 3, 5, 8, 3, 5, 1} {
		r.touch(ci)
	}
	trace := r.trace()
	if want := []int{5, 3, 8, 1}; !reflect.DeepEqual(trace.Chunks, want) {
		t.Fatalf("first-touch order = %v, want %v", trace.Chunks, want)
	}
	if trace.FormatVersion != WSTraceFormatVersion || trace.ChunkSize != 16384 {
		t.Fatalf("trace header = %+v", trace)
	}
}
