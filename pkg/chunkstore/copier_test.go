package chunkstore

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"testing"
)

func TestCopierCopySkipsExisting(t *testing.T) {
	ctx := context.Background()
	src, dst := newFakeStore(), newFakeStore()
	var hashes []string
	for i := 0; i < 20; i++ {
		h := fmt.Sprintf("ab%038d", i)
		hashes = append(hashes, h)
		src.objects[h] = bytes.Repeat([]byte{byte(i)}, 64)
	}
	// Pre-seed 5 chunks in dst.
	for _, h := range hashes[:5] {
		dst.objects[h] = src.objects[h]
	}

	copied, err := Copier{Src: src, Dst: dst, Parallel: 4}.Copy(ctx, hashes)
	if err != nil {
		t.Fatal(err)
	}
	if copied != 15 {
		t.Fatalf("copied = %d, want 15", copied)
	}
	for _, h := range hashes {
		if !bytes.Equal(dst.objects[h], src.objects[h]) {
			t.Fatalf("chunk %s content mismatch after copy", h)
		}
	}
}

func TestCopierPropagatesErrors(t *testing.T) {
	src, dst := newFakeStore(), newFakeStore()
	src.objects["aa1"] = []byte("x")
	src.objects["aa2"] = []byte("y")
	src.failGet["aa2"] = true
	_, err := Copier{Src: src, Dst: dst}.Copy(context.Background(), []string{"aa1", "aa2"})
	if err == nil {
		t.Fatal("Copy swallowed a source failure")
	}
}

func TestCopierCanceledContext(t *testing.T) {
	src, dst := newFakeStore(), newFakeStore()
	src.objects["aa1"] = []byte("x")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (Copier{Src: src, Dst: dst}).Copy(ctx, []string{"aa1"}); err == nil {
		t.Fatal("Copy ignored canceled context")
	}
}

func TestCopierEmptyList(t *testing.T) {
	copied, err := Copier{Src: newFakeStore(), Dst: newFakeStore()}.Copy(context.Background(), nil)
	if err != nil || copied != 0 {
		t.Fatalf("empty Copy = %d, %v", copied, err)
	}
}

func TestMissingPreservesOrder(t *testing.T) {
	ctx := context.Background()
	s := newFakeStore()
	s.objects["bb2"] = []byte("y")
	got, err := Missing(ctx, s, []string{"bb3", "bb2", "bb1"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"bb3", "bb1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Missing = %v, want %v", got, want)
	}
}
