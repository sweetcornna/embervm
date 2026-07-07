package guestapi_test

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/embervm/embervm/pkg/guestapi"
	"github.com/embervm/embervm/pkg/guestd"
)

// newClient wires the client against the REAL guestd handler so the wire
// format is exercised end to end.
func newClient(t *testing.T) *guestapi.Client {
	t.Helper()
	srv := httptest.NewServer(guestd.NewServer(guestd.Options{Version: "client-test"}))
	t.Cleanup(srv.Close)
	return guestapi.NewClient(srv.URL, nil)
}

func TestClientHealthSeqContinuity(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	h1, err := c.Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	h2, err := c.Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !h1.OK || h1.Seq != 1 || h2.Seq != 2 {
		t.Errorf("health = %+v then %+v, want ok with seq 1 then 2", h1, h2)
	}
	if h1.Version != "client-test" {
		t.Errorf("version = %q, want %q", h1.Version, "client-test")
	}
}

func TestClientExec(t *testing.T) {
	c := newClient(t)
	out, err := c.Exec(context.Background(), &guestapi.ExecRequest{
		Cmd: "sh", Args: []string{"-c", "printf hi >&2; exit 4"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out.ExitCode != 4 {
		t.Errorf("exit_code = %d, want 4", out.ExitCode)
	}
	if got := string(out.Stderr); got != "hi" {
		t.Errorf("stderr = %q, want %q", got, "hi")
	}
}

func TestClientExecStartFailureIsError(t *testing.T) {
	c := newClient(t)
	_, err := c.Exec(context.Background(), &guestapi.ExecRequest{Cmd: "/nonexistent-embervm-binary"})
	if err == nil {
		t.Fatal("Exec of nonexistent binary: err = nil, want error")
	}
}

func TestClientFilesRoundtrip(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nested", "file.txt")

	if err := c.WriteFile(ctx, path, 0o640, []byte("payload")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	data, err := c.ReadFile(ctx, path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "payload" {
		t.Errorf("data = %q, want %q", data, "payload")
	}
}

func TestClientReadFileMissing(t *testing.T) {
	c := newClient(t)
	_, err := c.ReadFile(context.Background(), filepath.Join(t.TempDir(), "missing"))
	if err == nil {
		t.Fatal("ReadFile(missing): err = nil, want error")
	}
	if !strings.Contains(err.Error(), "404") && !strings.Contains(err.Error(), "no such file") {
		t.Errorf("error = %q, want it to carry the server's 404 detail", err)
	}
}
