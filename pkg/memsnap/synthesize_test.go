package memsnap

import (
	"bytes"
	"testing"
	"time"
)

func TestSynthesizeFlattensChain(t *testing.T) {
	full := layer("p1", "", KindFull, ref(0, "a0"), ref(1, "a1"), ref(2, "a2"))
	d1 := layer("p2", "p1", KindDiff, ref(1, "b1"))
	d2 := layer("p3", "p2", KindDiff, zref(0))
	v, err := Resolve([]*Manifest{full, d1, d2})
	if err != nil {
		t.Fatal(err)
	}

	syn, err := Synthesize(v, "cold", time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if syn.Kind != KindFull || syn.Parent != "" || syn.LayerID != "cold" {
		t.Fatalf("synthetic manifest header = %+v", syn)
	}
	if len(syn.Chunks) != 3 {
		t.Fatalf("chunks = %d, want 3 (full coverage)", len(syn.Chunks))
	}
	if !syn.Chunks[0].Zero || syn.Chunks[1].Hash != "b1" || syn.Chunks[2].Hash != "a2" {
		t.Fatalf("newest-wins flatten broken: %+v", syn.Chunks)
	}

	// Resolving JUST the synthetic layer must give the same view.
	v2, err := Resolve([]*Manifest{syn})
	if err != nil {
		t.Fatal(err)
	}
	for i := range v.Chunks {
		if v.Chunks[i].Hash != v2.Chunks[i].Hash || v.Chunks[i].Zero != v2.Chunks[i].Zero {
			t.Fatalf("chunk %d differs after synthesize round trip", i)
		}
	}
}

// Byte-level equivalence: reconstructing from the synthetic full equals
// reconstructing from the original chain.
func TestSynthesizeReconstruction(t *testing.T) {
	img := buildTestImage(t)
	sink := newMemSink()
	full, err := WriteLayer(writeTemp(t, img), WriteOptions{LayerID: "p1"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	v1, err := Resolve([]*Manifest{full})
	if err != nil {
		t.Fatal(err)
	}
	syn, err := Synthesize(v1, "cold", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := Resolve([]*Manifest{syn})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reconstruct(t, v2, sink), img) {
		t.Fatal("synthetic-full reconstruction differs from source image")
	}
}

func TestSynthesizeRejectsEmptyView(t *testing.T) {
	if _, err := Synthesize(nil, "cold", time.Time{}); err == nil {
		t.Fatal("accepted nil view")
	}
	if _, err := Synthesize(&View{}, "cold", time.Time{}); err == nil {
		t.Fatal("accepted empty view")
	}
}
