//go:build linux

package nodeagent

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// placeCgroup best-effort creates a cgroup v2 slice for the sandbox, sets a
// memory ceiling (guest memory plus a modest VMM overhead), and moves the
// Firecracker pid into it. Failures are logged and ignored: strict cgroup
// enforcement is part of the M4 host-hardening pass, and M1 must keep
// booting on runners where controllers are not delegated.
func (a *Agent) placeCgroup(sandboxID string, pid, memMiB int) {
	dir := filepath.Join(a.cfg.CgroupRoot, sandboxID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "nodeagent: cgroup mkdir %s: %v\n", dir, err)
		return
	}
	// +256 MiB headroom for the VMM and page cache.
	limit := int64(memMiB+256) << 20
	if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(strconv.FormatInt(limit, 10)), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "nodeagent: cgroup memory.max: %v\n", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "nodeagent: cgroup.procs: %v\n", err)
	}
}

// removeCgroup removes the sandbox's cgroup slice (best-effort; a non-empty
// slice or missing dir is fine to ignore).
func (a *Agent) removeCgroup(sandboxID string) {
	_ = os.Remove(filepath.Join(a.cfg.CgroupRoot, sandboxID))
}
