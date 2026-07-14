//go:build linux

package nodeagent

import (
	"testing"

	"github.com/embervm/embervm/pkg/nodeapi"
)

// TestGoldenMatches pins the geometry-match contract for both golden slots
// (M7): the hotplug region is part of the snapshot, so ceilings compare too.
func TestGoldenMatches(t *testing.T) {
	fixed := goldenMeta{SandboxID: "g-1", VCPUs: 1, MemoryMiB: 256, DataDiskGiB: 1,
		MaxMemoryMiB: 256, MaxVCPUs: 1}
	elastic := goldenMeta{SandboxID: "g-2", VCPUs: 1, MemoryMiB: 256, DataDiskGiB: 1,
		MaxMemoryMiB: 1024, MaxVCPUs: 2}

	req := func(v, m, d, maxM, maxV int) nodeapi.CreateSandboxRequest {
		return nodeapi.CreateSandboxRequest{VCPUs: v, MemoryMiB: m, DataDiskGiB: d,
			MaxMemoryMiB: maxM, MaxVCPUs: maxV}
	}

	cases := []struct {
		name string
		meta goldenMeta
		req  nodeapi.CreateSandboxRequest
		want bool
	}{
		{"fixed exact", fixed, req(1, 256, 1, 256, 1), true},
		{"elastic exact", elastic, req(1, 256, 1, 1024, 2), true},
		{"elastic request vs fixed golden", fixed, req(1, 256, 1, 1024, 2), false},
		{"fixed request vs elastic golden", elastic, req(1, 256, 1, 256, 1), false},
		{"ceiling off by one slot", elastic, req(1, 256, 1, 1152, 2), false},
		{"vcpu ceiling differs", elastic, req(1, 256, 1, 1024, 4), false},
		{"base differs", elastic, req(1, 512, 1, 1024, 2), false},
		{"disk differs", fixed, req(1, 256, 2, 256, 1), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := goldenMatches(tc.meta, tc.req); got != tc.want {
				t.Errorf("goldenMatches(%+v, %+v) = %v, want %v", tc.meta, tc.req, got, tc.want)
			}
		})
	}
}

// TestGoldenElasticMetaNormalization proves buildGolden's elastic-meta
// rounding is the SAME formula CreateSandbox applies to requests — the
// invariant goldenFor's exact match depends on. An awkward configured
// ceiling (1000) must land where a normalized request lands (1024).
func TestGoldenElasticMetaNormalization(t *testing.T) {
	base := 256
	for _, cfgMax := range []int{257, 384, 1000, 1024, 4096} {
		// buildGolden's meta formula.
		meta := base + roundUpMiB(cfgMax-base, hotplugSlotMiB)
		// CreateSandbox's request normalization (agent_linux.go).
		req := nodeapi.CreateSandboxRequest{MemoryMiB: base, MaxMemoryMiB: cfgMax}
		if req.MaxMemoryMiB > req.MemoryMiB {
			req.MaxMemoryMiB = req.MemoryMiB + roundUpMiB(req.MaxMemoryMiB-req.MemoryMiB, hotplugSlotMiB)
		}
		if meta != req.MaxMemoryMiB {
			t.Errorf("cfgMax %d: meta ceiling %d != normalized request ceiling %d", cfgMax, meta, req.MaxMemoryMiB)
		}
	}
	if got := 256 + roundUpMiB(1000-256, hotplugSlotMiB); got != 1024 {
		t.Errorf("1000 MiB ceiling on 256 base = %d, want 1024", got)
	}
}

// TestGoldenForPreM6Normalization pins the load-time clamp: a pre-M6
// golden.json (no ceiling fields) reads as fixed geometry and must match a
// fixed request after the clamp goldenFor applies.
func TestGoldenForPreM6Normalization(t *testing.T) {
	meta := goldenMeta{SandboxID: "g-old", VCPUs: 1, MemoryMiB: 256, DataDiskGiB: 1}
	// goldenFor's clamp (kept inline there; mirrored here).
	if meta.MaxMemoryMiB < meta.MemoryMiB {
		meta.MaxMemoryMiB = meta.MemoryMiB
	}
	if meta.MaxVCPUs < meta.VCPUs {
		meta.MaxVCPUs = meta.VCPUs
	}
	if !goldenMatches(meta, nodeapi.CreateSandboxRequest{
		VCPUs: 1, MemoryMiB: 256, DataDiskGiB: 1, MaxMemoryMiB: 256, MaxVCPUs: 1}) {
		t.Error("clamped pre-M6 meta should match a fixed request")
	}
}
