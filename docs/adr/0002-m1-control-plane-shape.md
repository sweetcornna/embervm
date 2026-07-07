# ADR-0002: M1 single-node MVP shape

- Status: Accepted
- Date: 2026-07-08

## 中文摘要

M1（单机 MVP）确立 EmberVM 的控制面/数据面骨架并落码。**REST 面**（`pkg/controlplane`，Gin + PostgreSQL）：模板 CRUD、沙箱 CRUD + pause/resume/snapshot/kill、以及 guest exec/文件代理；Bearer token 鉴权 + 每 token `max_sandboxes` 配额；PostgreSQL 为唯一权威状态（不自研分布式状态同步），Redis 与 Gateway 推迟到 M4。**控制面↔节点契约**（`pkg/nodeapi.Agent`）：一个 Go 接口，`embervm dev` 直接进程内装配，独立节点走 unix socket 上的 HTTP/JSON 镜像（gRPC 推迟到 M4）。**guestd**（PID 1）：挂载伪文件系统后 fork 出服务子进程并回收孤儿；`/healthz` 每进程单调序号证明恢复是同一进程；exec 进程组超时终止；文件读写。**模板构建**：go-containerregistry 无守护进程拉取+展平 Docker/OCI 镜像 → 安全 tar 解包（filepath-securejoin 防 tar-slip）→ 注入 guestd + image.json → `mkfs.ext4 -d`。**存储**：`<pool>/templates/<id>@final` 克隆到 `<pool>/sandboxes/<id>`，数据盘为 dataset 上的稀疏 raw 文件（绝不用 zvol），`recordsize=16k / primarycache=metadata / compression=lz4`；另有 plain-dir 后端供 dev/测试。**生命周期**：`PENDING→STARTING→RUNNING⇄PAUSING→PAUSED_HOT→RESUMING`，`→STOPPING→STOPPED`，任意活动态可 `FAILED`；WARM/COLD/RECYCLED 保留给 M3。**热恢复路径**：新 FC 进程在原 netns 内 `PUT /snapshot/load`（Uffd 后端由 M0 uffd-handler 服务，默认 prefetch），rootfs.ext4 + data.raw 停在 dataset 路径 O(1) 挂载，数据盘不进恢复关键路径。**关键推迟决策**：jailer 加固（chroot/pivot_root/独立 uid-gid/seccomp）推迟到 M4——jailer 把 rootfs 拷进 chroot，会破坏 ZFS CoW 克隆，也会破坏快照恢复所依赖的稳定驱动路径；M1 以 per-sandbox netns + best-effort cgroup v2 隔离。模板在 M1 为节点全局（无 owner 列），沙箱按 owner 严格鉴权。鉴权 fail-closed：无 `--tokens-file` 且未显式 `--insecure-dev-token` 则拒绝启动。退出标准 CI 相对测量（嵌套虚拟化），裸金属复测为跟踪项。

## Context

M0 proved the primitives (Firecracker boot, snapshot→uffd resume, ZFS clone) work and measured their latency. M1's job is to turn those primitives into a usable single-node product: a full lifecycle over a REST API, a template builder that turns Docker images into microVMs, and a one-command `embervm dev`. The source documents (docs/zh/02–05) specify *what* M1 delivers and its exit criteria, but deliberately leave the concrete API surface, wire contracts, storage naming, and DB schema as implementation decisions. This ADR records those decisions (D1–D10) and the scoping calls made while implementing them.

## Decision

### D1 — REST API v0 (`pkg/controlplane`, Gin)

```
POST/GET/DELETE  /v0/templates[/{id}]
POST/GET         /v0/sandboxes[/{id}]      (create validates quota)
POST             /v0/sandboxes/{id}/{pause,resume,snapshot}
DELETE           /v0/sandboxes/{id}        (kill)
POST             /v0/sandboxes/{id}/exec
GET/PUT          /v0/sandboxes/{id}/files?path=…
GET              /healthz                  (unauthenticated)
```
`Authorization: Bearer <token>`; each token carries `max_sandboxes`. Errors are `{"error":…}`.
v0.x may break, with a CHANGELOG entry (docs/zh/05 §4).

### D2 — Control-plane ↔ node contract (`pkg/nodeapi.Agent`)

One Go interface (BuildTemplate, CreateSandbox, Stop/Pause/Resume/Snapshot, Status, and guest
Exec/Health/Read/WriteFile). `embervm dev` wires the concrete agent in-process; a standalone
node serves it as HTTP/JSON over `unix:///run/embervm/nodeagent.sock` and the API server dials
it with a `Client` implementing the same interface. gRPC is deferred to M4 — HTTP/JSON over a
socket is enough for one node and needs no second toolchain.

### D3 — guestd is PID 1 (`cmd/guestd`)

Template rootfs boots `init=/usr/local/bin/guestd`. As PID 1 it mounts proc/sys/dev/tmp/run,
then forks a server child and reaps orphans, respawning the child if it dies. `/healthz`
returns a per-process monotone `seq` (restore-continuity proof, same semantics as
test/probe/server); `exec` buffers output and kills the process group on timeout; `files`
reads/writes absolute paths.

### D4 — Template pipeline (`pkg/template`)

go-containerregistry (`crane`) pulls and flattens an image daemonlessly (production nodes run
no Docker daemon), the tar is extracted with `filepath-securejoin` scoping (tar-slip defense),
guestd + `/etc/embervm/image.json` (ENV/ENTRYPOINT/CMD/WORKDIR) + net identity are injected,
and `mkfs.ext4 -d` writes the rootfs (no root, no mount). squashfs+overlayfs base-sharing is an
M2 optimization; ext4 satisfies the M1 charter.

### D5 — ZFS layout (`pkg/storage`)

`<pool>/templates/<id>` (rootfs.ext4, snapshot `@final`) is cloned to `<pool>/sandboxes/<id>`;
the data disk is a sparse `data.raw` on the clone. Properties `recordsize=16k`,
`primarycache=metadata`, `compression=lz4`. Never a zvol (docs/zh/04 §1). A plain-directory
backend mirrors the interface for dev and unit tests.

### D6 — PostgreSQL schema (`pkg/controlplane`)

`templates`, `sandboxes`, `sandbox_events` (append-only transition log). PostgreSQL is the sole
source of truth (docs/zh/04 §6). Redis is not used in M1.

### D7 — `embervm dev` (`cmd/embervm`)

`dev` runs migrations + REST server + in-proc agent in one root process (docs/zh/05 §6). No
socket hop; same `controlplane.Server`.

### D8 — Lifecycle machine (`pkg/lifecycle`)

`PENDING→STARTING→RUNNING⇄(PAUSING→PAUSED_HOT→RESUMING→RUNNING)→STOPPING→STOPPED`, `FAILED`
from any active state, validated in one place; every transition appends a `sandbox_events` row.
WARM/COLD/RECYCLED are reserved for M3 archive tiers.

### D9 — Hot-resume path (`pkg/nodeagent`)

A fresh Firecracker process starts in the sandbox's existing netns; the M0 `uffd-handler`
(default `prefetch`) serves memory; `PUT /snapshot/load` with a `Uffd` backend and
`resume_vm:true` restores it. rootfs.ext4 + data.raw stay at their dataset paths, so the data
disk is an O(1) re-attach and never enters the resume critical path (docs/zh/02 §1).

### D10 — Node agent internals

A root daemon with a pre-created netns pool (`ember<N>`, default 24 — covers the 20-concurrency
exit criterion). Every guest shares 172.16.0.2; isolation is per-namespace + NAT, so the host
reaches guestd through a setns dialer confined to a throwaway locked thread. Best-effort cgroup
v2 slice per sandbox.

## Scoping decisions (what M1 deliberately does NOT do)

- **Jailer / host hardening → M4.** The M0 `--jailer` path copies rootfs into the chroot, which
  would defeat the ZFS CoW clone AND break snapshot/resume (a Full snapshot records each drive's
  absolute host path; those paths must stay stable across the FC process restart). Full jailer
  hardening — chroot/pivot_root, per-VM uid/gid, `--new-pid-ns`, seccomp — is the 宿主加固 line
  docs/zh/03 assigns to M4. M1 isolates with per-sandbox netns + best-effort cgroup v2.
- **Templates are node-global in M1.** The `templates` table has no owner column; any tenant may
  reference or delete a template. Sandboxes, by contrast, are strictly owner-scoped (a caller
  gets 404 on another tenant's sandbox, on every verb). Per-tenant template ACLs are future work.
- **Auth fails closed.** The server refuses to start without `--tokens-file` unless
  `--insecure-dev-token` is explicitly passed; there is no silent default-open token.
- **Redis / Gateway / gRPC / multi-node → M4.**

## Consequences

- One Go binary set, one interface, two wirings (in-proc / socket): the control plane cannot
  tell `embervm dev` from a split deployment, so tests exercise the real code both ways.
- Resume correctness hinges on stable drive paths, which is why jailer is deferred rather than
  half-implemented.
- Exit-criteria numbers (20 concurrent; hot resume <1s with a 15 GiB data disk) are measured
  CI-relative on nested virtualization per ADR-0001; bare-metal re-measurement is a tracked
  follow-up, not an M1 gate.
