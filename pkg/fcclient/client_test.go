package fcclient

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// recordedReq captures one FC API call for assertions.
type recordedReq struct {
	method string
	path   string
	body   map[string]any
}

// fakeFC serves the Firecracker API on a unix socket and records requests.
func fakeFC(t *testing.T) (sock string, seen *[]recordedReq) {
	t.Helper()
	// Keep the path short (<104 bytes) for the macOS unix-socket limit.
	dir, err := os.MkdirTemp("", "fc")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock = filepath.Join(dir, "fc.sock")

	var reqs []recordedReq
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			_ = json.Unmarshal(raw, &body)
		}
		reqs = append(reqs, recordedReq{r.Method, r.URL.Path, body})
		w.WriteHeader(http.StatusNoContent)
	})

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock, &reqs
}

func TestClientBootSequence(t *testing.T) {
	sock, seen := fakeFC(t)
	c := New(sock)
	ctx := context.Background()

	if err := c.PutMachineConfig(ctx, MachineConfig{VCPUCount: 2, MemSizeMiB: 512}); err != nil {
		t.Fatalf("PutMachineConfig: %v", err)
	}
	if err := c.PutBootSource(ctx, BootSource{KernelImagePath: "/vmlinux", BootArgs: "console=ttyS0"}); err != nil {
		t.Fatalf("PutBootSource: %v", err)
	}
	if err := c.PutDrive(ctx, Drive{DriveID: "rootfs", PathOnHost: "/rootfs.ext4", IsRootDevice: true}); err != nil {
		t.Fatalf("PutDrive rootfs: %v", err)
	}
	if err := c.PutDrive(ctx, Drive{DriveID: "data", PathOnHost: "/data.raw"}); err != nil {
		t.Fatalf("PutDrive data: %v", err)
	}
	if err := c.PutNetworkInterface(ctx, NetworkInterface{IfaceID: "eth0", GuestMAC: "06:00:AC:10:00:02", HostDevName: "tap0"}); err != nil {
		t.Fatalf("PutNetworkInterface: %v", err)
	}
	if err := c.InstanceStart(ctx); err != nil {
		t.Fatalf("InstanceStart: %v", err)
	}

	reqs := *seen
	if len(reqs) != 6 {
		t.Fatalf("recorded %d requests, want 6: %+v", len(reqs), reqs)
	}
	want := []struct {
		method, path string
	}{
		{"PUT", "/machine-config"},
		{"PUT", "/boot-source"},
		{"PUT", "/drives/rootfs"},
		{"PUT", "/drives/data"},
		{"PUT", "/network-interfaces/eth0"},
		{"PUT", "/actions"},
	}
	for i, w := range want {
		if reqs[i].method != w.method || reqs[i].path != w.path {
			t.Errorf("req %d = %s %s, want %s %s", i, reqs[i].method, reqs[i].path, w.method, w.path)
		}
	}
	// Drive wire shape.
	if reqs[2].body["drive_id"] != "rootfs" || reqs[2].body["is_root_device"] != true ||
		reqs[2].body["path_on_host"] != "/rootfs.ext4" {
		t.Errorf("rootfs drive body = %v", reqs[2].body)
	}
	if reqs[2].body["is_read_only"] != false {
		t.Errorf("rootfs is_read_only = %v, want false", reqs[2].body["is_read_only"])
	}
	// InstanceStart action.
	if reqs[5].body["action_type"] != "InstanceStart" {
		t.Errorf("action body = %v", reqs[5].body)
	}
}

func TestClientPauseAndSnapshot(t *testing.T) {
	sock, seen := fakeFC(t)
	c := New(sock)
	ctx := context.Background()

	if err := c.PatchVMState(ctx, "Paused"); err != nil {
		t.Fatalf("PatchVMState: %v", err)
	}
	if err := c.CreateSnapshot(ctx, SnapshotCreate{SnapshotPath: "/snap/snapfile", MemFilePath: "/snap/memfile"}); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	reqs := *seen
	if reqs[0].method != "PATCH" || reqs[0].path != "/vm" || reqs[0].body["state"] != "Paused" {
		t.Errorf("pause req = %+v", reqs[0])
	}
	if reqs[1].method != "PUT" || reqs[1].path != "/snapshot/create" {
		t.Errorf("snapshot req = %+v", reqs[1])
	}
	if reqs[1].body["snapshot_type"] != "Full" || reqs[1].body["mem_file_path"] != "/snap/memfile" {
		t.Errorf("snapshot body = %v", reqs[1].body)
	}
}

func TestClientLoadSnapshotUffdResume(t *testing.T) {
	sock, seen := fakeFC(t)
	c := New(sock)

	err := c.LoadSnapshot(context.Background(), SnapshotLoad{
		SnapshotPath: "/snap/snapfile",
		MemBackend:   MemBackend{BackendType: "Uffd", BackendPath: "/snap/uffd.sock"},
		ResumeVM:     true,
	})
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	reqs := *seen
	if reqs[0].method != "PUT" || reqs[0].path != "/snapshot/load" {
		t.Fatalf("load req = %+v", reqs[0])
	}
	if reqs[0].body["resume_vm"] != true {
		t.Errorf("resume_vm = %v, want true", reqs[0].body["resume_vm"])
	}
	mb, ok := reqs[0].body["mem_backend"].(map[string]any)
	if !ok || mb["backend_type"] != "Uffd" || mb["backend_path"] != "/snap/uffd.sock" {
		t.Errorf("mem_backend = %v", reqs[0].body["mem_backend"])
	}
}

func TestClientPropagatesAPIError(t *testing.T) {
	// A server that always 400s: the client must surface an error.
	dir, _ := os.MkdirTemp("", "fc")
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "fc.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"fault_message":"bad"}`, http.StatusBadRequest)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	if err := New(sock).InstanceStart(context.Background()); err == nil {
		t.Error("InstanceStart against 400 server: want error")
	}
}
