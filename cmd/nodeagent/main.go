// Command nodeagent is the EmberVM node daemon (root): it owns Firecracker
// processes, the netns pool, cgroups, ZFS storage, and the in-guest daemon
// connections, and serves the node Agent API over a unix socket so a
// separate API server can drive sandbox lifecycles (see pkg/nodeapi).
//
// `embervm dev` wires the same agent in-process instead of over the socket.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/embervm/embervm/pkg/metrics"
	"github.com/embervm/embervm/pkg/netns"
	"github.com/embervm/embervm/pkg/nodeagent"
	"github.com/embervm/embervm/pkg/nodeapi"
	"github.com/embervm/embervm/pkg/storage"
)

func main() {
	var (
		socket      = flag.String("socket", "/run/embervm/nodeagent.sock", "unix socket to serve the node API on")
		pool        = flag.String("pool", "embervm", "ZFS pool for sandbox datasets")
		plainRoot   = flag.String("plain-root", "", "use a plain-directory storage backend rooted here instead of ZFS")
		netnsPool   = flag.Int("netns-pool", 24, "number of pre-created netns slots")
		netnsBase   = flag.Int("netns-base", 0, "first netns slot id (multiple daemons on one host partition the ember<N> range)")
		scriptDir   = flag.String("script-dir", "scripts", "directory containing setup/teardown-network.sh")
		workDir     = flag.String("work-dir", "/var/lib/embervm/work", "per-sandbox runtime state directory")
		kernel      = flag.String("kernel", "", "guest kernel (vmlinux) path")
		fcBin       = flag.String("fc-bin", "firecracker", "firecracker binary")
		uffdBin     = flag.String("uffd-handler", "uffd-handler", "uffd handler binary")
		guestdBin   = flag.String("guestd-bin", "", "guestd binary to inject into templates")
		restoreMode = flag.String("restore-mode", "prefetch", "uffd restore mode: chunked|prefetch|lazy|file")
		watchdog    = flag.Duration("watchdog-interval", 5*time.Second, "zombie-reaper scan interval (0 disables)")
		chunkDir    = flag.String("chunk-store-dir", "", "node-local chunk cache for --restore-mode=chunked (default <work-dir>/chunks)")
		capacity    = flag.Int("capacity-mib", 0, "memory the scheduler may pack onto this node (0 = unlimited)")
		fcVersion   = flag.String("fc-version", "", "firecracker version stamped into snapshot manifests")
		kernelVer   = flag.String("kernel-version", "", "guest kernel version stamped into snapshot manifests")
		jailerBin   = flag.String("jailer-bin", "", "jailer binary; enables chroot+uid/gid+seccomp hardening when set")
		jailRoot    = flag.String("jailer-chroot-base", "/srv/jailer", "jailer chroot base directory")
	)
	flag.Parse()

	if *kernel == "" || *guestdBin == "" {
		log.Fatal("nodeagent: --kernel and --guestd-bin are required")
	}

	var backend storage.Backend
	if *plainRoot != "" {
		backend = storage.NewPlainBackend(*plainRoot)
	} else {
		backend = storage.NewZFSBackend(*pool)
	}

	p := netns.NewPoolAt(*scriptDir, *netnsBase, *netnsPool)
	ctx := context.Background()
	if err := p.Setup(ctx); err != nil {
		log.Fatalf("nodeagent: netns pool setup: %v", err)
	}

	agent, err := nodeagent.New(nodeagent.Config{
		Storage:          backend,
		Pool:             p,
		WorkDir:          *workDir,
		KernelPath:       *kernel,
		FCBin:            *fcBin,
		UffdHandlerBin:   *uffdBin,
		GuestdBin:        *guestdBin,
		RestoreMode:      *restoreMode,
		WatchdogInterval: *watchdog,
		ChunkStoreDir:    *chunkDir,
		CapacityMiB:      *capacity,
		FCVersion:        *fcVersion,
		KernelVersion:    *kernelVer,
		JailerBin:        *jailerBin,
		JailerChrootBase: *jailRoot,
	})
	if err != nil {
		log.Fatalf("nodeagent: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(*socket), 0o755); err != nil {
		log.Fatalf("nodeagent: socket dir: %v", err)
	}
	_ = os.Remove(*socket)
	ln, err := net.Listen("unix", *socket)
	if err != nil {
		log.Fatalf("nodeagent: listen %s: %v", *socket, err)
	}
	// /metrics rides the node socket (curl --unix-socket; the transport mux
	// stays metrics-free).
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	mux.Handle("/", nodeapi.NewServer(agent))
	// Header-read bound only: guest-proxy responses stream (WebSockets), so
	// no global WriteTimeout.
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigc
		// Drain in-flight requests (a mid-flight pause write-through must
		// finish) instead of cutting them.
		shCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
		_ = p.Teardown(ctx)
	}()

	fmt.Printf("nodeagent listening socket=%s pool=%s netns=%d\n", *socket, *pool, *netnsPool)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatalf("nodeagent: serve: %v", err)
	}
}
