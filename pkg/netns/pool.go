// Package netns manages a pre-created pool of per-sandbox network
// namespaces (ember<N>, matching scripts/setup-network.sh) and provides a
// dialer that reaches a guest at its fixed address 172.16.0.2 from inside
// its namespace. Every sandbox shares the same guest IP; isolation comes
// from the namespace + NAT, so the host must dial *inside* the namespace
// (docs/zh/02 §4).
//
// The pool is created once and slots are leased/returned without tearing
// down namespaces — namespace creation rate collapses past ~500 (docs/zh/04
// §5), and reuse keeps the hot path free of that cost.
package netns

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// GuestIP is the fixed in-namespace address of every EmberVM guest.
const GuestIP = "172.16.0.2"

// runner runs a command; injectable so pool bookkeeping is unit-tested
// without creating real namespaces (which need linux + root).
type runner func(ctx context.Context, name string, args ...string) (string, error)

func execRun(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// Pool hands out namespace slots ember<base>..ember<base+size-1>.
type Pool struct {
	scriptDir string
	size      int
	base      int
	run       runner

	mu   sync.Mutex
	free []int
	used map[int]bool
}

// NewPool prepares a pool of the given size whose setup/teardown scripts
// live in scriptDir.
func NewPool(scriptDir string, size int) *Pool {
	return NewPoolAt(scriptDir, 0, size)
}

// NewPoolAt is NewPool with a starting slot id — multiple node agents on
// one host (the CI 3-node cluster) partition the ember<N> namespace range
// instead of colliding on it.
func NewPoolAt(scriptDir string, base, size int) *Pool {
	return &Pool{scriptDir: scriptDir, size: size, base: base, run: execRun, used: map[int]bool{}}
}

// Setup creates every namespace in the pool (idempotent per id via the
// script's own teardown-then-create). Call once at daemon start.
func (p *Pool) Setup(ctx context.Context) error {
	p.mu.Lock()
	p.free = p.free[:0]
	p.mu.Unlock()
	script := filepath.Join(p.scriptDir, "setup-network.sh")
	for id := p.base; id < p.base+p.size; id++ {
		if _, err := p.run(ctx, script, "--id", strconv.Itoa(id)); err != nil {
			return fmt.Errorf("setup netns ember%d: %w", id, err)
		}
		p.mu.Lock()
		p.free = append(p.free, id)
		p.mu.Unlock()
	}
	return nil
}

// Teardown removes every namespace. Call at daemon shutdown.
func (p *Pool) Teardown(ctx context.Context) error {
	script := filepath.Join(p.scriptDir, "teardown-network.sh")
	var firstErr error
	for id := p.base; id < p.base+p.size; id++ {
		if _, err := p.run(ctx, script, "--id", strconv.Itoa(id)); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Acquire leases a free namespace slot.
func (p *Pool) Acquire() (Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.free) == 0 {
		return Lease{}, fmt.Errorf("netns pool exhausted (size %d)", p.size)
	}
	id := p.free[len(p.free)-1]
	p.free = p.free[:len(p.free)-1]
	p.used[id] = true
	return Lease{
		ID:        id,
		Netns:     fmt.Sprintf("ember%d", id),
		NetnsPath: fmt.Sprintf("/var/run/netns/ember%d", id),
		GuestIP:   GuestIP,
		pool:      p,
	}, nil
}

// release returns a slot to the pool; idempotent for a given id.
func (p *Pool) release(id int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.used[id] {
		return
	}
	delete(p.used, id)
	p.free = append(p.free, id)
}

// Lease is a leased namespace slot. The guest reachable at GuestIP is dialed
// through the namespace via DialContext.
type Lease struct {
	ID        int
	Netns     string
	NetnsPath string
	GuestIP   string
	pool      *Pool
}

// Release returns the slot to the pool without tearing the namespace down.
func (l Lease) Release() {
	if l.pool != nil {
		l.pool.release(l.ID)
	}
}

// VethNet is the slot's root-ns veth subnet (setup-network.sh: 10.200.<ID>.0/30).
func (l Lease) VethNet() string { return fmt.Sprintf("10.200.%d.0/30", l.ID) }

// BlockEgress cuts the guest off from the world: a root-ns FORWARD drop on
// the slot's veth subnet, inserted ahead of the pool's ACCEPT rules.
// Host→guest dialing is unaffected — DialContext enters the namespace and
// never crosses root-ns FORWARD. The rule carries the slot's embervm-<ID>
// comment so teardown-network.sh sweeps it with the rest.
func (l Lease) BlockEgress(ctx context.Context) error {
	_, err := l.pool.run(ctx, "iptables", egressRule("-I", "FORWARD", "1", l)...)
	return err
}

// UnblockEgress removes the BlockEgress rule. Exact-spec delete: the slot's
// NAT/FORWARD ACCEPT rules share the comment but not the spec.
func (l Lease) UnblockEgress(ctx context.Context) error {
	_, err := l.pool.run(ctx, "iptables", egressRule("-D", "FORWARD", "", l)...)
	return err
}

func egressRule(op, chain, pos string, l Lease) []string {
	args := []string{op, chain}
	if pos != "" {
		args = append(args, pos)
	}
	return append(args, "-s", l.VethNet(), "-j", "DROP",
		"-m", "comment", "--comment", fmt.Sprintf("embervm-%d", l.ID))
}
