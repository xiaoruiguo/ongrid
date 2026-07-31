# `repo.go` 技术实现文档

> 源文件：`internal/manager/data/alert/store/repo.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/alert/store`

## 1. 概述

本文件是 alert store 的核心 CRUD 实现，覆盖 Incident / Event / Silence / Rule / Channel / Delivery 六张表的全部 biz.Repo + Store 接口方法。关键设计：Incident 状态机由 `UpdateIncidentStatus` 分支 stamp 不同时间戳列；`BumpIncidentFiring` 与 `ReopenIncident` 用 `gorm.Expr` 原子自增 `event_count` 并刷新 summary/value/threshold；`DeleteRule` 用 `Unscoped()` 硬删以释放 rule_key；`CountRulesReferencingChannel` 用 LIKE 模式跨 JSON 列查 channel 引用。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/alert`
- **依赖方向**：被 `cmd/ongrid` 装配；依赖 `internal/manager/biz/alert`（接口与 filter）、`internal/manager/model/alert`、`internal/pkg/errs`、`gorm.io/gorm`。

## 3. 关键类型与接口

```go
// Repo 是 biz/alert.Repo 的 GORM 实现。
type Repo struct {
    db *gorm.DB
}

// 编译期接口断言
var _ biz.Repo = (*Repo)(nil)
var _ Store = (*Repo)(nil)  // 在 types.go 中
```

## 4. 关键函数与流程

### Incident

#### `ListIncidents` / `CountIncidents`
- **签名**：`ListIncidents(ctx, f biz.IncidentFilter) ([]*model.Incident, error)` + `CountIncidents(ctx, f) (int64, error)`
- **职责**：按 filter 列出 / 计数 incident，`last_fired_at DESC, id DESC` 排序。
- **关键约束**：CountIncidents 必须镜像 ListIncidents 的 filter 谓词，否则"badge says 1 but page shows 9" bug 复现。
- **filter 字段**：Status / Severity / RuleKey / DeviceID / Limit / Offset。

#### `GetIncidentByID` / `GetIncidentByDedupeKey`
- 按 id 或 dedupe_key 取 incident；`gorm.ErrRecordNotFound` → `errs.ErrNotFound`。

#### `CreateAlertIncident` / `CreateIncident`
- 两者同义，`CreateIncident` 委托 `CreateAlertIncident`；保留双名让 biz.Repo 接口与 data 层 Store 各自可读。`in == nil` → `ErrInvalid`。

#### `UpdateIncident` / `UpdateIncidentStatus`
- `UpdateIncident`：全字段 Updates（`in == nil` → ErrInvalid；`RowsAffected == 0` → ErrNotFound）。
- `UpdateIncidentStatus`：状态机分支，按 status stamp 不同列：
  - `Acknowledged` → `acknowledged_at` + `acknowledged_by`
  - `Silenced` → `silenced_until`
  - `Resolved` → `resolved_at` + `resolved_by`
  - 校验通过 `validateIncidentStatus`，无效 → `ErrInvalid`。
  - `occurredAt.IsZero()` → `time.Now().UTC()`。

#### `BumpIncidentFiring`
- **职责**：re-firing 时 `event_count + 1`（`gorm.Expr` 原子自增）+ 刷新 `last_fired_at` / summary / value / threshold。
- **关键约束**：**不动 status** —— Ack / Silence 在后续 firing 中保持（rules 3 and 4）。resolved → open 用 `ReopenIncident`。

#### `ReopenIncident`
- **职责**：resolved → open，清空 `silenced_until` / `resolved_at` / `resolved_by`，同 BumpIncidentFiring 刷新计数与字段。

#### `MarkIncidentNotified`
- 记录最近 notify 尝试时间，firing-path cooldown 门控读此列抑制重复通知。

### Event

#### `CreateEvent`
- `ev == nil` → ErrInvalid；Create。

#### `CountEventsByType`
- 按 `event_type` + `created_at >= since` 计数；可选 severity / rule 过滤。rule 过滤用子查询保留 `incident_id` 索引可用性。

#### `ListEventsByIncident`
- 按 incident_id 列事件，`created_at DESC, id DESC`，可选 limit。

### Silence

#### `CreateSilence` / `GetSilenceByID` / `ListSilenceRows`
- 标准 CRUD；`ListSilenceRows` 支持 status / ActiveAt 窗口过滤。

#### `ListActiveSilences`
- 返回 `status = active` 且 `[starts_at, ends_at)` 覆盖 `at` 的 silences；caller 在内存按 scope/edge/rule 二次过滤。

#### `UpdateSilenceStatus`
- 状态机校验 `validateSilenceStatus`；可选 `cancelled_by` / `cancelled_at`。

### Rule

#### `CreateRule` / `GetRuleByID` / `GetRuleByKey`
- 标准 CRUD；`GetRuleByKey` 空 key → ErrInvalid。

#### `ListRuleRows` / `ListRules`
- 支持 Enabled / ScopeType / SourceType 过滤；`ListRules` 是 `ListRuleRows` 的 scope-only 简版。

#### `ListEnabledRulesByScope` / `ListAllEnabledRules`
- `ListAllEnabledRules` 单查询全量启用 rule，PR-E CachedRulesProvider 按 Kind in-memory bucketing；`ListEnabledRulesByScope` 保留给测试与 scope-targeted UI。

#### `UpsertBuiltinRule`
- **职责**：seed 路径用，按 RuleKey 插入或 no-op。**built-in seed 永不覆盖 admin 编辑过的行**，存在即返回 existing。

#### `UpdateRule` / `UpdateRuleEnabled` / `DeleteRule`
- `UpdateRule`：map-based Updates，注释明示必须包含 `notify_window_seconds` / `notify_min_fires` / `notify_channel_ids_json` 三列，否则静默丢失发送策略编辑。
- `DeleteRule`：`Unscoped()` 硬删以释放 rule_key 复用；biz 层在到达此方法前阻止 built-in rule 删除。

### Channel

#### `CreateChannel` / `GetChannelByID` / `GetChannelByName`
- 标准 CRUD；`GetChannelByName` 供 firing-path 填充 `notification_deliveries.channel_id`。

#### `ListChannelRows` / `ListChannels` / `ListEnabledChannels`
- `ListChannels` 是 biz-facing wrapper，把 `biz.ChannelFilter` 翻译为 storage `ChannelFilter`，避免 service 层 import sqlite 包。
- `ListEnabledChannels` 单查询全量启用 channel，Notification Router 每 incident 消费。

#### `UpsertChannelByName`
- **职责**：`SeedChannelsFromConfig` 每 boot 同步 env 配置用。`name == ""` → ErrInvalid。先 First 探测，不存在则 Create，存在则 Updates（channel_type / enabled / config_json）。保留历史 delivery 的 channel_id FK。

#### `UpdateChannel` / `UpdateChannelEnabled` / `DeleteChannel` / `PurgeLegacyLogChannels`
- `UpdateChannel`：map-based Updates，含 `match_severity_min` / `match_scope_types`。
- `DeleteChannel`：软删（gorm DeletedAt）。
- `PurgeLegacyLogChannels`：软删 legacy "log" 类型 channel，幂等；pinned 到此 channel 的 rule 回退全局默认 channel 集。

#### `CountRulesReferencingChannel`
- **职责**：DeleteChannel 守卫，防止 admin 孤立 pinned rule。
- **实现**：SQLite GORM 驱动无原生 JSON helper，rule 数量小（< 1k lifetime），用 LIKE 模式扫描 `notify_channel_ids_json`。anchor 4 种 JSON list 形状（`[id]` / `[id,...` / `...,id]` / `...,id,...`）避免 id 1 撞 id 11。

### Delivery

#### `CreateDelivery` / `GetDeliveryByID` / `ListDeliveryRows`
- 标准 CRUD；filter 支持 IncidentID / ChannelID / Status。

#### `ListRetriableDeliveries`
- 返回 `status = failed` 且 `attempt_count < maxAttempts` 且 `finished_at` 早于 `before` 的 delivery，retry worker 每 tick 消费。`limit <= 0` → 100。

#### `UpdateDeliveryStatus`
- 状态机校验 `validateDeliveryStatus`；填充 status / attempt_count / response_json / error_message / sent_at / finished_at / provider_message_id（可选）。

## 5. 依赖关系

- **内部包**：`internal/manager/biz/alert`（接口、`IncidentFilter`、`ChannelFilter`）、`internal/manager/model/alert`、`internal/pkg/errs`
- **外部库**：`gorm.io/gorm`
- **被调用方**：`internal/manager/biz/alert` usecase；`cmd/ongrid` 通过 `NewBizRepo` / `provider.go` 装配；`seed.go` / `seed_rules.go` 调用 `UpsertChannelByName` / `UpsertBuiltinRule`。

## 6. 并发与资源管理

- **无显式锁**：依赖 gorm 与 DB 索引。
- **原子自增**：`BumpIncidentFiring` / `ReopenIncident` 用 `gorm.Expr("event_count + ?", 1)` 避免 read-modify-write 竞态。
- **ctx 透传**：所有方法首参 ctx。
- **Unscoped 谨慎使用**：仅 `DeleteRule` 用 Unscoped 硬删以释放 rule_key；其余删除走软删。

## 7. 设计模式与亮点

- **状态机分支 stamp**：`UpdateIncidentStatus` 按 status 分支写不同时间戳列，集中状态转换语义。
- **BumpIncidentFiring 不动 status**：注释明示 Ack/Silence 跨 firing 保持，匹配 rules 3 and 4。
- **CountIncidents 镜像 ListIncidents filter**：修复"badge says 1 but page shows 9" bug，filter 谓词必须同步。
- **UpsertBuiltinRule 不覆盖 admin 编辑**：seed 路径幂等且保护 admin 编辑。
- **UpsertChannelByName 保留 FK**：disabled channel 不删而是 upsert enabled=false，保留历史 delivery 的 channel_id。
- **CountRulesReferencingChannel LIKE 锚定**：4 种 JSON list 形状 anchor 避免 id 1 撞 id 11。
- **ListChannels biz wrapper**：filter 类型翻译隔离 service 层与 storage 层。
- **ListAllEnabledRules 单查询 + in-memory bucketing**：避免 N 次 scope 查询，PR-E CachedRulesProvider 设计。

## 8. 注意事项

- **UpdateRule 字段必须完整**：注释明示漏掉 `notify_window_seconds` / `notify_min_fires` / `notify_channel_ids_json` 会静默丢失发送策略编辑。扩展 Rule 模型需同步更新 updates map。
- **DeleteRule 仅 custom rule**：biz 层在到达此方法前阻止 built-in rule 删除；repo 层不重复校验。
- **DeleteRule Unscoped**：硬删释放 rule_key；如需保留审计请用 DisableRule。
- **CountRulesReferencingChannel LIKE 扫描**：rule 数量 < 1k lifetime 才安全；超出需切换至原生 JSON helper（MySQL JSON_CONTAINS）。
- **BumpIncidentFiring 不动 status**：resolved → open 必须用 `ReopenIncident`，不能用 Bump。
- **ListRetriableDeliveries limit 默认 100**：caller 不传 limit 时兜底 100，避免全表扫。
- **状态机校验**：`validateIncidentStatus` / `validateSilenceStatus` / `validateDeliveryStatus` 在 types.go 中，扩展状态需同步更新。
- **时区**：所有时间戳统一 UTC。
