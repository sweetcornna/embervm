// Descriptor and object-key layout of a sandbox snapshot in the L1/cold
// object stores. This file is portable: the lifecycle engine
// (pkg/controlplane) reads and rewrites descriptors when it archives a
// sandbox to the cold tier, so the contract lives in one place.

package nodeagent

// SnapshotDescriptor is the restore entry point a node publishes to L1 on
// every pause: everything another node needs to rebuild the sandbox.
// Producer is the pause path; consumers mirror it exactly.
type SnapshotDescriptor struct {
	FormatVersion int      `json:"format_version"`
	SandboxID     string   `json:"sandbox_id"`
	TemplateID    string   `json:"template_id"`
	VCPUs         int      `json:"vcpus"`
	MemoryMiB     int      `json:"memory_mib"`
	DataDiskGiB   int      `json:"data_disk_gib"`
	Dir           string   `json:"dir"`    // dataset mountpoint; snapfile drive paths point here
	Layers        []string `json:"layers"` // memory chain order, full root first: ["p1", "p2", ...]
	HasWS         bool     `json:"has_ws"`
	// M3 tiering (additive; consumers tolerate their absence in old
	// descriptors). Tier names the store the objects live in; DiskLayers is
	// the zfs delta chain (it can outlive memory-chain restarts, e.g. after
	// a cold restore forces a fresh Full); SnapSeq seeds the next layer
	// number so tags never collide across restores.
	Tier       string   `json:"tier,omitempty"`
	DiskLayers []string `json:"disk_layers,omitempty"`
	SnapSeq    int      `json:"snap_seq,omitempty"`
	// DiskOrigin, when set, roots the disk delta chain off ANOTHER
	// sandbox's snapshot (a golden clone) instead of the template — a
	// restoring node must materialize that sandbox's chain first (GUID
	// lineage).
	DiskOrigin *DiskOrigin `json:"disk_origin,omitempty"`
	// Egress is the sandbox's outbound-network policy ("nat"/"none"; M4 D8).
	// It rides the descriptor so a "none" sandbox cannot regain internet by
	// being tiered out and restored on another node.
	Egress string `json:"egress,omitempty"`
	// M6 runtime resize (additive). MemoryMiB above is the CURRENT effective
	// size; BaseMemoryMiB is the boot mem_size_mib the hotplug region sits on
	// top of, and MaxMemoryMiB/MaxVCPUs are the resize ceilings. Old
	// descriptors read as 0 = fixed geometry. The virtio-mem plug state
	// itself rides the Firecracker snapfile; these fields let the restoring
	// node re-derive cgroup bounds and future resize targets.
	BaseMemoryMiB int `json:"base_memory_mib,omitempty"`
	MaxMemoryMiB  int `json:"max_memory_mib,omitempty"`
	MaxVCPUs      int `json:"max_vcpus,omitempty"`
}

// DiskOrigin names a sandbox-snapshot clone base.
type DiskOrigin struct {
	SandboxID string `json:"sandbox_id"`
	Tag       string `json:"tag"`
}

// L1 object keys, all under the store's meta/ namespace.
func KeySnapshotJSON(id string) string     { return "sandboxes/" + id + "/snapshot.json" }
func KeyLayer(id, layer string) string     { return "sandboxes/" + id + "/layer-" + layer + ".json" }
func KeySnapfile(id, layer string) string  { return "sandboxes/" + id + "/snapfile-" + layer }
func KeyWS(id string) string               { return "sandboxes/" + id + "/ws.json" }
func KeyDiskDelta(id, layer string) string { return "sandboxes/" + id + "/disk-" + layer + ".zstream" }
func KeyTemplateStream(tid string) string  { return "templates/" + tid + ".zstream" }

// KeyArtifacts is the RECYCLED remnant ExtractArtifacts leaves in the cold
// store.
func KeyArtifacts(id string) string { return "sandboxes/" + id + "/artifacts.tar.zst" }
