# IB Port-Level Congestion Index Checker

## Overview

Add a new InfiniBand checker that computes per-port congestion index from existing sysfs counters and alerts when congestion exceeds configured thresholds.

**Formula**: `congestion_index = delta(port_xmit_wait) / delta(port_xmit_packets)`

Where delta is the difference between two consecutive collection cycles.

## Data Source

Counters are already collected by `IBCounters.Collect()` from:
- `/sys/class/infiniband/{IBDev}/ports/1/counters/port_xmit_wait`
- `/sys/class/infiniband/{IBDev}/ports/1/counters/port_xmit_packets`

These values are cumulative 64-bit counters maintained by the HCA firmware. No new data collection is needed.

## Design Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Increment computation | Two consecutive collection cycles | Natural fit with existing QueryInterval-driven collection; no extra timers needed |
| State storage | Checker-internal `prevCounters` | Minimal change, doesn't touch collector; consistent with other checker patterns |
| Granularity | Per-port independent | Enables precise identification of congested ports |
| Alert levels | Dual threshold (Warning + Critical) | Distinguishes mild from severe congestion |
| Thresholds | Configurable in spec YAML | Allows per-cluster tuning |
| Metrics export | Raw counters already exported; congestion_index gauge optional | Prometheus can compute rate() from raw counters; checker adds sichek-native alerting |

## Checker Implementation

### New File: `components/infiniband/checker/ib_congestion.go`

```go
type IBCongestionChecker struct {
    name         string
    spec         *config.InfinibandSpec
    prevCounters map[string]collector.IBCounters
}
```

**Check() logic**:

1. If `prevCounters` is empty (first invocation): save current counters, return StatusNormal.
2. For each IB device in `infinibandInfo.IBCounters`:
   a. Read `port_xmit_wait` and `port_xmit_packets` (current values).
   b. Compute deltas against `prevCounters[ibDev]`.
   c. If `deltaPkt == 0`: skip (no traffic).
   d. Compute `congestionIndex = float64(deltaWait) / float64(deltaPkt)`.
3. Update `prevCounters` with current snapshot.
4. Determine result from the worst (highest) congestion index across all ports:
   - `> CriticalThreshold` (default 0.2): StatusAbnormal, LevelCritical
   - `> WarningThreshold` (default 0.05): StatusAbnormal, LevelWarning
   - Otherwise: StatusNormal
5. Detail includes: port name, congestion index value, deltaWait, deltaPkt.

**Counter overflow handling**: If `cur < prev` (64-bit counter wrap), treat delta as 0 and skip that port for this cycle.

## Config Changes

### check_items.go

New constant and CheckerResult entry:

```go
const CheckIBCongestion = "check_ib_congestion"

CheckIBCongestion: {
    Name:        "check_ib_congestion",
    Description: "Check IB port congestion index (XmitWait/XmitPkt)",
    Level:       consts.LevelWarning,
    ErrorName:   "IBCongestion",
    Suggestion:  "Check network traffic patterns and switch configuration",
}
```

### Spec Extension

Add threshold fields to `InfinibandSpec`:

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

## Registration

In `checker/checker.go`, add to `checkerConstructors`:

```go
config.CheckIBCongestion: NewIBCongestionChecker,
```

## Metrics (Optional)

Raw counters (`port_xmit_wait`, `port_xmit_packets`) are already exported as:
```
sichek_infiniband_counter{ib_dev="mlx5_0", counter_name="port_xmit_wait"}
sichek_infiniband_counter{ib_dev="mlx5_0", counter_name="port_xmit_packets"}
```

Prometheus/Grafana can compute `rate(port_xmit_wait) / rate(port_xmit_packets)` directly.

Optionally, a `sichek_infiniband_congestion_index{ib_dev="mlx5_0"}` gauge can be added in a follow-up.

## File Change Summary

| File | Change |
|------|--------|
| `components/infiniband/checker/ib_congestion.go` | **New** - checker implementation |
| `components/infiniband/checker/ib_congestion_test.go` | **New** - unit tests |
| `components/infiniband/config/check_items.go` | Add constant + CheckerResult |
| `components/infiniband/config/spec.go` | Add threshold fields to InfinibandSpec |
| `config/default_spec.yaml` | Add default thresholds |
| `components/infiniband/checker/checker.go` | Register new checker |
| `consts/consts.go` | Add CheckerID constant |

## Out of Scope

- Full-fabric congestion map (requires ibdiagnet2 from management node)
- Node-level aggregated congestion index
- Congestion index export as dedicated Prometheus gauge (follow-up)
