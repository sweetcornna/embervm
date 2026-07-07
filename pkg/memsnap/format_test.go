package memsnap

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func validManifest() *Manifest {
	return &Manifest{
		FormatVersion: FormatVersion,
		LayerID:       "p1",
		Kind:          KindFull,
		FCVersion:     "v1.16.1",
		KernelVersion: "6.1.155",
		MemSizeBytes:  32 * 1024,
		ChunkSize:     16 * 1024,
		CreatedAt:     time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC),
		Chunks: []ChunkRef{
			{Index: 0, Zero: true, ULen: 16384},
			{Index: 1, Hash: "ab", Codec: CodecLZ4, ULen: 16384, CLen: 512},
		},
	}
}

func TestManifestRoundTrip(t *testing.T) {
	m := validManifest()
	path := filepath.Join(t.TempDir(), "layer-p1.json")
	if err := m.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	got, err := ReadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m, got) {
		t.Fatalf("round trip mismatch:\nwrote %+v\nread  %+v", m, got)
	}
}

// The JSON key names are the wire contract (producer is source of truth).
func TestManifestWireFieldNames(t *testing.T) {
	data, err := json.Marshal(validManifest())
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, key := range []string{
		`"format_version":1`, `"layer_id":"p1"`, `"kind":"full"`,
		`"fc_version":"v1.16.1"`, `"kernel_version":"6.1.155"`,
		`"mem_size_bytes":32768`, `"chunk_size":16384`, `"created_at":`,
		`"i":1`, `"h":"ab"`, `"z":true`, `"c":"lz4"`, `"ul":16384`, `"cl":512`,
	} {
		if !strings.Contains(s, key) {
			t.Errorf("wire JSON missing %s in %s", key, s)
		}
	}
	if strings.Contains(s, `"parent"`) {
		t.Errorf("empty parent must be omitted: %s", s)
	}
}

func TestChunkCount(t *testing.T) {
	cases := []struct {
		mem   int64
		chunk int
		want  int
	}{
		{16384, 16384, 1},
		{16385, 16384, 2},
		{32768, 16384, 2},
		{1000, 16384, 1},
		{0, 16384, 0},
		{16384, 0, 0},
	}
	for _, c := range cases {
		if got := ChunkCount(c.mem, c.chunk); got != c.want {
			t.Errorf("ChunkCount(%d,%d) = %d, want %d", c.mem, c.chunk, got, c.want)
		}
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]func(*Manifest){
		"bad kind":            func(m *Manifest) { m.Kind = "incremental" },
		"full with parent":    func(m *Manifest) { m.Parent = "p0" },
		"diff without parent": func(m *Manifest) { m.Kind = KindDiff },
		"empty layer id":      func(m *Manifest) { m.LayerID = "" },
		"bad chunk size":      func(m *Manifest) { m.ChunkSize = 0 },
		"index out of range":  func(m *Manifest) { m.Chunks[1].Index = 2 },
		"duplicate index":     func(m *Manifest) { m.Chunks[1].Index = 0 },
		"zero with hash":      func(m *Manifest) { m.Chunks[0].Hash = "ff" },
		"missing hash":        func(m *Manifest) { m.Chunks[1].Hash = "" },
		"bad codec":           func(m *Manifest) { m.Chunks[1].Codec = "zstd" },
		"bad clen":            func(m *Manifest) { m.Chunks[1].CLen = 0 },
		"ulen over chunk":     func(m *Manifest) { m.Chunks[1].ULen = 16385 },
		"full missing chunks": func(m *Manifest) { m.Chunks = m.Chunks[:1] },
		"wrong version":       func(m *Manifest) { m.FormatVersion = 99 },
	}
	for name, mutate := range cases {
		m := validManifest()
		mutate(m)
		if err := m.Validate(); err == nil {
			t.Errorf("%s: Validate accepted invalid manifest", name)
		}
	}
	if err := validManifest().Validate(); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
}

func TestDecodeRawAndErrors(t *testing.T) {
	raw := ChunkRef{Index: 0, Hash: "x", Codec: CodecRaw, ULen: 4, CLen: 4}
	out, err := Decode(raw, []byte("data"))
	if err != nil || string(out) != "data" {
		t.Fatalf("raw decode = %q, %v", out, err)
	}
	if _, err := Decode(raw, []byte("toolong")); err == nil {
		t.Error("decode accepted wrong stored length")
	}
	if _, err := Decode(ChunkRef{Index: 1, Zero: true, ULen: 4}, nil); err == nil {
		t.Error("decode accepted zero chunk")
	}
	if _, err := Decode(ChunkRef{Index: 2, Codec: "zstd", ULen: 4, CLen: 4}, []byte("data")); err == nil {
		t.Error("decode accepted unknown codec")
	}
}
