# OnGrid Ollama Stream Error 深度分析与解决方案

> 本文档分析 OnGrid 系统 LLM stream chat 的完整工作机制、前后端调用过程、Ollama 会话出现 stream error 的根因，并给出解决办法。

---

## 目录

1. [LLM Stream Chat 架构总览](#1-llm-stream-chat-架构总览)
2. [后端 Stream 工作机制](#2-后端-stream-工作机制)
3. [前端 SSE 调用过程](#3-前端-sse-调用过程)
4. [完整数据流图](#4-完整数据流图)
5. [Ollama Stream Error 根因分析](#5-ollama-stream-error-根因分析)
6. [解决办法](#6-解决办法)
7. [架构红线](#7-架构红线)
8. [关键文件索引](#8-关键文件索引)

---

## 1. LLM Stream Chat 架构总览

OnGrid 的 LLM 流式聊天采用分层架构，数据流从 HTTP 请求到前端 SSE 经历以下层次：

```
HTTP Request → Handler(SSE) → Service → ChatRuntime → Graph(ReAct) → CallbackChain → LLM Client → OpenAI-compatible API → SSE Response
```

### 核心设计决策

| 决策 | 说明 |
|------|------|
| **当前无真正的 token-by-token 流式** | `clientChatModel.Stream()` 是伪流式——将完整响应包装成单 chunk StreamReader |
| **流式体验通过 eino Graph 回调链实现** | SSEHandler 监听 `OnEndWithStreamOutput` 回调，逐 chunk 发送 `assistant_delta` 帧 |
| **所有 provider 均走 OpenAI 兼容协议** | 包括 Ollama、Anthropic、Zhipu、Gemini、DeepSeek、Kimi |
| **Ollama 无专用适配器** | 通过 Custom Provider + URL 自动修正 `/v1` 支持 |
| **请求脱离 HTTP 生命周期** | `context.WithoutCancel` 确保长时间操作不受浏览器断连影响 |

---

## 2. 后端 Stream 工作机制

### 2.1 HTTP Handler 层 — SSE 响应入口

**文件**: [http.go](file:///d:/claude/ongrid/internal/manager/server/aiops/http.go)

**路由注册** (行 135):
```go
r.Post("/v1/chat/sessions/{id}/messages/stream", h.postMessageStream)
```

**`postMessageStream` 方法** (行 414-477):

1. **SSE 握手** (行 451-457):
   ```go
   w.Header().Set("Content-Type", "text/event-stream")
   w.Header().Set("Cache-Control", "no-cache")
   w.Header().Set("X-Accel-Buffering", "no") // 禁用 nginx 缓冲
   w.WriteHeader(http.StatusOK)
   _, _ = w.Write([]byte(": ok\n\n"))  // 心跳帧
   flusher.Flush()
   ```

2. **Emit 闭包** (行 459-461): 每个 agent.Event 通过 `writeSSE` 序列化并立即 Flush

3. **调用服务层** (行 463):
   ```go
   reply, err := h.svc.PostMessageStreamWithOpts(r.Context(), caller, id, req.Content, emit, opts)
   ```

4. **错误后补** (行 464-469): HTTP 头已发送后，错误通过 SSE `error` 帧传递

**`writeSSE` 函数** (行 601-612):
```go
func writeSSE(w http.ResponseWriter, f http.Flusher, name string, payload any) {
    body, _ := json.Marshal(payload)
    _, _ = w.Write([]byte("event: "))
    _, _ = w.Write([]byte(name))
    _, _ = w.Write([]byte("\ndata: "))
    _, _ = w.Write(body)
    _, _ = w.Write([]byte("\n\n"))
    f.Flush()  // 每帧立即 Flush
}
```

**SSE 帧类型** (行 479-496):

| 帧名 | 触发时机 |
|------|---------|
| `assistant` | 一次 assistant turn 持久化后 |
| `tool_start` | tool_call 行写入 pending 状态后 |
| `tool_end` | 工具执行完成 |
| `done` | 终端成功 |
| `approval_pending` | 阻塞工具等待审批 |
| `error` | 终端失败 |

### 2.2 Service 层 — 内核路由

**文件**: [service.go](file:///d:/claude/ongrid/internal/manager/service/aiops/service.go)

**`runWithKernel`** (行 334-376):

1. **HLD-021: 请求脱离 HTTP 生命周期** (行 353):
   ```go
   ctx = context.WithoutCancel(ctx)
   ```
   浏览器刷新/SSE 断连不会取消正在运行的 turn。

2. **显式可取消** (行 358-360):
   ```go
   ctx, cancel := context.WithCancel(ctx)
   s.registerCancel(sess.ID, cancel)
   ```
   用户按 Esc 调用 `StopSession` 可中断。

3. **内核选择** (行 363-365): 选择 graph 内核或 legacy 内核。

### 2.3 ChatRuntime 层 — 编排核心

**文件**: [runtime.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go)

**`Handle` 方法** (行 468-851) 完整流程：

1. Emit 注入 ctx → 所有权检查 → @-mention 渲染 → 用户消息持久化
2. 技能解析 + 系统提示词组装 → 构建 eino 历史
3. 构建 ReAct Graph (行 680): `graph.BuildReActGraph(rt.cfg.ChatModel, sessionToolBag, graphCfg)`
4. 组装回调链 (行 689-701): `callbacks.NewDefaultHandlers(deps)`
5. Graph Invoke (行 772-781): `g.Invoke(ctx, &graph.Input{...}, invokeOpts...)`
6. 错误软降级 (行 782-816): Graph 错误时发送道歉消息 + Done 帧

**`toCallbackEmitter`** (行 859-925) — 事件翻译与过滤：

| SSE 事件 | 处理方式 |
|---------|---------|
| `SSEEventAssistantEnd` | → `EventAssistant`（完整 turn 内容） |
| `SSEEventAssistantStart` | 丢弃（前端不支持） |
| `SSEEventAssistantDelta` | **丢弃**（行 880-883，待 SPA 支持后启用） |
| `SSEEventToolStart` | → `EventToolStart` |
| `SSEEventToolEnd` | → `EventToolEnd` |

> **关键发现**: `assistant_delta` 帧当前被丢弃，前端只能收到完整的 `assistant` 帧（一个 turn 结束后一次性推送）。这意味着即使底层实现了真正的 token-by-token 流式，前端也暂时无法逐字显示。

### 2.4 Graph 层 — ReAct 循环

**文件**: [react.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/react.go)

拓扑结构 (行 78-188):
```
START → MessageAssembler → ReActSubgraph → OutputProjector → END
```

内部 ReAct 子图:
```
ChatModel ↔ Branch(tool_calls?) ↔ ToolsNode
  yes → 回到 ChatModel
  no → END
```

### 2.5 Callback 链 — 横切关注点

**文件**: [chain.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/chain.go)

**`NewDefaultHandlers`** (行 88-118) 按序组装：

1. **AlertDraftGuardHandler** — 阻止不安全的告警规则草稿
2. **PersistenceHandler** — 写入 chat_messages / chat_tool_calls
3. **SSEHandler** — 流式推送
4. **AuditHandler** — 审计日志
5. **MetricsHandler** — Prometheus 指标
6. **BudgetCallbackHandler** — 预算门控

### 2.6 SSE Handler — 流式事件发射

**文件**: [sse.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/sse.go)

**`OnEndWithStreamOutput`** (行 404-417): 启动 goroutine 消费流式输出
```go
func (h *SSEHandler) OnEndWithStreamOutput(ctx context.Context, info *callbacks.RunInfo, out *schema.StreamReader[callbacks.CallbackOutput]) context.Context {
    go h.drainStream(out)
    return ctx
}
```

**`drainStream`** (行 419-440): 逐 chunk 读取并发送 `assistant_delta`

> 由于底层 `Stream()` 是伪流式，`drainStream` 实际只会在整个 Generate 完成后收到一个 chunk。

### 2.7 LLM Client 层 — 非流式 + 适配器

**文件**: [client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go)

**核心发现**: `Client.Chat()` (行 355-474) 调用的是**非流式** `sdk.CreateChatCompletion`:
```go
sdkResp, err := sdk.CreateChatCompletion(callCtx, sdkReq)
```

**超时** (行 44): `defaultTimeout = 120 * time.Second`

**重试** (行 408-414): 仅对 reasoning model 的采样参数错误重试一次，工具调用不幂等不重试。

**`clientChatModel.Stream()`** — 伪流式 (行 370-378):
```go
// Stream wraps Generate behind an array-backed StreamReader. PR-1 is
// scaffolding only — real token-by-token streaming lands in a later PR.
func (c *clientChatModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
    msg, err := c.Generate(ctx, input, opts...)
    if err != nil { return nil, err }
    return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}
```

### 2.8 Ollama URL 自动修正

**文件**: [client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go) 行 298-331

`normalizeOpenAIBaseURL` — 当 URL path 为空时自动补 `/v1`：

> Operators pointing the Custom (OpenAI-compatible) provider at a local Ollama / LM Studio / vLLM box routinely paste just the bare address — e.g. "http://192.168.8.5:11434". The SDK then POSTs to ".../chat/completions", which Ollama serves nothing on (its OpenAI-compatible route is "/v1/chat/completions"), so the request 404s and the chat surfaces a "stream error".

### 2.9 错误分类器

**文件**: [runtime.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go) 行 1699-1778

`buildGraphErrorApology` 将 graph 错误分类为用户友好消息：

| 顺序 | 匹配关键字 | 用户消息 | Ollama 场景 |
|------|-----------|---------|------------|
| 3 | `context canceled` / `context deadline` | "本次请求超时或被取消" | 冷加载超 120s |
| 6 | `429` / `rate limit` / `quota` | "LLM provider 当前不可用" | OOM/过载 |
| 7 | `llm: chat completion` / `openai api` / `api error` | "LLM provider 报错（可能是 API key / 模型名 / 网络）" | 非标准错误 |
| 8 | default | 原始错误前 200 字符 | 未知错误 |

---

## 3. 前端 SSE 调用过程

### 3.1 流式请求发起

**文件**: [chat.ts](file:///d:/claude/ongrid/web/src/api/chat.ts) 行 252-317

前端使用原生 `fetch` + `ReadableStream` 手动消费 SSE 流（**不是** EventSource）：

```typescript
const url = `/api/v1/chat/sessions/${sessionId}/messages/stream`;
const res = await fetch(url, {
  method: 'POST',
  headers: {
    Accept: 'text/event-stream',
    'Content-Type': 'application/json',
    Authorization: `Bearer ${token}`,
  },
  body: JSON.stringify(body),
  signal,  // AbortController 取消
});
```

请求体包含：`content`、`provider`、`model`、`mentions`、`web_search_enabled`、`locale`。

### 3.2 SSE 帧解析

**文件**: [chat.ts](file:///d:/claude/ongrid/web/src/api/chat.ts) 行 298-317

```typescript
const reader = res.body.getReader();
const decoder = new TextDecoder();
let buf = '';

while (true) {
  const { value, done } = await reader.read();
  if (done) break;
  buf += decoder.decode(value, { stream: true });
  // SSE 帧以空行 (\n\n) 分隔
  let sep: number;
  while ((sep = buf.indexOf('\n\n')) >= 0) {
    const frame = buf.slice(0, sep);
    buf = buf.slice(sep + 2);
    dispatchFrame(frame, cbs);
  }
}
if (buf.trim()) dispatchFrame(buf, cbs);  // 刷残余帧
```

### 3.3 帧分发器

**文件**: [chat.ts](file:///d:/claude/ongrid/web/src/api/chat.ts) 行 319-358

`dispatchFrame` 手动解析 SSE 文本格式，支持 6 种事件类型：

| 事件类型 | 回调 | 用途 |
|---------|------|------|
| `assistant` | `onAssistant` | 助手完整 turn（含 `message_id`、`content`、`iteration`） |
| `tool_start` | `onToolStart` | 工具调用开始 |
| `tool_end` | `onToolEnd` | 工具调用结束 |
| `approval_pending` | `onApprovalPending` | 人工审批等待 |
| `done` | `onDone` | 整个 turn 完成 |
| `error` | `onError` | 服务端错误帧 |

### 3.4 流式错误处理

**文件**: [ChatThread.tsx](file:///d:/claude/ongrid/web/src/pages/ChatThread.tsx)

**传输层错误** (chat.ts 行 281-296): HTTP 非 2xx → 解析 JSON 错误体 → 抛出 `ApiError`

**SSE 错误帧** (chat.ts 行 350-354): `event: error` → 构造 Error → `onError` 回调

**消费侧** (ChatThread.tsx 行 379-415):
```typescript
catch (err) {
  // AbortError（Esc 停止）视为正常中断
  if (ac.signal.aborted || (err as Error).name === 'AbortError') {
    return false;
  }
  // 其他错误：设置 error 状态 + 追加错误提示消息
  setError(msg);
  setMessages((prev) => [...prev, { /* 错误气泡 */ }]);
}
```

**无自动重连机制**: 流断开后，已接收的部分消息保留在状态中，5 秒轮询会在下次可见时重新拉取完整历史。

### 3.5 Esc 停止机制

**文件**: [ChatThread.tsx](file:///d:/claude/ongrid/web/src/pages/ChatThread.tsx) 行 423-431

双管齐下：
1. **服务端**: `POST /chat/sessions/{id}/stop`（turn 已脱离请求上下文）
2. **客户端**: `abortRef.current?.abort()`（立即中断 fetch）

### 3.6 Ollama 前端处理

**文件**: [LLM.tsx](file:///d:/claude/ongrid/web/src/pages/settings/LLM.tsx) 行 139-153

Ollama 作为 "Custom (OpenAI-compatible)" 提供商，**没有**前端专属逻辑。提示用户：
- 无需鉴权的本地服务随便填占位 key
- Base URL 示例: `http://localhost:11434/v1`
- 模型名示例: `llama3.1 / qwen2.5-coder / ...`

---

## 4. 完整数据流图

```
前端 POST /v1/chat/sessions/{id}/messages/stream
  │ body: { content, provider, model, mentions, web_search_enabled, locale }
  ▼
http.go::postMessageStream (行 414)
  │ 设置 SSE 头 + 心跳帧 ": ok\n\n"
  │ 构建 emit 闭包 → writeSSE
  ▼
service.go::PostMessageStreamWithOpts (行 326)
  │
  ▼
service.go::runWithKernel (行 334)
  │ context.WithoutCancel (HLD-021，脱离 HTTP 生命周期)
  │ context.WithCancel (显式可停止)
  │ 选择 kernel: graph vs legacy
  ▼
service.go::runGraph (行 434)
  │ 翻译 agent.Emit → chatruntime.Emit
  │ 构建 chatruntime.Request
  ▼
runtime.go::Handle (行 468)
  │ 所有权检查 → @-mention 渲染 → 用户消息持久化
  │ 技能解析 + 系统提示词 → 构建 eino 历史
  │ 构建 ReAct Graph → 组装回调链
  ▼
react.go::BuildReActGraph → g.Invoke (行 772)
  │ MessageAssembler → ReActSubgraph → OutputProjector
  │ 内部: ChatModel ↔ Branch(tool_calls?) ↔ ToolsNode
  ▼
eino ReAct 循环:
  │ ChatModel.Generate/Stream
  │   │
  │   ▼
  │ RoutingChatModel.Stream (eino_routing.go 行 200)
  │   │ pick inner ChatModel by WithProvider
  │   ▼
  │ clientChatModel.Stream (eino_routing.go 行 372)  ← 伪流式！
  │   │ 调用 Generate (非流式)
  │   ▼
  │ clientChatModel.Generate (eino_routing.go 行 357)
  │   │ buildChatReq → c.inner.Chat(ctx, req)
  │   ▼
  │ openaiClient.Chat (client.go 行 355)
  │   │ effectiveCreds (解析凭据, 60s TTL)
  │   │ budget.Check (预算门控)
  │   │ toOpenAIReq (构建请求)
  │   │ context.WithTimeout (120s 默认)
  │   │ sdk.CreateChatCompletion (非流式 HTTP)
  │   │   │
  │   │   ▼
  │   │   Ollama: /v1/chat/completions (OpenAI 兼容)
  │   │   │  ← 冷加载可能 30-90s，超过 120s 则超时
  │   │   │  ← 模型未 pull → 404
  │   │   │  ← OOM → 500
  │   │
  │   │ reasoning model 重试 (isSamplingParamError)
  │   │ budget.Record (记录用量)
  │   ▼
  │ 返回 *schema.Message (包装成单 chunk StreamReader)
  │
  │ eino 回调触发:
  │   OnStart(ChatModel) → SSEHandler: assistant_start (丢弃)
  │   OnEndWithStreamOutput → SSEHandler: assistant_delta (丢弃，待 SPA 支持)
  │   OnEnd(ChatModel) → PersistenceHandler: 写入 assistant 行
  │                      SSEHandler: assistant_end → "assistant" 帧
  │   OnStart(Tool) → PersistenceHandler: 写入 tool_call 行
  │                   SSEHandler: tool_start 帧
  │   OnEnd(Tool) → PersistenceHandler: 更新 tool_call
  │                 SSEHandler: tool_end 帧
  │   OnEnd(Graph) → SSEHandler: done 帧
  ▼
runtime.go::Handle 返回 Reply
  │ emit(EventDone)
  ▼
前端接收 SSE 帧:
  event: assistant    ← 完整 assistant turn（一次性推送）
  event: tool_start   ← 工具开始
  event: tool_end     ← 工具完成
  event: done         ← 终端成功
  event: error        ← 终端失败
```

---

## 5. Ollama Stream Error 根因分析

### 5.1 根因总览

| # | 根因 | 触发条件 | 错误表现 | 严重度 |
|---|------|---------|---------|--------|
| 1 | **模型冷加载超时** | 首次推理需从磁盘加载到 GPU/CPU，大模型 30-90s | 探测 20s 超时 / 会话 120s 超时 | **高** |
| 2 | **Base URL 缺少 /v1** | 用户填裸地址 `http://host:11434` | 404 → "stream error" | **高** |
| 3 | **模型未 pull** | 本地不存在该模型 | 404 → "LLM provider 报错" | **中** |
| 4 | **docker-compose 网络缺陷** | `ongrid-ollama` 未接入 `ongrid_net` | DNS 解析失败 → 连接拒绝 | **高** |
| 5 | **OOM / GPU 内存不足** | 模型大小超过可用显存 | 500 → "LLM provider 报错" | **中** |
| 6 | **探测与生产超时不一致** | 探测 20s vs 生产 120s | 探测失败但会话可能成功，或两者都失败 | **中** |
| 7 | **Resolver 缓存延迟** | 改完配置后 60s TTL 内仍用旧凭据 | 旧配置导致连接失败 | **低** |
| 8 | **当前无真正流式** | `clientChatModel.Stream` 是伪流式 | Ollama 长推理期间前端无任何输出，用户以为卡死 | **中** |

### 5.2 根因详解

#### 根因 1: 模型冷加载超时（最常见）

Ollama 默认行为：
- 模型首次推理时需从磁盘加载到 GPU/CPU 内存
- 大模型（qwen2.5:14b、llama3:8b）冷加载可能 **30-90s**
- 加载完成后的推理通常 < 5s

**探测路径** (20s 超时):
- 文件: [llm_probe.go](file:///d:/claude/ongrid/internal/manager/biz/setting/llm_probe.go) 行 36-37
- `defaultLLMProbeTimeout = 20 * time.Second`
- 冷加载期间连接已建立但无响应 → 被判定为 `timeout`

**会话路径** (120s 超时):
- 文件: [client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go) 行 44
- `defaultTimeout = 120 * time.Second`
- 大模型冷加载 < 120s 时会话可成功，但前端长时间无输出

**用户看到**:
- 探测失败: "连接或模型响应超时 · provider did not respond before the probe deadline"
- 会话超时: "本次请求超时或被取消。一般是上游 LLM / 设备响应慢。"

#### 根因 2: Base URL 缺少 /v1

**文件**: [client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go) 行 298-331

Ollama 的 OpenAI 兼容端点是 `/v1/chat/completions`，但用户经常只填裸地址 `http://192.168.8.5:11434`。

**已有自动修正**: `normalizeOpenAIBaseURL` 会在 path 为空时自动补 `/v1`。但如果用户填了 `http://host:11434/api`（有 path 但不对），则不会被修正，仍然 404。

**用户看到**: "LLM provider 报错（可能是 API key / 模型名 / 网络）"

#### 根因 3: 模型未 pull

Ollama 返回非 OpenAI 兼容错误：
```json
{"error":"model 'xxx' not found, try pulling it first"}
```
被 openai SDK 包装后命中 `buildGraphErrorApology` 第 7 类 → "LLM provider 报错"

#### 根因 4: docker-compose 网络缺陷

**文件**: [docker-compose.yml](file:///d:/claude/ongrid/deploy/docker-compose.yml) 行 365-382

`ongrid-ollama` 服务**缺少 `networks: - ongrid_net`**，导致 manager 容器无法通过服务名 DNS 解析到 Ollama。

对比 `ongrid-grafana`（行 362-363）正确声明了 `networks: - ongrid_net`。

#### 根因 5: OOM / GPU 内存不足

Ollama 返回：
```json
{"error":"cuda out of memory"}
```
→ 500 → "LLM provider 报错"

#### 根因 6: 探测与生产超时不一致

| 路径 | 超时 | 文件:行 |
|------|------|---------|
| 探测 | 20s | llm_probe.go:48 |
| 生产 | 120s | client.go:44 |
| Resolver 缓存 | 60s TTL | client.go:185 |

Ollama 冷加载大模型经常 30-90s，**探测几乎必然超时**，但会话可能成功（如果冷加载 < 120s）。这导致"探测失败但实际可用"的矛盾状态。

#### 根因 7: Resolver 缓存延迟

admin 改完 provider 配置后，会话路径最长 60s 内仍用旧凭据。如果旧配置指向错误的 Ollama 地址/模型，60s 内的会话请求都会失败。

#### 根因 8: 当前无真正流式 — 用户体验问题

**文件**: [eino_routing.go](file:///d:/claude/ongrid/internal/pkg/llm/eino_routing.go) 行 370-378

`clientChatModel.Stream` 是伪流式——调用 `Generate` 后包装成单 chunk StreamReader。注释明确标注 "PR-1 is scaffolding only — real token-by-token streaming lands in a later PR"。

**后果**:
- Ollama 推理期间（可能 10-60s），前端**完全无输出**
- 用户看到空白等待，以为系统卡死
- `assistant_delta` 帧在 `toCallbackEmitter` 中被丢弃（行 880-883），即使底层实现流式也无法逐字显示
- 只有整个 Generate 完成后才发一个 `assistant` 帧

### 5.3 错误传播链

```
Ollama 返回错误 (404/500/timeout)
  → go-openai SDK 包装为 *openai.APIError
    → openaiClient.Chat 返回 error
      → clientChatModel.Generate 返回 error
        → RoutingChatModel.Generate 返回 error
          → eino ReAct Graph 返回 error
            → runtime.go::Handle 捕获 error
              → buildGraphErrorApology 分类翻译
                → emit(EventError) → writeSSE "error" 帧
                  → 前端 dispatchFrame → onError 回调
                    → ChatThread catch → setError + 错误气泡
```

---

## 6. 解决办法

### 6.1 立即可做（无需改代码）

#### 方案 A: 预热 Ollama 模型（推荐）

在 Ollama 启动后、用户使用前，手动预热模型：

```bash
# 预热并保持模型在内存中 30 分钟
ollama run qwen2.5:7b "Reply with OK." --keepalive 30m

# 或使用 API 预热
curl http://localhost:11434/api/generate -d '{
  "model": "qwen2.5:7b",
  "prompt": "OK",
  "keep_alive": "30m"
}'
```

**效果**: 模型常驻内存，首次推理 < 5s，探测和会话都不会超时。

#### 方案 B: 使用小模型探测

在 LLM 设置中：
- 探测用小模型（如 `qwen2.5:0.5b`，冷加载 < 5s）
- 会话用大模型（如 `qwen2.5:7b`）

#### 方案 C: 确保 Base URL 正确

配置 Custom Provider 时，确保 Base URL 为以下之一：
- `http://host:11434/v1`（推荐，显式指定）
- `http://host:11434`（可接受，`normalizeOpenAIBaseURL` 会自动补 `/v1`）

**避免**: `http://host:11434/api`（不会被自动修正）

#### 方案 D: 确保 Ollama 模型已 pull

```bash
# 查看已 pull 的模型
ollama list

# pull 需要的模型
ollama pull qwen2.5:7b
```

#### 方案 E: 修复 docker-compose 网络

在 `deploy/docker-compose.yml` 的 `ongrid-ollama` 服务中添加网络声明：

```yaml
  ongrid-ollama:
    image: docker.io/ollama/ollama:latest
    container_name: ongrid-ollama
    networks:
      - ongrid_net          # ← 添加此行
    environment:
      - OLLAMA_MODELS=/modelfiles
    # ...
```

#### 方案 F: 检查 GPU 内存

```bash
# NVIDIA
nvidia-smi

# 确保 Ollama 进程的 GPU 内存占用 < 总显存
# 如果 OOM，考虑：
# 1. 使用更小的模型
# 2. 使用量化版本（q4_K_M 等）
# 3. 设置 OLLAMA_NUM_GPU=0 强制 CPU 推理
```

### 6.2 代码级改进（需改代码）

#### 改进 1: 实现真正的 token-by-token 流式

**当前**: `clientChatModel.Stream` 调用 `Generate` 后包装成单 chunk StreamReader

**改进**: 切换底层 SDK 为 `sdk.CreateChatCompletionStream`，实现真正的逐 token 流式：

```go
// 改进后的 Stream 实现（示意）
func (c *clientChatModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
    req, sdkReq, err := c.buildChatReq(ctx, input, opts)
    if err != nil { return nil, err }

    stream, err := sdk.CreateChatCompletionStream(ctx, sdkReq)
    if err != nil { return nil, err }

    // 将 SDK 的 *http.Response body 转为 schema.StreamReader
    return newOpenAIStreamReader(stream), nil
}
```

**同时需要**:
- 在 `toCallbackEmitter` 中启用 `SSEEventAssistantDelta`（行 880-883 改为转发）
- 前端 `dispatchFrame` 已支持 `assistant` 帧的增量更新逻辑

#### 改进 2: 增大探测超时或添加冷加载重试

**方案 2a**: 增大探测超时到 60s（不推荐，影响所有 provider 的探测速度）

**方案 2b**（推荐）: 对 Ollama/custom provider 使用更长的探测超时：

```go
// llm_probe.go 中根据 provider 类型选择超时
probeTimeout := defaultLLMProbeTimeout  // 20s
if cfg.Provider == "custom" {
    probeTimeout = 90 * time.Second  // Ollama 冷加载可能 30-90s
}
```

**方案 2c**（推荐）: 探测失败时自动重试一次（冷加载可能刚好在首次超时后完成）：

```go
result, err := probeOnce(ctx, cfg)
if err != nil && isTimeoutErr(err) && cfg.Provider == "custom" {
    // 冷加载可能仍在进行，等待后重试
    time.Sleep(10 * time.Second)
    result, err = probeOnce(ctx, cfg)
}
```

#### 改进 3: 为 Ollama 添加健康检查

在 docker-compose.yml 中为 `ongrid-ollama` 添加 healthcheck：

```yaml
  ongrid-ollama:
    # ...
    healthcheck:
      test: ["CMD", "ollama", "list"]
      interval: 30s
      timeout: 10s
      retries: 3
```

#### 改进 4: 添加 Ollama 冷加载状态提示

在 `buildGraphErrorApology` 中识别 Ollama 冷加载特征：

```go
// 识别 Ollama 冷加载超时
case strings.Contains(low, "model") && strings.Contains(low, "loading"):
    return "Ollama 正在加载模型到内存，这通常需要 30-90 秒。请稍后重试，或使用 `ollama run --keepalive` 预热模型。"
```

#### 改进 5: SDK 自定义 HTTP Transport

为 Ollama 长连接/慢响应优化 HTTP 客户端：

```go
transport := &http.Transport{
    MaxIdleConns:        10,
    MaxIdleConnsPerHost: 5,
    IdleConnTimeout:     120 * time.Second,  // Ollama 冷加载可能很长
    DisableKeepAlives:   false,
}
```

### 6.3 Nginx 层确认

当前 Nginx 配置已正确关闭 SSE 缓冲：

**文件**: [nginx.conf](file:///d:/claude/ongrid/deploy/nginx/nginx.conf) 行 101-125

```nginx
proxy_buffering       off;     # SSE 流必须关闭
proxy_cache           off;
chunked_transfer_encoding on;
proxy_read_timeout    300s;    # 5 分钟读超时，足够长
```

后端也设置了 `X-Accel-Buffering: no` (http.go 行 453)。

> **结论**: Nginx 层配置正确，不是 stream error 的根因。

---

## 7. 架构红线

| # | 红线 | 说明 |
|---|------|------|
| 1 | **禁止在探测路径和会话路径使用不同配置源** | 探测用 UI 临时配置，会话用 DB 配置 + Resolver 60s TTL 缓存，不一致时探测成功但会话失败 |
| 2 | **禁止忽略 Ollama 冷加载时间** | 大模型冷加载 30-90s，必须通过预热或增大超时覆盖 |
| 3 | **禁止裸 URL 不补 /v1** | `normalizeOpenAIBaseURL` 已处理，但自定义 path 不会被修正 |
| 4 | **禁止 docker-compose 服务不声明网络** | `ongrid-ollama` 缺少 `networks: - ongrid_net` |
| 5 | **禁止伪流式长期存在** | 当前 `clientChatModel.Stream` 是伪流式，必须尽快实现真正的 token-by-token 流式 |
| 6 | **禁止丢弃 assistant_delta 帧长期存在** | `toCallbackEmitter` 行 880-883 丢弃 delta 帧，前端无法逐字显示 |
| 7 | **禁止无重试的 Ollama 超时** | 冷加载超时后应重试一次，而非直接失败 |
| 8 | **禁止 Ollama 无健康检查** | docker-compose 中 Ollama 服务需要 healthcheck |

---

## 8. 关键文件索引

### 后端

| 文件 | 关键行号 | 职责 |
|------|---------|------|
| [http.go](file:///d:/claude/ongrid/internal/manager/server/aiops/http.go) | 414-477 | `postMessageStream` SSE 入口 |
| [http.go](file:///d:/claude/ongrid/internal/manager/server/aiops/http.go) | 601-612 | `writeSSE` 帧序列化 + Flush |
| [service.go](file:///d:/claude/ongrid/internal/manager/service/aiops/service.go) | 334-376 | `runWithKernel` 内核路由 |
| [service.go](file:///d:/claude/ongrid/internal/manager/service/aiops/service.go) | 434-467 | `runGraph` graph 内核入口 |
| [runtime.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go) | 468-851 | `Handle` 完整编排 |
| [runtime.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go) | 859-925 | `toCallbackEmitter` 事件翻译（delta 丢弃） |
| [runtime.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go) | 1699-1778 | `buildGraphErrorApology` 错误分类 |
| [react.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/react.go) | 78-188 | `BuildReActGraph` 拓扑 |
| [sse.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/sse.go) | 236-441 | SSEHandler 回调→事件 |
| [sse.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/sse.go) | 404-440 | `OnEndWithStreamOutput` + `drainStream` |
| [chain.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/chain.go) | 88-118 | `NewDefaultHandlers` 回调链 |
| [persistence.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/persistence.go) | 560-622 | `flushIncompleteBatch` 自动修复 |
| [client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go) | 44 | `defaultTimeout = 120s` |
| [client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go) | 298-331 | `normalizeOpenAIBaseURL` Ollama 修正 |
| [client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go) | 355-474 | `Chat` 非流式调用 |
| [eino_routing.go](file:///d:/claude/ongrid/internal/pkg/llm/eino_routing.go) | 200-207 | `RoutingChatModel.Stream` |
| [eino_routing.go](file:///d:/claude/ongrid/internal/pkg/llm/eino_routing.go) | 370-378 | `clientChatModel.Stream`（伪流式） |
| [llm_probe.go](file:///d:/claude/ongrid/internal/manager/biz/setting/llm_probe.go) | 36-37 | `defaultLLMProbeTimeout = 20s` |
| [llm_probe.go](file:///d:/claude/ongrid/internal/manager/biz/setting/llm_probe.go) | 368-418 | `classifyLLMProbeError` 错误分类 |

### 前端

| 文件 | 关键行号 | 职责 |
|------|---------|------|
| [chat.ts](file:///d:/claude/ongrid/web/src/api/chat.ts) | 252-317 | `streamMessage` fetch + ReadableStream SSE 消费 |
| [chat.ts](file:///d:/claude/ongrid/web/src/api/chat.ts) | 319-358 | `dispatchFrame` SSE 帧解析与事件分发 |
| [chat.ts](file:///d:/claude/ongrid/web/src/api/chat.ts) | 101-106 | `stopSession` 服务端中断 |
| [ChatThread.tsx](file:///d:/claude/ongrid/web/src/pages/ChatThread.tsx) | 217-416 | `send()` 完整流式消费 + 消息状态管理 |
| [ChatThread.tsx](file:///d:/claude/ongrid/web/src/pages/ChatThread.tsx) | 423-431 | Esc 停止机制 |
| [ChatThread.tsx](file:///d:/claude/ongrid/web/src/pages/ChatThread.tsx) | 107-174 | 5 秒轮询 + 指纹去重 |
| [LLM.tsx](file:///d:/claude/ongrid/web/src/pages/settings/LLM.tsx) | 139-153 | Ollama 配置提示 |

### 部署

| 文件 | 关键行号 | 职责 |
|------|---------|------|
| [docker-compose.yml](file:///d:/claude/ongrid/deploy/docker-compose.yml) | 365-382 | Ollama 服务定义（缺 networks） |
| [nginx.conf](file:///d:/claude/ongrid/deploy/nginx/nginx.conf) | 101-125 | SSE 代理配置（buffering off） |

### 独立工具

| 文件 | 关键行号 | 职责 |
|------|---------|------|
| [main.go](file:///d:/claude/ongrid/cmd/ollama/main.go) | 55-145 | 独立 Ollama NDJSON 流式测试工具 |

---

## 附录: 超时层次总览

| 层级 | 超时值 | 文件:行 | 说明 |
|------|--------|---------|------|
| Nginx proxy_connect | 10s | nginx.conf:120 | 建立连接超时 |
| Nginx proxy_send | 300s | nginx.conf:121 | 发送请求超时 |
| Nginx proxy_read | 300s | nginx.conf:122 | 读取响应超时 |
| Go LLM 生产 | 120s | client.go:44 | Chat Completion 调用超时 |
| Go LLM 探测 | 20s | llm_probe.go:48 | 探测请求超时 |
| Go Resolver 缓存 | 60s | client.go:185 | 凭据缓存 TTL |
| Go Persistence 写入 | 5s | persistence.go:55 | DB 写入超时 |
