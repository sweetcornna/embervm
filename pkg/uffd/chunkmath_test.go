package uffd

import (
	"reflect"
	"testing"
)

// Two regions with a hole between them, like x86 guests below/above the
// 3.5 GiB PCI hole (scaled down): region A covers offsets [0, 64K),
// region B covers [96K, 160K); offsets [64K, 96K) are unannounced.
func testMappings() []GuestRegionUffdMapping {
	return []GuestRegionUffdMapping{
		{BaseHostVirtAddr: 0x7f0000000000, Size: 64 << 10, Offset: 0, PageSize: 4096},
		{BaseHostVirtAddr: 0x7f0000100000, Size: 64 << 10, Offset: 96 << 10, PageSize: 4096},
	}
}

func TestChunkPiecesInsideOneRegion(t *testing.T) {
	got := chunkPieces(testMappings(), 16<<10, 16<<10) // chunk fully in region A
	want := []chunkPiece{{hostAddr: 0x7f0000000000 + 16<<10, dataOff: 0, length: 16 << 10}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pieces = %+v, want %+v", got, want)
	}
}

func TestChunkPiecesInHole(t *testing.T) {
	if got := chunkPieces(testMappings(), 64<<10, 16<<10); len(got) != 0 {
		t.Fatalf("chunk in unannounced hole returned pieces: %+v", got)
	}
}

func TestChunkPiecesStraddlingRegionEnd(t *testing.T) {
	// Chunk [56K, 72K): first 8K in region A, rest in the hole.
	got := chunkPieces(testMappings(), 56<<10, 16<<10)
	want := []chunkPiece{{hostAddr: 0x7f0000000000 + 56<<10, dataOff: 0, length: 8 << 10}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pieces = %+v, want %+v", got, want)
	}
}

func TestChunkPiecesStraddlingRegionStart(t *testing.T) {
	// Chunk [88K, 104K): first 8K in the hole, last 8K at region B's start.
	got := chunkPieces(testMappings(), 88<<10, 16<<10)
	want := []chunkPiece{{hostAddr: 0x7f0000100000, dataOff: 8 << 10, length: 8 << 10}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pieces = %+v, want %+v", got, want)
	}
}

func TestChunkPiecesPartialTrailingChunk(t *testing.T) {
	// Last chunk of a 160K snapshot may be short (ULen 4K).
	got := chunkPieces(testMappings(), 156<<10, 4<<10)
	want := []chunkPiece{{hostAddr: 0x7f0000100000 + 60<<10, dataOff: 0, length: 4 << 10}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pieces = %+v, want %+v", got, want)
	}
}

func TestChunkIndexForFault(t *testing.T) {
	ms := testMappings()
	// Fault at 8K into region B => offset 96K + 8K = 104K => chunk 6 (16K chunks).
	if got := chunkIndexForFault(&ms[1], ms[1].BaseHostVirtAddr+8<<10, 16<<10); got != 6 {
		t.Fatalf("chunk index = %d, want 6", got)
	}
	if got := chunkIndexForFault(&ms[0], ms[0].BaseHostVirtAddr, 16<<10); got != 0 {
		t.Fatalf("chunk index = %d, want 0", got)
	}
}
