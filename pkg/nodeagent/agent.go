// Package nodeagent is the concrete node-side Agent: it wires storage,
// Firecracker, the netns pool, the template builder, and the M0 uffd handler
// into the lifecycle operations the control plane drives. The exported
// surface is portable; the implementation is linux-only (agent_linux.go),
// with a stub elsewhere so `embervm dev` and cmd/nodeagent build on any host.
package nodeagent

import (
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

	RestoreMode string // "prefetch" | "lazy" | "file"; default "prefetch"
	CgroupRoot  string // cgroup v2 slice parent; default /sys/fs/cgroup/embervm

	// BootExtraArgs is appended to the guest kernel command line; defaults to
	// the docs/zh/04 §5 microVM args.
	BootExtraArgs string
}
