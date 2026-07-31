# `doc.go` 技术实现文档

> 源文件：`internal/pkg/tunnel/doc.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/tunnel`

## 1. 概述

本文件仅为 `tunnel` 包的包级文档注释，不含可执行代码。说明该包是 edge 侧 cloud channel 抽象：建立并维持 edge 到 cloud-side broker（frontier）的多路复用双向 RPC 通道，分发入站反向 RPC 到注册 handler，并提供与 manager 侧 frontierbound handler 共享的 JSON wire 消息形状（`messages.go`）。

## 2. 包信息

- **包名**：`tunnel`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被 edge agent 调用；cloud 侧 listening end 已不在本 repo，由上游 `github.com/singchia/frontier` broker 终结 geminio，manager 通过 `internal/manager/service/frontierbound` 拨号

## 3. 关键类型与接口

无显著类型定义（仅包注释）。

## 4. 关键函数与流程

无（纯文档文件）。

文档要点：
- **职责**：建立并维持 edge → cloud 的多路复用双向 RPC 通道
- **三个子职责**：
  1. 拨号与维护连接（含重连）
  2. 分发入站反向 RPC（cloud→edge）到注册 handler
  3. 提供 JSON wire 消息形状（`messages.go`）与 manager 侧共享
- **架构定位**：cloud 侧 listening end 不在本 repo；frontier broker 终结 geminio；manager 经 `internal/manager/service/frontierbound` 拨号
- **本包仅保留 edge 的 `NewClient`**：manager 侧不通过本包拨号

## 5. 依赖关系

- **内部包**：无
- **外部库**：无（文档文件）
- **被调用方**：N/A（仅文档）

## 6. 并发与资源管理

无并发控制（纯文档文件）。

## 7. 设计模式与亮点

- **架构边界清晰**：注释明示 cloud 侧 listening end 不在本 repo，本包仅保留 edge 侧 `NewClient`，避免职责越界
- **wire 形状共享**：`messages.go` 是 edge 与 manager 共享的 wire 定义，单点维护
- **frontier 解耦**：使用上游 `github.com/singchia/frontier` 作为 geminio 终结 broker，本包不实现 server 端

## 8. 注意事项

- **manager 侧不通过本包拨号**：manager 经 `internal/manager/service/frontierbound` 独立拨号；本包仅服务 edge
- **wire 协议是 JSON**：当前 MVP 用 JSON；未来若换 protobuf 需评估 `messages.go` 的替换
- **frontier broker 是外部依赖**：版本升级可能影响 geminio 协议兼容性
