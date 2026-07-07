package uffd

import "sync"

// rangeSet tracks half-open [start, end) address ranges that the guest has
// discarded (balloon inflation → madvise(MADV_DONTNEED) → UFFD_EVENT_REMOVE).
// Faults inside a removed range must be served with zero pages, never with
// stale snapshot contents. The set is expected to stay tiny (M0 VMs carry no
// balloon device), so a merged, sorted slice with linear scans is enough.
type rangeSet struct {
	mu     sync.Mutex
	ranges []addrRange
}

type addrRange struct {
	start, end uint64
}

func (s *rangeSet) add(start, end uint64) {
	if end <= start {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	merged := addrRange{start, end}
	out := s.ranges[:0]
	for _, r := range s.ranges {
		if r.end < merged.start || r.start > merged.end {
			out = append(out, r)
			continue
		}
		if r.start < merged.start {
			merged.start = r.start
		}
		if r.end > merged.end {
			merged.end = r.end
		}
	}
	s.ranges = append(out, merged)
}

func (s *rangeSet) contains(addr uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.ranges {
		if addr >= r.start && addr < r.end {
			return true
		}
	}
	return false
}

// intersects reports whether any part of [start, end) is in the set.
func (s *rangeSet) intersects(start, end uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.ranges {
		if start < r.end && end > r.start {
			return true
		}
	}
	return false
}

func (s *rangeSet) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.ranges)
}
