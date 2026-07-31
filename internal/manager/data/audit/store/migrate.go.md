# `migrate.go` 技术实现文档

> 源文件：`internal/manager/data/audit/store/migrate.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/audit/store`

## 1. 概述

本文件是 audit store 的 schema 入口，仅 AutoMigrate 单张 `audit_logs` 表（HLD-010）。Migration 由 `cmd/ongrid` 通过 `dbx.RunMigrations` 组装。红线：生产 schema 演进应迁至版本化 SQL migration 文件。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/audit`
- **依赖方向**：被 `cmd/ongrid` 启动调用；依赖 `gorm.io/gorm`、`internal/manager/model/audit`。

## 3. 关键类型与接口

无自定义类型；使用 `model.Log`。

## 4. 关键函数与流程

### `Migrate`
- **签名**：`func Migrate(db *gorm.DB) error`
- **职责**：`db.AutoMigrate(&model.Log{})`。

## 5. 依赖关系

- **内部包**：`internal/manager/model/audit`
- **外部库**：`gorm.io/gorm`
- **被调用方**：`cmd/ongrid` 启动序列（`dbx.RunMigrations`）

## 6. 并发与资源管理

- **无锁**：启动期串行执行。

## 7. 设计模式与亮点

- **单表单行 migrate**：极简，便于审计。
- **HLD-010 关联**：注释明示对应 HLD-010 audit_logs 设计文档。

## 8. 注意事项

- **预生产阶段**：生产应迁至版本化 SQL migration 文件。
- **AutoMigrate 不删列**：废弃列需显式 migration 清理。
- **append-only 表**：audit_logs 仅追加，唯一 mutation 是 retention sweep（见 repo.go `DeleteOlderThan`）；schema 设计应避免 UPDATE 操作。
