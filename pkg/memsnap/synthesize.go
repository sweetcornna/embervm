package memsnap

import (
	"fmt"
	"time"
)

// Synthesize flattens a resolved layer chain into ONE full-kind manifest —
// the Veeam-style synthetic full, and the reason M3 cold restores read
// exactly one memory layer no matter how long the diff chain grew. Because
// chunks are content-addressed, this is metadata-only: the new manifest
// simply references the newest-wins chunk of every index; zero chunk bytes
// move. The referenced chunks must be copied alongside the manifest when it
// changes stores (chunkstore.Copier does that dedup-aware).
func Synthesize(v *View, layerID string, createdAt time.Time) (*Manifest, error) {
	if v == nil || len(v.Chunks) == 0 {
		return nil, fmt.Errorf("synthesize %q: empty view", layerID)
	}
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	m := &Manifest{
		FormatVersion: FormatVersion,
		LayerID:       layerID,
		Kind:          KindFull,
		FCVersion:     v.FCVersion,
		KernelVersion: v.KernelVersion,
		MemSizeBytes:  v.MemSizeBytes,
		ChunkSize:     v.ChunkSize,
		CreatedAt:     createdAt,
		Chunks:        append([]ChunkRef(nil), v.Chunks...),
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("synthesize %q: %w", layerID, err)
	}
	return m, nil
}
