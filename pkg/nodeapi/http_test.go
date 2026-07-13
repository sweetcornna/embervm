package nodeapi

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/embervm/embervm/pkg/guestapi"
)

// mockAgent records the last call and returns scripted results.
type mockAgent struct {
	lastCreate CreateSandboxRequest
	lastExec   *guestapi.ExecRequest
	lastWrite  struct {
		path string
		mode fs.FileMode
		data []byte
	}
	lastBalloon  int
	lastResize   ResizeRequest
	lastFork     struct{ parent, layer, newID string }
	lastRollback string
	failStop     bool
}

func (m *mockAgent) BuildTemplate(_ context.Context, id, image string) error { return nil }
func (m *mockAgent) Healthz(context.Context) (NodeHealth, error) {
	return NodeHealth{CapacityMiB: 4096, UsedMiB: 512, Sandboxes: 2}, nil
}
func (m *mockAgent) CreateSandbox(_ context.Context, req CreateSandboxRequest) (SandboxStatus, error) {
	m.lastCreate = req
	return SandboxStatus{SandboxID: req.SandboxID, State: "RUNNING", GuestAddr: "172.16.0.2:7777", Netns: "ember0"}, nil
}
func (m *mockAgent) Status(_ context.Context, id string) (SandboxStatus, error) {
	return SandboxStatus{SandboxID: id, State: "RUNNING"}, nil
}
func (m *mockAgent) StopSandbox(_ context.Context, id string) error {
	if m.failStop {
		return errors.New("boom")
	}
	return nil
}
func (m *mockAgent) PauseSandbox(_ context.Context, id string) error { return nil }
func (m *mockAgent) ResumeSandbox(_ context.Context, id string) (SandboxStatus, error) {
	return SandboxStatus{SandboxID: id, State: "RUNNING"}, nil
}
func (m *mockAgent) SnapshotSandbox(_ context.Context, id, tag string) (string, error) {
	return "snap-" + id + "-" + tag, nil
}
func (m *mockAgent) ReleaseLocal(_ context.Context, id string) error { return nil }
func (m *mockAgent) RestoreSandbox(_ context.Context, id, tier string) (SandboxStatus, error) {
	return SandboxStatus{SandboxID: id, State: "RUNNING", Netns: "tier-" + tier}, nil
}
func (m *mockAgent) ExtractArtifacts(_ context.Context, id string, paths []string) error {
	return nil
}
func (m *mockAgent) Prewarm(_ context.Context, id, tier string) error { return nil }
func (m *mockAgent) SetBalloon(_ context.Context, id string, mib int) error {
	m.lastBalloon = mib
	return nil
}
func (m *mockAgent) ResizeSandbox(_ context.Context, id string, req ResizeRequest) (ResizeResult, error) {
	m.lastResize = req
	return ResizeResult{MemoryMiB: req.MemoryMiB, VCPUs: req.VCPUs}, nil
}
func (m *mockAgent) Fork(_ context.Context, parentID, layer, newID string) (SandboxStatus, error) {
	m.lastFork = struct{ parent, layer, newID string }{parentID, layer, newID}
	return SandboxStatus{SandboxID: newID, State: "RUNNING", Netns: "ember1"}, nil
}
func (m *mockAgent) Rollback(_ context.Context, id, layer string) (SandboxStatus, error) {
	m.lastRollback = layer
	return SandboxStatus{SandboxID: id, State: "RUNNING"}, nil
}

func (m *mockAgent) Exec(_ context.Context, id string, req *guestapi.ExecRequest) (*guestapi.ExecResponse, error) {
	m.lastExec = req
	return &guestapi.ExecResponse{ExitCode: 0, Stdout: []byte("ok")}, nil
}
func (m *mockAgent) Health(_ context.Context, id string) (*guestapi.HealthResponse, error) {
	return &guestapi.HealthResponse{OK: true, Seq: 7}, nil
}
func (m *mockAgent) ReadFile(_ context.Context, id, path string) ([]byte, error) {
	return []byte("file:" + path), nil
}
func (m *mockAgent) WriteFile(_ context.Context, id, path string, mode fs.FileMode, data []byte) error {
	m.lastWrite.path, m.lastWrite.mode, m.lastWrite.data = path, mode, data
	return nil
}
func (m *mockAgent) ListDir(_ context.Context, id, path string) (*guestapi.ListDirResponse, error) {
	return &guestapi.ListDirResponse{Path: path, Entries: []guestapi.DirEntry{
		{Name: "bin", IsDir: true, Mode: "drwxr-xr-x"},
		{Name: "hello.txt", Size: 5, Mode: "-rw-r--r--"},
	}}, nil
}

func serveMock(t *testing.T, m Agent) *Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "na")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "n.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: NewServer(m)}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return NewClient(sock)
}

func TestNodeAPIRoundtrip(t *testing.T) {
	m := &mockAgent{}
	c := serveMock(t, m)
	ctx := context.Background()

	if err := c.BuildTemplate(ctx, "tpl1", "alpine:3.20"); err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}

	st, err := c.CreateSandbox(ctx, CreateSandboxRequest{
		SandboxID: "sbx1", TemplateID: "tpl1", VCPUs: 2, MemoryMiB: 256, DataDiskGiB: 15,
	})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if st.State != "RUNNING" || st.GuestAddr != "172.16.0.2:7777" {
		t.Errorf("status = %+v", st)
	}
	if m.lastCreate.DataDiskGiB != 15 || m.lastCreate.VCPUs != 2 {
		t.Errorf("server saw create req %+v", m.lastCreate)
	}

	if _, err := c.ResumeSandbox(ctx, "sbx1"); err != nil {
		t.Fatalf("ResumeSandbox: %v", err)
	}
	if err := c.PauseSandbox(ctx, "sbx1"); err != nil {
		t.Fatalf("PauseSandbox: %v", err)
	}
	snap, err := c.SnapshotSandbox(ctx, "sbx1", "p1")
	if err != nil {
		t.Fatalf("SnapshotSandbox: %v", err)
	}
	if snap != "snap-sbx1-p1" {
		t.Errorf("snapshot id = %q", snap)
	}

	exec, err := c.Exec(ctx, "sbx1", &guestapi.ExecRequest{Cmd: "echo", Args: []string{"hi"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if exec.ExitCode != 0 || string(exec.Stdout) != "ok" {
		t.Errorf("exec resp = %+v", exec)
	}
	if m.lastExec.Cmd != "echo" {
		t.Errorf("server saw exec %+v", m.lastExec)
	}

	h, err := c.Health(ctx, "sbx1")
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !h.OK || h.Seq != 7 {
		t.Errorf("health = %+v", h)
	}

	data, err := c.ReadFile(ctx, "sbx1", "/etc/hostname")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "file:/etc/hostname" {
		t.Errorf("read = %q", data)
	}

	if err := c.WriteFile(ctx, "sbx1", "/tmp/x", 0o600, []byte("payload")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if m.lastWrite.path != "/tmp/x" || m.lastWrite.mode != 0o600 || string(m.lastWrite.data) != "payload" {
		t.Errorf("server saw write %+v", m.lastWrite)
	}

	listing, err := c.ListDir(ctx, "sbx1", "/opt")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if listing.Path != "/opt" || len(listing.Entries) != 2 || !listing.Entries[0].IsDir {
		t.Errorf("listing = %+v", listing)
	}

	if err := c.SetBalloon(ctx, "sbx1", 128); err != nil {
		t.Fatalf("SetBalloon: %v", err)
	}
	if m.lastBalloon != 128 {
		t.Errorf("server saw balloon target %d, want 128", m.lastBalloon)
	}

	if err := c.StopSandbox(ctx, "sbx1"); err != nil {
		t.Fatalf("StopSandbox: %v", err)
	}
}

func TestNodeAPIPropagatesError(t *testing.T) {
	c := serveMock(t, &mockAgent{failStop: true})
	if err := c.StopSandbox(context.Background(), "sbx1"); err == nil {
		t.Error("StopSandbox against failing agent: want error")
	}
}
