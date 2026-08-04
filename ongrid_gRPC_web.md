# OnGrid gRPC 格式 API 与 Web 端交互分析

> 本文档分析 OnGrid 项目中 gRPC 格式 API 的定义、实际传输方式、以及 Web 前端如何消费这些 API。
> 生成时间：2026-08-03

---

## 目录

1. [核心结论](#1-核心结论)
2. [Proto 文件定义全览](#2-proto-文件定义全览)
3. [Proto 代码生成工具链](#3-proto-代码生成工具链)
4. [gRPC 在 OnGrid 中的实际角色](#4-grpc-在-ongrid-中的实际角色)
5. [HTTP 路由层：chi 手写路由](#5-http-路由层chi-手写路由)
6. [流式 RPC → SSE：StreamMessage 的实现](#6-流式-rpc--ssestreammessage-的实现)
7. [前端 API 调用方式](#7-前端-api-调用方式)
8. [SSE 流式消费机制](#8-sse-流式消费机制)
9. [WebSocket 通信](#9-websocket-通信)
10. [边云隧道：geminio 替代 gRPC](#10-边云隧道geminio-替代-grpc)
11. [架构全景图](#11-架构全景图)
12. [Proto 与 HTTP 端点映射表](#12-proto-与-http-端点映射表)

---

## 1. 核心结论

**OnGrid 当前不使用 gRPC 服务端，也不使用 gRPC-Gateway。** Proto 定义仅用于生成 Go 消息类型（DTO），HTTP API 全部手写于 chi 路由。

关键事实：

| 维度 | 状态 |
|------|------|
| gRPC 服务端 | **不存在** — 无 `grpc.NewServer`、无 `RegisterXxxServer` 调用 |
| gRPC-Gateway | **不使用** — `api/README.md` 明确声明 "no grpc-gateway in MVP" |
| grpc-web / connectrpc | **不使用** — 前端无任何 protobuf/gRPC 相关依赖 |
| Proto 文件 | 9 个 `.proto` 文件定义了 7 个 service、43 个 RPC |
| 代码生成 | `buf generate` 生成 `.pb.go` + `_grpc.pb.go`，但 `api/gen/` 被 `.gitignore` |
| 实际传输 | **REST JSON over HTTP**（chi 路由）+ **SSE**（流式）+ **WebSocket**（终端） |
| 边云通信 | **geminio**（JSON over TCP），非标准 gRPC |

---

## 2. Proto 文件定义全览

### 2.1 文件清单

| 文件 | 包 | Service | RPC 数量 |
|------|-----|---------|----------|
| `api/iam/v1/iam.proto` | `ongrid.iam.v1` | `IamService` | 9 |
| `api/manager/edge/v1/edge.proto` | `ongrid.manager.edge.v1` | `EdgeService` | 5 |
| `api/manager/k8s/v1/k8s.proto` | `ongrid.manager.k8s.v1` | `KubernetesService` | 12 |
| `api/manager/metric/v1/metric.proto` | `ongrid.manager.metric.v1` | `MetricService` | 1 |
| `api/manager/aiops/v1/aiops.proto` | `ongrid.manager.aiops.v1` | `AiopsService` | 4（含 1 个 server-streaming） |
| `api/manager/alert/v1/alert.proto` | `ongrid.manager.alert.v1` | `AlertService` | 4 |
| `api/manager/notification/v1/notification.proto` | `ongrid.manager.notification.v1` | `NotificationService` | 6 |
| `api/manager/setting/v1/setting.proto` | `ongrid.manager.setting.v1` | `SettingService` | 2 |
| `api/tunnel/v1/tunnel.proto` | `ongrid.tunnel.v1` | **无 service**（纯消息定义） | 0 |

**总计：7 个 service、43 个 RPC。**

### 2.2 各 Service 的 RPC 定义

#### IamService（9 个 Unary RPC）

```
rpc Register(RegisterRequest) returns (RegisterResponse);
rpc Login(LoginRequest) returns (LoginResponse);
rpc Refresh(RefreshRequest) returns (RefreshResponse);
rpc GetSelf(GetSelfRequest) returns (GetSelfResponse);
rpc CreateOrg(CreateOrgRequest) returns (CreateOrgResponse);
rpc ListOrgs(ListOrgsRequest) returns (ListOrgsResponse);
rpc InviteMember(InviteMemberRequest) returns (InviteMemberResponse);
rpc ListMembers(ListMembersRequest) returns (ListMembersResponse);
rpc SwitchOrg(SwitchOrgRequest) returns (SwitchOrgResponse);
```

#### EdgeService（5 个 Unary RPC）

```
rpc CreateEdge(CreateEdgeRequest) returns (CreateEdgeResponse);
rpc ListEdges(ListEdgesRequest) returns (ListEdgesResponse);
rpc GetEdge(GetEdgeRequest) returns (GetEdgeResponse);
rpc DeleteEdge(DeleteEdgeRequest) returns (google.protobuf.Empty);
rpc RotateSecret(RotateSecretRequest) returns (RotateSecretResponse);
```

#### KubernetesService（12 个 Unary RPC）

```
rpc CreateCluster(CreateClusterRequest) returns (CreateClusterResponse);
rpc ListClusters(ListClustersRequest) returns (ListClustersResponse);
rpc GetCluster(GetClusterRequest) returns (GetClusterResponse);
rpc GetClusterHealth(GetClusterHealthRequest) returns (GetClusterHealthResponse);
rpc ListEdgeAttachments(ListEdgeAttachmentsRequest) returns (ListEdgeAttachmentsResponse);
rpc ListNodes(ListNodesRequest) returns (ListNodesResponse);
rpc ListWorkloads(ListWorkloadsRequest) returns (ListWorkloadsResponse);
rpc ListPods(ListPodsRequest) returns (ListPodsResponse);
rpc ListEvents(ListEventsRequest) returns (ListEventsResponse);
rpc RotateBootstrapToken(RotateBootstrapTokenRequest) returns (RotateBootstrapTokenResponse);
rpc DeleteCluster(DeleteClusterRequest) returns (DeleteClusterResponse);
rpc Enroll(EnrollRequest) returns (EnrollResponse);
```

#### MetricService（1 个 Unary RPC）

```
rpc QueryHostMetrics(QueryHostMetricsRequest) returns (QueryHostMetricsResponse);
```

#### AiopsService（3 个 Unary + 1 个 Server-Streaming RPC）

```
rpc CreateChatSession(CreateChatSessionRequest) returns (CreateChatSessionResponse);
rpc ListChatSessions(ListChatSessionsRequest) returns (ListChatSessionsResponse);
rpc PostMessage(PostMessageRequest) returns (PostMessageResponse);
rpc StreamMessage(PostMessageRequest) returns (stream StreamChunk);  // 唯一的流式 RPC
rpc ListMessages(ListMessagesRequest) returns (ListMessagesResponse);
```

#### AlertService（4 个 Unary RPC）

```
rpc ListIncidents(ListIncidentsRequest) returns (ListIncidentsResponse);
rpc GetIncident(GetIncidentRequest) returns (GetIncidentResponse);
rpc AcknowledgeIncident(AcknowledgeIncidentRequest) returns (AcknowledgeIncidentResponse);
rpc ResolveIncident(ResolveIncidentRequest) returns (ResolveIncidentResponse);
```

#### NotificationService（6 个 Unary RPC）

```
rpc ListChannels(ListChannelsRequest) returns (ListChannelsResponse);
rpc GetChannel(GetChannelRequest) returns (GetChannelResponse);
rpc CreateChannel(CreateChannelRequest) returns (CreateChannelResponse);
rpc UpdateChannel(UpdateChannelRequest) returns (UpdateChannelResponse);
rpc DeleteChannel(DeleteChannelRequest) returns (google.protobuf.Empty);
rpc TestChannel(TestChannelRequest) returns (TestChannelResponse);
```

#### SettingService（2 个 Unary RPC）

```
rpc ValidateLLMConfiguration(ValidateLLMConfigurationRequest) returns (ValidateLLMConfigurationResponse);
rpc ValidateAndSaveLLMConfiguration(ValidateAndSaveLLMConfigurationRequest) returns (ValidateAndSaveLLMConfigurationResponse);
```

### 2.3 StreamMessage 的 Proto 定义

`api/manager/aiops/v1/aiops.proto` 第 25 行定义了唯一的 server-streaming RPC：

```protobuf
rpc StreamMessage(PostMessageRequest) returns (stream StreamChunk);
```

`StreamChunk` 使用 `oneof` 定义四种互斥形态：

```protobuf
message StreamChunk {
    oneof payload {
        ContentDelta content_delta = 1;      // 新增一截 assistant 文本
        ToolCallStart tool_call_start = 2;   // LLM 宣布要调用一个 tool
        ToolCallResult tool_call_result = 3; // tool 执行结果已就绪
        Done done = 4;                       // 整个会话收敛完毕
    }
}
```

**注意**：Proto 中定义了 `ContentDelta`（token-by-token 增量输出），但当前 SSE 实现中 **不发送 `content_delta` 帧**，只发送完整的 `assistant` 帧（每轮 assistant turn 完成后一次性推送）。

### 2.4 tunnel.proto — 无 Service 定义

`tunnel.proto` 头部注释明确声明：

> These messages are transported over a geminio tunnel (not gRPC). Encoded as JSON on the wire in MVP (see ADR-001); may switch to proto binary in Phase 2.
>
> No gRPC service is declared here — cloud and edge register handlers by method name on geminio End objects; these messages are just the request / response body shapes.

隧道消息通过 geminio 方法名注册（如 `register_edge`、`heartbeat`、`push_host_metrics`、`get_host_load`、`bash_exec` 等），序列化为 JSON 传输。

---

## 3. Proto 代码生成工具链

### 3.1 buf 配置

**`api/buf.yaml`**：

```yaml
version: v2
modules:
  - path: .
lint:
  use:
    - STANDARD
breaking:
  use:
    - FILE
```

**`api/buf.gen.yaml`**：

```yaml
version: v2
plugins:
  - remote: buf.build/protocolbuffers/go:v1.34.2
    out: gen
    opt:
      - paths=source_relative
  - remote: buf.build/grpc/go:v1.5.1
    out: gen
    opt:
      - paths=source_relative
      - require_unimplemented_servers=true
```

**关键发现**：只配置了 `protoc-gen-go`（消息类型）和 `protoc-gen-go-grpc`（gRPC 服务桩），**没有** `protoc-gen-grpc-gateway` 插件。即使执行 `buf generate`，也不会生成 gRPC-Gateway 的 `xxx.pb.gw.go` 文件。

### 3.2 Makefile 生成命令

`Makefile` 第 115-131 行：

```makefile
proto: ## [api] 重新生成 proto（优先 buf，回退 protoc + protoc-gen-go/grpc）
	@if command -v buf >/dev/null 2>&1; then \
		cd api && buf generate; \
	else \
		cd api && protoc --proto_path=. \
			--go_out=gen --go_opt=paths=source_relative \
			--go-grpc_out=gen --go-grpc_opt=paths=source_relative \
			--go-grpc_opt=require_unimplemented_servers=true \
			frontierbound/v1/frontierbound.proto; \
	fi
```

### 3.3 生成产物

- 生成目录：`api/gen/`，被 `.gitignore` 忽略
- `api/README.md` 第 19 行：`gen/  generated, .gitignored`
- 当前 `api/gen/` 目录为空（未执行过 `buf generate` 或已清理）
- Proto 文件中 **没有** `import "google/api/annotations.proto"` 或 `option (google.api.http) = ...` 注解，因此 gRPC-Gateway 无法从 proto 生成 HTTP 路由映射

---

## 4. gRPC 在 OnGrid 中的实际角色

### 4.1 依赖状态

| 依赖 | 版本 | 状态 | 说明 |
|------|------|------|------|
| `google.golang.org/grpc` | v1.80.0 | `// indirect` | 被 `singchia/geminio` 间接引入 |
| `github.com/grpc-ecosystem/grpc-gateway/v2` | v2.28.0 | `// indirect` | 被间接引入，未直接使用 |

### 4.2 Proto 的实际用途

Proto 在 OnGrid 中扮演 **类型契约（Type Contract）** 角色：

1. **Go 结构体生成**：`buf generate` 生成 `.pb.go` 文件，提供强类型 DTO
2. **API 契约文档**：Proto 文件本身就是 API 的权威定义（Single Source of Truth）
3. **CI 破坏性变更检测**：`buf breaking` 在 CI 中检测 API 兼容性
4. **未来 gRPC 迁移基础**：当需要引入 gRPC 传输时，Service 定义已就绪

**Proto 不参与**：
- 运行时 gRPC 传输
- gRPC-Gateway HTTP 路由生成
- 前端 protobuf 序列化/反序列化

### 4.3 api/README.md 的明确声明

> REST routes are hand-written (`internal/*/server/`, chi) against these Go types — **no grpc-gateway in MVP**.

---

## 5. HTTP 路由层：chi 手写路由

### 5.1 路由注册入口

`cmd/ongrid/main.go` 中的核心路由组装：

```go
mux := chi.NewRouter()
// ... 中间件 ...
mux.Route("/api", func(api chi.Router) {
    // 公开路由
    iamHandler.RegisterPublic(api)
    // 认证保护路由
    protected.Group(func(protected chi.Router) {
        edgeHandler.Register(protected)
        k8sHandler.RegisterProtected(protected)
        aiopsHandler.Register(protected)
        alertHandler.Register(protected)
        // ... 等等
    })
})
```

### 5.2 各域路由文件

每个子域在 `internal/manager/server/<domain>/http.go` 中手写路由：

| 域 | 路由文件 | 路由前缀 |
|-----|---------|----------|
| IAM | `internal/iam/server/http.go` | `/api/v1/auth/*`, `/api/v1/orgs/*` |
| Aiops | `internal/manager/server/aiops/http.go` | `/api/v1/chat/*` |
| Edge | `internal/manager/server/edge/http.go` | `/api/v1/edges/*` |
| K8s | `internal/manager/server/k8s/http.go` | `/api/v1/kubernetes/*` |
| Alert | `internal/manager/server/alert/http.go` | `/api/v1/alerts/*` |
| Notification | `internal/manager/server/notification/http.go` | `/api/v1/notifications/*` |
| Setting | `internal/manager/server/setting/http.go` | `/api/v1/system-settings/*`, `/api/v1/integrations/*` |
| Metric | `internal/manager/server/metric/http.go` | `/api/v1/metrics/*` |

### 5.3 Aiops 路由详情

`internal/manager/server/aiops/http.go` 的路由注册：

```go
func (h *Handler) Register(r chi.Router) {
    r.Route("/v1/chat", func(chat chi.Router) {
        // 会话 CRUD
        chat.Post("/sessions", h.createSession)
        chat.Get("/sessions", h.listSessions)
        chat.Get("/sessions/{id}", h.getSession)
        chat.Delete("/sessions/{id}", h.deleteSession)
        chat.Patch("/sessions/{id}", h.renameSession)

        // 消息
        chat.Post("/sessions/{id}/messages", h.postMessage)        // 阻塞式
        chat.Post("/sessions/{id}/messages/stream", h.postMessageStream) // SSE 流式
        chat.Get("/sessions/{id}/messages", h.listMessages)

        // 停止
        chat.Post("/sessions/{id}/stop", h.stopSession)

        // @-提及搜索
        chat.Get("/aiops/mentions/search", h.searchMentions)

        // 模型目录
        chat.Get("/aiops/models", h.listModels)
    })
}
```

---

## 6. 流式 RPC → SSE：StreamMessage 的实现

### 6.1 Proto 定义 vs 实际实现

| 维度 | Proto 定义 | 实际实现 |
|------|-----------|---------|
| 传输协议 | gRPC server-streaming | SSE over HTTP/1.1 |
| 端点 | `StreamMessage` RPC | `POST /api/v1/chat/sessions/{id}/messages/stream` |
| 请求格式 | Protobuf `PostMessageRequest` | JSON `{ content, provider?, model?, mentions?, web_search_enabled?, locale? }` |
| 响应格式 | Protobuf `stream StreamChunk` | SSE 帧 `event: <type>\ndata: <json>\n\n` |
| 帧类型 | `content_delta` / `tool_call_start` / `tool_call_result` / `done` | `assistant` / `tool_start` / `tool_end` / `done` / `error` / `task_notification` / `approval_pending` |

**关键差异**：Proto 定义了 `ContentDelta`（token-by-token 增量），但 SSE 实现发送的是 `assistant`（每轮 turn 完成后的完整内容），当前是"伪流式"。

### 6.2 后端 SSE 写入

`internal/manager/server/aiops/http.go` 第 414-477 行：

```go
func (h *Handler) postMessageStream(w http.ResponseWriter, r *http.Request) {
    // 1. 解析请求
    // 2. 检查 ResponseWriter 是否支持 Flusher
    flusher, ok := w.(http.Flusher)
    if !ok {
        // 无流式支持 → 回退到阻塞 JSON
        reply, err := h.svc.PostMessageWithOpts(...)
        writeJSON(w, http.StatusOK, toPostMessageResp(id, reply))
        return
    }

    // 3. 写 SSE 头
    w.Header().Set("Content-Type", "text/event-stream")
    w.Header().Set("Cache-Control", "no-cache")
    w.Header().Set("X-Accel-Buffering", "no")  // 禁用 nginx 缓冲
    w.WriteHeader(http.StatusOK)

    // 4. 心跳帧
    _, _ = w.Write([]byte(": ok\n\n"))
    flusher.Flush()

    // 5. 定义 emit 回调
    emit := func(e agent.Event) {
        writeSSE(w, flusher, eventName(e.Type), eventPayload(id, e))
    }

    // 6. 调用 Service 层流式方法
    reply, err := h.svc.PostMessageStreamWithOpts(r.Context(), caller, id, req.Content, emit, opts)
    // 7. 错误处理 + 兜底 summary 帧
}
```

### 6.3 writeSSE 函数

`internal/manager/server/aiops/http.go` 第 601-612 行：

```go
func writeSSE(w http.ResponseWriter, f http.Flusher, name string, payload any) {
    body, err := json.Marshal(payload)
    if err != nil {
        body = []byte(`{}`)
    }
    _, _ = w.Write([]byte("event: "))
    _, _ = w.Write([]byte(name))
    _, _ = w.Write([]byte("\ndata: "))
    _, _ = w.Write(body)
    _, _ = w.Write([]byte("\n\n"))
    f.Flush()
}
```

关键设计：
- 错误静默吞掉（`_, _`），因为客户端断开后无法恢复
- 每帧立即 `Flush()`，确保浏览器能增量渲染
- JSON 序列化失败时回退为 `{}`

### 6.4 SSE 事件类型映射

| agent.EventType | SSE event 名 | 数据内容 |
|-----------------|-------------|---------|
| `EventAssistant` | `assistant` | iteration, message_id, content, created_at, pending_tool_calls |
| `EventToolStart` | `tool_start` | tool_call_id, name, status, started_at, arguments |
| `EventToolEnd` | `tool_end` | tool_call_id, name, status, ended_at, duration_ms, result, error |
| `EventDone` | `done` | 完整 PostMessageResponse |
| `EventTaskNotification` | `task_notification` | task_id, status, summary, result |
| `EventApprovalPending` | `approval_pending` | approval_id, tool_call_id, kind, command, credentials |
| 错误 | `error` | error, code |

### 6.5 eventPayload 的 JSON 序列化策略

`internal/manager/server/aiops/http.go` 第 498-596 行：

- **arguments**：先尝试 `json.Unmarshal` 解析为 JSON 对象，成功则发送 `arguments`（parsed），失败则发送 `arguments_raw`（原始字符串）
- **result**：同上策略，`result`（parsed）或 `result_raw`（原始字符串）
- **device_id**：映射为 `edge_id`（保持前端字段名一致）

---

## 7. 前端 API 调用方式

### 7.1 HTTP 客户端

`web/src/api/client.ts` 封装了通用 `request<T>()` 函数：

```typescript
async function request<T>(method: string, path: string, body?: unknown): Promise<T>
```

- **基础路径**：`/api/v1`
- **认证**：从 zustand store 获取 JWT token，通过 `Authorization: Bearer <token>` 传递
- **Token 刷新**：401 响应时自动调用 `refreshAccessToken()`，通过 `POST /api/v1/auth/refresh` 换取新 token 并重试原请求（仅重试一次）
- **请求体**：默认 JSON 序列化；`FormData` 场景让浏览器自动设置 `multipart/form-data`
- **错误处理**：自定义 `ApiError` 类，携带 HTTP status、后端 code、payload
- **语言偏好**：每个请求带 `Accept-Language` 头

### 7.2 Vite 开发代理

`web/vite.config.ts` 将 `/api` 前缀代理到后端（当前指向 `http://172.31.84.217:8090`）。

### 7.3 无任何 Protobuf / gRPC-web 代码

- `package.json` 中无 `@bufbuild/*`、`connectrpc/*`、`grpc-web`、`@grpc/*`、`protobufjs` 等依赖
- `web` 目录下无 `.proto` 文件
- 无 `*.pb.js`、`*.pb.ts` 等生成代码
- 前端与后端之间 **完全通过 REST JSON API 通信**

### 7.4 API 客户端文件一览

`web/src/api/` 目录下共 35 个文件，全部基于 `client.ts` 的 `request()` 函数封装：

| 文件 | 端点前缀 | 说明 |
|------|---------|------|
| `chat.ts` | `/chat/sessions` | 聊天会话 CRUD + SSE 流式 |
| `agents.ts` | `/agents` | Agent 管理 |
| `alerts.ts` | `/alerts` | 告警/事件 |
| `devices.ts` | `/devices` | 设备管理 |
| `edges.ts` | `/edges` | 边缘节点 |
| `flows.ts` | `/flows` | 工作流 |
| `knowledge.ts` | `/knowledge` | 知识库（含文件上传） |
| `settings.ts` | `/system-settings` + `/integrations` | 系统设置 + 集成测试 |
| `webshell.ts` | `/devices/{id}/shell` (WS) | WebSocket 终端 |
| 其他 | 各自领域 | approvals, audit, auth, grafana, kubernetes, mcp, monitorPanels, orgs, prom, prometheus, reports, secrets, skills, systemHealth, systemUpgrade, tasks, topology, traces, users, version, imbridge, marketplace, logs, pages, aiops |

---

## 8. SSE 流式消费机制

### 8.1 streamMessage 函数

`web/src/api/chat.ts` 第 252-317 行：

```typescript
export async function streamMessage(
  sessionId: string | number,
  content: string,
  cbs: StreamCallbacks,
  opts: SendOptions = {},
  signal?: AbortSignal,
): Promise<void>
```

**为什么不使用浏览器原生 `EventSource`**：
- `EventSource` 只支持 `GET` 请求，而 OnGrid 的 SSE 端点是 `POST`
- `EventSource` 不支持自定义请求头（无法传 `Authorization: Bearer`）
- 因此使用 `fetch` + `ReadableStream` 手动解析

### 8.2 SSE 帧解析流程

```
1. fetch POST → 获取 Response
2. res.body.getReader() → ReadableStreamDefaultReader
3. TextDecoder 解码二进制块
4. 以 \n\n 为分隔符拆分 SSE 帧
5. 对每个帧解析 event: 和 data: 行
6. data: 内容用 JSON.parse 解析
7. 按 event 类型分发到对应回调
```

### 8.3 帧解析代码

`web/src/api/chat.ts` 第 319-358 行：

```typescript
function dispatchFrame(raw: string, cbs: StreamCallbacks) {
  let event = 'message';
  const dataLines: string[] = [];
  for (const line of raw.split('\n')) {
    if (!line || line.startsWith(':')) continue;   // 跳过空行和注释
    if (line.startsWith('event:')) event = line.slice(6).trim();
    else if (line.startsWith('data:')) dataLines.push(line.slice(5).trim());
  }
  if (dataLines.length === 0) return;
  let payload: unknown;
  try { payload = JSON.parse(dataLines.join('\n')); } catch { return; }
  switch (event) {
    case 'assistant':       cbs.onAssistant?.(payload); break;
    case 'tool_start':      cbs.onToolStart?.(payload); break;
    case 'tool_end':        cbs.onToolEnd?.(payload); break;
    case 'approval_pending': cbs.onApprovalPending?.(payload); break;
    case 'done':            cbs.onDone?.(payload); break;
    case 'error':           cbs.onError?.(new Error(payload.error || 'stream error')); break;
  }
}
```

### 8.4 StreamCallbacks 类型

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

### 8.5 前端消费方式

在 `web/src/pages/ChatThread.tsx` 中：

- **用户消息**：先乐观渲染（optimistic update），立即在 UI 中显示用户气泡
- **assistant 事件**：按 `message_id` 去重更新消息列表；空内容的工具轮次被跳过
- **tool_start 事件**：创建 `kind: 'tool_card'` 的合成消息，显示为折叠的工具卡片
- **tool_end 事件**：更新对应工具卡片的状态/结果/错误信息
- **approval_pending 事件**：渲染内联审批卡片（批准/拒绝）
- **停止机制**：同时调用 `stopSession()` API（服务端中断）和 `AbortController.abort()`（客户端断开流）

### 8.6 SSE 连接生命周期

```
前端                          后端
  │                             │
  │── POST /messages/stream ──→ │
  │                             │  写 SSE 头 + 心跳帧 ": ok\n\n"
  │←── ": ok\n\n" ────────────│
  │                             │
  │←── "event: assistant\n..." │  ← agent.EventAssistant
  │←── "event: tool_start\n..."│  ← agent.EventToolStart
  │←── "event: tool_end\n..."  │  ← agent.EventToolEnd
  │←── "event: assistant\n..." │  ← 可能多轮
  │←── "event: done\n..."      │  ← agent.EventDone
  │                             │
  │  (连接关闭)                  │
```

---

## 9. WebSocket 通信

除了 REST + SSE，前端还使用 **原生 WebSocket** 用于 WebShell 终端：

- **端点**：`ws(s)://<host>/api/v1/devices/{device_id}/shell?token=<jwt>`
- **子协议**：`ongrid.shell.v1`
- **认证**：JWT 通过 query string `?token=` 传递（浏览器 WebSocket 不支持自定义请求头）
- **帧格式**：
  - 文本帧：JSON 控制消息（`open`/`resize`/`close`/`ready`/`auth_error`/`exit`）
  - 二进制帧：终端 I/O
- **消费组件**：`web/src/pages/DeviceShell.tsx`

---

## 10. 边云隧道：geminio 替代 gRPC

### 10.1 通信架构

OnGrid 使用 **singchia/geminio** + **singchia/frontier** 作为 Edge-Cloud 通信框架，而非标准 gRPC：

```
云端 Manager
    │
    │ frontierbound.Client (geminio SDK)
    ▼
Frontier Broker (独立容器)
    │
    │ geminio (JSON over TCP)
    ▼
Edge Agent
    tunnel.Client (geminio)
    RegisterHandler(method, handler)
```

### 10.2 隧道方法注册

| 方向 | 方法名 | 请求类型 | 响应类型 |
|------|--------|---------|---------|
| edge → cloud | `register_edge` | `RegisterEdgeRequest` | `RegisterEdgeResponse` |
| edge → cloud | `heartbeat` | `HeartbeatRequest` | `HeartbeatResponse` |
| edge → cloud | `push_host_metrics` | `PushHostMetricsRequest` | `PushHostMetricsResponse` |
| cloud → edge | `get_host_load` | `GetHostLoadRequest` | `GetHostLoadResponse` |
| cloud → edge | `get_process_list` | `GetProcessListRequest` | `GetProcessListResponse` |
| cloud → edge | `get_netstat` | `GetNetstatRequest` | `GetNetstatResponse` |
| cloud → edge | `bash_exec` | `BashRequest` | `BashResponse` |
| cloud → edge | `host_files` | `HostFilesRequest` | `HostFilesResponse` |
| cloud → edge | `restart_service` | `RestartServiceRequest` | `RestartServiceResponse` |
| cloud → edge | `describe_k8s_resource` | `KubernetesDescribeResourceRequest` | `KubernetesDescribeResourceResponse` |
| cloud → edge | `query_k8s_logs` | `KubernetesPodLogsRequest` | `KubernetesPodLogsResponse` |
| cloud → edge | `execute_k8s_action` | `KubernetesActionRequest` | `KubernetesActionResponse` |

### 10.3 序列化方式

- **当前（MVP）**：JSON over geminio（proto 消息仅用于类型定义，实际序列化为 JSON）
- **Phase 2 计划**：可能切换到 protobuf binary 编码

### 10.4 gRPC 依赖来源

`google.golang.org/grpc v1.80.0` 是 `// indirect` 依赖，被 `singchia/geminio` 间接引入。geminio 内部可能使用 gRPC 作为传输层，但 OnGrid 代码不直接调用 gRPC API。

---

## 11. 架构全景图

```
┌─────────────────────────────────────────────────────────────┐
│                    前端 SPA (React)                          │
│                                                             │
│  ┌─────────────┐  ┌──────────────┐  ┌───────────────────┐  │
│  │ fetch + JSON│  │ fetch + SSE  │  │ WebSocket         │  │
│  │ (CRUD API)  │  │ (AI 聊天流)   │  │ (WebShell 终端)   │  │
│  └──────┬──────┘  └──────┬───────┘  └────────┬──────────┘  │
└─────────┼────────────────┼───────────────────┼─────────────┘
          │ HTTP/REST       │ SSE over HTTP     │ WS
          │ JSON            │ event+data JSON   │ JSON ctrl + binary I/O
          ▼                 ▼                   ▼
┌─────────────────────────────────────────────────────────────┐
│              Go 后端 (cmd/ongrid/main.go)                    │
│                                                             │
│  ┌─────────────────────────────────────────────────────┐   │
│  │              chi.NewRouter()                         │   │
│  │  ├── /api/v1/auth/*        → iamHandler             │   │
│  │  ├── /api/v1/chat/*        → aiopsHandler           │   │
│  │  │   └── POST /sessions/{id}/messages/stream (SSE)  │   │
│  │  ├── /api/v1/edges/*       → edgeHandler            │   │
│  │  ├── /api/v1/kubernetes/*  → k8sHandler             │   │
│  │  ├── /api/v1/alerts/*      → alertHandler           │   │
│  │  ├── /api/v1/notifications/* → notificationHandler  │   │
│  │  ├── /api/v1/system-settings/* → settingHandler     │   │
│  │  ├── /api/v1/metrics/*     → metricHandler          │   │
│  │  └── /api/v1/devices/{id}/shell → webshellHandler   │   │
│  └─────────────────────────────────────────────────────┘   │
│          │                                                  │
│          │ (无 gRPC 服务端)                                  │
│          │                                                  │
│  ┌───────┴───────────────────────────────────────────┐     │
│  │  frontierbound.Client (geminio SDK)                │     │
│  └───────────────────────┬───────────────────────────┘     │
└──────────────────────────┼──────────────────────────────────┘
                           │ geminio (JSON over TCP)
                           ▼
┌─────────────────────────────────────────────────────────────┐
│              Frontier Broker (独立容器)                       │
└──────────────────────────┬──────────────────────────────────┘
                           │ geminio (JSON over TCP)
                           ▼
┌─────────────────────────────────────────────────────────────┐
│              Edge Agent (cmd/ongrid-edge/main.go)            │
│  tunnel.Client (geminio)                                    │
│  RegisterHandler("bash_exec", ...)                          │
│  RegisterHandler("get_host_load", ...)                      │
│  RegisterHandler("host_files", ...)                         │
│  RegisterHandler("restart_service", ...)                    │
│  ...                                                        │
└─────────────────────────────────────────────────────────────┘
```

**Proto 的位置**：纯类型契约，位于 `api/` 目录，通过 `buf generate` 生成 Go 结构体供 HTTP handler 使用。不参与运行时 gRPC 传输。

---

## 12. Proto 与 HTTP 端点映射表

### IamService

| Proto RPC | HTTP 方法 | HTTP 路径 |
|-----------|----------|----------|
| `Register` | POST | `/api/v1/auth/register` |
| `Login` | POST | `/api/v1/auth/login` |
| `Refresh` | POST | `/api/v1/auth/refresh` |
| `GetSelf` | GET | `/api/v1/auth/me` |
| `CreateOrg` | POST | `/api/v1/orgs` |
| `ListOrgs` | GET | `/api/v1/orgs` |
| `InviteMember` | POST | `/api/v1/orgs/{org_id}/members` |
| `ListMembers` | GET | `/api/v1/orgs/{org_id}/members` |
| `SwitchOrg` | POST | `/api/v1/auth/switch-org` |

### AiopsService

| Proto RPC | HTTP 方法 | HTTP 路径 |
|-----------|----------|----------|
| `CreateChatSession` | POST | `/api/v1/chat/sessions` |
| `ListChatSessions` | GET | `/api/v1/chat/sessions` |
| `PostMessage` | POST | `/api/v1/chat/sessions/{id}/messages` |
| `StreamMessage` | POST | `/api/v1/chat/sessions/{id}/messages/stream` |
| `ListMessages` | GET | `/api/v1/chat/sessions/{id}/messages` |

### EdgeService

| Proto RPC | HTTP 方法 | HTTP 路径 |
|-----------|----------|----------|
| `CreateEdge` | POST | `/api/v1/edges` |
| `ListEdges` | GET | `/api/v1/edges` |
| `GetEdge` | GET | `/api/v1/edges/{id}` |
| `DeleteEdge` | DELETE | `/api/v1/edges/{id}` |
| `RotateSecret` | POST | `/api/v1/edges/{id}/rotate-secret` |

### KubernetesService

| Proto RPC | HTTP 方法 | HTTP 路径 |
|-----------|----------|----------|
| `CreateCluster` | POST | `/api/v1/kubernetes/clusters` |
| `ListClusters` | GET | `/api/v1/kubernetes/clusters` |
| `GetCluster` | GET | `/api/v1/kubernetes/clusters/{id}` |
| `GetClusterHealth` | GET | `/api/v1/kubernetes/clusters/{id}/health` |
| `ListEdgeAttachments` | GET | `/api/v1/kubernetes/edge-attachments` |
| `ListNodes` | GET | `/api/v1/kubernetes/clusters/{id}/nodes` |
| `ListWorkloads` | GET | `/api/v1/kubernetes/clusters/{id}/workloads` |
| `ListPods` | GET | `/api/v1/kubernetes/clusters/{id}/pods` |
| `ListEvents` | GET | `/api/v1/kubernetes/clusters/{id}/events` |
| `RotateBootstrapToken` | POST | `/api/v1/kubernetes/clusters/{id}/rotate-token` |
| `DeleteCluster` | DELETE | `/api/v1/kubernetes/clusters/{id}` |
| `Enroll` | POST | `/api/v1/kubernetes/enroll` |

### AlertService

| Proto RPC | HTTP 方法 | HTTP 路径 |
|-----------|----------|----------|
| `ListIncidents` | GET | `/api/v1/alerts/incidents` |
| `GetIncident` | GET | `/api/v1/alerts/incidents/{id}` |
| `AcknowledgeIncident` | POST | `/api/v1/alerts/incidents/{id}/ack` |
| `ResolveIncident` | POST | `/api/v1/alerts/incidents/{id}/resolve` |

### NotificationService

| Proto RPC | HTTP 方法 | HTTP 路径 |
|-----------|----------|----------|
| `ListChannels` | GET | `/api/v1/notifications/channels` |
| `GetChannel` | GET | `/api/v1/notifications/channels/{id}` |
| `CreateChannel` | POST | `/api/v1/notifications/channels` |
| `UpdateChannel` | PATCH | `/api/v1/notifications/channels/{id}` |
| `DeleteChannel` | DELETE | `/api/v1/notifications/channels/{id}` |
| `TestChannel` | POST | `/api/v1/notifications/channels/{id}/test` |

### SettingService

| Proto RPC | HTTP 方法 | HTTP 路径 |
|-----------|----------|----------|
| `ValidateLLMConfiguration` | POST | `/api/v1/integrations/llm/test` |
| `ValidateAndSaveLLMConfiguration` | POST | `/api/v1/integrations/llm/validate-and-save` |

### MetricService

| Proto RPC | HTTP 方法 | HTTP 路径 |
|-----------|----------|----------|
| `QueryHostMetrics` | GET | `/api/v1/metrics/hosts/{edge_id}` |

---

## 附录 A：Proto 与 SSE 事件类型对比

### StreamChunk.oneof payload vs SSE event

| Proto StreamChunk | SSE event | 差异说明 |
|-------------------|-----------|---------|
| `content_delta` | **未实现** | Proto 定义了 token-by-token 增量，但 SSE 不发送此帧 |
| `tool_call_start` | `tool_start` | 名称微调 |
| `tool_call_result` | `tool_end` | 名称微调，语义从"结果就绪"变为"工具结束" |
| `done` | `done` | 一致 |
| — | `assistant` | SSE 独有，Proto 无对应（Proto 只有 content_delta） |
| — | `error` | SSE 独有，Proto 无对应 |
| — | `task_notification` | SSE 独有，Proto 无对应 |
| — | `approval_pending` | SSE 独有，Proto 无对应 |
| — | `summary` | SSE 独有（兜底帧，当 agent 返回但未发 done 时） |

---

## 附录 B：gRPC 依赖链分析

```
ongrid (直接依赖)
├── singchia/geminio v1.3.0-rc.2
│   └── google.golang.org/grpc v1.80.0 (间接)
│       └── grpc-ecosystem/grpc-gateway/v2 v2.28.0 (间接)
└── singchia/frontier v1.2.4
    └── (可能间接引入 grpc)
```

OnGrid 代码中：
- **不调用** `grpc.NewServer()`、`grpc.Dial()`、`RegisterXxxServer()`
- **不调用** `runtime.NewServeMux()`、`RegisterXxxHandlerFromEndpoint()`
- **不使用** `protoc-gen-grpc-gateway` 生成代码
- gRPC 和 grpc-gateway 仅作为间接依赖存在于 `go.mod` 中
