# OnGrid `/chat/:sessionId` 完整调用链分析

> 从前端路由 `/chat/:sessionId` 出发，追踪到后端 LLM 调用的端到端全链路。
> 包含源码文件路径、关键行号、函数签名、数据流向。

---

## 目录

1. [前端路由入口](#1-前端路由入口)
2. [ChatThread 页面组件](#2-chatthread-页面组件)
3. [前端 API 层](#3-前端-api-层)
4. [API Client 封装](#4-api-client-封装)
5. [Chat 子组件](#5-chat-子组件)
6. [Auth Store](#6-auth-store)
7. [后端路由注册与 Handler](#7-后端路由注册与-handler)
8. [中间件链](#8-中间件链)
9. [Service 层](#9-service-层)
10. [Biz 层 — chatruntime](#10-biz-层--chatruntime)
11. [Biz 层 — agent（Legacy Kernel）](#11-biz-层--agentlegacy-kernel)
12. [Biz 层 — graph（ReAct 图）](#12-biz-层--graphreact-图)
13. [Callback 链](#13-callback-链)
14. [LLM 调用链](#14-llm-调用链)
15. [Data 层（GORM 持久化）](#15-data-层gorm-持久化)
16. [Model 层（数据模型）](#16-model-层数据模型)
17. [端到端数据流总览](#17-端到端数据流总览)
18. [关键设计要点](#18-关键设计要点)

---

## 1. 前端路由入口

**文件**: [App.tsx](file:///d:/claude/ongrid/web/src/App.tsx)

```tsx
// 第 8 行：懒加载 ChatThread 页面
const ChatThreadPage = lazy(() => import('@/pages/ChatThread'));

// 第 89-95 行：RequireAuth + Layout 包裹
<Route element={<RequireAuth><Layout /></RequireAuth>}>
  // 第 98 行：chat 路由声明
  <Route path="/chat/:sessionId" element={<ChatThreadPage />} />
```

**数据流**: 浏览器 URL `/chat/abc123` → React Router 解析 → 懒加载 `ChatThread` 模块 → 渲染 `ChatThreadPage`。

**RequireAuth 守卫**（第 63-70 行）: 从 `useAuth` 读取 token，无 token 则重定向到 `/login`。

---

## 2. ChatThread 页面组件

**文件**: [ChatThread.tsx](file:///d:/claude/ongrid/web/src/pages/ChatThread.tsx)（588 行）

### 2.1 URL 参数提取

```tsx
// 第 28 行
const { sessionId = '' } = useParams<{ sessionId: string }>();
// 第 29-30 行
const location = useLocation();
const initialPrompt = (location.state as LocationState)?.initialPrompt;
```

`sessionId` 从 URL 提取；`initialPrompt` 从路由 state 读取（Home 页透传的初始提问）。

### 2.2 关键 State / Ref

| 行号 | 声明 | 用途 |
|------|------|------|
| 37 | `const [messages, setMessages] = useState<ChatMessage[]>([])` | 消息列表（含合成 tool card 行） |
| 38 | `const [submitting, setSubmitting] = useState(false)` | 是否有进行中的 turn |
| 39 | `const [error, setError]` | 错误文案 |
| 40 | `const [loading, setLoading]` | 首次加载态 |
| 45 | `const abortRef = useRef<AbortController>` | 当前 turn 的 AbortController（Esc 取消） |
| 59 | `const stickToBottomRef = useRef(true)` | 是否自动贴底滚动 |
| 66 | `const [providers, setProviders]` | LLM 提供商目录 |
| 70-73 | `storeModel` / `catalogDefault` / `selectedModel` | 模型选择 |
| 78 | `const [webSearchEnabled, setWebSearchEnabled] = useState(true)` | 联网搜索开关 |
| 104-105 | `submittingRef` | submitting 的 ref 镜像，供定时器闭包读取 |

### 2.3 消息列表加载与轮询

**文件**: ChatThread.tsx 第 107-174 行

```tsx
// 第 119-160 行：refetch 函数
Promise.all([getMessages(sessionId), listApprovals('pending').catch(()=>({items:[]}))])
```

- `getMessages` 拉历史消息（[chat.ts:108](file:///d:/claude/ongrid/web/src/api/chat.ts#L108)）
- `listApprovals` 拉待审批，过滤本 session 的审批卡片
- 第 162 行：`refetch(true)` 首次加载
- 第 163-169 行：`setInterval(tick, 5000)` 每 5 秒轮询
- 三重守卫：`cancelled` / `document.visibilityState !== 'visible'` / `submittingRef.current` — 标签页隐藏或有 SSE 在飞时不轮询

### 2.4 核心：send 函数（SSE 流式发送）

**文件**: ChatThread.tsx 第 217-416 行

```tsx
async function send(content, mentions, opts?) {
  // 第 225-226 行：新建 AbortController
  const ac = new AbortController();
  abortRef.current = ac;

  // 第 235-239 行：乐观渲染用户气泡
  setMessages(prev => [...prev, { id: `optimistic-user-${Date.now()}`, role: 'user', content }]);

  // 第 242-391 行：调用 streamMessage
  await streamMessage(sessionId, content, callbacks, options, ac.signal);
  // callbacks:
  //   onAssistant (246-275)  → 按 message_id 去重，替换或追加 assistant 气泡
  //   onToolStart (276-291)  → 插入 kind:'tool_card' pending 卡片
  //   onToolEnd (292-320)    → 更新卡片 status/result/duration
  //   onApprovalPending (321-375) → 盖 pending_approval blob 到卡片 result
  //   onDone (376-378)       → invalidateChatSessions() 刷侧栏
  //   onError (379-381)      → throw err 交给 catch

  // 第 393-411 行 catch：AbortError 静默；其余设 error + 追加错误气泡
  // 第 412-415 行 finally：setSubmitting(false)，清 abortRef
}
```

### 2.5 Esc 停止

**文件**: ChatThread.tsx 第 423-431 行

```tsx
useEffect(() => {
  const onKey = (e: KeyboardEvent) => {
    if (e.key === 'Escape' && submittingRef.current) {
      stopSession(sessionId);        // 服务端停
      abortRef.current?.abort();     // 客户端断流
    }
  };
  window.addEventListener('keydown', onKey);
  return () => window.removeEventListener('keydown', onKey);
}, [sessionId]);
```

### 2.6 渲染结构

**文件**: ChatThread.tsx 第 509-587 行

- `PageHeader`：标题 + `AgentBadge` + `#sessionId`
- 滚动区：`messages.map(m => <MessageBubble key={m.id} message={m} />)`
- `submitting` 时显示 `ThinkingIndicator`
- `ChatInput`：`onSubmit={(p) => void send(p.text, p.mentions)}`

---

## 3. 前端 API 层

**文件**: [chat.ts](file:///d:/claude/ongrid/web/src/api/chat.ts)（359 行）

### 3.1 REST 函数

| 行号 | 函数 | HTTP 路径 |
|------|------|-----------|
| 73-78 | `listSessions(params?)` | `GET /chat/sessions` |
| 80-87 | `createSession(input)` | `POST /chat/sessions` |
| 89-95 | `renameSession(sessionId, title)` | `PATCH /chat/sessions/{id}` |
| 101-106 | `stopSession(sessionId)` | `POST /chat/sessions/{id}/stop` |
| 108-113 | `getMessages(sessionId)` | `GET /chat/sessions/{id}/messages` |
| 147-159 | `postMessage(sessionId, content, opts)` | `POST /chat/sessions/{id}/messages` |
| 163-170 | `searchMentions(params)` | `GET /aiops/mentions/search` |
| 184-186 | `listModels()` | `GET /aiops/models` |
| 192-194 | `deleteSession(sessionId)` | `DELETE /chat/sessions/{id}` |

所有 REST 函数走 `request<T>(method, path, body?)` 封装（见第 4 节）。

### 3.2 SSE 流式：streamMessage

**文件**: chat.ts 第 252-317 行

```typescript
export async function streamMessage(
  sessionId: string | number,
  content: string,
  cbs: StreamCallbacks,
  opts: SendOptions = {},
  signal?: AbortSignal,
): Promise<void> {
  // 第 259 行：URL（注意：不走 client.ts 的 request，自己拼全 URL）
  const url = `/api/v1/chat/sessions/${encodeURIComponent(String(sessionId))}/messages/stream`;

  // 第 264-265 行：手动注入 Authorization
  const token = getToken();
  if (token) headers['Authorization'] = `Bearer ${token}`;

  // 第 274-279 行：fetch POST
  const res = await fetch(url, { method: 'POST', headers, body: JSON.stringify(body), signal });

  // 第 281-296 行：非 2xx 解析 JSON 错误体，抛 ApiError

  // 第 298-316 行：ReadableStream + TextDecoder 切帧
  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buf = '';
  while (true) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    let sep: number;
    while ((sep = buf.indexOf('\n\n')) >= 0) {  // 帧分隔符
      const frame = buf.slice(0, sep);
      buf = buf.slice(sep + 2);
      dispatchFrame(frame, cbs);
    }
  }
  if (buf.trim()) dispatchFrame(buf, cbs);  // 残留帧
}
```

### 3.3 帧分发：dispatchFrame

**文件**: chat.ts 第 319-358 行

- 解析 `event:` 行（默认 `message`）与 `data:` 行
- `JSON.parse(dataLines.join('\n'))`
- `switch(event)` 分派到 `onAssistant` / `onToolStart` / `onToolEnd` / `onApprovalPending` / `onDone` / `onError`

### 3.4 SSE 事件类型

| event | 回调 | payload 类型 | 行号 |
|-------|------|-------------|------|
| `assistant` | `onAssistant` | `AssistantStreamEvent` | 200-206 |
| `tool_start` | `onToolStart` | `ToolStreamEvent` | 208-221 |
| `tool_end` | `onToolEnd` | `ToolStreamEvent` | 208-221 |
| `approval_pending` | `onApprovalPending` | `ApprovalPendingStreamEvent` | 229-235 |
| `done` | `onDone` | `PostMessageResponse` | 65-71 |
| `error` | `onError` | `Error` | — |

---

## 4. API Client 封装

**文件**: [client.ts](file:///d:/claude/ongrid/web/src/api/client.ts)（162 行）

### 4.1 request\<T\> 函数

**第 27-115 行**:

```typescript
async function request<T>(method, path, body?, opts?): Promise<T> {
  // 第 33-41 行：默认 headers
  //   Accept: application/json
  //   Accept-Language: getLocale()  ← 让后端 LLM 输出跟随 UI 语言

  // 第 43-46 行：注入 Authorization
  const token = getToken();
  if (token && !opts?.noAuth) headers['Authorization'] = `Bearer ${token}`;

  // 第 48-57 行：body 处理（FormData 透传 / JSON.stringify）
  // 第 59 行：URL 拼接 BASE = '/api/v1'

  // 第 62-67 行：fetch，网络异常抛 ApiError(msg, 0)

  // 第 82-111 行：非 res.ok 时
  //   提取 error / message / code
  //   401 自动刷新（第 97-110 行）：
  //     调 refreshAccessToken()，成功且未重试过则递归重试一次
  //     刷新失败则 useAuth.getState().logout()

  // 第 114 行：返回 parsed as T
}
```

### 4.2 refreshAccessToken

**第 117-162 行**: 单飞（`refreshInFlight` 去重），用 `getRefreshToken()` 调 `POST /api/v1/auth/refresh`，成功则 `useAuth.getState().setSession(...)`。

---

## 5. Chat 子组件

### 5.1 ChatInput

**文件**: [ChatInput.tsx](file:///d:/claude/ongrid/web/src/components/ChatInput.tsx)（777 行）

- 第 77-91 行：`export function ChatInput({ value, onChange, onSubmit, placeholder, disabled, providers, selectedModel, onModelChange, webSearchEnabled, ... })`
- 第 41-44 行：`SubmitPayload = { text: string; mentions: Mention[] }`
- 第 126-132 行：textarea 自动撑高（最多 6 行）
- 第 163-181 行：`recomputeMentionContext` — 从光标回溯找 `@`
- 第 184-206 行：debounce 150ms 调 `searchMentions({q, limit:8})`
- 第 221-245 行：`insertMention` — 插入 `@type:id(label) ` + push chip
- 第 251-258 行：`submit` — trim 后调 `onSubmit({text, mentions: chips})`
- 第 260-313 行：`onKeyDown` — Enter 提交、Shift+Enter 换行
- 第 605-725 行：`ModelDropdown` — 模型选择下拉
- 第 732-758 行：`WebSearchToggle` — 联网搜索开关

### 5.2 MessageBubble

**文件**: [MessageBubble.tsx](file:///d:/claude/ongrid/web/src/components/MessageBubble.tsx)（647 行）

- 第 36-52 行：按 `message.kind` / `message.role` 分派：
  - `kind === 'tool_card'` → `ToolCallSummaryBlock`
  - `role === 'tool'` → `ToolBubble`（历史回放）
  - `role === 'user'` → `UserBubble`（右对齐）
  - 空内容 assistant → `null`（抑制工具专用空泡）
  - 其余 → `AssistantBubble`（ReactMarkdown + remarkGfm）
- 第 141-244 行：`ToolCallSummaryBlock` — 可折叠工具卡片
  - 第 165-168 行：`pendingApproval(call.result)` → `PendingApprovalCard`
  - 第 169 行：`asConfigDraft(call.result)` → `ConfigDraftCard`
- 第 246-330 行：`ConfigDraftCard` — 告警规则草案确认卡
- 第 485-633 行：`PendingApprovalCard` — HLD-017 内联审批卡
  - 第 503-537 行：mount 时 `getApproval(approvalID)` 对账真实状态
  - 第 539-554 行：`approve` → `approveApproval(approvalID)`
  - 第 555-564 行：`reject` → `rejectApproval(approvalID, '')`

### 5.3 AgentBadge

**文件**: [AgentBadge.tsx](file:///d:/claude/ongrid/web/src/components/AgentBadge.tsx)（70 行）

- 第 41-69 行：按 `agentId` 映射中英文 persona 标签，indigo chip + Bot 图标

### 5.4 SSE 事件 → UI state 更新链路

```
后端 SSE 帧
  ↓ fetch ReadableStream
streamMessage (chat.ts:252) 逐帧切分
  ↓ dispatchFrame (chat.ts:319) JSON.parse + switch(event)
StreamCallbacks
  ↓
ChatThread.send (ChatThread.tsx:217) 内回调:
  onAssistant  → setMessages 替换/追加 assistant 气泡 (按 message_id 去重)
  onToolStart  → setMessages 追加 kind:'tool_card' pending 卡片
  onToolEnd    → setMessages 更新对应卡片 status/result/duration
  onApprovalPending → setMessages 盖 pending_approval 到卡片 result
  onDone       → invalidateChatSessions() 刷侧栏
  onError      → throw → catch 设 error + 追加错误气泡
  ↓
MessageBubble 按 kind/role 分派渲染
  ToolCallSummaryBlock 检测 result:
    pending_approval → PendingApprovalCard (内联批准/拒绝)
    config_draft     → ConfigDraftCard (告警草案确认)
```

---

## 6. Auth Store

**文件**: [auth.ts](file:///d:/claude/ongrid/web/src/store/auth.ts)（49 行）

```typescript
// 第 20-41 行：zustand + persist，持久化到 localStorage key "ongrid.auth"
export const useAuth = create<AuthState>()(persist(...));

// 第 43-45 行：非 hook，供 client.ts 和 chat.ts 在非组件上下文同步读取
export function getToken(): string | null {
  return useAuth.getState().token;
}

// 第 47-49 行
export function getRefreshToken(): string | null {
  return useAuth.getState().refreshToken;
}
```

**调用点**:
- `client.ts:44` — `getToken()` 注入 Authorization
- `client.ts:98` — 401 时 `refreshAccessToken` 用 `getRefreshToken()`
- `chat.ts:264` — `streamMessage` 绕过 request，自己用 `getToken()` 注入

---

## 7. 后端路由注册与 Handler

**文件**: [http.go](file:///d:/claude/ongrid/internal/manager/server/aiops/http.go)

### 7.1 路由注册

**第 131-156 行** — `Register` 方法：

| 行号 | HTTP | 路由 | Handler |
|------|------|------|---------|
| 132 | POST | `/v1/chat/sessions` | `h.createSession` |
| 133 | GET | `/v1/chat/sessions` | `h.listSessions` |
| 134 | POST | `/v1/chat/sessions/{id}/messages` | `h.postMessage` |
| 135 | POST | `/v1/chat/sessions/{id}/messages/stream` | `h.postMessageStream` |
| 136 | POST | `/v1/chat/sessions/{id}/stop` | `h.stopSession` |
| 137 | GET | `/v1/chat/sessions/{id}/messages` | `h.listMessages` |
| 138 | DELETE | `/v1/chat/sessions/{id}` | `h.closeSession` |
| 139 | PATCH | `/v1/chat/sessions/{id}` | `h.renameSession` |

### 7.2 AIOpsService 接口

**第 41-55 行**:

```go
type AIOpsService interface {
    CreateSession(ctx, caller, in CreateSessionInput) (*model.Session, error)
    ListSessions(ctx, caller, limit, offset int, relatedIncidentID *uint64) ([]*model.Session, error)
    ListMessages(ctx, caller, sessionID string) ([]*model.Message, error)
    CloseSession(ctx, caller, sessionID string) error
    DeleteSession(ctx, caller, sessionID string) error
    RenameSession(ctx, caller, sessionID string, title string) error
    PostMessage(ctx, caller, sessionID, content string) (*agent.Reply, error)
    PostMessageWithOpts(ctx, caller, sessionID, content string, opts agent.RunOptions) (*agent.Reply, error)
    PostMessageStream(ctx, caller, sessionID, content string, emit agent.Emit) (*agent.Reply, error)
    PostMessageStreamWithOpts(ctx, caller, sessionID, content string, emit agent.Emit, opts agent.RunOptions) (*agent.Reply, error)
    StopSession(ctx, caller, sessionID string) (bool, error)
    UsageToday(ctx) (*biz.DailyUsage, error)
    ListMutatingProposals(ctx, caller, f biz.MutatingProposalFilter) ([]*model.MutatingProposal, int64, error)
}
```

### 7.3 postMessageStream Handler（SSE 流式）

**第 414-477 行**:

```go
func (h *Handler) postMessageStream(w http.ResponseWriter, r *http.Request) {
    // 第 438 行：检测 http.Flusher 支持，不支持时降级为阻塞 PostMessageWithOpts
    flusher, ok := w.(http.Flusher)

    // 第 451-453 行：SSE 头
    w.Header().Set("Content-Type", "text/event-stream")
    w.Header().Set("Cache-Control", "no-cache")
    w.Header().Set("X-Accel-Buffering", "no")  // 禁用 nginx 缓冲

    // 第 454 行：立即发送 200
    w.WriteHeader(http.StatusOK)

    // 第 456-457 行：心跳帧
    w.Write([]byte(": ok\n\n"))
    flusher.Flush()

    // 第 459-461 行：emit 闭包
    emit := func(e agent.Event) {
        writeSSE(w, flusher, eventName(e.Type), eventPayload(id, e))
    }

    // 第 463 行：调用 service
    reply, err := h.svc.PostMessageStreamWithOpts(r.Context(), caller, id, req.Content, emit, opts)

    // 第 464-470 行：流开始后出错，发 error SSE 帧（不改 status code）
    // 第 474-476 行：兜底 summary 帧
}
```

### 7.4 eventName / eventPayload / writeSSE

- **eventName**（第 479-496 行）: `agent.EventType` → SSE wire 名映射
  - `EventAssistant` → `"assistant"`
  - `EventToolStart` → `"tool_start"`
  - `EventToolEnd` → `"tool_end"`
  - `EventDone` → `"done"`
  - `EventApprovalPending` → `"approval_pending"`

- **eventPayload**（第 498-596 行）: 按 `e.Type` 分支构造 JSON payload
  - EventAssistant: `session_id / iteration / message_id / content / created_at / pending_tool_calls`
  - EventToolStart/End: `tool_call_id / name / status / duration_ms / arguments / result`
  - EventDone: 委托 `toPostMessageResp` 生成完整 Reply DTO
  - EventApprovalPending: `approval_id / tool_call_id / kind / command / credentials`

- **writeSSE**（第 601-612 行）:
  ```go
  func writeSSE(w http.ResponseWriter, f http.Flusher, name string, payload any) {
      body, _ := json.Marshal(payload)
      w.Write([]byte("event: "))
      w.Write([]byte(name))
      w.Write([]byte("\ndata: "))
      w.Write(body)
      w.Write([]byte("\n\n"))
      f.Flush()
  }
  ```

### 7.5 callerFromCtx

**第 967-973 行**:

```go
func callerFromCtx(ctx context.Context) (svc.Caller, bool) {
    t, ok := tenantctx.From(ctx)
    return svc.Caller{UserID: t.UserID, Role: t.Role}, ok
}
```

从 context 中的 `tenantctx.Tenant` 提取调用者身份。

---

## 8. 中间件链

### 8.1 完整中间件栈（从外到内）

**文件**: [main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go)

```
HTTP 请求
    │
    ▼
mux (chi.NewRouter)                          // 第 2718 行
  ├── otelhttpmw (OTel span)                 // 第 2726 行
  ├── MetricsMiddleware (HTTP 指标)           // 第 2729 行
  └── AuditMiddleware (审计日志)              // 第 2732 行
    │
    ▼
/api 路由组                                   // 第 2750 行
  └── protected.Group                         // 第 2779 行
       └── auth.Middleware(signer)            // 第 2780 行
            ├── 提取 Bearer JWT
            ├── signer.Verify(tok)
            └── tenantctx.With(ctx, Tenant)
    │
    ▼
aiopsHandler.Register(protected)             // 第 2850 行
```

### 8.2 Auth 中间件

**文件**: [middleware.go](file:///d:/claude/ongrid/internal/pkg/auth/middleware.go) 第 21-53 行

```go
func Middleware(signer *Signer) func(http.Handler) http.Handler
```

1. 从 `Authorization: Bearer <token>` 头提取 JWT
2. `signer.Verify(tok)` 验证
3. 构造 `tenantctx.Tenant{UserID, Email, Role, IsSuperuser}`
4. `tenantctx.SetOnSlot(r.Context(), t)` — 写入可变 slot（供审计中间件读取）
5. `tenantctx.With(r.Context(), t)` — 写入 context

### 8.3 tenantctx 双层存储

**文件**: [tenantctx.go](file:///d:/claude/ongrid/internal/pkg/tenantctx/tenantctx.go)

- `With(ctx, t)` / `From(ctx)` — 标准 context 存取（不可变）
- `WithSlot(ctx)` / `SetOnSlot(ctx, t)` — 可变 slot，让外层中间件看到内层写入的值
- `From` 优先读 slot，再读 context value

---

## 9. Service 层

**文件**: [service.go](file:///d:/claude/ongrid/internal/manager/service/aiops/service.go)

### 9.1 Service 结构体

**第 73-89 行**:

```go
type Service struct {
    legacyAgent *agent.Agent              // 旧版 for-loop kernel
    runtime     RuntimeHandler            // 新版 graph kernel
    kernel      Kernel                    // "legacy" | "graph" 分发开关
    sessions    biz.SessionRepo           // 会话持久化
    proposals   biz.MutatingProposalRepo
    usage       *biz.UsageUsecase
    log         *slog.Logger
    cancelMu    sync.Mutex
    cancels     map[string]context.CancelFunc  // sessionID → cancelFunc
}
```

### 9.2 runWithKernel — 核心分发

**第 334-376 行**:

```go
func (s *Service) runWithKernel(ctx, caller, sessionID, content string, emit agent.Emit, opts agent.RunOptions) (*agent.Reply, error) {
    // 1. 内容校验 (335-338)
    // 2. 所有权检查 (339) — s.GetSession，非 owner 非 admin 返回 ErrNotFound
    // 3. HLD-021 上下文分离 (353)
    ctx = context.WithoutCancel(ctx)
    // 4. 注册取消 (358-360)
    ctx, cancel := context.WithCancel(ctx)
    s.registerCancel(sess.ID, cancel)
    defer s.unregisterCancel(sess.ID, cancel)
    // 5. 内核分发 (363-375)
    if kernel == graph && runtime != nil {
        return s.runGraph(ctx, sess, content, emit, opts)
    }
    return s.legacyAgent.RunStreamWithOpts(ctx, sessionID, sess.UserID, content, emit, opts)
}
```

### 9.3 runGraph — Graph Kernel 路径

**第 434-467 行**:

```go
func (s *Service) runGraph(ctx, sess, content, emit, opts) (*agent.Reply, error) {
    // 第 436-439 行：构造 chatruntime.Emit，内部调 translateRuntimeEvent
    graphEmit := func(ev chatruntime.Event) {
        emit(translateRuntimeEvent(ev))
    }
    // 第 450-461 行：构造 chatruntime.Request
    req := &chatruntime.Request{SessionID, UserID, Role, UserText, Mentions, Provider, Model, ...}
    // 第 462 行：调用 runtime
    reply, err := s.runtime.Handle(ctx, req)
    // 第 466 行：类型转换
    return runtimeReplyToAgentReply(reply), nil
}
```

### 9.4 translateRuntimeEvent

**第 472-520 行**: 将 `chatruntime.Event` 字段逐一映射到 `agent.Event`，保证 SSE 帧字节级兼容。

### 9.5 StopSession

**第 406-421 行**:

```go
func (s *Service) StopSession(ctx, caller, sessionID string) (bool, error) {
    // 所有权检查
    s.cancelMu.Lock()
    cancel, ok := s.cancels[sessionID]
    delete(s.cancels, sessionID)
    s.cancelMu.Unlock()
    if ok { cancel(); return true, nil }
    return false, nil  // 幂等
}
```

### 9.6 registerCancel / unregisterCancel

- **registerCancel**（第 382-389 行）: 先 cancel 同 session 旧 turn，再注册新的
- **unregisterCancel**（第 394-400 行）: 用 `reflect.ValueOf(a).Pointer()` 指针比较，避免误删新 turn 的句柄

---

## 10. Biz 层 — chatruntime

**文件**: [runtime.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go)

### 10.1 Runtime 结构体

**第 292-304 行**:

```go
type Runtime struct {
    cfg Config
    log *slog.Logger
    workersMu sync.Mutex
    workers   map[string]*Worker  // 内存中的子 agent
    bag ToolBagProvider
}
```

### 10.2 Handle 方法 — 10 步编排

**第 468-851 行**:

```go
func (rt *Runtime) Handle(ctx context.Context, req *Request) (*Reply, error)
```

| 步骤 | 行号 | 说明 |
|------|------|------|
| 1. 所有权检查 | 486-494 | `GetSession` + `sess.UserID != req.UserID` → ErrNotFound |
| 2. @-mention 水合 | 497-508 | `MentionResolver.Resolve` → markdown bullet 前缀 |
| 3. 持久化用户消息 | 511-521 | `AppendMessage` 写 role=user 行 |
| 4. 加载历史 | 524-527 | `ListMessages(sess.ID, HistoryLimit)` |
| 5. 技能+系统提示词 | 533-658 | SkillRegistry.Resolve + persona + 工具过滤 + ComposeSystemPrompt |
| 6. 构建 eino 历史 | 662 | `buildEinoHistory` — 含 HOIST 机制 |
| 7. 构建 ReAct 图 | 668-683 | `graph.BuildReActGraph(chatModel, tools, cfg)` |
| 8. 组装回调链 | 689-701 | `callbacks.NewDefaultHandlers(deps)` — 6 个 handler |
| 9. 动态提示 | 707-713 | `calcDynamicHints` — 重复检测、承诺未执行 |
| 10. 调用图 | 727-850 | `g.Invoke(ctx, &graph.Input{...})` |

### 10.3 toCallbackEmitter

**第 859-925 行**: 将 `agent.Emit` 适配为 `callbacks.Handler` 链中的 SSE 事件发射器。当前 `assistant_start` 和 `assistant_delta` 被丢弃（伪流式）。

---

## 11. Biz 层 — agent（Legacy Kernel）

**文件**: [agent.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/agent/agent.go)

### 11.1 核心类型

```go
// 第 55-60 行
type Reply struct {
    Message    *model.Message
    Usage      llm.Usage
    Iterations int
    ToolCalls  []*model.ToolCall
}

// 第 160 行
type Emit func(Event)

// 第 96-103 行
type Event struct {
    Type         EventType
    Assistant    *AssistantEvent
    Tool         *ToolEvent
    Done         *Reply
    Notification *TaskNotificationEvent
    Approval     *ApprovalPendingEvent
}

// 第 194-202 行
type RunOptions struct {
    Provider         string
    Model            string
    Mentions         []Mention
    WebSearchEnabled bool
    Locale           string
}
```

### 11.2 EventType 枚举

**第 67-92 行**:

- `EventAssistant` = "assistant"
- `EventToolStart` = "tool_start"
- `EventToolEnd` = "tool_end"
- `EventDone` = "done"
- `EventTaskNotification` = "task_notification"
- `EventApprovalPending` = "approval_pending"

### 11.3 runInternal — for-loop 编排

**第 296-675 行**:

1. 所有权检查 (304-314)
2. @-mention 水合 + 用户消息持久化 (324-341)
3. 加载历史 (344-347)
4. `buildMessages` 构建 LLM 消息数组 (350)
5. **for 循环** (372-645):
   - `a.llm.Chat(ctx, ...)` 调用 LLM
   - 持久化 assistant 消息 + emit `EventAssistant`
   - 无 tool_calls → 返回 Reply
   - 有 tool_calls → 逐个执行: 持久化 pending → emit `EventToolStart` → 执行 → 更新 → emit `EventToolEnd`
6. 超过 MaxIterations → 持久化道歉消息 (653-674)

---

## 12. Biz 层 — graph（ReAct 图）

**文件**: [react.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/react.go)

### 12.1 BuildReActGraph

**第 78-188 行**:

```go
func BuildReActGraph(
    model einomodel.ToolCallingChatModel,
    tools []basetool.BaseTool,
    cfg Config,
) (compose.Runnable[*Input, *Output], error)
```

**拓扑**:
```
START
  ↓
MessageAssembler (lambda) — *Input → []*schema.Message
  ↓
ReActSubgraph (eino react.Agent) — []*schema.Message → *schema.Message
  ↓
OutputProjector (lambda) — *schema.Message → *Output
  ↓
END
```

### 12.2 ReAct 子图内部

```
ChatModel ↔ Branch(tool_calls?) ↔ ToolsNode
  yes → ToolsNode → 回 ChatModel
  no  → END
```

- `MaxStep: cfg.MaxIterations*2 + 2`（第 97 行）
- `UnknownToolsHandler`（第 107-109 行）: LLM 幻觉工具名时返回可恢复 tool-result

### 12.3 assembleMessages

**第 212-248 行**: 将 `*Input` 展开为 eino 消息序列:
1. System message（含语言指令）
2. History（历史回放）
3. `<system-reminder>` 块（per-turn 反漂移注入）
4. User text（含 @-mention 前缀）

### 12.4 buildSystemReminder

**第 304-350 行**: per-turn 反漂移提醒块，包含:
- 基线规则（同工具失败两次换思路、device_id 必须数字等）
- 语言指令
- web_search 关闭提示
- persona `CriticalReminder`
- `DynamicHints`（动态提示）

---

## 13. Callback 链

**文件**: [chain.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/chain.go)

### 13.1 NewDefaultHandlers — 6 handler 有序组装

**第 88-118 行**:

```go
func NewDefaultHandlers(deps Deps) []callbacks.Handler {
    out := make([]callbacks.Handler, 0, 6)
    // 1. AlertDraftGuard (91) — 阻止 model-only 告警规则草稿
    // 2. Persistence (100-103) — 写 chat_messages / chat_tool_calls
    // 3. SSE (104-107) — 流式事件转发
    // 4. Audit (108) — slog 审计
    // 5. Metrics (109) — Prometheus 计数
    // 6. Budget (114-116) — 预算门禁
}
```

### 13.2 assistantIDRelay — 跨 handler 通信

**第 26-45 行, 98 行**:

```go
type assistantIDRelay struct {
    id atomic.Pointer[string]
}
```

- Persistence 的 `OnEnd` 写完 assistant 行后 `relay.store(row.ID)`
- SSE 的 `OnEnd` 读取 `relay.load()` 填入 `assistant_end` 帧的 MessageID
- eino 在同一 goroutine 内按注册序调 OnEnd，atomic 足够

### 13.3 SSE Handler

**文件**: [sse.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/sse.go)

- **OnStart**（第 236-271 行）: ChatModel start → 发 `assistant_start`；Tool start → 发 `tool_start`
- **OnEnd**（第 274-344 行）: ChatModel end → 发 `assistant_end`（带 MessageID）；Tool end → 发 `tool_end`；Graph → 发 `done`
- **OnEndWithStreamOutput**（第 404-440 行）: token 级流式 — 启 goroutine `drainStream`，每个 chunk 发 `assistant_delta`
- **OnError**（第 347-387 行）: Tool 错误发 `tool_end`(error)；其他发 `error` 终端帧

### 13.4 Persistence Handler

**文件**: [persistence.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/persistence.go)

- **OnStart - ChatModel**（第 272-348 行）: `flushIncompleteBatch` 自动愈合上一轮未完成的 tool_call
- **OnEnd - ChatModel**（第 436-503 行 `persistAssistant`）: `AppendMessage` 写 assistant 行 + `relay.store(row.ID)`
- **OnEnd - Tool**（第 728-811 行 `persistToolEnd`）: `UpdateToolCallResult` + `AppendMessage` 写 role=tool 行
- **flushIncompleteBatch**（第 560-622 行）: 下一轮 LLM 调用前补齐上一轮丢失的 tool 响应，防止 provider 400

---

## 14. LLM 调用链

### 14.1 RoutingChatModel

**文件**: [eino_routing.go](file:///d:/claude/ongrid/internal/pkg/llm/eino_routing.go)

**第 89-101 行**:

```go
type RoutingChatModel struct {
    inner           map[string]model.ChatModel  // provider id → ChatModel
    defaultProvider string
    defaultResolver func(ctx) (provider, mdl string)  // 动态默认
}
```

**pick 方法**（第 173-184 行）: 从 `providerOpts` 取 provider，空则用 `defaultProvider`。

**withDynamicDefault**（第 151-170 行）: 未指定 provider 时动态查询当前配置的默认 provider/model。

**Generate / Stream**（第 189-207 行）:

```go
func (r *RoutingChatModel) Generate(ctx, input, opts...) (*schema.Message, error) {
    opts = r.withDynamicDefault(ctx, opts)
    inner, _, err := r.pick(opts...)
    return inner.Generate(ctx, input, opts...)
}
```

### 14.2 clientChatModel — 伪流式

**第 318-394 行**:

```go
type clientChatModel struct {
    inner      Client   // llm.Client (即 MultiClient)
    model      string
    userID     uint64
    boundTools []*schema.ToolInfo
}
```

**Generate**（第 357-368 行）:
1. `buildChatReq` 转换 eino `[]*schema.Message` → `llm.Message`
2. 调 `c.inner.Chat(ctx, req)` — 即 `MultiClient.Chat`
3. `einoMessageFromChatResp` 转回 `*schema.Message`

**Stream — 伪流式**（第 372-378 行）:

```go
func (c *clientChatModel) Stream(ctx, input, opts...) (*schema.StreamReader[*schema.Message], error) {
    msg, err := c.Generate(ctx, input, opts...)  // 先阻塞拿完整响应
    if err != nil { return nil, err }
    return schema.StreamReaderFromArray([]*schema.Message{msg}), nil  // 单 chunk
}
```

### 14.3 MultiClient

**文件**: [router.go](file:///d:/claude/ongrid/internal/pkg/llm/router.go)

**第 67-87 行**:

```go
type MultiClient struct {
    staticSubs  map[string]Client      // 构造时静态 provider
    staticInfos []ProviderInfo
    staticDefID string
    fallback    Client                 // legacy 降级
    resolver   ProvidersResolver       // 动态 catalog 源(DB)
    resolveTTL time.Duration           // 60s
    mu          sync.RWMutex
    dynSubs     map[string]Client      // 动态缓存
    dynInfos    []ProviderInfo
    dynDefID    string
    dynLoadedAt time.Time
    dynActive   bool
}
```

**activeSubs**（第 158-225 行）:
1. 无 resolver → 返回 static 集
2. 缓存有效（`time.Since(loadedAt) < 60s`）→ 返回 dyn 缓存
3. 缓存过期 → 调 `resolver.ResolveProviders(ctx)` 重建

**Chat**（第 284-329 行）:

```go
func (m *MultiClient) Chat(ctx, req) (*ChatResp, error) {
    subs, _, defID, _ := m.activeSubs(ctx)
    id := req.Provider; if id == "" { id = defID }
    return subs[id].Chat(ctx, req)
    // 记录 Prometheus 指标
}
```

### 14.4 Provider 配置

7 个 provider 全部使用 OpenAI-compatible 协议:
- OpenAI / Azure / DeepSeek / Ollama / Zhipu / Moonshot / SiliconFlow
- 通过 `(apiKey, baseURL)` 区分
- 底层用 `sashabaranov/go-openai` 客户端

---

## 15. Data 层（GORM 持久化）

**文件**: [session.go](file:///d:/claude/ongrid/internal/manager/data/aiops/store/session.go)

### 15.1 SessionRepo 结构体

**第 16-18 行**:

```go
type SessionRepo struct {
    db *gorm.DB
}
```

通过 `var _ biz.SessionRepo = (*SessionRepo)(nil)` 编译期保证实现接口。

### 15.2 Session CRUD

| 方法 | 行号 | 说明 |
|------|------|------|
| `CreateSession` | 31-36 | `db.Create(s)` |
| `GetSession` | 39-48 | `Where("id = ?", id).First(&s)`，不存在返回 ErrNotFound |
| `ListSessions` | 72-94 | `Where("user_id = ?").Order("created_at DESC")` |
| `CloseSession` | 99-109 | `Update("closed_at", now)` |
| `RenameSession` | 114-125 | `Updates(map{title, updated_at})` |
| `DeleteSession` | 136-155 | 事务级联删除: tool_calls → messages → session |

### 15.3 Message CRUD

| 方法 | 行号 | 说明 |
|------|------|------|
| `AppendMessage` | 158-163 | `db.Create(m)` |
| `ListMessages` | 174-196 | limit>0 取最近 N 条（DESC 后反转）；limit=0 全部（ASC） |
| `hydrateToolCalls` | 203-230 | 批量查询 `chat_tool_calls` 关联到 assistant 消息 |

### 15.4 ToolCall CRUD

| 方法 | 行号 | 说明 |
|------|------|------|
| `CreateToolCall` | 233-238 | `db.Create(tc)` |
| `UpdateToolCallResult` | 262-284 | `Updates(map{status, ended_at, result_json, error})` |
| `FinalizePendingToolCalls` | 290-311 | 批量标记 pending → error |

### 15.5 SumTokensSince

**第 244-257 行**: 原始 SQL 聚合查询 token 用量。

---

## 16. Model 层（数据模型）

**文件**: [model.go](file:///d:/claude/ongrid/internal/manager/model/aiops/model.go)

### 16.1 Session 模型

**第 49-73 行** — 表名 `chat_sessions`:

| 字段 | 类型 | 说明 |
|------|------|------|
| ID | string | UUIDv4，`primaryKey;type:char(36)` |
| UserID | uint64 | `index;not null` |
| Title | string | `size:256;not null` |
| ScopeJSON | *string | edge 名称白名单 |
| AgentID | *string | persona 名称 |
| ParentSessionID | *string | 父会话（子 agent） |
| RelatedIncidentID | *uint64 | 关联告警 |
| Kind | string | user/investigation |
| ClosedAt | *time.Time | 软关闭 |

`BeforeCreate` 钩子（第 86-91 行）: ID 为空时自动生成 UUIDv4。

### 16.2 Message 模型

**第 99-125 行** — 表名 `chat_messages`:

| 字段 | 类型 | 说明 |
|------|------|------|
| ID | string | UUIDv4 |
| SessionID | string | `index:idx_session_msg` |
| Role | string | user/assistant/tool/system |
| Content | *string | 可空（纯 tool_call 的 assistant） |
| ToolCallID | *string | role=tool 时非空 |
| ToolName | *string | role=tool 时非空 |
| Model | *string | assistant 的 LLM 模型 id |
| PromptTokens | *int | |
| CompletionTokens | *int | |
| ToolCalls | []ToolCall | `gorm:"-"` 瞬态字段 |

### 16.3 ToolCall 模型

**第 143-163 行** — 表名 `chat_tool_calls`:

| 字段 | 类型 | 说明 |
|------|------|------|
| ID | string | UUIDv4 |
| MessageID | string | `index` 关联 assistant 消息 |
| ToolName | string | 工具名 |
| ArgumentsJSON | string | 调用参数 |
| ResultJSON | *string | 执行结果 |
| Status | string | pending/success/error/timeout |
| Error | *string | 错误信息 |
| StartedAt | time.Time | |
| EndedAt | *time.Time | |
| DeviceID | *uint64 | |
| LLMCallID | *string | LLM call id（历史回放配对用） |

---

## 17. 端到端数据流总览

### 17.1 创建会话流

```
用户点击 "New Chat"
  ↓
createSession (chat.ts:80) → request POST /chat/sessions
  ↓ client.ts:request → 注入 Bearer token
  ↓
HTTP POST /api/v1/chat/sessions
  ↓ otelhttpmw → Metrics → Audit → auth.Middleware(JWT verify → tenantctx)
  ↓
aiopsHandler.createSession (http.go:307)
  ↓ callerFromCtx → svc.Caller{UserID, Role}
  ↓
svc.Service.CreateSession (service.go:204)
  ↓ trim title, 构造 model.Session
  ↓
biz.SessionRepo.CreateSession (repo.go:28)
  ↓
data.SessionRepo.CreateSession (session.go:31)
  ↓ GORM db.Create → BeforeCreate 生成 UUID
  ↓ INSERT INTO chat_sessions
  ← 返回 *model.Session
  ← HTTP 201 + sessionDTO
  ← 前端 navigate(`/chat/${session.id}`)
```

### 17.2 发送消息流（SSE 流式）

```
用户在 ChatInput 输入文本，按 Enter
  ↓
ChatInput.submit (ChatInput.tsx:251)
  ↓ onSubmit({text, mentions})
  ↓
ChatThread.send (ChatThread.tsx:217)
  ↓ 乐观渲染 user 气泡
  ↓ streamMessage (chat.ts:252)
  ↓ fetch POST /api/v1/chat/sessions/{id}/messages/stream
  ↓   headers: Authorization: Bearer {token}
  ↓   body: {content, provider, model, mentions, web_search_enabled, locale}
  ↓
HTTP POST /api/v1/chat/sessions/{id}/messages/stream
  ↓ otelhttpmw → Metrics → Audit → auth.Middleware
  ↓
aiopsHandler.postMessageStream (http.go:414)
  ↓ SSE 握手: Content-Type: text/event-stream, X-Accel-Buffering: no
  ↓ 心跳帧 ": ok\n\n" + Flush
  ↓ emit 闭包: func(e agent.Event) { writeSSE(w, flusher, eventName, eventPayload) }
  ↓
svc.Service.PostMessageStreamWithOpts (service.go:319)
  ↓ runWithKernel (service.go:334)
  ↓   1. 内容校验
  ↓   2. 所有权检查: GetSession → 非 owner 返回 ErrNotFound
  ↓   3. HLD-021: ctx = context.WithoutCancel(ctx)  ← 浏览器刷新不杀 turn
  ↓   4. ctx, cancel = context.WithCancel(ctx) + registerCancel
  ↓   5. kernel == graph → runGraph
  ↓
svc.Service.runGraph (service.go:434)
  ↓ 构造 chatruntime.Emit (含 translateRuntimeEvent)
  ↓ 构造 chatruntime.Request
  ↓
chatruntime.Runtime.Handle (runtime.go:468)
  ↓ Step 1: GetSession + 所有权检查
  ↓ Step 2: MentionResolver.Resolve → @-mention 水合
  ↓ Step 3: AppendMessage (用户消息持久化)
  ↓ Step 4: ListMessages (加载历史, limit=50)
  ↓ Step 5: SkillRegistry.Resolve + persona + 工具过滤 + ComposeSystemPrompt
  ↓ Step 6: buildEinoHistory (含 HOIST 机制)
  ↓ Step 7: graph.BuildReActGraph(chatModel, tools, cfg)
  ↓ Step 8: callbacks.NewDefaultHandlers(deps) — 6 handler 链
  ↓ Step 9: calcDynamicHints (重复检测、承诺未执行)
  ↓ Step 10: g.Invoke(ctx, &graph.Input{...})
  ↓
eino Graph 执行:
  MessageAssembler → ReActSubgraph → OutputProjector
    ↓
  ReActSubgraph 内部循环:
    ChatModel.Generate(ctx, messages, opts)
      ↓
    RoutingChatModel.Generate (eino_routing.go:189)
      ↓ withDynamicDefault → pick provider
      ↓
    clientChatModel.Generate (eino_routing.go:357)
      ↓ buildChatReq → llm.Message
      ↓
    MultiClient.Chat (router.go:284)
      ↓ activeSubs → 60s TTL 缓存 / resolver.ResolveProviders
      ↓ subs[providerID].Chat(ctx, req)
      ↓ sashabaranov/go-openai → HTTP POST {baseURL}/chat/completions
      ← ChatResp{Content, ToolCalls, Usage}
      ← Prometheus 指标记录
    ← *schema.Message
    ↓
  Branch(tool_calls?):
    yes → ToolsNode 执行工具 → 回 ChatModel
    no  → END
  ↓
  Callback 链（每步触发）:
    Persistence: AppendMessage(assistant) + CreateToolCall + UpdateToolCallResult
    SSE: emit assistant / tool_start / tool_end / done
    Audit: slog 审计日志
    Metrics: Prometheus 计数
    Budget: token 预算检查
  ↓
  emit EventDone → writeSSE → "event: done\ndata: {reply}\n\n"
  ↓
chatruntime.Runtime.Handle 返回 *Reply
  ↓ runtimeReplyToAgentReply
  ↓
svc.Service.runGraph 返回
  ↓ unregisterCancel
  ↓
aiopsHandler.postMessageStream 返回
  ↓ 若未发 done，补发 summary 帧
  ↓
HTTP SSE 响应流关闭
  ↓
前端 streamMessage reader.read() done=true
  ↓ flush 残留帧
  ↓ Promise resolve
  ↓
ChatThread.send finally:
  setSubmitting(false)
  abortRef.current = null
  ↓
ChatInput 重新可用
```

### 17.3 Esc 停止流

```
用户按 Esc
  ↓
ChatThread keydown listener (ChatThread.tsx:423)
  ↓ submittingRef.current === true
  ↓
stopSession(sessionId) (chat.ts:101)
  ↓ request POST /chat/sessions/{id}/stop
  ↓
HTTP POST /api/v1/chat/sessions/{id}/stop
  ↓ auth.Middleware
  ↓
aiopsHandler.stopSession (http.go:646)
  ↓
svc.Service.StopSession (service.go:406)
  ↓ 所有权检查
  ↓ cancels[sessionID] 取出 CancelFunc + delete
  ↓ cancel() — 取消 context
  ↓
  Graph 收到 ctx.Done() → 软降级
  ↓ 部分状态仍持久化（Persistence handler 的 defer）
  ↓
  ← {stopped: true}
  ↓
同时: abortRef.current.abort()
  ↓ fetch AbortError
  ↓ streamMessage throw AbortError
  ↓ ChatThread.send catch: AbortError 静默返回
  ↓ setSubmitting(false)
```

### 17.4 历史消息加载流

```
ChatThread mount / sessionId 变化
  ↓
useEffect (ChatThread.tsx:107)
  ↓ refetch(true)
  ↓
getMessages(sessionId) (chat.ts:108)
  ↓ request GET /chat/sessions/{id}/messages
  ↓
HTTP GET /api/v1/chat/sessions/{id}/messages
  ↓ auth.Middleware
  ↓
aiopsHandler.listMessages (http.go:614)
  ↓
svc.Service.ListMessages (service.go:256)
  ↓ GetSession (所有权检查)
  ↓ sessions.ListMessages(ctx, sessionID, 0)
  ↓
data.SessionRepo.ListMessages (session.go:174)
  ↓ SELECT * FROM chat_messages WHERE session_id = ? ORDER BY created_at ASC
  ↓ hydrateToolCalls: SELECT * FROM chat_tool_calls WHERE message_id IN (?)
  ← []*model.Message (含 ToolCalls)
  ← HTTP 200 + listMessagesResp
  ← 前端 setMessages
  ↓
MessageBubble 渲染:
  user → UserBubble
  assistant → AssistantBubble (ReactMarkdown)
  tool → ToolBubble
  tool_card → ToolCallSummaryBlock
```

---

## 18. 关键设计要点

### 18.1 HLD-021 上下文分离

**文件**: [service.go](file:///d:/claude/ongrid/internal/manager/service/aiops/service.go#L353) 第 353 行

```go
ctx = context.WithoutCancel(ctx)
```

将 chat turn 从 HTTP 请求生命周期分离。浏览器刷新 / SSE 断连不会取消正在运行的 turn（如等待人工审批的 cloud_bash）。通过 `context.WithCancel` + `cancels map` 支持显式 Esc 停止。

### 18.2 双 Kernel 架构

Legacy（for-loop）与 Graph（eino ReAct）并存，通过 `ONGRID_AGENT_KERNEL` 环境变量切换。`translateRuntimeEvent` 保证两个 kernel 产出字节级相同的 SSE 帧，SPA 无需改动。

### 18.3 所有权模型

每个 session 有单一 owner（`user_id`）。非 owner 非 admin 返回 `ErrNotFound`（非 `ErrForbidden`），避免泄露 session 存在性。

### 18.4 assistantIDRelay 跨 handler 通信

**文件**: [chain.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/chain.go#L26) 第 26-45 行

`atomic.Pointer[string]` 在 Persistence 和 SSE handler 间传递持久化的 message ID。eino 在同一 goroutine 内按注册序调 OnEnd，atomic 足够无需额外同步。

### 18.5 伪流式

**文件**: [eino_routing.go](file:///d:/claude/ongrid/internal/pkg/llm/eino_routing.go#L372) 第 372-378 行

`clientChatModel.Stream` 包装 `Generate` 为单 chunk StreamReader。真 token 级流式待后续 PR。`assistant_delta` 帧当前被丢弃。

### 18.6 自动愈合机制

**文件**: [persistence.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/persistence.go#L560) 第 560-622 行

`flushIncompleteBatch` 在下一轮 LLM 调用前补齐上一轮丢失的 tool 响应，防止 provider 400。

### 18.7 动态 Provider 路由

**文件**: [router.go](file:///d:/claude/ongrid/internal/pkg/llm/router.go#L158) 第 158-225 行

`MultiClient` 60s TTL 缓存 + `ProvidersResolver` 动态解析，支持运行时热切换默认 provider。管理员保存 LLM 配置后调 `Invalidate` 强制刷新。

### 18.8 per-turn 反漂移

**文件**: [react.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/react.go#L304) 第 304-350 行

`<system-reminder>` 块每轮注入，防止长会话注意力漂移。包含基线规则、语言指令、persona 提醒、动态提示。

### 18.9 历史回放 HOIST 机制

Tool 响应按 `tool_call_id` 提升到父 assistant 的 `tool_calls` 之后，满足严格 provider（DeepSeek v4+）的 "tool messages must immediately follow tool_calls" 约束。

### 18.10 接口在消费方定义

`SessionRepo` 在 `biz/aiops/repo.go` 定义，`data/aiops/store/session.go` 实现，符合依赖倒置原则。

---

## 附录：关键文件索引

| 文件 | 作用 | 关键行号 |
|------|------|----------|
| [App.tsx](file:///d:/claude/ongrid/web/src/App.tsx) | 前端路由声明 | 8, 98 |
| [ChatThread.tsx](file:///d:/claude/ongrid/web/src/pages/ChatThread.tsx) | 页面组件 | 28, 107, 217, 423 |
| [chat.ts](file:///d:/claude/ongrid/web/src/api/chat.ts) | 前端 API 层 | 108, 252, 319 |
| [client.ts](file:///d:/claude/ongrid/web/src/api/client.ts) | API client 封装 | 27, 117 |
| [ChatInput.tsx](file:///d:/claude/ongrid/web/src/components/ChatInput.tsx) | 输入框组件 | 77, 251 |
| [MessageBubble.tsx](file:///d:/claude/ongrid/web/src/components/MessageBubble.tsx) | 消息气泡组件 | 36, 141, 485 |
| [auth.ts](file:///d:/claude/ongrid/web/src/store/auth.ts) | Auth store | 20, 43 |
| [http.go](file:///d:/claude/ongrid/internal/manager/server/aiops/http.go) | HTTP handler + SSE | 41, 131, 414, 601 |
| [main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) | 路由挂载 + DI | 2718, 2780, 2850 |
| [middleware.go](file:///d:/claude/ongrid/internal/pkg/auth/middleware.go) | JWT auth 中间件 | 21 |
| [tenantctx.go](file:///d:/claude/ongrid/internal/pkg/tenantctx/tenantctx.go) | 请求级身份传递 | — |
| [service.go](file:///d:/claude/ongrid/internal/manager/service/aiops/service.go) | Service 层 | 73, 204, 334, 434, 406 |
| [runtime.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go) | Graph kernel | 292, 468, 859 |
| [agent.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/agent/agent.go) | Legacy kernel | 55, 96, 160, 296 |
| [react.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/react.go) | ReAct 图 | 78, 212, 304 |
| [chain.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/chain.go) | Callback 链 | 26, 88 |
| [sse.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/sse.go) | SSE callback | 22, 236, 274 |
| [persistence.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/persistence.go) | 持久化 callback | 272, 436, 560 |
| [eino_routing.go](file:///d:/claude/ongrid/internal/pkg/llm/eino_routing.go) | LLM 路由 | 89, 189, 318, 372 |
| [router.go](file:///d:/claude/ongrid/internal/pkg/llm/router.go) | MultiClient | 67, 158, 284 |
| [session.go](file:///d:/claude/ongrid/internal/manager/data/aiops/store/session.go) | GORM 持久化 | 16, 31, 158, 174 |
| [model.go](file:///d:/claude/ongrid/internal/manager/model/aiops/model.go) | 数据模型 | 49, 99, 143 |
| [repo.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/repo.go) | SessionRepo 接口 | 26 |
