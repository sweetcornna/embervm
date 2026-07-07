//go:build unix

package guestd

import (
	"os/exec"
	"syscall"
)

// setPgid places the child in its own process group so a timeout can kill
// the whole tree (e.g. sh -c "sleep 5" forks sleep into the same group).
func setPgid(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup delivers SIGKILL to the child's entire process group.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
