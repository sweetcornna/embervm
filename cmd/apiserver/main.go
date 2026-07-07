// Command apiserver is the EmberVM control-plane API server: it persists
// state in PostgreSQL and drives a node agent (reached over a unix socket)
// through the sandbox lifecycle over a bearer-authenticated REST API.
//
// `embervm dev` runs the same control plane in-process with an in-proc agent
// instead of connecting over the socket.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"

	"github.com/embervm/embervm/pkg/controlplane"
	"github.com/embervm/embervm/pkg/nodeapi"
)

func main() {
	var (
		dbURL       = flag.String("database-url", "postgres:///embervm", "PostgreSQL connection URL")
		listen      = flag.String("listen", ":8080", "HTTP listen address")
		tokensFile  = flag.String("tokens-file", "", "JSON file mapping bearer tokens to {owner,max_sandboxes}")
		insecureDev = flag.Bool("insecure-dev-token", false, "accept the well-known 'dev-token' when no --tokens-file (INSECURE — local trials only)")
		naSocket    = flag.String("nodeagent-socket", "/run/embervm/nodeagent.sock", "node agent unix socket")
	)
	flag.Parse()

	ctx := context.Background()
	store, err := controlplane.NewStore(ctx, *dbURL)
	if err != nil {
		log.Fatalf("apiserver: connect database: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		log.Fatalf("apiserver: migrate: %v", err)
	}

	tokens, usedInsecure, err := controlplane.ResolveTokens(*tokensFile, *insecureDev)
	if err != nil {
		log.Fatalf("apiserver: %v", err)
	}
	if usedInsecure {
		log.Printf("apiserver: WARNING accepting the well-known %q (owner dev) — do NOT expose this to untrusted networks",
			controlplane.DevTokenName)
	}

	agent := nodeapi.NewClient(*naSocket)
	srv := controlplane.NewServer(store, agent, tokens)

	fmt.Printf("apiserver listening addr=%s nodeagent=%s\n", *listen, *naSocket)
	if err := http.ListenAndServe(*listen, srv.Handler()); err != nil {
		log.Fatalf("apiserver: serve: %v", err)
	}
}
