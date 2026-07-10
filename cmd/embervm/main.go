// Command embervm is the EmberVM CLI. Its headline subcommand, `dev`, runs
// the entire stack — database migrations, the REST API server, and an
// in-process node agent — in one root process on a single machine. This is
// the "single-node mode as a first-class citizen" experience from
// docs/zh/05 §6: one cheap cloud VM is enough to try EmberVM end to end.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/embervm/embervm/pkg/chunkstore"
	"github.com/embervm/embervm/pkg/controlplane"
	"github.com/embervm/embervm/pkg/netns"
	"github.com/embervm/embervm/pkg/nodeagent"
	"github.com/embervm/embervm/pkg/storage"
)

const version = "v0.1.0-m1"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "dev":
		runDev(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("embervm %s\n", version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "embervm: unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `embervm — self-hostable sandbox cloud

usage:
  embervm dev [flags]   run the whole stack (API + scheduler + node agent) in one process
  embervm version       print the version

Run "embervm dev --help" for dev flags.
`)
}

func runDev(args []string) {
	fs := flag.NewFlagSet("dev", flag.ExitOnError)
	var (
		dbURL       = fs.String("database-url", "postgres:///embervm", "PostgreSQL connection URL")
		listen      = fs.String("listen", ":8080", "HTTP listen address")
		tokensFile  = fs.String("tokens-file", "", "JSON file mapping bearer tokens to {owner,max_sandboxes}")
		insecureDev = fs.Bool("insecure-dev-token", false, "accept the well-known 'dev-token' when no --tokens-file (INSECURE — local trials only)")
		zfsPool     = fs.String("zfs-pool", "embervm", "ZFS pool for sandbox datasets")
		plainRoot   = fs.String("plain-root", "", "use a plain-directory storage backend rooted here instead of ZFS")
		netnsPool   = fs.Int("netns-pool", 24, "number of pre-created netns slots")
		scriptDir   = fs.String("script-dir", "scripts", "directory containing setup/teardown-network.sh")
		workDir     = fs.String("work-dir", "/var/lib/embervm/work", "per-sandbox runtime state directory")
		kernel      = fs.String("kernel", "", "guest kernel (vmlinux) path")
		fcBin       = fs.String("fc-bin", "firecracker", "firecracker binary")
		uffdBin     = fs.String("uffd-handler", "uffd-handler", "uffd handler binary")
		guestdBin   = fs.String("guestd-bin", "", "guestd binary to inject into templates")
		restoreMode = fs.String("restore-mode", "prefetch", "uffd restore mode: prefetch|lazy|file")
		watchdog    = fs.Duration("watchdog-interval", 5*time.Second, "zombie-reaper scan interval (0 disables)")
	)
	_ = fs.Parse(args)

	if *kernel == "" || *guestdBin == "" {
		log.Fatal("embervm dev: --kernel and --guestd-bin are required")
	}

	ctx := context.Background()

	var backend storage.Backend
	if *plainRoot != "" {
		backend = storage.NewPlainBackend(*plainRoot)
	} else {
		backend = storage.NewZFSBackend(*zfsPool)
	}

	pool := netns.NewPool(*scriptDir, *netnsPool)
	if err := pool.Setup(ctx); err != nil {
		log.Fatalf("embervm dev: netns pool setup: %v", err)
	}

	agent, err := nodeagent.New(nodeagent.Config{
		Storage:          backend,
		Pool:             pool,
		WorkDir:          *workDir,
		KernelPath:       *kernel,
		FCBin:            *fcBin,
		UffdHandlerBin:   *uffdBin,
		GuestdBin:        *guestdBin,
		RestoreMode:      *restoreMode,
		WatchdogInterval: *watchdog,
	})
	if err != nil {
		log.Fatalf("embervm dev: %v", err)
	}

	store, err := controlplane.NewStore(ctx, *dbURL)
	if err != nil {
		log.Fatalf("embervm dev: connect database: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		log.Fatalf("embervm dev: migrate: %v", err)
	}

	tokens, usedInsecure, err := controlplane.ResolveTokens(*tokensFile, *insecureDev)
	if err != nil {
		log.Fatalf("embervm dev: %v", err)
	}
	if usedInsecure {
		log.Printf("embervm dev: WARNING accepting the well-known %q (owner dev) — do NOT expose this to untrusted networks",
			controlplane.DevTokenName)
	}

	l1, _, err := chunkstore.L1FromEnv()
	if err != nil {
		log.Fatalf("L1 store: %v", err)
	}
	cold, _, err := chunkstore.ColdFromEnv()
	if err != nil {
		log.Fatalf("cold store: %v", err)
	}
	engCfg, err := controlplane.EngineConfigFromEnv()
	if err != nil {
		log.Fatalf("lifecycle engine config: %v", err)
	}
	engine := controlplane.NewEngine(store, controlplane.SingleAgent(agent), l1, cold, engCfg)
	srv := controlplane.NewServer(store, agent, tokens, l1, cold)
	engine.CanFit = srv.CanFit // autoscale growth admission (M6)
	go engine.Run(ctx)
	httpSrv := &http.Server{
		Addr:    *listen,
		Handler: srv.Handler(),
		// Header-read bound only: the guest proxy streams (WebSockets), so
		// no global WriteTimeout.
		ReadHeaderTimeout: 10 * time.Second,
	}

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigc
		// Drain in-flight requests instead of cutting them mid-response.
		shCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shCtx)
		_ = pool.Teardown(ctx)
	}()

	fmt.Printf("embervm dev listening addr=%s storage=%s\n", *listen, storageDesc(*plainRoot, *zfsPool))
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("embervm dev: serve: %v", err)
	}
}

func storageDesc(plainRoot, zfsPool string) string {
	if plainRoot != "" {
		return "plain:" + plainRoot
	}
	return "zfs:" + zfsPool
}
