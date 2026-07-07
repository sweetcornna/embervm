//go:build linux

package netns

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// DialContext dials addr from inside the lease's network namespace. The
// switch is confined to a throwaway goroutine's locked OS thread: if the
// namespace cannot be restored afterwards the goroutine returns while still
// locked, which retires the (now-poisoned) thread instead of leaking the
// wrong namespace back into the pool of runnable threads.
func (l Lease) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread()

		origin, err := os.Open(fmt.Sprintf("/proc/self/task/%d/ns/net", unix.Gettid()))
		if err != nil {
			runtime.UnlockOSThread()
			ch <- result{nil, fmt.Errorf("open origin netns: %w", err)}
			return
		}
		defer origin.Close()

		target, err := os.Open(l.NetnsPath)
		if err != nil {
			runtime.UnlockOSThread()
			ch <- result{nil, fmt.Errorf("open %s: %w", l.NetnsPath, err)}
			return
		}
		defer target.Close()

		if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
			runtime.UnlockOSThread()
			ch <- result{nil, fmt.Errorf("setns %s: %w", l.NetnsPath, err)}
			return
		}

		conn, dialErr := (&net.Dialer{}).DialContext(ctx, network, addr)

		if err := unix.Setns(int(origin.Fd()), unix.CLONE_NEWNET); err != nil {
			// Cannot restore: do NOT unlock — let the goroutine exit locked
			// so the runtime destroys this thread rather than reusing it in
			// the wrong namespace.
			if conn != nil {
				conn.Close()
			}
			ch <- result{nil, fmt.Errorf("restore netns: %w", err)}
			return
		}
		runtime.UnlockOSThread()
		ch <- result{conn, dialErr}
	}()
	r := <-ch
	return r.conn, r.err
}
