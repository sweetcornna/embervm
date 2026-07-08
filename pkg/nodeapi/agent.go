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

	"github.com/embervm/embervm/pkg/guestapi"
)

// CreateSandboxRequest describes a sandbox to create.
type CreateSandboxRequest struct {
	SandboxID   string `json:"sandbox_id"`
	TemplateID  string `json:"template_id"`
	VCPUs       int    `json:"vcpus"`
	MemoryMiB   int    `json:"memory_mib"`
	DataDiskGiB int    `json:"data_disk_gib"`
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

	Exec(ctx context.Context, sandboxID string, req *guestapi.ExecRequest) (*guestapi.ExecResponse, error)
	Health(ctx context.Context, sandboxID string) (*guestapi.HealthResponse, error)
	ReadFile(ctx context.Context, sandboxID, path string) ([]byte, error)
	WriteFile(ctx context.Context, sandboxID, path string, mode fs.FileMode, data []byte) error
}
