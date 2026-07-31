# `migrate.go` 技术实现文档

> 源文件：`internal/manager/data/flow/store/migrate.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/flow/store`

## 1. 概述

本文件是 flow store 的 schema 入口，AutoMigrate 三张表：`flows`（flow 定义）、`flow_runs`（执行实例）、`flow_run_nodes`（节点执行记录）。注释明示"additive-only (columns/indexes)"，与同级 domain 一致。红线：生产 schema 演进应迁至版本化 SQL migration 文件。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/flow`
- **依赖方向**：被 `cmd/ongrid` 启动调用；依赖 `gorm.io/gorm`、`internal/manager/model/flow`。

## 3. 关键类型与接口

无自定义类型；使用 `model.Flow` / `model.FlowRun` / `model.FlowRunNode`。

## 4. 关键函数与流程

### `Migrate`
- **签名**：`func Migrate(db *gorm.DB) error`
- **职责**：`db.AutoMigrate(&model.Flow{}, &model.FlowRun{}, &model.FlowRunNode{})`。

## 5. 依赖关系

- **内部包**：`internal/manager/model/flow`
- **外部库**：`gorm.io/gorm`
- **被调用方**：`cmd/ongrid` 启动序列

## 6. 并发与资源管理

- **无锁**：启动期串行执行。

## 7. 设计模式与亮点

- **additive-only**：仅加列/索引，不删改，迁移风险最小。
- **三表一并 migrate**：单 AutoMigrate 调用注册三张表，便于审计。

## 8. 注意事项

- **预生产阶段**：生产应迁至版本化 SQL migration 文件。
- **AutoMigrate 不删列**：废弃列需显式 migration 清理。
- **三表依赖关系**：`flow_runs.flow_id → flows.id`，`flow_run_nodes.run_id → flow_runs.id`；AutoMigrate 不建 FK，cascade 由代码层（`PruneRuns`）处理。
