# `audit.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/decorators/audit.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools/decorators`

## 1. 概述

本文件实现 audit 装饰器：在 `InvokableRun` 前后通过 `AuditSink` 接口发出 `OnToolStart`/`OnToolEnd` 事件，对应 `chat_tool_calls` 表的 pending 行与终态行。关键红线：`OnToolEnd` 错误必须吞掉——审计失败不能让工具失败；`OnToolStart` 错误会 abort 调用（如配额超限）。

## 2. 包信息

- **包名**：`decorators`
- **所属模块**：`internal/manager/biz/aiops/tools/decorators`
- **依赖方向**：被 `chain.go` 的 `Wrap` 调用；依赖 `basetool`、标准库

## 3. 关键类型与接口

```go
type AuditSink interface {
    OnToolStart(ctx context.Context, ev ToolStartEvent) (id string, err error)
    OnToolEnd(ctx context.Context, id string, ev ToolEndEvent) error
}

type ToolStartEvent struct {
    ToolName  string
    ArgsJSON  string
    Tenant    string
    UserID    uint64
    DeviceID  *uint64
    StartedAt time.Time
}

type ToolEndEvent struct {
    ResultJSON string
    Err        error
    EndedAt    time.Time
    Duration   time.Duration
}

type AuditTool struct {
    inner basetool.BaseTool
    sink  AuditSink
}
```

## 4. 关键函数与流程

### `WithAudit`
- **签名**：`func WithAudit(inner basetool.BaseTool, sink AuditSink) basetool.BaseTool`
- **职责**：包装 inner，emit start/end 事件
- **流程**：`sink == nil` → 直接返回 inner（no-op pass-through，便于测试/开发关闭 audit）；否则返回 `&AuditTool{inner, sink}`

### `AuditTool.Info`
- **签名**：`func (a *AuditTool) Info(ctx) (*basetool.ToolInfo, error)`
- **职责**：透传 inner.Info（audit 仅作用于 invocation）

### `AuditTool.InvokableRun`
- **签名**：`func (a *AuditTool) InvokableRun(ctx, argsJSON string, opts ...basetool.InvokeOption) (string, error)`
- **职责**：emit OnToolStart → 跑 inner → emit OnToolEnd
- **流程**：
  1. `ResolveOptions(opts)` 拿 tenant/user/device
  2. `inner.Info(ctx)` 取 tool name（Info 应廉价，闭包式 tool 返回常量）
  3. `sink.OnToolStart(ToolStartEvent{...})` → err 非 nil 直接 return（配额/preflight 失败透传）
  4. `inner.InvokableRun(ctx, argsJSON, opts...)`
  5. `sink.OnToolEnd(ToolEndEvent{ResultJSON, Err, EndedAt, Duration})` 错误用 `_ =` 吞掉（注释明示"audit must not fail the tool"）
  6. 返回 inner 的 out + runErr
- **错误处理**：OnToolStart 错误原样返回；OnToolEnd 错误吞掉（sink 内部应自己 log）

## 5. 依赖关系

- **内部包**：`basetool`
- **外部库**：标准库 `context`、`time`
- **被调用方**：`chain.go` 的 `Wrap`；生产绑定（后续 PR）由 chat_tool_calls repo 实现 `AuditSink`

## 6. 并发与资源管理

- `AuditTool` 本身无状态（除 inner 和 sink 引用），多 goroutine 共享安全
- inner 的并发契约由 inner 自己负责

## 7. 设计模式与亮点

- **接口隔离**：`AuditSink` 在本文件定义而非 import biz/aiops.SessionRepo，保持装饰器包不耦合 biz repo（模块边界不变量）
- **OnToolStart err 短路**：sink 可用 OnToolStart 返回 err 实现"配额超限 → 跳过 InvokableRun"，error 原样透传不被吞
- **OnToolEnd err 吞掉**：审计失败不让工具失败——可观测性 best-effort 原则
- **Info 透传**：audit 仅作用于 invocation，schema 不变

## 8. 注意事项

- **Info 必须廉价**：每次 InvokableRun 都会调一次 inner.Info 取 name；闭包式 registry.go 的 tool 返回常量无 I/O
- **未来 eino 切换**：agent loop 迁移到 eino + ToolsNode 时实现可切到 eino callbacks，AuditSink 契约不变
- **nil sink pass-through**：测试/开发可不传 sink，audit 自动关闭
- **id 透传**：OnToolStart 返回的 id 必须原样传给 OnToolEnd，sink 实现内部映射到 chat_tool_calls.id (UUID)
