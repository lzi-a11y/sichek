# IB 端口级拥塞指数检查器

## 概述

新增一个 InfiniBand 检查器，基于现有 sysfs 计数器计算每端口拥塞指数，当拥塞超过配置的阈值时触发告警。

**公式**：`congestion_index = delta(port_xmit_wait) / delta(port_xmit_packets)`

其中 delta 是两次连续采集周期之间的差值。

## 数据源

计数器已由 `IBCounters.Collect()` 从以下路径采集：
- `/sys/class/infiniband/{IBDev}/ports/1/counters/port_xmit_wait`
- `/sys/class/infiniband/{IBDev}/ports/1/counters/port_xmit_packets`

这些值是 HCA 固件维护的累计 64 位计数器，无需新增数据采集。

## 设计决策

| 决策项 | 选择 | 理由 |
|--------|------|------|
| 增量计算方式 | 两次连续采集周期的差值 | 天然适配现有 QueryInterval 驱动的采集机制，无需额外定时器 |
| 状态存储位置 | Checker 内部 `prevCounters` | 改动最小，不动 collector；与现有 checker 模式一致 |
| 检查粒度 | 每端口独立 | 能精确定位拥塞端口 |
| 告警级别 | 双阈值（Warning + Critical） | 区分轻微拥塞和严重拥塞 |
| 阈值配置 | 在 spec YAML 中可配置 | 允许按集群调优 |
| 指标导出 | 原始计数器已导出；congestion_index gauge 可选 | Prometheus 可从原始计数器用 rate() 自行计算；checker 提供 sichek 原生告警 |

## Checker 实现

### 新文件：`components/infiniband/checker/ib_congestion.go`

```go
type IBCongestionChecker struct {
    name         string
    spec         *config.InfinibandSpec
    prevCounters map[string]collector.IBCounters
}
```

**Check() 逻辑**：

1. 若 `prevCounters` 为空（首次调用）：保存当前计数器快照，返回 StatusNormal。
2. 遍历 `infinibandInfo.IBCounters` 中的每个 IB 设备：
   a. 读取 `port_xmit_wait` 和 `port_xmit_packets`（当前值）。
   b. 与 `prevCounters[ibDev]` 计算增量。
   c. 若 `deltaPkt == 0`：跳过（无流量）。
   d. 计算 `congestionIndex = float64(deltaWait) / float64(deltaPkt)`。
3. 用当前快照更新 `prevCounters`。
4. 取所有端口中最差（最高）的拥塞指数判定结果：
   - `> CriticalThreshold`（默认 0.2）：StatusAbnormal，LevelCritical
   - `> WarningThreshold`（默认 0.05）：StatusAbnormal，LevelWarning
   - 否则：StatusNormal
5. Detail 包含：端口名、拥塞指数值、deltaWait、deltaPkt。

**计数器溢出处理**：若 `cur < prev`（64 位计数器回绕），将 delta 视为 0 并跳过该端口本周期的检查。

## 配置变更

### check_items.go

新增常量和 CheckerResult 条目：

```go
const CheckIBCongestion = "check_ib_congestion"

CheckIBCongestion: {
    Name:        "check_ib_congestion",
    Description: "检查 IB 端口拥塞指数（XmitWait/XmitPkt）",
    Level:       consts.LevelWarning,
    ErrorName:   "IBCongestion",
    Suggestion:  "检查网络流量模式和交换机配置",
}
```

### Spec 扩展

在 `InfinibandSpec` 中新增阈值字段：

```go
CongestionWarningThreshold  float64 `json:"congestion_warning_threshold" yaml:"congestion_warning_threshold"`
CongestionCriticalThreshold float64 `json:"congestion_critical_threshold" yaml:"congestion_critical_threshold"`
```

### default_spec.yaml

```yaml
infiniband:
  default:
    congestion_warning_threshold: 0.05
    congestion_critical_threshold: 0.2
```

## 注册

在 `checker/checker.go` 的 `checkerConstructors` 中添加：

```go
config.CheckIBCongestion: NewIBCongestionChecker,
```

## 指标导出（可选）

原始计数器（`port_xmit_wait`、`port_xmit_packets`）已作为以下 Prometheus 指标导出：
```
sichek_infiniband_counter{ib_dev="mlx5_0", counter_name="port_xmit_wait"}
sichek_infiniband_counter{ib_dev="mlx5_0", counter_name="port_xmit_packets"}
```

Prometheus/Grafana 可直接计算 `rate(port_xmit_wait) / rate(port_xmit_packets)`。

后续可选新增 `sichek_infiniband_congestion_index{ib_dev="mlx5_0"}` gauge。

## 文件变更清单

| 文件 | 变更 |
|------|------|
| `components/infiniband/checker/ib_congestion.go` | **新增** — checker 实现 |
| `components/infiniband/checker/ib_congestion_test.go` | **新增** — 单元测试 |
| `components/infiniband/config/check_items.go` | 新增常量 + CheckerResult |
| `components/infiniband/config/spec.go` | InfinibandSpec 新增阈值字段 |
| `config/default_spec.yaml` | 新增默认阈值 |
| `components/infiniband/checker/checker.go` | 注册新 checker |
| `consts/consts.go` | 新增 CheckerID 常量 |

## 不在本次范围内

- 全网拥塞热力图（需要从管理节点运行 ibdiagnet2）
- 节点级聚合拥塞指数
- 拥塞指数作为独立 Prometheus gauge 导出（后续迭代）
