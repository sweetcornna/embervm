package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/embervm/embervm/pkg/metrics"
	"github.com/embervm/embervm/pkg/nodeapi"
)

// ErrNoCapacity means no up node can fit the sandbox.
var ErrNoCapacity = errors.New("no node with free capacity")

// Registry maps node ids to their agents (static membership in M4 — a
// config-listed set of unix sockets; dynamic join is future work). The
// sandbox row's node_id IS the routing table (no Redis, ADR-0005). The
// mutex is idle today (the map is written only in NewRegistry) and exists
// for the dynamic-join future; do not rely on it elsewhere.
type Registry struct {
	mu     sync.RWMutex
	agents map[string]nodeapi.Agent
}

// NewRegistry builds a registry from a static node map.
func NewRegistry(agents map[string]nodeapi.Agent) *Registry {
	cp := make(map[string]nodeapi.Agent, len(agents))
	for id, a := range agents {
		cp[id] = a
	}
	return &Registry{agents: cp}
}

// Agent resolves a node id.
func (r *Registry) Agent(nodeID string) (nodeapi.Agent, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.agents[nodeID]
	if !ok {
		return nil, fmt.Errorf("unknown node %q", nodeID)
	}
	return a, nil
}

// IDs lists registered node ids.
func (r *Registry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.agents))
	for id := range r.agents {
		out = append(out, id)
	}
	return out
}

// SchedulerConfig tunes the liveness poll.
type SchedulerConfig struct {
	PollInterval  time.Duration // default 5s
	MissThreshold int           // consecutive failed polls before eviction; default 3
	// MemOvercommit scales each node's memory budget for placement
	// (docs/zh/03 M4 超售). Overselling is survivable because balloon
	// reclaim (SetBalloon) and balloon-assisted pause hand free guest
	// pages back to the host. Default 1.0 (no oversell).
	MemOvercommit float64
	// CPUOvercommit budgets Σ vcpus at cores × ratio — vCPUs are time-
	// shared, so 3x is the documented starting point. Nodes that never
	// reported cores (cpu_cores=0) are unconstrained. Default 3.0.
	CPUOvercommit float64
}

func (c SchedulerConfig) withDefaults() SchedulerConfig {
	if c.PollInterval <= 0 {
		c.PollInterval = 5 * time.Second
	}
	if c.MissThreshold <= 0 {
		c.MissThreshold = 3
	}
	if c.MemOvercommit <= 0 {
		c.MemOvercommit = 1.0
	}
	if c.CPUOvercommit <= 0 {
		c.CPUOvercommit = 3.0
	}
	return c
}

// Scheduler owns node liveness (polled heartbeats — the control plane
// dials the nodes it knows; nodes never gossip, docs/zh/04 §6) and
// placement (sticky to the previous node, else bin-packing by free memory,
// docs/zh/02 §2.1).
type Scheduler struct {
	store    *Store
	registry *Registry
	cfg      SchedulerConfig

	mu     sync.Mutex
	misses map[string]int
}

// NewScheduler wires the scheduler; Register must have been called for every
// static node before Run.
func NewScheduler(store *Store, registry *Registry, cfg SchedulerConfig) *Scheduler {
	return &Scheduler{store: store, registry: registry, cfg: cfg.withDefaults(), misses: map[string]int{}}
}

// Overcommit exposes the placement budget multipliers so the nodes API can
// report the budgets the scheduler actually enforces (M7 oversell view).
func (s *Scheduler) Overcommit() (mem, cpu float64) {
	return s.cfg.MemOvercommit, s.cfg.CPUOvercommit
}

// RegisterNodes writes the static membership into the nodes table: members
// are upserted and revived (an upsert keeps an existing row's state, so a
// previously-retired node must be touched back up), and rows absent from
// the registry are retired — the config is the truth, and a stale row from
// an earlier topology (single-node 'local' vs a cluster, a removed worker)
// must not win placement.
func (s *Scheduler) RegisterNodes(ctx context.Context, addrs map[string]string, capacities map[string]int) error {
	ids := s.registry.IDs()
	for _, id := range ids {
		if err := s.store.UpsertNode(ctx, Node{
			ID: id, Addr: addrs[id], CapacityMiB: capacities[id],
		}); err != nil {
			return err
		}
		if err := s.store.TouchNode(ctx, id); err != nil {
			return err
		}
	}
	return s.store.RetireAbsentNodes(ctx, ids)
}

// Run polls until ctx is canceled.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.pollOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("scheduler: %v", err)
			}
		}
	}
}

// pollOnce health-checks every registered node concurrently, updating
// liveness and evicting nodes past the miss threshold. Concurrent + per-node
// error isolation on purpose: sequential polling of unreachable nodes (3s
// timeout each) could exceed the poll interval, and one node's store blip
// must not skip the health verdict of every node after it. Split out for
// tests.
func (s *Scheduler) pollOnce(ctx context.Context) error {
	var up atomic.Int64
	defer func() { metrics.NodesUp.Set(float64(up.Load())) }()
	var wg sync.WaitGroup
	for _, id := range s.registry.IDs() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			agent, err := s.registry.Agent(id)
			if err != nil {
				log.Printf("scheduler: resolve node %s: %v", id, err)
				return
			}
			hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			h, err := agent.Healthz(hctx)
			cancel()
			if err != nil {
				s.recordMiss(ctx, id, err)
				return
			}
			up.Add(1)
			s.mu.Lock()
			s.misses[id] = 0
			s.mu.Unlock()
			if err := s.store.TouchNode(ctx, id); err != nil {
				log.Printf("scheduler: touch node %s: %v", id, err)
			}
			for _, f := range h.FailedSandboxes {
				sbID, cause, _ := strings.Cut(f, ": ")
				if err := s.store.FailSandbox(ctx, sbID, "watchdog: "+cause); err != nil {
					log.Printf("scheduler: record watchdog failure %s: %v", sbID, err)
				} else {
					log.Printf("scheduler: node %s watchdog reaped %s (%s)", id, sbID, cause)
				}
			}
			if h.CapacityMiB > 0 || h.CPUCores > 0 {
				if err := s.store.UpsertNode(ctx, Node{ID: id, CapacityMiB: h.CapacityMiB, CPUCores: h.CPUCores}); err != nil {
					log.Printf("scheduler: upsert node %s: %v", id, err)
					return
				}
				_ = s.store.TouchNode(ctx, id) // upsert resets state; re-stamp
			}
		}()
	}
	wg.Wait()
	return nil
}

func (s *Scheduler) recordMiss(ctx context.Context, id string, cause error) {
	s.mu.Lock()
	s.misses[id]++
	n := s.misses[id]
	s.mu.Unlock()
	log.Printf("scheduler: node %s health poll failed (%d/%d): %v", id, n, s.cfg.MissThreshold, cause)
	if n != s.cfg.MissThreshold {
		// Below: not yet. Above: already evicted — re-running the eviction
		// every poll would spam SetNodeState + 0-row FailRunningOnNode and
		// log a misleading "evicted; 0 sandboxes" line each tick.
		return
	}
	// Eviction: the node is gone. Its active sandboxes become FAILED —
	// their last write-through snapshot restores on demand elsewhere;
	// paused/archived ones already live in L1/L2 and need nothing.
	// On a store error, rewind the counter so the next miss retries the
	// eviction instead of silently never marking the node down.
	retry := func() {
		s.mu.Lock()
		s.misses[id] = s.cfg.MissThreshold - 1
		s.mu.Unlock()
	}
	if err := s.store.SetNodeState(ctx, id, "down"); err != nil {
		log.Printf("scheduler: mark node %s down: %v", id, err)
		retry()
		return
	}
	failed, err := s.store.FailRunningOnNode(ctx, id, "node "+id+" evicted (missed heartbeats)")
	if err != nil {
		log.Printf("scheduler: fail sandboxes on %s: %v", id, err)
		retry()
		return
	}
	log.Printf("scheduler: node %s evicted; %d active sandboxes marked FAILED (restorable from L1)", id, failed)
}

// CanFit reports whether nodeID can absorb memDeltaMiB / vcpuDelta MORE
// resources under the oversold budgets (M6 resize growth check). Non-
// positive deltas always fit — shrink frees budget. The same PG usage truth
// as Place; a resize that passes here can still lose a race with a
// concurrent create, which the oversell slack absorbs (budgets are already
// soft by design).
func (s *Scheduler) CanFit(ctx context.Context, nodeID string, memDeltaMiB, vcpuDelta int) error {
	if memDeltaMiB <= 0 && vcpuDelta <= 0 {
		return nil
	}
	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		return err
	}
	usage, err := s.store.NodeUsage(ctx)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if n.ID != nodeID {
			continue
		}
		if n.State != "up" {
			return fmt.Errorf("node %s is %s", nodeID, n.State)
		}
		if n.CapacityMiB > 0 && memDeltaMiB > 0 {
			free := int(float64(n.CapacityMiB)*s.cfg.MemOvercommit) - usage[n.ID].MemMiB
			if free < memDeltaMiB {
				return fmt.Errorf("%w (node %s: %d MiB free, need %d more)", ErrNoCapacity, nodeID, free, memDeltaMiB)
			}
		}
		if n.CPUCores > 0 && vcpuDelta > 0 {
			free := int(float64(n.CPUCores)*s.cfg.CPUOvercommit) - usage[n.ID].VCPUs
			if free < vcpuDelta {
				return fmt.Errorf("%w (node %s: %d vcpus free, need %d more)", ErrNoCapacity, nodeID, free, vcpuDelta)
			}
		}
		return nil
	}
	return fmt.Errorf("unknown node %q", nodeID)
}

// Place picks a node for a sandbox needing memoryMiB and vcpus: the
// previous node when it is up with room (L0 cache stickiness), else the up
// node with the most free memory. Budgets are oversold per SchedulerConfig
// (memory × MemOvercommit, cores × CPUOvercommit). PostgreSQL is the source
// of truth for usage.
func (s *Scheduler) Place(ctx context.Context, previousNode string, memoryMiB, vcpus int) (string, error) {
	return s.place(ctx, previousNode, "", memoryMiB, vcpus)
}

// PlaceExcluding bin-packs like Place but never returns exclude — the M6
// migrate verb's "anywhere but here".
func (s *Scheduler) PlaceExcluding(ctx context.Context, exclude string, memoryMiB, vcpus int) (string, error) {
	return s.place(ctx, "", exclude, memoryMiB, vcpus)
}

func (s *Scheduler) place(ctx context.Context, previousNode, exclude string, memoryMiB, vcpus int) (string, error) {
	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		return "", err
	}
	usage, err := s.store.NodeUsage(ctx)
	if err != nil {
		return "", err
	}
	// fits returns the node's free memory budget and whether both the
	// memory and vCPU constraints admit the sandbox.
	fits := func(n Node) (int, bool) {
		freeMem := 1 << 30 // unlimited (dev)
		if n.CapacityMiB > 0 {
			freeMem = int(float64(n.CapacityMiB)*s.cfg.MemOvercommit) - usage[n.ID].MemMiB
		}
		if freeMem < memoryMiB {
			return 0, false
		}
		if n.CPUCores > 0 {
			freeCPU := int(float64(n.CPUCores)*s.cfg.CPUOvercommit) - usage[n.ID].VCPUs
			if freeCPU < vcpus {
				return 0, false
			}
		}
		return freeMem, true
	}
	var best string
	bestFree := -1
	for _, n := range nodes {
		if n.State != "up" || n.ID == exclude {
			continue
		}
		f, ok := fits(n)
		if !ok {
			continue
		}
		if n.ID == previousNode {
			return n.ID, nil // sticky wins outright
		}
		if f > bestFree {
			best, bestFree = n.ID, f
		}
	}
	if best == "" {
		return "", fmt.Errorf("%w (need %d MiB / %d vcpus)", ErrNoCapacity, memoryMiB, vcpus)
	}
	return best, nil
}
