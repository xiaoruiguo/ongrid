# `provider.go` 技术实现文档

> 源文件：`internal/manager/data/alert/store/provider.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/alert/store`

## 1. 概述

本文件是 `biz/alert.Repo` 的 wire-ready 装配入口，仅一行实现：把 `*gorm.DB` 绑定为 `biz.Repo` 接口返回。设计目的是让 `cmd/ongrid` 装配时无需导入 `store` 包的具体类型，只依赖 biz 接口。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/alert`
- **依赖方向**：被 `cmd/ongrid` 装配调用；依赖 `internal/manager/biz/alert`（接口）、`gorm.io/gorm`。

## 3. 关键类型与接口

无类型定义。

## 4. 关键函数与流程

### `NewBizRepo`
- **签名**：`func NewBizRepo(db *gorm.DB) biz.Repo`
- **职责**：wire-ready 构造器，返回 `NewRepo(db)` 的结果，类型为 biz 接口。
- **流程**：直接委托给同包 `NewRepo`。

## 5. 依赖关系

- **内部包**：`internal/manager/biz/alert`（`Repo` 接口）
- **外部库**：`gorm.io/gorm`
- **被调用方**：`cmd/ongrid` 装配序列

## 6. 并发与资源管理

不涉及。

## 7. 设计模式与亮点

- **wire-ready 构造器模式**：返回 biz 接口而非具体类型，让 composition root 无需依赖 data 层具体类型，符合"接口在消费方定义"的架构约束。
- **薄封装**：仅一行委托，保持装配点清晰。

## 8. 注意事项

- **不可单独使用**：依赖 `repo.go` 中的 `NewRepo` 与 `Repo` 类型；修改 `Repo` 的构造签名需同步检查此入口。
- **接口断言在 repo.go**：`var _ biz.Repo = (*Repo)(nil)` 在 `repo.go` 中保证编译期接口实现；此处无需重复。
