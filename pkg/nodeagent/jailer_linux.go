//go:build linux

package nodeagent

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

// jailerExecPath is the stable-named Firecracker binary handed to the
// jailer. The jailer builds its chroot at <chroot-base>/<exec-file
// basename>/<id>/root, so the versioned asset name (firecracker-v1.16.1-
// x86_64) would put the jail somewhere jailRoot() — and every path derived
// from it — does not point. Staging a hard link named exactly "firecracker"
// pins the layout regardless of the FC release in use.
func (a *Agent) jailerExecPath() string {
	return filepath.Join(a.cfg.JailerChrootBase, "bin", "firecracker")
}

// stageJailerExec places cfg.FCBin at jailerExecPath(): hard link on the
// same filesystem, copy across filesystems, atomically renamed so
// concurrent jail builds never see a partial binary.
func (a *Agent) stageJailerExec(sb *sandbox) error {
	dst := a.jailerExecPath()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + "." + sb.id
	_ = os.Remove(tmp)
	if err := os.Link(a.cfg.FCBin, tmp); err != nil {
		if err := copyFileSimple(tmp, a.cfg.FCBin); err != nil {
			return err
		}
		if err := os.Chmod(tmp, 0o755); err != nil {
			return err
		}
	}
	return os.Rename(tmp, dst)
}

// buildJail assembles the chroot: directories, bind mounts, the guest
// kernel, and ownership. Idempotent (re-binding over an existing mount is
// prevented by tearing down first).
func (a *Agent) buildJail(sb *sandbox) error {
	if err := a.teardownJail(sb); err != nil { // clean slate; stale mounts poison snapshots
		return fmt.Errorf("clean slate: %w", err)
	}
	if err := a.stageJailerExec(sb); err != nil {
		return fmt.Errorf("stage jailer exec file: %w", err)
	}
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

// teardownJail unmounts and removes the chroot. Safe when absent. Most
// callers (cleanup, reap, release) ignore the error — jail litter is
// harmless — but buildJail must not proceed over a live mount.
func (a *Agent) teardownJail(sb *sandbox) error {
	root := a.jailRoot(sb.id)
	binds := []string{filepath.Join(root, "snap"), filepath.Join(root, "data")}
	for _, m := range binds {
		if !isMountpoint(m) {
			continue
		}
		if out, err := exec.Command("umount", m).CombinedOutput(); err != nil {
			log.Printf("nodeagent: teardown jail %s: umount %s: %v: %s", sb.id, m, err, out)
		}
	}
	// RemoveAll through a still-live bind would recurse into the REAL
	// dataset (rootfs.ext4, data.raw) and delete it — jail litter is
	// recoverable, a deleted dataset is not. Verify before removing.
	for _, m := range binds {
		if isMountpoint(m) {
			err := fmt.Errorf("teardown jail %s: %s still mounted; refusing to remove the jail dir", sb.id, m)
			log.Printf("nodeagent: %v", err)
			return err
		}
	}
	_ = os.RemoveAll(filepath.Join(a.cfg.JailerChrootBase, "firecracker", sb.id))
	return nil
}

// isMountpoint reports whether path is a mount target per /proc/self/mounts.
// (A device-id comparison cannot detect a same-filesystem bind mount, which
// is exactly what the plain backend produces.) On a read error it reports
// true — the caller then refuses destructive action, the safe direction.
func isMountpoint(path string) bool {
	data, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return true
	}
	clean := filepath.Clean(path)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && unescapeMount(fields[1]) == clean {
			return true
		}
	}
	return false
}

// unescapeMount decodes the octal escapes /proc/self/mounts uses for
// whitespace in paths (\040 etc.).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// jailerCommand builds the jailer invocation that execs Firecracker inside
// the chroot, netns, and per-VM uid/gid with default seccomp.
func (a *Agent) jailerCommand(sb *sandbox) *exec.Cmd {
	uid := strconv.Itoa(a.jailUIDFor(sb))
	return exec.Command(a.cfg.JailerBin,
		"--id", sb.id,
		"--exec-file", a.jailerExecPath(),
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
