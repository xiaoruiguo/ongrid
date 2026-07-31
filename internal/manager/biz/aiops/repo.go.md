# `repo.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/repo.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops`

## 1. 概述

本文件定义 AIOps 会话领域的核心仓储接口与值对象：`SessionRepo`（chat session/messages/tool_calls 读写）、`MutatingProposalRepo`（配置变更草案持久化）、`TokenSums`（per-user/per-day token 累计聚合）。它是 biz 层与 repo 层之间的契约边界，由 `internal/manager/repo/aiops` 实现，biz 层消费方注入使用。

## 2. 包信息

- **包名**：`aiops`
- **所属模块**：`internal/manager/biz/aiops`
- **依赖方向**：被 agent loop、chatruntime、graph/callbacks/persistence、investigator 调用；仅依赖 `context`、`time`、`model/aiops` 类型

## 3. 关键类型与接口

```go
type TokenSums struct {
    PromptTokens, CompletionTokens, TotalTokens int64
}

type SessionRepo interface {
    AppendMessage(ctx, sessionID string, msg *aiopsmodel.Message) (string, error)
    AppendToolCall(ctx, sessionID string, tc *aiopsmodel.ToolCall) (int64, error)
    UpdateToolCall(ctx, id int64, status, result, errStr string, endedAt time.Time) error
    ListMessages(ctx, sessionID string) ([]*aiopsmodel.Message, error)
    ListToolCalls(ctx, sessionID string) ([]*aiopsmodel.ToolCall, error)
    FinalizePendingToolCalls(ctx, sessionID, resultJSON, errStr string, endedAt time.Time) (int64, error)
    GetSession(ctx, sessionID string) (*aiopsmodel.Session, error)
    // ... 等若干签名
}

type MutatingProposalRepo interface {
    SaveProposal(ctx, p *aiopsmodel.MutatingProposal) (string, error)
    GetProposal(ctx, id string) (*aiopsmodel.MutatingProposal, error)
    ListProposalsBySession(ctx, sessionID string) ([]*aiopsmodel.MutatingProposal, error)
}
```

`SessionRepo` 的所有方法第一个参数为 `context.Context`（遵循 gospec IO 规范）；`AppendMessage` 返回新写入行的 id，用于回填到 SSE 帧 / 后续 tool_call 关联。

## 4. 关键函数与流程

本文件仅声明接口，无函数实现。流程性内容分布在实现侧 `repo/aiops/`：
- 写消息：`AppendMessage`（role=assistant 或 role=tool）→ 回填 id
- 写工具调用：`AppendToolCall`（status=pending）→ 返回 row id
- 更新工具调用：`UpdateToolCall`（status=success/error/timeout + result/error）
- 自动愈伤：`FinalizePendingToolCalls` 批量关闭未结束的 tool_call（用于会话结束 / graph 中断时）

## 5. 依赖关系

- **内部包**：`internal/manager/model/aiops`（DTO 定义）
- **实现方**：`internal/manager/repo/aiops`（MySQL 实现）
- **消费方**：agent、chatruntime、graph/callbacks/persistence、graph/callbacks/chain

## 6. 并发与资源管理

- 接口契约未规定实现并发安全级别；调用方（`PersistenceHandler`）在 `chat_messages.id` 写入后通过 ctx / handler-internal FIFO 跨回调传递 id，单 graph run 内串行
- 所有 IO 操作带 `context.Context`，调用方负责 deadline / cancel
- `FinalizePendingToolCalls` 是幂等的批关闭操作，可重复调用

## 7. 设计模式与亮点

- **接口在消费方定义**：符合 gospec 红线"接口在消费方定义，禁止循环依赖"；本文件位于 biz 而非 repo
- **窄接口分离**：`SessionRepo` 与 `MutatingProposalRepo` 拆开，避免单接口膨胀；后者专门服务配置变更流程
- **ID 回填契约**：`AppendMessage` 返回 string id 而非仅 error，便于 SSE `assistant_end` 帧附带 DB id（参考 `persistence.go` 的 `assistantIDRelay`）
- **`FinalizePendingToolCalls` 自动愈伤接口**：作为 eino ToolsNode 偶发丢 OnEnd 回调的兜底契约

## 8. 注意事项

- 接口变更需同步更新实现 `repo/aiops/`；破坏性变更应走新版本方法（gospec API 红线）
- `Message` / `ToolCall` 字段定义在 `model/aiops`，调整字段需考虑数据库 migration
- `TokenSums` 是值对象（value object），不可变；用作 `BudgetChecker` 的输入
- 多租户过滤（tenant_id）目前未在接口暴露，由实现层注入；tenancy 落地时需扩展接口签名
