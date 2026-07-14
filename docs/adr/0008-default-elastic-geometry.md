# ADR-0008: M7 default-elastic geometry — on-demand by default, resizable goldens

- Status: Accepted
- Date: 2026-07-14
- Note: docs/zh/08 suggested "0008" for the backend-abstraction ADR; that
  work moves to ADR-0009 — this number records default-elastic, which
  shipped first.

## 中文摘要

M6 交付了弹性的全部机械件（virtio-mem resize、cgroup `cpu.max`、PSI autoscale、migrate），但都是 **opt-in**：创建必须显式声明 `max_*` 上限（且必须带显式 base）与 `autoscale:true`，默认仍是"创建即固定"；弹性沙箱还错过 golden 快照只能冷启动。M7 把**按需分配变成默认行为**。**默认解析**（D1）：控制面新增纯函数 `resolveGeometry`（`pkg/controlplane/defaults.go`），决策表——不带任何几何 → base 256MiB/1vCPU + 默认上限 4GiB/4vCPU（`ElasticDefaults` 可配，env `EMBERVM_DEFAULT_*`；`embervm dev` 旗标同源喂给 Server 与节点 golden 配置保证自洽）+ **autoscale 默认开**；只带上限不带 base（控制台预设形态）→ 补默认 base，autoscale 开；显式 base 不带 max → 固定几何与 M6 字节级一致（缺的半边补默认值，修复了旧路径 `memory_mib=0` 入库、`Place(0,0)` 少记账的瑕疵）；显式 base+max → 与 M6 完全一致（autoscale 省略=关）；`autoscale` 从 bool 改 `*bool`（nil=平台默认，显式 false 恒生效，线上兼容）；`EMBERVM_DEFAULT_ELASTIC=false` 整体还原旧行为。取整仍单点在 createSandbox（默认 4096−256=3840 恰 30 slot 幂等）。**双槽 golden**（D2）：`Config.GoldenMaxMemoryMiB/GoldenMaxVCPUs` 设置时 BuildTemplate 急切构建第二个 **elastic golden**（带 hotplug 区 + max 启动核数，L1 键 `golden-elastic.json`，id `goldenID(tid+"#elastic")`）；elastic meta 构建时应用与 CreateSandbox **相同的取整公式**（否则 goldenFor 精确匹配永 miss）；`goldenFor` 按请求是否弹性选槽位，匹配抽为纯函数 `goldenMatches`，几何 miss 打日志（CP 默认与节点 golden 配置漂移=静默冷启动，这行日志是诊断线）；elastic 构建失败仅降级（fixed golden 保留）；`cmd/nodeagent` 补齐五个 `--golden-*` 旗标（此前生产节点根本无法启用 golden）。**引擎加固**（D3）：autoscale 默认开后扫描面变大——两阶段扫描（errgroup ≤16 并发收集 guest Health，每探针 3s 超时；决策/动作保持串行，滞回 map 无锁），一个卡死 guest 不再拖垮整个 tick。**API 补口**（D4，控制台需要）：`POST /v0/sandboxes/:id/autoscale {autoscale:bool}` 运行时开关（开启需已有 ceiling，409；写审计事件）；resize/autoscale/migrate 动作写入 `sandbox_events`（复用 detail jsonb，`from_state=to_state`，判别式 `ResourceEventDetail{kind,actor,reason,memory_mib:[old,new],...}`，deferred 每 episode 只写一条）；`GET /v0/nodes` 增补 `base_mib/ceiling_mib/base_vcpus/ceiling_vcpus/mem_budget_mib/vcpu_budget`（全部 additive/omitempty，超售系数向 scheduler 取）。**控制台**（D5）：创建对话框弹性默认（预设 chips：标准=服务端默认/小型/大型；Advanced 显式 base+max+autoscale；Fixed 保留），toast 回读服务端解析值；工作台资源中心（Mem/CpuGauge + AutoscaleBadge、有效内存 ~10 分钟阶梯图带 base/max 参考线、autoscale 与手动 resize 的诚实交互文案 + "关闭自动伸缩并应用"、节点满 409 → 迁移重试面板、伸缩记录卡、运行时 autoscale Toggle 对旧服务端 404 降级隐藏）；舰队列表（余量列/最近伸缩列/autoscale 筛选）；Nodes 页 OversellBar（base 实心/effective/ceiling 斜纹/capacity+budget 刻度，超预算警示）；活动流 All/Lifecycle/Resources 过滤；⌘K 三个新动作。**门禁**（D6）：`e2e-m7` 一门 `TestFastCreateElasticKVM`——双 golden 入 L1、**memmap 税实测探针**（4GiB ceiling 下 guest MemAvailable ≥100MiB 地板，ADR-0007 的 ~1.6% 论断首次被度量）、弹性 fast-create P50<500ms、**jailed golden 克隆上的 hotplug PATCH**（M6 门禁未证明的唯一交互：新身份 clone-restore 后 resize 收敛 + tmpfs 写满新内存 + pause/resume 后再 resize）、fixed golden 无回归。

## Context

M6 (ADR-0007) closed the runtime-elasticity gap mechanically, but left the
default untouched: a create that names no geometry produced a FIXED
256 MiB/1 vCPU sandbox, elasticity required declaring `max_*` ceilings with
an explicit base, autoscale required `autoscale:true`, and resize-enabled
creates missed golden fast-create and cold-booted (a documented M6
out-of-scope). The product ask for M7: **on-demand allocation as the
default** — sandboxes start small and grow with load, nothing frozen at
create unless explicitly requested — plus a console that manages all of it.

Two prior defects fall out of the same change: a no-geometry create stored
`memory_mib=0` in PostgreSQL and placed with `Place(ctx, "", 0, 0)`, so
`NodeUsage` under-counted every defaulted sandbox; and `cmd/nodeagent` had
no golden flags at all, so production nodes could not enable fast-create.

## Decision

### D1 — Default resolution in the control plane, one pure function

`resolveGeometry(body, ElasticDefaults)` (pkg/controlplane/defaults.go) runs
after `validateGeometry`, before `validateCeilings`:

| base given? | max given? | autoscale (JSON) | result |
|---|---|---|---|
| no | no | nil / true | default elastic: base 1/256, ceiling 4/4096 (config), autoscale ON |
| no | no | false | default elastic geometry, autoscale off |
| no | yes | nil / true | elastic with the custom ceiling, default base, autoscale ON |
| yes | no | nil / false | fixed geometry, exactly M6 (missing halves filled for accounting truth) |
| yes | no | true | 400 — a fixed sandbox cannot autoscale |
| yes | yes | any | user-declared elastic, exactly M6 (nil = off) |

`Autoscale` becomes `*bool` (nil = platform default; wire-compatible — the
only behavior change for an omitted field is the brand-new no-geometry
case). `ElasticDefaults` hangs off `Server.Elastic` (assigned
post-construction like `Engine.CanFit`); zero value = enabled with platform
defaults, `Disabled` restores pre-M7 byte for byte. Env:
`EMBERVM_DEFAULT_ELASTIC`, `EMBERVM_DEFAULT_MAX_MEMORY_MIB` (pre-rounded to
the slot grid, logged), `EMBERVM_DEFAULT_MAX_VCPUS`; `embervm dev` flags
feed the same values to the node agent's golden config so dev-mode
fast-create geometry matches by construction. Slot rounding stays at its
single site in createSandbox; the node's `roundUpMiB` is idempotent on the
result.

### D2 — Two golden slots per template

`Config.GoldenMaxMemoryMiB/GoldenMaxVCPUs` (nodeagent) build a SECOND golden
at BuildTemplate: same base, hotplug region to the ceiling, max boot cores —
parked as `templates/<tid>/golden-elastic.json` with sandbox id
`goldenID(tid+"#elastic")`. The elastic meta applies CreateSandbox's exact
ceiling normalization (`base + roundUpMiB(max-base, 128)`), the invariant
`goldenFor`'s exact match depends on. `goldenFor` picks the slot by whether
the (already normalized) request is elastic; the match is the pure
`goldenMatches`, and a geometry miss now logs — config drift between the
control plane's defaults and the node's golden geometry would otherwise be
an invisible cold-boot regression. Elastic build failure keeps the fixed
golden (optimization-not-correctness discipline). `fastCreate`/
`cloneRestore`/descriptor needed zero changes (M6 already plumbed ceilings
through the clone path). `cmd/nodeagent` gains `--golden-vcpus`,
`--golden-memory-mib`, `--golden-data-disk-gib`, `--golden-max-memory-mib`,
`--golden-max-vcpus`.

### D3 — Autoscale scan hardening

Default-on autoscale turns the engine's per-tick scan into "most RUNNING
sandboxes". The scan is now two-phase: guest health probes run concurrently
(errgroup, ≤16) each under a 3s deadline (guestapi takes timeouts only from
the call ctx — one wedged guest used to stall the whole tick on the raw
engine ctx); decide/act stays serial, so the hysteresis map and the
CanFit-then-resize ordering remain single-threaded and the unit suites
deterministic.

### D4 — The console's three API gaps

1. `POST /v0/sandboxes/:id/autoscale {autoscale}` — runtime toggle
   (create-time was the only switch). Enabling without a ceiling is 409;
   toggles append an audit event.
2. Resource events on the timeline: `Store.AppendSandboxEvent` writes
   non-transition rows (`from_state = to_state`) with a typed
   `ResourceEventDetail{kind: resize|migrate|autoscale_config, actor,
   reason: manual|pressure|deferred, memory_mib/vcpus: [old,new], from/to
   node, psi/avail context}`. Writers: the resize verb, `autoscaleOne`
   (deferred writes ONCE per episode via a `scaleState.deferred` latch —
   a full node must not write a row every 30s), migrate (both modes), the
   autoscale toggle. `GET /v0/events` ships `detail` verbatim — no API
   change.
3. `GET /v0/nodes` gains `base_mib`, `ceiling_mib`, `base_vcpus`,
   `ceiling_vcpus` (fixed rows count effective on both ends),
   `mem_budget_mib` (= capacity × MemOvercommit), `vcpu_budget` (= cores ×
   CPUOvercommit) — all additive/omitempty, budgets read from the scheduler
   that enforces them.

### D5 — Console: complete resource management

Create dialog: Elastic (recommended) vs Fixed segmented modes; simple
elastic sends only `template_id` (+ ceiling preset when non-default) so the
server's resolution stays the single truth, and the success toast reads the
RESPONSE back; Advanced exposes explicit base/max/autoscale with client
validation mirroring the server rules (web/src/lib/geometry.ts). Workspace
Resources panel: Mem+Cpu gauges with base/max tracks, AutoscaleBadge, a
~10-minute effective-memory staircase chart (the 2.5s health poller's
`mem_total_kib` IS the live plugged size) with base/max reference lines,
honest autoscale-vs-manual interplay copy plus "turn off autoscale &
apply", a node-full 409 panel offering migrate-and-retry / target choice /
pause-resume, a scaling-activity card, and a runtime autoscale toggle that
hides itself on pre-M7 servers (404). Fleet: headroom sort + last-scale
column + autoscale filter on the list; Nodes page renders an OversellBar
(solid base / effective / hatched ceiling-sum with capacity+budget ticks,
danger tint when ceiling > budget) degrading to the old CapacityBar when
the fields are absent; the activity feed gains All/Lifecycle/Resources
filters; ⌘K gains resize/toggle/migrate actions. One parser module
(web/src/lib/resourceEvents.ts) is the single place the event contract
lives; unknown kinds fall back to generic rows.

### D6 — Gate

`e2e-m7` runs `TestFastCreateElasticKVM`: both golden metas land in L1; a
4 GiB-ceiling cold boot measures the memmap tax (guest MemAvailable must
keep a ≥100 MiB floor on the 256 MiB base — ADR-0007's "~1.6% of the
region" claim, measured for the first time); five elastic-geometry creates
fast-create at P50 < 500 ms; the golden CLONE — a jailed clone-restore into
a new identity, the one interaction no M6 gate proved — answers
PATCH /hotplug/memory (grow to 768 converges, tmpfs dirtying past base
proves the memory is real, cpu quota moves), then pause/resume preserves
the plug state and a post-restore resize to 1024 converges; a fixed-
geometry create still rides the fixed golden.

## Out of scope

- Shrink below base — the create-time floor stays the contract (user
  decision).
- Sticky manual overrides while autoscale is on — the engine recomputes
  from current geometry every tick; a manual grow on an idle guest WILL
  shrink back (~10 ticks). The console says so honestly and offers the
  turn-off-first path; override semantics are future backend work if asked
  for.
- Resizable goldens beyond the ONE configured elastic geometry — arbitrary
  ceilings still cold-boot (they miss the exact match by design).
- Replicating golden geometry via node heartbeat (would close the
  config-drift gap properly; the goldenFor miss log is the interim
  diagnostic).

## Consequences

- A bare `POST /v0/sandboxes {"template_id": …}` now yields an elastic,
  autoscaling sandbox (256 MiB→4 GiB, 1→4 vCPU) that fast-creates — the
  "fixed at creation" default is gone. Existing clients passing explicit
  geometry are byte-for-byte unaffected; `EMBERVM_DEFAULT_ELASTIC=false`
  restores everything.
- PG rows always carry real geometry: the `Place(0,0)` under-accounting for
  defaulted creates is fixed as a side effect.
- The memmap tax of the default ceiling is now measured per CI run instead
  of quoted; if the probe's floor ever fails, the default pairing
  (256/4096) gets retuned via the existing knobs, not re-plumbed.
- The timeline now records WHY geometry changed (user vs engine vs
  deferred), which the console surfaces — capacity pressure is visible
  before it becomes a support ticket.
