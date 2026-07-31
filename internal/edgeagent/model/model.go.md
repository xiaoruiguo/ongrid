# `model.go` 技术实现文档

> 源文件：`internal/edgeagent/model/model.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/model`

## 1. 概述

本文件定义 edgeagent 本地 metric 与 process 数据结构：`HostMetric`（push_host_metrics 用的 8 字段样本）和 `ProcessInfo`（get_process_top 响应单条目）。刻意与 `manager/model/metric` 类型结构相似但独立维护，以保留 BC（业务能力）边界——edge 不允许 import `manager/**`。

## 2. 包信息

- **包名**：`model`
- **所属模块**：edgeagent 本地类型层
- **依赖方向**：被 `biz/collector/*`（Phase 1 桩）引用；不依赖其他业务包

## 3. 关键类型与接口

```go
// HostMetric 是 push_host_metrics 推送的样本
type HostMetric struct {
    Ts          time.Time
    CPUPct      float32
    MemPct      float32
    Load1       float32
    Load5       float32
    Load15      float32
    NetRxBps    uint64
    NetTxBps    uint64
    DiskUsedPct float32
}

// ProcessInfo 是 get_process_top 响应单条目
type ProcessInfo struct {
    PID     int32
    Name    string
    CPUPct  float32
    MemRSS  uint64
    Command string
}
```

## 4. 关键函数与流程

无函数定义。仅类型声明 + 包注释。

## 5. 依赖关系

- **内部包**：无
- **外部库**：标准库 `time`
- **被调用方**：`internal/edgeagent/biz/collector/cpu.go` / `mem.go` / `net.go`（Phase 1 桩返回零值 HostMetric）

## 6. 并发与资源管理

无并发控制。类型本身是数据结构，无行为。

## 7. 设计模式与亮点

- **类型复制而非 import**：与 `manager/model/metric` 结构相似但独立——保留 BC 边界，edge 不依赖 manager 包
- **float32 而非 float64**：metric 字段用 float32 节省内存（推送大量样本时）；精度足够（CPU% 不需要 15 位有效数字）
- **uint64 for 网络字节**：高带宽场景字节计数可能超 int32 范围
- **PID 用 int32**：与 gopsutil `process.Process.Pid` 类型一致

## 8. 注意事项

- 这些类型**未被生产路径使用**——真实采集走 `internal/edgeagent/collector/embedded.go`，其 `CollectorOutput.HostPoint` 用的是 `tunnel.HostMetricPoint`（不是 `model.HostMetric`）；`biz/collector/` 的 Phase 1 桩返回 `model.HostMetric` 但目前无调用方
- `ProcessInfo` 字段 `MemRSS uint64` 与 `tunnel.ProcessInfo.MemPct float64` 不一致——是不同 metric（RSS bytes vs 内存百分比），但命名相似易混淆
- `Command` 字段名（vs `tunnel.ProcessInfo.Cmdline`）——同样的字段在不同类型中命名不同
- 若 Phase 1 桩被移除，本文件可能成为无引用代码——清理前需确认 `biz/collector/*.go` 的状态
- 修改这些类型不会影响生产——因为生产用 `tunnel.*` 类型；本文件仅是 Phase 1 遗留
- 包注释提到「edge 不允许 import manager/**」——这是 BC 边界约束，未来如需共享类型应通过 `internal/shared/` 或显式接口
