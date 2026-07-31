# `provider.go` 技术实现文档

> 源文件：`internal/manager/data/metric/store/provider.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/metric/store`

## 1. 概述

本文件是 `biz/metric.Writer` 与 `biz/metric.Reader` 的 wire-ready 装配入口。`NewBizWriter` / `NewBizReader` 返回 biz 接口而非具体类型，让 `cmd/ongrid` 装配层与 `*Writer` / `*Reader` 具体类型解耦。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/metric`
- **依赖方向**：被 `cmd/ongrid` 装配调用；依赖 `internal/manager/biz/metric`（接口）、`gorm.io/gorm`。

## 3. 关键类型与接口

无类型定义。

## 4. 关键函数与流程

### `NewBizWriter`
- **签名**：`func NewBizWriter(db *gorm.DB) biz.Writer`
- **职责**：wire-ready 构造器，返回 `NewWriter(db)` 的结果，类型为 biz.Writer 接口。
- **设计目的**：构造 biz.Ingester 时不暴露 `*Writer` 具体类型给 wiring 层。

### `NewBizReader`
- **签名**：`func NewBizReader(db *gorm.DB) biz.Reader`
- **职责**：wire-ready 构造器，返回 `NewReader(db)` 的结果，类型为 biz.Reader 接口。
- **设计目的**：与 NewBizWriter 对称的装配便利。

## 5. 依赖关系

- **内部包**：`internal/manager/biz/metric`（`Writer` / `Reader` 接口）
- **外部库**：`gorm.io/gorm`
- **被调用方**：`cmd/ongrid` 装配序列

## 6. 并发与资源管理

不涉及。

## 7. 设计模式与亮点

- **wire-ready 构造器模式**：返回 biz 接口，隔离装配层与具体类型，符合"接口在消费方定义"约束。
- **Writer / Reader 对称装配**：两个构造器成对，便于装配层一次绑定。

## 8. 注意事项

- **依赖 writer.go / reader.go**：修改 `Writer` / `Reader` 构造签名需同步检查此入口。
- **接口断言在 writer.go / reader.go**：`var _ biz.Writer = (*Writer)(nil)` 等保证编译期实现。
