package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/embervm/embervm/pkg/guestapi"
	"github.com/embervm/embervm/pkg/nodeapi"
)

// scaleMockAgent is a cpMockAgent with controllable guest pressure and a
// resize recorder — the engine type-asserts it to scaleAgent.
type scaleMockAgent struct {
	cpMockAgent
	health    guestapi.HealthResponse
	healthErr error
	resized   []nodeapi.ResizeRequest
	cur       nodeapi.ResizeResult
}

func (m *scaleMockAgent) Health(context.Context, string) (*guestapi.HealthResponse, error) {
	if m.healthErr != nil {
		return nil, m.healthErr
	}
	h := m.health
	return &h, nil
}

func (m *scaleMockAgent) ResizeSandbox(_ context.Context, _ string, req nodeapi.ResizeRequest) (nodeapi.ResizeResult, error) {
	m.resized = append(m.resized, req)
	if req.MemoryMiB != 0 {
		m.cur.MemoryMiB = req.MemoryMiB
	}
	if req.VCPUs != 0 {
		m.cur.VCPUs = req.VCPUs
	}
	return m.cur, nil
}

// autoscaleSandbox inserts a RUNNING autoscale row.
func autoscaleSandbox(t *testing.T, s *Store, memMiB, maxMiB, vcpus, maxVCPUs int) Sandbox {
	t.Helper()
	ctx := context.Background()
	tid := uuid.NewString()
	if _, err := s.CreateTemplate(ctx, tid, "tmpl-"+tid[:8], "alpine:3.20"); err != nil {
		t.Fatal(err)
	}
	sb, err := s.CreateSandbox(ctx, Sandbox{
		ID: uuid.NewString(), TemplateID: tid, State: "RUNNING",
		VCPUs: vcpus, MemoryMiB: memMiB, DataDiskGiB: 1,
		MaxMemoryMiB: maxMiB, MaxVCPUs: maxVCPUs,
		BaseMemoryMiB: memMiB, BaseVCPUs: vcpus, Autoscale: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sb
}

func TestAutoscaleGrowShrinkMemory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	sb := autoscaleSandbox(t, s, 512, 1024, 1, 1)

	mock := &scaleMockAgent{cur: nodeapi.ResizeResult{MemoryMiB: 512, VCPUs: 1}}
	e := NewEngine(s, SingleAgent(mock), nil, nil, EngineConfig{
		AutoscaleStepMiB: 256, AutoscaleCooldown: time.Nanosecond,
	})

	// Pressure: 5% available → grow after exactly growTicksNeeded ticks.
	mock.health = guestapi.HealthResponse{MemTotalKiB: 512 << 10, MemAvailableKiB: 25 << 10}
	if err := e.autoscaleScan(ctx); err != nil {
		t.Fatal(err)
	}
	if len(mock.resized) != 0 {
		t.Fatalf("resized after 1 tick (hysteresis broken): %+v", mock.resized)
	}
	if err := e.autoscaleScan(ctx); err != nil {
		t.Fatal(err)
	}
	if len(mock.resized) != 1 || mock.resized[0].MemoryMiB != 768 {
		t.Fatalf("after 2 pressure ticks resized = %+v, want one grow to 768", mock.resized)
	}
	got, _ := s.GetSandbox(ctx, sb.ID)
	if got.MemoryMiB != 768 {
		t.Fatalf("accounting = %d MiB, want 768", got.MemoryMiB)
	}

	// Idle: >50% available → shrink only after shrinkTicksNeeded ticks,
	// and never below the create-time base.
	mock.health = guestapi.HealthResponse{MemTotalKiB: 768 << 10, MemAvailableKiB: 600 << 10}
	for i := 0; i < shrinkTicksNeeded; i++ {
		if err := e.autoscaleScan(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(mock.resized) != 2 || mock.resized[1].MemoryMiB != 512 {
		t.Fatalf("after idle ticks resized = %+v, want shrink to 512 (base)", mock.resized)
	}
	for i := 0; i < 2*shrinkTicksNeeded; i++ {
		if err := e.autoscaleScan(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(mock.resized) != 2 {
		t.Fatalf("shrank below base: %+v", mock.resized)
	}
}

func TestAutoscaleCooldownAndErrors(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	autoscaleSandbox(t, s, 512, 1024, 1, 1)

	mock := &scaleMockAgent{cur: nodeapi.ResizeResult{MemoryMiB: 512, VCPUs: 1}}
	e := NewEngine(s, SingleAgent(mock), nil, nil, EngineConfig{
		AutoscaleStepMiB: 256, AutoscaleCooldown: time.Hour,
	})
	mock.health = guestapi.HealthResponse{MemTotalKiB: 512 << 10, MemAvailableKiB: 10 << 10}
	for i := 0; i < 5; i++ {
		if err := e.autoscaleScan(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(mock.resized) != 1 {
		t.Fatalf("cooldown ignored: %d actions", len(mock.resized))
	}

	// An unreachable guest resets the counters instead of acting on stale
	// ones.
	e2 := NewEngine(s, SingleAgent(mock), nil, nil, EngineConfig{
		AutoscaleStepMiB: 256, AutoscaleCooldown: time.Nanosecond,
	})
	mock.resized = nil
	_ = e2.autoscaleScan(ctx) // tick 1: pressure
	mock.healthErr = errors.New("guest down")
	_ = e2.autoscaleScan(ctx) // tick 2: error → reset
	mock.healthErr = nil
	_ = e2.autoscaleScan(ctx) // tick 3: pressure again (counter restarted)
	if len(mock.resized) != 0 {
		t.Fatalf("acted on stale counters across a health error: %+v", mock.resized)
	}

	// CanFit denial defers the action.
	e3 := NewEngine(s, SingleAgent(mock), nil, nil, EngineConfig{
		AutoscaleStepMiB: 256, AutoscaleCooldown: time.Nanosecond,
	})
	e3.CanFit = func(context.Context, string, int, int) error { return ErrNoCapacity }
	mock.resized = nil
	for i := 0; i < 4; i++ {
		_ = e3.autoscaleScan(ctx)
	}
	if len(mock.resized) != 0 {
		t.Fatalf("resized despite CanFit denial: %+v", mock.resized)
	}
}
