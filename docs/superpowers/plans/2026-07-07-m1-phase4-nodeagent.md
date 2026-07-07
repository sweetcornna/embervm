# M1 Phase 4: Node Agent v0 Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development or
> superpowers:executing-plans. Steps use `- [ ]` checkboxes. This is the first phase that boots
> the template through Firecracker, so its final gate is a new KVM CI job; everything below the
> boot itself is unit-tested off-KVM.

**Goal:** A root daemon that, given a template, creates a microVM sandbox and drives its full
M1 lifecycle — create / stop / pause / resume / snapshot — over a stable Go interface, reusing
the M0 Firecracker/netns/uffd machinery.

**Architecture:** Five small packages behind one façade.
- `pkg/fcclient` — minimal Firecracker API client over the unix socket (the exact PUT/PATCH
  sequence the M0 scripts use: machine-config, boot-source, drives, network-interfaces,
  actions InstanceStart, vm Paused, snapshot/create Full, snapshot/load Uffd resume_vm).
- `pkg/netns` — a pre-created pool of `ember<N>` namespaces (wraps scripts/setup-network.sh),
  plus a `DialContext` that dials a guest IP from inside a namespace (setns on a locked thread).
- `pkg/lifecycle` — the M1 state machine (pure), the single place transitions are validated.
- `pkg/nodeapi` — the `Agent` interface + an HTTP-over-UDS server/client wiring (in-proc for
  `embervm dev`, split for a standalone node).
- `cmd/nodeagent` — wires storage + fcclient + netns + template into a concrete Agent and
  serves it; replaces the M0 placeholder.

**Tech Stack:** Go 1.24, golang.org/x/sys/unix (setns), net/http over UDS. Reuses pkg/storage
(P3), pkg/template (P2), pkg/guestapi (P1), pkg/uffd + cmd/uffd-handler (M0).

## Global Constraints
- Guest is always 172.16.0.2 inside its own netns; isolation is per-sandbox netns + NAT, so
  the host MUST reach guestd by dialing inside the namespace (docs/zh/02 §4, master-spec D10).
- Firecracker under jailer by default: per-VM uid/gid, `--new-pid-ns`, `--cgroup-version 2`,
  `--netns /var/run/netns/ember<N>` (matches scripts/fc-boot.sh --jailer path).
- io_engine=Sync; resume uses the M0 uffd-handler (default mode uffd-prefetch).
- Data disk (data.raw) is a second drive, attached at boot AND re-attached on resume, never in
  the resume critical path (O(1) — docs/zh/02 §1).
- netns pool default size 24 (covers the 20-concurrency exit criterion); creating >~500 is
  where creation rate collapses (docs/zh/04 §5) — irrelevant at 24.
- All host-side unix socket paths stay <104 bytes (macOS test limit; short temp dirs).

## Design decisions
**Agent interface** (`pkg/nodeapi`, what Phase 5's apiserver consumes):
```go
type CreateSandboxRequest struct {
	SandboxID, TemplateID string
	VCPUs, MemoryMiB, DataDiskGiB int
}
type SandboxStatus struct {
	SandboxID string
	State     string // lifecycle state name
	GuestAddr string // e.g. "172.16.0.2:7777" (reachable via the netns dialer)
	Netns     string
}
type Agent interface {
	BuildTemplate(ctx, templateID, image string) error
	CreateSandbox(ctx, CreateSandboxRequest) (SandboxStatus, error)
	StopSandbox(ctx, sandboxID string) error
	PauseSandbox(ctx, sandboxID string) error
	ResumeSandbox(ctx, sandboxID string) (SandboxStatus, error)
	SnapshotSandbox(ctx, sandboxID, tag string) (string, error)
	GuestClient(sandboxID string) (*guestapi.Client, error) // netns-dialing client
	Status(ctx, sandboxID string) (SandboxStatus, error)
}
```

**Lifecycle** (`pkg/lifecycle`) states + legal transitions (master-spec D8):
`PENDING→STARTING→RUNNING⇄(PAUSING→PAUSED_HOT→RESUMING→RUNNING)→STOPPING→STOPPED`, `FAILED`
from any active state. `Machine` guards `Transition(from,to)`; unknown transitions error.

**Pause/resume mechanics** (reuse M0 exactly):
- pause: `PATCH /vm {"state":"Paused"}` → `PUT /snapshot/create {snapshot_type:"Full",
  snapshot_path:<dir>/snapfile, mem_file_path:<dir>/memfile}` → kill FC → storage.Snapshot.
- resume: start uffd-handler (`--socket <dir>/uffd.sock --memfile <dir>/memfile
  --mode=prefetch`), start a NEW FC in the same netns, `PUT /snapshot/load {snapshot_path,
  mem_backend:{backend_type:"Uffd",backend_path:<uffd.sock>}, resume_vm:true}`. Drives/net are
  restored from the snapshot; rootfs.ext4 + data.raw are at their dataset paths (unchanged).

## Tasks (each ends testable; TDD where the surface is reachable off-KVM)

### Task 1: pkg/fcclient — FC API client
Files: `pkg/fcclient/client.go`, `pkg/fcclient/client_test.go`.
`New(socketPath)`; methods PutMachineConfig, PutBootSource, PutDrive, PutNetworkInterface,
InstanceStart, PatchVMState, CreateSnapshot, LoadSnapshot — each a typed struct → PUT/PATCH.
Test against an `httptest`-style fake FC: a real `net.Listener` on a unix socket with an
`http.ServeMux` asserting method+path+decoded JSON body, returning 204. Verifies exact wire
shapes (drive_id/path_on_host/is_root_device, mem_backend Uffd, resume_vm, etc.). No KVM.

### Task 2: pkg/netns — pool + netns dialer
Files: `pkg/netns/pool_linux.go` (+`pool_stub.go` !linux), `pkg/netns/dial_linux.go`
(+stub), `pkg/netns/pool_test.go`.
- `Pool` wraps setup/teardown-network.sh: `Acquire()` → free `ember<N>`, `Release(n)`.
  Pool operations shell through an injectable runner so acquire/release bookkeeping is
  unit-tested with a fake (real netns creation is linux+root, exercised in KVM CI).
- `DialContext(ctx, network, addr)` for id N: `runtime.LockOSThread`, open
  `/proc/self/ns/net` (save), `setns(open(/var/run/netns/ember<N>))`, dial, restore saved ns,
  unlock. Returns the connected conn. This is what `guestapi.Client`'s http.Client uses.
- Pure bookkeeping tests off-KVM; setns path is linux-gated and covered by the KVM job.

### Task 3: pkg/lifecycle — state machine
Files: `pkg/lifecycle/machine.go`, `pkg/lifecycle/machine_test.go`. Pure; exhaustive
transition table test (legal transitions pass, every illegal one errors). No external deps.

### Task 4: pkg/nodeapi — interface + HTTP wiring
Files: `pkg/nodeapi/agent.go` (types+interface), `pkg/nodeapi/http.go` (server+client over
UDS), `pkg/nodeapi/http_test.go`. Test: a mock Agent behind the HTTP server, exercised through
the HTTP client, asserting request/response round-trips for every method. No KVM.

### Task 5: cmd/nodeagent — concrete Agent + daemon
Files: `cmd/nodeagent/main.go` (replace placeholder), `cmd/nodeagent/agent_linux.go`
(concrete impl: storage+fcclient+netns+template+uffd-handler), `agent_stub.go` (!linux →
"linux only"). Concrete impl is integration-tested by the KVM job (Task 7), not unit tests.
Flags: `--socket /run/embervm/nodeagent.sock`, `--pool embervm`, `--netns-pool 24`,
`--assets <dir>` (kernel + fc binary + uffd-handler), `--restore-mode prefetch`.

### Task 6: test/integration/nodeagent-smoke.sh — KVM lifecycle smoke
Files: `test/integration/nodeagent-smoke.sh`. Builds a tiny template (busybox tar → ext4 via
pkg/template through a small helper, OR reuse the M0 rootfs with guestd injected), then via the
Agent (in-proc test harness binary `cmd/nodeagent selftest` or a Go test with build tag
`kvm`): create → guestd /healthz seq=1 (dialed through netns) → exec `echo` → pause → resume →
/healthz seq=2 (SAME process survived) → stop. Asserts seq continuity across restore.

### Task 7: .github/workflows/integration-kvm.yml — add nodeagent-smoke
Add a job (or matrix leg) running nodeagent-smoke.sh under the same KVM setup as the existing
smoke job (udev bounded-wait, unprivileged_userfaultfd, fetch-assets, build). Keep the M0
`smoke` job untouched. Green on both FC versions.

### Task 8: gate + commit + push + mark #14
`make lint && make test && GOOS=linux go build ./...`; push; watch integration-kvm
nodeagent-smoke green on v1.16.1 + v1.15.1.

## Verification
- Off-KVM unit tests: fcclient wire shapes, netns pool bookkeeping, lifecycle table, nodeapi
  round-trips — all green on macOS + linux.
- KVM CI: nodeagent-smoke boots a real template microVM, dials guestd through the netns, and
  proves pause→resume keeps the same guest process alive (seq 1→2). Both FC versions.
