# `migrate.go` 技术实现文档

> 源文件：`internal/iam/data/membership/store/migrate.go`
> 包路径：`github.com/ongridio/ongrid/internal/iam/data/membership/store`

## 1. 概述

本文件是 IAM BC 成员关系表的 GORM AutoMigrate 入口。仅暴露 `Migrate(db *gorm.DB) error`，由 `cmd/ongrid` 启动时调用，确保 `org_memberships` 表结构与 `model.OrgMembership` 结构体保持同步。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/iam/data/membership/store` —— data 层成员关系存储
- **依赖方向**：被 `cmd/ongrid` 启动流程调用；依赖 `internal/iam/model`

## 3. 关键类型与接口

无显著类型定义。

## 4. 关键函数与流程

### `Migrate`
- **签名**：`func Migrate(db *gorm.DB) error`
- **职责**：对 `model.OrgMembership` 执行 AutoMigrate。
- **流程**：直接调用 `db.AutoMigrate(&model.OrgMembership{})`。
- **错误处理**：AutoMigrate 错误透传。

## 5. 依赖关系

- **内部包**：`internal/iam/model`
- **外部库**：`gorm.io/gorm`
- **被调用方**：`cmd/ongrid` 通过 `dbx.RunMigrations 组合调用`

## 6. 并发与资源管理

无并发控制。AutoMigrate 自身负责 DDL 锁。

## 7. 设计模式与亮点

- **极简迁移函数**：单一职责，仅注册一个模型；与其他 data 子包 migrate 保持一致风格，便于 `dbx.RunMigrations` 统一编排。

## 8. 注意事项

- AutoMigrate 不会删除列或约束，仅做加法；schema 变更需配合显式 SQL（参考 `data/user/sqlite/migrate.go` 的 CHECK 约束处理模式）。
- 表结构定义在 `model.OrgMembership` 的 gorm tag 中，修改字段需同步更新模型。
- 生产 MySQL 大表变更不应依赖 AutoMigrate，需走 migration 文件（gospec 数据存储红线）。
