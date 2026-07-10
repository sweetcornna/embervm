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
	for _, sb := range list {
		seen[sb.ID] = true
		if err := e.autoscaleOne(ctx, sb); err != nil {
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

func (e *Engine) autoscaleOne(ctx context.Context, sb Sandbox) error {
	ta, err := e.agentFor(sb.NodeID)
	if err != nil {
		return err
	}
	sa, ok := ta.(scaleAgent)
	if !ok {
		return nil
	}
	st := e.scale[sb.ID]
	if st == nil {
		st = &scaleState{}
		e.scale[sb.ID] = st
	}
	if time.Since(st.lastAction) < e.cfg.AutoscaleCooldown {
		return nil
	}
	h, err := sa.Health(ctx, sb.ID)
	if err != nil {
		*st = scaleState{lastAction: st.lastAction} // stale counters lie
		return err
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
	*st = scaleState{lastAction: time.Now()}
	log.Printf("engine: autoscale %s %s -> %d MiB / %d vcpus (avail %.0f%%, psi mem %.1f cpu %.1f)",
		sb.ID, dir, res.MemoryMiB, res.VCPUs, availPct, h.PSIMemSome10, h.PSICPUSome10)
	return nil
}
