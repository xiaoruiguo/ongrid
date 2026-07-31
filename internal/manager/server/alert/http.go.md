# `http.go` 技术实现文档

> 源文件：`internal/manager/server/server/alert/http.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/server/alert`

## 1. 概述

本文件是 alert 子域的 HTTP 路由层：把 `/v1/alerts/*`、`/v1/notification-channels/*`、`/v1/alert-rules/*` 端点绑定到 `service/alert` 的三个 service 接口（Incident / Channel / Rule）。设计要点：通过 narrow interface + builder-style `With*` 注入可选能力（runtime info / investigation reader / investigation trigger），nil 时走"feature off"降级（404 / 503 / `{status:feature_disabled}`）；写操作（channel / rule CRUD）强制 admin 角色；所有 mutation 都通过 `auditmw.SetAuditEvent` 写审计。关键红线：list 端点的 `total` 必须是 unfiltered count（不是 `len(items)`），否则 sidebar badge 用 `page_size=1` 轮询时会少报。

## 2. 包信息

- **包名**：`alert`
- **所属模块**：`internal/manager/server/`（HTTP handler 层）
- **依赖方向**：
  - 被上层 router 装配调用 `NewHandler` / `With*` / `Register`
  - 依赖 `biz/audit`、`biz/alert/investigator`、`service/alert`、`model/alert`、`model/audit`、`server/middleware`（audit）、`pkg/errs`、`pkg/tenantctx`

## 3. 关键类型与接口

```go
type IncidentService interface {
    ListIncidents / CountIncidents / GetIncident
    AcknowledgeIncident / ResolveIncident / SilenceIncident
    ListIncidentEvents
    GetIncidentModel(ctx, caller, id) (*alertmodel.Incident, error) // 给 trigger 端点用
}

type ChannelService interface {
    ListChannels / GetChannel
    CreateChannel / UpdateChannel / DeleteChannel
    TestChannel
}

type RuleService interface {
    ListRules / GetRule
    CreateRule / UpdateRule / SetRuleEnabled / DeleteRule
    PreviewRule(ctx, caller, in svc.RuleInput, lookbackSeconds int) (*svc.PreviewResult, error)
}

// 可选注入（nil = feature off）
type InvestigationReader interface {
    GetByIncident(ctx, incidentID uint64) (*alertmodel.InvestigationReport, error)
}
type InvestigationTrigger interface {
    ForceEnqueue(ctx, incident *alertmodel.Incident) error
    ForceEnqueueWith(ctx, incident *alertmodel.Incident, opts investigator.EnqueueOpts) error
}

// wire-level investigation report（解析后的 JSON 字段）
type InvestigationReport struct {
    ID, IncidentID, Status, StatusReason, RootCause, AffectedWindow string
    PinpointedTarget, RelatedAlerts, Evidence, SuggestedActions json.RawMessage
    FindingsMD string
    Confidence *float64
    ConfidenceFactors json.RawMessage
    AuditSessionID, WorkerID *string
    ToolCallCount int
    CreatedAt string
    ReadyAt *string
}

type Handler struct {
    incidents             IncidentService
    channels              ChannelService
    rules                 RuleService
    investigations        InvestigationReader
    investigationsTrigger InvestigationTrigger
    evaluatorInterval     time.Duration // 通过 GET /v1/alerts/runtime-info 暴露
    notifyCooldown        time.Duration
}
```

DTO：`listIncidentsResp`、`silenceReq`、`channelReq`、`ruleReq`（含 `NotifyWindowSeconds`/`NotifyMinFires` 发送策略）、`rulePreviewReq`（embed `ruleReq` + `LookbackSeconds`）、`mutationReq`、`errorBody`。

## 4. 关键函数与流程

### `NewHandler` + `WithRuntime` / `WithInvestigations` / `WithInvestigationTrigger`
- **签名**：`func NewHandler(incidents, channels, rules) *Handler` + 三个 `With*` builder
- **职责**：构造 Handler；可选能力按需注入
- **流程**：`rules` 可为 nil（路由块整体跳过）；`With*` 返回 `*Handler` 支持 chaining

### `Register`
- **签名**：`func (h *Handler) Register(r chi.Router)`
- **职责**：挂载 alert 全部路由
- **流程**：注册 incidents（list/get/events/investigation GET+POST/ack/resolve/silence/runtime-info）、channels（CRUD + test）、rules（仅当 `h.rules != nil` 才注册，含 preview）

### `listIncidents`
- **职责**：`GET /v1/alerts/incidents`
- **流程**：filter 含 `Status/Severity/Query/Page/PageSize` → service.ListIncidents → **CountIncidents 取真实 total**（失败回退 `len(items)`）→ 200
- **关键**：`total` 是 unfiltered-by-pagination count，不是 `len(items)`；sidebar badge 用 `page_size=1` 轮询依赖此字段

### `getIncidentInvestigation`
- **职责**：`GET /v1/alerts/incidents/{id}/investigation`
- **流程**：
  1. caller / id 校验
  2. `h.incidents.GetIncident` 存在性检查（同时强制 tenant scoping + 404）
  3. `h.investigations == nil` → 200 + `{status:"feature_disabled", status_reason:"investigator not wired..."}`（让 SPA 显示「投资分析未启用」徽章而非误导性的 spinner）
  4. `h.investigations.GetByIncident`；`ErrNotFound` → 200 + `{status:"not_started"}`；其他错误 → 透传
  5. `investigationReportToWire` → 200

### `triggerIncidentInvestigation`
- **职责**：`POST /v1/alerts/incidents/{id}/investigation` — 手动触发调查
- **流程**：
  1. `h.investigationsTrigger == nil` → 503 + feature_disabled
  2. `h.incidents.GetIncidentModel` 取原始 incident row
  3. `opts.Locale = localeFromRequest(r)`（从 Accept-Language 提取 en/zh）
  4. `h.investigationsTrigger.ForceEnqueueWith(ctx, incident, opts)`；err → 400 wrap ErrInvalid
  5. best-effort 读当前 row 状态回显（race 时返 `{status:"pending"}` stub）→ 202 Accepted
- **关键**：ForceEnqueue 语义 = 杀掉该 incident 上的 running worker + 删旧 report + 重新 spawn；threads Locale 让报告跟随操作者 UI 语言

### `investigationReportToWire`
- **职责**：DB row → wire shape
- **流程**：把 `*JSON` 字符串字段通过 `rawOrNil` 转成 `json.RawMessage`（`null`/空 → nil，避免 SPA 双重 decode）；时间字段 UTC + RFC3339

### `ackIncident` / `resolveIncident` / `silenceIncident`
- **职责**：incident 状态变更
- **流程**：ack/resolve 走 `mutateIncident` helper（统一 audit 写入）；silence 单独处理（多 `Until`/`Reason` 字段）
- **审计**：`auditmw.SetAuditEvent` 写 `bizaudit.Event`，含 Action / ResourceType / ResourceID / ResourceName / Status / Payload

### `mutateIncident`（helper）
- **签名**：`func (h *Handler) mutateIncident(w, r, action string, fn func(ctx, caller, id, in) (*svc.Incident, error))`
- **职责**：ack/resolve 共用模板——caller 校验、id 解析、body 解码、调 fn、写 audit、返结果

### channel CRUD（`createChannel` / `updateChannel` / `deleteChannel` / `testChannel`）
- **职责**：通知通道管理
- **权限**：全部 `requireAdmin`（非 admin → 403）
- **审计**：每个写操作都写 audit（ActionChannelCreate/Update/Delete）

### rule CRUD（`createRule` / `updateRule` / `setRuleEnabled` / `deleteRule` / `previewRule`）
- **职责**：告警规则管理
- **权限**：全部 `requireAdmin`
- **关键**：`previewRule` 也 gating admin——避免无权限 token 烧 24h Prom range

### `getRuntimeInfo`
- **职责**：`GET /v1/alerts/runtime-info` — 暴露 `evaluator_interval_seconds` / `notify_cooldown_seconds`（来自 env `ONGRID_ALERT_EVAL_INTERVAL` / `ONGRID_ALERT_COOLDOWN`）

### `localeFromRequest`
- **职责**：从 `Accept-Language` 提取 primary subtag（`en`/`zh`），其他返空
- **流程**：取逗号前第一段 → 按 `-` 切取首段 → ToLower → 只认 `en`/`zh`
- **注释**：`[[feedback_ai_output_locale]]` —— AI 输出语言跟随用户 UI locale

### helpers
- `callerFromRequest`：从 `tenantctx` 取 caller
- `requireAdmin`：caller 校验 + role == "admin" 校验
- `parseID`：`chi.URLParam` "id" → uint64；0 → ErrInvalid
- `intQuery`：query int 解析，失败/≤0 返默认值
- `writeJSON` / `writeErr` / `errCode` / `errSlug`：标准响应 helper
- `rawOrNil` / `stripWhitespace`：investigation JSON 字段处理

## 5. 依赖关系

- **内部包**：
  - `biz/audit`（`bizaudit.Event`）、`biz/alert/investigator`（`EnqueueOpts`）
  - `service/alert`（Caller / Incident / Channel / Rule / Input 类型）
  - `model/alert`（`InvestigationReport`、`Incident`）、`model/audit`（Action / Resource / Status 常量）
  - `server/middleware`（`auditmw.SetAuditEvent`）
  - `pkg/errs`、`pkg/tenantctx`
- **外部库**：`github.com/go-chi/chi/v5`
- **被调用方**：上层 router 装配代码

## 6. 并发与资源管理

- **Handler 字段零锁**：启动期 `With*` 完成注入，运行时只读
- **ctx 透传**：所有 service 调用透传 `r.Context()`
- **best-effort 回显**：`triggerIncidentInvestigation` 在 enqueue 后读 row 时可能 race insert；race 时返 synthetic `{status:"pending"}`，不阻塞

## 7. 设计模式与亮点

- **Narrow interface + builder `With*`**：investigation reader/trigger 可选注入，nil = feature off；`With*` chaining 风格易读
- **`{status:"feature_disabled"}` 优于 404**：让 SPA 区分「未启用」vs「未生成」两种 404 语义，避免误导性 spinner
- **`total` 必须是 unfiltered count**：注释明示 sidebar badge 用 `page_size=1` 轮询依赖此字段；CountIncidents 失败回退 `len(items)` 不阻塞 list
- **ForceEnqueue 语义**：手动触发 = 杀 running worker + 删旧 report + spawn fresh，无视旧状态——「give me a new investigation now」
- **Locale 跟随 Accept-Language**：手动 re-trigger 时报告跟随操作者 UI 语言（`[[feedback_ai_output_locale]]`）
- **previewRule gating admin**：避免无权限 token 烧 24h Prom range
- **`rawOrNil` 防 SPA 双重 decode**：存储层是 `*JSON` 字符串，wire 层是 `json.RawMessage`，null/空统一返 nil
- **mutation 统一 audit**：`mutateIncident` helper 把 ack/resolve 共用模板抽出，audit 写入一致

## 8. 注意事项

- **`rules == nil` 时整块路由不注册**：binary 不含 rule 端点时直接跳过，而不是返 503
- **`total` 字段语义**：list 端点的 total 是 unfiltered count（不含分页），不是 `len(items)`；CountIncidents 失败回退 `len(items)`
- **`triggerIncidentInvestigation` race 兜底**：enqueue 是异步的，立即读 row 可能 race insert；race 时返 `{status:"pending"}` stub
- **`getIncidentInvestigation` 三态**：`feature_disabled`（investigator 未启用）/ `not_started`（无 report row）/ 正常 report；SPA 据此渲染不同 UI
- **`localeFromRequest` 只认 en/zh**：其他语言返空，由 investigator 的 `Config.DefaultLocale` 兜底
- **`NotifyWindowSeconds`/`NotifyMinFires` 必须同时非零或同时为零**：注释明示 biz 层会拒绝 mixed 配置并返 400
- **`errSlug` vs `errCode`**：errCode 返 HTTP 状态码，errSlug 返 wire slug 字符串（如 `invalid-argument`）；新增 sentinel 需同步两处 case
- **`evaluatorInterval`/`notifyCooldown` 来自 env**：是 deployment 级而非 rule 级，SPA 应作为全局 banner/chip 显示
