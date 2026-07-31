# `doc.go` 技术实现文档

> 源文件：`internal/manager/service/frontierbound/doc.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/service/frontierbound`

## 1. 概述

本文件是 `frontierbound` 包的包级文档注释载体，无任何可执行代码。声明本包职责：作为 manager 侧对上游 `github.com/singchia/frontier` service-end SDK 的封装，维持长连接 geminio 服务连接，注册 frontier 所需的生命周期回调（GetEdgeID / EdgeOnline / EdgeOffline），并暴露窄 Caller 表面（Call / Register）让 manager biz 代码无需学习 geminio 类型。强调 wire-level message names + JSON shapes 与 edge agent 共享于 `internal/pkg/tunnel/messages.go`，本包有意不重新声明。

## 2. 包信息

- **包名**：`frontierbound`
- **所属模块**：`internal/manager/service/frontierbound`
- **依赖方向**：本文件无 import；包整体依赖 `internal/pkg/tunnel`、`github.com/singchia/frontier/api/dataplane/v1/service`、`github.com/singchia/geminio`

## 3. 关键类型与接口

本文件无类型、接口、常量、变量定义。仅 package 注释。

## 4. 关键函数与流程

无函数。

## 5. 依赖关系

- **内部包**：无（本文件无 import）
- **外部库**：无
- **被调用方**：无（文档文件）

## 6. 并发与资源管理

不适用 —— 纯文档文件。

## 7. 设计模式与亮点

- **wire 协议单一来源**：注释明示 message names + JSON shapes 与 edge agent 共享于 `internal/pkg/tunnel/messages.go`，本包不重新声明 —— 避免两端 drift。
- **Lifecycle Meta 复用**：edge 已发送的 `{access_key, secret_key}` JSON Meta 被 GetEdgeID 复用认证，无需新增 wire format。
- **包文档作为架构边界声明**：通过 package doc 显式标注本包是 SDK 封装层，biz 代码只应通过 Caller 表面访问。

## 8. 注意事项

- **本文件仅文档**：实际类型与逻辑在 `client.go`、`handlers.go`。
- **wire format 不在本包**：新增 RPC method 需在 `internal/pkg/tunnel/messages.go` 加常量 + 请求/响应 struct。
- **Lifecycle Meta 形态**：`{access_key, secret_key}` JSON；edge 端发送，manager 端 GetEdgeID 解析认证。
- **保持简短**：doc.go 仅声明包职责与边界，不重复 client.go 的实现细节。
