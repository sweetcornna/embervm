//go:build linux

package nodeagent

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/embervm/embervm/pkg/chunkstore"
	"github.com/embervm/embervm/pkg/lifecycle"
	"github.com/embervm/embervm/pkg/storage"
	"github.com/klauspost/compress/zstd"
)

// keyArtifacts is where ExtractArtifacts leaves the RECYCLED remnant.
func keyArtifacts(id string) string { return "sandboxes/" + id + "/artifacts.tar.zst" }

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
	ok, err := a.l1.HasObject(ctx, keySnapshotJSON(sandboxID))
	if err != nil {
		return fmt.Errorf("release %s: verify descriptor: %w", sandboxID, err)
	}
	if !ok {
		return fmt.Errorf("release %s: L1 has no restore descriptor; refusing to drop local state", sandboxID)
	}

	a.killFC(sb)
	a.killUffd(sb)
	a.removeCgroup(sb.id)
	sb.lease.Release()
	if err := a.cfg.Storage.DestroySandbox(ctx, sandboxID); err != nil {
		return fmt.Errorf("release %s: destroy dataset: %w", sandboxID, err)
	}
	if err := os.RemoveAll(sb.dir); err != nil {
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
	var desc snapshotDescriptor
	if err := getJSONFrom(ctx, a.cold, keySnapshotJSON(sandboxID), &desc); err != nil {
		return fmt.Errorf("extract %s: descriptor: %w", sandboxID, err)
	}

	// Disk chain: template lineage from L1, deltas from the cold store.
	if err := a.receiveObject(ctx, keyTemplateStream(desc.TemplateID), func(r io.Reader) error {
		return repl.ReceiveTemplate(ctx, desc.TemplateID, r)
	}); err != nil {
		return fmt.Errorf("extract %s: template: %w", sandboxID, err)
	}
	_ = a.cfg.Storage.DestroySandbox(ctx, sandboxID) // scratch must start clean
	for _, layer := range desc.DiskLayers {
		rc, err := a.cold.GetObject(ctx, keyDiskDelta(sandboxID, layer))
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

	return a.putStream(ctx, keyArtifacts(sandboxID), func(w io.Writer) error {
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
