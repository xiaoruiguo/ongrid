# `usecase.go` 技术实现文档

> 源文件：`internal/manager/biz/alert/usecase.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/alert`

## 1. 概述

本文件是 alert 子域的主 `Usecase`——incident 生命周期 + firing-path upsert + 通知闸门 + 规则 CRUD 的 facade。核心流程 `RecordFiring`：按 dedupe key upsert incident（新建/重开/bump），写 firing 事件，匹配 silence，仅 isNew 时调 investigator，isNew OR isReopen 时调 workflow dispatcher。`MaybeNotify` 是通知闸门：silence → cooldown → dampening（发送策略）→ inhibition → 渠道解析 → per-channel 投递 + delivery 行 + 事件。`SystemResolveIncident` 是恢复路径。`buildRuleRow` 是规则校验+编译的唯一入口，metric_threshold 在此编译为 metric_raw（Phase-3 collapse）。`compileMetricThresholdExpr` 把 closed-set 条件列表编译为单 PromQL 谓词。

## 2. 包信息

- **包名**：`alert`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 `pipeline.go` / `retry.go` / HTTP handler / `cmd` 装配调用；依赖 `internal/manager/model/alert` + `internal/pkg/errs` + `internal/pkg/notify` + `internal/pkg/prom` + `internal/pkg/tunnel`

## 3. 关键类型与接口

```go
type Notifier interface {
    Send(ctx, msg notify.Message, channels ...string) error
    SendVia(ctx, msg notify.Message, sender notify.Sender) error
}

type NoopHostMetricIngester struct{}  // Phase-3 后占位，Push 永返 nil
type Investigator interface { InvestigateAsync(incident *model.Incident) }  // 非阻塞
type WorkflowDispatcher interface {  // 非阻塞
    OnAlertFired(incidentID uint64, rule, severity string, edgeID, deviceID uint64, labels map[string]string, firedAt time.Time)
}

type Usecase struct {
    repo  Repo
    clock Clock
    log   *slog.Logger
    investigator        Investigator       // 可选
    workflowDispatcher  WorkflowDispatcher  // 可选
}

type FiringInput struct { ScopeType, Scope, Rule, RuleName, Severity string; DeviceID *uint64; OccurredAt time.Time; DedupeKey, SourceType string; Title, Summary, Description string; Value, Threshold *float64; Labels, Annotations map[string]string; RunbookURL string }

type FiringResult struct {
    Incident *model.Incident
    IsNew, IsReopen, Silenced bool
    SilencedBy *uint64
}

type RuleInput struct { RuleKey, Kind, Name, ScopeType, JoinMode, Severity string; Enabled bool; Conditions []model.RuleCondition; Spec map[string]any; Labels map[string]string; RunbookURL string; NotifyChannelIDs []uint64; NotifyWindowSeconds, NotifyMinFires int }

type NotifyOpts struct { Notifier; Resolver ChannelResolver; DefaultChannels []string; Cooldown time.Duration; Inhibitor Inhibitor }
```

## 4. 关键函数与流程

### `NewUsecase` + Set* 注入

- `NewUsecase(repo, log)`：log nil → `slog.Default()`；clock = `realClock{}`
- `SetInvestigator(inv)` / `SetWorkflowDispatcher(d)`：可选注入，nil-safe

### `createEvent`

- **签名**：`func (u *Usecase) createEvent(ctx, ev *model.Event, ruleKey string) error`
- **职责**：集中化事件写入 + 喂 `alert_events_total` 计数器
- **流程**：
  1. `u.repo.CreateEvent(ctx, ev)`；err → return（不喂计数器，避免失败时膨胀）
  2. 成功 → `prom.IncAlertEvent(ev.EventType, ev.Severity, ruleKey)`
- **错误处理**：返回 err 让调用方决定（`RecordFiring` 把 event 写入失败 Warn 不阻断）

### `RecordFiring`

- **签名**：`func (u *Usecase) RecordFiring(ctx, in FiringInput) (*FiringResult, error)`
- **职责**：按 dedupe key upsert incident + 写 firing 事件 + 触发 investigator / workflow
- **流程**：
  1. `validateFiring(in)` 校验 rule/scope_type/severity 必填、host scope 必须有 device_id
  2. `occurredAt = in.OccurredAt ?? u.clock.Now()`
  3. `dedupeKey = in.DedupeKey ?? buildDedupeKey(scopeType, deviceID, rule)`
  4. `existing := u.repo.GetIncidentByDedupeKey(ctx, dedupeKey)`；err 且非 ErrNotFound → return
  5. **existing == nil 路径（创建）**：
     - 构造 `newInc`（Status=Open、EventCount=1、FirstFiredAt=LastFiredAt=occurredAt）
     - `u.repo.CreateIncident(ctx, newInc)`；err：
       - **race recovery**：再次 `GetIncidentByDedupeKey`；命中 → 走 existing 路径
       - 未命中 → return err
     - 成功 → `incident = newInc; isNew = true`
  6. **existing 路径（bump / reopen）**：
     - `existing.Status == Resolved` → `ReopenIncident` + 清 SilencedUntil/ResolvedAt/ResolvedBy + `isReopen = true`
     - 否则 → `BumpIncidentFiring`
     - `incident = existing`
  7. 写 firing 事件（eventType = Firing 或 Reopened）；`createEvent` 失败 Warn 不阻断
  8. `silenced, silencedBy := u.matchSilence(ctx, incident, occurredAt)`
  9. **`isNew && u.investigator != nil`** → `u.investigator.InvestigateAsync(incident)`（仅新建，避免 reopen flap 烧 LLM 账单）
  10. **`(isNew || isReopen) && u.workflowDispatcher != nil`** → `OnAlertFired(...)`（reopen 是真实复发，remediation workflow 应再跑）
  11. 返回 `FiringResult`
- **错误处理**：validate 失败 return err；DB 失败 return err；event/investigator/workflow 失败 Warn 不阻断

### `MaybeNotify`

- **签名**：`func (u *Usecase) MaybeNotify(ctx, res *FiringResult, msg notify.Message, opts NotifyOpts)`
- **职责**：通知闸门——silence / cooldown / dampening / inhibition / 渠道解析 / per-channel 投递
- **流程**：
  1. nil 守卫
  2. `opts.Notifier == nil` → return
  3. `res.Silenced` → return（silence 优先级最高）
  4. `!CooldownExceeded(incident, cooldown, occurredAt)` → `RecordRepeatSuppressed("cooldown")` + return
  5. **发送策略 dampening**：`rule := lookupRuleForIncident`；rule 有 `NotifyWindowSeconds>0 && NotifyMinFires>0`：
     - `CountEventsByType(Firing, since=now-window, rule, "")` 计数
     - `count < NotifyMinFires` → `RecordRepeatSuppressed("dampened: N/M fires in Ws window")` + return
  6. **inhibition**：`opts.Inhibitor.Suppress(ctx, incident)` 命中 → 写 `EventTypeInhibited` 事件 + return
  7. `channels := u.resolveChannels(ctx, incident, opts)`；空 → return
  8. per-channel：
     - nil / 空名 / disabled → 跳过
     - **ID>0 但无 destination**（seeded placeholder）→ 跳过（避免误导 "notification_sent"）
     - ID>0 → `RecordDelivery(incidentID, channelID)` 拿 deliveryID
     - ID>0 → `BuildSenderFromChannel(ch)` 构造 typed sender；`Notifier.SendVia(ctx, msg, sender)`
     - ID==0（synthetic fallback）→ `Notifier.Send(ctx, msg, ch.Name)`
     - `sendErr == nil` → `anySuccess = true`
     - deliveryID>0 → `FinishDelivery(deliveryID, incidentID, channelName, sendErr, occurredAt)`
  9. `anySuccess` → `MarkNotified(incident, occurredAt)`
- **错误处理**：每个 IO 失败 Warn 不阻断；`MarkNotified` 失败 Warn（ErrNotFound 吞掉）

### `BuildSenderFromChannel`

- **签名**：`func BuildSenderFromChannel(ch *model.Channel) (notify.Sender, error)`
- **职责**：从 ChannelType + ConfigJSON 构造 typed sender
- **流程**：
  1. `cfg := ch.Config()` 解码 ConfigJSON
  2. `endpoint := cfg["endpoint"] ?? cfg["url"]`（defensive legacy key）
  3. endpoint 空 → err `"channel X has no endpoint"`
  4. `secret := cfg["secret"]`
  5. switch ChannelType：Slack / Feishu / DingTalk / WeCom / Telegram / Webhook（默认）→ 对应 `notify.NewXxxSender`
- **错误处理**：未知 type → err

### `SystemResolveIncident`

- **签名**：`func (u *Usecase) SystemResolveIncident(ctx, dedupeKey, reason string, occurredAt time.Time) (bool, error)`
- **职责**：条件恢复时按 dedupe key resolve incident
- **流程**：
  1. dedupeKey 空 → err
  2. `incident := GetIncidentByDedupeKey`；ErrNotFound → `(false, nil)`（无 prior firing）
  3. 已 Resolved → `(false, nil)`
  4. `UpdateIncidentStatus(Resolved, nil, occurredAt)`
  5. `createEvent(EventTypeResolved, ActorType=System, Reason)`
  6. 返回 `(true, nil)`
- **错误处理**：DB 失败 return err

### `buildRuleRow` + `buildConditionsJSON`

- **签名**：`func buildRuleRow(in RuleInput, requireKey bool) (*model.Rule, error)`
- **职责**：规则校验+编译的唯一入口
- **流程**：
  1. `rule_key` 校验：requireKey 时必填 + `validRuleKey`（lower_snake [a-z0-9_]）
  2. `kind := NormalizeKind(in.Kind)`；`IsKnownKind` 校验
  3. `scope := in.ScopeType ?? defaultScopeForKind(kind)`；`scopeAllowedForKind` 校验
  4. `join := in.JoinMode ?? "all"`；校验 all/any
  5. `severity` 必填
  6. `condJSON := buildConditionsJSON(kind, in)`——按 kind 编译 spec
  7. **metric_threshold → metric_raw 编译**：`storageKind = metric_raw`（UI-only entry form）
  8. **发送策略校验**：both zero=disabled / both>0=enabled / mixed=reject；window [60, 1440*60]、threshold [1, 100]
  9. 构造 `model.Rule` 行
  10. `Labels` / `RunbookURL` / `NotifyChannelIDs`（去重 + 校验）填入
- **错误处理**：每个校验失败 return `errs.ErrInvalid` + 详情

### `compileMetricThresholdExpr`

- **签名**：`func compileMetricThresholdExpr(conds []model.RuleCondition, joinMode string) (string, error)`
- **职责**：把 closed-set 条件列表编译为单 PromQL 谓词
- **流程**：
  1. per-cond：`metricExprFor(c.Metric)` 取 closed-set 表达式；拼 `(<expr>) <op> <thr>`
  2. 单条件 → 直接返回
  3. join=all → `and on(device_id)` join（per-device AND）
  4. join=any → `or` join
- **错误处理**：metric 不在 closed-set → err

### 其他 helper

- `CooldownExceeded(incident, cooldown, now)`：`LastNotifiedAt == nil || now - LastNotifiedAt >= cooldown`
- `matchSilence(ctx, incident, at)`：incident 自身 silenced 状态 + 活跃 silence 行匹配
- `silenceMatches(s, inc)`：scope_type / device_id / rule 三层匹配，空字段当 wildcard
- `validateFiring(in)`：rule/scope_type/severity 必填、host scope 必须有 device_id
- `buildDedupeKey(scopeType, deviceID, rule)`：host→`host:<id>:<rule>`、pipeline→`pipeline:<rule>`、其他→`<scope>:<rule>`
- `parseSilenceUntil(now, raw)`：duration / RFC3339 / unix seconds 三种格式
- `lookupRuleForIncident(ctx, incident)`：按 rule_key 查 rule；任何 miss 返回 nil（dampening 优雅降级）
- `channelHasDestination(ch)`：config 含 `endpoint` 或 `url` 才算可投递

## 5. 依赖关系

- **内部包**：
  - `internal/manager/model/alert`（`Incident` / `Event` / `Silence` / `Channel` / `Delivery` / `Rule` / `RuleCondition` / 状态常量）
  - `internal/pkg/errs`（`ErrNotWiredYet` / `ErrInvalid` / `ErrConflict` / `ErrForbidden` / `ErrNotFound`）
  - `internal/pkg/notify`（`Message` / `Severity*` / `Sender` / `NewXxxSender`）
  - `internal/pkg/prom`（`IncAlertEvent`）
  - `internal/pkg/tunnel`（`HostMetricPoint`——仅 `NoopHostMetricIngester.Push` 签名）
- **外部库**：`context` / `encoding/json` / `errors` / `fmt` / `log/slog` / `strconv` / `strings` / `time`
- **被调用方**：`pipeline.go`（`RecordFiring` / `SystemResolveIncident` / `MaybeNotify`）、`retry.go`（`Notifier` / `ChannelResolver`）、HTTP handler（`ListIncidents` / `AckIncident` / `ResolveIncident` / `SilenceIncident` / `CreateRule` / `UpdateRule` 等）、`cmd` 装配（`SetInvestigator` / `SetWorkflowDispatcher`）
- **依赖本包**：`repo.go`（`Repo`）、`router.go`（`ChannelResolver`）、`inhibit.go`（`Inhibitor`）、`rules.go`（`defaultScopeForKind` / `scopeAllowedForKind`）、`evaluators_phaseA.go`（`metricExprFor`）、`burn_rate_sli.go`（`normalizeBurnRate*`）

## 6. 并发与资源管理

- `Usecase` 无锁、无 inflight 状态——`RecordFiring` 的 race 由 DB unique 约束 + race recovery 处理
- **Race recovery**：`CreateIncident` 失败时再次 `GetIncidentByDedupeKey`——并发两 goroutine 同时 Create，第二个 unique 冲突，re-fetch 走 bump 路径
- `investigator.InvestigateAsync` / `workflowDispatcher.OnAlertFired` 契约要求非阻塞——实现内部 goroutine 化
- `MaybeNotify` 内 per-channel 投递串行——避免并发投递导致 delivery 行乱序

## 7. 设计模式与亮点

- **dedupe key 驱动 incident 生命周期**：host→`host:<id>:<rule>`、pipeline→`pipeline:<rule>`——稳定标识，跨重启复现
- **Race recovery**：`CreateIncident` unique 冲突 → re-fetch + bump——并发安全且不丢 firing
- **isNew vs isReopen 语义**：investigator 仅 isNew（避免 reopen flap 烧 LLM）；workflow isNew OR isReopen（reopen 是真实复发，remediation 应再跑）
- **事件写入失败不阻断**：`RecordFiring` 的 event 写入 Warn 不 return err——incident 行已持久化，事件是时间线辅助
- **silence / cooldown / dampening / inhibition 四级闸门**：silence（最高）→ cooldown → dampening（发送策略）→ inhibition → 投递；每级失败写 `repeat_suppressed` / `inhibited` 事件，时间线透明
- **发送策略 dampening**：`NotifyWindowSeconds` + `NotifyMinFires`——"窗口内 ≥N 次 firing 才通知"，避免告警风暴
- **seeded placeholder 跳过**：`channelHasDestination` 检测 config 无 endpoint 的 seeded channel，跳过投递——避免误导 "notification_sent" 事件
- **`BuildSenderFromChannel` typed sender**：per ChannelType 构造对应 sender——UI 创建的 channel 真正投递（之前只 env-config channel by name 投递，UI channel 静默 no-op）
- **metric_threshold → metric_raw 编译**：UI-only entry form 在 `buildRuleRow` 编译为 metric_raw——一个 evaluator、一个存储 shape
- **`compileMetricThresholdExpr` `and on(device_id)`**：per-device AND join——同设备的多个条件 AND，不同设备独立
- **`createEvent` 集中化**：所有事件写入走同一入口，喂同一 `alert_events_total` 计数器——metric_raw 规则可统一告警"通知投递失败"
- **nil-safe 可选注入**：`investigator` / `workflowDispatcher` nil 时 RecordFiring 跳过——fresh install 未配 LLM 也能跑

## 8. 注意事项

- **`RecordFiring` 不做通知**：只 upsert + 事件 + 触发 investigator/workflow；通知由调用方调 `MaybeNotify`
- **`MaybeNotify` 闸门顺序**：silence → cooldown → dampening → inhibition → 投递；每级 return 前写对应事件
- **investigator 仅 isNew**：避免 reopen flap 烧 LLM 账单；reopen 是已诊断过的复发
- **workflow isNew OR isReopen**：reopen 是真实复发（条件已恢复后又触发），remediation 应再跑
- **race recovery**：`CreateIncident` unique 冲突 → re-fetch + bump；不丢 firing
- **发送策略 window [60, 1440*60]**（1min ~ 24h）、threshold [1, 100]——UI 下拉框对齐
- **`channelHasDestination` 检查 endpoint/url**：seeded placeholder（`{}` config）返回 false，跳过投递
- **`BuildSenderFromChannel` endpoint 优先 `cfg["endpoint"]`**：`url` 是 defensive legacy key
- **`SystemResolveIncident` 已 Resolved 返 (false, nil)**：恢复无 prior firing 不是错误
- **`parseSilenceUntil` 三种格式**：duration（`30m`）/ RFC3339 / unix seconds
- **`validateFiring` host scope 必须有 device_id**：避免脏数据
- **`buildDedupeKey` 形式**：host→`host:<id>:<rule>`、pipeline→`pipeline:<rule>`；改格式要同步 `inhibit.go` 的前缀匹配
- **`NoopHostMetricIngester`**：Phase-3 后占位，`Push` 永返 nil；legacy edge 的 push_host_metrics 仍 200，但 cloud Prom 是 canonical 路径
