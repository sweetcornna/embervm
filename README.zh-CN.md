# EmberVM（余烬）

[![lint-unit](https://github.com/sweetcornna/embervm/actions/workflows/lint-unit.yml/badge.svg)](https://github.com/sweetcornna/embervm/actions/workflows/lint-unit.yml)
[![integration-kvm](https://github.com/sweetcornna/embervm/actions/workflows/integration-kvm.yml/badge.svg)](https://github.com/sweetcornna/embervm/actions/workflows/integration-kvm.yml)
[![bench](https://github.com/sweetcornna/embervm/actions/workflows/bench.yml/badge.svg)](https://github.com/sweetcornna/embervm/actions/workflows/bench.yml)

> *永不冷透的沙箱云。*

[English README](README.md) · 许可证：**AGPL-3.0** · 状态：**Pre-alpha（设计与调研阶段）**

**EmberVM** 是可自托管的开源沙箱云：给每个 AI Agent 会话 / 云端开发负载一台隔离的"云电脑"（Firecracker microVM），对标 E2B / Manus 沙箱 / Cursor Cloud，并提供它们开箱不具备的能力：

- **秒级恢复**：热恢复 P50 < 500ms；含 10-20GB 持久数据盘的温恢复 P99 < 3s 可交互
- **持久化存储一等公民**：每沙箱 10-20GB 数据盘，pause/resume 内存+磁盘全状态保留，零丢失（写穿对象存储，RPO ≤ 5min）
- **分层冷归档**：热（本机 NVMe）→ 温（对象存储）→ 冷（低价层）→ 回收（仅留 artifacts），自动流转，冷层 ≤ $5/TB/月
- **弹性计算**：创建 < 500ms，单节点 50+ 并发，节点池水平扩容
- **零厂商锁定**：普通 Linux + PostgreSQL + Redis + 任意 S3 兼容存储；不依赖 Nomad/Consul/GCP（这正是自托管 E2B 的最大痛点）
- **内置 Web 控制台**：API server 在 `/` 内嵌管理界面（舰队热度图、生命周期操作、运行时 resize、checkpoint/fork/rollback、guest 内 exec、存储成本）；单二进制交付,无需额外部署（`make web` 从 `web/` 重建）

## 原理（一段话）

10GbE 下搬 15GB 需 12s——整份复制永远做不到秒级。行业（E2B/CodeSandbox/Lambda SnapStart/Cursor）验证过的统一答案是：**O(1) CoW clone + 缺页懒加载 + 工作集预取 + 只搬 diff**。EmberVM 的组合：Firecracker（KVM 硬隔离 + 原生快照）+ 本地 NVMe ZFS（dataset + raw file，clone O(1)、`send -i` 增量）+ uffd 内存 handler + REAP 式工作集预取（纯懒加载是陷阱：43MB/s vs 预取后 533MB/s）+ 8-16KiB lz4 chunk 化内存快照 + FastCDC 内容寻址 chunk 仓库（Garage/SeaweedFS/任意 S3）分层归档。CodeSandbox 已在生产验证此类设计：resume 平均 400ms / P99 2s。

```
Client ─▶ Gateway ─▶ API Server ─▶ Scheduler        （控制面：PostgreSQL + Redis）
                                      │ gRPC
              Worker 裸金属 ×N：Node Agent ─▶ Firecracker ×50（内含 guestd）
                │ uffd handler（懒加载 + WS 预取，每 VM 一进程）
                └ 本地 NVMe ZFS（L0 热层）
                      │
              L1: chunk 仓库，FastCDC 去重（Garage/SeaweedFS/任意 S3） ─▶ L2: 冷归档
```

生命周期：`RUNNING → PAUSED-HOT → PAUSED-WARM → ARCHIVED-COLD → RECYCLED`，调度粘性 + 唤醒直方图预测（用户回来前预取快照）。

## 在云服务器上实测

Firecracker 需要 `/dev/kvm`（裸金属或嵌套虚拟化）。已核实矩阵（2026-07）：

| 环境 | 可用？ | 入门成本 |
|---|---|---|
| Hetzner 独服 / Server Auction | ✅ 裸金属（基准级数据） | €25-40/月 |
| GCP（Intel 系 + `--enable-nested-virtualization`） | ✅ 嵌套 | Spot ~$14-20/月 |
| AWS C8i/M8i/R8i（2026-02 起 `NestedVirtualization=enabled`） | ✅ 嵌套 | c8i.large ~$68/月 |
| Azure Dv3+（安全类型须选 Standard 非 Trusted Launch） | ✅ 嵌套 | D2s_v5 ~$70/月 |
| DigitalOcean Droplet | ✅ 嵌套（冒烟测试） | $6/月 |
| GitHub Actions `ubuntu-latest`（x86_64） | ✅ 有 KVM——免费 CI 跑真 microVM | 免费 |
| Hetzner Cloud VM、Linode、阿里云标准 ECS / 腾讯云 CVM | ❌ 无嵌套虚拟化 | — |

完整指南：[docs/zh/06-云服务器实测指南.md](docs/zh/06-云服务器实测指南.md)。

## 文档

三轮调研、全部关键决策经一手数据验证：

| 文档 | 内容 |
|---|---|
| [01 调研报告](docs/zh/01-调研报告.md) | E2B 架构拆解、Manus/Cursor 云原理、竞品对比、Firecracker 快照机制、存储与归档技术 |
| [02 技术架构设计](docs/zh/02-技术架构设计.md) | 组件设计、生命周期状态机、三级恢复路径、网络/安全、技术选型表 |
| [03 立项计划书](docs/zh/03-立项计划书.md) | 目标指标、里程碑（M0-M5）、团队、供应商成本对比、风险登记册、验收 |
| [04 创新与最佳实践](docs/zh/04-创新与最佳实践.md) | **数据验证结论（冲突处以此为准）**+ 前沿论文（REAP/FaaSnap/DeltaBox/Sabre…）、12 个创新点排序、延迟预算模型、宿主加固清单、可靠性 SOP、分层归档经济学 |
| [05 开源项目规划](docs/zh/05-开源项目规划.md) | 命名、AGPL-3.0 + CLA 策略、仓库结构、版本治理、vs E2B/Daytona 定位 |
| [06 云服务器实测指南](docs/zh/06-云服务器实测指南.md) | 逐厂商核实的嵌套虚拟化矩阵、测试拓扑、单机实测流程、GitHub Actions KVM CI 策略 |
| [07 沙箱隔离方案深度对比](docs/zh/07-沙箱隔离方案深度对比.md) | Docker vs Firecracker 逐维度对比、2026 隔离技术与沙箱云全景、Cloud Hypervisor / gVisor 挑战者分析——支撑 M6 底座决策的运行时弹性判定 |

## 路线图

- [x] 三轮调研与立项（产品拆解 → 前沿创新 → 实测数据验证）
- [x] 开源规划：AGPL-3.0、双语文档、云端实测矩阵
- [x] M0（第 1-2 周）：裸金属 + 嵌套虚拟化双环境；Firecracker/ZFS/uffd 原型基线（补齐《04》§9 数据缺口）（已完成：CI 嵌套虚拟化环境基线；裸金属复测待 M1）
- [x] M1（第 3-6 周）：单机 MVP——REST 全生命周期 API + PostgreSQL、模板构建器（Docker 镜像 → microVM）、guestd、ZFS 集成、`embervm dev` 单进程模式（[单机快速上手](deploy/singlenode/README.md)；退出标准——20 并发、热恢复 <1s（含 15GB 数据盘）、单命令部署——已在嵌套虚拟化 CI 验证，裸金属复测待跟进）
- [x] M2（第 7-10 周）：秒级恢复管道——chunk 化 lz4 快照（内容寻址、零页跳过）、工作集记录 + 预取（REAP/FaaSnap）、Full→Diff 分层链、S3 chunk 仓库 + pause 写穿、异机恢复（[ADR-0003](docs/adr/0003-m2-restore-pipeline.md)；退出标准——热恢复 P50 <500ms、温恢复 P99 <3s、pause→上传→异机 resume——嵌套虚拟化 CI 验证，裸金属复测仍为跟踪项）
- [x] M3（第 11-13 周）：分层归档与生命周期引擎——TTL 驱动 HOT→WARM→COLD→RECYCLED、synthetic full 合并 + chunk GC、唤醒直方图预热、仅 artifacts 的选择性恢复、每沙箱成本报表（[ADR-0004](docs/adr/0004-m3-archive-lifecycle.md)；退出标准——冷归档恢复 <10s 可交互、归档成本门禁——嵌套虚拟化 CI 验证）
- [x] M4（第 14-16 周）：弹性与生产加固——多节点调度器（轮询心跳、粘性 + bin-pack 放置、驱逐 + 异机恢复）、jailer 加固 Firecracker、golden 快速创建（<500ms）、单节点 50 并发、WebSocket 透传网关代理、netns 级 egress 策略、Prometheus 指标（[ADR-0005](docs/adr/0005-m4-elasticity-hardening.md)；退出标准——3 节点集群 kill -9 任一 worker 沙箱可异机恢复、G1-G6 全部验收见 [docs/acceptance-v0.1.md](docs/acceptance-v0.1.md)——嵌套虚拟化 CI 验证）→ **开源 v0.1 发布**
- [x] M5（可选）：Agent 原生 fork/branch/rollback——checkpoint 一等 API、任意检查点 fork N 并行分支（ZFS clone + 内容寻址 chunk 共享 = 磁盘+内存 CoW）、原地 rollback、每步 exec 自动打点支持 time-travel 重放（[ADR-0006](docs/adr/0006-m5-fork-branch.md)；退出标准——单沙箱 fork 出 10 分支并行执行且父实例不停顿——嵌套虚拟化 CI 验证）
- [x] M6：运行时弹性——单沙箱内存运行时可增可减（virtio-mem 真热插拔，宿主真实回收,跨 chunked pause / uffd restore 存活）、CPU 按开机核数上限内配额伸缩（cgroup cpu.max）、按 guest 压力自动弹性（PSI/MemAvailable → 生命周期引擎）、显式跨节点 `migrate` 动词（[ADR-0007](docs/adr/0007-m6-runtime-resize.md)，底座对比见 [docs/zh/07](docs/zh/07-沙箱隔离方案深度对比.md)；退出标准——resize 双向跨快照恢复存活、真实压力驱动自动扩缩、RUNNING 沙箱活体迁移——嵌套虚拟化 CI 验证）
- [x] M7：默认按需分配——不带几何的创建**默认弹性**：从小规格（256 MiB / 1 vCPU）起步、按压力自动伸缩到可配上限（4 GiB / 4 vCPU），不再"创建即固定"；弹性沙箱从每模板第二个 **elastic golden 快照**（预烘焙 hotplug 区）毫秒级快速创建而非冷启动；运行时 autoscale 开关、resize/autoscale/migrate 时间线事件、节点超售视图与完整的控制台资源管理面（[ADR-0008](docs/adr/0008-default-elastic-geometry.md)；退出标准——弹性 fast-create P50 <500ms、jailed golden 克隆上跨 pause/resume 的 hotplug resize、memmap 税实测探针——嵌套虚拟化 CI 门禁）

到可收费 beta 的现实预期：4-6 个月（后 70% 工作量在网络隔离、调度与可靠性加固）。

## 基准与方法学

本 README 中引用的性能数字只会来自裸金属实测（遵循 docs/zh/06 环境政策），绝不引用共享 CI 上测得的数据。`bench` 工作流每周产出一份 CI 基线报告（嵌套虚拟化，仅用于恢复模式间的相对比较），以构建产物（artifact）形式发布。方法学见 [docs/adr/0001-m0-baseline-methodology.md](docs/adr/0001-m0-baseline-methodology.md)。在任意带 KVM 的 Linux 上本地复现：先跑 `test/integration/smoke.sh`，再跑 `test/bench/restore-matrix.sh`。

## 贡献与许可

M0/M1 骨架落地后欢迎贡献——关注 Issues/Milestones。贡献条款：DCO + CLA（保留双许可能力）。代码复用政策：Apache-2.0 来源（Firecracker、E2B 的 Apache 部分）可带署名并入；BSL 代码仅作设计参考，绝不复制。

**AGPL-3.0** 许可——修改 EmberVM 并以网络服务形式对外提供时，必须开源你的修改。
