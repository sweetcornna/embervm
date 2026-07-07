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

	"github.com/embervm/embervm/pkg/guestapi"
	"github.com/embervm/embervm/pkg/guestd"
)

const version = "v0.1.0-m1"

// childEnv marks the forked server child so it skips init duties.
const childEnv = "EMBERVM_GUESTD_CHILD"

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

	fmt.Printf("guestd listening addr=%s pid=%d version=%s\n", *addr, os.Getpid(), version)
	err := http.ListenAndServe(*addr, guestd.NewServer(guestd.Options{Version: version}))
	fmt.Fprintf(os.Stderr, "guestd: serve: %v\n", err)
	os.Exit(1)
}
