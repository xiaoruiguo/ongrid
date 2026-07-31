# graph/callbacks/audit.go

## 1. 概述

本文件实现 `AuditHandler`——一个 eino callback handler，为 graph 运行的每个阶段输出结构化 `slog.INFO` 日志。覆盖两类组件：
- **ChatModel**：每次 LLM 调用记录 prompt token 估算 + 回复 token + tool_calls 数
- **Tool**：每次工具调用记录名称 + 耗时 + 状态 + args/result 字节数

**核心红线**：用户原始 prompt content **绝不**进入日志，只记录计数和标识符。Tool args 同样只记 `args_bytes`（args 可能含用户编写的 PromQL/LogQL 查询，禁止明文记录）。

## 2. 包信息

- **包名**：`callbacks`
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/biz/aiops/graph/callbacks`
- **角色**：eino callback handler；结构化审计日志
- **依赖**：
  - 标准库 `context`、`log/slog`、`sync`、`sync/atomic`、`time`
  - `github.com/cloudwego/eino/callbacks`
  - `github.com/cloudwego/eino/components`
  - `github.com/cloudwego/eino/components/model`（`einomodel.ConvCallbackInput/Output`）
  - `github.com/cloudwego/eino/components/tool`（`einotool.ConvCallbackInput/Output`）
  - `github.com/cloudwego/eino/schema`

## 3. 关键类型与接口

### `AuditDeps` 结构体

```go
type AuditDeps struct {
    Logger    *slog.Logger
    SessionID string
    UserID    uint64
}
```

- `Logger`：必填，nil 时 `NewAuditHandler` 返回 nil（cutover 层可按环境 opt in/out）
- `SessionID`：本次 chat session 标识，每行日志都带，便于 grep 同一对话所有阶段
- `UserID`：数字 scope，记录为 `slog.Uint64`

### `AuditHandler` 结构体

```go
type AuditHandler struct {
    deps     AuditDeps
    chatTurn atomic.Int64
    startsMu sync.Mutex
    starts   map[string]auditStart
}
```

- `chatTurn`：原子计数 ChatModel 调用次数
- `starts`：mutex 保护的 map，存每个阶段的开始信息（用于 OnEnd 计算 duration）

### `auditStart` 结构体（未导出）

```go
type auditStart struct {
    at         time.Time
    component  components.Component
    name       string
    estTokens  int        // chat_model 的估算 prompt token
    toolCallID string     // tool 的 call id
}
```

## 4. 关键函数与流程

### `NewAuditHandler(deps) *AuditHandler`

```go
if deps.Logger == nil {
    return nil  // cutover 层可短路
}
return &AuditHandler{deps: deps, starts: make(map[string]auditStart)}
```

### `Needed(_ ctx, info, timing) bool`

仅响应 `ComponentOfChatModel` 和 `ComponentOfTool` 的 `OnStart`/`OnEnd`/`OnError`。

### `OnStart(ctx, info, input) context.Context`

记录阶段开始时间，按组件类型分组记录：

**ChatModel**：
```go
mi := einomodel.ConvCallbackInput(input)
if mi != nil {
    entry.estTokens = estimatePromptTokens(mi.Messages)
}
turn := h.chatTurn.Add(1)
h.deps.Logger.Info("graph stage start",
    slog.String("session_id", h.deps.SessionID),
    slog.Uint64("user_id", h.deps.UserID),
    slog.String("kind", "chat_model"),
    slog.String("name", info.Name),
    slog.Int64("iteration", turn),
    slog.Int("est_prompt_tokens", entry.estTokens),
)
```

**Tool**：
```go
entry.toolCallID = toolCallIDFromCtx(ctx, info)
args := ""
if tin := einotool.ConvCallbackInput(input); tin != nil {
    args = tin.ArgumentsInJSON
}
// 只记 args_bytes，不记 args content
h.deps.Logger.Info("graph stage start",
    ...
    slog.String("kind", "tool"),
    slog.String("tool_call_id", entry.toolCallID),
    slog.Int("args_bytes", len(args)),
)
```

最后把 `entry` 存入 `starts[stageKey(ctx, info)]`，供 `OnEnd` 取出计算 duration。

### `OnEnd(ctx, info, output) context.Context`

1. 从 `starts` 取出 entry 并 delete（一次性）
2. 计算 `dur := time.Since(entry.at)`
3. 按组件类型分组记录：

**ChatModel**：
```go
mo := einomodel.ConvCallbackOutput(output)
usage := slog.Group("token_usage")  // 默认空 group
toolCalls := 0
if mo != nil {
    // 优先从 mo.TokenUsage 取，其次从 mo.Message.ResponseMeta.Usage 取
    if mo.Message != nil {
        toolCalls = len(mo.Message.ToolCalls)
    }
}
h.deps.Logger.Info("graph stage end",
    ...
    slog.Int64("duration_ms", dur.Milliseconds()),
    slog.Int("tool_calls_emitted", toolCalls),
    slog.String("status", "success"),
    usage,
)
```

**Tool**：
```go
body := ""
if tout := einotool.ConvCallbackOutput(output); tout != nil {
    body = tout.Response
}
h.deps.Logger.Info("graph stage end",
    ...
    slog.Int64("duration_ms", dur.Milliseconds()),
    slog.Int("result_bytes", len(body)),  // 只记字节数，不记 content
    slog.String("status", "success"),
)
```

### `OnError(ctx, info, err) context.Context`

```go
status := "error"
if isDeadlineErr(err) {
    status = "timeout"
}
h.deps.Logger.Warn("graph stage end",
    ...
    slog.String("kind", componentKind(info.Component)),
    slog.Int64("duration_ms", dur.Milliseconds()),
    slog.String("status", status),
    slog.String("error", err.Error()),
)
```

注意：`OnError` 用 `Warn` 级别（其他用 `Info`），且只在这里记录 `error` 字段。

### `OnStartWithStreamInput` / `OnEndWithStreamOutput`

drain + close stream copy，不做审计。streaming 路径审计未实现。

### `estimatePromptTokens(msgs) int`

本地 rule-of-thumb 实现（chars/4 + perMsgOverhead=4）：

```go
const perMsgOverhead = 4
total := 0
for _, m := range msgs {
    total += perMsgOverhead
    total += len(m.Content) / 4
    for _, tc := range m.ToolCalls {
        total += len(tc.Function.Name) / 4
        total += len(tc.Function.Arguments) / 4
    }
}
return total
```

注释明确："Local copy so we don't pull the llm package in just for the function (avoids an import cycle with biz/aiops/graph)."——避免 `callbacks` 包反向依赖 `graph` 包。

### `componentKind(c) string`

```go
switch c {
case components.ComponentOfChatModel: return "chat_model"
case components.ComponentOfTool:       return "tool"
default:                               return string(c)
}
```

### `stageKey(ctx, info) string`

per-stage 关联键，用于 OnEnd/OnError 找匹配的 OnStart entry：

```go
if info.Component == components.ComponentOfTool {
    return "tool|" + toolCallIDFromCtx(ctx, info)
}
if v, ok := ctx.Value(messageIDCtxKey{}).(string); ok && v != "" {
    return string(info.Component) + "|" + info.Name + "|" + v
}
return string(info.Component) + "|" + info.Name
```

- **Tool**：用 `tool_call_id`（同一工具不同 call 不冲突）
- **ChatModel**：用 `component|name|messageID`（容忍未来 SOP 图并发 fan-out chat models）；无 messageID 时退化到 `component|name`

## 5. 依赖关系

### 上游
- `callbacks/chain.go::NewDefaultHandlers` 装配本 handler
- `toolCallIDFromCtx` / `messageIDCtxKey` / `isDeadlineErr` 来自 callbacks 包内其他文件（`context_keys.go` 等）

### 下游
- `slog.Logger`（标准库结构化日志）
- eino callback 接口

## 6. 并发与资源管理

### `chatTurn atomic.Int64`

ChatModel 调用次数用原子计数，无锁。tool fan-out 并发调用 OnStart 时安全递增。

### `startsMu sync.Mutex` 保护 map

```go
h.startsMu.Lock()
h.starts[stageKey(ctx, info)] = entry
h.startsMu.Unlock()
```

锁范围最小化——仅 map 操作。OnEnd 同样模式：lock → 取出 → delete → unlock，然后无锁计算 duration 和记日志。

### 单次 graph run 一个实例

注释明确："One handler instance per graph run. Concurrency: tool fan-out may call OnStart / OnEnd concurrently; per-call timestamps live in a mutex-guarded map."

每个 graph run 创建新实例，状态天然隔离，无需跨 run 清理。

## 7. 设计模式与亮点

### 红线设计：不记录 content

注释三次强调：
- "the user's raw prompt content is NEVER included in the log line; only counts and identifiers."
- "Log args size, not args content — the args may carry user-authored text (PromQL / LogQL queries) that forbids us from logging in cleartext."
- Tool OnEnd 仅记 `result_bytes`，不记 `body` content

这是合规设计的典范——审计日志必须能交给运维/审计方查看，不能成为 PII 泄露点。

### `Needed` 精准过滤

```go
switch info.Component {
case components.ComponentOfChatModel, components.ComponentOfTool:
    switch timing {
    case callbacks.TimingOnStart, callbacks.TimingOnEnd, callbacks.TimingOnError:
        return true
    }
}
return false
```

只对感兴趣的组件 + 时序返回 true，其他一律 false——避免无谓的 callback 调用开销。eino callback 系统会在调用 OnStart 前先调 Needed 判定。

### `stageKey` 防并发冲突

Tool 用 `tool_call_id` 作 key——同一工具不同 call 不冲突。
ChatModel 用 `component|name|messageID`——容忍未来 SOP 图并发 fan-out chat models（eino 当前不 fan out chat models，但前向兼容）。

### `Logger == nil` 短路

```go
if deps.Logger == nil {
    return nil
}
```

cutover 层可按环境 opt in/out：测试环境传 nil Logger 跳过审计；生产环境传真实 Logger 启用。返回 nil 让 `NewDefaultHandlers` 跳过本 handler。

### Token 估算的本地实现

`estimatePromptTokens` 是 `llm` 包同款 rule-of-thumb 的本地副本。注释解释：避免 `callbacks` 包反向依赖 `biz/aiops/graph` 包（imports `llm`）。这是模块边界设计——callbacks 应该是叶子包，不应依赖业务包。

### Token usage 双路径提取

```go
if mo.TokenUsage != nil {
    usage = slog.Group("token_usage", prompt, completion, total)
} else if mo.Message != nil && mo.Message.ResponseMeta != nil && mo.Message.ResponseMeta.Usage != nil {
    u := mo.Message.ResponseMeta.Usage
    usage = slog.Group("token_usage", prompt, completion, total)
}
```

兼容两种 eino 版本/模型的 token usage 返回路径——`TokenUsage` 字段和 `ResponseMeta.Usage` 字段。任一可用即记录。

### `OnError` 用 Warn 级别

错误用 `Warn` 而非 `Info`，便于运维按级别过滤问题。`status` 字段区分 `error`/`timeout`（`isDeadlineErr` 判定）。

## 8. 注意事项

### `isDeadlineErr` 在包内其他文件

本文件调用 `isDeadlineErr(err)` 但未定义——它在 callbacks 包的其他文件（如 `context_keys.go` 或专门的错误判定文件）。修改时注意跨文件依赖。

### `toolCallIDFromCtx` / `messageIDCtxKey` 同样跨文件

这些 context key 工具函数在包内其他文件定义。本文件依赖它们构造 `stageKey`。

### `chatTurn` 跨 graph run 不重置

`chatTurn` 是 `atomic.Int64`，每个 `AuditHandler` 实例独立。由于"One handler instance per graph run"，每次 graph run 创建新实例，计数从 0 开始——这是正确语义。但若误把同一 handler 实例跨 run 复用，计数会累积。

### `starts` map 不清理孤儿 entry

若 OnStart 成功但 OnEnd/OnError 因异常未调用，`starts` 中会留下孤儿 entry，直到 handler 实例被 GC。当前实现可接受（handler per-run 创建，GC 自动回收），但长期运行的服务可能积累。

### Streaming 路径未实现

`OnStartWithStreamInput`/`OnEndWithStreamOutput` 仅 drain + close，不做审计。streaming 模式下的 token 用量、tool 结果字节数等不会进入审计日志。这是一个待补完的缺口。

### `est_prompt_tokens` 是估算非精确

`chars/4` 是粗略估算，对中文（每字 1-2 token）和高 emoji 内容偏差较大。日志中标记 `est_` 前缀提示读者这是估算值，不能用于计费。

### `Logger.Info` 字段顺序固定

所有 `Info` 调用都按 `session_id` → `user_id` → `kind` → `name` → ... 顺序记录，便于 grep 后人工对齐。修改时保持字段顺序一致。

### `UserID` 作为 `slog.Uint64`

```go
slog.Uint64("user_id", h.deps.UserID)
```

UserID 是数字 scope，可作为 Prometheus label 的高基数字段——但本文件**只**写入 slog 日志，不进 metrics（metrics 由 `MetricsHandler` 负责）。这符合"高基数字段禁止作为 Prometheus label"的红线。
