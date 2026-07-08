# ADR-0006: M5 agent-native fork/branch/rollback

- Status: Accepted
- Date: 2026-07-09

## 中文摘要

M5 把 checkpoint/fork/rollback 变成沙箱一等 API(docs/zh/03 §3 M5,《04》#4/#8),退出标准"单沙箱 fork 出 10 分支并行执行,父实例不停顿"。**checkpoint 即 pause 产物**(D1):每次 chunked pause 本就产出内存层 p\<N\>(内容寻址 chunk)+ ZFS 快照 @p\<N\>,checkpoint 只是给它命名并记录在 PG(checkpoints 表 tag→layer/seq);`POST /v0/sandboxes/{id}/checkpoints`(空 tag 自动命名 cp\<seq\>),重复 tag 409。**fork = golden fast-create 泛化**(D2):`zfs clone parent@p\<N\>`(磁盘 CoW)+ 复制父沙箱不可变 staging 里的 manifest 链 p1..pN + snapfile-pN + ws 提示 + 常规 chunked 热恢复;内存天然 CoW——内容寻址 chunk 全体共享,子沙箱从同一本地库缺页读入;父沙箱状态机全程不动(链文件写定后不可变,fork 可与父的下一次 pause 安全竞速);子沙箱 diskOrigin 记录克隆基(GUID 谱系),其后续 pause 写穿与跨节点恢复原样工作(首个磁盘增量读 dataset 真实 origin 属性);同节点放置(M5 范围),几何与 egress 继承,配额照算,省略 checkpoint 即"现在分支"(先自动打点再克隆);fastCreate 重构为共享 cloneRestore 的薄壳。**rollback = DeltaBox 层切换**(D3):`zfs rollback -r @p\<N\>`(dataset 同名同挂载点,jail 无需重建)+ 内存链裁剪到目标 + 与各层恢复共用的热 resume;snapCount 只增不减(tag 单调契约跨 rollback 成立);被丢弃层的 staging 与 L1 对象删除、L1 恢复描述符按裁剪后的链重写(rollback 与下次 pause 之间节点死亡,恢复到的是 checkpoint 而非悬空引用);rollback **丢弃**目标之后的一切,含更新的 checkpoint——若其上有活 fork 则 409(zfs 在底层也会拒绝销毁有克隆依赖的快照)。**time-travel**(D4):exec 带 `"checkpoint": true` 先打点再执行(该步 checkpoint = 命令所见状态),响应带回 tag;fork 任意步的 tag 即重放,无需独立历史子系统——checkpoints 表就是时间线。**谱系守卫**(D5):sandboxes.parent_id/forked_from;有活子沙箱的父:DELETE 409、TTL 引擎跳过(ReleaseLocal 会在克隆依赖下销毁 dataset)、更新 checkpoint 有活 fork 时 rollback 409。**熵与身份**(D6):每次 fork/rollback 经 LoadSnapshot(VMGenID 重播 PRNG)+ guestd /resumed 钩子;fork 是"一份快照多次恢复"的显式声明(charter 风险 #4)。**退出门禁**(D7):e2e-m5——TestFork10Branches(父后台计数器跨 fork 窗口严格递增、状态恒 RUNNING、10 分支各自 exec 验证/见检查点态/相互隔离)、TestRollbackCheckpoint(REST 层丢弃+守卫+重打点)、TestTimeTravelReplay(3 步自动打点、fork 第 2 步分叉重放)、节点动词门(TestForkKVM/TestRollbackKVM);checkpoint 暂停窗口与 fork/rollback 到可交互耗时全程测量记录(CI 相对,<100ms 是裸金属目标,DeltaBox 实证 checkpoint 14ms/rollback 5ms 可达)。

## Context

M1–M4 built a cluster of sandboxes that pause, tier, restore, and survive node loss — but a
sandbox is still a single timeline. The agent workloads EmberVM targets (tree-of-thought
exploration, RL rollout fan-out, time-travel debugging — docs/zh/00 §agent, 04 #4) need
branching: take a moment of a live sandbox and run N futures from it, or return to it. Morph
sells this as Infinibranch; E2B's open-source version has nothing. M5's charter: fork/branch/
rollback as first-class API over uffd-CoW memory + ZFS layer chains, checkpoint/rollback
targeting <100ms (bare-metal; DeltaBox demonstrated checkpoint 14ms / rollback 5ms), exit
criterion "one sandbox forks 10 branches executing in parallel, parent never stalls".

The load-bearing observation: **every mechanism already existed**. A chunked pause produces a
memory layer p\<N\> (content-addressed chunks) plus a ZFS snapshot @p\<N\>; M4's golden
fast-create already clones another sandbox's snapshot and hot-restores its memory image; the
M2/M3 restore machinery already handles clone lineage (DiskOrigin) and layer switching. M5 is
those pieces, named and guarded.

## Decision

### D1 — A checkpoint IS the pause artifact, named

`POST /v0/sandboxes/{id}/checkpoints {"tag"?}` drives the existing snapshot verb (pause →
diff layer + zfs snapshot → resume; the brief pause window is the checkpoint cost, kept small
by diff layers) and records `(tag, layer p<N>, seq N)` in the `checkpoints` table (migration
0005). Empty tag self-names `cp<seq>`; duplicate tags 409; `GET .../checkpoints` is the
timeline, oldest first. The handler parses the layer from the verb's producer-defined return
format (`<id>@<tag>-<seq>`). Checkpoints are usable while the sandbox chain is local (HOT);
fork/rollback of tiered-out sandboxes is refused — resume first (lineage-aware tiering is out
of scope).

### D2 — Fork generalizes golden fast-create

Node verb `Fork(parentID, layer, newID)`: ZFS clone of `parent@p<N>` (disk CoW), copy the
manifest chain p1..pN + `snapfile-p<N>` + the ws prefetch hint from the parent's staging,
seed the child's chain, fresh netns lease, inherited egress, ordinary chunked hot resume.
Memory is CoW by construction — chunks are content-addressed and shared, the child faults
pages from the same local store; nothing is copied. The parent's state machine never
transitions: committed chain files are immutable, so the chain is walked from disk
(`chainFor`) and fork races the parent's next pause safely. The child records
`diskOrigin={parent, p<N>}`, so its own pause write-through and cross-node restore work
unchanged (the first disk delta reads the dataset's real `origin` property — the same
mechanism golden children use). `fastCreate` is now a thin wrapper over the shared
`cloneRestore`. Fork requires chunked + jailed + ZFS for golden's reason: chroot-relative
snapfile paths are what make one sandbox's snapshot loadable inside another's jail.

REST: `POST /v0/sandboxes/{id}/fork {"checkpoint"?}` — omitted checkpoint = "branch now"
(auto-checkpoint the live parent, then clone; ten forks reuse one checkpoint by tag). The
child is a full sandbox row: owner-scoped, quota-counted, `parent_id`+`forked_from` set,
placed on the parent's node (same-node in M5 — the staging chain is node-local).

### D3 — Rollback is the layer switch, in place

Node verb `Rollback(sandboxID, layer)`: claim via the state machine (RUNNING→PAUSING kills
the processes un-snapshotted — discarding this epoch is the point; PAUSED_HOT has nothing to
kill), `zfs rollback -r @p<N>` (same dataset name, same mountpoint — no jail rebuild), trim
the in-memory chain to the target, hot resume from p\<N\> — the same resume the tiers use
(04 #8: rollback and resume share one mechanism). `snapCount` keeps counting upward: tags
stay monotone across rollbacks (the standing seq/tag contract). Discarded layers' staging
files and L1 objects are deleted and the L1 restore descriptor is rewritten from the trimmed
chain, so a node death between rollback and the next pause restores the checkpoint rather
than a dangling reference. Rollback DISCARDS checkpoints newer than the target (their zfs
snapshots die with `-r`); the control plane deletes their rows and refuses (409) while any
newer checkpoint has live forks — zfs would refuse the snapshot destroy under dependent
clones anyway, we surface it before touching anything. Time-travel replay therefore uses
fork-from-step (non-destructive); rollback is the destructive "go back".

### D4 — Time-travel is exec-with-checkpoint

`POST .../exec {..., "checkpoint": true}` checkpoints BEFORE running the command — a step's
checkpoint is the state the command saw, so forking `step-k`'s tag replays step k. The tag
rides the exec response (additive JSON). No history subsystem: the checkpoints table is the
timeline (每步自动快照 + 任意步 fork 重放).

### D5 — Lineage guards: the ZFS clone dependency, made loud

Children are ZFS clones of the parent's snapshots. Three guards keep that dependency from
surfacing as mysterious bottom-layer failures: DELETE of a parent with live (non-STOPPED)
children → 409 listing them; the TTL engine skips fork parents (ReleaseLocal would
`zfs destroy` under the clones); rollback past a checkpoint with live forks → 409 (D3).
Children tier and die freely. Promote/lineage-GC (letting a parent die by reparenting clones)
is future work.

### D6 — Entropy and identity per branch

Every fork/rollback passes LoadSnapshot → VMGenID reseeds the guest PRNG → guestd /resumed
hook refreshes application-level state. Fork is the explicit multi-resume declaration
(charter risk #4); the default "one snapshot resumes once" stands everywhere else. Each child
owns a netns lease (same guest IP, namespace isolation — the M0 contract).

### D7 — Exit gates (e2e-m5, `--- PASS:` guards)

- `TestFork10Branches` (退出标准 verbatim): a background counter pulses in the parent; one
  checkpoint; 10 parallel REST forks; every branch exec-verified, sees the checkpoint state,
  diverges invisibly to parent and siblings; the parent's counter grows strictly across the
  fork window AND the branch-execution window, and its state never leaves RUNNING.
- `TestRollbackCheckpoint`: keep/discard semantics through REST, the live-fork 409, pruned
  timeline, re-checkpoint after rollback.
- `TestTimeTravelReplay`: three auto-checkpointed exec steps; fork step 2's tag; the branch
  holds the pre-step-2 state and replays divergently; the parent's timeline is untouched.
- Node-verb gates `TestForkKVM`/`TestRollbackKVM` (fork sees checkpoint state, parent
  untouched; rollback discards, chain continues monotone).
- Checkpoint pause window, per-fork latency, rollback-to-interactive all measured and logged
  (CI-relative, ADR-0001; the <100ms charter figure remains a bare-metal target).

## Out of scope (recorded so nobody "finds" them missing)

Cross-node fork (children live beside the parent; a child restores cross-node AFTER its own
first pause); fork/rollback of WARM/COLD sandboxes (resume first); checkpoint retention/
pruning policies (rollback and recycle are the only reapers); clone promotion / lineage GC;
overlayfs/EROFS rootfs sharing (04 #7); GPU state (04 #11).

## Consequences

- Branching is O(clone + hot resume): CI shows the same ~150-250ms per fork as golden
  fast-create, because it IS golden fast-create. Ten branches cost ten clones, not ten boots.
- A forked parent is pinned HOT while children live. That is the honest cost of ZFS clone
  lineage; the guards make it visible (409s, engine skip) instead of mysterious.
- Rollback trades away later checkpoints for simplicity (one dataset, no generation-swapping,
  no jail rebuilds). The non-destructive alternative is always available: fork the earlier
  checkpoint instead.
- The API is what differentiates against E2B's open-source version (docs/zh/02 §7 #5):
  tree-of-thought fan-out, RL rollouts, and step-replay debugging are one HTTP call each.
