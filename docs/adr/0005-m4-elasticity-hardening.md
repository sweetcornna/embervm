# ADR-0005: M4 elasticity & production hardening

- Status: Accepted
- Date: 2026-07-09

## 中文摘要

M4 把单节点系统变成 3 节点集群并完成宿主加固,退出门禁是"杀任一 worker,其上沙箱可异机恢复"。**节点注册与心跳**(D1):`nodes` 表 + 静态成员配置(apiserver `--nodes id=socket,...`),调度器按 tick 轮询节点 `Healthz`(容量/占用/沙箱数),连续 miss 达阈值即驱逐——节点标 down、其 RUNNING 沙箱标 FAILED(最后一次写穿快照仍可恢复),暂停中的沙箱不动(本就活在 L1/L2);恢复是按需恢复(resume 时重新放置),不做迁移风暴。**放置**(D2):sandboxes.node_id 即路由表(不引 Redis);create/resume 先粘住上一节点(L0 缓存局部性),否则按剩余内存 bin-pack;预算按内存 × MemOvercommit、vCPU × 核数 × CPUOvercommit(默认 3x,vCPU 是分时线程)双约束;PostgreSQL 是占用的唯一真相。**jailer 加固**(D3):每个 FC 进程 chroot + 独立 uid/gid + netns + 默认 seccomp;dataset 与 snap 目录 bind 进 chroot,快照里的盘路径变 chroot 相对(/data/...)——每个节点、每个沙箱完全一致,由此淘汰 M3 的挂载点钉死(jailed 恢复不再钉挂载点,同宿主多子树也不冲突;unjailed dev 路径保留钉死),并解锁 fast-create。**golden fast-create**(D4):模板构建时 boot 一次 golden 沙箱、走正常 chunked pause 后"停车"(运行时资源归还,dataset 快照与 staging 留作模板热像);几何匹配的 CreateSandbox 走 `zfs clone golden@p1` + 热恢复内存像,跳过内核启动(CI 实测 P50 <500ms);几何不匹配或无 golden 回退冷启动。**balloon 超售**(D5):boot 带 virtio-balloon(deflate_on_oom),`SetBalloon` 动词可回收空闲内存;balloon 辅助 pause(充气→静置→快照,零页跳过落掉释放页);50 并发/节点 e2e 验证(逻辑 12.8GiB,零页惰性故障实占远低)。**僵尸回收**(D6):节点 watchdog 用 wait4(WNOHANG) 探活自己的子进程(zombie 会应答 kill(pid,0),故必须收尸探活),FC 或 uffd 单侧死亡即整组清理、机内 CAS RUNNING→FAILED、经 Healthz 上报控制面写穿;恢复走 D1 的按需恢复。**Gateway**(D7):`ANY /v0/sandboxes/{id}/proxy/{port}/*path`,两跳 httputil.ReverseProxy(apiserver→节点 daemon UDS→netns 拨号 172.16.0.2),WebSocket 升级透传;in-proc 模式(embervm dev)单跳直拨。**egress 策略**(D8):create 带 `egress: nat|none`;none 在根 ns FORWARD 链插 DROP(打 embervm-<ID> 注释,teardown 顺带清理),宿主→guest 的 in-ns 拨号不受影响;规则随租约清理(槽位复用)、随快照描述符持久化(分层/跨节点恢复不会悄悄重新开网);零信任 L7 egress 代理(域名白名单/凭证注入)明确推迟到 v0.1 后。**可观测性**(D9):Prometheus /metrics 双平面(gin 路由 + 节点 socket),恢复延迟按层、创建延迟按路径、生命周期迁移、chunk 操作、代理请求、watchdog 回收、节点在线数、引擎 tick 错误;Grafana 起步盘随 deploy/ 发布;OTel tracing 推迟(单跳系统)。**退出门禁**(D10):e2e-m4 起 3 个 jailed nodeagent daemon(独立 ZFS 子树/工作目录/chunk 缓存/netns 区间)+ PG + MinIO,kill -9 任一 worker:调度器驱逐、RUNNING 标 FAILED、暂停沙箱异机恢复且进程连续、FAILED 沙箱从最后写穿恢复(写穿后数据丢失即 RPO 契约,e2e 验证"丢得对")、恢复后的 guest 经两跳网关代理出 HTTP;创建 <500ms 与 50 并发单独成门;G1-G6 验收记录在 docs/acceptance-v0.1.md,tag v0.4.0-m4 即开源 v0.1。

## Context

M1–M3 built a complete single-node lifecycle: REST + PostgreSQL, chunked sub-second restores
with an L1 write-through (ADR-0003), and TTL-driven tiering with a cold layer (ADR-0004). But
one node is a demo, not a service: Firecracker ran unjailed (the M1-deferred debt), creation
cold-booted a kernel every time, a dead FC process wedged its sandbox forever, and there was
no way to reach a guest port from outside. M4's charter (docs/zh/03 §3) is the jump to an
internal MVP: multi-node scheduling, jailer hardening, sub-500ms creation, 50 concurrency per
node, a gateway, egress policy, observability — with the exit criterion "kill any worker in a
3-node cluster and every sandbox on it is recoverable elsewhere", gating the open-source v0.1
release.

## Decision

### D1 — Node registry & heartbeats: poll, don't push

A `nodes` table (`id, addr, state 'up'|'down', capacity_mib, cpu_cores, last_seen`) with
**static membership**: `apiserver --nodes id=socket,...` (env `EMBERVM_NODES`); `embervm dev`
auto-registers its in-proc agent as node `local`. The scheduler goroutine polls every node's
`Healthz` (capacity + used MiB + sandbox count + watchdog-failed IDs) each tick (default 5s);
`MissThreshold` consecutive failures (default 3) → node `down`, its RUNNING/STARTING/RESUMING/
PAUSING sandboxes FAILED (their last write-through snapshot stays restorable), paused ones
untouched — they live in L1/L2 by construction. Recovery is **restore-on-demand**: the next
resume re-places the sandbox on a healthy node. No mass-migration storm, no gossip — the
control plane dials the nodes it knows (docs/zh/04 §6).

### D2 — Placement: sticky, then bin-pack; PostgreSQL is the only truth

`sandboxes.node_id` IS the routing table (no Redis at this scale). On create/resume: prefer
the previous node when it is up and has budget (L0 chunk-cache locality), else the up node
with the most free memory. Budgets are two-dimensional (docs/zh/03 超售): memory at
`capacity × MemOvercommit` (default 1.0) and vCPUs at `cores × CPUOvercommit` (default 3.0 —
vCPUs are time-shared threads). Free = budget − Σ(active sandboxes on the node) computed by
PG query; the in-memory registry is transport, never truth (the Corrosion lesson, docs/zh/04
§6). No capacity anywhere → 503.

### D3 — Jailer hardening: chroot-relative paths retire mountpoint pinning

Every Firecracker runs under `jailer --uid/--gid <base+netns-slot> --chroot-base-dir ...
--netns /var/run/netns/<lease>` with default seccomp ON. The chroot sees exactly two bind
mounts: `data/` (the sandbox dataset: rootfs.ext4, data.raw) and `snap/` (the workdir staging:
snapfiles, uffd.sock). Consequences:

- Snapfile drive paths become chroot-relative (`/data/rootfs.ext4`) — **identical on every
  node and sandbox**. Jailed restores therefore skip M3's `SetSandboxMountpoint` pinning and
  bind the receiving node's own dataset path into the fresh chroot; this is also what lets
  three nodes share one CI host without mountpoint collisions. The unjailed dev path keeps
  the pinning.
- Fast-create (D4) becomes possible at all: a snapshot taken in one chroot loads in any other.
- The jailer builds its chroot at `<base>/<exec-basename>/<id>/root`, so the FC binary is
  staged under a stable `firecracker` name (CI lesson #9).

### D4 — Golden fast-create: boot once per template, clone forever

At template build (when chunked + jailed + ZFS + L1 are all present), a **golden sandbox**
boots from the fresh template, pauses through the ordinary chunked write-through, and parks:
runtime resources (lease, cgroup, jail) return to the pool while its dataset snapshot
(`golden@p1`) and staging artifacts remain as the template's warm image
(`templates/<tid>/golden.json` in L1). `CreateSandbox` with matching geometry then skips the
kernel boot: `zfs clone golden@p1` (the disk must match the memory image's moment — cloning
the pristine template would be wrong), copy the golden's chain-root artifacts, hot-restore the
memory image. Mismatched geometry or missing golden falls back to cold boot — including on
nodes that never built the template, which first **receive the template stream from L1 on
demand** (same GUID-lineage stream the M2 restore path uses; a scheduler may place a create
anywhere). Clones are identical by design: VMGenID reseeds the guest RNG on load, guestd seq
continuity stays monotone-above-snapshot. Gate: create-to-interactive P50 < 500ms in CI.

### D5 — Balloon + oversell: reclaim idle guest memory

Boot config gains a virtio-balloon (`amount_mib: 0, deflate_on_oom: true`); the `SetBalloon`
verb retargets it on a RUNNING sandbox. Balloon-assisted pause (config `PauseBalloonSettle`)
inflates before snapshotting so freed pages drop out of the diff via the zero-page skip, and
resume deflates. CPU oversell is inherent (vCPU threads); memory oversell rides zero-page lazy
faulting. Gate: `TestConcurrency50` — 50 × 256 MiB on one agent, all exec-verified (logical
12.8 GiB on a 16 GiB runner).

### D6 — Zombie reaping: probe children with wait4, not signal 0

The agent watchdog (tick `WatchdogInterval`) scans RUNNING sandboxes: a dead FC with a live
uffd handler (or the reverse) is a sandbox that will never answer again. Liveness of our OWN
children cannot use `kill(pid, 0)` — an un-`Wait()`ed corpse is a zombie and zombies answer
signal 0; the probe is `wait4(WNOHANG)`, where collecting the corpse IS the death
notification. Reap = CAS RUNNING→FAILED in the in-memory machine (losing the CAS means a live
verb moved it — abandon), kill both processes, release jail/cgroup/lease/egress, drop local
state, record the ID for the next `Healthz` to write through (`store.FailSandbox`, active
states only). Recovery is D1's restore-on-demand. lifecycle.Machine is internally locked and
grew `CAS(from, to)` for exactly this destructive-observer discipline.

### D7 — Gateway: two chained reverse proxies, WebSocket-transparent

`ANY /v0/sandboxes/{id}/proxy/{port}/*path` (authenticated, owner-scoped) forwards any
HTTP(S)/WebSocket traffic to the guest port: apiserver → node daemon (UDS hop, nodeapi
`GuestProxy`) → guest `172.16.0.2:{port}` via the netns dialer. Both hops are
`httputil.ReverseProxy` (Upgrade passes through). In-proc agents (`embervm dev`) skip the
middle hop and dial the netns directly (`GuestDialer`). The sandbox row's node_id routes the
request; the proxy reaches only the sandbox's own netns. Port allowlists are deferred.

### D8 — Egress policy at the netns level; zero-trust L7 proxy deferred

`CreateSandbox` gains `egress: "nat" | "none"` (default nat = the existing MASQUERADE path).
`none` inserts a root-ns `FORWARD` DROP on the slot's veth subnet ahead of the pool's ACCEPT
rules — guest-originated traffic cannot leave the host, while host→guest dialing (which enters
the namespace and never crosses root-ns FORWARD) still works, so guestd and the gateway keep
functioning. Disciplines: the rule carries the slot's `embervm-<ID>` comment so
`teardown-network.sh` sweeps it with the slot's other rules; every lease-release path
(cleanup, WARM release, watchdog reap) clears it first — netns slots are pooled and a leaked
rule would cut off the next tenant; the policy rides the snapshot descriptor so a `none`
sandbox cannot regain internet by being tiered out and restored elsewhere. The full zero-trust
L7 egress proxy (domain allowlists, credential injection, secret redaction — docs/zh/02 §4) is
a product subsystem, explicitly deferred past v0.1.

### D9 — Observability: Prometheus now, tracing when there is a second hop

One process-wide registry (`pkg/metrics`), `/metrics` on both planes: the gin router
(apiserver and `embervm dev`) and the node daemon's unix socket. Instruments: restore seconds
by tier (hot observed at the resume verb, warm/cold end-to-end around fetch+resume — a shared
internal `resume()` keeps every user-facing flow observed exactly once), create seconds by
path (cold/fast), lifecycle transitions counted at the PostgreSQL truth, chunk ops
(put / dedup_hit / remote_get / gc_sweep), gateway proxy results, watchdog reaps, nodes-up
gauge, engine tick errors. `deploy/grafana/embervm.json` ships a starter dashboard. OTel
distributed tracing is deferred: a single-hop system traces nothing worth the dependency.

### D10 — Exit gates and the v0.1 release

`e2e-m4` boots PG + MinIO + loop ZFS and runs three gates with `--- PASS:` guards:

- **TestClusterKillNode** (THE exit gate): three **jailed nodeagent daemons** (separate ZFS
  subtrees, workdirs, chunk caches, netns ranges via `--netns-base`) + in-proc cluster control
  plane. Placement spreads one paused sandbox per node (observing bin-pack and the on-demand
  template receive); a RUNNING sandbox with a write-through behind it marks the victim.
  `kill -9` the victim daemon → scheduler evicts, RUNNING → FAILED; the paused-on-victim
  sandbox fails its hot resume then restores on a healthy node with continuity; the FAILED one
  restores from its last write-through — data written after that snapshot is verified GONE
  (the RPO contract, not a bug); healthy nodes' sandboxes hot-resume in place; the recovered
  guest serves HTTP through both gateway hops.
- **TestFastCreateUnder500ms** (G4): golden-path create P50 < 500ms to interactive, plus a
  pause/resume continuity check on a clone.
- **TestConcurrency50** (G4): 50 × 256 MiB on one agent, exec-verified.

Jailed lifecycle and watchdog gates stay in `integration-kvm` (both FC versions). G1–G6
acceptance evidence is recorded in `docs/acceptance-v0.1.md`; tag `v0.4.0-m4` ships as GitHub
Release **EmberVM v0.1** (开源 v0.1 发布).

## Out of scope (recorded here so nobody "finds" them missing)

- Dynamic node join/leave — static `--nodes` config in M4.
- Zero-trust L7 egress proxy — D8 ships the netns-level on/off only.
- Redis routing tables — the sandbox row is the routing table; PG suffices at this scale.
- Blue-green upgrade SOP — snapshot formats are version-stamped (FCVersion/KernelVersion in
  manifests) and CHANGELOG-gated; a scripted SOP waits for a second real deployment.
- OTel tracing, Temporal, L0 chunk-cache LRU, disk synthetic full, ARIMA prewarm — carried
  debts, see the M4 plan.
- G3's cold-tier $/TB and G5's 99.9% availability are procurement/ops claims, documented in
  the acceptance map rather than CI-gated.

## Consequences

- A worker loss is an inconvenience, not an incident: everything paused survives untouched,
  everything running loses at most the writes since its last pause (RPO = write-through), and
  the cluster keeps serving. The e2e proves the recovery path, including its data-loss edge.
- The jailer chroot turned out to be the load-bearing decision: chroot-relative snapshot paths
  are what make snapshots location-independent — they retired the M3 mountpoint pinning,
  unlocked golden fast-create, and made the shared-host CI cluster possible.
- Fast-create changes the create cost model: one kernel boot per template (amortized), then
  ~clone+restore per sandbox. The golden image is node-local; other nodes cold-boot after an
  on-demand template receive — replicating golden staging artifacts is future work (doc 04 #3
  memfd sharing is the deeper version).
- Static membership means adding a node is a config change + apiserver restart. Fine for an
  internal MVP; dynamic membership is deliberately not designed yet.
- The egress toggle is a hardening flag, not a security product. Anyone needing per-domain
  policy must wait for the L7 proxy.
