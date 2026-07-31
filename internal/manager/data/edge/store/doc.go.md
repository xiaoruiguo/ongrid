# `doc.go` 技术实现文档

> 源文件：`internal/manager/data/edge/store/doc.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/edge/store`

## 1. 概述

本文件是 `edge/store` 包的 package doc，声明本包是 `internal/manager/biz/edge` 中 `Repo` 接口的 GORM/SQLite 落地实现层。注释提到"Despite the package name it works against MySQL too"——GORM 在此层隐藏方言差异。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/edge`
- **依赖方向**：被 `cmd/ongrid` 装配；依赖 `internal/manager/biz/edge`（接口）、`internal/manager/model/edge`、`internal/pkg/errs`、`gorm.io/gorm`。

## 3. 关键类型与接口

无类型定义；仅 package doc 注释。

```go
// Package sqlite provides the GORM/SQLite implementation of the manager/edge
// Repo interface defined in internal/manager/biz/edge.
package store
```

## 4. 关键函数与流程

无函数定义。

## 5. 依赖关系

- 仅声明性依赖。
- **被调用方**：通过同包 `edge.go` 的 `NewRepo` / `NewBizRepo`、`plugin_config.go` 的 `NewPluginConfigRepo`、`migrate.go` 的 `Migrate` 被外部装配调用。

## 6. 并发与资源管理

不涉及。

## 7. 设计模式与亮点

- **方言无关**：注释明示 GORM 隐藏方言，同代码跑 MySQL + SQLite。
- **包名笔误容忍**：注释写 "Package sqlite" 但实际包名为 `store`，是历史遗留笔误。

## 8. 注意事项

- 同其他 doc.go 一样存在 "Package sqlite" 笔误。
- 修改本包时需同步更新注释覆盖范围（当前覆盖 Edge + PluginConfig 两张表）。
