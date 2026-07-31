# `reader.go` 技术实现文档

> 源文件：`internal/manager/data/metric/store/reader.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/metric/store`

## 1. 概述

本文件实现 `biz/metric.Reader` 的 GORM 落地，是 plain DAO——time window 选择由 biz QueryUsecase 驱动，reader 仅提供 raw / 5m / 1h 三层查询与 downsample scan。核心设计：所有查询按 `(edge_id, ts)` 或 `ts` 范围过滤，`ts ASC` 排序保证回放顺序；`ScanRawForDownsample` / `Scan5mForDownsample` 跨 edge 全表扫，按 `(edge_id, ts)` 排序便于 downsample job 聚合。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/metric`
- **依赖方向**：被 `internal/manager/biz/metric` 装配；依赖 `internal/manager/biz/metric`（接口）、`internal/manager/model/metric`、`gorm.io/gorm`。

## 3. 关键类型与接口

```go
// Reader 是 biz.Reader 的 GORM 实现。
type Reader struct {
    db *gorm.DB
}

var _ biz.Reader = (*Reader)(nil)
```

## 4. 关键函数与流程

### `NewReader`
- **签名**：`func NewReader(db *gorm.DB) *Reader`

### `QueryRaw`
- **签名**：`func (r *Reader) QueryRaw(ctx, edgeID uint64, from, to time.Time) ([]model.Point, error)`
- **职责**：返回 edgeID 在 `[from, to]` 的 raw 样本，`ts ASC` 排序。
- **流程**：Where `edge_id = ? AND ts BETWEEN ? AND ?` + Order + Find；逐行转 `model.Point`。

### `Query5m` / `Query1h`
- 同 QueryRaw，分别查 `host_metrics_5m` / `host_metrics_1h`，转 `model.Bucket5m` / `model.Bucket1h`。
- `Query5m` 用 `rowToBucket5m` 辅助函数转换；`Query1h` 内联转换。

### `ScanRawForDownsample`
- **签名**：`func (r *Reader) ScanRawForDownsample(ctx, from, to time.Time) ([]model.Point, error)`
- **职责**：返回 `[from, to]` 内跨全部 edge 的 raw 点，`edge_id ASC, ts ASC` 排序。
- **用途**：downsample job 扫描 raw → 5m 聚合，按 edge_id 分组便于聚合。

### `Scan5mForDownsample`
- 同 ScanRawForDownsample，查 5m 表 → `model.Bucket5m`，供 5m → 1h downsample。

### `rowToBucket5m`
- **签名**：`func rowToBucket5m(row model.HostMetric5m) model.Bucket5m`
- **职责**：HostMetric5m → Bucket5m 字段映射辅助函数。

## 5. 依赖关系

- **内部包**：`internal/manager/biz/metric`（接口）、`internal/manager/model/metric`
- **外部库**：`gorm.io/gorm`、`context`、`time`
- **被调用方**：`internal/manager/biz/metric` QueryUsecase（time window 选择）+ downsample job（Scan*）

## 6. 并发与资源管理

- **无显式锁**：纯读 DAO，并发安全。
- **ctx 透传**：所有方法首参 ctx。

## 7. 设计模式与亮点

- **plain DAO 原则**：reader 仅提供查询，time window 选择由 biz QueryUsecase 驱动，职责分离。
- **Scan* 跨 edge 全表扫**：downsample job 用，按 `(edge_id, ts)` 排序便于分组聚合。
- **rowToBucket5m 辅助函数**：集中 HostMetric5m → Bucket5m 转换，Query5m 与 Scan5mForDownsample 复用。
- **ts ASC 排序**：所有查询按 ts 升序，保证回放顺序。

## 8. 注意事项

- **plain DAO**：reader 不做 time window 选择，caller 需根据窗口选 QueryRaw / Query5m / Query1h。
- **Scan* 全表扫**：跨 edge 全表扫，大表需确保 ts 索引覆盖；limit 由 caller 控制（当前无 limit）。
- **rowToBucket5m 复用**：扩展 Bucket5m 字段需同步更新辅助函数 + Query5m 内联转换（Query1h 内联，需单独更新）。
- **Query1h 内联转换**：未用辅助函数，扩展字段需单独更新。
- **时间范围 BETWEEN**：`ts BETWEEN from AND to` 含两端；caller 需注意边界。
