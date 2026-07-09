// Command guestd is the EmberVM in-guest daemon that runs inside each
// template-built microVM: process exec, file I/O, and health reporting on
// TCP :7777 (see pkg/guestd for the handler, pkg/guestapi for wire types).
//
// Template rootfs images boot it as PID 1 (init=/usr/local/bin/guestd). As
// PID 1 it mounts the pseudo-filesystems, then forks itself into a server
// child and spends its life reaping orphans and respawning the child if it
// dies (a tiny systemd Restart=always analogue). Splitting init from server
// keeps the reaper's Wait4 loop from ever racing the server's own os/exec
// waits: reparented orphans go to PID 1, the server's children stay its own.
//
// The M0 bench rootfs (scripts/build-rootfs.sh) keeps systemd+probe-server
// and does not use guestd.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/embervm/embervm/pkg/guestapi"
	"github.com/embervm/embervm/pkg/guestd"
)

const version = "v0.1.0-m1"

// childEnv marks the forked server child so it skips init duties.
const childEnv = "EMBERVM_GUESTD_CHILD"

// defaultPATH is applied when the guest kernel hands PID 1 an empty PATH, so
// exec resolves bare command names (e.g. "echo") and child processes inherit
// a usable PATH.
const defaultPATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

func main() {
	addr := flag.String("addr", fmt.Sprintf(":%d", guestapi.Port), "HTTP listen address")
	flag.Parse()

	if os.Getpid() == 1 && os.Getenv(childEnv) == "" {
		if err := runInit(); err != nil {
			fmt.Fprintf(os.Stderr, "guestd: init: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if os.Getenv("PATH") == "" {
		_ = os.Setenv("PATH", defaultPATH)
	}

	fmt.Printf("guestd listening addr=%s pid=%d version=%s\n", *addr, os.Getpid(), version)
	srv := &http.Server{
		Addr:    *addr,
		Handler: guestd.NewServer(guestd.Options{Version: version}),
		// The only client is the host agent, but a PID-1 daemon's port must
		// not be pinnable by a stuck connection. Header-level bounds only:
		// exec responses legitimately take as long as the command runs.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       5 * time.Minute,
	}
	err := srv.ListenAndServe()
	fmt.Fprintf(os.Stderr, "guestd: serve: %v\n", err)
	os.Exit(1)
}
