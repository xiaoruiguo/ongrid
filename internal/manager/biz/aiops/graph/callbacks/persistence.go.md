# graph/callbacks/persistence.go

## 1. 概述

本文件实现 `PersistenceHandler`——eino callback handler 链中最复杂的一个，负责把 graph 执行过程中的 assistant 消息和 tool 调用持久化到 DB（`chat_messages` + `chat_tool_calls` 表）。

核心职责：
1. **ChatModel.OnStart**：autoheal 上一轮 assistant 批次中 OnEnd 未触发的 tool 调用（写 stub 行）
2. **ChatModel.OnEnd**：INSERT `chat_messages` (role=assistant) + token usage + model id
3. **Tool.OnStart**：INSERT `chat_tool_calls` (status=pending) + stash row id
4. **Tool.OnEnd/OnError**：UPDATE `chat_tool_calls` (status=success/error/timeout) + INSERT `chat_messages` (role=tool)

附带职责：
- **autoheal 机制**：跟踪每轮 assistant 的 `tool_call_id` 集合，OnEnd 标记 seen，下一轮 ChatModel.OnStart 时为 missing 的 call 写 stub role=tool 行——防 eino ToolsNode 偶发丢失 OnEnd 导致 replay envelope 不完整
- **`assistantIDRelay`**：把刚写入的 `chat_messages.id` 通过原子指针传给 SSEHandler
- **`FinalizeBatch`**：请求结束后 flush 残留 batch + finalize pending tool calls

错误处理红线：**persist 失败绝不阻断 graph**——handler log+count 后返回 ctx unchanged。

## 2. 包信息

- **包名**：`callbacks`（包注释在本文件顶部）
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/biz/aiops/graph/callbacks`
- **角色**：DB 持久化 callback handler
- **依赖**：
  - 标准库 `context`、`errors`、`log/slog`、`sync`、`time`
  - `github.com/cloudwego/eino/callbacks`、`components`、`compose`、`schema`
  - `github.com/cloudwego/eino/components/model`、`tool`
  - `github.com/prometheus/client_golang/prometheus`
  - `github.com/ongridio/ongrid/internal/manager/biz/aiops`（`biz.SessionRepo`）
  - `github.com/ongridio/ongrid/internal/manager/model/aiops`（`model.Message`/`ToolCall`）

## 3. 关键类型与接口

### `PersistenceDeps`

```go
type PersistenceDeps struct {
    SessionID   string
    Repo        biz.SessionRepo
    Logger      *slog.Logger       // 可选，记录 best-effort 失败
    Registerer  prometheus.Registerer  // 可选，错误计数
    Model       string             // LLM model id，写入每行 assistant row
}
```

### `PersistenceHandler`

```go
type PersistenceHandler struct {
    deps             PersistenceDeps
    errCounter       *prometheus.CounterVec   // kind label
    lossCounter      *prometheus.CounterVec   // outcome, tool_name labels
    toolCalls        map[string]toolCallEntry // eino tool_call_id → entry
    toolCallsMu      sync.Mutex
    asstMu           sync.Mutex
    asstWrites       int
    assistantIDRelay *assistantIDRelay        // 跨 handler 共享
    lastIDsMu        sync.Mutex
    lastAssistantID  string
    pendingLLMCalls  []pendingLLMCall          // LLM-assigned call ids FIFO
    batchMu          sync.Mutex
    currentBatch     *assistantBatch           // autoheal 跟踪
}
```

### `assistantBatch`（未导出）

```go
type assistantBatch struct {
    assistantID string
    expected    map[string]string  // llmCallID → toolName
    seen        map[string]struct{}
    openedAt    time.Time
}
```

跟踪一轮 assistant turn 的 tool_call → tool_response 配对，用于 flushIncompleteBatch autoheal。

### `pendingLLMCall` / `toolCallEntry`（未导出）

`pendingLLMCall` 携带 LLM-assigned call id + tool name，从 ChatModel.OnEnd 流向 Tool.OnStart。
`toolCallEntry` 是 OnStart stash 的 per-call 状态，OnEnd 取出 update。

### `pendingToolCallFinalizer` 接口（未导出）

```go
type pendingToolCallFinalizer interface {
    FinalizePendingToolCalls(ctx, sessionID, resultJSON, errStr string, endedAt time.Time) (int64, error)
}
```

`biz.SessionRepo` 实现此接口时，`FinalizeBatch` 会调用它清理 graph shutdown 时残留的 pending tool calls。

## 4. 关键函数与流程

### `NewPersistenceHandler(deps)`

`SessionID == "" || Repo == nil` → 返回 nil（cutover 层视作 persistence disabled）。Registerer 非空时构造两个 CounterVec：
- `ongrid_persist_errors_total{kind}`：persist 失败计数
- `ongrid_chat_tool_response_loss_total{outcome, tool_name}`：autoheal 计数

### `Needed(ctx, info, timing) bool`

| Component | Timing |
|---|---|
| ChatModel | OnStart, OnEnd |
| Tool | OnStart, OnEnd, OnError |

注释解释 ChatModel.OnStart 的用途："flushIncompleteBatch can autoheal the previous turn before the next LLM call goes out — its position in the callback stream is the cleanest 'previous batch is terminally done' signal we have."

### `OnStart(ctx, info, input)`

**ChatModel**：
```go
h.trace(ctx, "chat_model_start", info, "")
h.flushIncompleteBatch(ctx)  // autoheal 上一轮
```

**Tool**：
1. 解析 `tin := einotool.ConvCallbackInput(input)`
2. 解析 MessageID（ctx seam 优先，否则 `h.currentAssistantID()`）
3. 解析 LLM call id（**ROOT FIX**：`compose.GetToolCallID(ctx)` 优先；fallback 到 ctx seam；再 fallback 到 `popPendingLLMCall`）
4. INSERT `chat_tool_calls` (status=pending)
5. stash entry by eino tool_call_id

### `OnEnd(ctx, info, output)`

**ChatModel**：
```go
mo := einomodel.ConvCallbackOutput(output)
h.persistAssistant(ctx, mo)
```

**Tool**：
```go
tout := einotool.ConvCallbackOutput(output)
h.persistToolEnd(ctx, info, tout, nil)
```

### `OnError(ctx, info, err)`

仅 Tool component：
```go
h.persistToolEnd(ctx, info, nil, err)
```

ChatModel error 不产生 chat row（无 message 可持久化，由 audit handler 记录）。

### `persistAssistant(ctx, mo)`

1. 构造 `model.Message{SessionID, Role, Content, PromptTokens, CompletionTokens, Model}`
2. `Repo.AppendMessage` 写入
3. `h.assistantIDRelay.store(row.ID)` 传给 SSE
4. `asstWrites++`（测试用计数）
5. 从 `msg.ToolCalls` 提取 `pendingLLMCalls` FIFO
6. `h.lastAssistantID = row.ID`（供下一 Tool.OnStart 作 MessageID）
7. `h.openBatch(row.ID, pending)` 启动 autoheal 跟踪

### `persistToolEnd(ctx, info, tout, execErr)`

1. `toolCalls[tcID]` 取出 entry（找不到 → `recordErr("tool_call_lookup_miss")`）
2. 确定 status：success / error / timeout（`errors.Is(execErr, context.DeadlineExceeded)`）
3. `Repo.UpdateToolCallResult` 更新 `chat_tool_calls` 行
4. INSERT `chat_messages` (role=tool) with `ToolCallID` + `ToolName`
5. `markSeen(entry.llmCallID)` 或 `markSeen(entry.toolCallID)`

**关键**：role=tool 行的 `ToolCallID` 用 `entry.llmCallID`（LLM-assigned "call_XX"）优先，fallback 到 eino tool_call_id。strict providers (DeepSeek v4+) 通过此 id 匹配 assistant turn 的 tool_calls slot，否则 HTTP 400。

### `flushIncompleteBatch(ctx)`（autoheal 核心）

```go
batch := h.currentBatch
h.currentBatch = nil
if batch == nil || len(batch.expected) == 0 {
    return
}
missing := {}  // llmCallID → toolName
for id, tname := range batch.expected {
    if _, ok := batch.seen[id]; !ok {
        missing[id] = tname
    }
}
if len(missing) == 0 {
    return
}
// Logger.Warn 记录损失
for callID, tname := range missing {
    // 1. 更新 chat_tool_calls 行为 status=error
    // 2. INSERT chat_messages role=tool with stub body
    //    {"error":"tool response was not persisted...","autoheal":true}
    // 3. lossCounter.WithLabelValues("autoheal_stub", tname).Inc()
}
```

**Idempotent**：清空 currentBatch 后再处理，重复调用是 no-op。

### `FinalizeBatch(ctx)`

```go
h.flushIncompleteBatch(ctx)
h.finalizePendingToolCalls(ctx)
```

由 `chain.go::FinalizeBatches` 在 graph invoke 返回后调用。`finalizePendingToolCalls` 调用 `Repo.FinalizePendingToolCalls`（若 Repo 实现了 `pendingToolCallFinalizer` 接口）清理 graph shutdown 时残留的 pending 行。

### `popPendingLLMCall(toolName) string`

注释明确："Match by tool name ANYWHERE in the queue, not just the head. Parallel tool calls complete out of issue-order (e.g. query_promql finishes before get_host_processes even though it was listed second), so a strict head match mis-pairs..."

```go
for i, c := range h.pendingLLMCalls {
    if c.toolName == toolName {
        id := c.llmCallID
        h.pendingLLMCalls = append(h.pendingLLMCalls[:i:i], h.pendingLLMCalls[i+1:]...)
        return id
    }
}
h.recordErr("tool_call_fifo_mismatch", ...)
return ""
```

### Context helpers

- `WithMessageID(ctx, mid)` / `messageIDFromCtx(ctx)`：assistant row id ctx seam
- `WithToolCallID(ctx, id)` / `ctxToolCallID(ctx)`：eino tool_call_id ctx seam
- `toolCallIDFromCtx(ctx, info)`：**ROOT FIX**——优先 `compose.GetToolCallID(ctx)`（eino ToolsNode 的权威 id），fallback ctx seam，再 fallback `info.Name|info.Type`

### `persistenceWriteContext(parent)`

```go
return context.WithTimeout(context.WithoutCancel(parent), persistenceWriteTimeout)  // 5s
```

每次 DB 写操作用独立 5s timeout context，且 `WithoutCancel(parent)`——graph 主 context 取消时仍能完成最后的 flush。

## 5. 依赖关系

### 上游
- `chain.go::NewDefaultHandlers` 装配，注入 `assistantIDRelay`
- chatruntime defer `FinalizeBatches` 调用 `FinalizeBatch`

### 下游
- `biz.SessionRepo`（DB 接口：`AppendMessage`/`CreateToolCall`/`UpdateToolCallResult`/`FinalizePendingToolCalls`）
- `model.Message`/`model.ToolCall`（DB 模型）
- `compose.GetToolCallID`（eino 权威 tool_call_id）
- `assistantIDRelay`（chain.go 共享）

## 6. 并发与资源管理

### 4 个 mutex 分域

| Mutex | 保护 |
|---|---|
| `toolCallsMu` | `toolCalls` map（OnStart 写 / OnEnd 删） |
| `asstMu` | `asstWrites` 计数 |
| `lastIDsMu` | `lastAssistantID` + `pendingLLMCalls` FIFO |
| `batchMu` | `currentBatch`（openBatch / markSeen / flushIncompleteBatch） |

分域锁减少竞争——tool fan-out 并发 OnStart/OnEnd 时，`toolCallsMu` 是主要热点，其他 mutex 影响小。

### `persistenceWriteTimeout = 5 * time.Second`

每次 DB 写操作独立 5s timeout。`context.WithoutCancel(parent)` 隔离 graph 主 context 取消——保证请求取消时仍能完成最后一次 flush。

### per-request handler 生命周期

注释明确："handler instances are designed for ONE graph run each. The cutover layer constructs a fresh PersistenceHandler per request so the SessionID + per-call state stay scoped."

→ 每请求新建 handler，状态天然隔离，无跨请求泄漏。`FinalizeBatch` 在 invoke 返回后调用清理。

### `toolCalls` map 清理

OnEnd/OnError 删除 entry；`flushIncompleteBatch` 通过 `popUnendedToolCall` 清理残留。`FinalizeBatch` 兜底。无孤儿 entry 累积。

## 7. 设计模式与亮点

### Autoheal 机制

生产事故背景（注释详述）："In live production (.91, session b528bfb0 on 2026-06-06) we observed an assistant turn emitting 4 host_bash tool_calls where only 2 of the 4 OnEnd callbacks fired — chat_tool_calls had all 4 rows but chat_messages was missing 2 role=tool rows, with NO recorded persistence error. Replay then produced an envelope strict providers (DeepSeek v4+) reject with HTTP 400 'insufficient tool messages following tool_calls'."

→ eino ToolsNode 偶发丢失 OnEnd callback，导致 role=tool 行缺失，replay envelope 不合法。autoheal 通过 `assistantBatch` 跟踪 expected/seen，下一轮 ChatModel.OnStart 时为 missing 写 stub 行——既保证 envelope 结构合法，又给 LLM 诚实信号"tool result was not captured"。

### `compose.GetToolCallID` ROOT FIX

注释明确："eino's ToolsNode stamps the authoritative LLM call id (call_XX) onto ctx before each tool runs — compose.GetToolCallID. Read it directly. It's exact and order-independent, so we never reconstruct the id from completion order (which mis-pairs under parallel out-of-order completion and was the source of the synthetic-id orphans → provider 400)."

→ 之前用 FIFO 顺序匹配 LLM call id，并行 tool 完成乱序时 mis-pair，导致 role=tool 行携带 synthetic id `<name>|einoToolAdapter`，replay 时 strict provider 400。改用 `compose.GetToolCallID` 直接读 eino 注入的权威 id，彻底解决。

### `popPendingLLMCall` by name ANYWHERE

不严格 FIFO head 匹配，而是按 tool name 在队列任意位置找。注释解释：parallel tool calls 完成乱序，严格 head 匹配会 mis-pair。同名 tool 仍按 FIFO 顺序配对。

### 错误降级：persist 失败不阻断 graph

```go
if err := h.deps.Repo.AppendMessage(writeCtx, row); err != nil {
    h.recordErr("assistant_insert", err)
    return  // 不返回 error，graph 继续
}
```

`recordErr` 记录到 Logger + errCounter，但 handler 返回 ctx unchanged。这是"可观测性 best-effort"原则——DB 故障不让用户对话中止。

### `assistantIDRelay` 跨 handler 共享

`PersistenceHandler` 写入 assistant row 后 `assistantIDRelay.store(row.ID)`，`SSEHandler` 在发 `assistant_end` 帧前 `assistantIDRelay.load()` 拿到真实 DB id。这是避免 SSE 反查 DB 的轻量方案。

### `persistenceWriteContext` 隔离主 context

```go
context.WithTimeout(context.WithoutCancel(parent), persistenceWriteTimeout)
```

`WithoutCancel` 是 Go 1.21+ 的新 API，让 persistence 写操作不受 graph 主 context 取消影响——保证请求取消时仍能完成最后一次 flush（autoheal stub 行）。

### `pendingToolCallFinalizer` 接口断言

```go
finalizer, ok := h.deps.Repo.(pendingToolCallFinalizer)
if !ok {
    return  // Repo 未实现，跳过
}
```

通过接口断言而非强制 `biz.SessionRepo` 接口包含 `FinalizePendingToolCalls`——让未实现此方法的 Repo 实现仍能工作（向后兼容）。

### `trace` 面包屑日志

每个 callback 触发都 emit 一条 `slog.Info` 面包屑：

```go
h.deps.Logger.Info("persistence callback",
    slog.String("event", event),
    slog.String("session_id", h.deps.SessionID),
    slog.String("component", typ),
    slog.String("name", name),
    slog.String("tool_call_id", toolCallID),
)
```

注释解释："Used to detect 'OnEnd never fired' cases by absence in logs — every expected callback should emit exactly one trace line."

## 8. 注意事项

### `toolCallIDFromCtx` 跨文件共享

本文件定义 `toolCallIDFromCtx`，被 audit.go、metrics.go、sse.go 等同包其他文件调用。本文件是包内 context 工具函数的"主家"。

### Autoheal stub body 硬编码

```go
body := `{"error":"tool response was not persisted (eino ToolsNode OnEnd loss); placeholder synthesised by manager to keep history envelope valid","autoheal":true}`
```

stub 行内容硬编码，LLM 看到此 JSON 后会理解"工具结果丢失"。若未来调整消息格式，注意 LLM prompt engineering 影响。

### `pendingLLMCalls` FIFO 假设 ReAct 串行

注释明确："ReAct serializes tool execution so we can dequeue by order"——但实际 eino ToolsNode 可能并行 fan-out。`popPendingLLMCall` 按 name ANYWHERE 匹配正是为此设计，但同名 tool 多次调用仍依赖 FIFO 顺序，极端情况下可能 mis-pair。

### `currentBatch` 单批次假设

注释明确："at most one batch is live at a time"——但 `openBatch` 检测到 `currentBatch != nil && len(expected) > 0` 时会 `recordErr("batch_overwrite_unflushed")`，说明设计上不期望并发批次。若 ReAct 拓扑改变（如 SOP 图并发 assistant turn），此假设失效。

### `persistenceWriteTimeout = 5 * time.Second` 硬编码

5s timeout 是经验值。若 DB 慢于 5s，persistence 会失败但 graph 继续——用户看不到错误，只是历史不完整。运维需要监控 `ongrid_persist_errors_total` 指标。

### `assistantIDRelay` 可能为空

```go
h.assistantIDRelay.store(row.ID)
```

`store` 方法 nil-safe（`if r == nil { return }`），但若 `NewPersistenceHandler` 直接调用（不经过 `NewDefaultHandlers`），`assistantIDRelay` 为 nil——`store` 静默跳过，SSE 拿不到 id。测试时需要注意。

### `model.Message.Model` 字段可选

```go
if h.deps.Model != "" {
    m := h.deps.Model
    row.Model = &m
}
```

`Model` 空 → column NULL。SPA 用此字段显示 per-message provenance（"this answer came from gpt-4o"）。若 cutover 层未传 Model，历史消息无模型来源标识。

### Streaming 路径未实现

`OnEndWithStreamOutput` 仅 drain+close。token-level streaming 持久化（在 stream-end 写最终 assembled message）属于 PR-7 cutover 层职责，PR-6 未实现。

### `AssistantWriteCount` 测试专用

```go
// AssistantWriteCount reports how many assistant rows this handler has
// persisted. Exposed for tests; production callers should not depend on it.
```

测试用导出方法，生产不要依赖。
