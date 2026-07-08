// Package metrics is EmberVM's Prometheus instrumentation (M4 observability,
// docs/zh/03 §3). One process-wide registry, exposed at /metrics on both the
// control plane and the node daemon. Distributed tracing (OTel) is deferred
// until there is a multi-hop call worth tracing (ADR-0005).
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// RestoreSeconds records restore latency by tier (hot/warm/cold).
	RestoreSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "embervm_restore_seconds",
		Help:    "Sandbox restore latency to interactive, by tier.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 3, 5, 10},
	}, []string{"tier"})

	// CreateSeconds records sandbox creation latency by path (cold/fast).
	CreateSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "embervm_create_seconds",
		Help:    "Sandbox creation latency, by path (cold boot vs golden fast-create).",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30},
	}, []string{"path"})

	// Transitions counts lifecycle edges taken (from->to).
	Transitions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "embervm_lifecycle_transitions_total",
		Help: "Lifecycle state transitions, by edge.",
	}, []string{"from", "to"})

	// ChunkOps counts chunk-store operations
	// (put/dedup_hit/remote_get/gc_sweep).
	ChunkOps = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "embervm_chunk_ops_total",
		Help: "Chunk-store operations, by kind.",
	}, []string{"op"})

	// ProxyRequests counts gateway proxy requests by result.
	ProxyRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "embervm_proxy_requests_total",
		Help: "Gateway guest-proxy requests, by result.",
	}, []string{"result"})

	// WatchdogReaps counts zombie sandboxes reaped by node watchdogs.
	WatchdogReaps = promauto.NewCounter(prometheus.CounterOpts{
		Name: "embervm_watchdog_reaps_total",
		Help: "Sandboxes force-failed by the node zombie reaper.",
	})

	// NodesUp is the count of healthy nodes the scheduler sees.
	NodesUp = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "embervm_nodes_up",
		Help: "Nodes currently in the 'up' state.",
	})

	// EngineTickErrors counts failed lifecycle-engine scans.
	EngineTickErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "embervm_engine_tick_errors_total",
		Help: "Lifecycle engine ticks that returned an error.",
	})
)

// Handler is the /metrics endpoint.
func Handler() http.Handler { return promhttp.Handler() }
