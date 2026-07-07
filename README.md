# EmberVM (余烬)

[![lint-unit](https://github.com/sweetcornna/embervm/actions/workflows/lint-unit.yml/badge.svg)](https://github.com/sweetcornna/embervm/actions/workflows/lint-unit.yml)
[![integration-kvm](https://github.com/sweetcornna/embervm/actions/workflows/integration-kvm.yml/badge.svg)](https://github.com/sweetcornna/embervm/actions/workflows/integration-kvm.yml)
[![bench](https://github.com/sweetcornna/embervm/actions/workflows/bench.yml/badge.svg)](https://github.com/sweetcornna/embervm/actions/workflows/bench.yml)

> *The sandbox cloud that never goes cold.*

[中文文档 / Chinese README](README.zh-CN.md) · License: **AGPL-3.0** · Status: **Pre-alpha (design & research phase)**

**EmberVM** is a self-hostable, open-source sandbox cloud — every AI-agent session or cloud-dev workload gets its own isolated "cloud computer" (Firecracker microVM), comparable to E2B / Manus sandboxes / Cursor Cloud, with capabilities they don't offer out of the box:

- **Second-level resume** — hot resume P50 < 500ms; warm resume with a 10–20GB persistent data disk P99 < 3s to interactive
- **Persistent storage as a first-class citizen** — 10–20GB data disk per sandbox, full memory+disk state preserved across pause/resume, zero data loss (write-through to object storage, RPO ≤ 5min)
- **Tiered cold archive** — hot (local NVMe) → warm (object storage) → cold (low-cost tier) → recycled (artifacts only), automatic lifecycle, cold tier ≤ $5/TB/month
- **Elastic compute** — sandbox creation < 500ms, 50+ concurrent sandboxes per node, horizontally scalable worker pool
- **Zero vendor lock-in** — plain Linux + PostgreSQL + Redis + any S3-compatible store; no Nomad/Consul/GCP coupling (the main pain point of self-hosting E2B)

## How it works (one paragraph)

Copying 15GB over 10GbE takes 12s — whole-copy designs can never be "second-level". The industry-proven answer (E2B, CodeSandbox, Lambda SnapStart, Cursor) is: **O(1) copy-on-write clone + lazy page loading + working-set prefetch + moving only diffs**. EmberVM combines Firecracker (KVM hard isolation + native snapshots), local NVMe ZFS (dataset + raw file, O(1) clone, incremental `send -i`), a userfaultfd memory handler with REAP-style working-set prefetch (pure lazy faulting is a trap: 43MB/s vs 533MB/s with prefetch), 8–16KiB lz4-compressed memory chunks, and a FastCDC content-addressed chunk store (Garage/SeaweedFS or any S3) with lifecycle tiering. CodeSandbox has proven this class of design in production: resume avg 400ms / P99 2s.

```
Client ─▶ Gateway ─▶ API Server ─▶ Scheduler        (control plane: PostgreSQL + Redis)
                                      │ gRPC
              Worker (bare metal) ×N: Node Agent ─▶ Firecracker ×50 (guestd inside)
                │ uffd handler (lazy memory + WS prefetch, one per VM)
                └ local NVMe ZFS (L0 hot tier)
                      │
              L1: chunk store, FastCDC dedup (Garage/SeaweedFS/any S3) ─▶ L2: cold archive
```

Sandbox lifecycle: `RUNNING → PAUSED-HOT → PAUSED-WARM → ARCHIVED-COLD → RECYCLED`, with placement stickiness and histogram-based wake-up prediction (pre-fetch snapshots before the user returns).

## Try it on a cloud server

Firecracker needs `/dev/kvm` — bare metal or nested virtualization. Verified matrix (2026-07):

| Environment | Works? | Entry cost |
|---|---|---|
| Hetzner dedicated / Server Auction | ✅ bare metal (benchmark-grade) | €25–40/mo |
| GCP (Intel series, `--enable-nested-virtualization`) | ✅ nested | Spot ~$14–20/mo |
| AWS C8i/M8i/R8i (`NestedVirtualization=enabled`, since 2026-02) | ✅ nested | c8i.large ~$68/mo |
| Azure Dv3+ (security type must be Standard, not Trusted Launch) | ✅ nested | D2s_v5 ~$70/mo |
| DigitalOcean droplets | ✅ nested (smoke tests) | $6/mo |
| GitHub Actions `ubuntu-latest` (x86_64) | ✅ KVM available — free CI with real microVMs | free |
| Hetzner Cloud VMs, Linode, standard Alibaba ECS / Tencent CVM | ❌ no nested virt | — |

Full guide: [docs/zh/06-云服务器实测指南.md](docs/zh/06-云服务器实测指南.md) (English version to follow).

## Documentation

English docs will be added incrementally with the code. The full research & design series (Chinese) — three research rounds, all key decisions validated against primary-source data:

| Doc | Contents |
|---|---|
| [01 调研报告](docs/zh/01-调研报告.md) | Industry teardown: E2B architecture, Manus & Cursor Cloud internals, Modal/Morph/Daytona/Fly/Cloudflare/CodeSandbox comparison, Firecracker snapshot mechanics, storage & archive tech |
| [02 技术架构设计](docs/zh/02-技术架构设计.md) | Component design, lifecycle state machine, 3-tier restore paths, network/security, technology selection |
| [03 立项计划书](docs/zh/03-立项计划书.md) | Goals & metrics, milestones (M0–M5), team, provider cost comparison, consolidated risk register, acceptance |
| [04 创新与最佳实践](docs/zh/04-创新与最佳实践.md) | **Data-validated verdicts (authoritative on conflicts)** + frontier survey (REAP/FaaSnap/DeltaBox/Sabre…), 12 ranked innovations, latency budget model, host hardening checklist, reliability SOPs, tiered-archive economics |
| [05 开源项目规划](docs/zh/05-开源项目规划.md) | Naming, AGPL-3.0 + CLA strategy, repo layout, versioning, governance, positioning vs E2B/Daytona |
| [06 云服务器实测指南](docs/zh/06-云服务器实测指南.md) | Verified nested-virt matrix per provider, test topologies, single-node walkthrough, CI strategy with KVM on GitHub Actions |

## Roadmap

- [x] Research & project chartering (3 rounds: product teardowns → frontier innovations → primary-data validation)
- [x] Open-source planning: AGPL-3.0, bilingual docs, cloud test matrix
- [x] M0 (wk 1-2): bare-metal + nested-virt environments; Firecracker/ZFS/uffd prototype baselines (fills the honest data gaps listed in doc 04 §9) (done: baseline on nested-virt CI; bare-metal re-run planned for M1)
- [ ] M1 (wk 3-6): single-node MVP — full lifecycle API, template builder (Docker image → microVM), `embervm dev` single-process mode
- [ ] M2 (wk 7-10): second-level restore pipeline — uffd + WS prefetch, diff snapshots, chunk store
- [ ] M3 (wk 11-13): tiered archive & lifecycle engine, selective restore
- [ ] M4 (wk 14-16): multi-node scheduling, gateway, hardening → internal MVP
- [ ] M5 (optional): VM fork/branch API for agents (tree-of-thought / RL rollouts / time-travel debugging)

Realistic path to a chargeable beta: 4–6 months (the last 70% is network isolation, scheduling, and reliability hardening).

## Benchmarks & methodology

Performance numbers in this README are only ever quoted from bare-metal measurements (per the docs/zh/06 environment policy) — never from shared CI. The `bench` workflow produces a weekly CI baseline report (nested virtualization; valid for relative mode-vs-mode comparison only) as a build artifact. Methodology: [docs/adr/0001-m0-baseline-methodology.md](docs/adr/0001-m0-baseline-methodology.md). Reproduce locally on any Linux with KVM: `test/integration/smoke.sh`, then `test/bench/restore-matrix.sh`.

## Contributing & License

Contributions welcome once the M0/M1 scaffolding lands — watch Issues/Milestones. Contributor terms: DCO + CLA (dual-licensing reserved). Code reuse policy: Apache-2.0 sources (Firecracker, E2B's Apache parts) may be incorporated with attribution; BSL-licensed code is design-reference only, never copied.

Licensed under **AGPL-3.0** — if you modify EmberVM and offer it as a network service, you must share your changes.
