# `retry.go` 技术实现文档

> 源文件：`internal/manager/biz/alert/retry.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/alert`

## 1. 概述

本文件是 alert 子域的 `RetryWorker`——失败通知投递的排空循环。每 tick（默认 30s）从 `notification_deliveries` 表列出"未达 `MaxAttempts` 上限且已过线性退避时间"的失败行，重新解析 incident + channel，重建 `notify.Message`，再调 `Notifier.Send`，按结果推进 delivery 状态 + 写 incident event 喂 `alert_events_total` 计数。退避策略保守：线性 `attempt_count * backoffPerAttempt`；incident 已 resolved 时短路标记成功；channel 被 operator 禁用时烧光预算停止重试。所有 IO 错误 Warn 不阻断循环。

## 2. 包信息

- **包名**：`alert`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 `cmd` / main 装配调用 `Loop`；依赖 `internal/manager/model/alert` + `internal/pkg/notify` + `internal/pkg/prom`

## 3. 关键类型与接口

```go
type RetryWorkerOpts struct {
    Repo              Repo
    Notifier          Notifier
    Resolver          ChannelResolver
    Usecase           *Usecase
    MaxAttempts       uint32
    BackoffPerAttempt time.Duration
    Tick              time.Duration
    Log               *slog.Logger
    Now               func() time.Time
}

type RetryWorker struct {
    repo              Repo
    notifier          Notifier
    resolver          ChannelResolver
    uc                *Usecase
    maxAttempts       uint32
    backoffPerAttempt time.Duration
    tick              time.Duration
    log               *slog.Logger
    now               func() time.Time
}
```

Sentinel 默认值：`MaxAttempts=5`、`BackoffPerAttempt=1*time.Minute`、`Tick=30*time.Second`。

## 4. 关键函数与流程

### `NewRetryWorker`

- **签名**：`func NewRetryWorker(opts RetryWorkerOpts) *RetryWorker`
- **职责**：构造 worker，应用默认值
- **流程**：
  1. MaxAttempts=0 → 5
  2. BackoffPerAttempt<=0 → 1min
  3. Tick<=0 → 30s
  4. Log==nil → `slog.Default()`
  5. Now==nil → `func() time.Time { return time.Now().UTC() }`
  6. 返回 `&RetryWorker{...}`
- **错误处理**：无

### `Loop`

- **签名**：`func (w *RetryWorker) Loop(ctx context.Context) error`
- **职责**：tick 驱动主循环
- **流程**：
  1. `w.repo == nil || w.notifier == nil` → 返回 nil（未装配）
  2. `time.NewTicker(w.tick)` + defer Stop
  3. 首次 `w.runOnce(ctx)` 立即执行
  4. for：select ctx.Done 返回 nil / tick.C 触发 `runOnce`
- **错误处理**：单 cycle 错误 Warn，不退出循环；只在 ctx.Done 返回

### `RunOnce`

- **签名**：`func (w *RetryWorker) RunOnce(ctx context.Context)`
- **职责**：暴露给测试的单 cycle 入口

### `runOnce`

- **签名**：`func (w *RetryWorker) runOnce(ctx context.Context)`
- **职责**：单 cycle 排空
- **流程**：
  1. `now := w.now()`
  2. `w.repo.ListRetriableDeliveries(ctx, w.maxAttempts, now.Add(-w.backoffPerAttempt), 200)`——`before = now - backoffPerAttempt` 是首次退避门限
  3. err → Warn + return
  4. per-row：`w.retryOne(ctx, d, now)`
- **错误处理**：list 失败 Warn + return；per-row 错误在 `retryOne` 内处理

### `retryOne`

- **签名**：`func (w *RetryWorker) retryOne(ctx context.Context, d *model.Delivery, now time.Time)`
- **职责**：单 delivery 重试
- **流程**：
  1. nil 守卫：`d == nil || d.IncidentID == nil` → return
  2. **线性退避**：`d.FinishedAt != nil` 时算 `minWait = attempt_count * backoffPerAttempt`；`now - FinishedAt < minWait` → 跳过（未到下次重试时间）
  3. `incident := w.repo.GetIncidentByID(ctx, *d.IncidentID)`；err → Warn + return
  4. **incident 已 resolved 短路**：`incident.Status == IncidentStatusResolved` → `UpdateDeliveryStatus(Success, attempt+1, "incident resolved before retry")` + return（操作员不再需要此通知）
  5. `channel := w.repo.GetChannelByID(ctx, d.ChannelID)`；err → Warn + return
  6. **channel 被 disabled 烧光预算**：`!channel.Enabled` → `UpdateDeliveryStatus(Failed, maxAttempts, "channel X disabled")` + return（停止重试）
  7. `msg := buildIncidentMessage(incident, now)`
  8. `sendErr := w.notifier.Send(ctx, msg, channel.Name)`
  9. status = Success / Failed；eventType = NotificationSent / NotificationFailed
  10. `UpdateDeliveryStatus(d.ID, status, attempt+1, nil, nil, errMsg, &sentAt, &finishedAt)`；err → Warn
  11. `CreateEvent(Event{IncidentID, eventType, StatusAfter: incident.Status, Severity, Title, ActorType: System, Reason: "<channel>: retry attempt N[: err]"})`
  12. event 创建成功 → `prom.IncAlertEvent(eventType, severity, rule)`（镜像 `Usecase.createEvent`，让重试路径的 sent/failed 也上同一计数器）
  13. `sendErr == nil` → `MarkIncidentNotified(incident, now)`
- **错误处理**：每个 IO 失败 Warn 不阻断；incident-resolved / channel-disabled 是短路路径，不是错误

### `buildIncidentMessage`

- **签名**：`func buildIncidentMessage(incident *model.Incident, now time.Time) notify.Message`
- **职责**：从 incident 重建 `notify.Message`
- **流程**：
  1. `severity := notify.Severity(incident.Severity)`；空 → `SeverityWarning`
  2. labels：`{rule, incident_id}`；有 DeviceID 加 `device_id`
  3. `subject := incident.Summary ?? incident.Title`
  4. `occurredAt := incident.LastFiredAt ?? now`
  5. 返回 `Message{Subject, Severity, Source: incident.ScopeType, DedupeKey, OccurredAt, Labels}`

### `ptrString`

- **签名**：`func ptrString(s string) *string { return &s }`
- **职责**：工具函数，取字符串指针

## 5. 依赖关系

- **内部包**：`internal/manager/model/alert`（`Delivery` / `Incident` / `Event` / 状态常量）、`internal/pkg/notify`（`Message` / `Severity`）、`internal/pkg/prom`（`IncAlertEvent`）
- **外部库**：`context` / `fmt` / `log/slog` / `time`
- **被调用方**：`cmd` / main 装配；`Loop` 是 alert 子域的次要驱动（与 `PipelineEvaluator.Loop` 并行）
- **依赖本包**：`usecase.go`（`Notifier` / `ChannelResolver` / `Repo`）、`repo.go`（`Repo` 接口）

## 6. 并发与资源管理

- `RetryWorker.Loop` 单 goroutine 驱动；无内部并发
- 每次重试走调用方 ctx；`Notifier.Send` 的超时由 Notifier 实现控制
- 无共享状态、无锁
- ticker 释放：`defer tick.Stop()`

## 7. 设计模式与亮点

- **保守线性退避**：`attempt_count * backoffPerAttempt`——简单可预测；指数退避会让远期重试间隔过长，线性更适合"操作员修了 channel 后尽快重试"的场景
- **incident-resolved 短路**：操作员不再需要已恢复 incident 的通知——直接标记成功，避免无意义重试
- **channel-disabled 烧光预算**：operator 禁用 channel 时立即停止重试（烧光 `maxAttempts`），让 delivery 行进入终态；避免 disabled channel 的 delivery 永远挂在重试队列
- **镜像 `createEvent`**：重试路径也喂 `alert_events_total`——首次失败 + 重试 sent/failed 都进同一计数器，metric_raw 规则可统一告警"通知投递失败"
- **`before = now - backoffPerAttempt`**：`ListRetriableDeliveries` 的 `before` 参数是首次退避门限，让 DB 过滤掉"刚失败还没到下次重试时间"的行
- **`Reason` 带 attempt 编号**：`<channel>: retry attempt N[: err]`——操作员在时间线能看到第几次重试 + 失败原因
- **nil 守卫贯穿**：`d == nil || d.IncidentID == nil` 在最前面，避免空指针

## 8. 注意事项

- **默认 MaxAttempts=5、BackoffPerAttempt=1min、Tick=30s**：5 次重试 × 1min 退避 = 最长约 5min 重试窗口
- **`ListRetriableDeliveries` 的 limit=200**：单 cycle 最多处理 200 行；大 backlog 需多 cycle 排空
- **线性退避不是指数**：远期重试间隔不增长；如果 channel 持续失败，5 次重试都在 5min 内完成
- **incident-resolved 标记 Success**：不是 Failed——操作员不再需要通知，delivery 算"使命完成"
- **channel-disabled 标记 Failed 且烧光预算**：让 delivery 行进入终态，不挂在重试队列
- **`buildIncidentMessage` 用 `incident.LastFiredAt`**：不是 `now`——保留原始触发时间，让通知 Subject 的时间是 incident 时间而非重试时间
- **`MarkIncidentNotified` 仅在 sendErr==nil 时调**：失败重试不更新 `last_notified_at`，避免 cooldown 闸门被错误重置
- **`prom.IncAlertEvent` 仅在 event 创建成功时调**：失败的事件写入不喂计数器，避免计数器膨胀超过实际行数
- **`Reason` 字段在 event 里**：操作员时间线可见"重试第 N 次"或"重试第 N 次: <err>"——便于诊断
