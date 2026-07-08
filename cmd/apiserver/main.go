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
	"os"
	"strings"

	"github.com/embervm/embervm/pkg/chunkstore"
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
		nodes       = flag.String("nodes", os.Getenv("EMBERVM_NODES"),
			"static cluster membership 'id=socket,id=socket,...' (M4 multi-node; overrides --nodeagent-socket)")
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

	var srv *controlplane.Server
	var resolver controlplane.AgentResolver
	if *nodes != "" {
		// M4 cluster shape (ADR-0005 D1): static membership, agents over
		// unix sockets, polled liveness + eviction, sticky/bin-pack
		// placement.
		agents := map[string]nodeapi.Agent{}
		addrs := map[string]string{}
		for _, ent := range strings.Split(*nodes, ",") {
			id, sock, ok := strings.Cut(strings.TrimSpace(ent), "=")
			if !ok || id == "" || sock == "" {
				log.Fatalf("apiserver: bad --nodes entry %q (want id=socket)", ent)
			}
			agents[id] = nodeapi.NewClient(sock)
			addrs[id] = sock
		}
		registry := controlplane.NewRegistry(agents)
		sched := controlplane.NewScheduler(store, registry, controlplane.SchedulerConfig{})
		if err := sched.RegisterNodes(ctx, addrs, map[string]int{}); err != nil {
			log.Fatalf("apiserver: register nodes: %v", err)
		}
		go sched.Run(ctx)
		srv = controlplane.NewClusterServer(store, registry, sched, tokens, l1, cold)
		resolver = srv.AgentResolver()
		fmt.Printf("apiserver listening addr=%s nodes=%s\n", *listen, *nodes)
	} else {
		agent := nodeapi.NewClient(*naSocket)
		srv = controlplane.NewServer(store, agent, tokens, l1, cold)
		resolver = controlplane.SingleAgent(agent)
		fmt.Printf("apiserver listening addr=%s nodeagent=%s\n", *listen, *naSocket)
	}
	engine := controlplane.NewEngine(store, resolver, l1, cold, engCfg)
	go engine.Run(ctx)
	if err := http.ListenAndServe(*listen, srv.Handler()); err != nil {
		log.Fatalf("apiserver: serve: %v", err)
	}
}
