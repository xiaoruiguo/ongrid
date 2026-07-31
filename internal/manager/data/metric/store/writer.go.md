# `writer.go` 技术实现文档

> 源文件：`internal/manager/data/metric/store/writer.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/metric/store`

## 1. 概述

本文件实现 `biz/metric.Writer` 的 GORM 落地，覆盖 raw / 5m / 1h 三层写入 + dead-letter + retention delete。核心设计：raw + DLQ 用 `CreateInBatches(500)`；5m + 1h 用 `Save`（upsert，rerun idempotent）；`deleteBefore` 用 `rowid IN (SELECT rowid ... LIMIT ?)` 子查询绕过 SQLite 默认未编译的 `DELETE ... LIMIT`；DLQ 每点一行（非 blob），便于运维查表。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/metric`
- **依赖方向**：被 `internal/manager/biz/metric` 装配；依赖 `internal/manager/biz/metric`（接口）、`internal/manager/model/metric`、`gorm.io/gorm`。

## 3. 关键类型与接口

```go
// Writer 是 biz.Writer 的 GORM 实现。
type Writer struct {
    db *gorm.DB
}

var _ biz.Writer = (*Writer)(nil)

const createInBatchesSize = 500  // gorm 内部批量大小

// 物理表名（镜像 model.*.TableName()）
const (
    tableRaw = "host_metrics_raw"
    table5m  = "host_metrics_5m"
    table1h  = "host_metrics_1h"
    tableDLQ = "host_metrics_dead_letter"
)
```

## 4. 关键函数与流程

### `NewWriter`
- **签名**：`func NewWriter(db *gorm.DB) *Writer`

### `WriteRaw`
- **签名**：`func (w *Writer) WriteRaw(ctx, batch []model.Point) error`
- **职责**：批量写 raw 点到 `host_metrics_raw`。
- **流程**：空 batch → nil；逐点转 `model.HostMetric`；`CreateInBatches(rows, 500)`。

### `WriteDeadLetter`
- **签名**：`func (w *Writer) WriteDeadLetter(ctx, batch []model.Point, reason string) error`
- **职责**：写失败点到 `host_metrics_dead_letter`，每点一行（非 blob），附 reason。
- **流程**：
  1. 空 batch → nil
  2. `len(reason) > 256` → 截断
  3. `failedAt = time.Now().UTC()`
  4. 逐点转 `model.DeadLetter`（含 ErrorReason + FailedAt）
  5. `CreateInBatches(rows, 500)`
- **关键约束**：每点一行，caller 查表看到每个丢失样本，非 blob。

### `Write5m` / `Write1h`
- **签名**：`Write5m(ctx, buckets []model.Bucket5m) error` + `Write1h(ctx, buckets []model.Bucket1h) error`
- **职责**：写 5m / 1h 聚合 bucket。
- **流程**：空 batch → nil；逐 bucket 转 `model.HostMetric5m` / `HostMetric1h`；`Save(&rows)`（upsert）。
- **关键约束**：`Save` 实现 upsert——rerun 5m / 1h job 幂等，`(edge_id, ts)` 冲突时覆盖。

### `DeleteRawBefore` / `Delete5mBefore` / `Delete1hBefore`
- **签名**：`DeleteRawBefore(ctx, cutoff time.Time, limit int) (int64, error)` + 同 5m / 1h
- **职责**：retention 删除，删 `ts < cutoff` 的行，limit 限制单次删除量。返回删除数。
- **实现**：委托 `deleteBefore`。

### `deleteBefore`
- **签名**：`func deleteBefore(ctx, db *gorm.DB, table string, cutoff time.Time, limit int) (int64, error)`
- **职责**：共享 limit-capped delete。
- **流程**：
  1. `limit <= 0` → 返回 0
  2. `DELETE FROM <table> WHERE rowid IN (SELECT rowid FROM <table> WHERE ts < ? LIMIT ?)`
  3. Exec + 返回 RowsAffected
- **关键约束**：SQLite 默认未编译 `DELETE ... LIMIT`，用 `rowid IN (SELECT rowid ... LIMIT ?)` 子查询绕过。

## 5. 依赖关系

- **内部包**：`internal/manager/biz/metric`（接口）、`internal/manager/model/metric`
- **外部库**：`gorm.io/gorm`、`context`、`fmt`、`time`
- **被调用方**：`internal/manager/biz/metric` Ingester（WriteRaw + WriteDeadLetter）+ downsample job（Write5m + Write1h）+ retention goroutine（Delete*Before）

## 6. 并发与资源管理

- **无显式锁**：依赖 gorm 与 DB。
- **批量写**：raw + DLQ 用 `CreateInBatches(500)`；5m + 1h 用 `Save`（全字段 upsert）。
- **ctx 透传**：所有方法首参 ctx。

## 7. 设计模式与亮点

- **raw + DLQ CreateInBatches vs 5m + 1h Save**：raw 是 append-only，用 CreateInBatches 高效；5m / 1h 是 upsert（rerun 幂等），用 Save。
- **DLQ 每点一行**：非 blob，caller 查表看每个丢失样本，便于运维。
- **deleteBefore rowid 子查询**：绕过 SQLite 默认未编译的 `DELETE ... LIMIT`，跨方言可移植。
- **reason 截断 256**：防止超长 reason 撑爆 DLQ 表。
- **Save upsert 幂等**：rerun 5m / 1h job 不产生重复行，`(edge_id, ts)` 冲突覆盖。

## 8. 注意事项

- **raw append-only**：WriteRaw 不 upsert，重复写会产生重复行；caller 需保证不重复写。
- **5m / 1h upsert**：Save 全字段更新；rerun 幂等但会覆盖已有行，caller 需保证 bucket 计算正确。
- **deleteBefore limit**：limit ≤ 0 返回 0；caller 需传正 limit。
- **deleteBefore rowid 子查询**：依赖 rowid（SQLite 内置 / MySQL InnoDB 也支持）；其他方言需验证。
- **DLQ reason 截断 256**：超长 reason 丢失尾部；如需完整 reason 需扩展列。
- **createInBatchesSize 500**：批量大小硬编码；超大 batch caller 需分块。
- **tableRaw / 5m / 1h / DLQ 常量**：镜像 model TableName()，扩展表需同步更新。
