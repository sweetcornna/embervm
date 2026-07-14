# Changelog

All notable changes to EmberVM are recorded here. The project is pre-1.0:
the `v0.x` REST API and on-disk/snapshot formats **may break between minor
versions**, and every break is listed here (docs/zh/05 §4). Format loosely
follows [Keep a Changelog](https://keepachangelog.com/); versions follow
[SemVer](https://semver.org/).

## [Unreleased]

### Added — M7 default-elastic geometry (ADR-0008)

- **On-demand allocation is now the default.** `POST /v0/sandboxes` with no
  geometry fields creates an ELASTIC sandbox: base 256 MiB / 1 vCPU, resize
  ceiling 4 GiB / 4 vCPU, **autoscale on** — it starts small and grows with
  guest pressure instead of being fixed at creation. Explicit geometry
  keeps its M6 meaning (base without `max_*` = fixed; base + `max_*` =
  user-declared elastic), so existing clients are unaffected. A ceiling
  without a base (`{"max_memory_mib": …}`) is now valid and gets the
  default base — the console presets' shape. Knobs:
  `EMBERVM_DEFAULT_MAX_MEMORY_MIB`, `EMBERVM_DEFAULT_MAX_VCPUS`, and
  `EMBERVM_DEFAULT_ELASTIC=false` to restore the pre-M7 behavior
  (`embervm dev` mirrors them as `--default-*` flags).
- **Elastic golden snapshots** — templates now bake a second golden with
  the hotplug region + max boot cores (`Config.GoldenMaxMemoryMiB/
  GoldenMaxVCPUs`; new `--golden-*` flags on `cmd/nodeagent`, which
  previously could not enable goldens at all), so default-elastic creates
  fast-create (<500 ms P50) instead of cold-booting. Gated by `e2e-m7`
  (`TestFastCreateElasticKVM`), which also proves hotplug resize on a
  jailed golden clone across pause/resume and measures the memmap tax of
  the 4 GiB default ceiling.
- **Runtime autoscale toggle** — `POST /v0/sandboxes/:id/autoscale
  {"autoscale": bool}` (enabling requires a ceiling; audited on the
  timeline).
- **Resource timeline events** — resize (user AND autoscale, with pressure
  context; deferred-growth once per episode), migrate, and autoscale
  toggles now land in `sandbox_events` as typed `detail` payloads
  (`kind: resize|migrate|autoscale_config`); `GET /v0/events` carries them
  unchanged.
- **Node oversell reporting** — `GET /v0/nodes` adds `base_mib`,
  `ceiling_mib`, `base_vcpus`, `ceiling_vcpus`, `mem_budget_mib`,
  `vcpu_budget` (all additive).
- **Console: complete resource management** — elastic-by-default create
  dialog with ceiling presets + advanced/fixed modes; workspace Resources
  panel (memory+CPU gauges, effective-memory history chart with base/max
  reference lines, honest autoscale-vs-manual interplay with "turn off
  autoscale & apply", node-full 409 panel offering migrate-and-retry,
  scaling-activity card, runtime autoscale toggle with pre-M7 degradation);
  fleet list headroom sort + last-scale column + autoscale filter; Nodes
  oversell bars (base/effective/ceiling vs capacity & budget); activity
  feed All/Lifecycle/Resources filters; ⌘K resize/toggle/migrate actions.

### Changed

- The lifecycle engine's autoscale scan probes guests concurrently with a
  3s per-probe deadline (one wedged guest no longer stalls the whole tick).
- No-geometry creates now store real base values in PostgreSQL and reserve
  them at placement (previously `memory_mib=0` rows under-counted
  `NodeUsage`).

### Compatibility

- `autoscale` in the create body is now tri-state (`null` = platform
  default). Omitting it behaves as before whenever any geometry is passed.
- Old golden.json objects, descriptors (FormatVersion 1), and pre-M7
  consoles/servers interoperate unchanged; new node fields and the
  `/autoscale` route degrade gracefully in both directions.

## [v0.7.1] — 2026-07-14 — Bilingual console — **v0.4.1**

### Added

- **Bilingual console (English / 简体中文)** — the whole console UI is now
  localizable, with a sidebar `EN | 中` toggle. A dependency-free i18n core
  (`web/src/lib/i18n.ts`: module-level locale + `useSyncExternalStore`,
  bilingual `t("English", "中文")` at each call site, persisted in
  `localStorage`, defaulting to the browser language) drives every page,
  workspace tab, dialog, toast, menu, empty/error state, and the lifecycle
  status labels (`STATE_META` gained a `zh` field + a `stateLabel(state, t)`
  helper). Technical values (API field names, IDs, monospace command
  examples, state enums on the wire) stay verbatim. No new dependency; the
  offline-bundle guarantee is unchanged.

### Fixed

- **`t`-variable shadowing hazard** — several components use `t` as a local
  map/loop/destructure variable, which the i18n `t` from `useI18n()` would
  silently shadow (a runtime bug tsc cannot catch when the types align).
  Renamed the locals (`tpl` / `item` / `tab` / `row` / `x`) across
  Templates, createSandbox, toast, tabs, Workspace, and Storage so every
  `t(en, zh)` call resolves to the translate function.

## [v0.7.0] — 2026-07-14 — Console workbench — **v0.4**

The embedded console becomes a **complete workbench**: a per-sandbox
IDE-style workspace (interactive terminal, file browser + editor, port
preview, live pressure gauges, a merged checkpoint/lifecycle timeline with
time-travel exec) plus a fleet operations cockpit (Nodes page, activity
feed, richer sandbox/storage/template views, a ⌘K command palette). Ships
five new bearer-authenticated endpoints (`…/term` WS PTY, `…/health`,
`…/events` + `/v0/events`, `…/files?op=list`, `POST/DELETE /v0/proxy-session`)
and one new Go dep (`github.com/coder/websocket`, guestd side). All
additions are backward-compatible; no wire or on-disk format changed.

### Fixed

- **Platform credentials leaked into untrusted guests (security)** — two
  paths let guest-controlled code obtain a usable EmberVM credential.
  (1) The Preview tab's "open in new tab" navigated a top-level window to
  the same-origin guest-proxy URL; a top-level document is not covered by
  the iframe `sandbox`, so guest scripts ran at the console's origin and
  could read the bearer token from `localStorage`. The affordance was
  removed — preview stays inside the opaque-origin sandboxed iframe.
  (2) `proxyGuest`/`termSandbox` forwarded the `embervm_proxy` session
  cookie (and any `Authorization` header) verbatim into the guest; an
  untrusted guest could capture the 8 h owner-scoped cookie and replay it
  against the owner's other sandboxes' proxy ports. Both handlers now strip
  `Cookie` + `Authorization` before forwarding (mirroring the existing
  `Sec-WebSocket-Protocol` bearer stripping in `Auth()`).
- **Console blank-screen on Storage (and future render throws)** — the
  storage-report endpoint returns an object (`{sandboxes, total_*}`), but
  the console typed it as an array and called `.reduce`, throwing and —
  with no error boundary — white-screening the whole app when the Storage
  tab was opened. Fixed the type/usage to consume the server's pre-summed
  totals, and added a route-keyed `ErrorBoundary` so any future render
  error degrades to a readable panel instead of a blank page.

- **uffd zero-fill truncation** — `zeroRange` treated the kernel stopping a
  multi-page `UFFDIO_ZEROPAGE` at an already-mapped page as "span done"; at
  the M6 coalesced zero-run sizes that left unfilled tails marked populated,
  and the next fault on such a page was consumed but never woken — hanging
  whatever touched it (CI e2e-m3: a post-cold-restore snapshot writer, 29
  minutes). zeroRange now skips mapped pages (authoritative by
  construction: zeros or newer guest writes) and resumes behind them;
  regression test drives a real userfaultfd.
- **Stale node rows win placement** — `RegisterNodes` now revives its
  members and retires rows absent from the static registry. Previously a
  DB that had run a different topology (a single-node `local` before a
  `--nodes` cluster) kept the old row `up` forever with unlimited
  capacity, so `Place` routed creates/builds to a node the registry could
  not resolve (503 "unknown node").

### Added

- **Workspace (console phase 1)** — the per-sandbox detail page grew into a
  full-bleed IDE-style workspace at `#/sandboxes/:id/{overview,terminal,
  files,checkpoints}` (tab state = URL): an **interactive terminal**
  (xterm.js, multi-session, scrollback survives tab switches, sessions
  survive hot pause/resume), a **file browser + editor** (lazy directory
  tree, CodeMirror 6 with a token-native theme, binary/size guards,
  upload/download/new-file, `⌘S` saves to the guest disk), **live guest
  gauges** (memory used, PSI memory/CPU sparklines off a 2.5 s health poll
  with a rolling in-browser window), a checkpoints tab, an always-visible
  health pill, optimistic lifecycle verbs with toasts, and a proper
  confirm dialog replacing every `window.confirm`. New deps (all
  offline-bundled): `radix-ui` (menu/tabs/tooltip/toast primitives),
  `@xterm/*`, `codemirror`; xterm and CodeMirror are code-split out of the
  entry chunk.
- **Interactive terminal API** — `GET /v0/sandboxes/:id/term` upgrades to a
  WebSocket and tunnels to a PTY served by guestd inside the guest
  (`/bin/sh -l`; subprotocol `embervm-term.v1`: binary frames = raw PTY
  bytes, text frames = JSON resize/exit control). The data path reuses the
  WS-transparent guest-proxy machinery (no new nodeapi verb); browser auth
  rides a `Sec-WebSocket-Protocol` `bearer.<base64url>` entry that the
  auth middleware validates and **strips**, so the platform credential
  never reaches guest code. Session policy lives in guestd: 8 concurrent
  sessions, 30 min idle timeout, ping/pong reaping of orphaned shells
  after snapshot restores. guestd's init now mounts `devpts` (with a
  `/dev/pts/ptmx` fallback in the PTY opener) so minimal images get PTYs.
  New Go dep: `github.com/coder/websocket` (guestd + tests; registered in
  THIRD_PARTY_NOTICES).
- **Guest health proxy** — `GET /v0/sandboxes/:id/health` exposes guestd's
  live pressure (MemTotal/MemAvailable, PSI some-avg10, resumes, seq) to
  the console; non-RUNNING answers `200 {ok:false}` without touching the
  node, and a 2 s server-side cache bounds the probe rate. New metrics:
  `embervm_guest_health_probes_total`, `embervm_term_sessions_active`,
  `embervm_term_sessions_total`.
- **Directory listing** — `GET /v0/sandboxes/:id/files?op=list&path=` lists
  a guest directory (lstat metadata, symlink targets, dirs-first, 10 000
  entry cap) via a new `ListDir` verb through nodeapi/guestd; plain
  `GET /files` reads are unchanged.
- **Preview & time travel (console phase 2)** — a **Preview tab** renders
  any guest TCP port in an iframe through the existing WS-transparent
  guest proxy (port chips + per-sandbox recent ports);
  since iframes cannot carry Authorization, `POST /v0/proxy-session`
  trades the bearer token for an HttpOnly `SameSite=Strict` cookie that
  the auth middleware honors **only on `/proxy/` routes** (ownership still
  enforced per-sandbox; `DELETE` revokes). The **Checkpoints tab** became
  a merged timeline — lifecycle transitions and checkpoints on one spine —
  fed by new endpoints `GET /v0/sandboxes/:id/events` and `GET /v0/events`
  (owner-scoped fleet feed; newest-first, id-cursor pagination, index in
  migration 0007), with a **time-travel composer** that runs a command
  with `checkpoint:true` so every step is a rollback anchor. Failed
  transitions now record their cause (`detail.error` jsonb) and surface as
  chips in the timeline. A **Settings tab** adds migrate-with-node-picker,
  the RECYCLED restore-artifacts flow, a geometry readout, and the danger
  zone; the Overview tab gained a recent-events card. The Preview iframe is
  sandboxed **without** `allow-same-origin` — the proxy is same-origin with
  the console, so granting it would let untrusted guest scripts read the
  bearer token from the console's `localStorage`; an opaque origin keeps
  the preview working while isolating it (see the Fixed section for the
  companion new-tab / credential-forwarding hardening).
- **Fleet cockpit (console phase 3)** — a dedicated **Nodes page** (capacity
  cards + a detail drawer listing each node's sandboxes). The **Sandboxes**
  list gained search, state-filter chips, sortable columns, and a per-row
  `⋯` menu (open/pause/resume/fork/kill with optimistic updates + toasts).
  **Overview** gained a legend-filtered fleet grid, a live activity feed
  (`GET /v0/events`), node cards linking to the Nodes page, and skeletons.
  **Storage** added a stored-bytes-by-tier stacked bar and sortable
  columns; **Templates** got a detail drawer (sandboxes-using-it) and a
  confirm dialog. The create-sandbox dialog was extracted so the Sandboxes
  page and the Overview empty state share it.
- **Command palette & polish (console phase 4)** — a `⌘K` command palette
  (cmdk) to navigate, fuzzy-jump to any sandbox, create, and act on the
  current workspace's sandbox; `g`-then-key navigation (`g o/s/n/t/g`), a
  `?` shortcut-help overlay, and a sidebar `⌘K` hint. A `check:offline`
  build step greps the built bundle for network-loading external URLs
  (CSS `url()`/`@import`, HTML `src`/`href`) and fails `make web` if the
  embed would ever fetch off-origin, enforcing the air-gap guarantee.

- **Web console** — a management UI embedded in the apiserver binary
  (`pkg/webui` + `web/`, React + TypeScript, served at `/` with the SPA
  fallback excluded from `/v0`): fleet heat map (every sandbox an ember on
  the lifecycle thermal ramp), node capacity, sandbox table with live
  memory gauges (base | effective | max), create dialog with resize
  ceilings + autoscale + egress, detail page (lifecycle verbs, resize
  slider, checkpoints/fork/rollback, in-guest exec, storage costs),
  templates and storage-report pages. Bearer-token login; fonts bundled
  (works air-gapped); dark operator theme. `GET /v0/nodes` added for the
  fleet view (nodes + live usage + active counts). Enterprise visual
  language — neutral slate surfaces, Inter, a single amber brand accent,
  flat semantic status badges, KPI stat tiles, dense tables. Built assets
  are committed so `go build` alone ships a working console; `make web`
  rebuilds them.

## [v0.6.0-m6] — 2026-07-10 — Runtime elasticity (M6) — **v0.3**

A sandbox's resources stop being fixed at create: memory grows and shrinks
at runtime through a virtio-mem hotplug region (real host reclaim,
mprotect-hard), CPU moves within its boot ceiling through the cgroup
`cpu.max` quota, an opted-in sandbox autoscales on guest-reported pressure,
and an explicit verb migrates a RUNNING sandbox across nodes. Decisions:
[ADR-0007](docs/adr/0007-m6-runtime-resize.md); base-technology comparison:
docs/zh/07.

### Added

- **Resize ceilings at create** — `POST /v0/sandboxes` accepts
  `max_memory_mib` / `max_vcpus` (0 = fixed geometry, fully opt-in). The
  VM boots with a virtio-mem region of `max − base` (128 MiB slot-rounded,
  starts unplugged and costs nothing) and `max_vcpus` cores clamped to
  `vcpus` by the cgroup quota; boot args gain
  `memhp_default_state=online_movable` so hot-UNplug actually reclaims.
- **Resize verb** — `POST /v0/sandboxes/{id}/resize {memory_mib?, vcpus?}`
  on a RUNNING sandbox: ceiling-validated, growth admission-checked
  against the node's oversold budget (`Scheduler.CanFit`; 409 with options
  when full — never a silent migration), guest-converged (shrink is
  cooperative and reports the achieved size), cgroups updated, achieved
  values written back to `sandboxes.memory_mib/vcpus`.
- **Autoscale** — `"autoscale": true` at create opts into the lifecycle
  engine's pressure loop: guestd `/healthz` now reports
  MemTotal/MemAvailable + PSI avg10; hysteresis policy (grow on <10%
  available or PSI>20 for 2 ticks, shrink toward the create-time base on
  >50% for 10 ticks, 30s cooldown, CanFit-deferred growth), thresholds
  config/env tunable (`EMBERVM_AUTOSCALE_*`).
- **Migrate verb** — `POST /v0/sandboxes/{id}/migrate {node_id?}`:
  RUNNING → pause (write-through) → release → warm-restore on the target
  → RUNNING (~2.3s in CI-class virt); PAUSED_HOT just moves its placement
  pointer (PAUSED_WARM). Default target excludes the current node
  (`PlaceExcluding`); explicit targets are CanFit-checked.
- **fcclient** — `PUT/PATCH/GET /hotplug/memory` wrappers; **uffd** —
  contiguous Zero-chunk backfill coalesced into ≤64 MiB `UFFDIO_ZEROPAGE`
  spans (a never-plugged region is not an ioctl storm); **metrics** —
  `embervm_resize_seconds/_total`, `embervm_autoscale_actions_total`,
  `embervm_migrations_total`.
- **e2e-m6 workflow** — four `--- PASS:`-guarded gates:
  `TestVirtioMemResizeKVM` (grow → dirty → shrink → chunked pause → uffd
  restore → resize AGAIN → hot-unplug → second snapshot round-trip),
  `TestResizeCPUQuotaKVM`, `TestAutoscaleMemoryKVM` (real tmpfs pressure
  end-to-end through engine + PG), `TestMigrateRunningKVM` (two jailed
  daemons).

### Changed

- **`sandboxes.memory_mib`/`vcpus` now mean CURRENT EFFECTIVE geometry**
  (they move with resize/autoscale and restore-time reconciliation);
  create-time values live in new `base_memory_mib`/`base_vcpus` columns
  (migration 0006, additive/idempotent). `NodeUsage` accounting is
  therefore live by construction.
- `SandboxStatus` reports effective `memory_mib`/`vcpus`; resume, fork,
  and rollback reconcile the PG row from it (a restore rewinds plug state
  to the checkpoint's — the node re-reads `GET /hotplug/memory` after
  every LoadSnapshot instead of trusting stale in-memory values).
- Snapshot descriptors carry `base_memory_mib`/`max_memory_mib`/
  `max_vcpus` (FormatVersion unchanged; old descriptors read as fixed
  geometry). Golden fast-create matches on the ceilings too — goldens are
  fixed-geometry, so resize-enabled creates cold-boot.
- The per-sandbox cgroup enables the `cpu` controller alongside `memory`;
  `memory.max` covers the declared ceiling (hard bound against a hostile
  guest driver, per the upstream virtio-mem trust model).

## [v0.5.0-m5] — 2026-07-09 — Agent-native fork/branch/rollback (M5) — **v0.2**

Sandboxes stop being single timelines: checkpoint a live sandbox, fork N
parallel branches from any checkpoint (tree-of-thought fan-out, RL
rollouts), roll back to any checkpoint, and auto-checkpoint every exec
step for time-travel replay. Decisions:
[ADR-0006](docs/adr/0006-m5-fork-branch.md).

### Added

- **Checkpoints** — `POST /v0/sandboxes/{id}/checkpoints {"tag"?}` names
  what every chunked pause already produces (memory layer + ZFS snapshot);
  `GET` lists the timeline (PG `checkpoints` table, migration 0005).
- **Fork** — `POST /v0/sandboxes/{id}/fork {"checkpoint"?}` branches a new
  quota-counted sandbox from a parent checkpoint without touching the
  parent: ZFS clone (disk CoW) + shared content-addressed chunks (memory
  CoW) + chunked hot resume (~fork = golden fast-create, which is now a
  thin wrapper over the same path). Omitted checkpoint = branch-now.
  Children carry `parent_id`/`forked_from`; same-node placement.
- **Rollback** — `POST /v0/sandboxes/{id}/rollback {"checkpoint"}`
  switches the sandbox back in place (`zfs rollback -r` + chain trim +
  the same hot resume the tiers use). Discards everything after the
  target including newer checkpoints; 409 while those have live forks.
- **Time-travel** — exec accepts `"checkpoint": true` (snapshot BEFORE
  the command; tag in the response); forking a step's tag replays it.
- **Lineage guards** — DELETE of a forked parent is 409 until children
  die; the TTL engine keeps fork parents HOT (children are ZFS clones of
  their snapshots); guest entropy reseeds per branch (VMGenID + resumed
  hook).
- **e2e-m5 workflow** — THE exit gate `TestFork10Branches` (10 parallel
  branches, parent's background counter never stalls), rollback and
  time-travel gates, node-verb gates.
- nodeapi verbs `Fork`/`Rollback`; `storage.Rollbacker`; metrics: fork
  under `embervm_create_seconds{path="fork"}`, rollback under
  `embervm_restore_seconds{tier="rollback"}`.

## [v0.4.0-m4] — 2026-07-09 — Elasticity & production hardening (M4) — **the open-source v0.1 release**

One node became a 3-node cluster you can lose a worker from: multi-node
scheduling with polled heartbeats and eviction, jailer-hardened Firecracker,
sub-500ms golden fast-create, 50 sandboxes per node, a WebSocket-transparent
gateway proxy, netns-level egress policy, and Prometheus observability.
G1–G6 acceptance: [docs/acceptance-v0.1.md](docs/acceptance-v0.1.md);
decisions: [ADR-0005](docs/adr/0005-m4-elasticity-hardening.md).

### Added

- **Multi-node scheduler** (`pkg/controlplane/scheduler.go`) — `nodes`
  registry in PostgreSQL, static membership (`apiserver --nodes
  id=socket,...` / `EMBERVM_NODES`), polled `Healthz` heartbeats with
  miss-threshold eviction (dead node's RUNNING sandboxes → FAILED, restore
  on demand), sticky-then-bin-pack placement budgeted on memory ×
  `MemOvercommit` and vCPUs × cores × `CPUOvercommit` (default 3x).
  `sandboxes.node_id` is the routing table.
- **Jailer hardening** — every Firecracker under chroot + per-VM uid/gid +
  netns + default seccomp; snapshot drive paths become chroot-relative,
  which retires M3's mountpoint pinning on jailed restores and unlocks
  fast-create (`--jailer-bin` on the node daemon).
- **Golden fast-create** — template build parks a paused golden sandbox;
  geometry-matched creates `zfs clone` it and hot-restore the memory image
  instead of cold-booting (P50 <500ms gate). Creates on nodes that never
  built the template receive the stream from L1 on demand.
- **Balloon + oversell** — virtio-balloon on boot, `SetBalloon` verb,
  balloon-assisted pause (`PauseBalloonSettle`); `TestConcurrency50` gates
  50 × 256 MiB on one node.
- **Zombie watchdog (G5)** — wait4(WNOHANG)-probed child liveness; a dead
  FC or uffd handler force-FAILs the sandbox, releases everything, and
  writes through to the control plane on the next heartbeat.
- **Gateway proxy** — `ANY /v0/sandboxes/{id}/proxy/{port}/*path`, two
  chained WebSocket-transparent reverse proxies (apiserver → node daemon
  unix socket → guest netns); owner-scoped.
- **Egress policy** — `POST /v0/sandboxes {"egress": "nat"|"none"}`;
  `none` blocks guest-originated forwarding at the root netns while the
  gateway and guestd keep working; persists across tiering and cross-node
  restore via the snapshot descriptor. (The zero-trust L7 egress proxy is
  deferred past v0.1 — ADR-0005.)
- **Prometheus observability** — `/metrics` on the API server and the node
  daemon socket: restore/create latency histograms (by tier/path),
  lifecycle transitions, chunk ops, proxy results, watchdog reaps, nodes-up,
  engine tick errors; starter dashboard in `deploy/grafana/embervm.json`.
- **e2e-m4 workflow** — THE exit gate `TestClusterKillNode` (3 jailed
  daemons, kill -9, eviction, cross-node recovery with continuity, RPO
  verified both ways, gateway proxied in-flow) + fast-create/concurrency
  gates.

### Changed

- `SnapshotDescriptor` gains `egress` (additive; old descriptors read as
  default NAT).
- `cmd/nodeagent` gains `--chunk-store-dir`, `--capacity-mib`,
  `--netns-base`, `--fc-version`, `--kernel-version`, `--jailer-bin`,
  `--jailer-chroot-base`.
- Jailed restores no longer pin the dataset mountpoint (chroot-relative
  paths hold on any node); the unjailed dev path keeps the M3 pinning.

## [v0.3.0-m3] — 2026-07-08 — Tiered archive & lifecycle engine (M3)

Paused sandboxes now decay automatically through storage tiers on
operator-set TTLs — HOT → WARM → COLD → RECYCLED — with resume from every
tier, wake-prediction pre-warming, artifacts-only recycling, and a
per-sandbox cost report.

### Added

- **Lifecycle engine** (`pkg/controlplane/engine.go`) — scans PostgreSQL
  on a tick and drives TTL transitions (`EMBERVM_TTL_WARM/COLD/RECYCLE`;
  zero disables). Race discipline: CAS-then-act — a user resume racing a
  demotion loses cleanly on the from-state; failing tier actions mark the
  sandbox FAILED, never silently retried. TTLs measure time in the
  current tier.
- **WARM** — `ReleaseLocal` frees the dataset, workdir, and netns lease
  after verifying L1 holds the restore descriptor (refused otherwise:
  that would be data loss, not tiering).
- **COLD** — a store-only archive: the memory chain compacts into a
  **synthetic full manifest** (`memsnap.Synthesize`, metadata-only —
  chunks are content-addressed, so zero bytes move; this retires the M2
  "diff chains grow unboundedly" debt and cold restores read exactly one
  memory layer); referenced chunks copy to the `EMBERVM_COLD_*` store via
  the dedup Copier; the L1 copy is deleted and swept by the new
  **mark-and-sweep chunk GC** (manifest-rooted, grace-windowed so
  in-flight pauses are safe).
- **RECYCLED** — `ExtractArtifacts` receives the archived disk chain,
  loop-mounts it read-only, and tars the sandbox's `artifact_paths` into
  `artifacts.tar.zst` (zstd); everything else is pruned and GC'd.
  `POST /v0/sandboxes/{id}/restore-artifacts` seeds a NEW sandbox from
  those artifacts (Manus-style selective restore).
- **Tier-aware resume** — `POST .../resume` transparently restores from
  L1 (WARM) or the cold store (COLD, uffd handler env re-pointed); a cold
  restore forces the next pause to root a fresh Full chain (the
  synthetic-full parent lives in L2 — one chain, one store), while the
  zfs disk delta chain continues across memory-chain restarts
  (descriptors grew `tier`/`disk_layers`/`snap_seq`).
- **Pre-warm prediction** (`pkg/prewarm`) — Serverless-in-the-Wild
  histogram policy over pause→resume intervals (P5 window, ≥3 samples,
  CV ≤ 2 — otherwise the TTLs act as fixed keep-alive); the engine pulls
  WS chunks node-local through the new `Prewarm` verb ahead of the
  predicted wake. Advisory: failures never block the scan.
- **Cost report** (成本报表) — `GET /v0/sandboxes/{id}/storage` and
  `GET /v0/storage-report`: logical vs stored bytes, unique chunks,
  stored ratio, artifact size — computed from manifests on demand.
- nodeapi verbs: `ReleaseLocal`, `RestoreSandbox(tier)`,
  `ExtractArtifacts`, `Prewarm` (interface + HTTP transport).
- CI: `e2e-m3` full-stack exit gate (REST + running engine + ZFS +
  warm/cold MinIO buckets + PostgreSQL): TTL-driven tier flow, **cold
  restore < 10s interactive** with continuity, archive cost gate
  (stored ≤ 60% of logical), recycle-to-artifacts, selective restore.
  Decisions in [ADR-0004](docs/adr/0004-m3-archive-lifecycle.md).

### Changed

- `POST /v0/sandboxes` accepts `artifact_paths` (guest paths preserved at
  recycle; default keeps nothing).
- Resume claims RESUMING via compare-and-swap first — clients may now see
  409 when racing a lifecycle transition (pre-1.0 API change).
- `nodeapi.Agent.RestoreSandbox` takes a tier argument (pre-1.0 break).

### Notes / deferred

- Disk synthetic full and node-local chunk-cache LRU eviction → M4;
  ARIMA wake prediction and rolling WS refresh → later; Xet-style
  CDC/Merkle repo unchanged → next storage evolution (ADR-0004).
- Latency gates CI-relative (nested virt, ADR-0001); bare-metal
  re-measurement remains a tracked debt.

## [v0.2.0-m2] — 2026-07-08 — Second-level restore pipeline (M2)

Snapshots become chunked, compressed, content-addressed, layered objects
that write through to an S3-compatible L1 store on pause; restores are
working-set-first and any node can rebuild a paused sandbox from L1 alone.

### Added

- **Chunked snapshot format** (`pkg/memsnap`) — 16 KiB fixed chunks,
  SHA-256 content addressing over uncompressed bytes, per-chunk lz4 (raw
  when incompressible), all-zero chunks recorded but never stored. Layered
  manifests (one Full root + Diff layers) carry `snapshot_format_version`
  and `(fc_version, kernel_version)`; `Resolve()` flattens newest-wins.
  Partially-dirty diff chunks are **merged with the parent chain at write
  time** — recording the raw sparse diff would clobber clean pages with
  hole-zeros (diff extraction is linux-only; staging must be ext4/tmpfs).
- **Chunk store** (`pkg/chunkstore`) — content-addressed local dir (L0) +
  S3-compatible L1 (minio-go; MinIO in CI, Garage/Hetzner OS in
  production) configured via `EMBERVM_L1_*`; `Tiered` local-first reads
  with write-through; parallel `Copier` that only moves missing chunks.
- **uffd handler `--mode chunked`** — faults populate whole chunks
  (fetch → lz4 decode → `UFFDIO_COPY`, `UFFDIO_ZEROPAGE` for zero chunks,
  correct across region straddles/PCI hole); first resume records the
  working set (first-touch order, REAP), later resumes eagerly prefetch
  the trace then backfill in the background while faults keep priority
  (FaaSnap) — fixing the M0 finding that whole-file prefetch loses
  cold-cache at 4/8 GiB. `--parent-pid` watchdog. Raw `lazy`/`prefetch`
  modes frozen.
- **Diff snapshot chain** — `track_dirty_pages` at boot and across every
  snapshot load; first pause Full, later pauses Diff; dataset snapshots
  share the layer tag; the raw memfile is deleted after chunkify.
- **Pause write-through** — chunks, manifest, snapfile, `ws.json`, a
  `zfs send -c` disk delta (first delta from the clone origin), and a
  `snapshot.json` restore descriptor go to L1 on every pause; a pause
  that cannot reach L1 fails (RPO contract, docs/zh/02 §3).
- **Cross-node restore** — `RestoreSandbox` rebuilds a sandbox from L1 on
  a node that never saw it: template distributed as a `zfs send` stream
  (GUID lineage — locally rebuilt templates cannot receive delta chains),
  disk chain via `zfs receive -o origin=`, dataset mountpoint pinned to
  the origin node's path (snapfiles record absolute drive paths), memory
  faulted from L1 through the tiered store. `storage.Replicator` (ZFS
  only; plain backend returns `ErrReplicationUnsupported`).
- **Correctness hooks** — snapshot loads pass `clock_realtime` (校时);
  guestd gains `POST /resumed` (bumps a `resumes` counter in `/healthz`,
  runs `/etc/embervm/resume-hook` if present); VMGenID reseeds the guest
  RNG by construction on FC ≥1.16.
- CI: `e2e-m2` exit gate (loop ZFS pool + MinIO): hot restore P50 <500ms,
  warm restore (cold chunk cache, L1-backed) P99 <3s, cross-node
  pause→upload→restore chain, 3-layer diff-chain correctness with
  diff <25% of full, dedup report. All gates grep `--- PASS:`; the
  chunked uffd core is proven to run against a real userfaultfd in
  lint-unit. Decisions in [ADR-0003](docs/adr/0003-m2-restore-pipeline.md).

### Changed

- **Snapshot on-disk format**: pause now produces `layer-p<N>.json` +
  content-addressed chunks instead of a raw `memfile` when
  `restore_mode=chunked` (pre-1.0 format break, per docs/zh/05 §4;
  `prefetch` remains the default until deploy configs flip).
- Seq-continuity assertions (fc-restore.sh, KVM tests) are now
  monotone-above-snapshot instead of exact `+1` — the exact check flaked
  when a probe's client timed out after the server counted it.

### Notes / deferred

- Template memfd sharing / uffd minor faults (doc 04 #3) → M3 density
  work; FastCDC/Blake3 Merkle repo + synthetic-full diff compaction →
  M3 (diff chains grow until then); rolling WS refresh → M3.
- Exit-criteria numbers are CI-relative (nested virt, ADR-0001);
  bare-metal re-measurement remains a tracked debt.

## [v0.1.0-m1] — 2026-07-08 — Single-node MVP (M1)

First end-to-end single-node sandbox cloud: build a template from a Docker
image and run the full microVM lifecycle over a REST API, on one cloud VM.

### Added

- **REST control plane** (`cmd/apiserver`, `pkg/controlplane`) — Gin + PostgreSQL.
  Template + sandbox CRUD, `pause`/`resume`/`snapshot`/`kill`, and guest
  `exec`/`files` proxy under `/v0`. Bearer-token auth with per-token
  `max_sandboxes` quota. PostgreSQL is the single source of truth; every
  lifecycle transition is logged to `sandbox_events`.
- **Node agent** (`cmd/nodeagent`, `pkg/nodeagent`) — boots Firecracker microVMs
  in per-sandbox network namespaces, drives create/stop/pause/resume/snapshot,
  and reaches the guest through a setns dialer. Hot resume uses the M0
  uffd-handler (prefetch). Best-effort cgroup v2 placement.
- **Template builder** (`pkg/template`) — daemonless Docker/OCI image → ext4
  rootfs (crane flatten → tar-slip-safe extraction → guestd injection →
  `mkfs.ext4`).
- **guestd** (`cmd/guestd`) — in-guest PID 1: mounts, orphan reaping, and an
  HTTP daemon for exec / file R/W / health (per-process restore-continuity seq).
- **Storage** (`pkg/storage`) — ZFS clone/snapshot backend
  (`recordsize=16k`, `primarycache=metadata`, sparse `data.raw`, never a zvol)
  plus a plain-directory backend for dev/tests.
- **`embervm dev`** (`cmd/embervm`) — the whole stack (migrations + API + in-proc
  agent) in one root process; `deploy/singlenode/install.sh` provisions a node
  on stock Ubuntu 24.04.
- Node contract `pkg/nodeapi.Agent` (in-proc for `embervm dev`, HTTP-over-UDS
  for a standalone node), lifecycle state machine (`pkg/lifecycle`), and
  Firecracker API client (`pkg/fcclient`).
- CI: `controlplane-pg` (Postgres), `nodeagent-smoke` + `dev-smoke` (KVM),
  `e2e-m1` exit-criteria gate (20 concurrent; hot resume <1s with a 15 GiB data
  disk; installer provisioning). Decisions recorded in
  [ADR-0002](docs/adr/0002-m1-control-plane-shape.md).

### Security

- Authentication **fails closed**: the server refuses to start without a
  `--tokens-file` unless `--insecure-dev-token` is explicitly passed.
- Sandbox operations are **owner-scoped**: a caller gets 404 on another
  tenant's sandbox on every verb (create/get/list/exec/files/…).
- Template tar extraction is hardened against symlink path traversal
  (tar-slip) via scoped resolution (`filepath-securejoin`).
- `install.sh` generates a random PostgreSQL password and API token, stored
  root-only under `/etc/embervm`; no credential is hardcoded.

### Notes / deferred

- Jailer host-hardening (chroot, per-VM uid/gid, seccomp) is deferred to M4;
  M1 isolates with per-sandbox networking + cgroup v2 (see ADR-0002).
- Templates are node-global in M1 (no per-tenant ACL yet).
- uffd chunk store / diff snapshots / working-set prefetch are M2.
- Exit-criteria numbers are CI-relative (nested virtualization, ADR-0001);
  bare-metal re-measurement is a tracked follow-up.

## [m0-baseline] — 2026-07-07 — Environment & prototype baselines (M0)

- Firecracker boot/jailer/netns, snapshot→uffd resume baselines (file /
  uffd-lazy / uffd-prefetch), UFFDIO_COPY throughput sweep, and ZFS raw-file vs
  zvol comparison, published as a reproducible report from GitHub Actions CI.
  Methodology frozen in [ADR-0001](docs/adr/0001-m0-baseline-methodology.md).
