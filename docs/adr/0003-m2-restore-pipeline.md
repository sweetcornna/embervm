# ADR-0003: M2 second-level restore pipeline

- Status: Accepted
- Date: 2026-07-08

## 中文摘要

M2 把快照从"每次暂停一个 1GiB+ 的 memfile"变成**分块、压缩、内容寻址、分层（diff）**的对象,并在 pause 时**写穿到 S3 兼容的 L1 对象存储**;恢复走**工作集优先的 uffd 管线**(REAP 记录 + FaaSnap 并发装载)。**chunk 格式**(`pkg/memsnap`):16 KiB 定长 chunk(缺页 O(1) 定位,内容寻址交给存储层;FastCDC/Blake3/Merkle 演进属 M3),SHA-256 按未压缩字节寻址,lz4 逐块压缩(不可压缩存 raw),全零 chunk 只记录不存储(恢复用 UFFDIO_ZEROPAGE)。**分层快照**:首次 pause 取 Full,之后取 Diff(`track_dirty_pages` 从启动和每次 snapshot load 都保持开启);FC 的稀疏 diff memfile 用 SEEK_DATA/SEEK_HOLE 提取脏区间——**关键正确性决策:16 KiB chunk > 4 KiB 脏页粒度,部分脏的 chunk 必须在写入时与父链内容合并**,否则干净页位置会被洞的零字节覆盖(TestWriteDiffLayerPartialDirtyMerge 在上线前抓住了这个静默损坏);合并要求页粒度的洞报告,因此快照暂存目录必须在 ext4/tmpfs(ZFS recordsize 会把区间取整,APFS 上报物化的整文件区间——diff 提取仅限 linux)。**chunk 仓库**(`pkg/chunkstore`):本地目录 L0 + minio-go S3 L1,Tiered 本地优先、L1 回退、写穿本地;并行 Copier(默认 16 路)只传缺失 chunk——去重即在 Put 层发生。**uffd handler chunked 模式**:缺页整 chunk 装载(取块→lz4 解码→UFFDIO_COPY,EEXIST 逐页回退,跨 region/PCI 洞正确切分);首次 resume 按缺页首触顺序记录 WS(ws.json),之后的 resume 先按 trace 顺序急切预取 WS,再后台顺序回填其余 chunk,缺页始终优先——这修复了 M0 发现的"全量预取在 4/8GB 冷缓存反而更慢"(与按需缺页争带宽);热路径不做逐块哈希校验(内容寻址存储 + lz4 解码错误 + e2e 连续性门禁足够,在线校验属 scrub 工具)。**节点代理**:pause = Full/Diff 快照→chunk 化→删除原始 memfile→dataset 快照(@p N)→写穿 L1(chunks、manifest、snapfile、ws.json、`zfs send -c` 磁盘增量、snapshot.json 恢复描述符);到不了 L1 的 pause 视为失败(RPO 契约)。resume = chunked handler(--parent-pid 看护)+ `track_dirty_pages`(链可延续)+ `clock_realtime`(校时)+ guestd `POST /resumed`(计数 + /etc/embervm/resume-hook)。**异机恢复**(`RestoreSandbox`):模板以 zfs send 流经 L1 分发——**GUID 同源是硬约束,各节点重建模板无法接收增量**;磁盘链 `zfs receive -o origin=` 重放;dataset 挂载点钉回源节点路径(snapfile 记录的是绝对盘路径,FC 无盘路径覆盖 API);内存全部从 L1 分层拉取。**seq 连续性语义修正**:精确 +1 有竞态(客户端超时但服务端已计数),正确断言是"严格大于快照值"(重启会归零)——fc-restore.sh 与所有 KVM 测试统一。**推迟**:模板 memfd 共享/uffd minor faults(文档 #3)、FastCDC/Xet 式仓库、synthetic full 合并、分层归档 → M3;jailer → M4。退出标准(热 P50<500ms、温 P99<3s、pause→上传→异机 resume)由 e2e-m2 在嵌套虚拟化 CI 上相对测量(ADR-0001),含 `--- PASS` 防伪绿守卫。

## Context

M1 ships a working single-node lifecycle, but every pause writes a full raw memfile and resume
reads it back from local disk; snapshots never leave the node, diffs don't exist, and a node
loss loses every paused sandbox. M2's charter (docs/zh/03 §3) is the second-level restore
pipeline: chunked lz4 memfiles with zero-page skip, mandatory working-set record + prefetch,
dirty-page diff snapshots, `zfs send -i` disk increments, a content-addressed chunk repository
with parallel transfer and pause write-through, and correctness hooks (clock, resume hook,
VMGenID). Exit: pause→upload→different-node resume, hot P50 < 500ms, warm P99 < 3s. The docs
fix the *what*; this ADR records the *how* (D1–D10) and two correctness hazards found while
building it.

## Decision

### D1 — Fixed 16 KiB chunks, content-addressed by uncompressed SHA-256 (`pkg/memsnap`)

A page fault must map guest offset → chunk in O(1), so the memfile format uses fixed-size
chunks (16 KiB = 4 pages, within the charter's 8–16 KiB), NOT content-defined chunking — CDC
dedup belongs to the storage layer and its Xet-style evolution is M3 (doc 04 #6). Per chunk:
SHA-256 of the uncompressed bytes is the content address; lz4 block compression is used only
when it wins (`codec: lz4|raw`); all-zero chunks are recorded (`z:true`) but never stored and
restore as UFFDIO_ZEROPAGE. Layer manifests (JSON, producer-is-truth) carry
`snapshot_format_version`, `(fc_version, kernel_version)`, geometry, and the chunk list;
`Resolve()` flattens a full-root + diff chain newest-wins.

### D2 — Diff layers MERGE partially-dirty chunks with the parent at write time

Firecracker's diff memfile holds exactly the dirty 4 KiB pages (holes elsewhere). A 16 KiB
chunk with one dirty page therefore reads as 12 KiB of hole-zeros + 4 KiB of data — recording
it verbatim would clobber clean parent pages with zeros on restore. `WriteDiffLayer` walks the
sparse file with SEEK_DATA/SEEK_HOLE and overlays only the dirty extents onto the resolved
parent chunk, emitting self-contained chunks; restore stays a simple newest-wins lookup.
Consequences: (a) diff extraction requires page-granular hole reporting — snapshot staging
lives on ext4/tmpfs (the WorkDir), never on a ZFS dataset (recordsize rounds extents up) and
never on macOS (APFS reports materialized whole-file extents; `WriteDiffLayer` refuses
off-linux, its tests skip there and run in CI); (b) a chunk dirtied back to all-zero still
records as a zero chunk, correctly overriding the parent. Caught before shipping by
`TestWriteDiffLayerPartialDirtyMerge`.

### D3 — Chunk store: local dir L0 + S3 L1, tiered reads, dedup-first writes (`pkg/chunkstore`)

`Store` (Put/Get/Has/Delete by hash) + `Objects` (named blobs: manifests, snapfiles, WS
traces, disk streams). Backends: sharded local directory (atomic tmp+rename, safe under
concurrent same-hash Put) and any S3-compatible endpoint via minio-go (MinIO in CI;
Garage/SeaweedFS/Hetzner OS in production), configured by `EMBERVM_L1_*` env
(`EMBERVM_L1_DIR` selects a directory L1 for tests/shared-FS). `Tiered` reads local-first,
falls back to L1, and writes fetched chunks through so a restore pays the network price once.
`Copier` fans out transfers (default 16-way) and skips chunks the destination has — dedup is
the Put contract everywhere, so write-through cost scales with new content, not memory size.
SHA-256 over Blake3/Merkle: stdlib, no new dependency; the Xet-style repo is M3.

### D4 — Chunked uffd mode with WS record → eager replay → concurrent backfill (`pkg/uffd`)

`--mode chunked` serves faults a whole chunk at a time (fetch → decode → UFFDIO_COPY, EEXIST
falls back pagewise; chunks straddling region boundaries or the PCI hole are split
correctly — unit-tested against a real userfaultfd, no KVM needed). First resume records
first-touch chunk order to `ws.json` (REAP); later resumes eagerly prefetch exactly that trace,
then backfill all remaining chunks sequentially in the background while demand faults keep
priority (FaaSnap concurrent paging). This replaces M0's blind whole-file prefetch, which lost
to lazy at 4/8 GiB cold-cache by contending with demand faults — the M0 finding that motivated
this design. The fault path does NOT hash-verify fetched chunks (content-addressed store +
lz4/length errors + e2e continuity gates; online scrubbing is a separate tool's job).
`lazy`/`prefetch` raw-memfile modes are byte-for-byte frozen (M0 bench contract).
`--parent-pid` self-exits an orphaned handler.

### D5 — Pause pipeline: Full→Diff chain, memfile deleted, write-through L1 is the RPO gate

Boot and every snapshot load set `track_dirty_pages`, so the first pause takes a Full snapshot
and every later pause a Diff. After chunkify the raw memfile is deleted — the chunk store is
the source of truth. The dataset snapshot shares the layer tag (`@p<N>`). With L1 configured,
pause then pushes: new chunks (Copier dedup), the layer manifest, the snapfile, `ws.json`, a
`zfs send -c` disk delta (first delta incremental from the clone origin), and finally
`snapshot.json` — a restore descriptor (template, geometry, mountpoint, layer list). A pause
that cannot reach L1 FAILS: write-through is docs/zh/02 §3's RPO guarantee, not an
optimization. The previous resume's handler is drained gracefully first so it can persist a
freshly recorded working set.

### D6 — Cross-node restore: template streams for GUID lineage, mountpoint pinning

`RestoreSandbox(id)` rebuilds a sandbox on a node that never saw it, from L1 alone. Two hard
constraints discovered in design: (a) ZFS incremental receive requires the base snapshot's
GUID, so templates are distributed as `zfs send` streams pushed at build time
(`templates/<tid>.zstream`) and received on demand — a node that rebuilt the same template
locally could never receive the sandbox's delta chain; (b) a Firecracker snapfile records
absolute drive paths and the API offers no override, so the received dataset's mountpoint is
pinned to the origin node's path (`zfs set mountpoint=`). Restore then replays the disk chain
(`zfs receive -o origin=` first, `-F` after), pulls manifests + snapfile + WS into the local
WorkDir, and runs the normal chunked resume with an empty chunk cache — memory faults and WS
prefetch pull from L1 tiered. PlainBackend returns `ErrReplicationUnsupported`: cross-node
needs ZFS. Placement stays test/scheduler-driven until M4.

### D7 — Correctness hooks: clock_realtime, resumed notification, VMGenID by construction

Snapshot loads pass `clock_realtime:true` (kvm-clock realtime re-arm — the 校时 line) and the
agent POSTs guestd `/resumed` after the guest answers: guestd bumps a `resumes` counter
(surfaced in `/healthz`) and best-effort runs `/etc/embervm/resume-hook` (5s timeout,
non-fatal) for token refresh and friends. VMGenID needs no code: FC ≥1.16 injects it on
snapshot load and the 6.1 guest kernel reseeds its RNG.

### D8 — Seq continuity is monotone-above-snapshot, not exact +1

A health probe whose client times out after the server counted it inflates the guest seq by
one; the first CI run of the chunked lifecycle and an M0 smoke flake on v1.15.1 both hit this.
The continuity claim is "the SAME process continued", whose faithful assertion is
`seq > seq_at_snapshot` (a rebooted guest resets to 1). fc-restore.sh and all KVM tests now
share that semantic.

### D9 — Control plane unchanged

No REST surface change: `pause` transparently becomes layered + write-through, `resume`
transparently becomes WS-first chunked. `RestoreSandbox` is a concrete-agent method exercised
by tests; scheduling/placement (and exposing it over the wire) is M4. `RestoreMode` defaults
to `prefetch` (M1 behavior) — `chunked` is opt-in until M2 exits, then flips in deploy configs.

### D10 — Exit gates (e2e-m2.yml), CI-relative per ADR-0001

Loop ZFS pool + local MinIO on the runner. `TestHotRestoreP50` (1 GiB guest, 15 GiB sparse
data disk, 15 pause/resume cycles, P50 < 500ms), `TestWarmRestoreP99` (chunk cache wiped every
cycle, WS + faults from L1, P99 < 3s), `TestCrossNodeRestore` (two ZFS subtrees + shared L1;
node A pauses and dies, node B restores: monotone seq, markers, disk state, resumes+1),
`TestDiffChain` (3-layer chain, markers from every layer, diff < 25% of full stored bytes),
`TestDedupReport` (same-template dedup ratio, report-only). Every gate is grepped for
`--- PASS:` — the M1 false-green lesson is now a convention.

## Deferred

- Template memfd sharing + uffd minor faults (doc 04 #3): the density play, not required by
  the M2 charter list; needs guest page-cache-aligned template memory and is scheduled with
  the M3 density work.
- FastCDC/Blake3/Merkle chunk repo, ~64 MB block aggregation, synthetic-full compaction
  (diff chains currently grow unboundedly until the next Full): M3, doc 04 #6.
- Rolling WS updates (current trace is first-resume-wins; refreshing it needs
  fault+prefetch-hit attribution): M3, alongside pre-warm prediction (doc 04 #5).
- Bare-metal re-measurement of all latency gates: ADR-0001 debt, unchanged.

## Consequences

Snapshots are now portable objects: any node with the L1 credentials can restore any paused
sandbox, which is the storage foundation M3's archive tiers and M4's scheduler assume. Memory
that is zero or unchanged costs nothing to store and nothing to re-upload; what a sandbox
actually touches is what a resume actually loads first. The price: pause latency now includes
chunkify + upload (bounded by dirty size, not memory size, after the first pause), restores
depend on chunk-store health (handler retries, then fails loudly rather than hanging a vCPU),
and the diff-chain length is unbounded until M3's synthetic full lands.
