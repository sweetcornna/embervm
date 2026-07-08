// Package nodeapi defines the contract between the control plane (API
// server) and a node's agent, plus an HTTP-over-unix-socket transport for
// it. `embervm dev` wires an in-process Agent directly; a standalone node
// runs the concrete Agent behind Server and the API server talks to it via
// Client. Both satisfy the same Agent interface (master-spec D2), so the
// control plane is oblivious to which wiring is in play.
package nodeapi

import (
	"context"
	"io/fs"
	"net"
	"net/http"

	"github.com/embervm/embervm/pkg/guestapi"
)

// CreateSandboxRequest describes a sandbox to create.
type CreateSandboxRequest struct {
	SandboxID   string `json:"sandbox_id"`
	TemplateID  string `json:"template_id"`
	VCPUs       int    `json:"vcpus"`
	MemoryMiB   int    `json:"memory_mib"`
	DataDiskGiB int    `json:"data_disk_gib"`
	// Egress controls the sandbox's outbound network: "nat" (default,
	// MASQUERADE) or "none" (no internet — the guest reaches only the
	// host-side proxy targets). The full zero-trust L7 egress proxy is a
	// deferred product subsystem (ADR-0005).
	Egress string `json:"egress,omitempty"`
}

// NodeHealth is a node's capacity heartbeat (M4 scheduler polling).
type NodeHealth struct {
	CapacityMiB int `json:"capacity_mib"`
	UsedMiB     int `json:"used_mib"`
	Sandboxes   int `json:"sandboxes"`
	// CPUCores is the node's physical core count — the base the scheduler
	// multiplies by its CPU overcommit ratio (M4 超售).
	CPUCores int `json:"cpu_cores,omitempty"`
	// FailedSandboxes are ids the node's watchdog reaped since they were
	// last reported; the scheduler writes them through to PostgreSQL.
	FailedSandboxes []string `json:"failed_sandboxes,omitempty"`
}

// SandboxStatus is the node's view of a sandbox.
type SandboxStatus struct {
	SandboxID string `json:"sandbox_id"`
	State     string `json:"state"`      // pkg/lifecycle state name
	GuestAddr string `json:"guest_addr"` // e.g. "172.16.0.2:7777", reachable via the node
	Netns     string `json:"netns"`
}

// GuestDialer is the optional data-plane contract behind the M4 gateway:
// dialing a guest port requires entering the sandbox netns, so only the
// concrete node agent implements it. The nodeapi transport bridges it as a
// reverse proxy hop; nodeapi.Client satisfies GuestProxier instead.
type GuestDialer interface {
	DialGuest(ctx context.Context, sandboxID string, port int) (net.Conn, error)
}

// GuestProxier serves HTTP(S)/WebSocket traffic destined for a guest port.
type GuestProxier interface {
	GuestProxy(sandboxID string, port int) http.Handler
}

// Agent is everything the control plane can ask a node to do. Guest
// operations (Exec/ReadFile/WriteFile/Health) are methods rather than a
// returned client so the interface serializes cleanly over the split-mode
// transport; the concrete agent dials guestd through the sandbox netns.
type Agent interface {
	BuildTemplate(ctx context.Context, templateID, image string) error

	// Healthz is the scheduler's liveness + capacity poll.
	Healthz(ctx context.Context) (NodeHealth, error)

	CreateSandbox(ctx context.Context, req CreateSandboxRequest) (SandboxStatus, error)
	StopSandbox(ctx context.Context, sandboxID string) error
	PauseSandbox(ctx context.Context, sandboxID string) error
	ResumeSandbox(ctx context.Context, sandboxID string) (SandboxStatus, error)
	SnapshotSandbox(ctx context.Context, sandboxID, tag string) (string, error)
	Status(ctx context.Context, sandboxID string) (SandboxStatus, error)

	// M3 tier verbs (docs/zh/02 §3). ReleaseLocal frees every node-local
	// resource of a paused sandbox after verifying L1 holds a complete
	// restore descriptor (HOT→WARM). RestoreSandbox rebuilds a sandbox
	// from the tier's store ("warm" = L1, "cold" = the cold store) and
	// resumes it. ExtractArtifacts tars the given guest paths from the
	// archived disk into the cold store (RECYCLED keeps only those).
	ReleaseLocal(ctx context.Context, sandboxID string) error
	RestoreSandbox(ctx context.Context, sandboxID, tier string) (SandboxStatus, error)
	ExtractArtifacts(ctx context.Context, sandboxID string, paths []string) error
	// Prewarm pulls the sandbox's working-set chunks from the tier's store
	// into the node-local cache ahead of a predicted wake.
	Prewarm(ctx context.Context, sandboxID, tier string) error
	// SetBalloon retargets a running sandbox's balloon (memory reclaim).
	SetBalloon(ctx context.Context, sandboxID string, targetMiB int) error
	// Fork creates a new sandbox from a parent's checkpoint layer ("p<N>")
	// without touching the parent; Rollback switches a sandbox back to an
	// earlier checkpoint layer in place, discarding everything after it
	// (M5, ADR-0006). Both require chunked mode; fork additionally needs
	// the jailer (chroot-relative snapfile paths) and same-node placement.
	Fork(ctx context.Context, parentID, layer, newID string) (SandboxStatus, error)
	Rollback(ctx context.Context, sandboxID, layer string) (SandboxStatus, error)

	Exec(ctx context.Context, sandboxID string, req *guestapi.ExecRequest) (*guestapi.ExecResponse, error)
	Health(ctx context.Context, sandboxID string) (*guestapi.HealthResponse, error)
	ReadFile(ctx context.Context, sandboxID, path string) ([]byte, error)
	WriteFile(ctx context.Context, sandboxID, path string, mode fs.FileMode, data []byte) error
}
