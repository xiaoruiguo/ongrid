# repo.go

## 1. 概述

`repo.go` 是 metric 包的存储接口声明文件，定义 `Writer` 与 `Reader` 两个窄接口。无任何实现 —— 具体实现在 `internal/manager/data/metric/store`（SQLite）与未来的 `.../clickhouse`。

接口用 domain `Point` / `Bucket` 类型（纯 Go struct，无 gorm tag），让 biz 层不关心数据落哪张表、哪个存储。

## 2. 包信息

- 包名：`metric`
- 路径：`internal/manager/biz/metric`

## 3. 关键类型与接口

### Writer 接口

```go
type Writer interface {
    WriteRaw(ctx, batch []model.Point) error
    WriteDeadLetter(ctx, batch []model.Point, reason string) error
    Write5m(ctx, buckets []model.Bucket5m) error
    Write1h(ctx, buckets []model.Bucket1h) error

    DeleteRawBefore(ctx, cutoff time.Time, limit int) (int64, error)
    Delete5mBefore(ctx, cutoff time.Time, limit int) (int64, error)
    Delete1hBefore(ctx, cutoff time.Time, limit int) (int64, error)
}
```

### Reader 接口

```go
type Reader interface {
    QueryRaw(ctx, edgeID uint64, from, to time.Time) ([]model.Point, error)
    Query5m(ctx, edgeID uint64, from, to time.Time) ([]model.Bucket5m, error)
    Query1h(ctx, edgeID uint64, from, to time.Time) ([]model.Bucket1h, error)

    ScanRawForDownsample(ctx, from, to time.Time) ([]model.Point, error)
    Scan5mForDownsample(ctx, from, to time.Time) ([]model.Bucket5m, error)
}
```

## 4. 关键函数与流程

本文件无函数实现，纯接口声明。

### 接口语义说明

- **`WriteRaw`**：批量写 raw 样本到 `host_metrics_raw`
- **`WriteDeadLetter`**：写失败重试耗尽后转 dead-letter 表，附 `reason`
- **`Write5m` / `Write1h`**：写下采样后的桶
- **`Delete*Before`**：删除 `cutoff` 之前的行，限 `limit` 行，返回删除数。调用方循环直到返回 0
- **`Query*`**：按 `edgeID` + `[from, to]` 闭区间查询，ts 升序。Post-pivot scoping 只按 `edge_id`，无 `org_id`
- **`ScanRawForDownsample`**：跨所有 edge 扫描 raw 点，按 `(edge_id, ts)` 升序，喂给 5m 聚合器
- **`Scan5mForDownsample`**：跨所有 edge 扫描 5m 桶，喂给 1h 聚合器

## 5. 依赖关系

### 内部包

- `model "github.com/ongridio/ongrid/internal/manager/model/metric"` —— `Point` / `Bucket5m` / `Bucket1h`

### 被谁实现

- `internal/manager/data/metric/store`（SQLite 实现）
- 未来 `.../clickhouse`

### 被谁调用

- `ingester.go` 的 `Ingester` 用 `Writer.WriteRaw` / `WriteDeadLetter`
- `downsample.go` 的 `Downsampler` 用 `Writer.Write5m` / `Write1h` + `Reader.ScanRawForDownsample` / `Scan5mForDownsample`
- `retention.go` 的 `Retention` 用 `Writer.Delete*Before`
- `query.go` 的 `QueryUsecase` 用 `Reader.Query*`

## 6. 并发与资源管理

不适用（纯接口声明）。

## 7. 设计模式与亮点

### 存储无关接口

接口用 domain 类型（`Point` / `Bucket`），无 gorm tag、无 SQL。biz 层不关心数据落 SQLite 还是 ClickHouse。这让存储切换是 data 层的事，biz 层零改动。

### Writer / Reader 分离

写路径（ingest + downsample + retention）与读路径（query）分到两个接口。实现方可分别注入不同 backend（如写 SQLite 读 ClickHouse）。

### Delete*Before 分批语义

`Delete*Before(ctx, cutoff, limit) (int64, error)` 返回删除数。调用方循环直到 0。这让 retention 能分批删除，避免长事务锁全表（SQLite writer lock 注释提及）。

### Scan 跨所有 edge

`ScanRawForDownsample` / `Scan5mForDownsample` 不带 `edgeID`，返回所有 edge 的数据。这是下采样器的需求 —— 它要按 edge 分组聚合。注释明确按 `(edge_id, ts)` 升序，让聚合器 cheaply group。

### 闭区间 [from, to]

所有 `Query*` / `Scan*` 是闭区间。`downsample.go` 的 `Run5m` 传 `to-1ns` 防下一桶首样本泄漏 —— 这个细节依赖闭区间语义。

## 8. 注意事项

- **无 `org_id` scoping**：注释明确"post-pivot scoping is by edge_id alone; there is no org_id to enforce"。多租户隔离由 edge_id 隐式保证（edge 属于某 tenant）
- **`Delete*Before` 必须分批**：调用方应循环直到返回 0。一次性删大批量会锁表
- **`Scan*ForDownsample` 跨所有 edge**：大部署下可能返回海量数据，应考虑流式或分 edge 扫描
- **接口未含 `EnsureSchema`**：表结构创建由 data 层自己处理（boot 时）。biz 层不关心
- **`WriteDeadLetter` 的 reason 是自由文本**：调用方（ingester）应截断到合理长度（ingester 截到 256）
- **`Query*` 返回 ts 升序**：调用方依赖此顺序（如 SPA 渲染时序图）。实现方必须保证
- **接口窄但非最小**：`Writer` 含 7 个方法，`Reader` 含 5 个。比单一 `Write` / `Read` 接口大，但每个方法对应明确用途，避免 fake 实现填一堆空方法
