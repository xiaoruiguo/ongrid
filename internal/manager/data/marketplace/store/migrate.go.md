# `migrate.go` 技术实现文档

> 源文件：`internal/manager/data/marketplace/store/migrate.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/marketplace/store`

## 1. 概述

本文件是 marketplace store 的 schema 入口，AutoMigrate 单张 `installed_skills` 表（marketplace lock 表）。注释明示包名"sqlite"是历史约定，AutoMigrate 实际方言无关，MySQL/SQLite 通用。使用 `dbx.NeedsDeleteMarkerMigration` 检测 legacy soft-delete 列，需先 drop `idx_tenant_pack` 索引再 AutoMigrate 重建。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/marketplace`
- **依赖方向**：被 `cmd/ongrid` 启动调用；依赖 `gorm.io/gorm`、`internal/manager/model/marketplace`、`internal/pkg/dbx`。

## 3. 关键类型与接口

无自定义类型；使用 `model.InstalledPack`。

## 4. 关键函数与流程

### `Migrate`
- **签名**：`func Migrate(db *gorm.DB) error`
- **流程**：
  1. `dbx.NeedsDeleteMarkerMigration(installed_skills)` → `dbx.DropIndexes(idx_tenant_pack)`
  2. `AutoMigrate(InstalledPack)`
  3. `dbx.BackfillDeleteMarker(installed_skills)`

## 5. 依赖关系

- **内部包**：`internal/manager/model/marketplace`、`internal/pkg/dbx`
- **外部库**：`gorm.io/gorm`
- **被调用方**：`cmd/ongrid` 启动序列（`dbx.RunMigrations`）

## 6. 并发与资源管理

- **无锁**：启动期串行执行。

## 7. 设计模式与亮点

- **dbx soft-delete 迁移模式**：`NeedsDeleteMarkerMigration` 检测 legacy `deleted_at`，先 drop `idx_tenant_pack`（含 deleted_at 的唯一索引），AutoMigrate 重建为 delete_marker 形态，再 BackfillDeleteMarker。
- **命名约定注释**：明示包名"sqlite"是历史约定，实际方言无关。

## 8. 注意事项

- **dbx.NeedsDeleteMarkerMigration**：检测 legacy soft-delete 列；post-launch 表已迁移则 no-op。
- **DropIndexes 顺序**：必须在 AutoMigrate 之前 drop，否则重建索引撞冲突。
- **预生产阶段**：生产应迁至版本化 SQL migration 文件。
- **包名笔误**：注释"Package sqlite"实际包名为 `store`，历史遗留。
