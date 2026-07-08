package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/embervm/embervm/pkg/chunkstore"
	"github.com/embervm/embervm/pkg/lifecycle"
	"github.com/embervm/embervm/pkg/memsnap"
	"github.com/embervm/embervm/pkg/nodeagent"
	"github.com/embervm/embervm/pkg/nodeapi"
	"github.com/embervm/embervm/pkg/prewarm"
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
	// Prewarm pulls the sandbox's working-set chunks to the node-local
	// cache ahead of a predicted wake.
	Prewarm(ctx context.Context, sandboxID, tier string) error
}

// AgentResolver routes a tier verb to the agent of the node a sandbox
// lives on (M4 multi-node; SingleAgent for one-node deployments).
type AgentResolver func(nodeID string) (TierAgent, error)

// SingleAgent resolves every node id to one agent.
func SingleAgent(a TierAgent) AgentResolver {
	return func(string) (TierAgent, error) { return a, nil }
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
	GCGrace     time.Duration // chunk-GC grace window; default 1h
}

// EngineConfigFromEnv reads EMBERVM_TTL_WARM / EMBERVM_TTL_COLD /
// EMBERVM_TTL_RECYCLE / EMBERVM_ENGINE_TICK / EMBERVM_PREWARM_LEAD /
// EMBERVM_GC_GRACE as Go durations (e.g. "45m", "12h"). Unset TTLs stay
// zero = that transition disabled, so a plain deployment archives nothing
// until the operator opts in.
func EngineConfigFromEnv() (EngineConfig, error) {
	var cfg EngineConfig
	for _, f := range []struct {
		env string
		dst *time.Duration
	}{
		{"EMBERVM_ENGINE_TICK", &cfg.Tick},
		{"EMBERVM_TTL_WARM", &cfg.TTLWarm},
		{"EMBERVM_TTL_COLD", &cfg.TTLCold},
		{"EMBERVM_TTL_RECYCLE", &cfg.TTLRecycle},
		{"EMBERVM_PREWARM_LEAD", &cfg.PrewarmLead},
		{"EMBERVM_GC_GRACE", &cfg.GCGrace},
	} {
		v := os.Getenv(f.env)
		if v == "" {
			continue
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("bad %s %q: %w", f.env, v, err)
		}
		*f.dst = d
	}
	return cfg, nil
}

func (c EngineConfig) withDefaults() EngineConfig {
	if c.Tick <= 0 {
		c.Tick = 30 * time.Second
	}
	if c.PrewarmLead <= 0 {
		c.PrewarmLead = time.Minute
	}
	if c.GCGrace <= 0 {
		c.GCGrace = time.Hour
	}
	return c
}

// Engine drives TTL tier transitions (docs/zh/02 §3): it scans PostgreSQL on
// a tick and moves sandboxes HOT→WARM→COLD→RECYCLED. Two race disciplines,
// chosen by what the action does to the CURRENT tier's data:
//
//   - Destructive-to-source (HOT→WARM releases node state): CAS FIRST, so a
//     racing resume either wins the CAS or sees WARM and restores from L1.
//     A failing action then marks the sandbox FAILED — loud, not retried.
//   - Copy-then-prune (WARM→COLD, COLD→RECYCLED): PREPARE first (idempotent,
//     dedup-skipped copies into the destination store), CAS second, PRUNE
//     last. Readers of the new tier only ever see a complete copy; a resume
//     racing the prepare still finds the old tier intact; prepare failures
//     leave the sandbox untouched and retry next tick.
type Engine struct {
	store    *Store
	agentFor AgentResolver
	l1       chunkstore.ListingBackend // warm object store (nil disables COLD/RECYCLE)
	cold     chunkstore.ListingBackend // cold object store (nil disables COLD/RECYCLE)
	cfg      EngineConfig
}

// NewEngine wires the lifecycle engine. l1/cold may be nil: WARM demotion
// still works (the node agent guards its own L1 requirement); COLD archive
// and RECYCLE require both. Call Run to start it.
func NewEngine(store *Store, agentFor AgentResolver, l1, cold chunkstore.ListingBackend, cfg EngineConfig) *Engine {
	return &Engine{store: store, agentFor: agentFor, l1: l1, cold: cold, cfg: cfg.withDefaults()}
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
	if e.cfg.TTLCold > 0 && e.l1 != nil && e.cold != nil {
		if err := e.demoteWarmToCold(ctx); err != nil {
			return err
		}
	}
	if e.cfg.TTLRecycle > 0 && e.cold != nil {
		if err := e.recycleCold(ctx); err != nil {
			return err
		}
	}
	if err := e.prewarmScan(ctx); err != nil {
		return err
	}
	return nil
}

// demoteWarmToCold archives sandboxes idle in WARM past TTLCold: the memory
// layer chain is compacted into a synthetic full (metadata-only), referenced
// chunks and objects move to the cold store, and the L1 copy is deleted.
func (e *Engine) demoteWarmToCold(ctx context.Context) error {
	due, err := e.store.ListTransitionDue(ctx, string(lifecycle.StatePausedWarm),
		time.Now().Add(-e.cfg.TTLCold))
	if err != nil {
		return err
	}
	for _, sb := range due {
		if err := e.archivePrepare(ctx, sb.ID); err != nil {
			// Idempotent (dedup-skipped) — retried on the next tick.
			log.Printf("lifecycle engine: archive prepare %s: %v", sb.ID, err)
			continue
		}
		err := e.store.TransitionSandbox(ctx, sb.ID,
			string(lifecycle.StatePausedWarm), string(lifecycle.StateArchivedCold), "")
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			continue // a resume won; the cold copy is harmless and reusable
		}
		if err != nil {
			return err
		}
		log.Printf("lifecycle engine: %s PAUSED_WARM -> ARCHIVED_COLD", sb.ID)
		e.archivePrune(ctx, sb.ID)
	}
	return nil
}

// recycleCold turns sandboxes idle in COLD past TTLRecycle into their
// artifacts: extraction (when artifact_paths is set), deletion of every
// other snapshot object, then a chunk GC on both stores.
func (e *Engine) recycleCold(ctx context.Context) error {
	due, err := e.store.ListTransitionDue(ctx, string(lifecycle.StateArchivedCold),
		time.Now().Add(-e.cfg.TTLRecycle))
	if err != nil {
		return err
	}
	for _, sb := range due {
		if len(sb.ArtifactPaths) > 0 {
			agent, err := e.agentFor(sb.NodeID)
			if err != nil {
				log.Printf("lifecycle engine: recycle %s: %v", sb.ID, err)
				continue
			}
			if err := agent.ExtractArtifacts(ctx, sb.ID, sb.ArtifactPaths); err != nil {
				// Reads the cold copy, writes only the tarball — retryable.
				log.Printf("lifecycle engine: extract artifacts %s: %v", sb.ID, err)
				continue
			}
		}
		err := e.store.TransitionSandbox(ctx, sb.ID,
			string(lifecycle.StateArchivedCold), string(lifecycle.StateRecycled), "")
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			continue // a resume won; the tarball sits harmlessly beside the snapshot
		}
		if err != nil {
			return err
		}
		log.Printf("lifecycle engine: %s ARCHIVED_COLD -> RECYCLED", sb.ID)
		e.recyclePrune(ctx, sb.ID)
	}
	return nil
}

// archivePrepare copies everything the COLD tier needs into the cold store
// (control-plane only, no node involved). Idempotent: chunk copies dedup,
// object puts overwrite with identical content — safe to retry and safe to
// abandon if the CAS loses.
func (e *Engine) archivePrepare(ctx context.Context, id string) error {
	var desc nodeagent.SnapshotDescriptor
	if err := readJSONObject(ctx, e.l1, nodeagent.KeySnapshotJSON(id), &desc); err != nil {
		return fmt.Errorf("archive %s: descriptor: %w", id, err)
	}
	layers := make([]*memsnap.Manifest, 0, len(desc.Layers))
	for _, layer := range desc.Layers {
		data, err := readObject(ctx, e.l1, nodeagent.KeyLayer(id, layer))
		if err != nil {
			return fmt.Errorf("archive %s: manifest %s: %w", id, layer, err)
		}
		m, err := memsnap.ParseManifest(data)
		if err != nil {
			return fmt.Errorf("archive %s: %w", id, err)
		}
		layers = append(layers, m)
	}
	view, err := memsnap.Resolve(layers)
	if err != nil {
		return fmt.Errorf("archive %s: %w", id, err)
	}
	syn, err := memsnap.Synthesize(view, "cold", time.Time{})
	if err != nil {
		return fmt.Errorf("archive %s: %w", id, err)
	}

	var hashes []string
	for _, c := range syn.Chunks {
		if !c.Zero {
			hashes = append(hashes, c.Hash)
		}
	}
	if _, err := (chunkstore.Copier{Src: e.l1, Dst: e.cold}).Copy(ctx, hashes); err != nil {
		return fmt.Errorf("archive %s: copy chunks: %w", id, err)
	}
	synData, err := json.Marshal(syn)
	if err != nil {
		return err
	}
	if err := e.cold.PutObject(ctx, nodeagent.KeyLayer(id, "cold"),
		bytes.NewReader(synData), int64(len(synData))); err != nil {
		return fmt.Errorf("archive %s: synthetic manifest: %w", id, err)
	}
	// The device snapfile of the newest layer, the WS trace, and the disk
	// delta chain move as-is.
	lastLayer := desc.Layers[len(desc.Layers)-1]
	if err := copyObject(ctx, e.l1, e.cold,
		nodeagent.KeySnapfile(id, lastLayer), nodeagent.KeySnapfile(id, "cold")); err != nil {
		return fmt.Errorf("archive %s: snapfile: %w", id, err)
	}
	if desc.HasWS {
		if err := copyObject(ctx, e.l1, e.cold, nodeagent.KeyWS(id), nodeagent.KeyWS(id)); err != nil {
			return fmt.Errorf("archive %s: ws: %w", id, err)
		}
	}
	for _, layer := range desc.DiskLayers {
		key := nodeagent.KeyDiskDelta(id, layer)
		// A sandbox that was cold-restored and re-archived only has its
		// NEWEST disk deltas in L1 — earlier segments were pruned by the
		// previous archive and still sit in the cold store. Delta streams
		// are immutable per tag, so present-in-cold means done.
		if ok, err := e.cold.HasObject(ctx, key); err != nil {
			return fmt.Errorf("archive %s: probe cold disk %s: %w", id, layer, err)
		} else if ok {
			continue
		}
		if err := copyObject(ctx, e.l1, e.cold, key, key); err != nil {
			return fmt.Errorf("archive %s: disk %s: %w", id, layer, err)
		}
	}
	desc.Tier = "cold"
	desc.Layers = []string{"cold"}
	descData, err := json.Marshal(desc)
	if err != nil {
		return err
	}
	if err := e.cold.PutObject(ctx, nodeagent.KeySnapshotJSON(id),
		bytes.NewReader(descData), int64(len(descData))); err != nil {
		return fmt.Errorf("archive %s: descriptor: %w", id, err)
	}

	return nil
}

// archivePrune removes the L1 copy after the CAS committed the COLD tier.
// Best-effort: a failure leaves stale-but-harmless L1 objects for the GC.
func (e *Engine) archivePrune(ctx context.Context, id string) {
	if err := deleteSandboxObjects(ctx, e.l1, id, ""); err != nil {
		log.Printf("lifecycle engine: prune L1 after archiving %s: %v", id, err)
		return
	}
	if res, err := chunkstore.GC(ctx, e.l1, e.cfg.GCGrace); err != nil {
		log.Printf("lifecycle engine: L1 GC after archiving %s: %v", id, err)
	} else if res.SweptChunks > 0 {
		log.Printf("lifecycle engine: L1 GC swept %d chunks after archiving %s", res.SweptChunks, id)
	}
}

// recyclePrune deletes every cold object but the artifacts after the CAS.
func (e *Engine) recyclePrune(ctx context.Context, id string) {
	if err := deleteSandboxObjects(ctx, e.cold, id, nodeagent.KeyArtifacts(id)); err != nil {
		log.Printf("lifecycle engine: prune cold after recycling %s: %v", id, err)
		return
	}
	if res, err := chunkstore.GC(ctx, e.cold, e.cfg.GCGrace); err != nil {
		log.Printf("lifecycle engine: cold GC after recycling %s: %v", id, err)
	} else if res.SweptChunks > 0 {
		log.Printf("lifecycle engine: cold GC swept %d chunks after recycling %s", res.SweptChunks, id)
	}
}

// prewarmScan pulls working sets back to the node ahead of predicted wakes
// (docs/zh/04 #5). No prediction (thin or noisy history) means the TTLs act
// as the fixed keep-alive fallback.
func (e *Engine) prewarmScan(ctx context.Context) error {
	now := time.Now()
	for state, tier := range map[lifecycle.State]string{
		lifecycle.StatePausedWarm:   "warm",
		lifecycle.StateArchivedCold: "cold",
	} {
		sbs, err := e.store.ListSandboxes(ctx, "", string(state))
		if err != nil {
			return err
		}
		for _, sb := range sbs {
			if sb.PrewarmedAt != nil || sb.PausedAt == nil {
				continue
			}
			intervals, err := e.store.WakeIntervals(ctx, sb.ID)
			if err != nil {
				return err
			}
			if !prewarm.ShouldPrewarm(now, *sb.PausedAt, intervals, e.cfg.PrewarmLead) {
				continue
			}
			agent, err := e.agentFor(sb.NodeID)
			if err != nil {
				log.Printf("lifecycle engine: prewarm %s: %v", sb.ID, err)
				continue
			}
			if err := agent.Prewarm(ctx, sb.ID, tier); err != nil {
				log.Printf("lifecycle engine: prewarm %s (%s): %v", sb.ID, tier, err)
				continue // advisory: never block the scan on a prewarm
			}
			t := now
			if err := e.store.SetPrewarmedAt(ctx, sb.ID, &t); err != nil {
				return err
			}
			log.Printf("lifecycle engine: prewarmed %s from %s tier", sb.ID, tier)
		}
	}
	return nil
}

// --- small store plumbing ----------------------------------------------------

func readObject(ctx context.Context, b chunkstore.Objects, key string) ([]byte, error) {
	rc, err := b.GetObject(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func readJSONObject(ctx context.Context, b chunkstore.Objects, key string, v any) error {
	data, err := readObject(ctx, b, key)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func copyObject(ctx context.Context, src, dst chunkstore.Objects, from, to string) error {
	data, err := readObject(ctx, src, from)
	if err != nil {
		return err
	}
	return dst.PutObject(ctx, to, bytes.NewReader(data), int64(len(data)))
}

// deleteSandboxObjects removes every object under sandboxes/<id>/ except
// keep (empty = delete everything).
func deleteSandboxObjects(ctx context.Context, b chunkstore.ListingBackend, id, keep string) error {
	keys, err := b.ListObjectKeys(ctx, "sandboxes/"+id+"/")
	if err != nil {
		return err
	}
	for _, key := range keys {
		if keep != "" && key == keep {
			continue
		}
		if err := b.DeleteObject(ctx, key); err != nil {
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
		agent, err := e.agentFor(sb.NodeID)
		if err != nil {
			log.Printf("lifecycle engine: demote %s: %v", sb.ID, err)
			continue
		}
		if err := e.transition(ctx, sb.ID, lifecycle.StatePausedHot, lifecycle.StatePausedWarm,
			func() error { return agent.ReleaseLocal(ctx, sb.ID) }); err != nil {
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
