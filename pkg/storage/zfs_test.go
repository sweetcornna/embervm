package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeZFS records every command and answers probes from a scripted state.
type fakeZFS struct {
	calls       [][]string
	existing    map[string]bool // datasets/snapshots that "exist"
	mountRoot   string          // mountpoints returned as <mountRoot>/<ds-tail>
	originValue string          // answer for `zfs get ... origin`
}

func (f *fakeZFS) run(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	switch {
	case name == "zfs" && len(args) >= 2 && args[0] == "list":
		ds := args[len(args)-1]
		if f.existing[ds] {
			return ds, nil
		}
		return "dataset does not exist", os.ErrNotExist
	case name == "zfs" && len(args) >= 2 && args[0] == "get" && args[len(args)-2] == "origin":
		return f.originValue, nil
	case name == "zfs" && len(args) >= 2 && args[0] == "get" && args[len(args)-2] == "mountpoint":
		ds := args[len(args)-1]
		mp := filepath.Join(f.mountRoot, strings.ReplaceAll(ds, "/", "_"))
		_ = os.MkdirAll(mp, 0o755)
		return mp, nil
	case name == "zfs" && len(args) >= 1 && args[0] == "create":
		f.existing[args[len(args)-1]] = true
		return "", nil
	case name == "zfs" && len(args) >= 1 && args[0] == "snapshot":
		f.existing[args[len(args)-1]] = true
		return "", nil
	case name == "zfs" && len(args) >= 1 && args[0] == "clone":
		f.existing[args[len(args)-1]] = true
		return "", nil
	case name == "zfs" && len(args) >= 1 && args[0] == "destroy":
		return "", nil
	}
	return "", nil
}

func newFakeBackend(t *testing.T) (*ZFSBackend, *fakeZFS) {
	t.Helper()
	f := &fakeZFS{existing: map[string]bool{}, mountRoot: t.TempDir()}
	return &ZFSBackend{pool: "embervm", run: f.run}, f
}

// findCall returns true if some recorded call has the given prefix words.
func (f *fakeZFS) findCall(prefix ...string) bool {
	for _, c := range f.calls {
		if len(c) < len(prefix) {
			continue
		}
		match := true
		for i, w := range prefix {
			if c[i] != w {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func TestZFSEnsureTemplateCreatesAndSnapshots(t *testing.T) {
	be, f := newFakeBackend(t)
	src := writeTemplateSrc(t, "img")

	if err := be.EnsureTemplate(context.Background(), "tpl1", src); err != nil {
		t.Fatalf("EnsureTemplate: %v", err)
	}
	if !f.findCall("zfs", "create", "-p", "-o", "recordsize=16k") {
		t.Errorf("no create with recordsize=16k; calls=%v", f.calls)
	}
	if !f.findCall("zfs", "snapshot", "embervm/templates/tpl1@final") {
		t.Errorf("no @final snapshot; calls=%v", f.calls)
	}
	// primarycache + compression props present.
	joined := ""
	for _, c := range f.calls {
		joined += strings.Join(c, " ") + "\n"
	}
	for _, want := range []string{"primarycache=metadata", "compression=lz4"} {
		if !strings.Contains(joined, want) {
			t.Errorf("create missing prop %q", want)
		}
	}
}

func TestZFSEnsureTemplateIdempotent(t *testing.T) {
	be, f := newFakeBackend(t)
	f.existing["embervm/templates/tpl1@final"] = true

	if err := be.EnsureTemplate(context.Background(), "tpl1", "unused"); err != nil {
		t.Fatalf("EnsureTemplate idempotent: %v", err)
	}
	if f.findCall("zfs", "create") || f.findCall("zfs", "snapshot") {
		t.Errorf("idempotent EnsureTemplate re-created/snapshotted; calls=%v", f.calls)
	}
}

func TestZFSCloneSandbox(t *testing.T) {
	be, f := newFakeBackend(t)
	paths, err := be.CloneSandbox(context.Background(), "sbx1", "tpl1", 15)
	if err != nil {
		t.Fatalf("CloneSandbox: %v", err)
	}
	if !f.findCall("zfs", "clone", "embervm/templates/tpl1@final", "embervm/sandboxes/sbx1") {
		t.Errorf("no clone from @final; calls=%v", f.calls)
	}
	// The sandboxes container dataset must be ensured before the clone
	// (zfs clone will not create parents) — regression for the real-ZFS
	// "parent does not exist" failure.
	if !f.findCall("zfs", "create", "-p", "embervm/sandboxes") {
		t.Errorf("sandboxes parent not ensured before clone; calls=%v", f.calls)
	}
	fi, err := os.Stat(paths.DataRaw)
	if err != nil {
		t.Fatalf("stat data.raw: %v", err)
	}
	if fi.Size() != 15<<30 {
		t.Errorf("data.raw size = %d, want %d", fi.Size(), int64(15)<<30)
	}
}

func TestZFSSnapshotAndDestroy(t *testing.T) {
	be, f := newFakeBackend(t)
	snap, err := be.Snapshot(context.Background(), "sbx1", "p1")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap != "embervm/sandboxes/sbx1@p1" {
		t.Errorf("snapshot id = %q", snap)
	}
	if err := be.DestroySandbox(context.Background(), "sbx1"); err != nil {
		t.Fatalf("DestroySandbox: %v", err)
	}
	if !f.findCall("zfs", "destroy", "-r", "embervm/sandboxes/sbx1") {
		t.Errorf("no recursive destroy; calls=%v", f.calls)
	}
}

func TestZFSRejectsBadIDs(t *testing.T) {
	be, _ := newFakeBackend(t)
	if _, err := be.Snapshot(context.Background(), "sbx", "bad tag"); err == nil {
		t.Error("Snapshot with bad tag: want error")
	}
	if _, err := be.CloneSandbox(context.Background(), "ok", "bad/tpl", 1); err == nil {
		t.Error("CloneSandbox with bad template id: want error")
	}
}
