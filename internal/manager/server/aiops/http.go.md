# `http.go` 技术实现文档

> 源文件：`internal/manager/server/aiops/http.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/server/aiops`

## 1. 概述

本文件是 AIOps 子域的 HTTP 路由层：把 `chi.Router` 上的 `/v1/chat/*`、`/v1/agents/*`、`/v1/aiops/*`、`/v1/usage/today` 等端点绑定到 `biz/aiops` 的 service 接口。设计要点：所有可注入依赖（mentions / catalog / agents / userAgents / llmClient）都是 narrow interface + 可选注入；nil 时走 graceful-degradation（返回空列表或 503/501）而不是 panic。关键红线：非 owner 非 admin 的 caller 拿不到他人 session（service 层返回 404 而非 403，避免泄露 session 存在性）；`tenantctx` 必须由上游 auth middleware 注入。

## 2. 包信息

- **包名**：`aiops`
- **所属模块**：`internal/manager/server/`（HTTP handler 层）
- **依赖方向**：
  - 被上层 router 装配（`cmd → web → controlplane`）调用 `Register`、`NewHandler`、`Set*` 注入器
  - 依赖 `biz/aiops`、`biz/aiops/agent`、`biz/aiops/chatruntime`、`biz/aiops/mentions`、`service/aiops`、`model/aiops`、`pkg/llm`、`pkg/errs`、`pkg/tenantctx`

## 3. 关键类型与接口

```go
// 核心业务接口：由 svc.Service 通过 structural typing 满足
type AIOpsService interface {
    CreateSession(ctx, caller svc.Caller, in svc.CreateSessionInput) (*model.Session, error)
    ListSessions(ctx, caller svc.Caller, limit, offset int, relatedIncidentID *uint64) ([]*model.Session, error)
    ListMessages(ctx, caller svc.Caller, sessionID string) ([]*model.Message, error)
    CloseSession / DeleteSession / RenameSession
    PostMessage / PostMessageWithOpts
    PostMessageStream / PostMessageStreamWithOpts
    StopSession(ctx, caller svc.Caller, sessionID string) (bool, error)
    UsageToday(ctx) (*biz.DailyUsage, error)
    ListMutatingProposals(ctx, caller svc.Caller, f biz.MutatingProposalFilter) ([]*model.MutatingProposal, int64, error)
}

// 可选注入接口（nil 时降级）
type MentionSearcher interface { Search(ctx, q mentions.Query) ([]mentions.Item, error) }
type ModelCatalog interface { Providers() []llm.ProviderInfo; Default() (string, string) }
type AgentLister interface { All() []*chatruntime.Agent; ByName(name string) (*chatruntime.Agent, bool); Remove(name string) bool }
type UserAgentManager interface { Create / Update / Delete }

type Handler struct {
    svc        AIOpsService
    mentions   MentionSearcher
    catalog    ModelCatalog
    agents     AgentLister
    userAgents UserAgentManager
    llmClient  llm.Client // for /v1/aiops/query-translate; nil = endpoint 503
}
```

DTO：`createSessionReq`、`sessionDTO`、`postMessageReq`、`mentionInput`、`postMessageResp`、`toolCallDTO`、`usageDTO`、`messageDTO`、`usageTodayDTO`、`mutatingProposalDTO`、`agentDTO`、`userAgentReq` 等。`mentionInput.toAgent()` 把 wire 形态翻译成 `agent.Mention`。

## 4. 关键函数与流程

### `NewHandler` + `Set*` 系列注入器
- **签名**：`func NewHandler(s AIOpsService) *Handler`，`SetMentionSearcher / SetModelCatalog / SetLLMClient / SetAgentLister / SetUserAgentManager`
- **职责**：构造 Handler 并按启动顺序注入依赖；`nil` 允许，触发对应降级路径
- **流程**：基础 svc 必传，其余 `Set*` 由 router 装配阶段调用

### `Register`
- **签名**：`func (h *Handler) Register(r chi.Router)`
- **职责**：挂载 aiops 全部路由
- **流程**：注册 chat sessions、messages、SSE stream、stop、agents、agents/custom、query-translate、mentions、models、usage、mutating-proposals 等路由
- **错误处理**：路由注册无错误返回

### `createSession`
- **签名**：`func (h *Handler) createSession(w, r)`
- **职责**：`POST /v1/chat/sessions`
- **流程**：取 caller → 解码 body → `h.svc.CreateSession` → `toSessionDTO` → 201
- **错误处理**：caller 缺失 → 401；decode 失败 → 400（join ErrInvalid）；service 错误透传

### `postMessage` / `postMessageStream`
- **签名**：`func (h *Handler) postMessage(w, r)` + `postMessageStream`
- **职责**：发起一次 agent turn（阻塞 / SSE 流式）
- **流程**（流式）：
  1. caller / id / body 校验
  2. `w.(http.Flusher)` 探测；不支持 → 退化为阻塞 JSON
  3. 写 SSE 头：`Content-Type: text/event-stream`、`Cache-Control: no-cache`、`X-Accel-Buffering: no`（关 nginx buffering）
  4. 写 `: ok\n\n` keep-alive 帧 → Flush
  5. `emit := func(e agent.Event) { writeSSE(...) }`
  6. `h.svc.PostMessageStreamWithOpts(ctx, caller, id, content, emit, opts)`
  7. err → 写 `error` 帧（不切状态码，header 已发）
  8. reply 非 nil → 写 `summary` 帧兜底
- **错误处理**：流式开始后只能靠 error 帧，不能改状态码

### `eventName` / `eventPayload`
- **职责**：把 `agent.Event` 翻译成 SSE wire 帧
- **流程**：`eventName` 映射 `EventAssistant/ToolStart/ToolEnd/Done/TaskNotification/ApprovalPending` 到 snake_case；`eventPayload` 按事件类型构造 map（含 ArgsJSON/ResultJSON 尝试解析为 parsed JSON，失败回退 `*_raw` 字符串）

### `writeSSE`
- **签名**：`func writeSSE(w, f http.Flusher, name string, payload any)`
- **职责**：写一帧 `event: <name>\ndata: <json>\n\n` 并立即 Flush
- **错误处理**：Write 错误**故意吞掉**——客户端断连时无能为力，下一次 Flush 会让 agent 知道

### `listSessions` / `listMessages` / `listAgents`
- **职责**：列表端点；统一返回 `{items, total}`
- **流程**：取 caller → 解析 query → service 调用 → DTO 翻译 → 200

### `closeSession` / `stopSession` / `renameSession`
- **职责**：DELETE 软删 / POST 中断当前 turn / PATCH 重命名
- **流程**：`stopSession` 返回 `{"stopped": bool}`，幂等；`closeSession` 实际是硬删（rows + 依赖消息 / tool_calls 全擦）
- **错误处理**：non-owner 由 service 返 404（避免泄露 session 存在性）

### `usageToday`
- **职责**：`GET /v1/usage/today` — 集群级 token 日预算汇总；任何已认证 caller 可访问（无 admin gating）

### `listMutatingProposals`
- **职责**：`GET /v1/aiops/mutating-proposals` — ReviewGate 提案审计列表
- **流程**：filter 包含 `tool_name`、`decision`、`limit`、`offset`；limit ≤0 或 >200 时归一化为 50

### `searchMentions`
- **职责**：`GET /v1/aiops/mentions/search?q=&type=&limit=`
- **错误处理**：`h.mentions == nil` → 返回 200 + 空列表（popover 干净降级）

### `listModels`
- **职责**：`GET /v1/aiops/models` — provider catalog + 默认 (provider, model)
- **错误处理**：`h.catalog == nil` → 返回空 providers 列表，SPA 隐藏模型选择器

### `createUserAgent / updateUserAgent / deleteUserAgent`
- **职责**：`POST/PATCH/DELETE /v1/agents/custom/{name}` — 用户自定义 persona CRUD
- **错误处理**：`h.userAgents == nil` → 503 `ErrNotWiredYet`；viewer → 403；decode 失败 → 400

### `deleteAgent`
- **职责**：`DELETE /v1/agents/{name}` — 通用删除（区分 source）
- **流程**：
  1. `default` persona → 400「默认助理不可删除」
  2. `builtin` → 400「内置助理不可删除」
  3. `user` source → 调 `userAgents.Delete`（DB 行删除，registry 通过 service hook 刷新）
  4. `disk` source（或空）→ `h.agents.Remove(name)` 仅从内存 registry 移除；.md 文件保留，重启后回来（session-scoped 删除）

### `callerFromCtx` / `parseID` / `writeJSON` / `writeErr` / `errCode`
- 通用 helper。`errCode` 把 sentinel error 映射成 wire slug（`not-found`/`unauthorized`/`forbidden`/`budget-exceeded`/`edge-offline`/`not-wired-yet`/`internal` 等）

## 5. 依赖关系

- **内部包**：
  - `biz/aiops`、`biz/aiops/agent`、`biz/aiops/chatruntime`、`biz/aiops/mentions`
  - `service/aiops`（Caller / Input 类型）
  - `model/aiops`（Session / Message / MutatingProposal / UserAgent）
  - `pkg/llm`、`pkg/errs`、`pkg/tenantctx`
- **外部库**：`github.com/go-chi/chi/v5`
- **被调用方**：上层 router 装配代码（`server/router.go` 或同层 wire 装配）

## 6. 并发与资源管理

- **Handler 字段零锁**：Handler 在启动期通过 `Set*` 完成所有注入，运行时只读；若热更新场景需并发改字段，调用方需自加锁（当前未出现）
- **SSE 流式**：agent loop 在请求 goroutine 内 inline 跑，事件按发生顺序立即 `Flush`；客户端断连后下一次 Flush 才能让 agent 感知
- **ctx 透传**：所有 service 调用透传 `r.Context()`；流式路径不主动设 deadline（agent loop 内部自管）

## 7. 设计模式与亮点

- **Narrow interface + 可选注入**：MentionSearcher / ModelCatalog / AgentLister / UserAgentManager 全是 narrow contract；nil 即降级而不是 panic，让"裁剪版二进制"（无 device / 无 LLM）也能跑
- **Structural typing**：`*svc.Service` 通过 structural typing 满足 `AIOpsService`，测试换 fake 即可
- **404 而非 403**：session ownership 检查失败返 404，避免泄露 session 存在性（安全侧信道防御）
- **SSE 优雅降级**：`http.Flusher` 不支持时退化为阻塞 JSON，dev 环境代理透明可用
- **`X-Accel-Buffering: no`**：显式关 nginx buffering，保证流式事件实时到达浏览器
- **兜底 summary 帧**：agent 成功返回但未发 done 事件时补一帧 summary，让客户端能干净 resolve
- **ArgsJSON 容错**：LLM 偶尔吐非严格 JSON 时回退 `arguments_raw` 字段
- **disk-source agent 删除语义**：仅从内存 registry 移除，.md 文件保留——SPA delete 是 session-scoped，重启后回来

## 8. 注意事项

- **`closeSession` 实为硬删**：路由名"close"是历史遗留；DELETE 实际擦除 rows + 依赖 messages / tool_calls。需要保留审计历史的 caller 要走 service 层 soft-close
- **`usageToday` 无 admin gating**：任何已认证 caller 都能看到集群级日用量（设计如此，便于用户自查预算）
- **listMutatingProposals limit 归一化**：query 传 limit ≤0 或 >200 都会被改成 50（默认）；offset <0 改成 0
- **`deleteAgent` 与 `deleteUserAgent` 路径分离**：`/v1/agents/custom/{name}` 走 userAgents service；`/v1/agents/{name}` 是通用入口，按 source 分派
- **`toAgentDTO` 默认 Source**：disk-loaded personas 当前不标 Source，函数内默认填 `"disk"`，让 SPA 能决定 edit/delete 可见性
- **`errCode` slug 表**：跨多个 sentinel；新增 sentinel 需同步加 case，否则落到 `internal`
- **流式路径无主动 deadline**：依赖 agent loop 内部超时；caller 可在 client 侧设 ctx deadline
