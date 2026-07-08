//go:build linux

package nodeagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/embervm/embervm/pkg/lifecycle"
	"github.com/embervm/embervm/pkg/memsnap"
	"github.com/embervm/embervm/pkg/metrics"
	"github.com/embervm/embervm/pkg/nodeapi"
	"github.com/embervm/embervm/pkg/storage"
)

// M5 fork/branch/rollback (master-spec 2026-07-09, ADR-0006). A checkpoint
// IS what every chunked pause already produces — memory layer p<N> + zfs
// snapshot @p<N> — so fork is "golden fast-create from an arbitrary layer of
// an arbitrary parent" and rollback is the DeltaBox layer switch: the same
// hot resume the tiers use, pointed at an earlier layer. Memory is CoW by
// construction (content-addressed chunks are shared); disk is CoW by ZFS
// clone. Both paths require chunked + jailed + ZFS for the same reason
// golden does: chroot-relative snapfile paths are what make one sandbox's
// snapshot loadable inside another's jail.

// cloneSpec describes a clone-restore: a new sandbox born from another
// sandbox's checkpoint. Golden fast-create and M5 fork share this path.
type cloneSpec struct {
	newID       string
	srcID       string   // clone origin: dataset snapshot + staging files
	srcSnapDir  string   // the src's snap dir (immutable committed layers)
	layers      []string // manifest chain, root first: ["p1", ..., "pN"]
	templateID  string
	vcpus       int
	memMiB      int
	dataDiskGiB int
	egress      string
}

// layerSeq parses the numeric sequence out of a p<N> layer name.
func layerSeq(layer string) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(layer, "p"))
	if err != nil || !strings.HasPrefix(layer, "p") || n < 1 {
		return 0, fmt.Errorf("bad layer name %q (want p<N>)", layer)
	}
	return n, nil
}

// chainFor walks manifest Parent links from layer down to its Full root,
// reading the source's staging files. Committed checkpoint files never
// change, so this needs no shared in-memory state — a fork may race the
// parent's next pause safely. A layer from a retired epoch (before a chain
// restart: cold restore, rollback) has no resolvable file and errors here.
func chainFor(snapDir, layer string) ([]string, error) {
	var rev []string
	for l := layer; ; {
		m, err := memsnap.ReadManifest(filepath.Join(snapDir, "layer-"+l+".json"))
		if err != nil {
			return nil, fmt.Errorf("checkpoint %s not in the current chain: %w", layer, err)
		}
		rev = append(rev, l)
		if m.Parent == "" {
			break
		}
		l = m.Parent
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev, nil
}

// cloneRestore builds a new sandbox from a source checkpoint: ZFS clone of
// src@p<N>, the chain's staging files, a fresh lease, and an ordinary
// chunked hot resume. The child's future pauses and cross-node restores
// work unchanged: diskOrigin records the clone base (GUID lineage) and the
// first disk delta reads the dataset's real `origin` property.
func (a *Agent) cloneRestore(ctx context.Context, spec cloneSpec) (nodeapi.SandboxStatus, error) {
	zfs, ok := a.cfg.Storage.(*storage.ZFSBackend)
	if !ok {
		return nodeapi.SandboxStatus{}, fmt.Errorf("clone-restore: requires the ZFS backend")
	}
	last := spec.layers[len(spec.layers)-1]
	seq, err := layerSeq(last)
	if err != nil {
		return nodeapi.SandboxStatus{}, err
	}
	paths, err := zfs.CloneSandboxFrom(ctx, spec.newID, spec.srcID, last)
	if err != nil {
		return nodeapi.SandboxStatus{}, fmt.Errorf("clone %s@%s: %w", spec.srcID, last, err)
	}
	lease, err := a.cfg.Pool.Acquire()
	if err != nil {
		_ = a.cfg.Storage.DestroySandbox(ctx, spec.newID)
		return nodeapi.SandboxStatus{}, err
	}

	sb := &sandbox{
		id:          spec.newID,
		machine:     lifecycle.New(lifecycle.StatePausedHot),
		lease:       lease,
		dir:         filepath.Join(a.cfg.WorkDir, spec.newID),
		vcpus:       spec.vcpus,
		memMiB:      spec.memMiB,
		rootfs:      paths.RootfsExt4,
		dataRaw:     paths.DataRaw,
		templateID:  spec.templateID,
		dataDiskGiB: spec.dataDiskGiB,
		mountDir:    paths.Dir,
		egress:      spec.egress,
		snapCount:   seq, // the chain continues above the checkpoint
		snapLayer:   last,
		diskOrigin:  &DiskOrigin{SandboxID: spec.srcID, Tag: last},
	}
	if err := os.MkdirAll(sb.snapDir(), 0o755); err != nil {
		a.cleanup(ctx, sb)
		return nodeapi.SandboxStatus{}, err
	}
	if err := a.applyEgress(ctx, sb); err != nil {
		a.cleanup(ctx, sb)
		return nodeapi.SandboxStatus{}, fmt.Errorf("apply egress policy: %w", err)
	}

	// The source's committed staging becomes the clone's chain: every
	// manifest, the checkpoint's snapfile, and (when present) the ws hint.
	files := make([]string, 0, len(spec.layers)+2)
	for _, l := range spec.layers {
		files = append(files, "layer-"+l+".json")
	}
	files = append(files, "snapfile-"+last, "ws.json")
	for _, f := range files {
		src := filepath.Join(spec.srcSnapDir, f)
		if _, err := os.Stat(src); err != nil {
			if f == "ws.json" {
				continue // no working set recorded: it is only a prefetch hint
			}
			a.cleanup(ctx, sb)
			return nodeapi.SandboxStatus{}, fmt.Errorf("clone-restore: source artifact %s: %w", f, err)
		}
		if err := copyFileSimple(filepath.Join(sb.snapDir(), f), src); err != nil {
			a.cleanup(ctx, sb)
			return nodeapi.SandboxStatus{}, err
		}
	}
	for _, l := range spec.layers {
		m, err := memsnap.ReadManifest(filepath.Join(sb.snapDir(), "layer-"+l+".json"))
		if err != nil {
			a.cleanup(ctx, sb)
			return nodeapi.SandboxStatus{}, err
		}
		sb.layers = append(sb.layers, m)
	}

	a.mu.Lock()
	a.sbx[spec.newID] = sb
	a.mu.Unlock()

	st, err := a.resume(ctx, spec.newID)
	if err != nil {
		a.mu.Lock()
		delete(a.sbx, spec.newID)
		a.mu.Unlock()
		a.cleanup(ctx, sb)
		return nodeapi.SandboxStatus{}, err
	}
	return st, nil
}

// Fork creates a new sandbox from a parent's checkpoint layer without
// touching the parent (its machine never transitions; committed chain files
// are immutable). Geometry and egress are inherited. Same-node in M5.
func (a *Agent) Fork(ctx context.Context, parentID, layer, newID string) (nodeapi.SandboxStatus, error) {
	forkStart := time.Now()
	parent, err := a.get(parentID)
	if err != nil {
		return nodeapi.SandboxStatus{}, err
	}
	if !a.chunked() || !a.jailed() {
		return nodeapi.SandboxStatus{}, fmt.Errorf("fork: requires restore_mode=chunked under the jailer")
	}
	chain, err := chainFor(parent.snapDir(), layer)
	if err != nil {
		return nodeapi.SandboxStatus{}, fmt.Errorf("fork %s: %w", parentID, err)
	}
	st, err := a.cloneRestore(ctx, cloneSpec{
		newID:       newID,
		srcID:       parentID,
		srcSnapDir:  parent.snapDir(),
		layers:      chain,
		templateID:  parent.templateID,
		vcpus:       parent.vcpus,
		memMiB:      parent.memMiB,
		dataDiskGiB: parent.dataDiskGiB,
		egress:      parent.egress,
	})
	if err == nil {
		metrics.CreateSeconds.WithLabelValues("fork").Observe(time.Since(forkStart).Seconds())
	}
	return st, err
}

// Rollback switches the sandbox back to checkpoint `layer` in place: the
// dataset zfs-rolls-back (same name, same mountpoint, no jail rebind), the
// memory chain trims to the target, and the same hot resume the tiers use
// brings it back interactive. Everything after the checkpoint is DISCARDED
// — later checkpoints die with it (the control plane refuses first when
// they have live forks; ZFS refuses at the bottom regardless).
func (a *Agent) Rollback(ctx context.Context, sandboxID, layer string) (nodeapi.SandboxStatus, error) {
	rbStart := time.Now()
	sb, err := a.get(sandboxID)
	if err != nil {
		return nodeapi.SandboxStatus{}, err
	}
	if !a.chunked() {
		return nodeapi.SandboxStatus{}, fmt.Errorf("rollback: requires restore_mode=chunked")
	}
	rb, ok := a.cfg.Storage.(storage.Rollbacker)
	if !ok {
		return nodeapi.SandboxStatus{}, fmt.Errorf("rollback: storage backend cannot roll back")
	}
	idx := -1
	for i, m := range sb.layers {
		if m.LayerID == layer {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nodeapi.SandboxStatus{}, fmt.Errorf("rollback %s: checkpoint %s not in the current chain", sandboxID, layer)
	}

	// Claim the sandbox. From RUNNING the processes die un-snapshotted (the
	// point of a rollback is discarding this epoch); from PAUSED_HOT there
	// is nothing to kill.
	wasRunning := false
	switch st := sb.machine.State(); st {
	case lifecycle.StateRunning:
		if err := sb.machine.CAS(lifecycle.StateRunning, lifecycle.StatePausing); err != nil {
			return nodeapi.SandboxStatus{}, err
		}
		wasRunning = true
		a.killFC(sb)
		a.killUffd(sb)
	case lifecycle.StatePausedHot:
	default:
		return nodeapi.SandboxStatus{}, fmt.Errorf("rollback %s: state %s, want RUNNING or PAUSED_HOT", sandboxID, st)
	}

	if err := rb.RollbackSandbox(ctx, sandboxID, layer); err != nil {
		if wasRunning {
			_ = sb.machine.CAS(lifecycle.StatePausing, lifecycle.StateFailed)
		}
		return nodeapi.SandboxStatus{}, fmt.Errorf("rollback %s to %s: %w", sandboxID, layer, err)
	}

	// Trim the chain. snapCount keeps counting upward — tags stay monotone
	// across the rollback (the standing seq/tag contract).
	discarded := sb.layers[idx+1:]
	sb.layers = sb.layers[:idx+1]
	sb.snapLayer = layer
	targetSeq, _ := layerSeq(layer)
	kept := sb.diskLayers[:0]
	for _, dl := range sb.diskLayers {
		if n, err := layerSeq(dl); err == nil && n <= targetSeq {
			kept = append(kept, dl)
		}
	}
	sb.diskLayers = kept
	for _, m := range discarded {
		_ = os.Remove(filepath.Join(sb.snapDir(), "layer-"+m.LayerID+".json"))
		_ = os.Remove(sb.snapfile(m.LayerID))
		if a.l1 != nil {
			// Best-effort: unreferenced layer objects would otherwise stay
			// GC roots forever (mark-and-sweep roots on layer manifests).
			_ = a.l1.DeleteObject(ctx, KeyLayer(sb.id, m.LayerID))
			_ = a.l1.DeleteObject(ctx, KeySnapfile(sb.id, m.LayerID))
		}
	}
	if a.l1 != nil && len(sb.diskLayers) > 0 {
		// Keep the L1 restore descriptor consistent with the trimmed chain:
		// a node death between rollback and the next pause must restore the
		// checkpoint, not a dangling reference to deleted layer objects.
		_, statErr := os.Stat(sb.wsPath())
		if err := a.pushDescriptor(ctx, sb, statErr == nil); err != nil {
			return nodeapi.SandboxStatus{}, fmt.Errorf("rollback %s: refresh descriptor: %w", sandboxID, err)
		}
	}

	if wasRunning {
		if err := sb.machine.To(lifecycle.StatePausedHot); err != nil {
			return nodeapi.SandboxStatus{}, err
		}
	}
	st, err := a.resume(ctx, sandboxID)
	if err == nil {
		metrics.RestoreSeconds.WithLabelValues("rollback").Observe(time.Since(rbStart).Seconds())
	}
	return st, err
}
