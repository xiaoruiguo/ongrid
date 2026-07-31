# `model.go` 技术实现文档

> 源文件：`internal/manager/model/metric/model.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/model/metric`

## 1. 概述

本文件是 metric 子域的 schema：三层时序数据——raw samples（10s cadence）/ 5m buckets / 1h buckets，加上 dead letter 表。设计要点：domain-facing value types（`Point` / `Bucket5m` / `Bucket1h`）**故意不带 GORM tag**，让 biz.Writer / biz.Reader 不泄漏 storage concerns 到 ingest path；GORM row types（`HostMetric` / `HostMetric5m` / `HostMetric1h`）是物理表形状，每表一行。红线：post-pivot 无 org_id，行按 (edge_id, ts) 索引；`DeadLetter` 保留 7d 后清理。

## 2. 包信息

- **包名**：`metric`
- **所属模块**：`internal/manager/model/`
- **依赖方向**：被 `manager/biz/metric` 的 Writer / Reader 与 `manager/data/metric` 调用；依赖 `github.com/ongridio/ongrid/internal/pkg/tunnel`、`time`

## 3. 关键类型与接口

```go
// Domain value type — 无 GORM tag，storage-agnostic
type Point struct {
    EdgeID      uint64
    Ts          time.Time
    CPUPct      float64
    MemPct      float64
    Load1       float64
    Load5       float64
    Load15      float64
    NetRxBps    uint64
    NetTxBps    uint64
    DiskUsedPct float64
}

// Bucket5m 是 5 分钟聚合行（domain value）
type Bucket5m struct {
    EdgeID      uint64
    Ts          time.Time // 5 分钟边界对齐
    CPUAvg, CPUMax      float64
    MemAvg, MemMax      float64
    Load1Avg, Load1Max  float64
    Load5Avg, Load5Max  float64
    Load15Avg, Load15Max float64
    NetRxSum, NetTxSum  uint64 // counter 字段 sum
    DiskUsedAvg, DiskUsedMax float64
}

// Bucket1h 是 1 小时聚合行，shape 同 Bucket5m 但 hour-aligned Ts
type Bucket1h struct { /* same fields as Bucket5m */ }

// GORM row types — 一表一 struct

type HostMetric struct {
    ID          uint64    `gorm:"primaryKey;autoIncrement;column:id"`
    EdgeID      uint64    `gorm:"index:idx_host_metrics_raw_edge_ts,priority:1;column:edge_id;not null"`
    Ts          time.Time `gorm:"index:idx_host_metrics_raw_edge_ts,priority:2;column:ts;not null"`
    CPUPct      float64   `gorm:"column:cpu_pct;not null"`
    // ... MemPct, Load1, Load5, Load15, NetRxBps, NetTxBps, DiskUsedPct
    CreatedAt   time.Time `gorm:"column:created_at;autoCreateTime"`
}

type HostMetric5m struct {
    EdgeID      uint64    `gorm:"primaryKey;priority:1;column:edge_id"`
    Ts          time.Time `gorm:"primaryKey;priority:2;column:ts"`
    // ... CPUAvg, CPUMax, MemAvg, MemMax, Load*Avg/Max, NetRxSum, NetTxSum, DiskUsedAvg/Max
}

type HostMetric1h struct { /* same as HostMetric5m, hour-aligned */ }

type DeadLetter struct {
    ID          uint64    `gorm:"primaryKey;autoIncrement;column:id"`
    EdgeID      uint64    `gorm:"column:edge_id;not null"`
    Ts          time.Time `gorm:"column:ts;not null"`
    // ... CPUPct, MemPct, Load*, NetRxBps, NetTxBps, DiskUsedPct
    ErrorReason string    `gorm:"column:error_reason;not null;size:256"`
    FailedAt    time.Time `gorm:"column:failed_at;autoCreateTime"`
    CreatedAt   time.Time `gorm:"column:created_at;autoCreateTime"`
}
```

## 4. 关键函数与流程

### `FromTunnelPoint`
- **签名**：`func FromTunnelPoint(edgeID uint64, p tunnel.HostMetricPoint) Point`
- **职责**：把 on-wire tunnel point 转 domain Point
- **流程**：
  1. `time.Unix(p.Ts, 0).UTC()` 把 unix 秒转 UTC time.Time
  2. 拷贝所有字段
  3. 返回 Point
- **关键：显式 UTC**：tunnel 时间戳是 unix 秒 UTC；转换后明确设 UTC 避免时区混淆

### `HostMetric.TableName / HostMetric5m.TableName / HostMetric1h.TableName / DeadLetter.TableName`
- 固定表名分别为 `host_metrics_raw` / `host_metrics_5m` / `host_metrics_1h` / `host_metrics_dead_letter`

## 5. 依赖关系

- **内部包**：`github.com/ongridio/ongrid/internal/pkg/tunnel`（HostMetricPoint wire 类型）
- **外部库**：`time`
- **被调用方**：`manager/biz/metric` 的 Writer / Reader；`manager/data/metric` 的 store；retention job（清理 DeadLetter）

## 6. 并发与资源管理

- 本文件仅定义 schema，无锁 / channel / 缓存
- `HostMetric5m` / `HostMetric1h` 用复合主键 (edge_id, ts)，upsert 覆盖更新
- `HostMetric` autoIncrement 主键；raw samples 追加写入
- `DeadLetter` autoIncrement 主键；retention job 定期清理（7d）

## 7. 设计模式与亮点

- **三层时序数据**：raw (10s) / 5m / 1h；trade-off 精度 vs 存储成本
- **Domain value types 无 GORM tag**：故意 storage-agnostic；biz.Writer / Reader 不泄漏 storage concerns 到 ingest path
- **GORM row types 一表一 struct**：物理表形状；data 层使用
- **Bucket5m vs Bucket1h 命名类型**：非 alias，防止 call site 误互换
- **Gauge vs Counter 聚合策略**：gauge 字段（CPU/Mem/Load/DiskUsed）带 avg + max；counter 字段（NetRx/Tx）带 sum
- **Ts 边界对齐**：5m Ts floor 到 5 分钟；1h Ts floor 到 1 小时
- **复合主键 upsert**：5m / 1h 表用 (edge_id, ts) 主键；同 bucket 重写覆盖
- **复合索引**：(edge_id, ts) 支持按 edge 时间范围查询
- **DeadLetter 7d 保留**：retention job 定期清理；ErrorReason 记录失败原因
- **UTC 显式**：FromTunnelPoint 显式设 UTC，避免时区混淆
- **Post-pivot 无 org_id**：行按 (edge_id, ts) 索引

## 8. 注意事项

- **Point / Bucket5m / Bucket1h 不落 DB**：仅 domain value；GORM row types 才落表
- **HostMetric autoIncrement 主键**：raw samples 不去重；append-only
- **HostMetric5m / HostMetric1h 复合主键**：(edge_id, ts) 联合；upsert 覆盖
- **Ts 必填**：raw / 5m / 1h / dead_letter 都 NOT NULL
- **EdgeID 必填**：所有表都 NOT NULL
- **DiskUsedPct 可空**：raw 表允许 NULL（未上报时）
- **DeadLetter 7d 清理**：retention job 定期 DELETE；本包不实现
- **ErrorReason size:256**：失败原因简短；长错误截断
- **FailedAt / CreatedAt 同值**：autoCreateTime 同时填两列
- **Bucket5m / Bucket1h 字段相同**：仅 Ts 对齐粒度不同；命名类型防互换
- **FromTunnelPoint 显式 UTC**：tunnel Ts 是 unix 秒 UTC；转换后保持 UTC
