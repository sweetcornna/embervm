//go:build linux

package guestd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// ptyShell is a login shell under a Linux PTY, built on /dev/ptmx directly
// (golang.org/x/sys is already a dependency; a pty library is not worth a
// new module for ~50 lines).
type ptyShell struct {
	master   *os.File
	cmd      *exec.Cmd
	waitOnce sync.Once
	exitCode int
}

// startShell opens a PTY pair and runs /bin/sh -l on the slave side (every
// template image has a POSIX sh; busybox and dash both qualify).
func startShell(cols, rows int) (termProc, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		// Minimal guests may lack the devtmpfs ptmx node; the devpts mount
		// (guestd's own init mounts it with ptmxmode=0666) always has one.
		master, err = os.OpenFile("/dev/pts/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("open ptmx: %w", err)
	}
	fd := int(master.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		return nil, fmt.Errorf("unlock pty: %w", err)
	}
	ptn, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		master.Close()
		return nil, fmt.Errorf("pty number: %w", err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", ptn), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, fmt.Errorf("open pts: %w", err)
	}

	cmd := exec.Command("/bin/sh", "-l")
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	// Setsid gives the shell its own session and process group (so kill()
	// can SIGKILL the whole tree); Setctty makes the slave (fd 0 in the
	// child) its controlling terminal, which is what delivers ^C/^Z.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		slave.Close()
		master.Close()
		return nil, fmt.Errorf("start shell: %w", err)
	}
	slave.Close() // the child holds its own copies

	p := &ptyShell{master: master, cmd: cmd}
	p.resize(cols, rows)
	return p, nil
}

func (p *ptyShell) Read(b []byte) (int, error)  { return p.master.Read(b) }
func (p *ptyShell) Write(b []byte) (int, error) { return p.master.Write(b) }

func (p *ptyShell) resize(cols, rows int) {
	if cols <= 0 || rows <= 0 || cols > 1000 || rows > 1000 {
		return
	}
	_ = unix.IoctlSetWinsize(int(p.master.Fd()), unix.TIOCSWINSZ,
		&unix.Winsize{Col: uint16(cols), Row: uint16(rows)})
}

func (p *ptyShell) wait() int {
	p.waitOnce.Do(func() {
		if err := p.cmd.Wait(); err != nil {
			p.exitCode = -1
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				p.exitCode = exitErr.ExitCode() // -1 when signal-killed
			}
		}
	})
	return p.exitCode
}

func (p *ptyShell) kill() {
	// Setsid made the child a group leader: -pid addresses the whole tree.
	// ESRCH (already exited) is fine.
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	p.wait()
	p.master.Close()
}
