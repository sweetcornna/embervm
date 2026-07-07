//go:build linux

// uffd-handler serves guest memory pages to a Firecracker microVM restored
// with mem_backend.backend_type == "Uffd". One handler process per VM.
//
// Raw-memfile modes (M0, frozen behavior):
//
//	uffd-handler --socket /run/uffd.sock --memfile snap/memfile \
//	             --mode lazy|prefetch [--metrics-out metrics.json]
//
// Chunked mode (M2 restore pipeline):
//
//	uffd-handler --socket /run/uffd.sock --mode chunked \
//	             --manifest-dir snap/ --store /var/lib/embervm/chunks \
//	             [--ws snap/ws.json] [--parent-pid N]
//
// In chunked mode, layer manifests are read from --manifest-dir
// (layer-*.json), chunks come from the local --store with an optional
// S3-compatible L1 fallback configured via EMBERVM_L1_* env (write-through
// local). --ws replays a recorded working set as the eager prefetch order,
// or records one on the first resume. --parent-pid makes the handler exit
// when its supervising node agent dies (watchdog hygiene: an orphaned
// handler pins the socket and the VM it serves is already doomed).
//
// Start the handler first; issue PUT /snapshot/load once the socket exists.
// The handler exits when the VM goes away (peer socket EOF) or on
// SIGTERM/SIGINT, writing its metrics on the way out.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/embervm/embervm/pkg/chunkstore"
	"github.com/embervm/embervm/pkg/memsnap"
	"github.com/embervm/embervm/pkg/uffd"
)

func main() {
	var (
		socket        = flag.String("socket", "", "unix socket path Firecracker will connect to (required)")
		memfile       = flag.String("memfile", "", "snapshot memory file (lazy/prefetch modes)")
		mode          = flag.String("mode", "lazy", "page delivery mode: lazy | prefetch | chunked")
		prefetchBlock = flag.Uint64("prefetch-block", 4<<20, "bytes per background UFFDIO_COPY in prefetch mode")
		metricsOut    = flag.String("metrics-out", "", "write handler metrics JSON to this path on exit")
		handshakeSec  = flag.Int("handshake-timeout", 120, "seconds to wait for Firecracker to connect")
		verbose       = flag.Bool("verbose", false, "log handler activity to stderr")
		manifestDir   = flag.String("manifest-dir", "", "directory with layer-*.json manifests (chunked mode)")
		storeDir      = flag.String("store", "", "local chunk store root (chunked mode)")
		wsPath        = flag.String("ws", "", "working-set trace path: replayed if present, recorded if absent (chunked mode)")
		parentPID     = flag.Int("parent-pid", 0, "exit when this process is no longer the parent (watchdog)")
	)
	flag.Parse()
	log.SetPrefix("uffd-handler: ")
	log.SetFlags(log.Lmicroseconds)

	if *socket == "" {
		fmt.Fprintln(os.Stderr, "uffd-handler: --socket is required")
		flag.Usage()
		os.Exit(2)
	}

	cfg := uffd.Config{
		SocketPath:       *socket,
		MemfilePath:      *memfile,
		Mode:             uffd.Mode(*mode),
		PrefetchBlock:    *prefetchBlock,
		HandshakeTimeout: time.Duration(*handshakeSec) * time.Second,
		Verbose:          *verbose,
		WSPath:           *wsPath,
	}

	if uffd.Mode(*mode) == uffd.ModeChunked {
		if *manifestDir == "" || *storeDir == "" {
			fmt.Fprintln(os.Stderr, "uffd-handler: --manifest-dir and --store are required in chunked mode")
			os.Exit(2)
		}
		view, chunks, err := chunkedInputs(*manifestDir, *storeDir)
		if err != nil {
			log.Fatal(err)
		}
		cfg.View, cfg.Chunks = view, chunks
	}

	h, err := uffd.New(cfg)
	if err != nil {
		log.Fatal(err)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-sig
		log.Printf("received %s, shutting down", s)
		h.Stop()
	}()
	if *parentPID > 0 {
		go watchParent(*parentPID, h.Stop)
	}

	serveErr := h.Serve()
	if err := h.Stats().WriteFile(*metricsOut); err != nil {
		log.Printf("writing metrics: %v", err)
	}
	if serveErr != nil {
		log.Fatal(serveErr)
	}
}

// chunkedInputs loads the layer chain and builds the chunk source:
// local store, tiered over an S3 L1 when EMBERVM_L1_* is configured.
func chunkedInputs(manifestDir, storeDir string) (*memsnap.View, uffd.ChunkGetter, error) {
	paths, err := filepath.Glob(filepath.Join(manifestDir, "layer-*.json"))
	if err != nil {
		return nil, nil, err
	}
	if len(paths) == 0 {
		return nil, nil, fmt.Errorf("no layer-*.json manifests in %s", manifestDir)
	}
	layers := make([]*memsnap.Manifest, 0, len(paths))
	for _, p := range paths {
		m, err := memsnap.ReadManifest(p)
		if err != nil {
			return nil, nil, err
		}
		layers = append(layers, m)
	}
	view, err := memsnap.Resolve(layers)
	if err != nil {
		return nil, nil, err
	}

	local, err := chunkstore.NewDir(storeDir)
	if err != nil {
		return nil, nil, err
	}
	var source chunkstore.Store = local
	if l1cfg, ok, err := chunkstore.S3FromEnv(); err != nil {
		return nil, nil, err
	} else if ok {
		remote, err := chunkstore.NewS3(l1cfg)
		if err != nil {
			return nil, nil, err
		}
		source = chunkstore.Tiered{Local: local, Remote: remote}
		log.Printf("L1 fallback enabled: %s/%s", l1cfg.Endpoint, l1cfg.Bucket)
	}
	return view, chunkstore.Bytes{Ctx: context.Background(), S: source}, nil
}

// watchParent stops the handler when the supervising process disappears
// (getppid changes once we are reparented to init/subreaper).
func watchParent(pid int, stop func()) {
	for range time.Tick(2 * time.Second) {
		if os.Getppid() != pid {
			log.Printf("parent %d is gone, shutting down", pid)
			stop()
			return
		}
	}
}
