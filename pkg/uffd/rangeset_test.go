package uffd

import "testing"

func TestRangeSet(t *testing.T) {
	var s rangeSet

	if s.contains(0) || s.intersects(0, 100) {
		t.Fatal("empty set should contain nothing")
	}

	s.add(100, 200)
	s.add(300, 400)
	if !s.contains(100) || !s.contains(199) || s.contains(200) || s.contains(250) {
		t.Fatal("half-open range semantics broken")
	}
	if !s.intersects(150, 350) {
		t.Fatal("intersects failed across two ranges")
	}
	if s.intersects(200, 300) {
		t.Fatal("gap [200,300) should not intersect")
	}

	// Merging: [150, 320) bridges both existing ranges.
	s.add(150, 320)
	if s.count() != 1 {
		t.Fatalf("expected 1 merged range, got %d", s.count())
	}
	if !s.contains(100) || !s.contains(399) || s.contains(400) {
		t.Fatal("merged range has wrong bounds")
	}

	// Degenerate input is ignored.
	s.add(500, 500)
	s.add(600, 500)
	if s.count() != 1 {
		t.Fatal("degenerate ranges should be ignored")
	}
}
