# M1 Phase 3: pkg/storage backends (plain dir + ZFS clone) Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development or
> superpowers:executing-plans. Steps use `- [ ]` checkboxes.

**Goal:** A `storage.Backend` abstraction the node agent uses to turn a built rootfs into an
immutable template and to clone per-sandbox writable storage (rootfs clone + sparse data disk),
with a ZFS implementation for production and a plain-directory implementation for dev/tests.

**Architecture:** One interface, two impls. `ZFSBackend` shells out to `zfs`/`zpool` through an
injectable runner, so command construction is unit-tested on any OS and a real loop-pool
integration test runs in CI. `PlainBackend` uses ordinary filesystem ops (copy + sparse
truncate) and works everywhere. Layout is master-spec D5: `<pool>/templates/<id>@final` cloned
to `<pool>/sandboxes/<id>`, data disk = sparse `data.raw`, props `recordsize=16k
primarycache=metadata compression=lz4`.

**Tech Stack:** Go 1.24 stdlib + os/exec; no new deps.

## Global Constraints
- Data disk is a raw file on a dataset, never a zvol (docs/zh/04 §1).
- data.raw is sparse and created once at clone time; it is NOT in the resume critical path
  (docs/zh/02 §1) — the 15GiB exit-criterion disk must not bloat snapshots.
- ZFS ops must be idempotent where the node agent may retry (EnsureTemplate, DestroySandbox).
- CI provisions a loop-file zpool exactly like test/bench/zfs-compare.sh `ensure_zfs`.

---

### Task 1: Backend interface + SandboxPaths
**Files:** Create `pkg/storage/backend.go`.
```go
type SandboxPaths struct { Dir, RootfsExt4, DataRaw string }
type Backend interface {
	EnsureTemplate(ctx context.Context, templateID, rootfsExt4Src string) error
	CloneSandbox(ctx context.Context, sandboxID, templateID string, dataDiskGiB int) (SandboxPaths, error)
	Paths(sandboxID string) SandboxPaths
	Snapshot(ctx context.Context, sandboxID, tag string) (string, error)
	DestroySandbox(ctx context.Context, sandboxID string) error
}
```
IDs validated (`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`) to keep them safe as dataset/path components.

### Task 2: PlainBackend (dev/tests)
**Files:** Create `pkg/storage/plain.go`, `pkg/storage/plain_test.go`.
- `NewPlainBackend(root string)`; templates at `<root>/templates/<id>/rootfs.ext4`, sandboxes at
  `<root>/sandboxes/<id>/{rootfs.ext4,data.raw}`.
- EnsureTemplate: mkdir + copy src → rootfs.ext4 (idempotent overwrite).
- CloneSandbox: mkdir, copy template rootfs → sandbox rootfs.ext4, `os.Truncate` a sparse
  data.raw of `dataDiskGiB<<30`. Reject unknown template.
- Snapshot: copy dataset dir to `<root>/sandboxes/<id>/.snap/<tag>`; return that path.
- DestroySandbox: RemoveAll.
- Tests: full roundtrip (ensure→clone→verify rootfs bytes match + data.raw is the right size and
  sparse), unknown template errors, bad id errors, 15GiB sparse data.raw creation is instant and
  on-disk blocks ≈ 0.

### Task 3: ZFSBackend (injectable runner)
**Files:** Create `pkg/storage/zfs.go`, `pkg/storage/zfs_test.go`.
- `NewZFSBackend(pool string)` wires the real exec runner; tests inject a fake recording runner
  returning canned `zfs get mountpoint` output.
- EnsureTemplate: `zfs list <ds>` probe → if absent `zfs create -p -o recordsize=16k -o
  primarycache=metadata -o compression=lz4 <pool>/templates/<id>`, copy src into mountpoint,
  `zfs snapshot <pool>/templates/<id>@final`.
- CloneSandbox: `zfs clone <pool>/templates/<tid>@final <pool>/sandboxes/<sid>`, then sparse
  `truncate` data.raw in the clone mountpoint.
- Snapshot: `zfs snapshot <pool>/sandboxes/<sid>@<tag>` → returns `<pool>/sandboxes/<sid>@<tag>`.
- DestroySandbox: `zfs destroy -r <pool>/sandboxes/<sid>` (idempotent: ignore "does not exist").
- Tests assert exact argv sequences + returned paths via the fake runner.

### Task 4: ZFS loop-pool integration test (CI)
**Files:** Create `pkg/storage/zfs_integration_test.go` (`//go:build linux`, gated
`EMBERVM_ZFS_TESTS=1`). Reuses zfs-compare.sh's ensure/loop-pool idea: create a loop file,
`zpool create embervm-test`, run a full ensure→clone→snapshot→destroy cycle asserting the files
exist, teardown the pool. Skips (not fails) if zpool userland is missing — non-blocking, matching
M0 ZFS policy.
**Files:** Modify `.github/workflows/lint-unit.yml` — add a `storage-zfs` job (or step) that
installs zfsutils-linux, creates the loop pool, runs `EMBERVM_ZFS_TESTS=1 go test
./pkg/storage/ -run TestZFSIntegration -v`, `continue-on-error: true` (matches bench zfs job).

### Task 5: gate + commit + push
- [ ] `make lint && make test && GOOS=linux go build ./...`; commit `feat(storage): ZFS + plain
  backends for template/sandbox datasets`; push; watch lint-unit + integration-kvm; mark #13 done.

## Verification
- Plain backend roundtrip tests green on macOS + linux.
- ZFS command-construction tests green everywhere (fake runner).
- CI storage-zfs job green (or cleanly skipped) on a real loop pool.
