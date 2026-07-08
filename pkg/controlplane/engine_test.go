package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/embervm/embervm/pkg/chunkstore"
	"github.com/embervm/embervm/pkg/memsnap"
	"github.com/embervm/embervm/pkg/nodeagent"
	"github.com/embervm/embervm/pkg/nodeapi"
	"github.com/google/uuid"
)

// mockTierAgent records tier verb calls and can inject failures.
type mockTierAgent struct {
	mu           sync.Mutex
	released     []string
	extracted    map[string][]string
	restored     map[string]string // id -> tier
	prewarmed    map[string]string // id -> tier
	prewarmCalls int
	failNext     error
}

func newMockTierAgent() *mockTierAgent {
	return &mockTierAgent{
		extracted: map[string][]string{},
		restored:  map[string]string{},
		prewarmed: map[string]string{},
	}
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

func (m *mockTierAgent) Prewarm(_ context.Context, id, tier string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.takeFailure(); err != nil {
		return err
	}
	m.prewarmed[id] = tier
	m.prewarmCalls++
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
	e := NewEngine(s, SingleAgent(agent), nil, nil, EngineConfig{TTLWarm: time.Minute})

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
	e := NewEngine(s, SingleAgent(agent), nil, nil, EngineConfig{}) // all TTLs zero

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
	e := NewEngine(s, SingleAgent(agent), nil, nil, EngineConfig{TTLWarm: time.Minute})

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
	e := NewEngine(s, SingleAgent(agent), nil, nil, EngineConfig{TTLWarm: time.Minute})

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

// dirStores builds two real Dir-backed stores (L1 + cold) for engine tests.
func dirStores(t *testing.T) (l1, cold *chunkstore.Dir) {
	t.Helper()
	var err error
	l1, err = chunkstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cold, err = chunkstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return l1, cold
}

// seedWarmSnapshot writes a 2-layer snapshot (full + diff) for id into l1,
// returning the layer chunks' hashes.
func seedWarmSnapshot(t *testing.T, l1 *chunkstore.Dir, id string) []string {
	t.Helper()
	ctx := context.Background()
	sink := chunkstore.Bytes{Ctx: ctx, S: l1}
	img := bytes.Repeat([]byte("M3"), 32*1024) // 4 chunks of 16KiB
	dir := t.TempDir()
	memPath := filepath.Join(dir, "memfile")
	if err := os.WriteFile(memPath, img, 0o644); err != nil {
		t.Fatal(err)
	}
	full, err := memsnap.WriteLayer(memPath, memsnap.WriteOptions{LayerID: "p1"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	putJSONManifest(t, l1, nodeagent.KeyLayer(id, "p1"), full)
	putBytes(t, l1, nodeagent.KeySnapfile(id, "p1"), []byte("snapfile-bytes"))
	putBytes(t, l1, nodeagent.KeyWS(id), []byte(`{"format_version":1,"chunk_size":16384,"chunks":[0,1]}`))
	putBytes(t, l1, nodeagent.KeyDiskDelta(id, "p1"), []byte("zstream-bytes"))
	desc := nodeagent.SnapshotDescriptor{
		FormatVersion: 1, SandboxID: id, TemplateID: "tmpl", VCPUs: 1, MemoryMiB: 256,
		DataDiskGiB: 1, Dir: "/pool/sandboxes/" + id, Layers: []string{"p1"},
		HasWS: true, Tier: "warm", DiskLayers: []string{"p1"}, SnapSeq: 1,
	}
	putJSON(t, l1, nodeagent.KeySnapshotJSON(id), desc)
	var hashes []string
	for _, c := range full.Chunks {
		if !c.Zero {
			hashes = append(hashes, c.Hash)
		}
	}
	return hashes
}

func putBytes(t *testing.T, b chunkstore.Objects, key string, data []byte) {
	t.Helper()
	if err := b.PutObject(context.Background(), key, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
}

func putJSON(t *testing.T, b chunkstore.Objects, key string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	putBytes(t, b, key, data)
}

func putJSONManifest(t *testing.T, b chunkstore.Objects, key string, m *memsnap.Manifest) {
	t.Helper()
	putJSON(t, b, key, m)
}

func TestEngineArchivesWarmToCold(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	agent := newMockTierAgent()
	l1, cold := dirStores(t)
	e := NewEngine(s, SingleAgent(agent), l1, cold, EngineConfig{TTLCold: time.Minute, GCGrace: time.Nanosecond})

	id := pausedSandbox(t, s, "PAUSED_WARM", time.Hour)
	hashes := seedWarmSnapshot(t, l1, id)

	if err := e.tickOnce(ctx); err != nil {
		t.Fatal(err)
	}
	sb, err := s.GetSandbox(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if sb.State != "ARCHIVED_COLD" {
		t.Fatalf("state = %s, want ARCHIVED_COLD", sb.State)
	}
	// Cold store holds the synthetic full + chunks + descriptor.
	var desc nodeagent.SnapshotDescriptor
	if err := readJSONObject(ctx, cold, nodeagent.KeySnapshotJSON(id), &desc); err != nil {
		t.Fatalf("cold descriptor: %v", err)
	}
	if desc.Tier != "cold" || len(desc.Layers) != 1 || desc.Layers[0] != "cold" {
		t.Fatalf("cold descriptor = %+v", desc)
	}
	if len(desc.DiskLayers) != 1 || desc.DiskLayers[0] != "p1" {
		t.Fatalf("disk layers not preserved: %+v", desc.DiskLayers)
	}
	for _, h := range hashes {
		if ok, _ := cold.Has(ctx, h); !ok {
			t.Fatalf("chunk %s missing from cold store", h)
		}
	}
	if ok, _ := cold.HasObject(ctx, nodeagent.KeySnapfile(id, "cold")); !ok {
		t.Fatal("snapfile-cold missing")
	}
	if ok, _ := cold.HasObject(ctx, nodeagent.KeyDiskDelta(id, "p1")); !ok {
		t.Fatal("disk delta missing from cold store")
	}
	// L1 no longer holds the sandbox's objects, and its chunks were GC'd.
	if ok, _ := l1.HasObject(ctx, nodeagent.KeySnapshotJSON(id)); ok {
		t.Fatal("L1 descriptor survived archive")
	}
	for _, h := range hashes {
		if ok, _ := l1.Has(ctx, h); ok {
			t.Fatalf("chunk %s survived L1 GC", h)
		}
	}
}

func TestEngineRecyclesCold(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	agent := newMockTierAgent()
	l1, cold := dirStores(t)
	e := NewEngine(s, SingleAgent(agent), l1, cold, EngineConfig{TTLRecycle: time.Minute, GCGrace: time.Nanosecond})

	id := pausedSandboxWithArtifacts(t, s, "ARCHIVED_COLD", time.Hour, []string{"/dirty.bin"})
	// Simulate the archived state: objects live in cold.
	seedWarmSnapshot(t, cold, id)
	// The agent's ExtractArtifacts would write this; the mock does not, so
	// pre-place it to verify the engine KEEPS it while pruning the rest.
	putBytes(t, cold, nodeagent.KeyArtifacts(id), []byte("tarball"))

	if err := e.tickOnce(ctx); err != nil {
		t.Fatal(err)
	}
	sb, err := s.GetSandbox(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if sb.State != "RECYCLED" {
		t.Fatalf("state = %s, want RECYCLED", sb.State)
	}
	if got := agent.extracted[id]; len(got) != 1 || got[0] != "/dirty.bin" {
		t.Fatalf("ExtractArtifacts called with %v", got)
	}
	if ok, _ := cold.HasObject(ctx, nodeagent.KeyArtifacts(id)); !ok {
		t.Fatal("artifacts pruned — recycle must keep them")
	}
	if ok, _ := cold.HasObject(ctx, nodeagent.KeySnapshotJSON(id)); ok {
		t.Fatal("descriptor survived recycle")
	}
	keys, err := cold.ListObjectKeys(ctx, "sandboxes/"+id+"/")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("cold keys after recycle = %v, want only artifacts", keys)
	}
}

func TestEnginePrewarmsPredictedWake(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	agent := newMockTierAgent()
	l1, cold := dirStores(t)
	e := NewEngine(s, SingleAgent(agent), l1, cold, EngineConfig{PrewarmLead: time.Hour})

	id := pausedSandbox(t, s, "PAUSED_WARM", 25*time.Minute)
	// History: three ~30-minute wake intervals -> predicted wake ≈ paused+28m,
	// within the 1h lead of a 25-minute-old pause.
	seedWakeHistory(t, s, id, []time.Duration{28 * time.Minute, 30 * time.Minute, 31 * time.Minute})
	if _, err := s.pool.Exec(ctx,
		`UPDATE sandboxes SET paused_at = now() - interval '25 minutes' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}

	if err := e.tickOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if tier := agent.prewarmed[id]; tier != "warm" {
		t.Fatalf("prewarmed[%s] = %q, want warm", id, tier)
	}
	sb, err := s.GetSandbox(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if sb.PrewarmedAt == nil {
		t.Fatal("prewarmed_at not stamped")
	}
	// Second tick must not prewarm again.
	if err := e.tickOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if agent.prewarmCalls != 1 {
		t.Fatalf("prewarm called %d times, want 1", agent.prewarmCalls)
	}
}

// seedWakeHistory fabricates pause→resume event pairs with the given
// intervals.
func seedWakeHistory(t *testing.T, s *Store, id string, intervals []time.Duration) {
	t.Helper()
	ctx := context.Background()
	at := time.Now().Add(-48 * time.Hour)
	for _, iv := range intervals {
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO sandbox_events (sandbox_id, from_state, to_state, at) VALUES ($1,'PAUSING','PAUSED_HOT',$2)`,
			id, at); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO sandbox_events (sandbox_id, from_state, to_state, at) VALUES ($1,'PAUSED_HOT','RESUMING',$2)`,
			id, at.Add(iv)); err != nil {
			t.Fatal(err)
		}
		at = at.Add(iv + time.Hour)
	}
}

// pausedSandboxWithArtifacts is pausedSandbox plus artifact_paths.
func pausedSandboxWithArtifacts(t *testing.T, s *Store, state string, age time.Duration, paths []string) string {
	t.Helper()
	id := pausedSandbox(t, s, state, age)
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE sandboxes SET artifact_paths=$2 WHERE id=$1`, id, paths); err != nil {
		t.Fatal(err)
	}
	return id
}
