package memsnap

import "fmt"

// View is the flat restore view of a layer chain: for every chunk index, the
// newest ChunkRef that covers it. The uffd handler serves faults from this.
type View struct {
	MemSizeBytes  int64
	ChunkSize     int
	FCVersion     string
	KernelVersion string
	Layers        []string   // chain order, root full layer first
	Chunks        []ChunkRef // len == ChunkCount(MemSizeBytes, ChunkSize)
}

// Resolve orders manifests into their parent chain (input order does not
// matter), validates cross-layer consistency, and flattens newest-wins.
func Resolve(layers []*Manifest) (*View, error) {
	if len(layers) == 0 {
		return nil, fmt.Errorf("resolve: no layers")
	}
	byID := make(map[string]*Manifest, len(layers))
	var root *Manifest
	for _, m := range layers {
		if err := m.Validate(); err != nil {
			return nil, err
		}
		if byID[m.LayerID] != nil {
			return nil, fmt.Errorf("resolve: duplicate layer_id %q", m.LayerID)
		}
		byID[m.LayerID] = m
		if m.Kind == KindFull {
			if root != nil {
				return nil, fmt.Errorf("resolve: two full layers (%q, %q)", root.LayerID, m.LayerID)
			}
			root = m
		}
	}
	if root == nil {
		return nil, fmt.Errorf("resolve: no full root layer")
	}

	children := make(map[string]*Manifest, len(layers))
	for _, m := range layers {
		if m.Kind == KindDiff {
			if children[m.Parent] != nil {
				return nil, fmt.Errorf("resolve: layers %q and %q share parent %q", children[m.Parent].LayerID, m.LayerID, m.Parent)
			}
			children[m.Parent] = m
		}
	}

	chain := []*Manifest{root}
	for cur := root; ; {
		next := children[cur.LayerID]
		if next == nil {
			break
		}
		chain = append(chain, next)
		cur = next
	}
	if len(chain) != len(layers) {
		return nil, fmt.Errorf("resolve: %d of %d layers are not on the chain from %q", len(layers)-len(chain), len(layers), root.LayerID)
	}

	v := &View{
		MemSizeBytes:  root.MemSizeBytes,
		ChunkSize:     root.ChunkSize,
		FCVersion:     root.FCVersion,
		KernelVersion: root.KernelVersion,
		Chunks:        make([]ChunkRef, ChunkCount(root.MemSizeBytes, root.ChunkSize)),
	}
	for _, m := range chain {
		if m.MemSizeBytes != v.MemSizeBytes || m.ChunkSize != v.ChunkSize {
			return nil, fmt.Errorf("resolve: layer %q geometry (%d/%d) differs from root (%d/%d)",
				m.LayerID, m.MemSizeBytes, m.ChunkSize, v.MemSizeBytes, v.ChunkSize)
		}
		v.Layers = append(v.Layers, m.LayerID)
		for _, c := range m.Chunks {
			v.Chunks[c.Index] = c
		}
	}
	return v, nil
}
