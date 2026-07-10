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
// Firecracker pid into it. Failures are ignored: strict cgroup enforcement
// is part of the M4 host-hardening pass, and M1 must keep booting on runners
// where controllers are not delegated (memory.max needs the memory
// controller enabled in the parent's subtree_control, which some CI hosts
// disallow).
func (a *Agent) placeCgroup(sandboxID string, pid, memMiB, cpuQuotaVCPUs int) {
	if err := os.MkdirAll(a.cfg.CgroupRoot, 0o755); err != nil {
		return
	}
	// Delegate the memory and cpu controllers to our subtree so memory.max /
	// cpu.max work in the per-sandbox slice; ignore failure (best-effort).
	_ = os.WriteFile(filepath.Join(a.cfg.CgroupRoot, "cgroup.subtree_control"), []byte("+memory +cpu"), 0o644)

	dir := filepath.Join(a.cfg.CgroupRoot, sandboxID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	// +256 MiB headroom for the VMM and page cache.
	limit := int64(memMiB+256) << 20
	_ = os.WriteFile(filepath.Join(dir, "memory.max"), []byte(strconv.FormatInt(limit, 10)), 0o644)
	if cpuQuotaVCPUs > 0 {
		_ = a.writeCPUMax(sandboxID, cpuQuotaVCPUs)
	}
	// Placing the pid is the part that matters for M1; keep its error visible.
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "nodeagent: cgroup place pid %d: %v\n", pid, err)
	}
}

// writeCPUMax clamps the sandbox slice to quotaVCPUs cores' worth of CPU
// time (M6 CPU resize: the VM boots with max_vcpus threads; this quota is
// the effective compute). 0 removes the clamp. The error is surfaced
// because the resize verb must not claim a quota it failed to apply; the
// boot path ignores it like the rest of the best-effort cgroup handling.
func (a *Agent) writeCPUMax(sandboxID string, quotaVCPUs int) error {
	val := "max 100000"
	if quotaVCPUs > 0 {
		val = strconv.Itoa(quotaVCPUs*100000) + " 100000"
	}
	return os.WriteFile(filepath.Join(a.cfg.CgroupRoot, sandboxID, "cpu.max"), []byte(val), 0o644)
}

// removeCgroup removes the sandbox's cgroup slice (best-effort; a non-empty
// slice or missing dir is fine to ignore).
func (a *Agent) removeCgroup(sandboxID string) {
	_ = os.Remove(filepath.Join(a.cfg.CgroupRoot, sandboxID))
}
