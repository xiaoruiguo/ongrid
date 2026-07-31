# `migrate.go` 技术实现文档

> 源文件：`internal/manager/data/metric/store/migrate.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/metric/store`

## 1. 概述

本文件是 metric store 的 schema 入口，AutoMigrate 四张表：`host_metrics_raw`（原始样本）+ `host_metrics_5m`（5 分钟聚合）+ `host_metrics_1h`（1 小时聚合）+ `host_metrics_dead_letter`（死信）。关键设计：聚合表复合主键通过 `primaryKey;priority:N` tag 表达，让 MySQL 与 SQLite 都收到正确的 `(edge_id, ts)` PK 顺序。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/metric`
- **依赖方向**：被 `cmd/ongrid` 启动调用；依赖 `gorm.io/gorm`、`internal/manager/model/metric`。

## 3. 关键类型与接口

无自定义类型；使用 `model.HostMetric` / `model.HostMetric5m` / `model.HostMetric1h` / `model.DeadLetter`。

## 4. 关键函数与流程

### `Migrate`
- **签名**：`func Migrate(db *gorm.DB) error`
- **职责**：AutoMigrate 四张表。
- **关键约束**：聚合表复合主键通过 `primaryKey;priority:N` tag 表达，跨方言 PK 顺序一致。

## 5. 依赖关系

- **内部包**：`internal/manager/model/metric`
- **外部库**：`gorm.io/gorm`
- **被调用方**：`cmd/ongrid` 启动序列

## 6. 并发与资源管理

- **无锁**：启动期串行执行。

## 7. 设计模式与亮点

- **三层聚合 + dead-letter**：raw / 5m / 1h 三层聚合 + 死信表，匹配时序数据 downsample 模式。
- **复合主键 priority tag**：`primaryKey;priority:N` 让 gorm 跨方言生成正确 PK 顺序，避免 MySQL / SQLite 差异。
- **dead-letter 单独表**：失败点单独存储，便于运维查表与重处理。

## 8. 注意事项

- **预生产阶段**：生产应迁至版本化 SQL migration 文件。
- **AutoMigrate 不删列**：废弃列需显式 migration 清理。
- **复合主键 priority tag**：扩展聚合表 PK 需同步更新 model tag。
- **Phase 2 升级**：`metric/clickhouse` 升级后，历史数据可能迁至 ClickHouse，本 schema 仍承担实时写入。
- **聚合表 PK (edge_id, ts)**：保证同 edge 同时间戳行唯一，Writer 用 Save（upsert）覆盖。
