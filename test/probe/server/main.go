// Command probe-server is a minimal TCP timing probe that runs inside an
// EmberVM guest, typically as a systemd service. The snapshot/restore
// benchmarks use it to answer two questions:
//
//  1. When did the guest become interactive? The host-side probe client
//     (test/probe/client) dials this server in a tight loop and records the
//     instant the first connection succeeds.
//  2. Did the SAME process survive a pause/snapshot/restore cycle? Every
//     accepted connection receives a monotonically increasing sequence
//     number held in process memory. After a restore the next connection
//     must return exactly prev+1; a guest reboot would reset it to 1.
//
// To keep restore numbers honest, the server can dirty an arbitrary amount
// of guest memory before it starts listening (--dirty-mb flag, or the
// "ember.dirty_mb=N" kernel command line token). The buffer is filled with
// a pseudorandom pattern rather than zeros, because zero pages are elided
// by snapshot tooling and would flatter the results.
//
// Stdlib-only and free of build tags; cross-compile with
// CGO_ENABLED=0 GOOS=linux.
package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const cmdlineToken = "ember.dirty_mb="

// dirtyBuf keeps the dirtied memory reachable for the lifetime of the
// process so the pages stay resident and end up in the snapshot.
var dirtyBuf []byte

// connSeq counts accepted connections. It starts at 0, so the first
// connection observes 1.
var connSeq atomic.Uint64

func main() {
	addr := flag.String("addr", ":7777", "TCP listen address")
	dirtyMB := flag.Int("dirty-mb", 0,
		"MiB of memory to allocate and dirty before listening; overrides "+cmdlineToken+"N from /proc/cmdline")
	flag.Parse()

	dirty := *dirtyMB
	flagSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "dirty-mb" {
			flagSet = true
		}
	})
	if !flagSet {
		if v, ok := dirtyMiBFromCmdline("/proc/cmdline"); ok {
			dirty = v
		}
	}
	if dirty < 0 {
		fmt.Fprintf(os.Stderr, "probe-server: invalid dirty MiB %d\n", dirty)
		os.Exit(1)
	}

	// Dirty the memory strictly before listening, so that "probe
	// reachable" implies "dirty memory resident".
	if dirty > 0 {
		dirtyBuf = make([]byte, dirty<<20)
		fillPseudorandom(dirtyBuf)
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe-server: listen %s: %v\n", *addr, err)
		os.Exit(1)
	}

	fmt.Printf("probe-server listening addr=%s dirty_mib=%d pid=%d\n",
		ln.Addr(), dirty, os.Getpid())

	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			fmt.Fprintf(os.Stderr, "probe-server: accept: %v\n", err)
			time.Sleep(10 * time.Millisecond)
			continue
		}
		seq := connSeq.Add(1)
		go answer(conn, seq)
	}
}

// answer writes the sequence number as a decimal line and closes the
// connection. A slow or dead peer must not stall the accept loop, hence
// the per-connection goroutine and the write deadline.
func answer(conn net.Conn, seq uint64) {
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(strconv.FormatUint(seq, 10) + "\n")); err != nil {
		fmt.Fprintf(os.Stderr, "probe-server: write seq %d: %v\n", seq, err)
	}
}

// dirtyMiBFromCmdline scans the kernel command line for cmdlineToken and
// returns its value. It reports ok=false when the file is unreadable (for
// example on non-Linux development hosts) or the token is absent or
// malformed.
func dirtyMiBFromCmdline(path string) (int, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, tok := range strings.Fields(string(raw)) {
		val, found := strings.CutPrefix(tok, cmdlineToken)
		if !found {
			continue
		}
		n, err := strconv.Atoi(val)
		if err != nil || n < 0 {
			fmt.Fprintf(os.Stderr, "probe-server: ignoring malformed cmdline token %q\n", tok)
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// fillPseudorandom fills buf with a fast xorshift64 pattern. Zero pages
// would be skipped by snapshot tooling, so every word must be nonzero
// pseudorandom data, forcing the whole buffer into the snapshot and onto
// the restore path. xorshift64 never reaches state 0 from a nonzero seed,
// so no emitted word is ever zero.
func fillPseudorandom(buf []byte) {
	x := uint64(0x9e3779b97f4a7c15) // arbitrary nonzero seed (2^64 / phi)
	i := 0
	for ; i+8 <= len(buf); i += 8 {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		binary.LittleEndian.PutUint64(buf[i:i+8], x)
	}
	for ; i < len(buf); i++ {
		buf[i] = byte(x)
		x >>= 8
	}
}
