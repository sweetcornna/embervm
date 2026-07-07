//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
)

// mountAll mounts the pseudo-filesystems a docker-exported rootfs lacks.
// Every mount is best-effort: the guest kernel may have auto-mounted some
// (CONFIG_DEVTMPFS_MOUNT), which surfaces as EBUSY.
func mountAll() {
	mounts := []struct {
		src, dst, typ string
	}{
		{"proc", "/proc", "proc"},
		{"sysfs", "/sys", "sysfs"},
		{"devtmpfs", "/dev", "devtmpfs"},
		{"tmpfs", "/tmp", "tmpfs"},
		{"tmpfs", "/run", "tmpfs"},
	}
	for _, m := range mounts {
		if err := os.MkdirAll(m.dst, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "guestd: mkdir %s: %v\n", m.dst, err)
			continue
		}
		if err := unix.Mount(m.src, m.dst, m.typ, 0, ""); err != nil && !errors.Is(err, unix.EBUSY) {
			fmt.Fprintf(os.Stderr, "guestd: mount %s: %v\n", m.dst, err)
		}
	}
}

// spawnChild re-executes this binary with the same flags as the server
// child. PID 1 reaps it via Wait4, so the process handle is released.
func spawnChild() (int, error) {
	cmd := exec.Command("/proc/self/exe", os.Args[1:]...)
	cmd.Env = append(os.Environ(), childEnv+"=1")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Release(); err != nil {
		return 0, err
	}
	return pid, nil
}

// runInit is the PID 1 loop: mount, spawn the server child, reap every
// terminated process, and respawn the child if it exits.
func runInit() error {
	mountAll()
	for {
		child, err := spawnChild()
		if err != nil {
			return fmt.Errorf("spawn server child: %w", err)
		}
		for {
			var ws unix.WaitStatus
			pid, err := unix.Wait4(-1, &ws, 0, nil)
			if err == unix.EINTR {
				continue
			}
			if err != nil {
				return fmt.Errorf("wait4: %w", err)
			}
			if pid == child {
				break
			}
		}
		fmt.Fprintln(os.Stderr, "guestd: server child exited; respawning in 1s")
		time.Sleep(time.Second)
	}
}
