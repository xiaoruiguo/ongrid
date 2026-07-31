# `migrate.go` 技术实现文档

> 源文件：`internal/manager/data/imbridge/store/migrate.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/imbridge/store`

## 1. 概述

本文件是 IM bridge store 的 schema 入口，AutoMigrate 两张表：`im_apps`（平台 bot 凭据）与 `im_threads`（IM 会话 → ongrid session 映射）。由 `cmd/ongrid` 通过 `dbx.RunMigrations` 启动。使用 `dbx.NeedsDeleteMarkerMigration` 检测 legacy soft-delete 列，需先 drop 受影响索引再 AutoMigrate 重建。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/imbridge`
- **依赖方向**：被 `cmd/ongrid` 启动调用；依赖 `gorm.io/gorm`、`internal/manager/model/imbridge`、`internal/pkg/dbx`。

## 3. 关键类型与接口

无自定义类型；使用 `model.ImApp` 与 `model.ImThread`。

## 4. 关键函数与流程

### `Migrate`
- **签名**：`func Migrate(db *gorm.DB) error`
- **流程**：
  1. `dbx.NeedsDeleteMarkerMigration(im_apps)` → `dbx.DropIndexes(uk_provider_app_id)`
  2. `AutoMigrate(ImApp, ImThread)`
  3. `dbx.BackfillDeleteMarker(im_apps)`
- **错误处理**：每步错误立即返回。
- **关键约束**：仅 `im_apps` 走 delete_marker 迁移；`im_threads` 无需（无 legacy 索引）。

## 5. 依赖关系

- **内部包**：`internal/manager/model/imbridge`、`internal/pkg/dbx`
- **外部库**：`gorm.io/gorm`
- **被调用方**：`cmd/ongrid` 启动序列（`dbx.RunMigrations`）

## 6. 并发与资源管理

- **无锁**：启动期串行执行。

## 7. 设计模式与亮点

- **dbx soft-delete 迁移模式**：`NeedsDeleteMarkerMigration` 检测 legacy `deleted_at`，先 drop `uk_provider_app_id` 唯一索引，AutoMigrate 重建为 delete_marker 形态，再 BackfillDeleteMarker。
- **仅 im_apps 迁移**：im_threads 无 legacy 索引，无需 drop。

## 8. 注意事项

- **dbx.NeedsDeleteMarkerMigration**：检测 legacy soft-delete 列；post-launch 表已迁移则 no-op。
- **DropIndexes 顺序**：必须在 AutoMigrate 之前 drop，否则重建索引撞冲突。
- **预生产阶段**：生产应迁至版本化 SQL migration 文件。
