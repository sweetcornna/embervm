# Changelog

All notable changes to EmberVM are recorded here. The project is pre-1.0:
the `v0.x` REST API and on-disk/snapshot formats **may break between minor
versions**, and every break is listed here (docs/zh/05 §4). Format loosely
follows [Keep a Changelog](https://keepachangelog.com/); versions follow
[SemVer](https://semver.org/).

## [Unreleased]

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
