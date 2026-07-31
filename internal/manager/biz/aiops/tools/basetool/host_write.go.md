# `host_write.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/basetool/host_write.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools/basetool`

## 1. 概述

本文件实现 admin "允许 Agent 写操作" 门控的 ctx 传播，专用于 `host_bash`。chat runtime 每次请求解析一次 live `AgentWriteEnabled` 设置，把结果 stamp 到 ctx；`host_bash` 读取后转发给 edge 作为 `BashExecRequest.Unrestricted`，让 edge 绕过 cmdpolicy 直接经 shell 跑原始命令。门控 OFF（默认）时 host_bash 走只读 cmdpolicy 路径。

## 2. 包信息

- **包名**：`basetool`
- **所属模块**：叶子包，与 session.go / artifact_source.go 同样的反循环设计
- **依赖方向**：被 chatruntime（producer）、`tools/bash_basetool`（consumer）调用；依赖标准库 `context`

## 3. 关键类型与接口

```go
type hostWriteAllowedCtxKeyT struct{}
var hostWriteAllowedCtxKey = hostWriteAllowedCtxKeyT{}
```

## 4. 关键函数与流程

### `WithHostWriteAllowed`
- **签名**：`func WithHostWriteAllowed(ctx context.Context, allowed bool) context.Context`
- **职责**：tag ctx 标记 host_bash 是否可跑 unrestricted（绕过 cmdpolicy）
- **流程**：仅 chat runtime 调用；用 admin write gate 的解析值 stamp
- **错误处理**：无

### `HostWriteAllowedFromContext`
- **签名**：`func HostWriteAllowedFromContext(ctx context.Context) bool`
- **职责**：报告 write gate 是否授权 unrestricted 命令
- **流程**：类型断言 `ctx.Value(key).(bool)`，失败返回 false
- **fail-safe**：缺失 key → false（锁只读），任何忘记 stamp 的路径都 fail safe

## 5. 依赖关系

- **内部包**：无
- **外部库**：标准库 `context`
- **被调用方**：`chatruntime`、`tools/bash_basetool.go`

## 6. 并发与资源管理

- bool 值不可变，并发安全
- 无锁、无 channel

## 7. 设计模式与亮点

- **fail-safe 默认**：缺失 ctx key 返回 false，遗忘 stamp 不会意外放开写权限
- **决策时机分离**：admin 设置每请求解析一次（避免热路径 DB round-trip），runtime 决策 → ctx 传递 → edge 执行
- **cmdpolicy 绕过显式化**：unrestricted 是显式标志，不能由 LLM 或用户在 run time 设置

## 8. 注意事项

- **门控默认 OFF**：未 stamp 时 host_bash 走只读 cmdpolicy
- **仅 chat runtime 可 set**：其他路径不应修改此标志
- **`Unrestricted` 转发**：edge 收到 `BashExecRequest.Unrestricted=true` 时绕过 cmdpolicy，承担安全责任
