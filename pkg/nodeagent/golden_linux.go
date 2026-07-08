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
}

// keyGolden is the L1 object recording a template's golden snapshot.
func keyGolden(tid string) string { return "templates/" + tid + "/golden.json" }

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

// buildGolden boots one sandbox from the freshly built template, pauses it
// through the normal write-through pipeline, releases its runtime resources
// (lease, in-memory entry) while KEEPING the dataset snapshot and staging
// files, and records templates/<tid>/golden.json in L1.
func (a *Agent) buildGolden(ctx context.Context, templateID string) error {
	gid := goldenID(templateID)
	meta := goldenMeta{
		SandboxID:   gid,
		VCPUs:       max(a.cfg.GoldenVCPUs, 1),
		MemoryMiB:   a.cfg.GoldenMemoryMiB,
		DataDiskGiB: max(a.cfg.GoldenDataDiskGiB, 1),
	}
	if _, err := a.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
		SandboxID: gid, TemplateID: templateID,
		VCPUs: meta.VCPUs, MemoryMiB: meta.MemoryMiB, DataDiskGiB: meta.DataDiskGiB,
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
		a.golden[templateID] = meta
		a.mu.Unlock()
	}

	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if err := a.l1.PutObject(ctx, keyGolden(templateID), bytes.NewReader(data), int64(len(data))); err != nil {
		return fmt.Errorf("golden meta: %w", err)
	}
	log.Printf("nodeagent: golden snapshot for template %s ready (%s, %dMiB)", templateID, gid, meta.MemoryMiB)
	return nil
}

// goldenFor finds a usable golden snapshot for the geometry, checking the
// in-memory record first and falling back to L1 (another build on this
// node's lifetime, or a rebuild after agent restart with the golden dataset
// still present).
func (a *Agent) goldenFor(ctx context.Context, templateID string, req nodeapi.CreateSandboxRequest) (goldenMeta, bool) {
	if !a.goldenEnabled() {
		return goldenMeta{}, false
	}
	a.mu.Lock()
	meta, ok := a.golden[templateID]
	a.mu.Unlock()
	if !ok {
		if err := getJSONFrom(ctx, a.l1, keyGolden(templateID), &meta); err != nil {
			return goldenMeta{}, false
		}
	}
	if req.VCPUs != meta.VCPUs || req.MemoryMiB != meta.MemoryMiB || req.DataDiskGiB != meta.DataDiskGiB {
		return goldenMeta{}, false // geometry mismatch: cold boot
	}
	// The golden dataset snapshot must exist locally to clone from.
	gsb := &sandbox{id: meta.SandboxID, dir: filepath.Join(a.cfg.WorkDir, meta.SandboxID)}
	if _, err := os.Stat(filepath.Join(gsb.snapDir(), "layer-p1.json")); err != nil {
		return goldenMeta{}, false
	}
	return meta, true
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
		dataDiskGiB: meta.DataDiskGiB,
		egress:      req.Egress,
	})
	if err != nil {
		return nodeapi.SandboxStatus{}, fmt.Errorf("fast create: %w", err)
	}
	return st, nil
}
