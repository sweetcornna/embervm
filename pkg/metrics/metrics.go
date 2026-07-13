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

	// ResizeSeconds observes user-facing resize latency (M6).
	ResizeSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "embervm_resize_seconds",
		Help:    "Runtime resize latency, request to converged.",
		Buckets: prometheus.ExponentialBuckets(0.01, 2, 12), // 10ms .. ~40s
	})

	// ResizeTotal counts resize outcomes (ok/no_capacity/error).
	ResizeTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "embervm_resize_total",
		Help: "Runtime resize requests, by outcome.",
	}, []string{"result"})

	// AutoscaleActions counts engine-driven automatic resizes (M6).
	AutoscaleActions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "embervm_autoscale_actions_total",
		Help: "Automatic elasticity actions, by direction (grow/shrink).",
	}, []string{"direction"})

	// Migrations counts explicit cross-node migrations (M6).
	Migrations = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "embervm_migrations_total",
		Help: "Explicit sandbox migrations, by result.",
	}, []string{"result"})

	// GuestHealthProbes counts console-driven guest health polls
	// (ok/error/cached).
	GuestHealthProbes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "embervm_guest_health_probes_total",
		Help: "Guest health probes via /v0, by result.",
	}, []string{"result"})

	// TermSessionsActive is the number of interactive terminals open now.
	TermSessionsActive = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "embervm_term_sessions_active",
		Help: "Interactive terminal sessions currently open.",
	})

	// TermSessions counts terminal session attempts (ok/denied/error).
	TermSessions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "embervm_term_sessions_total",
		Help: "Interactive terminal sessions, by result.",
	}, []string{"result"})
)

// Handler is the /metrics endpoint.
func Handler() http.Handler { return promhttp.Handler() }
