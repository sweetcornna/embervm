//go:build linux

package nodeagent

import (
	"context"
	"fmt"
	"log"
	"os"
	"syscall"
	"time"

	"github.com/embervm/embervm/pkg/lifecycle"
	"github.com/embervm/embervm/pkg/metrics"
)

// The G5 zombie reaper. A dead Firecracker with a live uffd handler (or the
// reverse) leaves a sandbox that will never answer again — and a hung vCPU
// cannot be waited on. The watchdog turns both cases into a clean FAILED:
// processes killed, jail and lease released, local state dropped. Recovery
// is the M4 resume path — the last write-through snapshot restores on any
// node; state since that snapshot is lost, which is what crash semantics
// mean. The uffd handler self-exits via --parent-pid when the AGENT dies;
// this is the FC-side mirror running while the agent lives.

// StartWatchdog launches the reaper loop; call once per agent daemon.
func (a *Agent) StartWatchdog(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				a.reapZombies(ctx)
			}
		}
	}()
}

// reapZombies scans for RUNNING sandboxes whose FC or uffd process died
// behind the agent's back. Only RUNNING: during RESUMING sb.fc still points
// at the previous (deliberately killed) process until launchFC swaps it in,
// and a resume that fails marks itself FAILED anyway.
func (a *Agent) reapZombies(ctx context.Context) {
	type victim struct {
		sb    *sandbox
		cause string
	}
	var victims []victim
	a.mu.Lock()
	for _, sb := range a.sbx {
		if sb.machine.State() != lifecycle.StateRunning {
			continue
		}
		if sb.fc != nil && sb.fc.Process != nil && !childAlive(sb.fc.Process.Pid) {
			victims = append(victims, victim{sb, "firecracker process died"})
			continue
		}
		// A dead memory handler means every future page fault hangs the
		// vCPU forever (docs/zh/04 §6) — the VM is unrecoverable in place.
		if sb.uffd != nil && sb.uffd.Process != nil && !childAlive(sb.uffd.Process.Pid) {
			victims = append(victims, victim{sb, "uffd handler died"})
		}
	}
	a.mu.Unlock()

	for _, v := range victims {
		a.reap(ctx, v.sb, v.cause)
	}
}

// reap force-releases everything a zombie held and records the FAILED id
// for the next Healthz poll to write through to the control plane.
// CAS-first (the destructive-transition discipline): winning RUNNING→FAILED
// makes the watchdog the sandbox's only actor; losing means a live verb
// (pause, stop) moved it between scan and reap, and the reap is abandoned.
func (a *Agent) reap(ctx context.Context, sb *sandbox, cause string) {
	if err := sb.machine.CAS(lifecycle.StateRunning, lifecycle.StateFailed); err != nil {
		return
	}
	log.Printf("nodeagent watchdog: reaping %s: %s", sb.id, cause)
	// Off the books before cleanup: a.get() must stop resolving the id so
	// no concurrent verb operates on a sandbox being dismantled.
	a.mu.Lock()
	delete(a.sbx, sb.id)
	a.failed = append(a.failed, fmt.Sprintf("%s: %s", sb.id, cause))
	a.mu.Unlock()

	a.killFC(sb)
	a.killUffd(sb)
	if a.jailed() {
		a.teardownJail(sb)
	}
	a.removeCgroup(sb.id)
	a.clearEgress(ctx, sb)
	sb.lease.Release()
	_ = os.RemoveAll(sb.dir)
	_ = a.cfg.Storage.DestroySandbox(ctx, sb.id)
	metrics.WatchdogReaps.Inc()
}

// childAlive reports whether our child pid is still running. A signal-0
// probe is NOT enough: an externally-killed child nobody Wait()ed is a
// zombie, and zombies still answer kill(pid, 0). Probe with a non-blocking
// wait instead — collecting the corpse IS the death notification. killFC/
// killUffd ignore their subsequent Wait errors (ECHILD), so consuming the
// status here is safe.
func childAlive(pid int) bool {
	var ws syscall.WaitStatus
	for {
		rc, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
		switch {
		case err == syscall.EINTR:
			continue
		case err == syscall.ECHILD:
			return false // already reaped elsewhere: dead
		case err != nil:
			return true // conservative: never reap on probe errors
		case rc == 0:
			return true // still running
		default:
			return false // rc == pid: we just collected the corpse
		}
	}
}
