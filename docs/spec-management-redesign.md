# Spec 管理架构重设计方案

## 执行摘要

Sichek 的 spec（硬件基线配置）是健康检查的判定依据。**spec 错了，健康检查就错了。**

当前 spec 管理存在以下核心风险：
- **4 个来源、3 个 git 仓库**（sichek 主仓库 + `lzi/sichek_spec` + `siflow/config-server`）维护同一份数据，优先级混乱，容易不一致
- **显卡和 IB 网络的 spec 缺失/错误是运维中最高频的问题**，直接导致误 cordon 健康节点或漏拦故障节点
- 节点端无版本追溯，出问题时**无法快速定位"谁改了什么、什么时候改的"**
- 裸金属交付场景下，离线环境 spec 可能不全

本方案目标：**spec 来源可信、变更可审、状态可查、离线可用。**

---

## 一、现状分析

### 1.1 当前 Spec 来源（4 个）

| # | 来源 | 路径 / 机制 | 谁维护 | 何时生效 |
|---|------|------------|--------|---------|
| 1 | **Git 仓库 (in-tree)** | `components/<name>/config/default_*_spec.yaml` | 开发者 | 编译打包时内置进 RPM/DEB |
| 2 | **K8s ConfigMap** | `sichek-default-spec` → 挂载后 cp 到 host `/var/sichek/config/` | K8s 部署者 | init container 阶段 |
| 3 | **OSS 远程下载** | `SICHEK_SPEC_URL/{component}/{deviceID}.yaml` | spec git 仓库维护者 | 运行时按需拉取 |
| 4 | **Board ID 自动发现** | sysfs/NVML 检测 → 用 ID 索引 spec map | 自动 | 运行时 |

> **注意**: 来源 3 "按 boardID 从 OSS 下载缺失 spec" 是**近期新上线的能力**（~2026-04 时间线）。
> 此前如果本地 spec 文件中不包含当前硬件的 boardID，组件只能 fallback 到默认值或报错。
> 新能力使得 sichek 可以在运行时自动从 OSS 按 `{component}/{boardID}.yaml` 路径拉取缺失的设备 spec 并合并到本地文件中。
> 相关代码入口: 各组件 `config/spec.go` 中的 `EnsureSpec()` → `DownloadSpecFile()` → `MergeAndWriteSpec()`。

### 1.2 两个 Spec Git 仓库

| | OSS 仓库 | ConfigMap 仓库 |
|---|---------|---------------|
| **Git 地址** | https://gitlab.scitix-inner.ai/lzi/sichek_spec | https://gitlab.scitix-inner.ai/siflow/config-server |
| 内容粒度 | 按 `{component}/{boardID}.yaml` 单文件 | 一个大的 `default_spec.yaml` |
| 消费方式 | sichek 运行时按需下载 | K8s 部署时挂载 |
| 更新频率 | 新硬件入库时 | 集群部署/升级时 |

两者的区别不是内容不同，而是**分发方式不同**。同一份 spec 数据走两条路到达节点。

### 1.3 运维实践中的高频问题

过往运维中，**显卡（NVIDIA GPU）和 IB 网络（HCA/InfiniBand）的 spec 缺失或错误**是最常发生的问题：

| 问题类型 | 表现 | 典型场景 | 业务影响 |
|---------|------|---------|---------|
| **Spec 缺失** | 新硬件 boardID 不在本地 spec 中，checker 跳过或报错 | 交付新集群时硬件型号未被 spec 收录 | 新节点无法纳管，交付延期 |
| **Spec 错误** | spec 中的硬件基线参数与实际不匹配 | 不同集群的硬件配置差异（如固件版本、OFED 版本不同），spec 未按集群区分 | 误报→需要通过 ignore checker 绕过，增加运维负担 |

> **当前的缓解手段**: 通过 `default_user_config.yaml` 中的 ignore checkers 配置，可以按集群分发不同的 user config，跳过与该集群不适用的检查项。但这本质上是**绕过而非修复**——正确的做法是让 spec 本身按集群/硬件准确描述基线，而不是靠关闭检查来消除误报。ignore checkers 越多，漏报风险越大。

> 这是推动 spec 管理体系重设计的**首要业务驱动力**，也是"按 boardID 从 OSS 下载缺失 spec"能力上线的直接原因。

### 1.4 核心问题

#### 问题 1：来源优先级不明确，一次启动 spec 文件被写入 5 次

```
init container: ConfigMap → cp → /host/var/sichek/config/default_spec.yaml  (第1次写入)
安装完成后:     再次 cp 同一文件                                               (第2次写入, deploy.yaml 267行 & 285行重复!)
daemon start:   EnsureSpecFile → 可能从 OSS 下载覆盖                          (第3次写入)
组件 LoadSpec:  FilterSpec → 检测到缺失 boardID → OSS 下载 → MergeAndWrite    (第4次写入)
组件 LoadSpec:  FilterSpec → 只保留本机 entry → 重写文件                       (第5次写入!)
```

读操作带写副作用（`FilterSpec` 会改写文件），是 bug 温床。

#### 问题 2：两个 Git 仓库维护同一类数据，无单一 source of truth

- OSS 仓库和 ConfigMap 仓库各自维护一份 spec，两者如何同步？
- A 改了 OSS 仓库忘了改 ConfigMap 仓库（或反过来）→ spec 不一致 → 线上误报/漏报
- 出问题时排查困难：节点的 spec 到底来自哪个仓库的哪个版本？

#### 问题 3：两条投递路径，两套维护流程，容易不一致

- **K8s**: ConfigMap 是 spec 到达节点的正式投递机制。流程：ConfigMap 挂载到 init container `/var/sichek/config/` → cp 到 host `/host/var/sichek/config/` → env 文件指定 daemon 启动参数 `-s /var/sichek/config/default_spec.yaml`，daemon 读取的就是 ConfigMap 投递的 spec。ConfigMap 按集群维护，可以针对不同集群分发不同的 spec 和 user config（含 ignore checkers）
- **裸金属**: 靠 RPM/DEB 内置的 spec + OSS 下载。大多数交付场景可以访问 OSS，但需要对网络隔离环境做兜底（见下方离线 spec 生成方案）

**问题在于**: 两条路径背后是两个 git 仓库（`config-server` 管 ConfigMap，`sichek_spec` 管 OSS），spec 内容需要人工保持同步。更新了 OSS 仓库的 spec 但没同步到 ConfigMap 仓库（或反过来），就会导致 K8s 集群和裸金属节点拿到的 spec 不一致

#### 问题 4：组件间 spec 模式不一致

| 组件 | Key 类型 | 文件组织 | 是否改写文件 |
|------|---------|---------|------------|
| NVIDIA | PCI Device ID (`0x20b210de`) | 9 个独立文件 | 是 (FilterSpec) |
| HCA | Board ID (`MT_0000000970`) | 1 个文件 26 entry | 否 (内存过滤) |
| InfiniBand | 集群名 + `default` | 1 个文件 | 否 |
| Ethernet | 无 key（单一 spec） | 1 个文件 | 否 |
| PCIe | 无 key | 1 个文件 | 否 |

#### 问题 5：节点端没有 spec 版本管理

Spec 在 git 仓库中有提交历史，但经过 ConfigMap/OSS 投递到节点后，版本信息丢失：

- 节点上的 spec 文件里没有版本号，无法知道当前用的是仓库的哪个版本
- 节点端没有回滚机制（只有 `.bak` 文件，但会被下次写入覆盖）
- 出问题时，仓库端可以查 git log，但无法快速确认"某个节点当前跑的是哪个版本的 spec"

#### 问题 6：责任不清晰

当前 spec 从制定到消费涉及多个角色，但职责边界模糊，出问题时容易互相推诿：

- ConfigMap 仓库 (`siflow/config-server`)：部署者维护，和 spec 内容维护者不是同一人
- OSS 仓库 (`lzi/sichek_spec`)：在个人名下，缺乏团队 review
- 新硬件到货时，谁负责录入 spec？谁负责审核？没有明确流程

### 1.5 角色与职责

项目涉及四个角色，在 spec 生命周期中的职责如下：

| 角色 | 职责 | 与 Spec 的关系 |
|------|------|---------------|
| **开发者** | 开发维护 sichek 代码，实现 checker 逻辑和 spec 加载机制 | spec 的**消费框架构建者**：定义 spec 结构、加载逻辑、校验规则。不负责 spec 内容的准确性 |
| **硬件领域专家**（显卡/IB） | 掌握硬件参数基线，制定各型号的 spec 内容 | spec 的**内容 owner**：负责 spec 的准确性（阈值、参数、boardID 映射）。新硬件入库时由该角色制定和审核 spec |
| **SRE（交付）** | 交付新集群时使用 sichek 做健康检查 | spec 的**交付消费者**：需要确保目标集群的 spec 齐全。发现 spec 缺失/错误时反馈给硬件专家 |
| **运维** | 日常运维，sichek 持续上报数据，做关联 cordon/告警 | spec 的**运营消费者**：依赖 spec 准确性保障告警质量。通过 user config（ignore checkers）做集群级调整 |

#### 协作关系

```
硬件领域专家                    开发者
  │ 制定/审核 spec 内容            │ 开发 spec 加载框架
  │ 新硬件入库                     │ 维护 checker 逻辑
  ▼                               ▼
┌──────────────────────────────────────┐
│         Spec 仓库 (siflow/sichek-config) │
│         CI/CD → OSS + ConfigMap        │
└──────────┬───────────────────────────┘
           │
     ┌─────┴─────┐
     ▼           ▼
   SRE         运维
   交付检查     持续监控
   ↓           ↓
   发现 spec    发现误报/漏报
   缺失/不适用  ↓
   ↓           反馈给硬件专家
   反馈给       或通过 user config
   硬件专家     调整 ignore checkers
```

#### 关键原则

- **spec 内容准确性由硬件领域专家负责**，不是开发者、不是 SRE、不是运维
- **spec 变更必须经硬件专家 review**，MR 的 approver 必须包含该角色
- **SRE 和运维是 spec 问题的发现者和反馈者**，不应直接修改 spec 内容
- **运维可以调整 user config**（ignore checkers 等），但这属于集群级运营配置，不等于修改 spec
- **开发者负责 spec 框架**（结构定义、加载逻辑、校验规则），确保硬件专家制定的 spec 能被正确消费

---

## 二、改进方案

### 设计原则

> **来源可信、变更可审、状态可查、离线可用**

### 2.1 合并为一个配置仓库，踢出 config-server

**目标：sichek 的所有配置（spec + user config）完全自治，不再依赖 config-server 仓库。**

当前 `config-server`（`siflow/config-server`）为 sichek 做两件事：

| 内容 | 说明 |
|------|------|
| `default_spec.yaml` | 全量 spec → ConfigMap 投递 |
| `default_user_config.yaml` | per-cluster 配置（ignore checkers、检查间隔等）→ ConfigMap 投递 |

两者都迁出，合并到一个新的团队仓库 `siflow/sichek-config`，以 `lzi/sichek_spec`（OSS 仓库）为基准，同时纳入 user config：

```
siflow/sichek-config/                   # 唯一的 sichek 配置仓库（团队归属）
├── specs/                              # spec 内容（硬件领域专家维护）
│   ├── nvidia/
│   │   ├── 0x20b210de.yaml             # A100 SXM4 80GB
│   │   ├── 0x26b510de.yaml             # H100
│   │   └── ...
│   ├── hca/
│   │   ├── MT_0000000970.yaml
│   │   └── ...
│   ├── infiniband/
│   │   └── default.yaml
│   ├── ethernet/
│   │   └── default.yaml
│   ├── pcie/
│   │   └── default.yaml
│   ├── transceiver/
│   │   └── default.yaml
│   └── VERSION                         # spec 版本号
├── clusters/                           # per-cluster user config（运维维护）
│   ├── cluster-abc.yaml
│   ├── cluster-xyz.yaml
│   └── default.yaml                    # 默认模板
└── .gitlab-ci.yml                      # 合并即发布

CI/CD pipeline (合并到 main 时自动触发):
    │
    ├──→ [specs/ 变更 → 发布到 OSS]
    │    按原有目录结构上传到 SICHEK_SPEC_URL/
    │    + 生成全量 default_spec.yaml
    │    → 供 sichek 运行时按 boardID 下载
```

权限通过 CODEOWNERS 按目录区分：
- `specs/` → 硬件领域专家审批
- `clusters/` → 运维自行管理

#### 踢出 config-server 的收益

| # | 收益 |
|---|------|
| 1 | **sichek 配置完全自治** — 不再依赖 config-server，减少跨仓库协调 |
| 2 | **config-server 解耦** — config-server 服务多个项目，sichek 的频繁变更不再影响其他项目 |
| 3 | **权限更精准** — CODEOWNERS 按目录区分：`specs/` 需硬件专家审批，`clusters/` 运维自行管理 |
| 4 | **CI/CD 链路缩短** — 一个仓库触发所有分发，不需要跨仓库同步 |
| 5 | **onboarding 更简单** — 新人只需关注一个仓库，不用理解 config-server 和 sichek_spec 的关系 |
| 6 | **一次 review 看全貌** — spec 变更和对应的 user config 调整可以在同一个 MR 里完成 |

#### 踢出 config-server 的代价

| # | 代价 | 应对 |
|---|------|------|
| 1 | 迁移成本 — 需要把 config-server 里所有集群的 user config 迁出 | 一次性工作，写脚本批量迁移 |
| 2 | config-server 可能有其他联动 — 拆出 sichek 部分需确认不破坏其他项目 | 迁移前梳理 config-server 的 CI/CD，确认 sichek 相关部分的边界 |
| 3 | 运维习惯改变 — 运维当前在 config-server 改 user config | 迁移后通知运维，仓库地址变更，操作方式不变（仍是改 YAML 提 MR） |

#### 合并实施路径

1. 在 `siflow/` 下创建 `sichek-config` 仓库
2. 导入 `lzi/sichek_spec` 内容到 `specs/` 目录
3. 从 `config-server` 迁移所有集群的 `default_user_config.yaml` 到 `clusters/` 目录
4. 加 CI pipeline：`specs/` 变更 → OSS 上传 + `default_spec.yaml` 生成；`clusters/` 变更 → 更新集群 ConfigMap
5. 配置 CODEOWNERS：`specs/` 需硬件专家审批，`clusters/` 运维自审
6. `config-server` 中移除 sichek 相关内容
7. 废弃 `lzi/sichek_spec`，README 标注已迁移

### 2.2 两层分层架构

```
┌─────────────────────────────────────────────────────┐
│  Layer 2: 运行时补充 (Runtime Supplement)             │
│  来源: OSS 按 boardID 下载缺失的设备 spec             │
│  场景: 遇到新硬件，内置 spec 中无此 boardID            │
│  K8s 和裸金属统一走此路径补充                         │
├─────────────────────────────────────────────────────┤
│  Layer 1: 内置基线 (Built-in Baseline)               │
│  来源: RPM/DEB 打包时内置默认 spec                    │
│  内容: 常见硬件的 spec，节点端按 boardID 过滤          │
│  K8s 和裸金属均以此为起点                             │
└─────────────────────────────────────────────────────┘

ConfigMap 不投递 spec，仅投递 default_user_config.yaml（per-cluster 运营配置）
```

### 2.3 二进制内置默认 spec

sichek 二进制打包时内置一份默认 spec（当前已有的模式，即 `components/<name>/config/default_*_spec.yaml`），保证：

- 裸金属离线场景安装后即可运行基本检查
- 新硬件或集群特定的 spec 通过 ConfigMap（K8s）或 OSS 下载（裸金属）补充，不需要全量打进二进制

### 2.4 消除读时写入，spec 加载变为纯函数

```go
// 当前 (有副作用):
func FilterSpec(file, rootKey, id) → 改写 file

// 改进 (纯函数):
func ResolveSpec(layers []SpecSource, boardID string) (*Spec, error)
// layers = [baseline, oss-download]
// 按优先级合并，返回最终 spec，不写文件
```

### 2.5 Spec 版本可观测

Spec 版本号由 `sichek-config` 仓库的 CI/CD 自动注入，不需要人工维护：

- **版本来源**: 仓库 git tag（如 `v1.2.3`）或 commit short hash（如 `a3b5c7d`）
- **注入时机**: CI/CD 发布到 OSS 时，自动在每个 spec 文件中写入版本信息
- **内置 spec**: sichek 构建时记录当时引用的 spec 仓库版本

```yaml
# CI/CD 自动注入，无需手动填写
metadata:
  spec_version: "v1.2.3"           # git tag，或 commit hash
  spec_commit: "a3b5c7d"           # 精确到 commit
  published_at: "2026-04-10T08:00:00Z"
```

运行时日志和 K8s annotation 中暴露当前 spec 版本：

```json
{
  "spec_version": "1.2.3",
  "devices": {
    "nvidia": {"board_id": "0x20b210de", "source": "built-in"},
    "hca": [
      {"board_id": "MT_0000000970", "source": "built-in"},
      {"board_id": "DEL0000000036", "source": "oss-download"}
    ]
  }
}
```

### 2.6 两个场景的清晰流程

**K8s 部署:**

```
DaemonSet 部署
  → init container 安装 sichek 到 host
  → daemon start:
      → 从 OSS 下载 default_spec.yaml + default_user_config.yaml
      → 按 boardID 过滤使用匹配的 spec
      → 缺失 boardID: 从 OSS 按 {component}/{boardID}.yaml 补充下载
```

**裸金属交付（可联网，常见情况）:**

```
RPM/DEB install
  → 内置 spec 作为 baseline
  → sichek run:
      → 从 OSS 下载 default_spec.yaml + default_user_config.yaml
      → 按 boardID 过滤使用匹配的 spec
      → 缺失 boardID: 从 OSS 按 {component}/{boardID}.yaml 补充下载
```

**裸金属交付（网络隔离，兜底）:**

```
RPM/DEB install
  → 内置 spec 作为 baseline
  → 交付工程师提前准备离线配置包（含 default_spec.yaml + default_user_config.yaml + 按 boardID 的设备 spec）
  → 放置到 /var/sichek/config/
  → sichek run: 使用内置 + 离线配置，无需网络
  → OSS 不可达时仅用本地配置，日志 warn
```

> 三个场景下 spec 获取方式统一：内置默认 + OSS 按 boardID 补充。不再通过 ConfigMap 投递 spec。

### 2.7 离线集群 Spec 快捷生成方案

网络隔离的裸金属交付场景，需要在**交付前**准备好该集群所需的全部 spec。提供以下工具链：

#### 方案 A：`sichek spec generate` 命令

根据硬件型号参数，从 spec 仓库批量生成，无需同型号节点：

```bash
# 指定硬件型号，从 OSS/spec 仓库拉取
sichek spec generate \
  --gpu 0x20b210de \
  --hca MT_0000000970,DEL0000000036 \
  --output cluster-abc-specs.tar.gz

# 或者从采购清单/CMDB 导入
sichek spec generate --from-inventory inventory.csv --output specs.tar.gz
```

#### 方案 B：后台平台导出

在后台管理平台的 Spec 管理模块中，按集群选择硬件配置，一键导出离线 spec 包：

```
后台平台 → 选择集群硬件型号 → 导出 spec 包 → 下载 tar.gz → 拷贝到离线节点
```

#### 离线交付标准流程

```
交付准备阶段（联网环境）:
  │
  ├─ 1. 确认目标集群硬件清单（GPU 型号、IB 网卡 board ID）
  │
  ├─ 2. 通过方案 A/B 任一方式生成离线配置包
  │
  ├─ 3. 将 spec 包与 sichek RPM/DEB 一起打入交付介质
  │
  └─ 4. （可选）在联网环境验证：sichek spec verify cluster-abc-specs.tar.gz

交付实施阶段（离线环境）:
  │
  ├─ 1. 安装 sichek RPM/DEB
  │
  ├─ 2. 解压 spec 包到 /var/sichek/config/
  │
  └─ 3. sichek run → 使用内置 + 离线 spec，无需网络
```

### 2.8 SRE 交付体验改进

> 注：集群级视图和交付报告输出已由交付检查工具解决，以下聚焦 spec 相关的两个改进。

#### 2.8.1 检查结果区分失败原因类型

当前所有检查失败都是 `abnormal`，SRE 无法判断是硬件坏了还是 spec 不对。建议在 `CheckerResult` 中增加 `ErrorType` 字段：

| ErrorType | 含义 | 判定依据 | SRE 该怎么做 |
|-----------|------|---------|-------------|
| `hardware_fault` | 硬件实际状态异常 | error count > 阈值、端口 down、温度过高等 | 报修，找硬件团队 |
| `spec_mismatch` | 硬件正常工作但与 spec 期望值不符 | 固件版本不一致、PCIe 速率不符预期、OFED 版本不同等 | 反馈给硬件专家，确认是 spec 要更新还是硬件要升级 |
| `spec_missing` | 本机 boardID 在 spec 中不存在 | boardID 查找失败 | 反馈给硬件专家录入 spec |

这个分类在 checker 代码层面已经隐含了——检测到 error count > 阈值是 `hardware_fault`，检测到版本不匹配是 `spec_mismatch`，检测到 boardID 找不到是 `spec_missing`。当前只是没有暴露出来。

实现改动不大：每个 checker 返回结果时标记 ErrorType 即可，不需要改 checker 判定逻辑本身。各错误项的处理建议可参考现有 wiki：https://acnizrso7ikb.feishu.cn/wiki/JDRTw5TFMiPrPhk1gSvcOnownZg

交付报告中可按 ErrorType 分组展示，SRE 一眼看清：

```
交付检查报告 - 集群 cluster-abc (128 节点)
─────────────────────────────────
硬件故障 (hardware_fault):     3 项  → 需报修
  node-017: GPU#4 NVLink error count 12 (阈值 0)
  node-089: HCA mlx5_0 端口 Down
  node-102: GPU#7 温度 92°C (阈值 85°C)

Spec 不匹配 (spec_mismatch):  15 项  → 需确认 spec 或硬件升级
  node-001~015: OFED 版本 5.8 vs spec 期望 5.9

Spec 缺失 (spec_missing):      0 项

硬件正常 (normal):            125 节点全部通过
```

---

## 三、管理平台建议

### 结论：不单独建平台，在现有后台管理平台中加 Spec 管理模块

Spec 管理的用户就是运维和交付团队，他们已经在用后台平台。单独平台意味着多一套登录、多一套权限、多一套部署，维护成本高。Spec 的核心操作就是 CRUD + 审批 + 下发，不需要独立系统的复杂度。

### 模块功能规划

```
后台管理平台
└── Spec 管理模块
    ├── Spec 库浏览        # 按组件/boardID 查看所有 spec，带搜索
    ├── Spec 编辑/新增     # 表单化编辑（而非直接改 YAML），字段校验
    ├── 变更审批流          # spec 变更需审批后才同步到 OSS / git 仓库
    ├── 集群 Spec 状态     # 每个集群/节点当前用的 spec 版本和来源
    └── 同步管理           # 手动/自动触发 spec 同步到 OSS
```

### 关键能力

| 能力 | 说明 | 解决的痛点 |
|------|------|-----------|
| **设备目录** | 维护 boardID → 硬件型号的映射表，新硬件入库时录入 | spec 缺失 |
| **字段校验** | NVLink 数量、PCIe 宽度等字段有合法值范围约束 | spec 错误 |
| **变更审批** | spec 修改走审批流，避免错误 spec 直接下发 | spec 错误 |
| **节点 spec 状态上报** | sichek daemon 上报当前 spec 版本+来源到后台 | 不知道节点用了什么 spec |
| **一键下发** | 审批通过后自动同步到 OSS，节点下次启动自动拉取 | 手动同步易出错 |

### 数据流

```
后台平台 (spec 编辑/审批)
    │
    ▼ 审批通过后自动推送
┌─────────┐
│ OSS 存储 │ ← siflow/sichek-config CI/CD 自动同步
└────┬────┘
     │ sichek 运行时按 boardID 拉取
     ▼
┌──────────┐
│ 各节点    │ → spec 状态上报回后台
└──────────┘
```

---

## 四、新硬件入库 SOP

当前新硬件到货后，spec 录入流程不清晰，导致 spec 缺失。建议明确标准流程：

```
新硬件到货
  │
  ├─ 1. 硬件领域专家确认硬件参数：型号、boardID、关键参数（NVLink数、PCIe宽度、端口速率等）
  │
  ├─ 2. 硬件领域专家在 `siflow/sichek-config` 仓库的 `specs/` 目录提交 MR，填写对应的 `specs/{component}/{boardID}.yaml`
  │     （后续可改为后台平台表单录入）
  │
  ├─ 3. 另一名硬件领域专家 review 审批
  │
  ├─ 4. 合并后 `sichek-config` CI 自动发布到 OSS + 生成 default_spec.yaml
  │
  └─ 5. 节点上的 sichek 下次启动时自动拉取新 spec
```

---

## 五、度量指标

如何衡量 spec 管理是否在变好：

| 指标 | 含义 | 目标 |
|------|------|------|
| **Spec 覆盖率** | 线上节点中，硬件 boardID 在 spec 库中有对应条目的比例 | 100% |
| **Spec 缺失告警数** | sichek 上报 `spec_missing` 类型错误的节点数 | → 0 |
| **Spec 变更导致的误报数** | 因 spec 变更引发的误 cordon / 误告警次数（按月） | → 0 |
| **Spec 版本一致性** | 同一集群内所有节点使用相同 spec 版本的比例 | 100% |

---

## 六、风险与不做的代价

| 如果不改进 | 后果 |
|-----------|------|
| 继续维护两个 spec 仓库 + 依赖 config-server | 不一致问题持续发生，跨仓库协调成本高，每次交付新集群都可能踩坑 |
| 不做节点端版本管理 | 出问题时无法快速定位和回滚，排查耗时长 |
| 不做离线 spec 工具 | 网络隔离的裸金属交付被卡住，依赖人工准备 spec |
| 不做管理平台集成 | spec 变更缺乏审批，错误 spec 直接上线的风险持续存在 |
| 不明确 spec owner | 责任不清，新硬件入库无人负责，spec 缺失反复出现 |
| 不区分 ErrorType | SRE 无法判断检查失败原因，交付效率低 |

---

## 七、实施优先级与路线图

| 阶段 | 内容 | 前置依赖 | 预期效果 |
|------|------|---------|---------|
| **P0** | 创建 `siflow/sichek-config`，合并 `lzi/sichek_spec` + 迁出 `config-server` 中 sichek 相关内容 | 无 | 单一 source of truth，sichek 配置完全自治 |
| **P0** | sichek spec 加载重构（纯函数，消除读时写入） | P0 | 消除副作用，架构清晰 |
| **P0** | `sichek-config` 加 CI/CD：`specs/` 变更→OSS + default_spec.yaml；`clusters/` 变更→集群 ConfigMap | P0 仓库创建 | 消除手动同步 |
| **P1** | CheckerResult 增加 ErrorType（hardware_fault/spec_mismatch/spec_missing） | 无 | SRE 能区分失败原因，提升交付效率 |
| **P1** | `sichek spec preflight` 预检命令 | 无 | 交付前提前发现 spec 缺失 |
| **P1** | 后台平台加 spec 浏览 + 节点 spec 状态查看（只读） | sichek 上报 spec 版本 | "看得清" |
| **P2** | 后台平台加 spec 编辑 + 审批流 | P1 浏览功能 | "管得住" |
| **P3** | 字段校验、变更 diff、历史回溯、度量 dashboard | P2 | 持续完善 |

---

## 八、改进总结

| 当前痛点 | 改进方向 |
|---------|---------|
| 4 个来源，优先级混乱 | 2 层分层（基线投递 + OSS 补充），优先级明确 |
| 2 个 spec 仓库 + 依赖 config-server | 合并为 `siflow/sichek-config`（specs/ + clusters/），踢出 config-server，CI/CD 自动分发 |
| 读操作有写副作用 | 纯函数加载，不改写文件 |
| 节点端无版本追溯 | spec VERSION + metadata + 节点上报 |
| spec 变更无审批、下发靠手动 | 后台平台 Spec 管理模块（浏览/编辑/审批/下发） |
| SRE 无法区分 spec 问题和硬件问题 | CheckerResult 增加 ErrorType 分类 |
| 交付前不知道 spec 是否齐全 | `sichek spec preflight` 预检命令 |
| 离线交付 spec 准备靠手动 | `sichek spec export/generate` 工具链 + 后台平台导出 |
