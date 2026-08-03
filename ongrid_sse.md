# OnGrid SSE (Server-Sent Events) 技术实现深度分析

> 本文档完整分析 OnGrid 系统中 SSE 的技术实现，涵盖后端写入、回调链事件发射、前端消费解析、连接生命周期、代理配置等全链路。

---

## 目录

1. [SSE 架构总览](#1-sse-架构总览)
2. [SSE 端点与帧协议](#2-sse-端点与帧协议)
3. [后端 SSE 写入实现](#3-后端-sse-写入实现)
4. [三层事件体系与翻译链](#4-三层事件体系与翻译链)
5. [SSE 回调处理器](#5-sse-回调处理器)
6. [回调链组装与 assistantIDRelay](#6-回调链组装与-assistantidrelay)
7. [前端 SSE 消费与解析](#7-前端-sse-消费与解析)
8. [前端事件处理与 UI 更新](#8-前端事件处理与-ui-更新)
9. [SSE 连接生命周期](#9-sse-连接生命周期)
10. [错误处理策略](#10-错误处理策略)
11. [Nginx 代理层 SSE 配置](#11-nginx-代理层-sse-配置)
12. [WebSocket vs SSE 分工](#12-websocket-vs-sse-分工)
13. [IM Bridge SSE 适配](#13-im-bridge-sse-适配)
14. [MCP Client SSE 读端](#14-mcp-client-sse-读端)
15. [架构红线](#15-架构红线)
16. [关键文件索引](#16-关键文件索引)

---

## 1. SSE 架构总览

### 1.1 SSE 在 OnGrid 中的定位

OnGrid 中 SSE **仅用于 AI 聊天流式响应**。WebSSH 使用 WebSocket，日志/通知没有 SSE 端点。

| 功能 | 协议 | 端点 |
|------|------|------|
| AI 聊天流式 | SSE | `POST /v1/chat/sessions/{id}/messages/stream` |
| WebSSH 终端 | WebSocket | `GET /v1/devices/{id}/shell` |

### 1.2 数据流全景

```
前端 fetch(POST, Accept: text/event-stream)
  │
  ▼
http.go::postMessageStream ─── SSE 握手 + 心跳帧
  │ emit 闭包 → writeSSE
  ▼
service.go::runWithKernel ─── context.WithoutCancel + context.WithCancel
  │
  ▼
runtime.go::Handle ─── 构建 Graph + 回调链
  │
  ▼
eino ReAct Graph Invoke
  │ 回调触发:
  │   OnStart(ChatModel) → SSEHandler → assistant_start (丢弃)
  │   OnEndWithStreamOutput → drainStream → assistant_delta (丢弃)
  │   OnEnd(ChatModel) → Persistence → SSEHandler → assistant_end → "assistant" 帧
  │   OnStart(Tool) → Persistence → SSEHandler → tool_start 帧
  │   OnEnd(Tool) → Persistence → SSEHandler → tool_end 帧
  │   OnEnd(Graph) → SSEHandler → done 帧
  │   OnError → SSEHandler → error 帧
  ▼
writeSSE() → "event: X\ndata: Y\n\n" → Flush → 网络 → 前端
  │
  ▼
前端 ReadableStream reader.read() → buf 累积 → \n\n 切割 → dispatchFrame → callbacks
```

### 1.3 核心设计决策

| 决策 | 说明 |
|------|------|
| 使用 fetch 而非 EventSource | EventSource 只支持 GET，且不支持自定义 headers |
| 每帧立即 Flush | 确保浏览器增量渲染 |
| 写入错误静默吞没 | 客户端断连后无法做任何有用的事 |
| 无周期性心跳 | 仅连接建立时发一次 `: ok\n\n`，依赖业务事件保活 |
| 请求脱离 HTTP 生命周期 | `context.WithoutCancel` (HLD-021) |
| assistant_delta 当前被丢弃 | SPA 尚未支持 token 级流式，待 feature flag 启用 |

---

## 2. SSE 端点与帧协议

### 2.1 唯一生产 SSE 端点

**文件**: [http.go](file:///d:/claude/ongrid/internal/manager/server/aiops/http.go) 行 135

```go
r.Post("/v1/chat/sessions/{id}/messages/stream", h.postMessageStream)
```

### 2.2 SSE 帧线格式

每帧的精确字节序列：

```
event: <name>\ndata: <json-payload>\n\n
```

示例（assistant 帧）：

```
event: assistant\n
data: {"session_id":"abc","iteration":1,"message_id":"def","content":"hello","created_at":"2026-01-01T00:00:00Z","pending_tool_calls":0}\n
\n
```

### 2.3 心跳/注释帧

```
: ok\n\n
```

SSE 规范中 `:` 开头为注释行，浏览器 EventSource 忽略但确认连接存活。

### 2.4 所有 SSE 事件类型

| 事件名 | 触发时机 | Payload |
|--------|---------|---------|
| `assistant` | ChatModel 完成，assistant turn 持久化后 | iteration, message_id, content, created_at, pending_tool_calls |
| `tool_start` | 工具调用开始，写入 pending 状态后 | tool_call_id, name, status="pending", started_at, arguments |
| `tool_end` | 工具执行完成 | tool_call_id, name, status, duration_ms, result, error |
| `done` | Graph 终端成功 | session_id, assistant_message, tool_calls, usage, iterations |
| `error` | 不可恢复错误 | error, code |
| `task_notification` | 后台子 agent worker 到达终态 | task_id, status, summary, result, error, usage |
| `approval_pending` | 阻塞工具等待人工审批 (HLD-021) | approval_id, tool_call_id, kind, command, credentials |
| `summary` | belt-and-suspenders 兜底 | 同 done |

---

## 3. 后端 SSE 写入实现

### 3.1 SSE 握手

**文件**: [http.go](file:///d:/claude/ongrid/internal/manager/server/aiops/http.go) 行 438-457

```go
flusher, ok := w.(http.Flusher)
if !ok {
    // 无流式支持 — 降级为阻塞 JSON
    reply, err := h.svc.PostMessageWithOpts(...)
    writeJSON(w, http.StatusOK, toPostMessageResp(id, reply))
    return
}

w.Header().Set("Content-Type", "text/event-stream")
w.Header().Set("Cache-Control", "no-cache")
w.Header().Set("X-Accel-Buffering", "no") // 禁用 nginx 响应缓冲
w.WriteHeader(http.StatusOK)
_, _ = w.Write([]byte(": ok\n\n"))  // 心跳帧
flusher.Flush()
```

| 项目 | 值 | 说明 |
|------|------|------|
| Content-Type | `text/event-stream` | SSE 标准头 |
| Cache-Control | `no-cache` | 禁止代理/浏览器缓存 |
| X-Accel-Buffering | `no` | nginx 专用：关闭响应缓冲 |
| 心跳帧 | `: ok\n\n` | SSE 注释行，确认连接存活 |
| Flusher 检测 | `w.(http.Flusher)` | 不支持时降级为阻塞 JSON |
| HTTP 状态码 | 200 | 握手成功后立即 WriteHeader |

### 3.2 writeSSE 函数

**文件**: [http.go](file:///d:/claude/ongrid/internal/manager/server/aiops/http.go) 行 598-612

```go
func writeSSE(w http.ResponseWriter, f http.Flusher, name string, payload any) {
    body, err := json.Marshal(payload)
    if err != nil {
        body = []byte(`{}`)  // JSON 编码失败兜底
    }
    _, _ = w.Write([]byte("event: "))
    _, _ = w.Write([]byte(name))
    _, _ = w.Write([]byte("\ndata: "))
    _, _ = w.Write(body)
    _, _ = w.Write([]byte("\n\n"))
    f.Flush()  // 每帧立即 Flush
}
```

关键行为：
- **写入错误被 `_, _` 吞没**：客户端断连后无法做任何有用的事
- **JSON 编码失败退化为 `{}`**：确保帧格式完整
- **每帧立即 Flush**：确保浏览器可以增量渲染

### 3.3 事件名映射

**文件**: [http.go](file:///d:/claude/ongrid/internal/manager/server/aiops/http.go) 行 478-495

```go
func eventName(t agent.EventType) string {
    switch t {
    case agent.EventAssistant:        return "assistant"
    case agent.EventToolStart:        return "tool_start"
    case agent.EventToolEnd:          return "tool_end"
    case agent.EventDone:             return "done"
    case agent.EventTaskNotification: return "task_notification"
    case agent.EventApprovalPending:  return "approval_pending"
    default:                          return string(t)
    }
}
```

### 3.4 Payload 构建

**文件**: [http.go](file:///d:/claude/ongrid/internal/manager/server/aiops/http.go) 行 497-595

#### `assistant` 帧 (行 499-510)

```json
{
  "session_id": "uuid",
  "iteration": 1,
  "message_id": "uuid",
  "content": "助手回复文本",
  "created_at": "2026-01-01T00:00:00Z",
  "pending_tool_calls": 2
}
```

#### `tool_start` / `tool_end` 帧 (行 511-551)

```json
{
  "session_id": "uuid",
  "tool_call_id": "uuid",
  "name": "query_promql",
  "status": "pending",
  "started_at": "2026-01-01T00:00:00Z",
  "duration_ms": 1234,
  "edge_id": 123,
  "ended_at": "2026-01-01T00:00:01Z",
  "error": "",
  "arguments": { ... },
  "arguments_raw": "{ ... }",
  "result": { ... },
  "result_raw": "{ ... }"
}
```

**ArgsJSON/ResultJSON 智能序列化** (行 532-550)：尝试 `json.Unmarshal`，成功则发送为 parsed JSON 对象（`arguments`/`result`），失败则发送为原始字符串（`arguments_raw`/`result_raw`）。

#### `done` 帧 (行 552-556)

Payload 等同于 `toPostMessageResp`，包含完整的 `assistant_message`、`tool_calls`、`usage`、`iterations`。

#### `task_notification` 帧 (行 557-576)

```json
{
  "session_id": "uuid",
  "task_id": "agent-12ab34cd",
  "status": "completed",
  "summary": "Agent incident-investigator completed",
  "result": "...",
  "error": "...",
  "usage": { "duration_ms": 12345 }
}
```

#### `approval_pending` 帧 (行 577-591)

```json
{
  "session_id": "uuid",
  "approval_id": "uuid",
  "tool_call_id": "uuid",
  "kind": "cloud_bash",
  "command": "rm -rf /",
  "credentials": ["sudo"]
}
```

#### `error` 帧 (行 465-468)

```json
{
  "error": "具体错误消息",
  "code": "not-found"
}
```

错误码枚举 (行 1015-1036): `not-found`, `unauthorized`, `forbidden`, `conflict`, `invalid`, `budget-exceeded`, `edge-offline`, `not-wired-yet`, `internal`

---

## 4. 三层事件体系与翻译链

### 4.1 三层事件定义

| 层级 | 文件 | 事件类型 | 说明 |
|------|------|---------|------|
| Graph Callback 层 | [sse.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/sse.go) | `SSEEvent*` (8 种) | eino 回调直接产出 |
| Chatruntime 层 | [runtime.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go) | `Event*` (7 种) | 面向前端的统一事件 |
| Agent 层 (Legacy) | [agent.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/agent/agent.go) | `Event*` (6 种) | HTTP 层消费的最终事件 |

### 4.2 Graph Callback 层事件

**文件**: [sse.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/sse.go) 行 22-50

| 常量 | 值 | 用途 |
|------|------|------|
| `SSEEventAssistantStart` | `"assistant_start"` | ChatModel 开始生成前 |
| `SSEEventAssistantDelta` | `"assistant_delta"` | token 级增量流式帧 |
| `SSEEventAssistantEnd` | `"assistant_end"` | ChatModel 完成后 |
| `SSEEventToolStart` | `"tool_start"` | 工具启动 |
| `SSEEventToolEnd` | `"tool_end"` | 工具完成 |
| `SSEEventDone` | `"done"` | 图终态成功 |
| `SSEEventError` | `"error"` | 不可恢复错误 |
| `SSEEventTaskNotification` | `"task_notification"` | 后台 worker 终态 |

### 4.3 SSEEvent 信封结构

**文件**: [sse.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/sse.go) 行 57-66

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

### 4.4 翻译链：toCallbackEmitter

**文件**: [runtime.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go) 行 859-925

将 `chatruntime.Emit` 适配为 `callbacks.SSEEmitter`：

| 回调层事件 | 翻译为 | 处理 |
|-----------|--------|------|
| `SSEEventAssistantEnd` | `EventAssistant` | 字段逐一映射 |
| `SSEEventAssistantStart` | **丢弃** | 前端没有 assistant_start 帧，避免空泡 (行 873-879) |
| `SSEEventAssistantDelta` | **丢弃** | SPA 尚未支持，受 feature flag 门控 (行 880-883) |
| `SSEEventToolStart` | `EventToolStart` | 字段映射 |
| `SSEEventToolEnd` | `EventToolEnd` | 字段映射 |
| `SSEEventDone` | **丢弃** | Handle 方法直接发射带完整 Reply 的 Done (行 910-916) |
| `SSEEventError` | `EventError` | 映射 Error.Message |

### 4.5 两条执行路径

**路径 A: Legacy Agent 内核**

```
HTTP handler → PostMessageStreamWithOpts()
  → legacyAgent.RunStreamWithOpts()
    → emit(agent.Event) → writeSSE()
```

**路径 B: Graph 内核 (KernelGraph)**

```
HTTP handler → PostMessageStreamWithOpts()
  → runGraph()
    → chatruntime.Handle()
      → eino graph callbacks
        → SSEHandler.emit(SSEEvent)
          → toCallbackEmitter() → chatruntime.Event
            → translateRuntimeEvent() → agent.Event
              → emit(agent.Event) → writeSSE()
```

两条路径最终都汇聚到 `writeSSE()`，SPA 看到的帧格式完全一致。

---

## 5. SSE 回调处理器

### 5.1 SSEHandler 结构

**文件**: [sse.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/sse.go)

`SSEHandler` 实现 `callbacks.Handler` + `callbacks.TimingChecker` 接口 (行 452-455)。

### 5.2 Needed 方法 — 回调时机门控 (行 209-233)

| 组件类型 | 订阅的 timing | 说明 |
|---------|-------------|------|
| `ComponentOfChatModel` | `OnStart`, `OnEnd`, `OnError`, `OnEndWithStreamOutput` | 全部四种 |
| `ComponentOfTool` | `OnStart`, `OnEnd`, `OnError` | 工具不产出流式输出 |
| 其他 (Graph/Workflow/Chain) | `OnEnd`, `OnError` | 仅观察终态 |

### 5.3 OnStart — 组件启动前 (行 236-271)

| 组件 | 行为 |
|------|------|
| ChatModel | `iterations` 原子加 1，发射 `SSEEventAssistantStart` |
| Tool | 提取 `ArgumentsInJSON`，记录 `toolStarts[key]`，发射 `SSEEventToolStart` (status="pending") |

### 5.4 OnEnd — 组件成功返回 (行 274-343)

| 组件 | 行为 |
|------|------|
| ChatModel | 提取 Content + ToolCalls 数量，`MessageID` 通过 `assistantIDRelay.load()` 获取，发射 `SSEEventAssistantEnd` |
| Tool | 查找 `toolStarts[key]` 计算耗时，发射 `SSEEventToolEnd` (status="success") |
| 其他 (Graph) | 发射 `SSEEventDone` |

### 5.5 OnError — 组件返回错误 (行 347-387)

| 组件 | 行为 |
|------|------|
| Tool | 发射 `SSEEventToolEnd`，status="error" 或 "timeout"（由 `isDeadlineErr` 判定） |
| 其他 | 发射 `SSEEventError` |

### 5.6 OnEndWithStreamOutput — 流式输出 (行 404-417)

```go
func (h *SSEHandler) OnEndWithStreamOutput(ctx, info, out) context.Context {
    if info.Component != components.ComponentOfChatModel {
        out.Close()   // 非 ChatModel 不处理
        return ctx
    }
    go h.drainStream(out)   // 启动独立协程消费流
    return ctx
}
```

### 5.7 drainStream — 逐 chunk 消费流式输出 (行 419-440)

```go
func (h *SSEHandler) drainStream(out *schema.StreamReader[callbacks.CallbackOutput]) {
    defer out.Close()
    for {
        chunk, err := out.Recv()
        if err != nil { return }
        mo := einomodel.ConvCallbackOutput(chunk)
        if mo == nil || mo.Message == nil { continue }
        if mo.Message.Content == "" { continue }  // 跳过空 chunk
        h.emit(SSEEvent{
            Type:  SSEEventAssistantDelta,
            Delta: &AssistantDelta{Content: mo.Message.Content},
        })
    }
}
```

- 在独立 goroutine 中运行，不阻塞 eino 图执行
- 每个非空 Content chunk 发射一个 `assistant_delta` 帧
- **当前实际效果**：由于底层 `clientChatModel.Stream` 是伪流式，`drainStream` 只会在整个 Generate 完成后收到一个 chunk

---

## 6. 回调链组装与 assistantIDRelay

### 6.1 NewDefaultHandlers 组装顺序

**文件**: [chain.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/chain.go) 行 88-118

```
AlertDraftGuard → Persistence → SSE → Audit → Metrics → Budget
```

| 顺序 | Handler | 职责 |
|------|---------|------|
| 1 | AlertDraftGuard | 阻止不安全的告警规则草稿 |
| 2 | Persistence | 写入 chat_messages / chat_tool_calls |
| 3 | SSE | 流式事件推送 |
| 4 | Audit | 审计日志 |
| 5 | Metrics | Prometheus 指标 |
| 6 | Budget | 预算门控 |

### 6.2 assistantIDRelay — 跨 Handler 通信

**文件**: [chain.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/chain.go) 行 26-45

```go
type assistantIDRelay struct {
    id atomic.Pointer[string]
}

func (r *assistantIDRelay) store(id string) { r.id.Store(&id) }
func (r *assistantIDRelay) load() string    { p := r.id.Load(); if p != nil { return *p }; return "" }
```

**SSE 必须排在 Persistence 之后的原因**：

1. `SSEHandler.OnEnd` 在发射 `assistant_end` 帧时需要 `MessageID`
2. 这个 ID 是 `PersistenceHandler.OnEnd` 写入 chat_messages 行后由数据库生成的
3. eino 框架在 `OnEnd` 时按 handler 注册顺序依次调用
4. Persistence 先注册 → 先执行 → `store(row.ID)` → SSE 后注册 → 后执行 → `load()` 拿到 ID

### 6.3 Deps 依赖注入结构

**文件**: [chain.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/chain.go) 行 47-73

```go
type Deps struct {
    AlertDraftGuard AlertDraftGuardDeps
    Persistence     PersistenceDeps
    SSE             SSEEmitter        // nil 时跳过 SSE handler
    Audit           AuditDeps
    Metrics         MetricsDeps
    BudgetChecker   llm.BudgetChecker
    BudgetUserID    uint64
}
```

每个 handler 对 nil 依赖做 "skip me" 处理，允许按请求选择性启用。

### 6.4 FinalizeBatches

**文件**: [chain.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/chain.go) 行 127-133

在 `runtime.go` 行 768-771 通过 defer 调用，确保"用户关闭浏览器中途"场景下不完整的 tool batch 被最终刷新。

---

## 7. 前端 SSE 消费与解析

### 7.1 streamMessage 函数

**文件**: [chat.ts](file:///d:/claude/ongrid/web/src/api/chat.ts) 行 252-317

```typescript
export async function streamMessage(
  sessionId: string | number,
  content: string,
  cbs: StreamCallbacks,
  opts: SendOptions = {},
  signal?: AbortSignal,
): Promise<void> {
  const url = `/api/v1/chat/sessions/${encodeURIComponent(String(sessionId))}/messages/stream`;
  const headers = {
    Accept: 'text/event-stream',
    'Content-Type': 'application/json',
    Authorization: `Bearer ${token}`,
  };
  const res = await fetch(url, { method: 'POST', headers, body: JSON.stringify(body), signal });

  // HTTP 非 2xx 错误处理
  if (!res.ok || !res.body) { /* ... throw ApiError */ }

  // ReadableStream 消费
  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buf = '';

  while (true) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });

    // SSE 帧以 \n\n 分隔
    let sep: number;
    while ((sep = buf.indexOf('\n\n')) >= 0) {
      const frame = buf.slice(0, sep);
      buf = buf.slice(sep + 2);
      dispatchFrame(frame, cbs);
    }
  }
  // 刷残余帧
  if (buf.trim()) dispatchFrame(buf, cbs);
}
```

关键设计：
- 使用 **POST** 方法（需要 JSON body）
- 不使用浏览器原生 `EventSource`（只支持 GET，不支持自定义 headers）
- `TextDecoder` 的 `stream: true` 确保跨 chunk UTF-8 多字节字符正确解码
- `signal` 直接传给 `fetch`，实现 AbortController 中断

### 7.2 帧缓冲机制

经典的"扫描-分割-保留"缓冲模式：

1. 每次 `reader.read()` 返回的 chunk 可能包含不完整数据
2. `buf` 累积所有未处理数据
3. 内层 `while` 查找 `\n\n` 分隔符，提取所有完整帧
4. 无 `\n\n` 则当前帧未完整，等待下一次 `reader.read()`
5. 流结束时 `buf.trim()` 非空则作为最后帧分发

### 7.3 dispatchFrame 帧解析器

**文件**: [chat.ts](file:///d:/claude/ongrid/web/src/api/chat.ts) 行 319-358

```typescript
function dispatchFrame(raw: string, cbs: StreamCallbacks) {
  let event = 'message';
  const dataLines: string[] = [];

  for (const line of raw.split('\n')) {
    if (!line || line.startsWith(':')) continue;      // 空行和注释行跳过
    if (line.startsWith('event:')) event = line.slice(6).trim();
    else if (line.startsWith('data:')) dataLines.push(line.slice(5).trim());
  }

  if (dataLines.length === 0) return;
  let payload: unknown;
  try { payload = JSON.parse(dataLines.join('\n')); }
  catch { return; }  // JSON 解析失败静默丢弃

  switch (event) {
    case 'assistant':         cbs.onAssistant?.(payload); break;
    case 'tool_start':        cbs.onToolStart?.(payload); break;
    case 'tool_end':          cbs.onToolEnd?.(payload); break;
    case 'approval_pending':  cbs.onApprovalPending?.(payload); break;
    case 'done':              cbs.onDone?.(payload); break;
    case 'error':             cbs.onError?.(new Error(payload.error || 'stream error')); break;
    default:                  break;  // 未知帧静默忽略
  }
}
```

### 7.4 StreamCallbacks 接口

**文件**: [chat.ts](file:///d:/claude/ongrid/web/src/api/chat.ts) 行 237-244

```typescript
export type StreamCallbacks = {
  onAssistant?: (e: AssistantStreamEvent) => void;
  onToolStart?: (e: ToolStreamEvent) => void;
  onToolEnd?: (e: ToolStreamEvent) => void;
  onApprovalPending?: (e: ApprovalPendingStreamEvent) => void;
  onDone?: (reply: PostMessageResponse) => void;
  onError?: (err: Error) => void;
};
```

所有回调可选，通过可选链 `cbs.onXxx?.()` 调用。

### 7.5 事件载荷类型

**AssistantStreamEvent** (行 200-206):
```typescript
{ iteration: number; message_id: string; content: string; created_at: string; pending_tool_calls: number; }
```

**ToolStreamEvent** (行 208-221):
```typescript
{ tool_call_id: string; name: string; device_id?: number; status: 'pending'|'success'|'error'|'timeout';
  started_at: string; ended_at?: string; duration_ms: number; error?: string;
  arguments?: unknown; arguments_raw?: string; result?: unknown; result_raw?: string; }
```

**ApprovalPendingStreamEvent** (行 229-235):
```typescript
{ approval_id: string; tool_call_id?: string; kind?: string; command?: string; credentials?: string[]; }
```

---

## 8. 前端事件处理与 UI 更新

**文件**: [ChatThread.tsx](file:///d:/claude/ongrid/web/src/pages/ChatThread.tsx) 行 246-381

### 8.1 onAssistant — 助手消息增量更新 (行 246-274)

- 空 content 的 tool-only 轮次被抑制（不显示空气泡）
- 同一 `message_id` 的多次事件**替换**而非追加（流式更新）
- fallback 到 `assistant-iter-{iteration}` 合成 ID

### 8.2 onToolStart — 工具卡片创建 (行 276-290)

- 追加 `kind: 'tool_card'` 的合成消息，ID 为 `tool-card-{tool_call_id}`
- 初始状态 `status: 'pending'`，显示旋转加载图标

### 8.3 onToolEnd — 工具卡片更新 (行 292-319)

- 通过 `toolCardId` 匹配到 `onToolStart` 创建的卡片，原地更新
- **保留** `onToolStart` 时的 `arguments`，合并 `result`
- `expectedTool` 机制用于 configDraft 确认流程

### 8.4 onApprovalPending — 审批卡片 (行 321-375)

- HLD-021：`cloud_bash`/`host_bash` 阻塞等待人工审批
- 审批帧将 `pending_approval` blob 写入已有 tool card 的 `result` 字段
- `MessageBubble` 检测 `result.status === 'pending_approval'`，渲染 `PendingApprovalCard`
- 页面刷新后，5 秒轮询通过 `listApprovals('pending')` 重建审批卡片

### 8.5 onDone (行 376-378)

- 只做一件事：`invalidateChatSessions()` 刷新侧边栏会话列表缓存
- 消息最终状态由 5 秒轮询兜底

### 8.6 onError (行 379-381)

- SSE error 帧被重新抛出为异常，由 `send()` 的 catch 块统一处理

### 8.7 5 秒轮询补偿

**文件**: [ChatThread.tsx](file:///d:/claude/ongrid/web/src/pages/ChatThread.tsx) 行 98-174

- 每 5 秒轮询 `getMessages` + `listApprovals`
- 页面不可见时跳过（`document.visibilityState`）
- SSE 流进行中时跳过（`submittingRef.current`）
- 指纹比对：`消息数量|最后消息ID|最后内容长度|pending approval IDs`，避免无效重渲染

---

## 9. SSE 连接生命周期

### 9.1 创建

1. **前端**: `ChatThread.send()` 创建 `AbortController`，调用 `streamMessage`
2. **后端**: `postMessageStream` 设置 SSE 头 + 心跳帧 + 构建 emit 闭包

### 9.2 维护

- 无周期性心跳，依赖业务事件本身保活
- Nginx `proxy_read_timeout 300s`（5 分钟无数据后断开）

### 9.3 终止

| 方式 | 触发 | 行为 |
|------|------|------|
| 正常完成 | 服务端发 `done`/`error` 帧 | `reader.read()` 返回 `done: true` |
| 客户端中断 | Esc 键 | `stopSession()` + `abort()` |
| 被动断连 | 浏览器关闭/刷新/网络中断 | `writeSSE` 吞错，turn 继续（HLD-021） |

### 9.4 context 双层设计

**文件**: [service.go](file:///d:/claude/ongrid/internal/manager/service/aiops/service.go) 行 344-360

```go
// 第一层：脱离 HTTP 生命周期 (HLD-021)
ctx = context.WithoutCancel(ctx)

// 第二层：显式可取消
ctx, cancel := context.WithCancel(ctx)
s.registerCancel(sess.ID, cancel)
defer s.unregisterCancel(sess.ID, cancel)
```

**`context.WithoutCancel`**：保留请求的 values（auth/tenant/emit）但切断取消传播，确保 `cloud_bash` 等待人工审批时浏览器刷新不会杀死 turn。

### 9.5 cancels map

**文件**: [service.go](file:///d:/claude/ongrid/internal/manager/service/aiops/service.go) 行 82-88

```go
cancelMu sync.Mutex
cancels  map[string]context.CancelFunc
```

- 以 sessionID 为 key，每个 session 最多一个 cancel 函数
- `registerCancel`：如果同一 session 有旧 turn 仍在运行，先 cancel 它
- `unregisterCancel`：仅当 `sameCancel(cur, cancel)` 为真才 delete，避免覆盖

### 9.6 StopSession 端点

**文件**: [http.go](file:///d:/claude/ongrid/internal/manager/server/aiops/http.go) 行 136, 643-663

```go
r.Post("/v1/chat/sessions/{id}/stop", h.stopSession)
```

幂等设计：无 turn 运行时返回 `{"stopped": false}`。

### 9.7 Esc 键双路中断

**文件**: [ChatThread.tsx](file:///d:/claude/ongrid/web/src/pages/ChatThread.tsx) 行 423-431

```typescript
void stopSession(sessionId).catch(() => {});  // 服务端停止
abortRef.current?.abort();                     // 客户端中断
```

1. `stopSession` → 服务端取消 turn 的 context → graph 软失败，partial state 持久化
2. `abort()` → 立即中断 fetch → UI 即时响应

---

## 10. 错误处理策略

### 10.1 后端写入层

| 场景 | 处理 |
|------|------|
| `w.Write` 失败 | `_, _` 吞没，客户端断连后无法做任何有用的事 |
| `json.Marshal` 失败 | payload 退化为 `{}` |
| HTTP 头已发送后服务层报错 | 发送 `error` SSE 帧（行 464-469） |
| agent 返回但未发 done 帧 | 发送 `summary` 帧兜底（行 474-476） |

### 10.2 回调链层

| 场景 | 处理 |
|------|------|
| Tool 超时 | `isDeadlineErr` 判定，status="timeout" |
| Tool 错误 | status="error"，携带 error 消息 |
| ChatModel/Graph 错误 | 发射 `SSEEventError`，携带 Message + Code |
| Persistence 写入失败 | 不阻断 graph，仅记录日志 + Prometheus 计数 |

### 10.3 前端消费层

| 场景 | 处理 |
|------|------|
| HTTP 非 2xx | 解析 JSON 错误体，抛出 `ApiError` |
| SSE error 帧 | 构造 `Error` 对象，通过 `onError` 回调 |
| ReadableStream 读取错误 | 冒泡到 `send()` 的 catch 块 |
| AbortError (Esc) | 静默返回 `false`，不显示错误消息 |
| 其他错误 | `setError(msg)` + 追加错误气泡 |
| JSON 解析失败 | 静默丢弃帧 |

---

## 11. Nginx 代理层 SSE 配置

**文件**: [nginx.conf](file:///d:/claude/ongrid/deploy/nginx/nginx.conf) 行 98-126

```nginx
location /api/ {
    proxy_pass http://ongrid_backend;
    proxy_http_version 1.1;

    # WebSocket upgrade bridge
    proxy_set_header Upgrade           $http_upgrade;
    proxy_set_header Connection        $connection_upgrade;

    proxy_connect_timeout 10s;
    proxy_send_timeout    300s;   # 5 分钟发送超时
    proxy_read_timeout    300s;   # 5 分钟读取超时
    proxy_buffering       off;    # SSE 核心配置：关闭响应缓冲
    proxy_cache           off;    # 关闭缓存
    chunked_transfer_encoding on; # 允许分块传输
}
```

| 配置项 | 值 | 说明 |
|--------|------|------|
| `proxy_buffering` | off | **SSE 核心**：关闭响应缓冲，帧立即到达客户端 |
| `proxy_cache` | off | 禁止缓存 |
| `chunked_transfer_encoding` | on | 允许分块传输 |
| `proxy_read_timeout` | 300s | 5 分钟读超时，覆盖 LLM 长推理 |
| `proxy_send_timeout` | 300s | 5 分钟发送超时 |
| `proxy_http_version` | 1.1 | 支持 keepalive + chunked |
| `keepalive` | 16 | upstream 连接池保持 16 个长连接 |

**双重保障**：后端设置 `X-Accel-Buffering: no` (http.go 行 453) + Nginx `proxy_buffering off`。

### WebSocket upgrade 桥接 (行 20-27)

```nginx
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      "";
}
```

SSE 和 WebSocket 通过同一 `/api/` location 共存。

---

## 12. WebSocket vs SSE 分工

| 维度 | Chat SSE | WebSSH WebSocket |
|------|----------|-----------------|
| 端点 | `POST /v1/chat/sessions/{id}/messages/stream` | `GET /v1/devices/{id}/shell` |
| 协议 | HTTP POST + text/event-stream | WebSocket (wss://) |
| 前端 API | fetch + ReadableStream | 原生 WebSocket API |
| 认证 | `Authorization: Bearer` header | `?token=<jwt>` query string |
| 子协议 | 无 | `ongrid.shell.v1` |
| 数据格式 | 文本 SSE 帧 (event + data JSON) | 二进制帧 (stdout) + 文本帧 (控制信令) |
| 方向 | 单向 (服务端→客户端) | 双向 |
| 中断 | AbortController + stopSession | `sendControl(close)` + `ws.close()` |
| 重连 | 无（5s 轮询补偿） | 用户手动点击"重连" |
| 生命周期 | 请求-响应模式，turn 结束即关闭 | 长连接，15 分钟 idle watchdog |

---

## 13. IM Bridge SSE 适配

**文件**: [sender.go](file:///d:/claude/ongrid/internal/manager/biz/imbridge/sender.go) 行 28-99

IM Bridge (飞书/钉钉/Slack) 复用同一个 `agent.Emit` 回调，但通过 `streamEditor` 将 SSE 事件转为 IM 平台的"编辑消息"操作：

| 事件 | IM Bridge 处理 |
|------|---------------|
| `EventAssistant` | 累积文本，节流 (800ms/200字符) 调用 IM EditText |
| `EventDone` | 强制刷新最后一段 |
| `EventToolStart`/`EventToolEnd`/`EventTaskNotification` | **丢弃** (IM 中太嘈杂) |

---

## 14. MCP Client SSE 读端

**文件**: [client.go](file:///d:/claude/ongrid/internal/pkg/mcpclient/client.go) 行 174-260

MCP Client 是 SSE **消费者**（非写端），向 MCP 服务器发送 `Accept: application/json, text/event-stream`，通过 `firstSSEData()` 解析 SSE 响应流，提取第一个包含 `"result"` 或 `"error"` 的 `data:` 行。

---

## 15. 架构红线

| # | 红线 | 说明 |
|---|------|------|
| 1 | **SSE 必须关闭代理缓冲** | `proxy_buffering off` + `X-Accel-Buffering: no` 双重保障 |
| 2 | **每帧必须立即 Flush** | `writeSSE` 中 `f.Flush()` 确保增量渲染 |
| 3 | **写入错误必须吞没** | 客户端断连后无法做任何有用的事 |
| 4 | **Persistence 必须在 SSE 之前** | `assistantIDRelay` 跨 handler 通信依赖执行顺序 |
| 5 | **请求必须脱离 HTTP 生命周期** | `context.WithoutCancel` (HLD-021) |
| 6 | **每 session 最多一个活跃 turn** | `cancels` map 以 sessionID 为 key |
| 7 | **SSE 不支持 GET** | 使用 POST + fetch，不使用 EventSource |
| 8 | **无周期性心跳** | 仅连接建立时一次 `: ok\n\n`，依赖业务事件保活 |
| 9 | **assistant_delta 当前丢弃** | 待 SPA 支持后启用，不可提前开放 |
| 10 | **IM Bridge 丢弃 tool 事件** | 避免在 IM 中产生嘈杂的工具调用通知 |

---

## 16. 关键文件索引

### 后端 — HTTP 层

| 文件 | 行号 | 内容 |
|------|------|------|
| [http.go](file:///d:/claude/ongrid/internal/manager/server/aiops/http.go) | 135 | SSE 路由注册 |
| [http.go](file:///d:/claude/ongrid/internal/manager/server/aiops/http.go) | 414-477 | `postMessageStream` SSE handler |
| [http.go](file:///d:/claude/ongrid/internal/manager/server/aiops/http.go) | 438-457 | SSE 握手 (headers + 心跳) |
| [http.go](file:///d:/claude/ongrid/internal/manager/server/aiops/http.go) | 459-461 | emit 闭包构建 |
| [http.go](file:///d:/claude/ongrid/internal/manager/server/aiops/http.go) | 478-495 | `eventName()` 事件名映射 |
| [http.go](file:///d:/claude/ongrid/internal/manager/server/aiops/http.go) | 497-595 | `eventPayload()` 各事件 payload 构建 |
| [http.go](file:///d:/claude/ongrid/internal/manager/server/aiops/http.go) | 598-612 | `writeSSE()` 帧序列化 + Flush |
| [http.go](file:///d:/claude/ongrid/internal/manager/server/aiops/http.go) | 643-663 | `stopSession` handler |

### 后端 — Service 层

| 文件 | 行号 | 内容 |
|------|------|------|
| [service.go](file:///d:/claude/ongrid/internal/manager/service/aiops/service.go) | 82-88 | `cancels` map 定义 |
| [service.go](file:///d:/claude/ongrid/internal/manager/service/aiops/service.go) | 344-360 | `context.WithoutCancel` + `context.WithCancel` |
| [service.go](file:///d:/claude/ongrid/internal/manager/service/aiops/service.go) | 382-400 | `registerCancel` / `unregisterCancel` |
| [service.go](file:///d:/claude/ongrid/internal/manager/service/aiops/service.go) | 402-421 | `StopSession` 实现 |
| [service.go](file:///d:/claude/ongrid/internal/manager/service/aiops/service.go) | 423-427 | `sameCancel` 指针比较 |
| [service.go](file:///d:/claude/ongrid/internal/manager/service/aiops/service.go) | 434-519 | `runGraph` + `translateRuntimeEvent` |

### 后端 — Chatruntime 层

| 文件 | 行号 | 内容 |
|------|------|------|
| [runtime.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go) | 85-97 | EventType 枚举 |
| [runtime.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go) | 132-146 | Event 结构体 + Emit 类型 |
| [runtime.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go) | 468-851 | `Handle` 完整编排 |
| [runtime.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go) | 859-925 | `toCallbackEmitter` 事件翻译 |
| [worker.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/worker.go) | 185-209 | TaskNotification + ApprovalPending 定义 |
| [worker.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/worker.go) | 211-238 | `withEmit` / `EmitFromContext` |

### 后端 — Graph Callback 层

| 文件 | 行号 | 内容 |
|------|------|------|
| [sse.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/sse.go) | 22-50 | SSEEventType 枚举 (8 种) |
| [sse.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/sse.go) | 57-139 | SSEEvent + 所有 Payload 结构体 |
| [sse.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/sse.go) | 141-147 | SSEEmitter 类型定义 |
| [sse.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/sse.go) | 209-233 | `Needed` 回调时机门控 |
| [sse.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/sse.go) | 236-271 | `OnStart` |
| [sse.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/sse.go) | 274-343 | `OnEnd` |
| [sse.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/sse.go) | 347-387 | `OnError` |
| [sse.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/sse.go) | 404-417 | `OnEndWithStreamOutput` |
| [sse.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/sse.go) | 419-440 | `drainStream` |
| [chain.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/chain.go) | 26-45 | `assistantIDRelay` |
| [chain.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/chain.go) | 47-73 | `Deps` 依赖注入 |
| [chain.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/chain.go) | 88-118 | `NewDefaultHandlers` 组装 |

### 后端 — Legacy Agent 层

| 文件 | 行号 | 内容 |
|------|------|------|
| [agent.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/agent/agent.go) | 62-139 | EventType 枚举 + Event 结构体 |

### 后端 — IM Bridge

| 文件 | 行号 | 内容 |
|------|------|------|
| [sender.go](file:///d:/claude/ongrid/internal/manager/biz/imbridge/sender.go) | 28-99 | `streamEditor` (SSE→IM 适配) |
| [bridge.go](file:///d:/claude/ongrid/internal/manager/biz/imbridge/bridge.go) | 224-235 | IM bridge emit 闭包 |

### 后端 — MCP Client (SSE 读端)

| 文件 | 行号 | 内容 |
|------|------|------|
| [client.go](file:///d:/claude/ongrid/internal/pkg/mcpclient/client.go) | 174-260 | MCP SSE 客户端 |

### 后端 — 独立工具

| 文件 | 行号 | 内容 |
|------|------|------|
| [main.go](file:///d:/claude/ongrid/cmd/ollama/main.go) | 55-151 | 开发用 Ollama SSE 代理 |

### 前端

| 文件 | 行号 | 内容 |
|------|------|------|
| [chat.ts](file:///d:/claude/ongrid/web/src/api/chat.ts) | 97-106 | `stopSession` API |
| [chat.ts](file:///d:/claude/ongrid/web/src/api/chat.ts) | 200-244 | SSE 类型定义 + StreamCallbacks |
| [chat.ts](file:///d:/claude/ongrid/web/src/api/chat.ts) | 252-317 | `streamMessage` SSE 消费 |
| [chat.ts](file:///d:/claude/ongrid/web/src/api/chat.ts) | 319-358 | `dispatchFrame` 帧解析 |
| [ChatThread.tsx](file:///d:/claude/ongrid/web/src/pages/ChatThread.tsx) | 98-174 | 5 秒轮询 + 指纹去重 |
| [ChatThread.tsx](file:///d:/claude/ongrid/web/src/pages/ChatThread.tsx) | 225-226 | AbortController 创建 |
| [ChatThread.tsx](file:///d:/claude/ongrid/web/src/pages/ChatThread.tsx) | 246-381 | SSE 事件处理 callbacks |
| [ChatThread.tsx](file:///d:/claude/ongrid/web/src/pages/ChatThread.tsx) | 393-415 | 错误处理 catch 块 |
| [ChatThread.tsx](file:///d:/claude/ongrid/web/src/pages/ChatThread.tsx) | 423-431 | Esc 键双路中断 |
| [MessageBubble.tsx](file:///d:/claude/ongrid/web/src/components/MessageBubble.tsx) | 36-52 | 消息类型路由 |
| [MessageBubble.tsx](file:///d:/claude/ongrid/web/src/components/MessageBubble.tsx) | 141-244 | `ToolCallSummaryBlock` |
| [MessageBubble.tsx](file:///d:/claude/ongrid/web/src/components/MessageBubble.tsx) | 485-633 | `PendingApprovalCard` |

### 部署

| 文件 | 行号 | 内容 |
|------|------|------|
| [nginx.conf](file:///d:/claude/ongrid/deploy/nginx/nginx.conf) | 20-27 | WebSocket upgrade map |
| [nginx.conf](file:///d:/claude/ongrid/deploy/nginx/nginx.conf) | 98-126 | `/api/` location SSE/WS 配置 |
| [nginx.conf](file:///d:/claude/ongrid/deploy/nginx/nginx.conf) | 33 | upstream keepalive 16 |
