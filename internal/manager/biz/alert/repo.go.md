# `repo.go` 技术实现文档

> 源文件：`internal/manager/biz/alert/repo.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/alert`

## 1. 概述

本文件定义 alert 子域 biz 层的持久化契约 `Repo` 接口、列表过滤器 `IncidentFilter` / `ChannelFilter`、以及 `Clock` 抽象。`Repo` 覆盖四大关注点：(1) incident 生命周期读 + 状态流转（ack/resolve/silence）；(2) firing-path upsert（按 dedupe key 查找、新建、bump 或 reopen）；(3) 活跃 silence 匹配；(4) delivery 投递记录——每次通知尝试都产生一行，由 retry worker 排空。同时声明 channel + rule 的 CRUD 接口供 admin UI 使用。实现位于 `internal/manager/data/alert/store`。

## 2. 包信息

- **包名**：`alert`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被本包 `usecase.go` / `retry.go` / `router.go` 等所有文件消费；依赖 `internal/manager/model/alert` + `context` + `time`

## 3. 关键类型与接口

```go
type IncidentFilter struct {
    Status    string
    Severity  string
    RuleKey   string
    DeviceID  *uint64
    Limit     int
    Offset    int
}

type ChannelFilter struct {
    Enabled     *bool
    ChannelType string
    Limit       int
    Offset      int
}

type Repo interface {
    // Incident 生命周期
    ListIncidents(ctx, filter IncidentFilter) ([]*model.Incident, error)
    CountIncidents(ctx, filter IncidentFilter) (int64, error)
    GetIncidentByID(ctx, id uint64) (*model.Incident, error)
    UpdateIncidentStatus(ctx, id uint64, status string, actorID *uint64, occurredAt time.Time) error
    CreateEvent(ctx, ev *model.Event) error
    ListEventsByIncident(ctx, incidentID uint64, limit int) ([]*model.Event, error)
    CountEventsByType(ctx, eventType string, since time.Time, filterRule, filterSeverity string) (int64, error)
    CreateSilence(ctx, in *model.Silence) error

    // Firing path
    GetIncidentByDedupeKey(ctx, dedupeKey string) (*model.Incident, error)
    CreateIncident(ctx, in *model.Incident) error
    BumpIncidentFiring(ctx, id uint64, firedAt time.Time, summary string, value, threshold *float64) error
    ReopenIncident(ctx, id uint64, firedAt time.Time, summary string, value, threshold *float64) error
    MarkIncidentNotified(ctx, id uint64, at time.Time) error

    // Silence 匹配
    ListActiveSilences(ctx, at time.Time) ([]*model.Silence, error)

    // Channel + delivery
    GetChannelByName / GetChannelByID / ListEnabledChannels / ListChannels
    CreateChannel / UpdateChannel / DeleteChannel
    CountRulesReferencingChannel(ctx, channelID uint64) (int64, error)
    CreateDelivery / UpdateDeliveryStatus / ListRetriableDeliveries

    // Rule reads + admin writes
    ListEnabledRulesByScope / ListAllEnabledRules / GetRuleByKey / GetRuleByID
    UpsertBuiltinRule / ListRules / CreateRule / UpdateRule / UpdateRuleEnabled / DeleteRule
}

type Clock interface {
    Now() time.Time
}

type realClock struct{}
func (realClock) Now() time.Time { return time.Now().UTC() }
```

`CountIncidents` 故意忽略 `Limit/Offset`——驱动 sidebar 的"全局 open count"徽标不应被分页截断。`CountEventsByType` 的 `filterRule` / `filterSeverity` 用于 `event_internal` evaluator 检测"过去 1 小时内 ≥5 条 silenced 事件"等聚合告警。

## 4. 关键函数与流程

本文件不含函数实现，仅声明接口与两个简单类型。

### `realClock.Now`

- **签名**：`func (realClock) Now() time.Time`
- **职责**：返回当前 UTC 时间
- **流程**：`time.Now().UTC()`
- **错误处理**：无

## 5. 依赖关系

- **内部包**：`internal/manager/model/alert`（仅类型）
- **外部库**：标准库 `context` / `time`
- **被调用方**：本包内所有文件（`usecase.go` 是最大消费者）；实现位于 `data/alert/store`
- **测试 fake**：因 `Repo` 接口庞大，本包还声明了多个窄接口（`channelLister` / `IncidentLookup` / `RuleSource`）让单测 fake 不必满足完整 `Repo`

## 6. 并发与资源管理

- 接口本身无并发约束；实现的并发安全由 `data/alert/store` 负责（典型实现是 gorm，本身线程安全）
- `Clock` 抽象让 usecase 可注入 fake clock 做时间相关的单测（如 cooldown / silence_until 解析）

## 7. 设计模式与亮点

- **接口聚合**：`Repo` 把 incident/event/silence/channel/delivery/rule 六类操作聚合到一个接口，避免 usecase 注入六个依赖；同时通过窄接口（`IncidentLookup` / `channelLister` / `RuleSource`）让单测可只 fake 需要的部分
- **CountIncidents 忽略分页**：`IncidentFilter.Limit/Offset` 仅作用于 `ListIncidents`；`CountIncidents` 忽略它们，返回真实总数——避免分页把徽标数字压低
- **CountEventsByType 双过滤**：`filterRule` / `filterSeverity` 是可选的窄化字段，让 `event_internal` evaluator 可以做"过去 1h 内 rule=X 的 silenced 事件 ≥5"这类聚合查询，无需新建专用接口
- **Delivery + Event 双写**：`CreateDelivery` 写投递行（pending），`UpdateDeliveryStatus` 推进状态；同时 usecase 在 `FinishDelivery` 写 `EventTypeNotificationSent/Failed` 事件——delivery 是机械记录，event 是给操作员看的时间线
- **`CountRulesReferencingChannel` 防孤儿**：`DeleteChannel` 前用此方法检查是否有 rule 把此 channel pin 在 `notify_channel_ids_json`，有则拒绝删除——避免 rule 的 pinned 路由被孤儿化
- **Clock 抽象**：UTC 强制；测试可注入 fake clock 控制 cooldown / silence_until 解析的"现在"
- **Soft delete 保留审计**：`DeleteChannel` 走 gorm 的 `DeletedAt` 软删除，保留 `notification_deliveries.channel_id` 外键完整性 + 审计历史

## 8. 注意事项

- **`UpsertBuiltinRule` 是内置规则专用**：boot 时 seeding 用；admin 不能用它写自定义规则，自定义规则走 `CreateRule`
- **`ListRetriableDeliveries` 的 `before` 参数**：retry worker 用 `now - backoffPerAttempt` 作为 `before`，只列出"已过最小退避时间"的失败 delivery
- **`GetIncidentByDedupeKey` 的错误语义**：未找到返回 `errs.ErrNotFound`（不返回 nil, nil），调用方用 `errors.Is(err, errs.ErrNotFound)` 判定
- **`UpdateIncidentStatus` 是状态机迁移的唯一入口**：ack/resolve/silence/reopen 都走它；`ReopenIncident` / `BumpIncidentFiring` 是 firing-path 专用，会同时更新 `last_fired_at` / `summary` / `value` / `threshold`，不混淆状态机
- **`CountEventsByType` 的 `since` 是闭区间**：典型用法 `since = now - 1h`
- **Channel 的 `MatchSeverityMin` / `MatchScopeTypes` 是字符串字段**：由 `router.go` 解析；`Repo` 不解释它们的语义
