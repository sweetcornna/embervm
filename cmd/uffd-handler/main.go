//go:build linux

// uffd-handler serves guest memory pages to a Firecracker microVM restored
// with mem_backend.backend_type == "Uffd". One handler process per VM.
//
// Usage:
//
//	uffd-handler --socket /run/uffd.sock --memfile snap/memfile \
//	             --mode lazy|prefetch [--metrics-out metrics.json]
//
// Start the handler first; issue PUT /snapshot/load once the socket exists.
// The handler exits when the VM goes away (peer socket EOF) or on
// SIGTERM/SIGINT, writing its metrics on the way out.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/embervm/embervm/pkg/uffd"
)

func main() {
	var (
		socket        = flag.String("socket", "", "unix socket path Firecracker will connect to (required)")
		memfile       = flag.String("memfile", "", "snapshot memory file to serve pages from (required)")
		mode          = flag.String("mode", "lazy", "page delivery mode: lazy | prefetch")
		prefetchBlock = flag.Uint64("prefetch-block", 4<<20, "bytes per background UFFDIO_COPY in prefetch mode")
		metricsOut    = flag.String("metrics-out", "", "write handler metrics JSON to this path on exit")
		handshakeSec  = flag.Int("handshake-timeout", 120, "seconds to wait for Firecracker to connect")
		verbose       = flag.Bool("verbose", false, "log handler activity to stderr")
	)
	flag.Parse()
	log.SetPrefix("uffd-handler: ")
	log.SetFlags(log.Lmicroseconds)

	if *socket == "" || *memfile == "" {
		fmt.Fprintln(os.Stderr, "uffd-handler: --socket and --memfile are required")
		flag.Usage()
		os.Exit(2)
	}

	h, err := uffd.New(uffd.Config{
		SocketPath:       *socket,
		MemfilePath:      *memfile,
		Mode:             uffd.Mode(*mode),
		PrefetchBlock:    *prefetchBlock,
		HandshakeTimeout: time.Duration(*handshakeSec) * time.Second,
		Verbose:          *verbose,
	})
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

	serveErr := h.Serve()
	if err := h.Stats().WriteFile(*metricsOut); err != nil {
		log.Printf("writing metrics: %v", err)
	}
	if serveErr != nil {
		log.Fatal(serveErr)
	}
}
