# `migrate.go` 技术实现文档

> 源文件：`internal/manager/data/edge/store/migrate.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/edge/store`

## 1. 概述

本文件是 edge store 的 schema 入口，AutoMigrate `edges` + `edge_plugin_configs` 两张表。注释明示"dialect-agnostic and suitable for both MySQL and SQLite"，由 `cmd/ongrid` 通过 `dbx.RunMigrations` 启动。使用 `dbx.NeedsDeleteMarkerMigration` 检测 legacy soft-delete 列，需先 drop 受影响索引再 AutoMigrate 重建。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/edge`
- **依赖方向**：被 `cmd/ongrid` 启动调用；依赖 `gorm.io/gorm`、`internal/manager/model/edge`、`internal/pkg/dbx`。

## 3. 关键类型与接口

无自定义类型；使用 `model.Edge` 与 `model.PluginConfig`。

## 4. 关键函数与流程

### `Migrate`
- **签名**：`func Migrate(db *gorm.DB) error`
- **流程**：
  1. `dbx.NeedsDeleteMarkerMigration(edges)` → `dbx.DropIndexes(idx_edges_access_key_id)`
  2. `dbx.NeedsDeleteMarkerMigration(edge_plugin_configs)` → `dbx.DropIndexes(uk_edge_plugin)`
  3. `AutoMigrate(Edge, PluginConfig)`
  4. `dbx.BackfillDeleteMarker(edges)`
  5. `dbx.BackfillDeleteMarker(edge_plugin_configs)`
- **错误处理**：每步错误立即返回。

## 5. 依赖关系

- **内部包**：`internal/manager/model/edge`、`internal/pkg/dbx`
- **外部库**：`gorm.io/gorm`
- **被调用方**：`cmd/ongrid` 启动序列（`dbx.RunMigrations`）

## 6. 并发与资源管理

- **无锁**：启动期串行执行。

## 7. 设计模式与亮点

- **dbx soft-delete 迁移模式**：`NeedsDeleteMarkerMigration` 检测 legacy `deleted_at` 列，需先 drop 受影响唯一索引（含 deleted_at 列），AutoMigrate 重建为 `delete_marker` 形态，再 `BackfillDeleteMarker` 填充历史行。
- **方言无关**：注释明示 MySQL + SQLite 通用。

## 8. 注意事项

- **dbx.NeedsDeleteMarkerMigration**：检测 legacy soft-delete 列；post-launch 表已迁移则 no-op。
- **DropIndexes 顺序**：必须在 AutoMigrate 之前 drop 受影响索引，否则 AutoMigrate 重建索引时撞冲突。
- **BackfillDeleteMarker**：填充历史行的 delete_marker = 0（live row sentinel）。
- **预生产阶段**：生产应迁至版本化 SQL migration 文件。
