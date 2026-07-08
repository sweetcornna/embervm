// Package nodeagent is the concrete node-side Agent: it wires storage,
// Firecracker, the netns pool, the template builder, and the M0 uffd handler
// into the lifecycle operations the control plane drives. The exported
// surface is portable; the implementation is linux-only (agent_linux.go),
// with a stub elsewhere so `embervm dev` and cmd/nodeagent build on any host.
package nodeagent

import (
	"time"

	"github.com/embervm/embervm/pkg/netns"
	"github.com/embervm/embervm/pkg/storage"
)

// Config configures a node agent.
type Config struct {
	Storage storage.Backend // ZFS in production, plain for dev
	Pool    *netns.Pool     // pre-created netns pool

	WorkDir        string // per-sandbox runtime state (api sockets, snapshots)
	KernelPath     string // guest kernel (vmlinux)
	FCBin          string // firecracker binary
	UffdHandlerBin string // cmd/uffd-handler binary (memory server on resume)
	GuestdBin      string // guestd binary injected into templates

	RestoreMode string // "chunked" (M2 pipeline) | "prefetch" | "lazy"; default "prefetch"
	CgroupRoot  string // cgroup v2 slice parent; default /sys/fs/cgroup/embervm

	// ChunkStoreDir is the node-local chunk cache for RestoreMode=chunked
	// (default <WorkDir>/chunks). The L1 object store is configured via
	// EMBERVM_L1_* environment (S3 endpoint or shared directory).
	ChunkStoreDir string
	// FCVersion/KernelVersion stamp snapshot manifests (charter: 快照带
	// (fc_ver, kernel_ver) 标签).
	FCVersion     string
	KernelVersion string
	// CapacityMiB is the memory the scheduler may pack onto this node
	// (0 = unlimited, single-node dev).
	CapacityMiB int

	// JailerBin enables jailer hardening when set (chroot + per-VM uid/gid
	// + netns + default seccomp, docs/zh/04 §5). Snapshot paths become
	// chroot-relative. Empty = unjailed (dev / M1-M3 compatibility).
	JailerBin        string
	JailerChrootBase string // default /srv/jailer
	JailUIDBase      int    // per-VM uid = base + netns slot; default 30000

	// BootExtraArgs is appended to the guest kernel command line; defaults to
	// the docs/zh/04 §5 microVM args.
	BootExtraArgs string

	// WatchdogInterval enables the zombie reaper (watchdog_linux.go) when
	// > 0: RUNNING sandboxes whose Firecracker or uffd handler died behind
	// the agent's back are force-FAILED and reported via Healthz. 0 = off
	// (unit tests drive reapZombies directly).
	WatchdogInterval time.Duration

	// PauseBalloonSettle enables balloon-assisted pause (chunked mode)
	// when > 0: inflate the balloon to half the guest's memory, wait this
	// long for the guest to hand pages back, then snapshot — the chunk
	// pipeline's zero-page skip drops the freed pages from the diff.
	// Resume deflates. 0 = off.
	PauseBalloonSettle time.Duration
}
