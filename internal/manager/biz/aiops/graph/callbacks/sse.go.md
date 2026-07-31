# graph/callbacks/sse.go

## 1. 概述

本文件实现 `SSEHandler`——eino callback handler，把 graph 执行事件翻译为 SSE 帧推送到客户端。帧类型与 legacy `internal/manager/server/aiops/http.go::writeSSE` 字节兼容，SPA 无需改动即可 round-trip。

帧类型清单：
- `assistant_start` / `assistant_delta` / `assistant_end`：ChatModel 生命周期
- `tool_start` / `tool_end`：Tool 生命周期
- `done` / `error`：graph 终态
- `task_notification`：后台 worker 子 agent 终态通知

`assistant_delta` 是 PR-6 新增的 token-level 流式帧，通过 `OnEndWithStreamOutput` drain ChatModel stream 并 fan-out 每个 chunk。

## 2. 包信息

- **包名**：`callbacks`
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/biz/aiops/graph/callbacks`
- **角色**：eino callback handler；SSE 流式帧发射
- **依赖**：
  - 标准库 `context`、`sync`、`sync/atomic`、`time`
  - `github.com/cloudwego/eino/callbacks`、`components`、`schema`
  - `github.com/cloudwego/eino/components/model`、`tool`

## 3. 关键类型与接口

### `SSEEventType`（字符串枚举）

```go
type SSEEventType string

const (
    SSEEventAssistantStart     SSEEventType = "assistant_start"
    SSEEventAssistantDelta     SSEEventType = "assistant_delta"
    SSEEventAssistantEnd       SSEEventType = "assistant_end"
    SSEEventToolStart          SSEEventType = "tool_start"
    SSEEventToolEnd            SSEEventType = "tool_end"
    SSEEventDone               SSEEventType = "done"
    SSEEventError              SSEEventType = "error"
    SSEEventTaskNotification   SSEEventType = "task_notification"
)
```

### `SSEEvent` 结构体（payload-typed envelope）

```go
type SSEEvent struct {
    Type         SSEEventType
    Iteration    int
    Assistant    *AssistantPayload         // assistant_start / assistant_end
    Delta        *AssistantDelta           // assistant_delta
    Tool         *ToolPayload              // tool_start / tool_end
    Done         *DonePayload              // done
    Error        *ErrorPayload             // error
    Notification *TaskNotificationPayload  // task_notification
}
```

handler 不关心 wire format（HTTP / `event:` lines / JSON encoding），由 cutover 层 emitter 实现编码。

### Payload 类型

| 类型 | 字段 |
|------|------|
| `AssistantPayload` | Iteration, MessageID, Content, PendingToolCalls, CreatedAt |
| `AssistantDelta` | Iteration, Content |
| `ToolPayload` | ToolCallID, Name, ArgsJSON, Status, StartedAt, EndedAt*, DurationMs, ResultJSON, Error |
| `DonePayload` | Iterations |
| `ErrorPayload` | Message, Code |
| `TaskNotificationPayload` | TaskID, Status, Summary, Result?, Err?, Usage? |

`AssistantPayload.MessageID` 由 `assistantIDRelay.load()` 从 PersistenceHandler 拿到真实 DB id。

### `SSEEmitter` 函数类型

```go
type SSEEmitter func(SSEEvent)
```

注释明确："emit MUST NOT block — slow consumers must be handled (drop or buffer) inside the implementation."——emitter 实现必须 non-blocking。

### `SSEHandler` / `toolStart`

```go
type SSEHandler struct {
    emit             SSEEmitter
    iterations       atomic.Int64
    toolStartsMu     sync.Mutex
    toolStarts       map[string]toolStart
    assistantIDRelay *assistantIDRelay  // 跨 handler 共享
}

type toolStart struct {
    at       time.Time
    argsJSON string
    name     string
}
```

## 4. 关键函数与流程

### `NewSSEHandler(emit)`

`emit == nil` → 返回 nil（cutover 层 per-request opt-in/out）。

### `Needed(ctx, info, timing) bool`

| Component | Timing |
|---|---|
| ChatModel | OnStart, OnEnd, OnError, **OnEndWithStreamOutput** |
| Tool | OnStart, OnEnd, OnError |
| Graph/其他 | OnEnd, OnError |

ChatModel 多了 `OnEndWithStreamOutput`——用于 token-level streaming。

### `OnStart(ctx, info, input)`

**ChatModel**：
```go
it := h.iterations.Add(1)
h.emit(SSEEvent{
    Type:      SSEEventAssistantStart,
    Iteration: int(it),
    Assistant: &AssistantPayload{Iteration: int(it)},
})
```

**Tool**：
```go
args := tin.ArgumentsInJSON  // 可能为 ""
startedAt := time.Now().UTC()
key := toolCallIDFromCtx(ctx, info)
h.toolStarts[key] = toolStart{at: startedAt, argsJSON: args, name: info.Name}
h.emit(SSEEvent{
    Type: SSEEventToolStart,
    Tool: &ToolPayload{
        ToolCallID: key,
        Name:       info.Name,
        ArgsJSON:   args,
        Status:     "pending",
        StartedAt:  startedAt,
    },
})
```

### `OnEnd(ctx, info, output)`

**ChatModel**：
```go
mo := einomodel.ConvCallbackOutput(output)
var content string
var pending int
if mo.Message != nil {
    content = mo.Message.Content
    pending = len(mo.Message.ToolCalls)
}
it := int(h.iterations.Load())
h.emit(SSEEvent{
    Type:      SSEEventAssistantEnd,
    Iteration: it,
    Assistant: &AssistantPayload{
        Iteration:        it,
        MessageID:        h.assistantIDRelay.load(),  // 从 Persistence 拿 DB id
        Content:          content,
        PendingToolCalls: pending,
        CreatedAt:        time.Now().UTC(),
    },
})
```

**Tool**：
```go
ts, ok := h.toolStarts[key]  // 取出并 delete
endedAt := time.Now().UTC()
var dur int64
if ok {
    dur = endedAt.Sub(ts.at).Milliseconds()
}
body := tout.Response
h.emit(SSEEvent{
    Type: SSEEventToolEnd,
    Tool: &ToolPayload{
        ToolCallID: key,
        Name:       info.Name,
        ArgsJSON:   ts.argsJSON,
        Status:     "success",
        StartedAt:  ts.at,
        EndedAt:    &endedAt,
        DurationMs: dur,
        ResultJSON: body,
    },
})
```

**Graph/其他**：
```go
h.emit(SSEEvent{
    Type: SSEEventDone,
    Done: &DonePayload{Iterations: int(h.iterations.Load())},
})
```

### `OnError(ctx, info, err)`

**Tool**：发 `tool_end` 帧，status="error" 或 "timeout"（`isDeadlineErr`）。

**其他（ChatModel / Graph）**：发 terminal `error` 帧：
```go
h.emit(SSEEvent{
    Type:  SSEEventError,
    Error: &ErrorPayload{Message: err.Error()},
})
```

### `OnEndWithStreamOutput(ctx, info, out)`（streaming 核心）

```go
if info.Component != components.ComponentOfChatModel {
    out.Close()
    return ctx
}
go h.drainStream(out)  // 异步 drain
return ctx
```

启动 goroutine 异步 drain ChatModel stream，每个非空 Content chunk 发一个 `assistant_delta` 帧：

```go
func (h *SSEHandler) drainStream(out *schema.StreamReader[callbacks.CallbackOutput]) {
    defer out.Close()
    for {
        chunk, err := out.Recv()
        if err != nil {
            return
        }
        mo := einomodel.ConvCallbackOutput(chunk)
        if mo == nil || mo.Message == nil {
            continue
        }
        if mo.Message.Content == "" {
            continue
        }
        it := int(h.iterations.Load())
        h.emit(SSEEvent{
            Type:      SSEEventAssistantDelta,
            Iteration: it,
            Delta:     &AssistantDelta{Iteration: it, Content: mo.Message.Content},
        })
    }
}
```

注释解释："We close the receiver-side copy when done so eino can reclaim the stream goroutine; the original output stream continues to flow to downstream graph nodes."

### `IterationCount() int`

测试用导出方法，返回 `iterations.Load()`。

## 5. 依赖关系

### 上游
- `chain.go::NewDefaultHandlers` 装配，注入 `assistantIDRelay`
- chatruntime 提供 `SSEEmitter` 实现（包装 http.Flusher + writeSSE）

### 下游
- `SSEEmitter`（cutover 层实现的函数类型）
- `assistantIDRelay`（chain.go 共享，从 PersistenceHandler 拿 chat_messages.id）
- `toolCallIDFromCtx`、`isDeadlineErr`（包内其他文件）

## 6. 并发与资源管理

### `iterations atomic.Int64`

ChatModel 调用次数原子计数，用于 assistant 帧的 `iteration` 字段。tool fan-out 并发 callback 时安全。

### `toolStartsMu sync.Mutex` 保护 map

```go
h.toolStartsMu.Lock()
h.toolStarts[key] = toolStart{...}
h.toolStartsMu.Unlock()
```

OnStart 写 / OnEnd 删，锁范围最小化。

### `drainStream` goroutine

`OnEndWithStreamOutput` 启动 goroutine 异步 drain stream：

```go
go h.drainStream(out)
```

注释明确："The emitter is called sequentially within the lifetime of a single graph run; tool fan-out (compose ToolsNode parallel execution) emits per-tool start/end events that may interleave — the iteration counter is atomic so the assistant_start sequence is consistent."

`drainStream` 内部 `defer out.Close()` 保证 stream 资源释放。emitter 调用是 sequential 的（goroutine 内单线程循环），无并发 emitter 调用。

### emitter non-blocking 约束

注释明确："emit MUST NOT block — slow consumers must be handled (drop or buffer) inside the implementation."

→ emitter 实现必须 non-blocking。cutover 层的 http.go writeSSE 通过 response writer 序列化，本身就是单 goroutine，但若客户端慢，writeSSE 可能 block——cutover 层需要实现 drop/buffer 策略。

### per-request handler 生命周期

handler per-request 创建，状态 scope 到请求。`drainStream` goroutine 在 stream 结束时自然退出，无 leak。

## 7. 设计模式与亮点

### payload-typed envelope

`SSEEvent` 用 payload-typed envelope（每个 Type 对应一个 Payload 字段），handler 不关心 wire format。这是类型安全的设计——emit 时编译器检查 payload 字段类型，避免 runtime JSON 错误。

### `assistantIDRelay` 跨 handler 协作

```go
MessageID: h.assistantIDRelay.load(),
```

PersistenceHandler 写入 assistant row 后 `store(row.ID)`，SSEHandler 在 `assistant_end` 帧前 `load()` 拿到真实 DB id。注释明确："chain.go registers Persistence before SSE so by the time we emit here, the AppendMessage for this iter is done. Empty string when persistence is disabled (e.g. tests that wire only SSE)."

→ 装配顺序保证 Persistence 先写，SSE 后读。测试时若只用 SSE，`assistantIDRelay` 为 nil，`load()` 返回空字符串。

### `assistant_delta` token-level streaming

`OnEndWithStreamOutput` 是 PR-6 新增能力——drain ChatModel stream，每个 Content chunk 发一个 `assistant_delta` 帧。SPA 收到后 append 到当前 bubble，实现"打字机"效果。

注释明确："assistant_delta 是新增的 token-level 帧"——区别于 `assistant_end` 的 full content。

### `tool_start` / `tool_end` 帧的 duration

```go
ts, ok := h.toolStarts[key]  // OnStart 时 stash
endedAt := time.Now().UTC()
if ok {
    dur = endedAt.Sub(ts.at).Milliseconds()
}
```

`tool_end` 帧携带 `DurationMs`，SPA 可显示"工具调用耗时 X ms"。这是 UX 增强。

### Graph-scope OnEnd 发 `done` 帧

```go
default:
    h.emit(SSEEvent{
        Type: SSEEventDone,
        Done: &DonePayload{Iterations: int(h.iterations.Load())},
    })
```

graph 终态（非 ChatModel/Tool）触发 `done` 帧，SPA 收到后关闭 streaming UI。`Iterations` 字段让 SPA 显示"本次对话共 N 轮 LLM 调用"。

### `OnError` 区分 tool error 和 graph error

- Tool error：发 `tool_end` 帧（status=error/timeout），graph 继续
- 其他 error：发 terminal `error` 帧，graph 终止

→ tool 失败不终止 graph（用户可看到 tool 失败 + 后续 assistant 回复），graph 级失败才发 terminal error。

### `TaskNotificationPayload` 多 agent 场景

```go
type TaskNotificationPayload struct {
    TaskID  string         `json:"task_id"`
    Status  string         `json:"status"`  // completed | failed | killed
    Summary string         `json:"summary"`
    Result  string         `json:"result,omitempty"`
    Err     string         `json:"error,omitempty"`
    Usage   map[string]any `json:"usage,omitempty"`
}
```

coordinator runtime 通过同一 SSE channel 发 worker 终态通知，SPA 内联渲染 worker-result tile。本 handler 不直接发 `task_notification` 帧——由 coordinator runtime 直接调 emitter。

## 8. 注意事项

### emitter non-blocking 是契约不是保证

注释明确 emit MUST NOT block，但这是契约约定。若 cutover 层 emitter 实现 block（如 http writer 慢），handler 的 OnEnd 会 block，整个 graph callback 链 block。cutover 层必须实现 drop/buffer。

### `drainStream` goroutine 与 graph 终态竞态

`OnEndWithStreamOutput` 启动 goroutine 异步 drain，但 graph 可能在 drain 完成前到达终态（发 `done` 帧）。当前实现未等待 drain 完成——SPA 可能先收到 `done` 再收到最后的 `assistant_delta`。这是已知的时序边界，SPA 需要容忍。

### `iterations.Load()` 在 streaming 中的语义

`drainStream` 内 `it := int(h.iterations.Load())`——读当前值。若 drain 跨越多个 ChatModel 调用（不太可能，但理论上），`iteration` 字段会变。当前实现假设 drain 在同一 iteration 内完成。

### `toolStarts` map 孤儿 entry

若 Tool OnStart 成功但 OnEnd/OnError 未调用，`toolStarts` 留下孤儿 entry。handler per-request 创建，GC 回收，但单次请求内可能积累。

### `AssistantPayload.MessageID` 可能为空

```go
MessageID: h.assistantIDRelay.load(),
```

`load()` 在以下情况返回空：
- persistence disabled（测试只装 SSE）
- PersistenceHandler 还没写入（首 iteration OnEnd 前）
- `assistantIDRelay` 为 nil（直接 `NewSSEHandler` 而非经 `NewDefaultHandlers`）

SPA 需要容忍空 MessageID（v0.7.63 hotfix 用 synthetic assistant-iter-N id dedupe）。

### `task_notification` 帧不由本 handler 发

`TaskNotificationPayload` 类型定义在本文件，但 `SSEEventTaskNotification` 帧由 coordinator runtime 直接调 emitter 发送，不经本 handler。本 handler 只负责 graph callback 翻译。

### `SSEEventError` 的 `Code` 字段未填

```go
h.emit(SSEEvent{
    Type:  SSEEventError,
    Error: &ErrorPayload{Message: err.Error()},  // Code 未填
})
```

`ErrorPayload.Code` 是空字符串。未来可扩展为 `budget_exceeded` / `max_iterations` / `internal_error` 等分类码，便于 SPA 区分错误类型。

### streaming 路径与 PersistenceHandler 协作

`OnEndWithStreamOutput` 启动 goroutine drain stream，但 `OnEnd`（非 stream 路径）仍会触发 `assistant_end` 帧。streaming 模式下 SPA 可能既收到 `assistant_delta` 又收到 `assistant_end`——SPA 需要把 `assistant_end` 视作"stream 结束 + 最终 content 确认"。
