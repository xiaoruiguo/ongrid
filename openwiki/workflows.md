# Workflows

> 重要端到端流程。每条流程标注源码文件和行号。

## 1. 用户发起对话（ChatThread SSE 流）

完整链路从 `/chat/:sessionId` 路由开始：

```
前端                                    后端
ChatThread.tsx (send)                   main.go (中间件链)
  ↓                                       ↓
chat.ts (streamMessage)                 http.go (streamMessage handler)
  ↓                                       ↓
client.ts (fetch + ReadableStream)      service.go (runWithKernel)
  ↓                                       ↓
auth.ts (Bearer token 注入)             chatruntime/runtime.go (10 步编排)
  ↓                                       ↓
SSE \n\n 帧分割                          graph/react.go (ReAct 图)
  ↓                                       ↓
MessageBubble.tsx (渲染)               callbacks/chain.go (6-handler 链)
                                          ↓
                                        eino_routing.go (LLM 路由)
                                          ↓
                                        router.go (MultiClient 60s TTL)
```

**关键文件**：
- `web/src/pages/ChatThread.tsx` L217-416 — send 函数
- `web/src/api/chat.ts` L252-317 — streamMessage（fetch + ReadableStream + TextDecoder）
- `internal/manager/server/aiops/http.go` — SSE handler
- `internal/manager/service/aiops/service.go` L334-376 — runWithKernel
- `internal/manager/biz/aiops/chatruntime/runtime.go` L468-851 — 10 步编排（Handle）
- `internal/manager/biz/aiops/graph/react.go` L78-188 — BuildReActGraph
- `internal/manager/biz/aiops/graph/callbacks/chain.go` L88-118 — 6-handler 链

详见 [ongrid_route_chat.md](../ongrid_route_chat.md)。

## 2. 创建会话（startSession）

用户在 Home 页输入消息后先创建 session，再跳转到 ChatThread 页通过 SSE 发送首条消息：

```
Home.tsx (startSession)
  ↓
chat.ts (createSession) → POST /v1/chat/sessions
  ↓
http.go (createSession handler) → service.go (CreateSession)
  ↓
session.go (GORM CreateSession) → model.go (BeforeCreate UUIDv4 hook)
  ↓
返回 session DTO → 前端 navigate(/chat/{id})
```

**关键文件**：
- `web/src/pages/Home.tsx` L226-245 — startSession
- `web/src/api/chat.ts` L80-87 — createSession
- `internal/manager/server/aiops/http.go` L307-329 — createSession handler
- `internal/manager/service/aiops/service.go` L204-232 — CreateSession
- `internal/manager/data/aiops/store/session.go` L31-36 — GORM CreateSession
- `internal/manager/model/aiops/model.go` L86-91 — BeforeCreate UUIDv4 hook

详见 [ongrid_startsession.md](../ongrid_startsession.md)。

## 3. 告警自动调查

```
告警触发 → alert/pipeline.go (告警评估)
  ↓
incident 创建 → IM 渠道通知 (Slack/Telegram/...)
  ↓
investigator agent 启动 → RCA worker
  ↓
走拓扑 → 关联 m/l/t → 定位根因 → 写回 chat
```

**关键文件**：
- `internal/manager/biz/alert/pipeline.go` — 告警评估管道
- `internal/manager/biz/alert/router.go` — 告警路由
- `internal/manager/biz/alert/inhibit.go` — 告警抑制
- `agents/incident-investigator.md` — 调查 agent 定义

## 4. 边端数据采集与上报

```
ongrid-edge 启动
  ↓
edgeagent/biz/agent.go — agent 生命周期
  ↓
collector (cpu/mem/net/disk) → 指标采集
  ↓
k8s agent (inventory/metrics/readonly) → K8s 资源
  ↓
tunnel (反向隧道) → frontier broker → cloud manager
```

**关键文件**：
- `cmd/ongrid-edge/main.go` — 边端入口
- `internal/edgeagent/biz/agent.go` — agent 生命周期
- `internal/edgeagent/collector/scrape.go` — 指标采集
- `internal/pkg/tunnel/client.go` — 反向隧道客户端

## 5. 认证与授权流程

```
前端 Login → POST /auth/login
  ↓
iam/server/http.go → iam/biz/user/usecase.go (argon2id 验证)
  ↓
auth/jwt.go (签发 access 15m + refresh 720h)
  ↓
后续请求: auth.Middleware (JWT 验证, 无 DB 查询)
  ↓
authzmw.Require (Casbin 5 级决策)
  ↓
tenantctx (双层 context 存储租户信息)
```

**关键文件**：
- `internal/pkg/auth/jwt.go` L24-30 — Claims (UserID/Email/Role/IsSuperuser)
- `internal/pkg/auth/middleware.go` L21-53 — JWT 中间件
- `internal/pkg/authzmw/middleware.go` L70-96 — Casbin 5 级决策
- `internal/pkg/tenantctx/tenantctx.go` — 双层 context（*slot 指针模式）
- `internal/iam/biz/user/hash.go` — argon2id 密码哈希

## 6. 前端 401 自动刷新

```
request() → 401
  ↓
refreshAccessToken() — refreshInFlight 单飞去重
  ↓
POST /auth/refresh → 新 token
  ↓
重试一次 (_retryingAfterRefresh)
  ↓
仍 401 → logout
```

**关键文件**：`web/src/api/client.ts` L97-162
