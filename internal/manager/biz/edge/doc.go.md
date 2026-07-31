# `doc.go` 技术实现文档

> 源文件：`internal/manager/biz/edge/doc.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/edge`

## 1. 概述

本文件是 `edge` 包的包级文档注释。声明 manager/edge 子域 biz 层的职责边界：注册新 edge（签发 AccessKey/SecretKey）、list、disable、跟踪 last_seen_at、暴露 `AccessKeyAuthenticator` 作为 tunnel server 的 `AuthFunc`。核心架构决策：**Edge 凭据是 device 级关注点，不属于 iam**。

## 2. 包信息

- **包名**：`edge`
- **所属模块**：`internal/manager/biz/`
- **职责**：
  - 注册新 edge（生成 AccessKey + argon2id hash SecretKey）
  - list / disable edges
  - 跟踪 last_seen_at
  - 暴露 `AccessKeyAuthenticator` 作为 tunnel `AuthFunc`
- **架构定位**：Edge 凭据是 device 级关注点（**不在 iam**）

## 3. 关键类型与接口

本文件无类型/接口定义，仅包注释。

## 4. 关键函数与流程

本文件无函数实现，仅包注释。

## 5. 依赖关系

- 包级文档；不引入新依赖
- 包内其他文件依赖：`model/edge`、`pkg/errs`、`pkg/passwd`、`pkg/tunnel`、`biz/device`

## 6. 并发与资源管理

不适用（仅文档）。

## 7. 设计模式与亮点

- **架构边界明示**：注释显式声明"Edge credentials are a device-level concern — they belong here, not in iam"，防止维护者把凭据逻辑挪到 iam 包
- **职责清单**：注释列出 5 项核心职责，便于新维护者快速理解包边界

## 8. 注意事项

- **包定位**：edge 包仅管 edge 身份和凭据；device 主表在 `biz/device`；junction 在 `EdgeDeviceRepo`
- **修改包注释**：职责扩展时同步更新此 doc.go，避免注释与实现脱节
- **doc.go 仅文档**：不引入代码，不污染包 API
