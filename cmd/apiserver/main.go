// Command apiserver is the EmberVM control-plane API server: it persists
// state in PostgreSQL and drives a node agent (reached over a unix socket)
// through the sandbox lifecycle over a bearer-authenticated REST API.
//
// `embervm dev` runs the same control plane in-process with an in-proc agent
// instead of connecting over the socket.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/embervm/embervm/pkg/controlplane"
	"github.com/embervm/embervm/pkg/nodeapi"
)

func main() {
	var (
		dbURL      = flag.String("database-url", "postgres:///embervm", "PostgreSQL connection URL")
		listen     = flag.String("listen", ":8080", "HTTP listen address")
		tokensFile = flag.String("tokens-file", "", "JSON file mapping bearer tokens to {owner,max_sandboxes}")
		naSocket   = flag.String("nodeagent-socket", "/run/embervm/nodeagent.sock", "node agent unix socket")
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

	tokens, err := loadTokens(*tokensFile)
	if err != nil {
		log.Fatalf("apiserver: tokens: %v", err)
	}

	agent := nodeapi.NewClient(*naSocket)
	srv := controlplane.NewServer(store, agent, tokens)

	fmt.Printf("apiserver listening addr=%s nodeagent=%s\n", *listen, *naSocket)
	if err := http.ListenAndServe(*listen, srv.Handler()); err != nil {
		log.Fatalf("apiserver: serve: %v", err)
	}
}

// loadTokens reads the token config file, or returns a single dev token when
// no file is given (dev convenience; log a warning).
func loadTokens(path string) (*controlplane.TokenStore, error) {
	if path == "" {
		log.Println("apiserver: WARNING no --tokens-file; using dev token 'dev-token' (owner dev, max 100)")
		return controlplane.NewTokenStore(map[string]controlplane.TokenInfo{
			"dev-token": {Owner: "dev", MaxSandboxes: 100},
		}), nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]controlplane.TokenInfo
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse tokens file: %w", err)
	}
	return controlplane.NewTokenStore(m), nil
}
