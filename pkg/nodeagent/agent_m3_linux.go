//go:build linux

package nodeagent

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/embervm/embervm/pkg/chunkstore"
	"github.com/embervm/embervm/pkg/lifecycle"
	"github.com/embervm/embervm/pkg/memsnap"
	"github.com/embervm/embervm/pkg/storage"
	"github.com/klauspost/compress/zstd"
)

// keyArtifacts is where ExtractArtifacts leaves the RECYCLED remnant.

// ReleaseLocal frees every node-local resource of a paused sandbox
// (HOT→WARM): dataset, workdir, netns lease, in-memory entry. It refuses
// unless L1 holds the restore descriptor — releasing without it is data
// loss, not tiering. The node-local chunk cache is deliberately untouched
// (content-addressed and shared; eviction is an LRU concern, M4).
func (a *Agent) ReleaseLocal(ctx context.Context, sandboxID string) error {
	sb, err := a.get(sandboxID)
	if err != nil {
		return err
	}
	if st := sb.machine.State(); st != lifecycle.StatePausedHot {
		return fmt.Errorf("release %s: state %s, want %s", sandboxID, st, lifecycle.StatePausedHot)
	}
	if a.l1 == nil {
		return fmt.Errorf("release %s: no L1 configured", sandboxID)
	}
	ok, err := a.l1.HasObject(ctx, KeySnapshotJSON(sandboxID))
	if err != nil {
		return fmt.Errorf("release %s: verify descriptor: %w", sandboxID, err)
	}
	if !ok {
		return fmt.Errorf("release %s: L1 has no restore descriptor; refusing to drop local state", sandboxID)
	}
	// Claim the sandbox (destructive-transition discipline, like the
	// watchdog's reap): winning PAUSED_HOT→STOPPING makes this release the
	// sole actor — a concurrent local resume can no longer enter RESUMING
	// mid-teardown. Losing means a live verb moved it first; abandon. The
	// a.sbx entry is deleted LAST so a cross-node restore keeps waiting the
	// release out (ADR-0004 D2).
	if err := sb.machine.CAS(lifecycle.StatePausedHot, lifecycle.StateStopping); err != nil {
		return fmt.Errorf("release %s: %w", sandboxID, err)
	}

	a.killFC(sb)
	a.killUffd(sb)
	if a.jailed() {
		a.teardownJail(sb)
	}
	a.removeCgroup(sb.id)
	a.clearEgress(ctx, sb)
	sb.lease.Release()
	if err := a.cfg.Storage.DestroySandbox(ctx, sandboxID); err != nil {
		_ = sb.machine.To(lifecycle.StateFailed)
		return fmt.Errorf("release %s: destroy dataset: %w", sandboxID, err)
	}
	if err := os.RemoveAll(sb.dir); err != nil {
		_ = sb.machine.To(lifecycle.StateFailed)
		return fmt.Errorf("release %s: remove workdir: %w", sandboxID, err)
	}
	a.mu.Lock()
	delete(a.sbx, sandboxID)
	a.mu.Unlock()
	return nil
}

// tierStore maps a tier name to its object store.
func (a *Agent) tierStore(tier string) (chunkstore.Backend, error) {
	switch tier {
	case "warm":
		if a.l1 == nil {
			return nil, fmt.Errorf("no L1 store configured")
		}
		return a.l1, nil
	case "cold":
		if a.cold == nil {
			return nil, fmt.Errorf("no cold store configured (EMBERVM_COLD_*)")
		}
		return a.cold, nil
	default:
		return nil, fmt.Errorf("unknown tier %q (want warm|cold)", tier)
	}
}

// handlerEnvForTier builds the uffd handler's child environment so its
// L1-fallback tiering points at the store the snapshot actually lives in:
// for a cold restore, EMBERVM_COLD_* values are re-exported as EMBERVM_L1_*.
func handlerEnvForTier(tier string) []string {
	if tier != "cold" {
		return nil // inherit as-is
	}
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, chunkstore.S3EnvPrefix) {
			continue // drop warm L1 config
		}
		env = append(env, kv)
		if strings.HasPrefix(kv, chunkstore.ColdEnvPrefix) {
			env = append(env, chunkstore.S3EnvPrefix+strings.TrimPrefix(kv, chunkstore.ColdEnvPrefix))
		}
	}
	return env
}

// Prewarm pulls a paused sandbox's working-set chunks from the tier's
// store into the node-local cache so a subsequent restore's eager WS
// prefetch hits locally (predicted-wake pull, docs/zh/04 #5). Without a
// recorded working set every referenced chunk is pulled instead.
func (a *Agent) Prewarm(ctx context.Context, sandboxID, tier string) error {
	src, err := a.tierStore(tier)
	if err != nil {
		return fmt.Errorf("prewarm %s: %w", sandboxID, err)
	}
	if a.localStore == nil {
		return fmt.Errorf("prewarm %s: requires restore_mode=chunked", sandboxID)
	}
	var desc SnapshotDescriptor
	if err := getJSONFrom(ctx, src, KeySnapshotJSON(sandboxID), &desc); err != nil {
		return fmt.Errorf("prewarm %s: descriptor: %w", sandboxID, err)
	}
	layers := make([]*memsnap.Manifest, 0, len(desc.Layers))
	for _, layer := range desc.Layers {
		rc, err := src.GetObject(ctx, KeyLayer(sandboxID, layer))
		if err != nil {
			return fmt.Errorf("prewarm %s: manifest %s: %w", sandboxID, layer, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return err
		}
		m, err := memsnap.ParseManifest(data)
		if err != nil {
			return fmt.Errorf("prewarm %s: %w", sandboxID, err)
		}
		layers = append(layers, m)
	}
	view, err := memsnap.Resolve(layers)
	if err != nil {
		return fmt.Errorf("prewarm %s: %w", sandboxID, err)
	}

	var hashes []string
	seen := map[string]bool{}
	addChunk := func(ref memsnap.ChunkRef) {
		if !ref.Zero && ref.Hash != "" && !seen[ref.Hash] {
			seen[ref.Hash] = true
			hashes = append(hashes, ref.Hash)
		}
	}
	ws := readWSChunks(ctx, src, sandboxID)
	if desc.HasWS && len(ws) > 0 {
		for _, ci := range ws {
			if ci >= 0 && ci < len(view.Chunks) {
				addChunk(view.Chunks[ci])
			}
		}
	} else {
		for _, ref := range view.Chunks {
			addChunk(ref)
		}
	}
	n, err := (chunkstore.Copier{Src: src, Dst: a.localStore}).Copy(ctx, hashes)
	if err != nil {
		return fmt.Errorf("prewarm %s: pull chunks: %w", sandboxID, err)
	}
	log.Printf("nodeagent: prewarmed %s from %s: %d/%d chunks pulled", sandboxID, tier, n, len(hashes))
	return nil
}

// readWSChunks best-effort loads the recorded working-set chunk order.
func readWSChunks(ctx context.Context, src chunkstore.Objects, sandboxID string) []int {
	rc, err := src.GetObject(ctx, KeyWS(sandboxID))
	if err != nil {
		return nil
	}
	defer rc.Close()
	var trace struct {
		Chunks []int `json:"chunks"`
	}
	if err := decodeJSONBody(rc, &trace); err != nil {
		return nil
	}
	return trace.Chunks
}

// ExtractArtifacts materializes the archived disk of a COLD sandbox on this
// node, tars the requested guest paths (missing ones are skipped, not
// errors), and stores sandboxes/<id>/artifacts.tar.zst in the cold store —
// the only remnant a RECYCLED sandbox keeps (docs/zh/03 §3 选择性恢复).
func (a *Agent) ExtractArtifacts(ctx context.Context, sandboxID string, paths []string) error {
	repl, ok := a.cfg.Storage.(storage.Replicator)
	if !ok {
		return fmt.Errorf("extract %s: %w", sandboxID, storage.ErrReplicationUnsupported)
	}
	if a.cold == nil || a.l1 == nil {
		return fmt.Errorf("extract %s: needs both L1 and cold stores", sandboxID)
	}
	var desc SnapshotDescriptor
	if err := getJSONFrom(ctx, a.cold, KeySnapshotJSON(sandboxID), &desc); err != nil {
		return fmt.Errorf("extract %s: descriptor: %w", sandboxID, err)
	}

	// Disk chain: template lineage from L1, deltas from the cold store.
	if err := a.receiveObject(ctx, KeyTemplateStream(desc.TemplateID), func(r io.Reader) error {
		return repl.ReceiveTemplate(ctx, desc.TemplateID, r)
	}); err != nil {
		return fmt.Errorf("extract %s: template: %w", sandboxID, err)
	}
	_ = a.cfg.Storage.DestroySandbox(ctx, sandboxID) // scratch must start clean
	for _, layer := range desc.DiskLayers {
		rc, err := a.cold.GetObject(ctx, KeyDiskDelta(sandboxID, layer))
		if err != nil {
			return fmt.Errorf("extract %s: disk %s: %w", sandboxID, layer, err)
		}
		err = repl.ReceiveSnapshotDelta(ctx, sandboxID, desc.TemplateID, rc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("extract %s: receive %s: %w", sandboxID, layer, err)
		}
	}
	defer func() { _ = a.cfg.Storage.DestroySandbox(ctx, sandboxID) }()

	// Read-only loop mounts; data.raw may be unformatted — skip it then.
	scratchPaths := a.cfg.Storage.Paths(sandboxID)
	var mounts []string
	rootMnt, err := mountRO(scratchPaths.RootfsExt4)
	if err != nil {
		return fmt.Errorf("extract %s: mount rootfs: %w", sandboxID, err)
	}
	mounts = append(mounts, rootMnt)
	if dataMnt, err := mountRO(scratchPaths.DataRaw); err == nil {
		mounts = append(mounts, dataMnt)
	}
	defer func() {
		for _, m := range mounts {
			_ = exec.Command("umount", m).Run()
			_ = os.Remove(m)
		}
	}()

	// The tarball is the RECYCLED remnant and lives in the COLD store —
	// the engine's prune keeps exactly this key there.
	return putStreamTo(ctx, a.cold, KeyArtifacts(sandboxID), func(w io.Writer) error {
		return writeArtifactsTar(w, mounts, paths)
	})
}

// mountRO loop-mounts an image read-only and returns the mountpoint.
func mountRO(image string) (string, error) {
	mnt, err := os.MkdirTemp("", "ember-extract-*")
	if err != nil {
		return "", err
	}
	// noload: do not replay the ext4 journal on a read-only mount.
	out, err := exec.Command("mount", "-o", "loop,ro,noload", image, mnt).CombinedOutput()
	if err != nil {
		_ = os.Remove(mnt)
		return "", fmt.Errorf("mount %s: %w: %s", image, err, out)
	}
	return mnt, nil
}

// writeArtifactsTar streams a zstd tar of the requested guest paths,
// searching each mount in order and preserving guest-absolute names.
func writeArtifactsTar(w io.Writer, mounts, paths []string) error {
	zw, err := zstd.NewWriter(w)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(zw)
	for _, guestPath := range paths {
		if !filepath.IsAbs(guestPath) {
			return fmt.Errorf("artifact path %q must be absolute", guestPath)
		}
		for _, mnt := range mounts {
			hostPath := filepath.Join(mnt, filepath.Clean(guestPath))
			if _, err := os.Lstat(hostPath); err != nil {
				continue // not on this disk
			}
			if err := addTree(tw, hostPath, strings.TrimPrefix(guestPath, "/")); err != nil {
				return err
			}
			break
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return zw.Close()
}

// addTree tars one file or directory tree under the given archive name.
func addTree(tw *tar.Writer, hostPath, name string) error {
	return filepath.Walk(hostPath, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(hostPath, p)
		if err != nil {
			return err
		}
		entry := name
		if rel != "." {
			entry = name + "/" + filepath.ToSlash(rel)
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = entry
		if info.IsDir() {
			hdr.Name += "/"
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return nil // artifacts are files/dirs; devices and links are skipped
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := io.Copy(tw, f); err != nil {
				return err
			}
		}
		return nil
	})
}

// getJSONFrom decodes a named object from an explicit store.
func getJSONFrom(ctx context.Context, b chunkstore.Objects, key string, v any) error {
	rc, err := b.GetObject(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()
	return decodeJSONBody(rc, v)
}
