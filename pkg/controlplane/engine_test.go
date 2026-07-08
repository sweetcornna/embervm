package controlplane

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/embervm/embervm/pkg/nodeapi"
	"github.com/google/uuid"
)

// mockTierAgent records tier verb calls and can inject failures.
type mockTierAgent struct {
	mu        sync.Mutex
	released  []string
	extracted map[string][]string
	restored  map[string]string // id -> tier
	failNext  error
}

func newMockTierAgent() *mockTierAgent {
	return &mockTierAgent{extracted: map[string][]string{}, restored: map[string]string{}}
}

func (m *mockTierAgent) takeFailure() error {
	err := m.failNext
	m.failNext = nil
	return err
}

func (m *mockTierAgent) ReleaseLocal(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.takeFailure(); err != nil {
		return err
	}
	m.released = append(m.released, id)
	return nil
}

func (m *mockTierAgent) RestoreSandbox(_ context.Context, id, tier string) (nodeapi.SandboxStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.takeFailure(); err != nil {
		return nodeapi.SandboxStatus{}, err
	}
	m.restored[id] = tier
	return nodeapi.SandboxStatus{SandboxID: id, State: "RUNNING"}, nil
}

func (m *mockTierAgent) ExtractArtifacts(_ context.Context, id string, paths []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.takeFailure(); err != nil {
		return err
	}
	m.extracted[id] = paths
	return nil
}

// pausedSandbox inserts a template + sandbox row sitting in `state` with an
// updated_at pushed `age` into the past.
func pausedSandbox(t *testing.T, s *Store, state string, age time.Duration) string {
	t.Helper()
	ctx := context.Background()
	tid := uuid.NewString()
	if _, err := s.CreateTemplate(ctx, tid, "tmpl-"+tid[:8], "alpine:3.20"); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	if _, err := s.CreateSandbox(ctx, Sandbox{
		ID: id, TemplateID: tid, State: state, VCPUs: 1, MemoryMiB: 256, DataDiskGiB: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE sandboxes SET updated_at = now() - $2::interval WHERE id=$1`,
		id, age.String()); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestEngineDemotesHotToWarm(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	agent := newMockTierAgent()
	e := NewEngine(s, agent, EngineConfig{TTLWarm: time.Minute})

	overdue := pausedSandbox(t, s, "PAUSED_HOT", 2*time.Minute)
	fresh := pausedSandbox(t, s, "PAUSED_HOT", time.Second)
	running := pausedSandbox(t, s, "RUNNING", time.Hour)

	if err := e.tickOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(agent.released) != 1 || agent.released[0] != overdue {
		t.Fatalf("released = %v, want exactly [%s]", agent.released, overdue)
	}
	for id, want := range map[string]string{overdue: "PAUSED_WARM", fresh: "PAUSED_HOT", running: "RUNNING"} {
		sb, err := s.GetSandbox(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if sb.State != want {
			t.Errorf("sandbox %s state = %s, want %s", id, sb.State, want)
		}
	}
	// The transition must be in the audit trail.
	n, err := s.CountSandboxEvents(ctx, overdue)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("no sandbox_events row for the WARM transition")
	}
}

func TestEngineZeroTTLDisables(t *testing.T) {
	s := testStore(t)
	agent := newMockTierAgent()
	e := NewEngine(s, agent, EngineConfig{}) // all TTLs zero

	id := pausedSandbox(t, s, "PAUSED_HOT", 24*time.Hour)
	if err := e.tickOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	sb, err := s.GetSandbox(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if sb.State != "PAUSED_HOT" || len(agent.released) != 0 {
		t.Fatalf("zero TTL still transitioned: state=%s released=%v", sb.State, agent.released)
	}
}

func TestEngineActionFailureMarksFailed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	agent := newMockTierAgent()
	agent.failNext = errors.New("dataset busy")
	e := NewEngine(s, agent, EngineConfig{TTLWarm: time.Minute})

	id := pausedSandbox(t, s, "PAUSED_HOT", time.Hour)
	if err := e.tickOnce(ctx); err != nil {
		t.Fatal(err)
	}
	sb, err := s.GetSandbox(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if sb.State != "FAILED" || sb.Error != "dataset busy" {
		t.Fatalf("failed action: state=%s error=%q, want FAILED/dataset busy", sb.State, sb.Error)
	}
}

func TestEngineCASLosesToConcurrentChange(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	agent := newMockTierAgent()
	e := NewEngine(s, agent, EngineConfig{TTLWarm: time.Minute})

	id := pausedSandbox(t, s, "PAUSED_HOT", time.Hour)
	// A resume slipped in between the scan and the CAS.
	if err := s.SetSandboxState(ctx, id, "PAUSED_HOT", "RESUMING", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.transition(ctx, id, "PAUSED_HOT", "PAUSED_WARM",
		func() error { return agent.ReleaseLocal(ctx, id) }); err != nil {
		t.Fatal(err)
	}
	sb, err := s.GetSandbox(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if sb.State != "RESUMING" || len(agent.released) != 0 {
		t.Fatalf("CAS race: state=%s released=%v, want RESUMING and no release", sb.State, agent.released)
	}
}

func TestTransitionSandboxCAS(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := pausedSandbox(t, s, "PAUSED_HOT", time.Hour)

	if err := s.TransitionSandbox(ctx, id, "PAUSED_HOT", "PAUSED_WARM", ""); err != nil {
		t.Fatal(err)
	}
	err := s.TransitionSandbox(ctx, id, "PAUSED_HOT", "PAUSED_WARM", "")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("second CAS = %v, want ErrConflict", err)
	}
}
