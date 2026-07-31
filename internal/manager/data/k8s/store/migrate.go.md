# `migrate.go` 技术实现文档

> 源文件：`internal/manager/data/k8s/store/migrate.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/k8s/store`

## 1. 概述

本文件是 Kubernetes onboarding store 的 schema 入口，AutoMigrate 七张表：`k8s_clusters` / `k8s_nodes` / `k8s_workloads` / `k8s_pods` / `k8s_events` / `k8s_installations` / `k8s_telemetry_credentials`。使用 `dbx.NeedsDeleteMarkerMigration` 检测 legacy soft-delete 列，仅 `k8s_clusters` 走 delete_marker 迁移（drop `idx_k8s_clusters_uid_deleted` 索引）。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/k8s`
- **依赖方向**：被 `cmd/ongrid` 启动调用；依赖 `gorm.io/gorm`、`internal/manager/model/k8s`、`internal/pkg/dbx`。

## 3. 关键类型与接口

无自定义类型；使用 model 包定义的七张表模型。

## 4. 关键函数与流程

### `Migrate`
- **签名**：`func Migrate(db *gorm.DB) error`
- **流程**：
  1. `dbx.NeedsDeleteMarkerMigration(k8s_clusters)` → `dbx.DropIndexes(idx_k8s_clusters_uid_deleted)`
  2. `AutoMigrate(Cluster, Node, Workload, Pod, Event, Installation, TelemetryCredential)`
  3. `dbx.BackfillDeleteMarker(k8s_clusters)`
- **关键约束**：仅 `k8s_clusters` 走 delete_marker 迁移；其余表无 legacy 索引。

## 5. 依赖关系

- **内部包**：`internal/manager/model/k8s`、`internal/pkg/dbx`
- **外部库**：`gorm.io/gorm`
- **被调用方**：`cmd/ongrid` 启动序列

## 6. 并发与资源管理

- **无锁**：启动期串行执行。

## 7. 设计模式与亮点

- **dbx soft-delete 迁移模式**：仅 k8s_clusters 走迁移，drop `idx_k8s_clusters_uid_deleted`（含 deleted_at 的唯一索引），AutoMigrate 重建为 delete_marker 形态。
- **七表一并 migrate**：单 AutoMigrate 调用注册七张表，便于审计。

## 8. 注意事项

- **dbx.NeedsDeleteMarkerMigration**：检测 legacy soft-delete 列；post-launch 表已迁移则 no-op。
- **仅 k8s_clusters 迁移**：其余表无 legacy 索引，无需 drop。
- **预生产阶段**：生产应迁至版本化 SQL migration 文件。
- **七表依赖关系**：k8s_nodes / k8s_workloads / k8s_pods / k8s_events / k8s_installations / k8s_telemetry_credentials 均通过 cluster_id 关联 k8s_clusters；AutoMigrate 不建 FK，cascade 由代码层（DeleteCluster）处理。
