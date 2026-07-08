package controlplane

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/embervm/embervm/pkg/lifecycle"
	"github.com/embervm/embervm/pkg/nodeapi"
)

// TierAgent is what the lifecycle engine needs from a node: the M3 tier
// verbs. The concrete nodeagent grows them in phase P3; tests mock this.
type TierAgent interface {
	// ReleaseLocal frees every node-local resource of a paused sandbox
	// (dataset, workdir, netns lease) after verifying L1 holds a complete
	// restore descriptor. HOT→WARM.
	ReleaseLocal(ctx context.Context, sandboxID string) error
	// RestoreSandbox rebuilds a sandbox from the tier's store ("warm" = L1,
	// "cold" = the cold store) and resumes it.
	RestoreSandbox(ctx context.Context, sandboxID, tier string) (nodeapi.SandboxStatus, error)
	// ExtractArtifacts tars the given guest paths from the sandbox's
	// archived disk into the cold store (sandboxes/<id>/artifacts.tar.zst).
	ExtractArtifacts(ctx context.Context, sandboxID string, paths []string) error
}

// EngineConfig sets the lifecycle engine's cadence. Each TTL measures time
// spent in the CURRENT tier (updated_at), not since the original pause; a
// zero TTL disables that transition.
type EngineConfig struct {
	Tick        time.Duration // scan interval; default 30s
	TTLWarm     time.Duration // PAUSED_HOT      → PAUSED_WARM
	TTLCold     time.Duration // PAUSED_WARM     → ARCHIVED_COLD
	TTLRecycle  time.Duration // ARCHIVED_COLD   → RECYCLED
	PrewarmLead time.Duration // pull-back lead before a predicted wake; default 60s
}

func (c EngineConfig) withDefaults() EngineConfig {
	if c.Tick <= 0 {
		c.Tick = 30 * time.Second
	}
	if c.PrewarmLead <= 0 {
		c.PrewarmLead = time.Minute
	}
	return c
}

// Engine drives TTL tier transitions (docs/zh/02 §3): it scans PostgreSQL on
// a tick and moves sandboxes HOT→WARM→COLD→RECYCLED. The state change is a
// compare-and-swap taken BEFORE the tier action, so a resume racing a
// transition either wins the CAS (transition skipped) or sees the new tier
// and takes the restore path; a tier action that then fails marks the
// sandbox FAILED with the error — loudly visible, never silently retried.
type Engine struct {
	store *Store
	agent TierAgent
	cfg   EngineConfig
}

// NewEngine wires the lifecycle engine. Call Run to start it.
func NewEngine(store *Store, agent TierAgent, cfg EngineConfig) *Engine {
	return &Engine{store: store, agent: agent, cfg: cfg.withDefaults()}
}

// Run scans until ctx is canceled.
func (e *Engine) Run(ctx context.Context) {
	t := time.NewTicker(e.cfg.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.tickOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("lifecycle engine: %v", err)
			}
		}
	}
}

// tickOnce runs one scan. Split out for tests.
func (e *Engine) tickOnce(ctx context.Context) error {
	if e.cfg.TTLWarm > 0 {
		if err := e.demoteHotToWarm(ctx); err != nil {
			return err
		}
	}
	return nil
}

// demoteHotToWarm releases node-local resources for sandboxes idle past
// TTLWarm. The M2 write-through means L1 already holds everything.
func (e *Engine) demoteHotToWarm(ctx context.Context) error {
	due, err := e.store.ListTransitionDue(ctx, string(lifecycle.StatePausedHot),
		time.Now().Add(-e.cfg.TTLWarm))
	if err != nil {
		return err
	}
	for _, sb := range due {
		if err := e.transition(ctx, sb.ID, lifecycle.StatePausedHot, lifecycle.StatePausedWarm,
			func() error { return e.agent.ReleaseLocal(ctx, sb.ID) }); err != nil {
			return err
		}
	}
	return nil
}

// transition performs CAS-then-act: losing the CAS (concurrent resume/stop)
// is a clean skip; a failing action marks the sandbox FAILED.
func (e *Engine) transition(ctx context.Context, id string, from, to lifecycle.State, act func() error) error {
	if err := lifecycle.Validate(from, to); err != nil {
		return err
	}
	err := e.store.TransitionSandbox(ctx, id, string(from), string(to), "")
	if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
		return nil // someone else moved it first; their transition wins
	}
	if err != nil {
		return err
	}
	if err := act(); err != nil {
		log.Printf("lifecycle engine: %s %s->%s action failed: %v", id, from, to, err)
		_ = e.store.TransitionSandbox(ctx, id, string(to), string(lifecycle.StateFailed), err.Error())
		return nil // recorded on the sandbox; keep scanning others
	}
	log.Printf("lifecycle engine: %s %s -> %s", id, from, to)
	return nil
}
