# ADR-0007: M6 runtime elasticity — resize, autoscale, migrate

- Status: Accepted
- Date: 2026-07-10

## 中文摘要

M6 补上系统唯一缺失的弹性维度:单沙箱规格的运行时动态调整。**底座决策**(D0):维持 Firecracker——virtio-mem 内存热插拔自 FC v1.14 引入(v1.15 修稳),本仓库钉的 v1.15.1/v1.16.1 均支持,guest 内核 6.1.155 已编入驱动(二进制验证),零版本升级;换 Cloud Hypervisor 可得 vCPU 热插拔但失去 uffd 内存后端与 diff 快照链(M2-M5 全部核心重写),不值(docs/zh/07 深度对比)。**Phase 0 验证实验**先行:boot 带 hotplug 区 → PATCH 扩/缩 → chunked pause → uffd restore → **恢复后再 PATCH**(文档未明说的关键交互)——全部通过,确认主路线,balloon-headroom 回退方案未启用。**创建期上限**(D1):`max_memory_mib`/`max_vcpus` 声明弹性上限(0=固定几何,特性 opt-in);内存按 `mem_size_mib=基础值` + `PUT /hotplug/memory{total=max−base}`(128MiB slot 取整,控制面镜像同一取整,两层恒等);boot args 追加 `memhp_default_state=online_movable`(缩容可靠优先,memmap 开销≈热插上限 1.6% 计入记账,memmap_on_memory 因缩容不可靠弃用);CPU 按 max_vcpus 核开机 + cgroup `cpu.max` 配额钳到 vcpus(FC 无 vCPU 热插拔,#2609;guest 可见核数恒为 max,quota 即有效算力);cgroup `memory.max` 上限取声明 max(扩容不撞墙,恶意 guest 有硬界)。**resize 动词**(D2):`POST /v0/sandboxes/:id/resize {memory_mib?,vcpus?}`,RUNNING 内执行不加新状态(SetBalloon 模式);控制面校验 ceiling + `Scheduler.CanFit` 增量准入(不足→409,提示 pause/resume 或 migrate,用户选定不自动迁移);节点侧 PATCH hotplug + 轮询 plugged 收敛(扩容必须收敛,缩容协作式允许部分达成回报实际值)+ 同步 cgroup;实际值写回 `sandboxes.memory_mib/vcpus`(语义变更:此二列即当前 effective,NodeUsage 的 SUM 自动成为调度记账)。**持久化与恢复**(D3):base/max 新列(0006 迁移)+ 描述符新字段(FormatVersion 仍 1,旧描述符=固定几何);插拔状态本体随 FC snapfile 走;**恢复后从 GET /hotplug/memory 回读 effective**(fork/rollback 恢复的是检查点时刻的插拔态,不是父/当前值)并经 SandboxStatus 新字段(memory_mib/vcpus)由控制面对账回写——resume/fork/rollback 三个处理器同一纪律;golden 几何匹配加入 max_*(golden 固定几何,可伸缩创建 cold boot);未插拔区=稀疏零页,memsnap 零页跳过天然消化,uffd 侧 EVENT_REMOVE 复用 balloon 的 removed rangeset(37504a0 已加固),backfill 对连续 Zero chunk 合并为 ≤64MiB 的整段 UFFDIO_ZEROPAGE(避免大热插区 ioctl 风暴)。**自动弹性**(D4/D5):create 带 `autoscale:true`(需 ceiling);guestd /healthz 扩展 MemTotal/MemAvailable/PSI avg10;生命周期引擎每 tick 扫 RUNNING+autoscale,滞回策略(扩:avail<10% 或 PSI-mem>20 连续 2 tick,+256MiB/+1cpu;缩:avail>50% 连续 10 tick,趋向 base 地板;动作后冷却 30s;阈值可配,门禁用宽带宽免 OOM 边缘);增长过不了 CanFit 记 deferred 指标静默延后;引擎经 scaleAgent 断言复用节点 resize 路径,PG 始终对账。**migrate 动词**(D6):`POST /v0/sandboxes/:id/migrate {node_id?}` = pause(写穿)→ CAS HOT→WARM + ReleaseLocal(引擎同款破坏源纪律)→ SetSandboxNode → 目标节点 warm restore → RUNNING + 几何对账;PAUSED_HOT 只挪指针(落 PAUSED_WARM,下次 resume 落目标);目标缺省 `PlaceExcluding`(排除当前节点 bin-pack),显式目标过 CanFit。**门禁**(D7):e2e-m6 四门 `--- PASS:` 守卫——TestVirtioMemResizeKVM(扩→写满→缩→chunked pause→uffd restore→**再 resize**→热拔→再快照往返)、TestResizeCPUQuotaKVM(nproc=max + cpu.max 随 resize 移动)、TestAutoscaleMemoryKVM(真实 guest 压力驱动引擎扩容、释放后缩回,PG 端到端)、TestMigrateRunningKVM(双 jailed 节点活体迁移,数据/存活/指针全验);单测覆盖 resize 处理器矩阵、CanFit、autoscale 滞回/冷却/错误复位/CanFit 延后、migrate 双态。

## Context

M0–M5 built full host-level elasticity — lazy faulting, balloon reclaim, two-axis oversell,
pause-to-zero — but a sandbox's own geometry (`memMiB`, `vcpus`) was immutable after create:
no vertical scaling, ever. That was the one structural advantage container runtimes held over
this architecture (`docker update` is trivial). The 2026 state of the underlying stack closed
the gap: Firecracker gained virtio-mem memory hotplug in v1.14 (stabilized v1.15) — both
pinned FC versions already have it, and the pinned guest kernel (6.1.155) ships the driver.
M6's charter: runtime memory resize, runtime CPU resize, pressure-driven automatic
elasticity, and an explicit cross-node migrate verb — with growth beyond node capacity
rejected loudly (409), never silently migrated.

The full base-technology comparison (Docker vs Firecracker vs Cloud Hypervisor vs gVisor,
2026 landscape) that anchors the "stay on Firecracker" decision is docs/zh/07.

## Decision

### D0 — Stay on Firecracker; spike before build

Cloud Hypervisor's mature CPU+memory hotplug does not outweigh losing the uffd memory
backend and diff snapshot chains that M2–M5 are built on. The undocumented risk — whether
`PATCH /hotplug/memory` still works on a snapshot-restored VM — was settled by a Phase-0
experiment before any plumbing: it does, across chunked pause → uffd restore → resize →
hot-unplug → snapshot round-trip. The balloon-headroom fallback design was therefore never
needed. (The experiment survives as the standing gate `TestVirtioMemResizeKVM`.)

### D1 — Ceilings are declared at create; the VM boots with them

`CreateSandbox` gains `max_memory_mib` / `max_vcpus` (0 = fixed geometry — the entire
feature is opt-in per sandbox). Memory: `mem_size_mib` stays the requested base; a
virtio-mem region of `max − base` (rounded up to the 128 MiB KVM slot; the control plane
mirrors the rounding so both layers agree by construction) is attached pre-boot and starts
fully unplugged — it costs nothing until a resize plugs blocks. Boot args gain
`memhp_default_state=online_movable`: hotplugged blocks land in ZONE_MOVABLE so unplug can
actually migrate pages away and hand memory back (the `memmap_on_memory` alternative avoids
the ~1.6%-of-ceiling memmap cost but makes reclaim unreliable — reclaim is the point).
CPU: the VM boots with `max_vcpus` cores (Firecracker will not hotplug vCPUs, upstream
#2609) and the cgroup `cpu.max` quota clamps effective compute to `vcpus`; guest-visible
core count is constant, the quota moves. The cgroup `memory.max` covers the declared max
(+256 MiB VMM headroom): growth must not trip it mid-resize, and it remains the hard bound
against a hostile guest driver (the trust-model mitigation the upstream docs prescribe).

### D2 — The resize verb: in-RUNNING, admission-checked, accounted by achieved values

`POST /v0/sandboxes/:id/resize {memory_mib?, vcpus?}` runs entirely within RUNNING (the
SetBalloon pattern — no new lifecycle state). The control plane validates ceiling bounds,
then admission-checks growth deltas via `Scheduler.CanFit` against the same oversold budgets
Place uses; insufficient room is a 409 naming the options (pause/resume re-place, or
migrate) — per the product decision, never an automatic migration. The node PATCHes the
hotplug target and polls `GET /hotplug/memory` until the guest driver converges: growth must
converge (a stuck driver is an error); shrink is cooperative by design and may legally land
above the ask — the achieved size is reported, not faked. cgroup limits move with the
operation. The achieved values are written back to `sandboxes.memory_mib/vcpus` — those
columns now mean CURRENT EFFECTIVE geometry, which makes `NodeUsage`'s existing SUM the
scheduler's live accounting with zero query changes. On a half-applied failure the handler
reconciles from `Status` before erroring, so accounting never lies.

### D3 — Restore rewinds plug state; read it back, don't assume it

The plug state itself rides the Firecracker snapfile. But the checkpoint being restored is
not necessarily the last one this process saw: fork restores the parent's checkpoint,
rollback an older one, cross-node restore whatever L1 holds. So after every LoadSnapshot the
node reads `GET /hotplug/memory` and recomputes `memMiB = base + plugged`; `SandboxStatus`
now carries effective geometry, and the resume/fork/rollback handlers all reconcile the PG
row from it. The descriptor gains `base_memory_mib`/`max_memory_mib`/`max_vcpus`
(FormatVersion stays 1; zero values read as fixed geometry). Golden fast-create treats the
hotplug region as part of the geometry: goldens are built fixed, so resize-enabled creates
miss and cold-boot (replicating resizable goldens is future work). Unplugged ranges are
sparse holes → memsnap's zero-page skip stores nothing; the uffd handler's existing
EVENT_REMOVE discipline (the balloon `removed` rangeset, hardened in 37504a0) covers
hot-unplug unchanged; backfill coalesces contiguous Zero chunks into ≤64 MiB
UFFDIO_ZEROPAGE spans so a large never-plugged region does not become an ioctl storm.

### D4/D5 — Autoscale: guest-reported pressure, engine-driven, conservatively damped

`autoscale: true` at create (requires a ceiling) opts the sandbox into the lifecycle
engine's scan. guestd's `/healthz` now reports MemTotal/MemAvailable and PSI some-avg10 for
memory and CPU (zeros on kernels without PSI — the engine skips what it cannot see). Policy
per tick, with per-sandbox hysteresis: grow when MemAvailable < 10% or PSI-mem > 20 for 2
consecutive ticks (+256 MiB or +1 vcpu, capped at max); shrink when MemAvailable > 50% for
10 consecutive ticks (toward the create-time base floor — never below what the user asked
for); 30s cooldown after any action; unreachable guests reset counters (stale counters lie).
Growth that fails CanFit is DEFERRED (metric, cooldown, retry) — the ceiling is a wish, the
node is reality. The engine reaches the node through a `scaleAgent` type-assertion on its
TierAgent and drives the same resize path as the verb; PG is reconciled with achieved values
even on partial failure. Thresholds are config-tunable (gates run a wider band so the test
guest is not held at the edge of the OOM killer — which reaps PID-1 guestd and, via the
watchdog, the whole sandbox, as the first gate run demonstrated).

### D6 — Migrate: the recovery path, packaged as a verb

`POST /v0/sandboxes/:id/migrate {node_id?}` composes machinery every piece of which M2/M4
already proved: pause (chunked write-through) → CAS HOT→WARM + `ReleaseLocal` (the engine's
destructive-to-source discipline) → `SetSandboxNode` → warm restore on the target →
RUNNING, with geometry reconciliation. A PAUSED_HOT sandbox just moves its placement pointer
(lands PAUSED_WARM; the next resume restores on the target) — migrating a paused sandbox
must not wake it. Default target is `PlaceExcluding(current)` (bin-pack anywhere but here);
an explicit target is CanFit-checked. RUNNING migration costs one pause/restore cycle
(seconds; the guest process survives).

### D7 — Gates

`e2e-m6` runs four `--- PASS:`-guarded gates: **TestVirtioMemResizeKVM** (grow → dirty the
new memory → shrink → chunked pause → uffd restore → MemTotal continuity → resize AGAIN →
hot-unplug under live uffd → second snapshot round-trip), **TestResizeCPUQuotaKVM** (guest
nproc = max_vcpus; cpu.max moves with the verb; above-ceiling rejected),
**TestAutoscaleMemoryKVM** (real tmpfs pressure grows the sandbox through the engine and PG;
releasing it shrinks back to base), **TestMigrateRunningKVM** (two jailed daemons; a RUNNING
sandbox migrates with data, liveness, and placement verified on the target, and the source
node no longer tracking it). Unit suites cover the resize handler matrix, CanFit, autoscale
hysteresis/cooldown/error-reset/defer, and both migrate modes.

## Out of scope (recorded here so nobody "finds" them missing)

- Guest-visible vCPU count changes — Firecracker has no vCPU hotplug; the quota is the
  effective-compute contract. guestd-driven in-guest CPU online/offline is a possible
  refinement, not shipped.
- Automatic migration on resize overflow — 409 is the contract; migrate is explicit.
- A load-driven cluster rebalancer — the migrate verb is its building block, not its policy.
- Resizable golden snapshots — resize-enabled creates cold-boot today.
- Balloon-headroom fallback — designed, documented here, not needed (Phase 0 said GO).
- CPU autoscale PSI thresholds are first-cut constants; production tuning waits for real
  workload data.

## Consequences

- The last "containers do this better" gap is closed: a sandbox's memory can grow and
  shrink at runtime with real host reclaim (mprotect-hard, better than balloon), CPU within
  its boot ceiling, automatically if opted in — while keeping every state-as-asset property
  (snapshot/diff/uffd/fork) that justified Firecracker in the first place.
- `memory_mib`/`vcpus` changed meaning from "requested at create" to "current effective".
  Everything that summed them (NodeUsage, Healthz capacity) got live accounting for free;
  anything that assumed them immutable must read `base_*`/`max_*` (new columns, additive).
- Fixed-geometry sandboxes are byte-for-byte unaffected: no hotplug device, no boot-arg
  change, quota unset, old descriptors and golden.json read as fixed. The feature costs
  nothing until asked for.
- The memmap tax of a declared ceiling (~1.6% of the hotplug region, resident in guest boot
  memory) is the price of reliable shrink; it scales with max, not with use.
- A resized sandbox's snapshot chain carries the region: restore-anywhere still holds, but
  cross-geometry fork/golden matching is stricter than before (max_* must match too).
