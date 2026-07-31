# `provider.go` 技术实现文档

> 源文件：`internal/manager/data/edge/store/provider.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/edge/store`

## 1. 概述

本文件是 `biz/edge.Repo` 的 wire-ready 装配入口。`NewBizRepo` 返回 biz 接口而非具体类型，让 `cmd/ongrid` 装配层与 `store` 具体类型解耦。bare `NewRepo` 返回具体类型供测试 introspection。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/edge`
- **依赖方向**：被 `cmd/ongrid` 装配调用；依赖 `internal/manager/biz/edge`（接口）、`gorm.io/gorm`。

## 3. 关键类型与接口

无类型定义。

## 4. 关键函数与流程

### `NewBizRepo`
- **签名**：`func NewBizRepo(db *gorm.DB) biz.Repo`
- **职责**：wire-ready 构造器，返回 `NewRepo(db)` 的结果，类型为 biz 接口。
- **流程**：直接委托给同包 `NewRepo`。
- **设计目的**：让 Usecase + Authenticator 消费 biz.Repo 接口；wiring 层无需依赖 `*Repo` 具体类型。bare `NewRepo` 返回具体类型供测试 introspection。

## 5. 依赖关系

- **内部包**：`internal/manager/biz/edge`（`Repo` 接口）
- **外部库**：`gorm.io/gorm`
- **被调用方**：`cmd/ongrid` 装配序列

## 6. 并发与资源管理

不涉及。

## 7. 设计模式与亮点

- **wire-ready 构造器模式**：返回 biz 接口，隔离装配层与具体类型，符合"接口在消费方定义"约束。
- **测试 introspection 入口保留**：bare `NewRepo` 返回具体类型，测试可访问 `*Repo` 字段。

## 8. 注意事项

- **依赖 edge.go 的 NewRepo**：修改 `Repo` 构造签名需同步检查此入口。
- **接口断言在 edge.go**：`var _ biz.Repo = (*Repo)(nil)` 在 edge.go 中保证编译期实现。
