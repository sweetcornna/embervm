//go:build linux

package nodeagent

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// The M4 jailer hardening (docs/zh/04 §5, the debt ADR-0002 deferred).
// Every Firecracker process runs chrooted with a per-VM uid/gid inside its
// sandbox netns, with default seccomp ON. The chroot sees exactly two bind
// mounts:
//
//	/data ← the sandbox dataset dir (rootfs.ext4, data.raw)
//	/snap ← the workdir snap dir (snapfiles, uffd.sock, memfile staging)
//
// so every path Firecracker records in a snapshot is chroot-relative and
// IDENTICAL on every node — which is what makes template-snapshot
// fast-create and cross-node restore path-stable (retiring the M3
// mountpoint pinning for jailed deployments).

// jailed reports whether this agent runs Firecracker under the jailer.
func (a *Agent) jailed() bool { return a.cfg.JailerBin != "" }

// jailRoot is the chroot root the jailer builds for a sandbox.
func (a *Agent) jailRoot(id string) string {
	return filepath.Join(a.cfg.JailerChrootBase, "firecracker", id, "root")
}

// jailUIDFor derives the per-VM uid/gid from the netns slot (host-unique
// even across multiple agents on one machine).
func (a *Agent) jailUIDFor(sb *sandbox) int {
	base := a.cfg.JailUIDBase
	if base <= 0 {
		base = 30000
	}
	return base + sb.lease.ID
}

// fcSnapPath is the snapshot-artifact path as Firecracker sees it.
func (a *Agent) fcSnapPath(sb *sandbox, name string) string {
	if a.jailed() {
		return "/snap/" + name
	}
	return filepath.Join(sb.snapDir(), name)
}

// fcDrivePath is a drive path as Firecracker sees it.
func (a *Agent) fcDrivePath(sb *sandbox, hostPath string) string {
	if a.jailed() {
		return "/data/" + filepath.Base(hostPath)
	}
	return hostPath
}

// fcAPISock is the Firecracker API socket host path.
func (a *Agent) fcAPISock(sb *sandbox) string {
	if a.jailed() {
		return filepath.Join(a.jailRoot(sb.id), "run", "firecracker.socket")
	}
	return filepath.Join(sb.dir, "fc.sock")
}

// buildJail assembles the chroot: directories, bind mounts, the guest
// kernel, and ownership. Idempotent (re-binding over an existing mount is
// prevented by tearing down first).
func (a *Agent) buildJail(sb *sandbox) error {
	a.teardownJail(sb) // clean slate; stale mounts poison snapshots
	root := a.jailRoot(sb.id)
	uid := a.jailUIDFor(sb)
	for _, d := range []string{
		filepath.Join(root, "data"),
		filepath.Join(root, "snap"),
		filepath.Join(root, "run"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	datasetDir := filepath.Dir(sb.rootfs)
	if out, err := exec.Command("mount", "--bind", datasetDir, filepath.Join(root, "data")).CombinedOutput(); err != nil {
		return fmt.Errorf("bind dataset: %w: %s", err, out)
	}
	if err := os.MkdirAll(sb.snapDir(), 0o755); err != nil {
		return err
	}
	if out, err := exec.Command("mount", "--bind", sb.snapDir(), filepath.Join(root, "snap")).CombinedOutput(); err != nil {
		return fmt.Errorf("bind snap: %w: %s", err, out)
	}
	// The kernel must be inside the chroot; hard-link (same fs) or copy.
	kernelDst := filepath.Join(root, "vmlinux")
	_ = os.Remove(kernelDst)
	if err := os.Link(a.cfg.KernelPath, kernelDst); err != nil {
		if err := copyFileSimple(kernelDst, a.cfg.KernelPath); err != nil {
			return fmt.Errorf("place kernel in jail: %w", err)
		}
	}
	// Firecracker (jail uid) must read/write its drives and staging dirs.
	for _, p := range []string{sb.rootfs, sb.dataRaw} {
		if err := os.Chown(p, uid, uid); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("chown %s: %w", p, err)
		}
	}
	for _, p := range []string{root, filepath.Join(root, "run"), filepath.Join(root, "snap"), kernelDst} {
		if err := os.Chown(p, uid, uid); err != nil {
			return fmt.Errorf("chown %s: %w", p, err)
		}
	}
	return nil
}

// teardownJail unmounts and removes the chroot. Safe when absent.
func (a *Agent) teardownJail(sb *sandbox) {
	root := a.jailRoot(sb.id)
	for _, m := range []string{filepath.Join(root, "snap"), filepath.Join(root, "data")} {
		_ = exec.Command("umount", m).Run()
	}
	_ = os.RemoveAll(filepath.Join(a.cfg.JailerChrootBase, "firecracker", sb.id))
}

// jailerCommand builds the jailer invocation that execs Firecracker inside
// the chroot, netns, and per-VM uid/gid with default seccomp.
func (a *Agent) jailerCommand(sb *sandbox) *exec.Cmd {
	uid := strconv.Itoa(a.jailUIDFor(sb))
	return exec.Command(a.cfg.JailerBin,
		"--id", sb.id,
		"--exec-file", a.cfg.FCBin,
		"--uid", uid,
		"--gid", uid,
		"--chroot-base-dir", a.cfg.JailerChrootBase,
		"--netns", sb.lease.NetnsPath,
		"--",
		"--api-sock", "/run/firecracker.socket",
	)
}

func copyFileSimple(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
