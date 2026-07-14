// M6 automatic elasticity (ADR-0007 D5): the lifecycle engine polls the
// guest-reported pressure of opted-in RUNNING sandboxes and drives the same
// resize path the REST verb uses. Deliberately conservative: hysteresis
// counters on both directions, a cooldown after every action, and growth
// that silently defers when the node budget is full — the ceiling is a
// wish, the node is reality.

package controlplane

import (
	"context"
	"log"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/embervm/embervm/pkg/guestapi"
	"github.com/embervm/embervm/pkg/metrics"
	"github.com/embervm/embervm/pkg/nodeapi"
)

// Policy thresholds. Growth reacts in 2 ticks (a minute at the default
// 30s tick) because an OOM-bound guest cannot wait; shrink waits 10 ticks
// because flapping costs pause-time balloon churn and host page-table work.
const (
	memGrowPSI        = 20.0 // /proc/pressure/memory some avg10 above → grow
	cpuGrowPSI        = 20.0 // /proc/pressure/cpu some avg10 above → grow
	cpuShrinkPSI      = 5.0  // below → shrink
	growTicksNeeded   = 2
	shrinkTicksNeeded = 10
)

// Probe bounds (M7): with autoscale on by default the scan touches most
// RUNNING sandboxes, so probes run concurrently and each gets its own
// deadline — guestapi.Client takes timeouts only from the call ctx, and one
// wedged guest on the raw engine ctx used to stall the whole tick (the
// console's health path learned the same lesson).
const (
	autoscaleProbeTimeout  = 3 * time.Second
	autoscaleProbeParallel = 16
)

// scaleAgent is the slice of nodeapi.Agent the autoscale loop needs. The
// engine type-asserts its TierAgent; a mock without these verbs is skipped.
type scaleAgent interface {
	Health(ctx context.Context, sandboxID string) (*guestapi.HealthResponse, error)
	ResizeSandbox(ctx context.Context, sandboxID string, req nodeapi.ResizeRequest) (nodeapi.ResizeResult, error)
}

// scaleState is one sandbox's hysteresis memory between ticks.
type scaleState struct {
	memGrow, memShrink int
	cpuGrow, cpuShrink int
	lastAction         time.Time
	// deferred marks an in-progress "grow wants room the node lacks"
	// episode so the timeline gets ONE deferral event, not one per tick.
	deferred bool
}

// autoscaleScan is the per-tick entry point. Per-sandbox failures are
// logged, not returned: one unreachable guest must not veto the whole scan.
func (e *Engine) autoscaleScan(ctx context.Context) error {
	list, err := e.store.ListAutoscaleRunning(ctx)
	if err != nil {
		return err
	}
	if e.scale == nil {
		e.scale = map[string]*scaleState{}
	}
	seen := make(map[string]bool, len(list))

	// Phase 1: collect guest health concurrently (bounded). Hysteresis
	// state is only READ here (the cooldown skip); every write happens in
	// the serial phase 2, so the map needs no locking.
	type probe struct {
		sa  scaleAgent
		h   *guestapi.HealthResponse
		err error
	}
	probes := make([]probe, len(list))
	var g errgroup.Group
	g.SetLimit(autoscaleProbeParallel)
	for i, sb := range list {
		seen[sb.ID] = true
		st := e.scale[sb.ID]
		if st == nil {
			st = &scaleState{}
			e.scale[sb.ID] = st
		}
		if time.Since(st.lastAction) < e.cfg.AutoscaleCooldown {
			continue
		}
		ta, err := e.agentFor(sb.NodeID)
		if err != nil {
			probes[i].err = err
			continue
		}
		sa, ok := ta.(scaleAgent)
		if !ok {
			continue
		}
		probes[i].sa = sa
		i, id := i, sb.ID
		g.Go(func() error {
			pctx, cancel := context.WithTimeout(ctx, autoscaleProbeTimeout)
			defer cancel()
			probes[i].h, probes[i].err = sa.Health(pctx, id)
			return nil
		})
	}
	_ = g.Wait() // goroutines never return errors; results live in probes

	// Phase 2: decide and act serially — the hysteresis counters and the
	// CanFit-then-resize ordering stay single-threaded and deterministic.
	for i, sb := range list {
		p := probes[i]
		if p.sa == nil && p.err == nil {
			continue // cooldown, or an agent without the resize verbs
		}
		if err := e.autoscaleOne(ctx, sb, p.sa, p.h, p.err); err != nil {
			log.Printf("engine: autoscale %s: %v", sb.ID, err)
		}
	}
	for id := range e.scale {
		if !seen[id] {
			delete(e.scale, id) // paused/killed: forget its counters
		}
	}
	return nil
}

// autoscaleOne applies the hysteresis policy to one sandbox given its
// already-collected health probe. sa == nil means the node agent could not
// be resolved (transient registry blip: counters keep); herr != nil with an
// agent means the guest probe failed (counters reset — stale counters lie).
func (e *Engine) autoscaleOne(ctx context.Context, sb Sandbox, sa scaleAgent, h *guestapi.HealthResponse, herr error) error {
	if sa == nil {
		return herr
	}
	st := e.scale[sb.ID]
	if herr != nil {
		*st = scaleState{lastAction: st.lastAction, deferred: st.deferred}
		return herr
	}
	if h.MemTotalKiB == 0 {
		return nil // guestd predates the pressure fields
	}
	availPct := float64(h.MemAvailableKiB) * 100 / float64(h.MemTotalKiB)

	var req nodeapi.ResizeRequest
	if sb.MaxMemoryMiB > 0 {
		floor := sb.BaseMemoryMiB
		if floor <= 0 {
			floor = sb.MemoryMiB
		}
		switch {
		case availPct < e.cfg.AutoscaleGrowAvailPct || h.PSIMemSome10 > memGrowPSI:
			st.memGrow, st.memShrink = st.memGrow+1, 0
		case availPct > e.cfg.AutoscaleShrinkAvailPct && h.PSIMemSome10 < 1:
			st.memShrink, st.memGrow = st.memShrink+1, 0
		default:
			st.memGrow, st.memShrink = 0, 0
		}
		switch {
		case st.memGrow >= growTicksNeeded && sb.MemoryMiB < sb.MaxMemoryMiB:
			req.MemoryMiB = min(sb.MemoryMiB+e.cfg.AutoscaleStepMiB, sb.MaxMemoryMiB)
		case st.memShrink >= shrinkTicksNeeded && sb.MemoryMiB > floor:
			req.MemoryMiB = max(sb.MemoryMiB-e.cfg.AutoscaleStepMiB, floor)
		}
	}
	if sb.MaxVCPUs > 0 {
		floor := max(sb.BaseVCPUs, 1)
		switch {
		case h.PSICPUSome10 > cpuGrowPSI:
			st.cpuGrow, st.cpuShrink = st.cpuGrow+1, 0
		case h.PSICPUSome10 < cpuShrinkPSI:
			st.cpuShrink, st.cpuGrow = st.cpuShrink+1, 0
		default:
			st.cpuGrow, st.cpuShrink = 0, 0
		}
		switch {
		case st.cpuGrow >= growTicksNeeded && sb.VCPUs < sb.MaxVCPUs:
			req.VCPUs = sb.VCPUs + 1
		case st.cpuShrink >= shrinkTicksNeeded && sb.VCPUs > floor:
			req.VCPUs = sb.VCPUs - 1
		}
	}
	if req.MemoryMiB == 0 && req.VCPUs == 0 {
		return nil
	}
	if e.CanFit != nil {
		if err := e.CanFit(ctx, sb.NodeID, req.MemoryMiB-sb.MemoryMiB, req.VCPUs-sb.VCPUs); err != nil {
			// Deferred, not failed: keep the counters, back off one
			// cooldown, and retry when the node has room.
			metrics.AutoscaleActions.WithLabelValues("deferred").Inc()
			if !st.deferred {
				st.deferred = true
				e.recordScaleEvent(ctx, sb, ResourceEventDetail{
					Kind: "resize", Actor: "autoscale", Reason: "deferred",
					PSIMem: h.PSIMemSome10, AvailPct: availPct,
				})
			}
			st.lastAction = time.Now()
			return nil
		}
	}
	res, err := sa.ResizeSandbox(ctx, sb.ID, req)
	if res.MemoryMiB > 0 {
		// The achieved geometry is truth even when the resize errored
		// halfway — NodeUsage must track it.
		_ = e.store.UpdateSandboxGeometry(ctx, sb.ID, res.VCPUs, res.MemoryMiB)
	}
	if err != nil {
		return err
	}
	dir := "grow"
	if (req.MemoryMiB != 0 && req.MemoryMiB < sb.MemoryMiB) || (req.VCPUs != 0 && req.VCPUs < sb.VCPUs) {
		dir = "shrink"
	}
	metrics.AutoscaleActions.WithLabelValues(dir).Inc()
	detail := ResourceEventDetail{Kind: "resize", Actor: "autoscale", Reason: "pressure",
		PSIMem: h.PSIMemSome10, AvailPct: availPct}
	if res.MemoryMiB > 0 && res.MemoryMiB != sb.MemoryMiB {
		detail.MemoryMiB = &[2]int{sb.MemoryMiB, res.MemoryMiB}
	}
	if res.VCPUs > 0 && res.VCPUs != sb.VCPUs {
		detail.VCPUs = &[2]int{sb.VCPUs, res.VCPUs}
	}
	e.recordScaleEvent(ctx, sb, detail)
	*st = scaleState{lastAction: time.Now()}
	log.Printf("engine: autoscale %s %s -> %d MiB / %d vcpus (avail %.0f%%, psi mem %.1f cpu %.1f)",
		sb.ID, dir, res.MemoryMiB, res.VCPUs, availPct, h.PSIMemSome10, h.PSICPUSome10)
	return nil
}

// recordScaleEvent appends an autoscale timeline row. Advisory only: the
// action already happened (or was deferred); a failed event write must not
// disturb the loop.
func (e *Engine) recordScaleEvent(ctx context.Context, sb Sandbox, detail ResourceEventDetail) {
	if err := e.store.AppendSandboxEvent(ctx, sb.ID, sb.State, detail); err != nil {
		log.Printf("engine: autoscale %s: record event: %v", sb.ID, err)
	}
}
