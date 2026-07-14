// M7 default-elastic geometry (ADR-0008): a create request that names no
// geometry no longer produces a fixed-size sandbox — it gets a small base, a
// platform-default resize ceiling, and autoscale, so resources are allocated
// on demand instead of being frozen at create. Explicit geometry keeps its
// M6 meaning exactly (base without max = fixed; base + max = user-declared
// elastic), so existing clients see no behavior change.

package controlplane

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
)

// createSandboxBody is POST /v0/sandboxes' request schema. It lives at
// package scope (rather than inline in the handler) so resolveGeometry can
// be a pure, table-testable function over it.
type createSandboxBody struct {
	TemplateID  string `json:"template_id"`
	VCPUs       int    `json:"vcpus"`
	MemoryMiB   int    `json:"memory_mib"`
	DataDiskGiB int    `json:"data_disk_gib"`
	// ArtifactPaths are preserved when the sandbox is RECYCLED
	// (M3 selective restore); empty keeps nothing.
	ArtifactPaths []string `json:"artifact_paths"`
	Egress        string   `json:"egress"`
	// M6 runtime-resize ceilings; 0 = fixed geometry when a base is given.
	// M7: omitting the base entirely selects the default-elastic geometry
	// (the resolver fills base and, when absent, these ceilings).
	MaxMemoryMiB int `json:"max_memory_mib"`
	MaxVCPUs     int `json:"max_vcpus"`
	// Autoscale opts into the engine's pressure-driven resize loop within
	// [memory_mib, max_memory_mib] / [vcpus, max_vcpus]. nil means "platform
	// default": on for default-elastic creates (no explicit base), off
	// everywhere else — so old clients that omit the field keep today's
	// behavior whenever they pass any geometry.
	Autoscale *bool `json:"autoscale"`
}

// ElasticDefaults configures what a no-geometry create resolves to (M7).
// The zero value means "enabled with the platform defaults" so existing
// Server construction sites get default-elastic without wiring.
type ElasticDefaults struct {
	// Disabled restores the pre-M7 behavior byte for byte: zero geometry
	// passes through to the node's own defaults and the sandbox is fixed.
	Disabled bool
	// Base geometry for creates that name none. Defaults mirror the node
	// agent's fills (pkg/nodeagent agent_linux.go CreateSandbox) so the
	// accounting row and the VM agree even in Disabled deployments.
	BaseVCPUs     int // default 1
	BaseMemoryMiB int // default 256
	DataDiskGiB   int // default 15
	// Ceilings for creates that name no max_*. The hotplug region costs
	// nothing until a resize plugs blocks, but its ~1.6%-of-ceiling memmap
	// tax is resident in guest boot memory — keep the default modest.
	MaxVCPUs     int // default 4
	MaxMemoryMiB int // default 4096
}

func (d ElasticDefaults) withDefaults() ElasticDefaults {
	if d.BaseVCPUs <= 0 {
		d.BaseVCPUs = 1
	}
	if d.BaseMemoryMiB <= 0 {
		d.BaseMemoryMiB = 256
	}
	if d.DataDiskGiB <= 0 {
		d.DataDiskGiB = 15
	}
	if d.MaxVCPUs <= 0 {
		d.MaxVCPUs = 4
	}
	if d.MaxMemoryMiB <= 0 {
		d.MaxMemoryMiB = 4096
	}
	// A ceiling below the base would fail validateCeilings on every create.
	if d.MaxVCPUs < d.BaseVCPUs {
		d.MaxVCPUs = d.BaseVCPUs
	}
	if d.MaxMemoryMiB < d.BaseMemoryMiB {
		d.MaxMemoryMiB = d.BaseMemoryMiB
	}
	return d
}

// ElasticDefaultsFromEnv reads EMBERVM_DEFAULT_ELASTIC (bool, default true),
// EMBERVM_DEFAULT_MAX_MEMORY_MIB and EMBERVM_DEFAULT_MAX_VCPUS. The memory
// ceiling is pre-rounded to the hotplug slot grid so the operator sees the
// effective value at startup instead of a silent per-create adjustment.
func ElasticDefaultsFromEnv() (ElasticDefaults, error) {
	var d ElasticDefaults
	if v := os.Getenv("EMBERVM_DEFAULT_ELASTIC"); v != "" {
		on, err := strconv.ParseBool(v)
		if err != nil {
			return d, fmt.Errorf("bad EMBERVM_DEFAULT_ELASTIC %q: %w", v, err)
		}
		d.Disabled = !on
	}
	if v := os.Getenv("EMBERVM_DEFAULT_MAX_MEMORY_MIB"); v != "" {
		n, err := strconv.Atoi(v)
		base := d.withDefaults().BaseMemoryMiB
		if err != nil || n < base || n > 1<<20 {
			return d, fmt.Errorf("bad EMBERVM_DEFAULT_MAX_MEMORY_MIB %q (want [%d,%d])", v, base, 1<<20)
		}
		if r := base + roundUpToSlot(n-base); r != n {
			log.Printf("controlplane: EMBERVM_DEFAULT_MAX_MEMORY_MIB %d rounds up to %d (%d MiB hotplug slots)",
				n, r, hotplugSlotMiB)
			n = r
		}
		d.MaxMemoryMiB = n
	}
	if v := os.Getenv("EMBERVM_DEFAULT_MAX_VCPUS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 64 {
			return d, fmt.Errorf("bad EMBERVM_DEFAULT_MAX_VCPUS %q (want [1,64])", v)
		}
		d.MaxVCPUs = n
	}
	return d, nil
}

// resolveGeometry applies the M7 decision table to a create request:
//
//	base given? | max given? | autoscale (json) | result
//	no          | no         | nil / true       | default elastic, autoscale ON
//	no          | no         | false            | default elastic, autoscale off
//	no          | yes        | nil / true       | elastic w/ custom ceiling, default base, autoscale ON
//	yes         | no         | nil / false      | fixed geometry (M6 contract, missing halves filled)
//	yes         | no         | true             | error — a fixed sandbox cannot autoscale
//	yes         | yes        | any              | user-declared elastic (M6 contract; nil = off)
//
// It only assigns values — slot rounding stays with the single site in
// createSandbox, and validateCeilings bounds-checks the resolved result.
// Disabled passes everything through untouched (pre-M7 behavior).
func resolveGeometry(b createSandboxBody, d ElasticDefaults) (createSandboxBody, error) {
	d = d.withDefaults()
	if d.Disabled {
		return b, nil
	}
	baseGiven := b.VCPUs > 0 || b.MemoryMiB > 0
	maxGiven := b.MaxVCPUs > 0 || b.MaxMemoryMiB > 0
	switch {
	case !baseGiven:
		// Simple mode: no explicit base means "start small, grow on demand"
		// — with the platform ceiling unless the caller picked one.
		b.VCPUs, b.MemoryMiB = d.BaseVCPUs, d.BaseMemoryMiB
		if !maxGiven {
			b.MaxVCPUs, b.MaxMemoryMiB = d.MaxVCPUs, d.MaxMemoryMiB
		}
		if b.Autoscale == nil {
			b.Autoscale = boolPtr(true)
		}
	case !maxGiven:
		// Explicit base without a ceiling keeps the M6 fixed-geometry
		// contract; fill the missing half so the accounting row (and
		// Place's reservation) carries real numbers instead of zeros.
		if b.Autoscale != nil && *b.Autoscale {
			return b, errors.New("autoscale requires max_memory_mib and/or max_vcpus" +
				" (omit vcpus/memory_mib entirely to get the default-elastic ceiling)")
		}
		if b.VCPUs == 0 {
			b.VCPUs = d.BaseVCPUs
		}
		if b.MemoryMiB == 0 {
			b.MemoryMiB = d.BaseMemoryMiB
		}
	}
	if b.DataDiskGiB == 0 {
		b.DataDiskGiB = d.DataDiskGiB
	}
	return b, nil
}

func boolPtr(b bool) *bool { return &b }
