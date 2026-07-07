# ADR-0001: M0 baseline benchmark methodology

- Status: Accepted
- Date: 2026-07-07

## 中文摘要

M0 基线测量三种恢复模式：`file`（Firecracker 原生文件加载）、`uffd-lazy`（纯缺页懒加载）、`uffd-prefetch`（后台顺序 UFFDIO_COPY 与按需缺页并发，作为 M2 工作集预取的 FaaSnap 式上界近似）。三个计时点均取宿主机 CLOCK_REALTIME 墙钟：T0 = 发起 `PUT /snapshot/load` 之前，T1 = 该请求返回 204（`t_api_ms = T1-T0`），T2 = probe-client 首次成功完成 guest TCP 探测往返（`t_interactive_ms = T2-T0`）。进程连续性由 guest 内 probe-server 的进程内原子计数器证明：快照在 seq=1 之后拍摄，每次恢复必须观测到 seq = 快照时序号 + 1（`seq_ok`），证明是同一进程恢复而非重启。每次迭代使用全新 Firecracker 进程与全新 per-VM 工作目录，netns 串行复用；cold 在每次迭代前执行 `sync` + `drop_caches(3)`，warm 不清缓存。脏内存通过内核参数 `ember.dirty_mb` 让 probe-server 在监听前用 xorshift 非零数据填充 N MiB，避免 memfile 稀疏/全零而虚高成绩。统计使用 pkg/benchstat 的线性插值 P50/P90/P99，每格 ≥15 次迭代。环境政策（关键）：所有 CI 数字来自共享 GitHub 托管 runner 上的嵌套虚拟化，仅对模式间相对比较有效；绝对数字与可写入 README 的结论必须在裸金属复测（推迟至 M1），每份报告嵌入环境三元组（虚拟化类型/供应商、CPU 型号、内核）。内存档位 1/2/4GB 为核心档，8GB 尽力而为（余量不足自动跳过），16GB 推迟到裸金属。范围限定：M0 仅在引导路径中演练 jailer（快照/恢复不带 jail，jailed 恢复属 M1 加固）；ZFS 对比使用 loop 文件 vdev，仅作功能参考。资产锁定：Firecracker v1.16.1 + v1.15.1 矩阵，guest 内核 vmlinux-6.1.155 与 ubuntu-24.04 squashfs 取自 firecracker-ci S3 bucket，sha256 锁文件为 `scripts/assets.sha256`。

## Context

M0's purpose is to fill the honest data gaps identified in the research phase (docs/zh/04 §9) with numbers we measured ourselves, on infrastructure anyone can reproduce for free (GitHub Actions `ubuntu-latest`, which exposes `/dev/kvm`). To make those numbers trustworthy — and to make it explicit which of them are *not* trustworthy for absolute claims — the measurement methodology must be fixed before any results are collected. This ADR freezes that methodology.

## Decision

### Restore modes measured

Three restore paths, identical guest and snapshot, differing only in how memory reaches the restored microVM:

1. **file** — Firecracker's native snapshot load with `mem_backend.backend_type = File`. The VMM maps the memfile itself; the host page cache does the paging.
2. **uffd-lazy** — `mem_backend.backend_type = Uffd` with our handler in pure demand-paging mode: every guest page fault is served individually with `UFFDIO_COPY` from the memfile. This is the known pathological baseline (the "lazy trap").
3. **uffd-prefetch** — same handler, plus a background thread performing sequential `UFFDIO_COPY` over the whole memfile concurrent with demand paging (faulted ranges are skipped via a shared rangeset). This is a FaaSnap-style *upper-bound approximation* of working-set prefetch: it shows the headroom that a real recorded-working-set prefetcher (REAP-style, planned for M2) can exploit, without yet having working-set records.

### Timing points

All timestamps are host-side wall clock (CLOCK_REALTIME, `date +%s%N` / Go `time.Now()`); no guest clocks are trusted for latency math.

- **T0** — immediately before issuing `PUT /snapshot/load` on the Firecracker API socket.
- **T1** — receipt of its `204` response. `t_api_ms = T1 - T0`. This is the VMM-visible restore cost (state file parse, memory backend attach, vCPU resume).
- **T2** — first successful guest TCP probe round-trip, taken from probe-client's `success_unix_ns` (the client retries in a tight loop until the guest's probe-server on 172.16.0.2:7777 answers). `t_interactive_ms = T2 - T0`. This is the user-visible metric: "how long until the sandbox responds again".

`t_api_ms` and `t_interactive_ms` diverge sharply for uffd modes (the 204 arrives before most memory exists); reporting both is mandatory.

### Process-continuity proof (`seq_ok`)

A fast reboot is indistinguishable from a restore by latency alone, so every measurement must prove the *same process* resumed. The guest probe-server keeps a per-process atomic counter: each accepted connection receives the incremented value (first connection after process start = 1). The benchmark takes the snapshot only after observing seq = 1; after every restore, the first probe must return exactly `seq_at_snapshot + 1` (i.e. 2, and monotonically increasing across repeated probes of the same restored VM). A restore whose first observed seq is 1 means the guest rebooted and the iteration is invalid (`seq_ok = false`, iteration discarded and the run fails loudly).

### Iteration isolation

- Fresh Firecracker process and fresh per-VM work dir (`$WORK_DIR/vm$N`) for every iteration; nothing is reused across iterations except the snapshot artifacts under `$WORK_DIR/snap$N` (which are read-only inputs).
- The network namespace (`ember$N`, TAP 172.16.0.1/30, guest fixed at 172.16.0.2) is created once and reused *serially*; iterations never overlap.
- **cold** iterations: `sync` followed by `echo 3 > /proc/sys/vm/drop_caches` immediately before T0, so the memfile and snapfile are read from disk, not page cache.
- **warm** iterations: nothing is dropped; the page cache state left by prior iterations is deliberately kept.

### Dirty-memory realism (`ember.dirty_mb`)

A freshly booted minimal guest has a memfile that is mostly zero pages, which snapshot/restore paths handle disproportionately well (sparse files, zero-page shortcuts) and which would flatter every number. The kernel cmdline token `ember.dirty_mb=D` instructs probe-server to allocate and fill D MiB with non-zero xorshift-generated data *before* it starts listening — so by the time the snapshot is taken, the memfile contains D MiB of incompressible, non-sparse payload. The default benchmark cell uses 256 MiB.

### Statistics

Latencies are aggregated as P50 / P90 / P99 using `pkg/benchstat` (percentile by linear interpolation between order statistics). Minimum 15 iterations per cell (mode × cold/warm × memory size × dirty size); fewer iterations fail the run rather than producing a report with meaningless tail percentiles.

### Environment policy (critical)

All M0 CI numbers are produced on **nested virtualization on shared GitHub-hosted runners** (KVM inside an Azure VM, noisy neighbors, unspecified CPU generation). Consequently:

- CI numbers are valid **only for relative comparisons between modes** measured in the same run (e.g. "prefetch beats lazy by N×").
- **Absolute latencies, and any figure quoted in the README or external material, require bare metal.** Bare-metal re-measurement is deferred to M1.
- Every generated report embeds the environment triple — virtualization type/provider, CPU model, host kernel — (`results/env.json`, surfaced in REPORT.md) so no number can be read without its provenance.

### Memory tiers

Guest memory sizes 1 / 2 / 4 GB are the core matrix. 8 GB is best-effort: the runner has 16 GB RAM and 14 GB free SSD, so the 8 GB cell auto-skips (recorded as skipped, not failed) when headroom (RAM or disk for the memfile) is insufficient. 16 GB is deferred to bare metal.

### Scope notes

- **jailer**: exercised in the boot path only in M0 (a jailed boot is part of the smoke gate). Snapshot and restore run unjailed; jailed restore (chroot'd memfile/UDS paths, uid/gid drop interactions with the uffd handler socket) is M1 hardening work.
- **ZFS comparison** (`test/bench/zfs-compare.sh`): uses a loop-file vdev on the runner's SSD, so its throughput numbers reflect the loop device, not NVMe. It is a *functional* reference (clone/snapshot semantics, O(1) clone verification) only, and exits 0 with a skipped marker where the ZFS module cannot load.

### Asset pinning

- Firecracker releases **v1.16.1** (primary) and **v1.15.1** (compat) form the CI matrix.
- Guest kernel **vmlinux-6.1.155** and **ubuntu-24.04 squashfs** are fetched from the upstream `firecracker-ci` S3 bucket.
- All downloaded assets are pinned in the sha256 lockfile **`scripts/assets.sha256`**; CI cache keys derive from its hash, so changing an asset invalidates caches everywhere at once.

## Consequences

- Numbers between modes are comparable and reproducible by anyone with a free GitHub account; nobody can accidentally quote a nested-virt number as an absolute claim, because the report format brands every figure with its environment.
- The prefetch mode overstates what a real working-set prefetcher will achieve on cold object-storage-backed restores (it reads a local memfile sequentially); M2 must re-baseline against this ADR when recorded working sets land.
- Adding a benchmark dimension (e.g. 16 GB tier, jailed restore, real NVMe ZFS) requires a follow-up ADR or an amendment here, not a silent script change.
