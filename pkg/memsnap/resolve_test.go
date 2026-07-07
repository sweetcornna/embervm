package memsnap

import (
	"testing"
	"time"
)

func layer(id, parent, kind string, chunks ...ChunkRef) *Manifest {
	return &Manifest{
		FormatVersion: FormatVersion,
		LayerID:       id,
		Parent:        parent,
		Kind:          kind,
		FCVersion:     "v1.16.1",
		KernelVersion: "6.1.155",
		MemSizeBytes:  3 * 16384,
		ChunkSize:     16384,
		CreatedAt:     time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC),
		Chunks:        chunks,
	}
}

func ref(i int, hash string) ChunkRef {
	return ChunkRef{Index: i, Hash: hash, Codec: CodecRaw, ULen: 16384, CLen: 16384}
}

func zref(i int) ChunkRef { return ChunkRef{Index: i, Zero: true, ULen: 16384} }

func TestResolveChainNewestWins(t *testing.T) {
	full := layer("p1", "", KindFull, ref(0, "a0"), ref(1, "a1"), ref(2, "a2"))
	d1 := layer("p2", "p1", KindDiff, ref(1, "b1"))
	d2 := layer("p3", "p2", KindDiff, ref(2, "c2"), zref(0))

	// Shuffled input order must not matter.
	v, err := Resolve([]*Manifest{d2, full, d1})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Layers) != 3 || v.Layers[0] != "p1" || v.Layers[2] != "p3" {
		t.Fatalf("layer order = %v", v.Layers)
	}
	if !v.Chunks[0].Zero {
		t.Errorf("chunk0 = %+v, want zero from p3", v.Chunks[0])
	}
	if v.Chunks[1].Hash != "b1" {
		t.Errorf("chunk1 hash = %s, want b1 from p2", v.Chunks[1].Hash)
	}
	if v.Chunks[2].Hash != "c2" {
		t.Errorf("chunk2 hash = %s, want c2 from p3", v.Chunks[2].Hash)
	}
}

func TestResolveSingleFull(t *testing.T) {
	v, err := Resolve([]*Manifest{layer("p1", "", KindFull, ref(0, "a"), zref(1), ref(2, "c"))})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Chunks) != 3 || v.Chunks[0].Hash != "a" || !v.Chunks[1].Zero {
		t.Fatalf("view = %+v", v.Chunks)
	}
}

func TestResolveErrors(t *testing.T) {
	full := func() *Manifest {
		return layer("p1", "", KindFull, ref(0, "a0"), ref(1, "a1"), ref(2, "a2"))
	}
	cases := map[string][]*Manifest{
		"no layers":     {},
		"no full root":  {layer("p2", "p1", KindDiff, ref(0, "x"))},
		"two fulls":     {full(), layer("q1", "", KindFull, ref(0, "b0"), ref(1, "b1"), ref(2, "b2"))},
		"orphan diff":   {full(), layer("p3", "missing", KindDiff, ref(0, "x"))},
		"forked chain":  {full(), layer("p2", "p1", KindDiff, ref(0, "x")), layer("p2b", "p1", KindDiff, ref(1, "y"))},
		"duplicate ids": {full(), full()},
	}
	for name, layers := range cases {
		if _, err := Resolve(layers); err == nil {
			t.Errorf("%s: Resolve accepted invalid chain", name)
		}
	}

	geom := layer("p2", "p1", KindDiff, ref(0, "x"))
	geom.ChunkSize = 8192
	geom.Chunks = []ChunkRef{{Index: 0, Hash: "x", Codec: CodecRaw, ULen: 8192, CLen: 8192}}
	if _, err := Resolve([]*Manifest{full(), geom}); err == nil {
		t.Error("geometry mismatch: Resolve accepted differing chunk_size")
	}
}
