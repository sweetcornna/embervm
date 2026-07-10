# EmberVM v0.1 acceptance — G1–G6 evidence map

v0.1 (tag `v0.4.0-m4`) ships when every project goal G1–G6 (docs/zh/03 §1) is either
CI-verified or explicitly documented as a non-CI claim. CI latencies are **relative** numbers
from GitHub Actions nested virtualization (ADR-0001): they prove the mechanism and the
regression gate, not absolute production performance. Bare-metal re-measurement is a tracked
debt.

Every gate below greps `--- PASS: <test>` in its workflow — a skipped test fails the job
(the M1 false-green lesson).

| Goal | Charter (docs/zh/03 §1) | Status | Evidence |
|---|---|---|---|
| **G1** 秒级恢复 | hot resume P50 <500ms (P99 <2s); warm restore (10–20GB data disk) P99 <3s interactive | ✅ CI-verified (M2, re-asserted every push) | `e2e-m2`: `TestHotRestoreP50` (measured P50 141ms), `TestWarmRestoreP99` (P99 1.25s from MinIO); `e2e-m1`: `TestHotResumeUnder1s15GiB` (15 GiB sparse data disk off the critical path) |
| **G2** 持久化存储 | 10–20GB per-sandbox data disk; zero data loss after pause (L1 write-through, RPO ≤ 5min); cross-node restore | ✅ CI-verified (M2 + M4) | `e2e-m2`: `TestCrossNodeRestore` (pause → upload → restore on a second agent); `e2e-m4`: `TestClusterKillNode` proves the RPO contract both ways — pre-pause marker survives node death, post-snapshot write is verifiably gone |
| **G3** 分层冷归档 | HOT→WARM→COLD→RECYCLED automatic flow; cold-tier cost ≤ $5/TB/month | ✅ flow CI-verified (M3); 💲 cost is procurement | `e2e-m3`: `TestLifecycleTTLFlow` (TTL transitions, cold restore 453ms < 10s gate, archive stored/logical 6.8% ≤ 60% gate, recycle + selective restore). The $/TB claim is a storage-procurement decision (S3 Glacier-class ≈ $1–4/TB/mo), not a property this codebase can test — recorded here per ADR-0005 |
| **G4** 弹性计算 | creation <500ms; ≥50 concurrent per node; horizontal node scaling; **M6: runtime resize + autoscale + explicit migrate** | ✅ CI-verified (M4 + M6) | `e2e-m4`: `TestFastCreateUnder500ms` (golden fast-create P50 to interactive), `TestConcurrency50` (50 × 256 MiB exec-verified on one agent), `TestClusterKillNode` (3-node placement observed: sticky/bin-pack spread + on-demand template receive); `e2e-m6` (ADR-0007): `TestVirtioMemResizeKVM` (runtime memory grow/shrink surviving chunked pause + uffd restore, resize-after-restore), `TestResizeCPUQuotaKVM` (cpu.max quota moves with the verb), `TestAutoscaleMemoryKVM` (real guest pressure grows the sandbox through the engine, releasing it shrinks back to base), `TestMigrateRunningKVM` (live cross-node move of a RUNNING sandbox, ~2.3s in CI-class virt) |
| **G5** 稳定性 | control-plane availability ≥ 99.9%; automatic uffd/process-zombie recovery | ✅ reaping CI-verified (M4); 📋 availability is an ops SLO | `integration-kvm`: `TestWatchdogReapsZombiesKVM` (kill FC behind the agent's back → FAILED + resources released, on both supported FC versions); `e2e-m4`: `TestClusterKillNode` (node-level death → eviction → cross-node recovery). 99.9% availability is a deployment SLO (redundant apiservers + managed PG), not a single-binary property — the mechanisms it needs (stateless apiserver, PG as truth, restore-on-demand) are what M4 built |
| **G6** 开源可部署性 | full `embervm dev` experience on one nested-virt cloud VM; free CI running real microVMs | ✅ CI-verified (M1, every push since) | Every e2e workflow boots real Firecracker microVMs on free GitHub Actions runners; `deploy/singlenode/` is the one-VM walkthrough (`embervm dev` single-process mode); `integration-kvm` runs the full smoke on two FC versions |

## M4 exit criterion

> 3 节点集群,杀任一 worker 沙箱可异机恢复 (docs/zh/03 §3)

Verified by `e2e-m4` / `TestClusterKillNode`: three jailed `nodeagent` daemons (own ZFS
subtrees, netns ranges, chunk caches) + PostgreSQL + MinIO L1; `kill -9` of the worker hosting
a RUNNING and a PAUSED_HOT sandbox → scheduler eviction (polled heartbeats, miss threshold),
RUNNING → FAILED; both sandboxes subsequently resume on healthy nodes with guest-process
continuity, and the recovered guest serves HTTP through the two-hop gateway proxy.

## Known limits shipping in v0.1 (ADR-0005 "out of scope")

Static node membership; netns-level egress on/off only (no L7 egress proxy); no L0 chunk-cache
LRU; disk chains replay deltas (no disk synthetic full); golden images are node-local;
bare-metal numbers pending (ADR-0001).
