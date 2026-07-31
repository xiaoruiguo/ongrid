# `hash.go` 技术实现文档

> 源文件：`internal/iam/biz/user/hash.go`
> 包路径：`github.com/ongridio/ongrid/internal/iam/biz/user`

## 1. 概述

本文件是 IAM BC 用户子包内的密码哈希薄包装层。将 argon2id 哈希能力从 `internal/pkg/passwd` 引入 biz 层，使得 user usecase 可复用同一哈希方案，同时避免 manager BC 反向 import iam BC（arch-lint 红线）。

## 2. 包信息

- **包名**：`user`（与同目录其他文件同包）
- **所属模块**：`internal/iam/biz/user` —— biz 层密码哈希适配
- **依赖方向**：被同包 `usecase.go` 调用；依赖 `internal/pkg/passwd`

## 3. 关键类型与接口

无显著类型定义。

## 4. 关键函数与流程

### `hashPassword`
- **签名**：`func hashPassword(password string) (string, error)`
- **职责**：调用 `passwd.Hash` 生成 argon2id 编码字符串。
- **流程**：直接透传 `passwd.Hash(password)`。
- **错误处理**：底层错误透传。

### `verifyPassword`
- **签名**：`func verifyPassword(password, encoded string) bool`
- **职责**：调用 `passwd.Verify` 校验明文与编码哈希是否匹配。
- **流程**：直接透传，返回 bool。

## 5. 依赖关系

- **内部包**：`github.com/ongridio/ongrid/internal/pkg/passwd`
- **外部库**：无
- **被调用方**：`internal/iam/biz/user/usecase.go`（Register / Login / BootstrapAdmin / Create / ResetPassword / EnsureSuperuser）

## 6. 并发与资源管理

无并发控制。argon2id 计算本身为 CPU 密集型同步操作，调用方需自行控制并发度（当前由 HTTP handler 串行触发）。

## 7. 设计模式与亮点

- **薄包装适配**：注释明确指出 argon2id 助手已被「提升」到 `internal/pkg/passwd`，使 manager BC 的 `SecretKeyHash` 能复用同一方案而不跨越 BC 边界。本文件仅保留包内别名，避免业务代码直接 import pkg/passwd 散落各处。
- **未导出**：两个函数均小写开头，仅限包内使用，防止外部直接绕过 usecase 调用哈希原语。

## 8. 注意事项

- argon2id 参数（时间/内存/并行度）由 `internal/pkg/passwd` 统一配置，本包不可单独调参。
- `verifyPassword` 返回 bool 而非 error，错误细节被吞掉；如需审计校验失败原因需在 `passwd.Verify` 内部记录。
- 哈希计算耗时较高，登录路径上的同步调用会阻塞 goroutine；高并发登录场景建议引入限流（当前 `server/http.go` 已有 `loginThrottle`）。
