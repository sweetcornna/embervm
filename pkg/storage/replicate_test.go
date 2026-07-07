package storage

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
)

// fakeStream records stream invocations and simulates stream contents.
type fakeStream struct {
	calls  [][]string
	output string // bytes "sent" to w on send calls
	gotIn  []string
}

func (f *fakeStream) run(_ context.Context, stdin io.Reader, stdout io.Writer, name string, args ...string) error {
	f.calls = append(f.calls, append([]string{name}, args...))
	if stdin != nil {
		data, _ := io.ReadAll(stdin)
		f.gotIn = append(f.gotIn, string(data))
	}
	if stdout != nil {
		_, _ = io.WriteString(stdout, f.output)
	}
	return nil
}

func newReplBackend(t *testing.T) (*ZFSBackend, *fakeZFS, *fakeStream) {
	t.Helper()
	fz := &fakeZFS{existing: map[string]bool{}, mountRoot: t.TempDir()}
	fs := &fakeStream{output: "STREAM"}
	return &ZFSBackend{pool: "embervm/n1", run: fz.run, srun: fs.run}, fz, fs
}

func TestSendTemplateArgs(t *testing.T) {
	b, _, fs := newReplBackend(t)
	var buf bytes.Buffer
	if err := b.SendTemplate(context.Background(), "tmpl1", &buf); err != nil {
		t.Fatal(err)
	}
	want := []string{"zfs", "send", "-c", "embervm/n1/templates/tmpl1@final"}
	if !reflect.DeepEqual(fs.calls[0], want) {
		t.Fatalf("calls[0] = %v, want %v", fs.calls[0], want)
	}
	if buf.String() != "STREAM" {
		t.Fatalf("stream bytes = %q", buf.String())
	}
}

func TestReceiveTemplateIdempotent(t *testing.T) {
	b, fz, fs := newReplBackend(t)
	fz.existing["embervm/n1/templates/tmpl1@final"] = true
	if err := b.ReceiveTemplate(context.Background(), "tmpl1", strings.NewReader("X")); err != nil {
		t.Fatal(err)
	}
	if len(fs.calls) != 0 {
		t.Fatalf("existing template still received: %v", fs.calls)
	}

	if err := b.ReceiveTemplate(context.Background(), "tmpl2", strings.NewReader("DATA")); err != nil {
		t.Fatal(err)
	}
	// The templates/ container must be created first: zfs receive does not
	// create ancestors, and a receiving node may never have built one.
	if !fz.findCall("zfs", "create", "-p", "embervm/n1/templates") {
		t.Fatalf("missing ancestor create before receive: %v", fz.calls)
	}
	want := []string{"zfs", "receive", "embervm/n1/templates/tmpl2"}
	if !reflect.DeepEqual(fs.calls[0], want) {
		t.Fatalf("calls[0] = %v, want %v", fs.calls[0], want)
	}
	if fs.gotIn[0] != "DATA" {
		t.Fatalf("receive stdin = %q", fs.gotIn[0])
	}
}

func TestSendSnapshotDeltaFromOrigin(t *testing.T) {
	b, fz, fs := newReplBackend(t)
	fz.originValue = "embervm/n1/templates/tmpl1@final"
	var buf bytes.Buffer
	if err := b.SendSnapshotDelta(context.Background(), "sb1", "", "p1", &buf); err != nil {
		t.Fatal(err)
	}
	want := []string{"zfs", "send", "-c", "-i", "embervm/n1/templates/tmpl1@final", "embervm/n1/sandboxes/sb1@p1"}
	if !reflect.DeepEqual(fs.calls[0], want) {
		t.Fatalf("calls[0] = %v, want %v", fs.calls[0], want)
	}
}

func TestSendSnapshotDeltaIncremental(t *testing.T) {
	b, _, fs := newReplBackend(t)
	var buf bytes.Buffer
	if err := b.SendSnapshotDelta(context.Background(), "sb1", "p1", "p2", &buf); err != nil {
		t.Fatal(err)
	}
	want := []string{"zfs", "send", "-c", "-i", "@p1", "embervm/n1/sandboxes/sb1@p2"}
	if !reflect.DeepEqual(fs.calls[0], want) {
		t.Fatalf("calls[0] = %v, want %v", fs.calls[0], want)
	}
}

func TestReceiveSnapshotDeltaChain(t *testing.T) {
	b, fz, fs := newReplBackend(t)
	ctx := context.Background()
	// First stream: dataset absent -> clone-origin receive.
	if err := b.ReceiveSnapshotDelta(ctx, "sb1", "tmpl1", strings.NewReader("D1")); err != nil {
		t.Fatal(err)
	}
	want := []string{"zfs", "receive", "-o", "origin=embervm/n1/templates/tmpl1@final", "embervm/n1/sandboxes/sb1"}
	if !reflect.DeepEqual(fs.calls[0], want) {
		t.Fatalf("first receive = %v, want %v", fs.calls[0], want)
	}
	// Second stream: dataset now exists -> plain -F receive.
	fz.existing["embervm/n1/sandboxes/sb1"] = true
	if err := b.ReceiveSnapshotDelta(ctx, "sb1", "tmpl1", strings.NewReader("D2")); err != nil {
		t.Fatal(err)
	}
	want = []string{"zfs", "receive", "-F", "embervm/n1/sandboxes/sb1"}
	if !reflect.DeepEqual(fs.calls[1], want) {
		t.Fatalf("second receive = %v, want %v", fs.calls[1], want)
	}
	if fs.gotIn[0] != "D1" || fs.gotIn[1] != "D2" {
		t.Fatalf("stream order = %v", fs.gotIn)
	}
}

func TestSetSandboxMountpoint(t *testing.T) {
	b, fz, _ := newReplBackend(t)
	if err := b.SetSandboxMountpoint(context.Background(), "sb1", "/embervm/n1/sandboxes/sb1"); err != nil {
		t.Fatal(err)
	}
	if !fz.findCall("zfs", "set", "mountpoint=/embervm/n1/sandboxes/sb1", "embervm/n1/sandboxes/sb1") {
		t.Fatalf("mountpoint call missing: %v", fz.calls)
	}
	if err := b.SetSandboxMountpoint(context.Background(), "sb1", "relative/path"); err == nil {
		t.Fatal("accepted relative mountpoint")
	}
}

func TestReplicationRejectsBadIDs(t *testing.T) {
	b, _, _ := newReplBackend(t)
	ctx := context.Background()
	var buf bytes.Buffer
	if err := b.SendTemplate(ctx, "../evil", &buf); err == nil {
		t.Error("SendTemplate accepted bad id")
	}
	if err := b.SendSnapshotDelta(ctx, "sb1", "", "../evil", &buf); err == nil {
		t.Error("SendSnapshotDelta accepted bad tag")
	}
	if err := b.ReceiveSnapshotDelta(ctx, "s b", "tmpl1", strings.NewReader("")); err == nil {
		t.Error("ReceiveSnapshotDelta accepted bad sandbox id")
	}
}
