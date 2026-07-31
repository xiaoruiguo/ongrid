# `doc.go` 技术实现文档

> 源文件：`internal/iam/biz/user/doc.go`
> 包路径：`github.com/ongridio/ongrid/internal/iam/biz/user`

## 1. 概述

本文件是 IAM BC `user` 子包的包级文档文件，仅含 package 注释。声明该子包是 IAM BC 唯一保留下来的子域（post-pivot），负责用户账号与认证流程；并明确列出 Register / Login / Refresh / BootstrapAdmin / 管理端列表等职责，以及「永不在公开 API 返回 PassHash」的红线。

## 2. 包信息

- **包名**：`user`
- **所属模块**：`internal/iam/biz/user` —— IAM BC biz 层用户子域
- **依赖方向**：被 `internal/iam/service` 调用；实现位于 `internal/iam/data/user/sqlite`

## 3. 关键类型与接口

无显著类型定义（仅 package 注释）。

## 4. 关键函数与流程

无函数定义。

## 5. 依赖关系

- **内部包**：无 import
- **外部库**：无
- **被调用方**：包级文档，不被调用

## 6. 并发与资源管理

无并发控制。

## 7. 设计模式与亮点

- **职责清单式文档**：注释以列表形式枚举子包职责，便于后续维护者快速定位。
- **政策中立声明**：明确「Role enforcement (admin-only registration) is the caller's job — biz is policy-neutral」，划清 biz 与 HTTP 层的权限边界。
- **安全红线**：注释中强调 PassHash 永不出现在公开 API，是本子包的核心安全约束。

## 8. 注意事项

- 该文件无逻辑，但注释是子包职责的权威说明；新增职责时需同步更新，避免与实际实现漂移。
- 若未来 pivot 再次引入 orgs/memberships 子包，需调整此处「only sub-domain」表述。
