//go:build linux

package nodeagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/embervm/embervm/pkg/nodeapi"
	"github.com/embervm/embervm/pkg/storage"
)

// The G4 fast-create path (master-spec D4): at template build a golden
// sandbox is booted once and paused through the ordinary chunked pipeline;
// CreateSandbox then clones the golden's dataset snapshot and restores the
// golden's memory image instead of cold-booting (~150ms hot restore vs
// seconds of kernel boot). Correctness hinges on two invariants:
//
//   - disk must match the memory image's moment, so clones come from the
//     GOLDEN's @p1 snapshot, never the pristine template;
//   - snapshot drive paths must be identical for every clone, which only
//     the jailer's chroot layout provides — fast-create therefore requires
//     jailed mode and quietly falls back to cold boot otherwise.

// goldenMeta locates a template's golden snapshot on this node.
type goldenMeta struct {
	SandboxID   string `json:"sandbox_id"`
	VCPUs       int    `json:"vcpus"`
	MemoryMiB   int    `json:"memory_mib"`
	DataDiskGiB int    `json:"data_disk_gib"`
	// M6: the hotplug region is part of the snapshot, so it is part of the
	// geometry. M7 builds TWO golden slots per template — fixed (Max ==
	// base, the pre-M7 shape) and, when Config.GoldenMax* is set, elastic
	// (hotplug region + max boot cores baked in) — so default-elastic
	// creates fast-create too. Old golden.json reads as 0 and is
	// normalized to the base on load.
	MaxMemoryMiB int `json:"max_memory_mib,omitempty"`
	MaxVCPUs     int `json:"max_vcpus,omitempty"`
}

// keyGolden is the L1 object recording a template's fixed golden snapshot.
func keyGolden(tid string) string { return "templates/" + tid + "/golden.json" }

// keyGoldenElastic is the L1 object recording a template's elastic golden
// snapshot (M7).
func keyGoldenElastic(tid string) string { return "templates/" + tid + "/golden-elastic.json" }

// goldenElasticSuffix distinguishes the elastic slot in the in-memory map
// and the derived golden sandbox id.
const goldenElasticSuffix = "#elastic"

// goldenID derives a deterministic, id-safe golden sandbox name.
func goldenID(templateID string) string {
	sum := sha256.Sum256([]byte(templateID))
	return "g-" + hex.EncodeToString(sum[:8])
}

// goldenEnabled reports whether this agent builds/uses golden snapshots.
func (a *Agent) goldenEnabled() bool {
	_, isRepl := a.cfg.Storage.(storage.Replicator)
	return a.cfg.GoldenMemoryMiB > 0 && a.chunked() && a.jailed() && a.l1 != nil && isRepl
}

// buildGolden boots golden sandboxes from the freshly built template — a
// fixed-geometry one, plus an elastic one when Config.GoldenMax* is set
// (M7) — pausing each through the normal write-through pipeline and
// recording its meta in L1. The elastic slot is itself an optimization on
// an optimization: its failure keeps the fixed golden.
func (a *Agent) buildGolden(ctx context.Context, templateID string) error {
	base := goldenMeta{
		SandboxID:   goldenID(templateID),
		VCPUs:       max(a.cfg.GoldenVCPUs, 1),
		MemoryMiB:   a.cfg.GoldenMemoryMiB,
		DataDiskGiB: max(a.cfg.GoldenDataDiskGiB, 1),
	}
	// Fixed geometry: no hotplug region in the golden snapshot (M6).
	base.MaxMemoryMiB, base.MaxVCPUs = base.MemoryMiB, base.VCPUs
	if err := a.buildGoldenSlot(ctx, templateID, templateID, keyGolden(templateID), base); err != nil {
		return err
	}
	if a.cfg.GoldenMaxMemoryMiB <= base.MemoryMiB && a.cfg.GoldenMaxVCPUs <= base.VCPUs {
		return nil
	}
	// Elastic slot (M7). Apply CreateSandbox's ceiling normalization HERE:
	// goldenFor compares against the normalized request, so meta recorded
	// with raw config values would never match.
	elastic := base
	elastic.SandboxID = goldenID(templateID + goldenElasticSuffix)
	if a.cfg.GoldenMaxMemoryMiB > elastic.MemoryMiB {
		elastic.MaxMemoryMiB = elastic.MemoryMiB +
			roundUpMiB(a.cfg.GoldenMaxMemoryMiB-elastic.MemoryMiB, hotplugSlotMiB)
	}
	elastic.MaxVCPUs = max(a.cfg.GoldenMaxVCPUs, elastic.VCPUs)
	if err := a.buildGoldenSlot(ctx, templateID, templateID+goldenElasticSuffix,
		keyGoldenElastic(templateID), elastic); err != nil {
		log.Printf("nodeagent: elastic golden for %s failed (elastic creates cold-boot): %v", templateID, err)
	}
	return nil
}

// buildGoldenSlot boots one golden sandbox with meta's geometry, pauses it,
// releases its runtime resources (lease, in-memory entry) while KEEPING the
// dataset snapshot and staging files, and records the meta under l1Key.
func (a *Agent) buildGoldenSlot(ctx context.Context, templateID, mapKey, l1Key string, meta goldenMeta) error {
	gid := meta.SandboxID
	if _, err := a.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
		SandboxID: gid, TemplateID: templateID,
		VCPUs: meta.VCPUs, MemoryMiB: meta.MemoryMiB, DataDiskGiB: meta.DataDiskGiB,
		MaxMemoryMiB: meta.MaxMemoryMiB, MaxVCPUs: meta.MaxVCPUs,
	}); err != nil {
		return fmt.Errorf("golden boot: %w", err)
	}
	if err := a.PauseSandbox(ctx, gid); err != nil {
		_ = a.StopSandbox(ctx, gid)
		return fmt.Errorf("golden pause: %w", err)
	}

	// Park it: the dataset snapshot and staging files are the template's
	// warm image; the runtime resources go back to the pool. The golden
	// never resumes in place — clones do.
	a.mu.Lock()
	sb := a.sbx[gid]
	delete(a.sbx, gid)
	a.mu.Unlock()
	if sb != nil {
		if a.jailed() {
			a.teardownJail(sb)
		}
		a.removeCgroup(sb.id)
		sb.lease.Release()
		a.mu.Lock()
		a.golden[mapKey] = meta
		a.mu.Unlock()
	}

	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if err := a.l1.PutObject(ctx, l1Key, bytes.NewReader(data), int64(len(data))); err != nil {
		return fmt.Errorf("golden meta: %w", err)
	}
	log.Printf("nodeagent: golden snapshot for template %s ready (%s, %dMiB ceiling %dMiB/%d vcpus)",
		templateID, gid, meta.MemoryMiB, meta.MaxMemoryMiB, meta.MaxVCPUs)
	return nil
}

// goldenFor finds a usable golden snapshot for the geometry, checking the
// in-memory record first and falling back to L1 (another build on this
// node's lifetime, or a rebuild after agent restart with the golden dataset
// still present). The request is already normalized (CreateSandbox rounds
// ceilings before calling here); an elastic request selects the elastic
// slot (M7), everything else the fixed one.
func (a *Agent) goldenFor(ctx context.Context, templateID string, req nodeapi.CreateSandboxRequest) (goldenMeta, bool) {
	if !a.goldenEnabled() {
		return goldenMeta{}, false
	}
	mapKey, l1Key := templateID, keyGolden(templateID)
	if req.MaxMemoryMiB > req.MemoryMiB || req.MaxVCPUs > req.VCPUs {
		mapKey, l1Key = templateID+goldenElasticSuffix, keyGoldenElastic(templateID)
	}
	a.mu.Lock()
	meta, ok := a.golden[mapKey]
	a.mu.Unlock()
	if !ok {
		if err := getJSONFrom(ctx, a.l1, l1Key, &meta); err != nil {
			return goldenMeta{}, false
		}
	}
	// Pre-M6 golden.json has no ceilings: fixed geometry.
	if meta.MaxMemoryMiB < meta.MemoryMiB {
		meta.MaxMemoryMiB = meta.MemoryMiB
	}
	if meta.MaxVCPUs < meta.VCPUs {
		meta.MaxVCPUs = meta.VCPUs
	}
	if !goldenMatches(meta, req) {
		// Loud on purpose: a control-plane default drifting from the node's
		// golden config degrades every default create to a silent cold
		// boot — this line is the diagnostic.
		log.Printf("nodeagent: golden %s geometry mismatch (golden %d/%dMiB max %d/%dMiB disk %dGiB; req %d/%dMiB max %d/%dMiB disk %dGiB): cold boot",
			mapKey, meta.VCPUs, meta.MemoryMiB, meta.MaxVCPUs, meta.MaxMemoryMiB, meta.DataDiskGiB,
			req.VCPUs, req.MemoryMiB, req.MaxVCPUs, req.MaxMemoryMiB, req.DataDiskGiB)
		return goldenMeta{}, false
	}
	// The golden dataset snapshot must exist locally to clone from.
	gsb := &sandbox{id: meta.SandboxID, dir: filepath.Join(a.cfg.WorkDir, meta.SandboxID)}
	if _, err := os.Stat(filepath.Join(gsb.snapDir(), "layer-p1.json")); err != nil {
		return goldenMeta{}, false
	}
	return meta, true
}

// goldenMatches reports whether a NORMALIZED create request's geometry is
// exactly the golden's. The hotplug region is part of the snapshot, so the
// ceilings must match too — a near-miss cannot be served (the region size
// is baked into the FC snapfile).
func goldenMatches(meta goldenMeta, req nodeapi.CreateSandboxRequest) bool {
	return req.VCPUs == meta.VCPUs && req.MemoryMiB == meta.MemoryMiB &&
		req.DataDiskGiB == meta.DataDiskGiB &&
		req.MaxMemoryMiB == meta.MaxMemoryMiB && req.MaxVCPUs == meta.MaxVCPUs
}

// fastCreate clones the golden snapshot into a new identity and hot-restores
// its memory image — a fork whose parent is the template's golden sandbox.
func (a *Agent) fastCreate(ctx context.Context, req nodeapi.CreateSandboxRequest, meta goldenMeta) (nodeapi.SandboxStatus, error) {
	st, err := a.cloneRestore(ctx, cloneSpec{
		newID:       req.SandboxID,
		srcID:       meta.SandboxID,
		srcSnapDir:  filepath.Join(a.cfg.WorkDir, meta.SandboxID, "snap"),
		layers:      []string{"p1"}, // the golden pauses exactly once
		templateID:  req.TemplateID,
		vcpus:       meta.VCPUs,
		memMiB:      meta.MemoryMiB,
		baseMemMiB:  meta.MemoryMiB,
		maxMemMiB:   meta.MaxMemoryMiB,
		maxVCPUs:    meta.MaxVCPUs,
		dataDiskGiB: meta.DataDiskGiB,
		egress:      req.Egress,
	})
	if err != nil {
		return nodeapi.SandboxStatus{}, fmt.Errorf("fast create: %w", err)
	}
	return st, nil
}
