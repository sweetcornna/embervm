# Changelog

All notable changes to EmberVM are recorded here. The project is pre-1.0:
the `v0.x` REST API and on-disk/snapshot formats **may break between minor
versions**, and every break is listed here (docs/zh/05 §4). Format loosely
follows [Keep a Changelog](https://keepachangelog.com/); versions follow
[SemVer](https://semver.org/).

## [Unreleased]

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
