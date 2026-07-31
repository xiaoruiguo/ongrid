# `migrate.go` 技术实现文档

> 源文件：`internal/iam/data/org/store/migrate.go`
> 包路径：`github.com/ongridio/ongrid/internal/iam/data/org/store`

## 1. 概述

本文件是 IAM BC 组织表的 GORM AutoMigrate 入口。仅暴露 `Migrate(db *gorm.DB) error`，由 `cmd/ongrid` 启动时调用，确保 `orgs` 表结构与 `model.Org` 结构体保持同步。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/iam/data/org/store` —— data 层组织存储
- **依赖方向**：被 `cmd/ongrid` 启动流程调用；依赖 `internal/iam/model`

## 3. 关键类型与接口

无显著类型定义。

## 4. 关键函数与流程

### `Migrate`
- **签名**：`func Migrate(db *gorm.DB) error`
- **职责**：对 `model.Org` 执行 AutoMigrate。
- **流程**：直接调用 `db.AutoMigrate(&model.Org{})`。
- **错误处理**：AutoMigrate 错误透传。

## 5. 依赖关系

- **内部包**：`internal/iam/model`
- **外部库**：`gorm.io/gorm`
- **被调用方**：`cmd/ongrid` 通过 `dbx.RunMigrations 组合调用`

## 6. 并发与资源管理

无并发控制。AutoMigrate 自身负责 DDL 锁。

## 7. 设计模式与亮点

- **极简迁移函数**：与同 BC 其他子包（membership / user）的 migrate 风格一致，单一职责，便于统一编排。

## 8. 注意事项

- AutoMigrate 仅做加法，不删列 / 不改约束；schema 演进需配合显式 SQL。
- `model.Org.ParentID` 为 `*uint64`，未做外键约束（注释提及 gorm 迁移 + sqlite 方言漂移使 FK 不可靠），完整性由 biz 层 `org.Service` 保障。
- 生产 MySQL 大表变更不应依赖 AutoMigrate，需走 migration 文件（gospec 数据存储红线）。
