// Egress policy (M4 D8): "nat" (default) keeps the slot's MASQUERADE path;
// "none" blocks guest-originated forwarding in the root ns, leaving only the
// host-side proxy targets reachable. The full zero-trust L7 egress proxy is
// deferred past v0.1 (ADR-0005).

package nodeagent

import (
	"context"
	"fmt"
	"log"
)

// applyEgress enforces the sandbox's policy on its leased slot. Call while
// the lease is held, before the guest runs.
func (a *Agent) applyEgress(ctx context.Context, sb *sandbox) error {
	switch sb.egress {
	case "", "nat":
		return nil
	case "none":
		return sb.lease.BlockEgress(ctx)
	default:
		return fmt.Errorf("unknown egress policy %q", sb.egress)
	}
}

// clearEgress removes the slot's rule before the lease returns to the pool —
// slots are reused, so a leaked rule would cut off the next tenant. Every
// path that releases a lease must come through here first. Best-effort: the
// delete fails harmlessly when a create aborted before applyEgress ran.
func (a *Agent) clearEgress(ctx context.Context, sb *sandbox) {
	if sb.egress != "none" {
		return
	}
	if err := sb.lease.UnblockEgress(ctx); err != nil {
		log.Printf("nodeagent: clear egress %s: %v", sb.id, err)
	}
}
