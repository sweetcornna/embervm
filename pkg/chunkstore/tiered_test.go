package chunkstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

func TestTieredLocalFirst(t *testing.T) {
	ctx := context.Background()
	local, remote := newFakeStore(), newFakeStore()
	local.objects["aa1"] = []byte("local")
	remote.objects["aa1"] = []byte("remote-should-not-win")
	tiered := Tiered{Local: local, Remote: remote}

	rc, err := tiered.Get(ctx, "aa1")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "local" {
		t.Fatalf("Get = %q, want local copy", got)
	}
}

func TestTieredFallbackWritesThrough(t *testing.T) {
	ctx := context.Background()
	local, remote := newFakeStore(), newFakeStore()
	remote.objects["bb2"] = []byte("from-l1")
	tiered := Tiered{Local: local, Remote: remote}

	rc, err := tiered.Get(ctx, "bb2")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "from-l1" {
		t.Fatalf("Get = %q", got)
	}
	if !bytes.Equal(local.objects["bb2"], []byte("from-l1")) {
		t.Fatal("fetched chunk not written through to local store")
	}

	if _, err := tiered.Get(ctx, "ccmissing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing everywhere = %v, want ErrNotFound", err)
	}
}

func TestTieredHasAndPutScopes(t *testing.T) {
	ctx := context.Background()
	local, remote := newFakeStore(), newFakeStore()
	remote.objects["dd3"] = []byte("x")
	tiered := Tiered{Local: local, Remote: remote}

	ok, err := tiered.Has(ctx, "dd3")
	if err != nil || !ok {
		t.Fatalf("Has remote-only = %v, %v", ok, err)
	}
	if _, err := tiered.Put(ctx, "ee4", bytes.NewReader([]byte("y")), 1); err != nil {
		t.Fatal(err)
	}
	if _, ok := remote.objects["ee4"]; ok {
		t.Fatal("Put leaked to remote tier")
	}
	if _, ok := local.objects["ee4"]; !ok {
		t.Fatal("Put did not land in local tier")
	}
}
