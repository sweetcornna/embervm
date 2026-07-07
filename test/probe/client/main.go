// Command probe-client runs on the HOST, inside the sandbox network
// namespace, and detects the exact moment the guest probe server
// (test/probe/server) becomes reachable. It dials the guest in a tight
// loop and, on the first successful connection, reads the server's
// sequence number and prints exactly one JSON line to stdout:
//
//	{"success_unix_ns":<ns>,"seq":<n>,"attempts":<n>}
//
// The timestamp is captured immediately after the read completes, so it
// marks the instant the guest was proven interactive end to end (TCP
// handshake plus a userspace reply, not just a SYN-ACK from the kernel).
// On overall timeout it prints {"error":"timeout","attempts":<n>} and
// exits 1. Stdout carries exactly one line either way; diagnostics go to
// stderr. Stdlib-only and portable.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", "172.16.0.2:7777", "guest probe server address")
	timeout := flag.Duration("timeout", 60*time.Second, "overall time budget for the probe")
	interval := flag.Duration("interval", 2*time.Millisecond, "sleep between failed attempts")
	dialTimeout := flag.Duration("dial-timeout", 25*time.Millisecond, "per-attempt TCP dial timeout")
	flag.Parse()

	deadline := time.Now().Add(*timeout)
	attempts := 0
	for time.Now().Before(deadline) {
		attempts++
		if seq, ns, ok := attempt(*addr, *dialTimeout, deadline); ok {
			fmt.Printf("{\"success_unix_ns\":%d,\"seq\":%d,\"attempts\":%d}\n", ns, seq, attempts)
			return
		}
		time.Sleep(*interval)
	}
	fmt.Printf("{\"error\":\"timeout\",\"attempts\":%d}\n", attempts)
	os.Exit(1)
}

// attempt makes a single dial+read attempt. On success it returns the
// sequence number reported by the guest and the timestamp captured
// immediately after the read completed.
func attempt(addr string, dialTimeout time.Duration, deadline time.Time) (seq int, unixNS int64, ok bool) {
	// Never let a single attempt outlive the overall budget.
	if remaining := time.Until(deadline); remaining < dialTimeout {
		dialTimeout = remaining
	}
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return 0, 0, false
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(deadline)
	line, err := bufio.NewReader(conn).ReadString('\n')
	unixNS = time.Now().UnixNano() // capture before any further work
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe-client: read: %v\n", err)
		return 0, 0, false
	}
	seq, err = strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe-client: bad sequence line %q: %v\n", line, err)
		return 0, 0, false
	}
	return seq, unixNS, true
}
