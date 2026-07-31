# `doc.go` 技术实现文档

> 源文件：`internal/iam/biz/doc.go`
> 包路径：`github.com/ongridio/ongrid/internal/iam/biz`

## 1. 概述

本文件是 IAM BC `biz` 包的包级文档文件，仅包含 package 注释，无任何可执行代码。用于声明 biz 层的职责边界与子域划分，指导后续维护者理解包结构演进。

## 2. 包信息

- **包名**：`biz`
- **所属模块**：`internal/iam/biz` —— IAM BC 的 usecase + 仓储接口层
- **依赖方向**：被 `internal/iam/service` 调用；定义 `internal/iam/data/**` 需实现的仓储接口

## 3. 关键类型与接口

无显著类型定义（仅有 package 注释）。

## 4. 关键函数与流程

无函数定义。

## 5. 依赖关系

- **内部包**：无 import
- **外部库**：无
- **被调用方**：包级文档，不被调用

## 6. 并发与资源管理

无并发控制。

## 7. 设计模式与亮点

- **接口在消费方定义**：文档明确指出「Interfaces live here (consumer side); implementations live in ../data」，遵循 gospec 红线。
- **历史演进标注**：注释记录架构 pivot 后 orgs/memberships 子包曾一度被移除，保留为后续 Phase-1 重新引入的注释线索。

## 8. 注意事项

- 该文件无逻辑，但注释是当前包结构的权威说明；后续新增子包时需同步更新注释，避免与实际包结构漂移。
