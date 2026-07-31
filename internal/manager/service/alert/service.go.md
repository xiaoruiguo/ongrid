# `service.go` 技术实现文档

> 源文件：`internal/manager/service/alert/service.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/service/alert`

## 1. 概述

本文件是 alert 控制面的 manager 应用服务，是 HTTP handler 唯一消费面 —— handler 不直接访问 `biz.Usecase` 或 `model`。本层只做 transport DTO ↔ biz input 翻译与最小校验，业务规则（去重、状态机、kind-specific 验证）在 biz 层。核心红线：(1) Channel endpoint 在 DTO 中 mask（前 50 字符 + `...`），避免 URL 内 secret 回显 UI；(2) TestChannel 绕过全局 `ONGRID_NOTIFY_ENABLED` 主开关直接投递（运维要知道渠道是否真通）；(3) 删除 channel 前检查规则引用计数，避免规则静默回退到全局默认；(4) `metric_threshold` 是 UI-only friendly 形态，biz 在 save 时编译为 `metric_raw`。

## 2. 包信息

- **包名**：`alert`
- **所属模块**：`internal/manager/service/alert`
- **依赖方向**：被 HTTP handler 调用；依赖 `biz/alert`、`model/alert`、`internal/pkg/errs`、`internal/pkg/notify`

## 3. 关键类型与接口

```go
type Caller struct { UserID uint64; Role string }

type Service struct {
    uc          *bizalert.Usecase
    repo        bizalert.Repo
    notifier    Notifier
    previewDeps bizalert.PreviewDeps
    log         *slog.Logger
}

type Notifier interface {
    Send(ctx, msg notify.Message, channels ...string) error
}

// Transport DTOs
type Incident struct { /* id, rule_key, severity, status, summary, target_type, target_id, ... */ }
type IncidentFilter struct { Status, Severity, Query string; Page, PageSize int }
type IncidentMutationInput struct { Note string }
type IncidentSilenceInput struct { Until, Reason string }
type Event struct { /* id, incident_id, event_type, status_after, severity, ... */ }
type Channel struct { ID, Name, Type string; Enabled bool; EndpointMasked string; ... }
type ChannelInput struct { Name, Type, Endpoint, Secret string; Enabled bool }
type ChannelTestResult struct { Accepted bool; Message string }
type Rule struct { /* id, rule_key, kind, name, scope_type, join_mode, severity, enabled, conditions, spec, labels, runbook_url, notify_channel_ids, notify_window_seconds, notify_min_fires, ... */ }
type RuleInput struct { /* 同上去掉 ID/CreatedAt/UpdatedAt */ }
type RuleCondition struct { Metric, Operator, Window, For, Aggregator string; Threshold float64 }
type PreviewSample struct { Timestamp time.Time; Labels map[string]string; Value float64; Summary string }
type PreviewSeriesPoint struct { Timestamp time.Time; Value float64 }
type PreviewResult struct { FireCount int; FirstFireAt, LastFireAt *time.Time; Samples []PreviewSample; Series []PreviewSeriesPoint; Threshold *float64; Unit, SkippedReason string }
```

## 4. 关键函数与流程

### 构造

- **`New(uc, repo, notifier, log)`**：DB-backed；uc/repo 必非 nil（注释建议测试用 NewStub）；notifier 可 nil（TestChannel 返回 `ErrNotWiredYet`）。
- **`NewStub()`**：返回 `&Service{}`，所有方法短路到 `ErrNotWiredYet`，仅最小校验，供 HTTP 单测使用。
- **`SetPreviewDeps(d)`**：后置注入只读 preview 客户端（Prom range + Loki range + event counter）；未注入时对应 kind 返回 `skipped_reason` 而非报错。

### Incident 查询

- **`ListIncidents(ctx, _, in)`**：`pageBounds` 算 limit/offset；调 `uc.ListIncidents` 后逐行 `toServiceIncident`。
- **`CountIncidents(ctx, _, in)`**：仅传 Status + Severity（不带分页）。
- **`GetIncident(ctx, _, id)`**：id==0 → `ErrInvalid`；uc==nil → `ErrNotWiredYet`。

### Incident 状态机

- **`AcknowledgeIncident(ctx, caller, id, in)`**：note 可空（UI 单击 ack 即可）；调 `uc.AckIncident` 后 re-fetch 返回最新 incident。
- **`ResolveIncident(ctx, caller, id, in)`**：note 必填（trim 后空 → `ErrInvalid`）；流程同上。
- **`SilenceIncident(ctx, caller, id, in)`**：until + reason 都必填；until 形态（duration / RFC3339 / unix sec）由 biz 解析。
- **`ListIncidentEvents(ctx, _, incidentID, limit)`**：limit 默认 200，max 1000；调 `uc.ListEvents`。

### Channel CRUD

- **`ListChannels(ctx, _, page, pageSize)`** / **`GetChannel(ctx, _, id)`**：repo==nil → `ErrNotWiredYet`。
- **`CreateChannel(ctx, caller, in)`**：`validateChannelInput(in, true)`（requireType=true）；构造 `model.Channel`（ConfigJSON 由 `encodeChannelConfig(endpoint, secret)` 生成，与 env-seeded 行同形）；caller.UserID 非 0 时填 CreatedBy。
- **`UpdateChannel(ctx, _, id, in)`**：`validateChannelInput(in, false)`；先 GetChannelByID；`mergeChannelConfig(existing, endpoint, secret)` —— 空 secret 保留原值，`"-"` 显式清空；调 `repo.UpdateChannel` 后 re-fetch 返回。
- **`DeleteChannel(ctx, _, id)`**：先 `repo.CountRulesReferencingChannel`，>0 → `ErrInvalid`（中文提示"N 条规则关联"）。
- **`TestChannel(ctx, _, id)`**：
  1. repo 或 notifier nil → `ErrNotWiredYet`。
  2. GetChannelByID。
  3. 构造 `notify.Message`（info severity，dedupe_key=`channel-test-<id>`）。
  4. `bizalert.BuildSenderFromChannel(channel)` 直接构造 typed sender —— 注释明示绕过全局 `ONGRID_NOTIFY_ENABLED` 主开关，"运维测试渠道要知道它真的通，不要 silent no-op"。
  5. 10s timeout ctx；`sender.Send`；返回 `{Accepted, Message}` —— 失败也返回（Accepted=false + 上游错误原文）。

### Rule CRUD

- **`ListRules(ctx, _, scopeType)`**：调 `uc.ListRules`；逐行 `toServiceRule`；decode 失败 Warn 跳过（不 fail 整个请求）。
- **`CreateRule(ctx, caller, in)`** / **`UpdateRule(ctx, _, id, in)`**：`validateRuleInput(in, requireKey)`；caller.UserID 作为 createdBy；调 uc 后 `toServiceRule`。
- **`SetRuleEnabled`** / **`DeleteRule`** / **`GetRule`**：常规转发，id==0 → `ErrInvalid`。
- **`PreviewRule(ctx, _, in, lookbackSeconds)`**：
  1. `validatePreviewInput(in)` —— 仅 `metric_threshold` 要求至少一个 condition；rule_key/name/severity/runbook 都不要求（让用户先试算再决定命名）。
  2. 10s timeout ctx。
  3. `bizalert.PreviewRule(ctx, bizalert.PreviewInput{Input, LookbackSeconds}, s.previewDeps)`。
  4. `toServicePreview(res)` 过滤 NaN/Inf 值。

### 验证函数

- **`validatePreviewInput(in)`**：仅 `metric_threshold` + 无 condition → `ErrInvalid`；其他 kind 直接通过。
- **`validateRuleInput(in, requireKey)`**：requireKey 时 rule_key 必填；name 必填；severity 必填；`metric_threshold` 要求 ≥1 condition。
- **`validateChannelInput(in, requireType)`**：name 必填；requireType 时 type 必填；endpoint 必填。

### DTO 翻译函数

- **`toServiceIncident(r)`**：拷贝字段；从 `LabelsJSON` 解析 labels；若 `DeviceID` 为 nil 但 labels 含 `device_id`，promote 为 target（处理 `device_offline` 这类全局 PromQL 规则）。
- **`toServiceChannel(r)`**：从 ConfigJSON 提取 `endpoint`/`url`/`webhook_url` 之一，经 `maskEndpoint` 截断 50 字符。
- **`toServiceEvent(r)`**：nil 安全；Message 是 `*string`，解引用。
- **`toServiceRule(r)`**：kind 空默认 `metric_raw`（Post-Phase-3-final）；从 `ConditionsJSON` 解析 `spec`；从 `LabelsJSON`/`NotifyChannelIDsJSON` 解析对应字段；decode 失败返回 error（不静默）。
- **`toBizRuleInput(in)`**：把 `[]RuleCondition` 翻译为 `[]model.RuleCondition`。
- **`toServicePreview(r)`**：过滤 `isFinitePreviewValue`（NaN/Inf 丢弃）；threshold 也要 finite。
- **`encodeChannelConfig(endpoint, secret)`**：构造 `{endpoint, secret, secret_set:"true"}` 形态，与 env-seeded 行同形；空返回 `"{}"`。
- **`mergeChannelConfig(existing, endpoint, secret)`**：先 unmarshal existing；endpoint 空则 delete；secret 空保留，`"-"` 清除 secret + secret_set，其他值覆盖。
- **`maskEndpoint(s)`**：>50 字符截断 + `...`。
- **`pageBounds(page, pageSize)`**：page<=0 → 1；pageSize<=0 → 20；pageSize>200 → 200；返回 (limit, offset)。

## 5. 依赖关系

- **内部包**：`biz/alert`、`model/alert`、`internal/pkg/errs`、`internal/pkg/notify`
- **外部库**：`log/slog`、`encoding/json`、`math`、`strings`、`time`、`fmt`
- **被调用方**：HTTP handler（incident / channel / rule 系列 endpoint）；`service/systemhealth` 通过 `RuleLister` / `IncidentCounter` 接口调用

## 6. 并发与资源管理

- **无共享可变状态**：Service 字段在 `New` 后只读（`SetPreviewDeps` 是启动期一次性注入）；并发安全依赖 biz/repo 层。
- **TestChannel 独立 ctx**：10s timeout，不继承 caller ctx 的 deadline。
- **PreviewRule 独立 ctx**：10s timeout。
- **DTO 翻译无锁**：纯函数，可并发调用。

## 7. 设计模式与亮点

- **transport/biz 严格分层**：handler 只见 transport DTO（Incident/Channel/Rule）；biz input 是独立类型；Service 做 DTO 翻译 + 最小校验。
- **stub vs DB 双构造**：`NewStub` 让 HTTP 单测不依赖 biz；`New` 是生产路径。
- **metric_threshold UI-only friendly**：UI 发 `kind=metric_threshold + conditions[]`，biz save 时编译为 `metric_raw`；toServiceRule 把空 kind 默认为 `metric_raw`（Post-Phase-3-final）。
- **endpoint mask**：URL 内 query string secret 不回显 UI；仅前 50 字符。
- **secret 合并语义**：空 = 保留，`"-"` = 清除，其他 = 覆盖 —— 与 rule editor 的可选字段 pattern 一致。
- **TestChannel 绕主开关**：注释明示运维测试要真投递，不要 silent no-op。
- **删除前引用计数**：避免规则静默回退到全局默认 channel。
- **device_id promote**：`device_offline` 这类全局 PromQL 规则的 incident 把 device 只放在 labels，service 层 promote 为 target 让 UI/通知显示 device。
- **preview 过滤 NaN/Inf**：Prom range query 偶尔返回 NaN（缺失采样点），UI chart 不能渲染 NaN。

## 8. 注意事项

- **`Caller` 参数大量 `_`**：当前多数方法忽略 caller（RBAC 在 HTTP handler 层）；`AcknowledgeIncident` / `ResolveIncident` / `SilenceIncident` 用 caller.UserID 作为 operator；未来 RBAC 可能扩展。
- **`GetIncidentModel` 返回 raw model**：仅供 manual investigation trigger 使用，需要 DeviceID + Rule 完整字段；注释明示"未来 RBAC 检查附加此处"。
- **`toServiceRule` decode 失败返回 error**：与 `ListRules` 的 decode 失败 Warn 跳过不同 —— 单条查询不应静默跳过。
- **`metric_threshold` 在 Conditions 字段**：其他 kind 用 Spec map；toServiceRule 把 `ConditionsJSON` 统一解析为 Spec。
- **`encodeChannelConfig` 与 env-seeded 同形**：保证 notify router 读取一致；新增 channel type 需同步两端。
- **`PreviewRule` 10s deadline**：注释提及匹配 Prom range-query 在 cold blocks 的预期。
- **`ChannelTestResult.Message` 含上游错误原文**：UI 直接显示给运维，便于诊断。
- **`ListIncidentEvents` 限 1000**：避免恶意/误操作拉全量 timeline。
- **`pageBounds` max pageSize=200**：与 `ListMutatingProposals` 一致，全列表端点统一上限。
