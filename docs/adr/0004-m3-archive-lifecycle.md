# ADR-0004: M3 tiered archive & lifecycle engine

- Status: Accepted
- Date: 2026-07-08

## 中文摘要

M3 让暂停的沙箱按可配置 TTL 自动流转 HOT→WARM→COLD→RECYCLED(docs/zh/02 §3)。**层级语义**(D1):HOT = 本机 + L1 全量;WARM = 仅 L1(节点资源全释放);COLD = 仅冷层(synthetic full);RECYCLED = 仅 artifacts.tar.zst。除 RECYCLED 外每层都可 resume;RECYCLED 对原沙箱终态,其 artifacts 灌入新沙箱(选择性恢复)。**生命周期引擎**(D2):控制面 goroutine 按 tick 扫 PostgreSQL,**CAS 先行再执行动作**——用户 resume 与引擎降级竞态时,CAS 输家干净退出;动作失败标 FAILED 带错误,绝不静默重试;TTL 以进入当前层的时刻(updated_at)计。**WARM 释放**(D3):校验 L1 有恢复描述符后销毁 dataset/工作目录/netns 租约;共享内容寻址的本地 chunk 缓存不按沙箱清除(LRU 驱逐属 M4)。**COLD 归档**(D4):纯存储面操作——内存链 `memsnap.Synthesize` 元数据级合成全量(chunk 零搬移,落掉 M2 的"diff 链无界增长"债;冷恢复只读一层),引用 chunk 经去重 Copier 搬到冷层,snapfile/ws/磁盘增量链原样搬移,描述符改写 tier=cold 后删除 L1 副本;磁盘 synthetic full 需要活 dataset,继续留链(zfs receive 顺序重放,记 M4)。**chunk GC**(D5):以 sandboxes/*/layer-*.json 为根的 mark-and-sweep,先列 manifest 后列 chunk,宽限窗保护"chunk 已传、manifest 未传"的进行中 pause。**RECYCLED**(D6):节点收链、只读 loop 挂载(noload)、打包指定 guest 路径为 zstd tar 存冷层,删除其余对象并 GC 双store;`POST /restore-artifacts` 以同模板新建沙箱、宿主侧解压后经 guestd 灌回(guest busybox tar 无 zstd)。**冷恢复链重启**(D7):synthetic full 的父块在冷层,若继续 Diff 会把链劈到两个 store——冷恢复后下一次 pause 强制 Full 重开链;磁盘增量链跨内存链重启延续(descriptor 分列 disk_layers + snap_seq 防 tag 冲突)。**预热**(D8):移植 Serverless in the Wild——pause→resume 间隔直方图,≥3 样本且 CV≤2 才预测(P5 窗口),引擎在预测唤醒前 lead 经节点 Prewarm 动词把 WS chunk 拉回本地缓存,pause 时清 prewarmed_at;历史稀疏/嘈杂回退固定 keep-alive(TTL 本身),ARIMA 推迟;预解压不做(lz4 5GB/s 非瓶颈)。**成本报表**(D9):按需从 manifest 算 logical/stored/去重比,分层出账;不缓存。**退出门禁**(D10):全栈 e2e(真实 REST + 引擎 + ZFS + 双 MinIO 桶 + PG)驱动完整层链,冷恢复 <10s 可交互 + seq 连续,归档 stored/logical ≤0.6 为"成本达标"的 CI 相对代理,RECYCLE 后仅剩 artifacts,选择性恢复经新沙箱验证;`--- PASS` 防伪绿。

## Context

M2 made snapshots portable objects with an L1 write-through, but nothing ever *left* a node's
working set or the L1: paused sandboxes kept their dataset, workdir, and netns forever, diff
chains grew without bound, and the only storage bill was "everything, twice". M3's charter
(docs/zh/03 §3) adds the economics: TTL-driven tier transitions, a cold layer, wake-prediction
pre-warming, artifacts-only recycling, and a cost report — with exit criteria 冷归档恢复 <10s
可交互 and 归档成本达标. This ADR records how the tiers map onto the M2 object model (D1–D10).

## Decision

### D1 — Tier semantics: what each state means for bytes

| State | node-local (L0) | warm store (L1) | cold store (L2) |
|---|---|---|---|
| PAUSED_HOT | dataset + workdir + chunk cache | everything (M2 write-through) | — |
| PAUSED_WARM | nothing | everything | — |
| ARCHIVED_COLD | nothing | nothing | synthetic full + chain objects |
| RECYCLED | nothing | nothing | artifacts.tar.zst only |

`PAUSED_HOT→PAUSED_WARM→ARCHIVED_COLD→RECYCLED`, resume legal from every tier except
RECYCLED (terminal for that sandbox; selective restore seeds a NEW one). Tiers may not be
skipped; WARM/COLD may FAIL like any active state.

### D2 — Lifecycle engine: CAS-then-act, TTLs per tier, failures are loud

A control-plane goroutine scans PostgreSQL each tick. The state change is a compare-and-swap
(`UPDATE ... WHERE state=from`) taken BEFORE the tier action: a user resume racing a demotion
either wins the CAS (transition skipped) or observes the new tier and takes the restore path —
the REST resume claims RESUMING through the same CAS. A tier action that fails marks the
sandbox FAILED with the error; nothing silently retries. TTLs measure time in the *current*
tier (updated_at is stamped on every transition); zero disables a transition, so plain
deployments archive nothing until the operator opts in (`EMBERVM_TTL_*` env).

### D3 — WARM releases the node, not the chunk cache

`ReleaseLocal` verifies L1 holds the restore descriptor (releasing without it is data loss —
refused), then destroys the dataset, removes the workdir, releases the netns lease, and drops
the in-memory entry. The node-local chunk cache is deliberately untouched: chunks are
content-addressed and shared across sandboxes; per-sandbox eviction would be wrong and global
eviction is an LRU policy that belongs with M4's multi-node work.

### D4 — COLD = synthetic full + move; a store-only operation

Archiving never touches a node. The engine reads the layer manifests from L1, resolves the
chain, and `memsnap.Synthesize`s ONE full-kind manifest — metadata-only, zero chunk movement,
because chunks are content-addressed. This lands the M2 debt ("diff chains grow unboundedly"):
a cold restore reads exactly one memory layer. Referenced chunks copy L1→L2 through the
dedup-aware Copier; the newest snapfile, the WS trace, and the disk delta chain move as-is;
the descriptor is rewritten (`tier: cold`, `layers: ["cold"]`, disk_layers/snap_seq
preserved); only then is the L1 copy deleted and GC'd. A disk synthetic full would need a live
dataset (`zfs send` full of the latest snapshot), so the disk keeps its delta chain — replayed
sequentially by `zfs receive` on restore — noted for M4.

### D5 — Chunk GC: mark-and-sweep with a grace window

Roots are every `sandboxes/*/layer-*.json` in the store; anything they don't reference is
swept. Two safety properties: manifests are listed before chunks, and unreferenced chunks
younger than the grace window (default 1h) survive — an in-flight pause uploads chunks before
its manifest, and the grace preserves that ordering. Runs after archive (on L1) and recycle
(on both stores), and is invocable standalone.

### D6 — RECYCLED keeps artifacts, deletes everything else

`artifact_paths` (set at sandbox creation) name the guest paths worth keeping. The engine has
a node `ExtractArtifacts`: receive the disk chain from cold (template lineage from L1 — never
archived), loop-mount rootfs.ext4 and data.raw read-only (`noload`), tar the requested paths
(missing ones skipped) into `artifacts.tar.zst` (zstd, pure-Go), destroy the scratch dataset.
The engine then prunes every cold object except the tarball and GCs. Selective restore
(`POST /v0/sandboxes/{id}/restore-artifacts`) creates a NEW sandbox from the same template and
untars host-side-decompressed artifacts through guestd — the guest's busybox tar has no zstd.

### D7 — Cold restore resets the memory chain, not the disk chain

The synthetic full's chunks live only in L2. Diffing the next pause against it would split one
chain across two stores, so a cold restore sets force-Full: the next pause roots a fresh Full
chain written through to L1, and tier semantics stay crisp (one chain, one store). The zfs
delta chain has no such problem — the received dataset carries all prior snapshots — so
descriptors split `disk_layers` from `layers`, and `snap_seq` keeps tags collision-free across
restarts. The uffd handler of a cold restore gets its child env re-pointed
(EMBERVM_COLD_* → EMBERVM_L1_*) so faults and WS prefetch pull from the cold store.

### D8 — Pre-warm: histogram prediction, conservative by design

`pkg/prewarm` ports the Serverless-in-the-Wild policy: per-sandbox pause→resume intervals
(from sandbox_events), predict the next wake at P5 of the distribution — but only with ≥3
samples and CV ≤ 2. Inside `[predicted-lead, predicted+lead]` the engine calls the node's
`Prewarm` verb: pull the WS-listed chunks (all referenced chunks when no WS exists) from the
tier store into the node-local cache, then stamp `prewarmed_at` (cleared on the next pause).
Sparse or noisy history predicts nothing — the TTLs are the fixed keep-alive fallback; ARIMA
is deferred. Pre-DEcompression is skipped on purpose: lz4 decodes at ~5 GB/s/core and is not
the restore bottleneck. Prewarm failures are advisory (logged, never block the scan) — it is
an optimization, and the restore path works from a cold cache regardless.

### D9 — Cost report: computed, not cached

`GET /v0/sandboxes/{id}/storage` and `GET /v0/storage-report` read the manifests of the tier
the sandbox lives in and report logical bytes (Σ uncompressed newest-wins view), stored bytes
(Σ compressed unique chunks), chunk count, stored/logical ratio, and artifact size for
RECYCLED. On-demand computation is exact and cheap at current scale; caching is an
optimization we would have to invalidate, deferred until it hurts.

### D10 — Exit gates, CI-relative per ADR-0001

`e2e-m3.yml`: loop ZFS pool + one MinIO with warm/cold buckets + PostgreSQL service. The
gating test drives the REAL stack — REST handlers, running engine (tick 300ms, second-scale
TTLs), ZFS agent — through the whole chain: pause → engine WARM (node released) → resume →
pause → engine COLD (synthetic full) → **cost report gate (stored ≤ 60% of logical — the
归档成本达标 proxy for a zero-skip+lz4+dedup+synthetic-full pipeline)** → **timed cold resume
< 10s to interactive** with artifact and disk-state continuity → pause → engine RECYCLED →
artifacts-only in cold store → selective restore into a new sandbox, artifact verified in the
new guest. Synthetic-full and prewarm-policy unit gates run alongside; every gate greps
`--- PASS:`.

## Deferred

- Disk synthetic full (single-stream cold disk) and L0 chunk-cache LRU eviction → M4.
- ARIMA prediction for sparse wake histories; rolling WS refresh → later milestones.
- Glacier-IR-style retrieval-fee awareness in archive destination choice (docs/zh/04 §7's
  histogram-driven destination selection) → M4, once real providers are wired.
- FastCDC/Blake3/Merkle repo + block aggregation (doc 04 #6) → unchanged, next major storage
  evolution.

## Consequences

Storage cost now has a shape: idle sandboxes decay into exactly what they are worth keeping,
on operator-set clocks, and the bill is inspectable per sandbox and per tier. The restore
SLO ladder (hot 141ms / warm 1.25s measured in M2; cold <10s gated here) maps directly onto
the tier a sandbox occupies. The prices paid: RECYCLED is irreversible beyond its artifacts;
resumes can now race the engine (resolved by CAS, but clients must expect 409s); and a cold
restore's first pause re-uploads a Full layer to L1 (chain reset) — bounded by the guest's
footprint, and the dedup Copier skips chunks L1 still has.
