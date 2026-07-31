# `doc.go` 技术实现文档

> 源文件：`internal/manager/data/aiops/store/doc.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/aiops/store`

## 1. 概述

本文件仅为 `aiops/store` 包的包注释（package doc）。声明本包是 `internal/manager/biz/aiops` 中 `SessionRepo` 接口的 GORM/SQLite 落地实现层，承担 AIOps 会话（chat session）、消息、工具调用、变更提案（mutating proposal）、用户自定义 agent persona 的持久化职责。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/aiops`
- **依赖方向**：被 `cmd/ongrid` 装配（wire）调用；依赖 `internal/manager/biz/aiops`（接口定义）、`internal/manager/model/aiops`（GORM 模型）、`internal/pkg/errs`（哨兵错误）、`gorm.io/gorm`。同目录文件还实现了 `UserAgentRepo`、`MutatingProposalRepo` 等。

## 3. 关键类型与接口

无类型定义；仅以 Go package doc 注释形式存在。

```go
// Package sqlite provides the GORM/SQLite implementation of the aiops
// SessionRepo interface defined in internal/manager/biz/aiops.
package store
```

## 4. 关键函数与流程

无函数定义。

## 5. 依赖关系

- 仅声明性依赖（无 import）。
- **被调用方**：通过同包 `session.go`、`mutating_proposal.go`、`user_agent.go`、`migrate.go` 中的 `NewBizRepo` / `NewSessionRepo` 等构造器被外部装配调用。

## 6. 并发与资源管理

不涉及。

## 7. 设计模式与亮点

- **包隔离原则**：包注释明确表述"data 层实现 biz 层接口"，遵循架构约束 `cmd → web → controlplane → repo → model`，biz 接口在消费方定义，data 层提供具体实现。

## 8. 注意事项

- 文件中含历史性笔误："Package sqlite" 实际对应 `store` 包；源文件名 `doc.go` 仅承载包注释，不引入代码逻辑。
- 修改本包时，包注释需同步更新覆盖范围（目前仅提及 `SessionRepo`，实际包内还实现了 `MutatingProposalRepo`、`UserAgentRepo` 等）。
