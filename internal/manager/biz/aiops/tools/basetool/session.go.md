# `session.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/basetool/session.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools/basetool`

## 1. 概述

本文件实现 chat session id 的 ctx 传播。runtime 每请求 attach 一次；为后续执行排队的工具读取，让工作能关联回原 session。`cloud_bash` 用它解析 per-session agent workspace（HLD-019）：approval payload 携带 session id，execute-on-approve hook 在 `<workspace>/sessions/<id>/` 跑命令——一次命令写的文件能被下次命令读到，而非扔进临时目录。

## 2. 包信息

- **包名**：`basetool`
- **所属模块**：叶子包，与 bound_credentials.go / llm_choice.go 同样的反循环设计
- **依赖方向**：被 chatruntime（set）、`tools/cloud_bash`（read）调用；依赖标准库 `context`

## 3. 关键类型与接口

```go
type sessionIDCtxKeyT struct{}
var sessionIDCtxKey = sessionIDCtxKeyT{}
```

## 4. 关键函数与流程

### `WithSessionID`
- **签名**：`func WithSessionID(ctx context.Context, id string) context.Context`
- **职责**：attach chat session id
- **流程**：`id == ""` → no-op；否则 `context.WithValue` attach
- **错误处理**：无

### `SessionIDFromContext`
- **签名**：`func SessionIDFromContext(ctx context.Context) string`
- **职责**：返回 session id，无则 `""`
- **流程**：类型断言取值

## 5. 依赖关系

- **内部包**：无
- **外部库**：标准库 `context`
- **被调用方**：`chatruntime`（producer）、`tools/cloud_bash_basetool.go`（consumer）

## 6. 并发与资源管理

- 不可变字符串 ctx value，并发安全
- 无锁

## 7. 设计模式与亮点

- **per-session agent workspace**：session id 决定命令执行目录，跨命令复用文件状态，避免临时目录丢文件
- **空值 no-op**：caller 可放心传空，不污染 ctx
- **叶子包反循环**：chatruntime 和 cloud_bash 都依赖 basetool

## 8. 注意事项

- **session id 字符串**：实际目录解析在 cloud_bash / edge 端完成，ctx 只携带 id
- **空 id = 无 workspace 关联**：执行时回退临时目录
- **多 worker 共享 session**：同一 session 的多个 worker 共用 workspace 目录
