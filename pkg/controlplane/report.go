package controlplane

import (
	"context"
	"fmt"
	"io"

	"github.com/embervm/embervm/pkg/chunkstore"
	"github.com/embervm/embervm/pkg/lifecycle"
	"github.com/embervm/embervm/pkg/memsnap"
	"github.com/embervm/embervm/pkg/nodeagent"
)

// StorageReport is one sandbox's storage footprint (成本报表, docs/zh/03 §3).
// Computed on demand from the layer manifests — nothing is cached.
type StorageReport struct {
	SandboxID    string `json:"sandbox_id"`
	State        string `json:"state"`
	Tier         string `json:"tier"` // hot | warm | cold | recycled | none
	LogicalBytes int64  `json:"logical_bytes"`
	StoredBytes  int64  `json:"stored_bytes"`
	ChunkCount   int    `json:"chunk_count"`
	// StoredRatio = stored/logical: what zero-skip + lz4 + dedup left to pay
	// for. Zero when nothing is stored.
	StoredRatio   float64 `json:"stored_ratio"`
	ArtifactBytes int64   `json:"artifact_bytes,omitempty"`
	Layers        int     `json:"layers"`
}

// storageReport builds the report for one sandbox row.
func (s *Server) storageReport(ctx context.Context, sb Sandbox) (StorageReport, error) {
	rep := StorageReport{SandboxID: sb.ID, State: sb.State, Tier: "none"}
	var store chunkstore.ListingBackend
	switch lifecycle.State(sb.State) {
	case lifecycle.StatePausedHot, lifecycle.StatePausedWarm:
		store = s.l1
		rep.Tier = "warm"
		if lifecycle.State(sb.State) == lifecycle.StatePausedHot {
			rep.Tier = "hot"
		}
	case lifecycle.StateArchivedCold:
		store = s.cold
		rep.Tier = "cold"
	case lifecycle.StateRecycled:
		rep.Tier = "recycled"
		if s.cold != nil {
			if n, err := objectSize(ctx, s.cold, nodeagent.KeyArtifacts(sb.ID)); err == nil {
				rep.ArtifactBytes = n
			}
		}
		return rep, nil
	default:
		return rep, nil // running or transient: no snapshot footprint to report
	}
	if store == nil {
		return rep, nil
	}

	var desc nodeagent.SnapshotDescriptor
	if err := readJSONObject(ctx, store, nodeagent.KeySnapshotJSON(sb.ID), &desc); err != nil {
		return rep, fmt.Errorf("report %s: descriptor: %w", sb.ID, err)
	}
	layers := make([]*memsnap.Manifest, 0, len(desc.Layers))
	for _, layer := range desc.Layers {
		data, err := readObject(ctx, store, nodeagent.KeyLayer(sb.ID, layer))
		if err != nil {
			return rep, fmt.Errorf("report %s: manifest %s: %w", sb.ID, layer, err)
		}
		m, err := memsnap.ParseManifest(data)
		if err != nil {
			return rep, err
		}
		layers = append(layers, m)
	}
	view, err := memsnap.Resolve(layers)
	if err != nil {
		return rep, err
	}
	rep.Layers = len(layers)
	unique := map[string]int64{}
	for _, ref := range view.Chunks {
		rep.LogicalBytes += int64(ref.ULen)
		if !ref.Zero {
			unique[ref.Hash] = int64(ref.CLen)
		}
	}
	for _, clen := range unique {
		rep.StoredBytes += clen
		rep.ChunkCount++
	}
	if rep.LogicalBytes > 0 {
		rep.StoredRatio = float64(rep.StoredBytes) / float64(rep.LogicalBytes)
	}
	return rep, nil
}

func objectSize(ctx context.Context, b chunkstore.Objects, key string) (int64, error) {
	rc, err := b.GetObject(ctx, key)
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	return io.Copy(io.Discard, rc)
}
