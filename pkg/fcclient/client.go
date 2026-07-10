// Package fcclient is a minimal Firecracker API client over the VMM's unix
// socket. It implements exactly the request sequence the M0 shell scripts
// use (scripts/fc-boot.sh, fc-snapshot.sh, fc-restore.sh): configure →
// InstanceStart for boot; PATCH /vm Paused + PUT /snapshot/create Full for
// pause; PUT /snapshot/load with a Uffd backend + resume_vm for hot restore.
//
// It speaks HTTP/1.1 over the AF_UNIX socket; the "host" in the URL is
// ignored by Firecracker. Every mutating call expects 204 No Content.
package fcclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
)

// Client talks to one Firecracker process via its API socket.
type Client struct {
	hc   *http.Client
	sock string
}

// New returns a client bound to the Firecracker API socket at socketPath.
func New(socketPath string) *Client {
	return &Client{
		sock: socketPath,
		hc: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

// MachineConfig is the body of PUT /machine-config.
type MachineConfig struct {
	VCPUCount  int `json:"vcpu_count"`
	MemSizeMiB int `json:"mem_size_mib"`
	// TrackDirtyPages arms KVM dirty-page logging so pauses can take Diff
	// snapshots (M2 layered snapshots).
	TrackDirtyPages bool `json:"track_dirty_pages,omitempty"`
}

// BootSource is the body of PUT /boot-source.
type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

// Drive is the body of PUT /drives/{id}.
type Drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

// NetworkInterface is the body of PUT /network-interfaces/{id}.
type NetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	GuestMAC    string `json:"guest_mac"`
	HostDevName string `json:"host_dev_name"`
}

// Balloon is the body of PUT /balloon (pre-boot attach).
type Balloon struct {
	AmountMib    int  `json:"amount_mib"`
	DeflateOnOom bool `json:"deflate_on_oom"`
}

// MemoryHotplug is the body of PUT /hotplug/memory (pre-boot only): it
// attaches a virtio-mem device whose region starts fully unplugged.
// TotalSizeMiB must be a multiple of SlotSizeMiB; zero Block/Slot sizes let
// Firecracker default them (2 MiB blocks, 128 MiB slots).
type MemoryHotplug struct {
	TotalSizeMiB int `json:"total_size_mib"`
	BlockSizeMiB int `json:"block_size_mib,omitempty"`
	SlotSizeMiB  int `json:"slot_size_mib,omitempty"`
}

// MemoryHotplugStatus is the response of GET /hotplug/memory. The guest
// driver plugs/unplugs asynchronously: PluggedSizeMiB converges toward
// RequestedSizeMiB only with guest cooperation, so callers poll.
type MemoryHotplugStatus struct {
	TotalSizeMiB     int `json:"total_size_mib"`
	BlockSizeMiB     int `json:"block_size_mib"`
	SlotSizeMiB      int `json:"slot_size_mib"`
	PluggedSizeMiB   int `json:"plugged_size_mib"`
	RequestedSizeMiB int `json:"requested_size_mib"`
}

// SnapshotCreate is the body of PUT /snapshot/create. SnapshotType defaults
// to "Full" when empty.
type SnapshotCreate struct {
	SnapshotType string `json:"snapshot_type"`
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
}

// MemBackend selects how restored guest memory is served.
type MemBackend struct {
	BackendType string `json:"backend_type"` // "File" or "Uffd"
	BackendPath string `json:"backend_path"`
}

// SnapshotLoad is the body of PUT /snapshot/load. TrackDirtyPages keeps
// dirty-page tracking armed across restores so the NEXT pause can be a Diff
// snapshot; ClockRealtime adjusts the guest realtime clock on restore
// (docs/zh/02 §4 校时).
type SnapshotLoad struct {
	SnapshotPath    string     `json:"snapshot_path"`
	MemBackend      MemBackend `json:"mem_backend"`
	ResumeVM        bool       `json:"resume_vm"`
	TrackDirtyPages bool       `json:"track_dirty_pages,omitempty"`
	ClockRealtime   bool       `json:"clock_realtime,omitempty"`
}

func (c *Client) PutMachineConfig(ctx context.Context, m MachineConfig) error {
	return c.do(ctx, http.MethodPut, "/machine-config", m)
}

func (c *Client) PutBootSource(ctx context.Context, b BootSource) error {
	return c.do(ctx, http.MethodPut, "/boot-source", b)
}

func (c *Client) PutDrive(ctx context.Context, d Drive) error {
	return c.do(ctx, http.MethodPut, "/drives/"+d.DriveID, d)
}

func (c *Client) PutNetworkInterface(ctx context.Context, n NetworkInterface) error {
	return c.do(ctx, http.MethodPut, "/network-interfaces/"+n.IfaceID, n)
}

// InstanceStart issues the boot action.
func (c *Client) InstanceStart(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "/actions", map[string]string{"action_type": "InstanceStart"})
}

// PutBalloon attaches a balloon device (pre-boot only).
func (c *Client) PutBalloon(ctx context.Context, b Balloon) error {
	return c.do(ctx, http.MethodPut, "/balloon", b)
}

// PatchBalloon retargets the balloon of a running VM (memory reclaim).
func (c *Client) PatchBalloon(ctx context.Context, amountMib int) error {
	return c.do(ctx, http.MethodPatch, "/balloon", map[string]int{"amount_mib": amountMib})
}

// PutMemoryHotplug attaches a virtio-mem hotplug region (pre-boot only; on
// snapshot-restored VMs the region comes from the snapshot itself).
func (c *Client) PutMemoryHotplug(ctx context.Context, m MemoryHotplug) error {
	return c.do(ctx, http.MethodPut, "/hotplug/memory", m)
}

// PatchMemoryHotplug retargets the plugged size of the hotplug region on a
// running VM. The guest driver converges asynchronously — poll
// GetMemoryHotplug until PluggedSizeMiB matches.
func (c *Client) PatchMemoryHotplug(ctx context.Context, requestedMiB int) error {
	return c.do(ctx, http.MethodPatch, "/hotplug/memory", map[string]int{"requested_size_mib": requestedMiB})
}

// GetMemoryHotplug reports the hotplug region's plug state.
func (c *Client) GetMemoryHotplug(ctx context.Context) (MemoryHotplugStatus, error) {
	var st MemoryHotplugStatus
	err := c.get(ctx, "/hotplug/memory", &st)
	return st, err
}

// PatchVMState transitions a running VM, e.g. state="Paused" or "Resumed".
func (c *Client) PatchVMState(ctx context.Context, state string) error {
	return c.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": state})
}

// CreateSnapshot writes a Full snapshot of the paused VM.
func (c *Client) CreateSnapshot(ctx context.Context, s SnapshotCreate) error {
	if s.SnapshotType == "" {
		s.SnapshotType = "Full"
	}
	return c.do(ctx, http.MethodPut, "/snapshot/create", s)
}

// LoadSnapshot restores (and optionally resumes) a VM from a snapshot.
func (c *Client) LoadSnapshot(ctx context.Context, s SnapshotLoad) error {
	return c.do(ctx, http.MethodPut, "/snapshot/load", s)
}

// get issues a GET and decodes the 200 JSON body into out.
func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("firecracker GET %s: HTTP %d: %s", path, resp.StatusCode, raw)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) do(ctx context.Context, method, path string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("firecracker %s %s: HTTP %d: %s", method, path, resp.StatusCode, raw)
	}
	return nil
}
