# `migrate.go` 技术实现文档

> 源文件：`internal/manager/data/mcp/store/migrate.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/mcp/store`

## 1. 概述

本文件是 MCP（Model Context Protocol）server 注册表的 schema 入口，仅 AutoMigrate 单张 `mcp_servers` 表。注释明示在 `cmd/ongrid/main.go` 中与其他 data-package migration 一同注册。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/mcp`
- **依赖方向**：被 `cmd/ongrid` 启动调用；依赖 `gorm.io/gorm`、`internal/manager/model/mcp`。

## 3. 关键类型与接口

无自定义类型；使用 `model.Server`。

## 4. 关键函数与流程

### `Migrate`
- **签名**：`func Migrate(db *gorm.DB) error`
- **职责**：`db.AutoMigrate(&model.Server{})`。

## 5. 依赖关系

- **内部包**：`internal/manager/model/mcp`
- **外部库**：`gorm.io/gorm`
- **被调用方**：`cmd/ongrid/main.go` 启动序列

## 6. 并发与资源管理

- **无锁**：启动期串行执行。

## 7. 设计模式与亮点

- **单表单行 migrate**：极简，便于审计。

## 8. 注意事项

- **预生产阶段**：生产应迁至版本化 SQL migration 文件。
- **AutoMigrate 不删列**：废弃列需显式 migration 清理。
- **Server 模型字段扩展**：扩展 model 字段时 AutoMigrate 会自动加列，但默认值 / NOT NULL 约束需在 model tag 正确声明。
