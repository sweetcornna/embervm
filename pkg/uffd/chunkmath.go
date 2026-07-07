package uffd

// chunkPiece is the part of one chunk that falls inside one announced guest
// region: copy data[dataOff:dataOff+length] to hostAddr.
type chunkPiece struct {
	hostAddr uint64
	dataOff  int
	length   uint64
}

// chunkPieces maps chunk-content space to host virtual address space. A
// chunk occupies [chunkOff, chunkOff+chunkLen) in snapshot-memory offset
// space; regions may start at arbitrary page-aligned offsets, so a chunk can
// straddle region boundaries or fall partly outside all regions (those bytes
// are simply never mapped, e.g. the PCI hole between regions).
func chunkPieces(mappings []GuestRegionUffdMapping, chunkOff uint64, chunkLen int) []chunkPiece {
	var out []chunkPiece
	chunkEnd := chunkOff + uint64(chunkLen)
	for i := range mappings {
		m := &mappings[i]
		lo, hi := chunkOff, chunkEnd
		if regionEnd := m.Offset + m.Size; hi > regionEnd {
			hi = regionEnd
		}
		if lo < m.Offset {
			lo = m.Offset
		}
		if hi <= lo {
			continue
		}
		out = append(out, chunkPiece{
			hostAddr: m.BaseHostVirtAddr + (lo - m.Offset),
			dataOff:  int(lo - chunkOff),
			length:   hi - lo,
		})
	}
	return out
}

// chunkIndexForFault maps a faulting page (host vaddr, already page-aligned)
// in region m to its chunk index.
func chunkIndexForFault(m *GuestRegionUffdMapping, page uint64, chunkSize int) int {
	return int((m.Offset + (page - m.BaseHostVirtAddr)) / uint64(chunkSize))
}
