# `doc.go` 技术实现文档

> 源文件：`internal/manager/data/alert/store/doc.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/alert/store`

## 1. 概述

本文件是 `alert/store` 包的 package doc，仅声明包的职责边界：以 GORM/SQLite 实现 `manager/alert` 的持久化层，并强调包是"自包含"的——alert 数据层可以独立落地，不依赖 biz/service 装配的同步推进。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/alert`
- **依赖方向**：被 `cmd/ongrid` 装配；依赖 `internal/manager/biz/alert`（接口）、`internal/manager/model/alert`（GORM 模型）、`internal/pkg/config`、`internal/pkg/notify`、`internal/pkg/errs`、`gorm.io/gorm`。

## 3. 关键类型与接口

无类型定义；仅 package doc 注释。

```go
// Package sqlite provides the GORM/SQLite implementation of the manager/alert
// persistence layer. This package is intentionally self-contained so alert
// data work can land before biz/service wiring is introduced.
package store
```

## 4. 关键函数与流程

无函数定义。

## 5. 依赖关系

- 仅声明性依赖。
- **被调用方**：通过同包 `repo.go` 的 `NewBizRepo`、`provider.go` 的入口、`seed.go` / `seed_rules.go` 的 seeder 被外部装配调用。

## 6. 并发与资源管理

不涉及。

## 7. 设计模式与亮点

- **包自包含原则**：注释明示"intentionally self-contained"，允许 data 层先行落地而不阻塞于 biz/service 装配进度，匹配 monorepo 中按域独立演进的约束。

## 8. 注意事项

- 同 aiops/store 的 doc.go 一样存在 "Package sqlite" 笔误，实际包名为 `store`。
- 修改本包时需同步更新注释覆盖范围（当前覆盖 Incident / Event / Silence / Rule / Channel / Delivery / InvestigationReport 等多张表）。
