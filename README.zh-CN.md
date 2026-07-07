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

## 路线图

- [x] 三轮调研与立项（产品拆解 → 前沿创新 → 实测数据验证）
- [x] 开源规划：AGPL-3.0、双语文档、云端实测矩阵
- [x] M0（第 1-2 周）：裸金属 + 嵌套虚拟化双环境；Firecracker/ZFS/uffd 原型基线（补齐《04》§9 数据缺口）（已完成：CI 嵌套虚拟化环境基线；裸金属复测待 M1）
- [ ] M1（第 3-6 周）：单机 MVP——全生命周期 API、模板系统、`embervm dev` 单进程模式
- [ ] M2（第 7-10 周）：秒级恢复管道——uffd + WS 预取、diff 快照、chunk 仓库
- [ ] M3（第 11-13 周）：分层归档与生命周期引擎、选择性恢复
- [ ] M4（第 14-16 周）：多节点调度、Gateway、加固 → 内部 MVP
- [ ] M5（可选）：面向 Agent 的 VM fork/branch API（tree-of-thought / RL rollout / time-travel 调试）

到可收费 beta 的现实预期：4-6 个月（后 70% 工作量在网络隔离、调度与可靠性加固）。

## 基准与方法学

本 README 中引用的性能数字只会来自裸金属实测（遵循 docs/zh/06 环境政策），绝不引用共享 CI 上测得的数据。`bench` 工作流每周产出一份 CI 基线报告（嵌套虚拟化，仅用于恢复模式间的相对比较），以构建产物（artifact）形式发布。方法学见 [docs/adr/0001-m0-baseline-methodology.md](docs/adr/0001-m0-baseline-methodology.md)。在任意带 KVM 的 Linux 上本地复现：先跑 `test/integration/smoke.sh`，再跑 `test/bench/restore-matrix.sh`。

## 贡献与许可

M0/M1 骨架落地后欢迎贡献——关注 Issues/Milestones。贡献条款：DCO + CLA（保留双许可能力）。代码复用政策：Apache-2.0 来源（Firecracker、E2B 的 Apache 部分）可带署名并入；BSL 代码仅作设计参考，绝不复制。

**AGPL-3.0** 许可——修改 EmberVM 并以网络服务形式对外提供时，必须开源你的修改。
