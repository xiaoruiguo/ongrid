# `migrate.go` 技术实现文档

> 源文件：`internal/manager/data/approval/store/migrate.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/approval/store`

## 1. 概述

本文件是 approval store 的 schema 入口，仅 AutoMigrate 单张 `approvals` 表。注释明示"additive — new table only"，即该表是新增表，不涉及历史数据迁移。红线：生产 schema 演进应迁至版本化 SQL migration 文件。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/approval`
- **依赖方向**：被 `cmd/ongrid` 启动调用；依赖 `gorm.io/gorm`、`internal/manager/model/approval`。

## 3. 关键类型与接口

无自定义类型；仅使用 model 包定义的 `model.Approval`。

## 4. 关键函数与流程

### `Migrate`
- **签名**：`func Migrate(db *gorm.DB) error`
- **职责**：`db.AutoMigrate(&model.Approval{})`。
- **流程**：单行 AutoMigrate 调用，错误直接返回。

## 5. 依赖关系

- **内部包**：`internal/manager/model/approval`
- **外部库**：`gorm.io/gorm`
- **被调用方**：`cmd/ongrid` 启动序列（通过 `dbx.RunMigrations`）

## 6. 并发与资源管理

- **无锁**：启动期串行执行。

## 7. 设计模式与亮点

- **additive-only**：仅新增表，不触碰既有 schema，迁移风险最小。
- **单表单行**：极简 migrate，便于审计。

## 8. 注意事项

- **预生产阶段**：注释明示生产应迁至版本化 SQL migration 文件。
- **AutoMigrate 不删列**：废弃列需显式 migration 文件清理。
- **Approval 模型字段扩展**：扩展 model 字段时 AutoMigrate 会自动加列，但默认值 / NOT NULL 约束需在 model tag 中正确声明，否则历史行插入失败。
