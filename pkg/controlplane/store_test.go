package controlplane

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
)

// testStore connects to the PG named by EMBERVM_TEST_DATABASE_URL, migrates,
// and truncates the tables. Gated behind EMBERVM_PG_TESTS=1.
func testStore(t *testing.T) *Store {
	t.Helper()
	if os.Getenv("EMBERVM_PG_TESTS") != "1" {
		t.Skip("set EMBERVM_PG_TESTS=1 (and EMBERVM_TEST_DATABASE_URL) to run store tests")
	}
	url := os.Getenv("EMBERVM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("EMBERVM_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	s, err := NewStore(ctx, url)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `TRUNCATE templates, sandboxes, sandbox_events CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestStoreTemplateCRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := uuid.NewString()

	tpl, err := s.CreateTemplate(ctx, id, "web", "alpine:3.20")
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	if tpl.State != "BUILDING" {
		t.Errorf("initial state = %q, want BUILDING", tpl.State)
	}

	if err := s.SetTemplateState(ctx, id, "READY", ""); err != nil {
		t.Fatalf("SetTemplateState: %v", err)
	}
	got, err := s.GetTemplate(ctx, id)
	if err != nil {
		t.Fatalf("GetTemplate: %v", err)
	}
	if got.State != "READY" || got.ReadyAt == nil {
		t.Errorf("after ready: state=%q ready_at=%v", got.State, got.ReadyAt)
	}

	list, err := s.ListTemplates(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListTemplates = %d rows, err=%v", len(list), err)
	}

	if err := s.DeleteTemplate(ctx, id); err != nil {
		t.Fatalf("DeleteTemplate: %v", err)
	}
	if _, err := s.GetTemplate(ctx, id); err != ErrNotFound {
		t.Errorf("GetTemplate after delete = %v, want ErrNotFound", err)
	}
}

func TestStoreSandboxLifecycleAndEvents(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tplID := uuid.NewString()
	if _, err := s.CreateTemplate(ctx, tplID, "web", "alpine:3.20"); err != nil {
		t.Fatal(err)
	}

	sbID := uuid.NewString()
	sb, err := s.CreateSandbox(ctx, Sandbox{
		ID: sbID, TemplateID: tplID, State: "PENDING",
		VCPUs: 2, MemoryMiB: 256, DataDiskGiB: 15, Owner: "alice",
	})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if sb.State != "PENDING" {
		t.Errorf("state = %q", sb.State)
	}

	for _, tr := range []struct{ from, to string }{
		{"PENDING", "STARTING"}, {"STARTING", "RUNNING"},
		{"RUNNING", "PAUSING"}, {"PAUSING", "PAUSED_HOT"},
	} {
		if err := s.SetSandboxState(ctx, sbID, tr.from, tr.to, "ember3", ""); err != nil {
			t.Fatalf("SetSandboxState %s->%s: %v", tr.from, tr.to, err)
		}
	}

	got, err := s.GetSandbox(ctx, sbID)
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	if got.State != "PAUSED_HOT" || got.PausedAt == nil {
		t.Errorf("state=%q paused_at=%v", got.State, got.PausedAt)
	}
	if got.Netns != "ember3" {
		t.Errorf("netns = %q, want ember3", got.Netns)
	}

	events, err := s.CountSandboxEvents(ctx, sbID)
	if err != nil {
		t.Fatalf("CountSandboxEvents: %v", err)
	}
	if events != 4 {
		t.Errorf("events = %d, want 4", events)
	}

	// Quota denominator: one active sandbox for alice.
	n, err := s.CountActiveSandboxes(ctx, "alice")
	if err != nil {
		t.Fatalf("CountActiveSandboxes: %v", err)
	}
	if n != 1 {
		t.Errorf("active = %d, want 1", n)
	}
	// After stopping, it no longer counts.
	if err := s.SetSandboxState(ctx, sbID, "PAUSED_HOT", "STOPPED", "", ""); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountActiveSandboxes(ctx, "alice"); n != 0 {
		t.Errorf("active after stop = %d, want 0", n)
	}
}

func TestStoreListFilter(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tplID := uuid.NewString()
	if _, err := s.CreateTemplate(ctx, tplID, "web", "img"); err != nil {
		t.Fatal(err)
	}
	for _, st := range []string{"RUNNING", "RUNNING", "STOPPED"} {
		if _, err := s.CreateSandbox(ctx, Sandbox{
			ID: uuid.NewString(), TemplateID: tplID, State: st, VCPUs: 1, MemoryMiB: 128, DataDiskGiB: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	running, err := s.ListSandboxes(ctx, "RUNNING")
	if err != nil || len(running) != 2 {
		t.Fatalf("ListSandboxes(RUNNING) = %d, err=%v", len(running), err)
	}
	all, _ := s.ListSandboxes(ctx, "")
	if len(all) != 3 {
		t.Errorf("ListSandboxes(all) = %d, want 3", len(all))
	}
}
