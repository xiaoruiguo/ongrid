# `model.go` 技术实现文档

> 源文件：`internal/manager/model/alert/model.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/model/alert`

## 1. 概述

本文件是 alert 子域的核心 schema：定义 Incident（事件）/ Event（事件流）/ Silence（屏蔽）/ Rule（规则）/ Channel（通知渠道）/ Delivery（投递记录）六类实体及配套的 status / event_type / rule_kind / channel_type 常量集合。设计要点：rule kind 体系在 Phase-3-final 收敛——`metric_threshold` 仅作 UI 输入形态，保存时 biz 层重写为 `metric_raw`；legacy kind 通过 `legacyKindAliases` 别名表 + 迁移脚本归一化到 `metric_raw`，保证 evaluator 不需要识别旧名。红线：rule_kind 与 channel_type 命名跨 release 稳定（UI / dedupe key / 告警事件都引用）；`log` 通知渠道类型已删除（manager stdout 易失 + alert_events 已是审计源）。

## 2. 包信息

- **包名**：`alert`
- **所属模块**：`internal/manager/model/`
- **依赖方向**：被 `manager/biz/alert` 与 `manager/data/alert/store` 调用；依赖 `gorm.io/gorm`、`encoding/json`、`time`

## 3. 关键类型与接口

```go
// 事件流 Status
const (
    StatusOpen         = "open"
    StatusAcknowledged = "acknowledged"
    StatusSilenced     = "silenced"
    StatusResolved     = "resolved"
)

// EventType 常量（节选）
const (
    EventTypeFiring             = "firing"
    EventTypeAcknowledged       = "acknowledged"
    EventTypeResolved           = "resolved"
    EventTypeReopened           = "reopened"
    EventTypeSilenced           = "silenced"
    EventTypeNotificationSent   = "notification_sent"
    EventTypeNotificationFailed = "notification_failed"
    EventTypeInhibited          = "inhibited"
    EventTypeAIInitialDiagnosis = "ai_initial_diagnosis" // P2 AI 主动调查
    EventTypeRepeatSuppressed   = "repeat_suppressed"    // dampening 网关
)

// Rule Kind 体系
const (
    RuleKindMetricThreshold = "metric_threshold" // UI-only INPUT，保存时 biz 重写为 metric_raw
    RuleKindMetricAnomaly   = "metric_anomaly"
    RuleKindMetricForecast  = "metric_forecast"
    RuleKindMetricBurnRate  = "metric_burn_rate"
    RuleKindMetricRaw       = "metric_raw"
    RuleKindLogMatch        = "log_match"        // Phase-B UI 可保存，evaluator 未就绪
    RuleKindLogVolume       = "log_volume"
    RuleKindTraceLatency    = "trace_latency"
    RuleKindTraceErrorRate  = "trace_error_rate"
)

// Channel 类型
const (
    ChannelTypeWebhook  = "webhook"
    ChannelTypeSlack    = "slack"
    ChannelTypeFeishu   = "feishu"
    ChannelTypeDingTalk = "dingtalk"
    ChannelTypeWeCom    = "wecom"    // 2026-05 新增
    ChannelTypeTelegram = "telegram"
)

type Incident struct {
    ID              uint64         `gorm:"column:id;primaryKey;autoIncrement"`
    RuleID          *uint64        `gorm:"column:rule_id;index:idx_alert_incidents_rule_id"`
    DeviceID        *uint64        `gorm:"column:device_id;index:idx_alert_incidents_device_id"`
    Title, Scope, ScopeType, Rule, RuleName, Severity, Status string
    Summary         string         `gorm:"column:summary;type:text;not null"`
    Description     string         `gorm:"column:description;type:text;not null"`
    DedupeKey       string         `gorm:"column:dedupe_key;size:191;not null;default:'';uniqueIndex"`
    Value           *float64
    Threshold       *float64
    LabelsJSON      string         `gorm:"column:labels_json;type:text;not null"`
    AnnotationsJSON string         `gorm:"column:annotations_json;type:text;not null"`
    RunbookURL      string         `gorm:"column:runbook_url;size:512;not null;default:''"`
    EventCount      uint64         `gorm:"column:event_count;not null;default:0"`
    FirstFiredAt    time.Time      `gorm:"column:first_fired_at;not null;index:idx_alert_incidents_first_fired_at"`
    LastFiredAt     time.Time      `gorm:"column:last_fired_at;not null;index:idx_alert_incidents_last_fired_at"`
    LastNotifiedAt  *time.Time     `gorm:"column:last_notified_at"`
    SilencedUntil   *time.Time     `gorm:"column:silenced_until"`
    AcknowledgedAt  *time.Time     `gorm:"column:acknowledged_at"`
    AcknowledgedBy  *uint64        `gorm:"column:acknowledged_by"`
    ResolvedAt      *time.Time     `gorm:"column:resolved_at"`
    ResolvedBy      *uint64        `gorm:"column:resolved_by"`
    SourceType      string         `gorm:"column:source_type;size:32;not null;default:''"`
    CreatedAt       time.Time      `gorm:"column:created_at;autoCreateTime"`
    UpdatedAt       time.Time      `gorm:"column:updated_at;autoUpdateTime"`
    DeletedAt       gorm.DeletedAt `gorm:"column:deleted_at;index"`
}

type Event struct {
    ID             uint64         `gorm:"column:id;primaryKey;autoIncrement"`
    IncidentID     uint64         `gorm:"column:incident_id;not null;index:idx_alert_events_incident_created,priority:1"`
    EventType      string         `gorm:"column:event_type;size:24;not null;default:''"`
    StatusAfter    string         `gorm:"column:status_after;size:16;not null;default:''"`
    Severity       string         `gorm:"column:severity;size:16;not null;default:''"`
    Title          string         `gorm:"column:title;size:256;not null;default:''"`
    Message        *string        `gorm:"column:message;type:text"`
    ActorType      string         `gorm:"column:actor_type;size:16;not null;default:system"`
    ActorID        *uint64        `gorm:"column:actor_id"`
    OperatorUserID *uint64        `gorm:"column:operator_user_id"`
    SnapshotJSON   string         `gorm:"column:snapshot_json;type:text;not null"`
    Reason         string         `gorm:"column:reason;type:text;not null"`
    OccurredAt     time.Time      `gorm:"column:occurred_at"`
    CreatedAt      time.Time      `gorm:"column:created_at;autoCreateTime;index:idx_alert_events_incident_created,priority:2"`
    UpdatedAt      time.Time      `gorm:"column:updated_at;autoUpdateTime"`
    DeletedAt      gorm.DeletedAt `gorm:"column:deleted_at;index"`
}

type Silence struct { /* ... */ }
type Rule struct {
    ID uint64 `gorm:"column:id;primaryKey;autoIncrement"`
    RuleKey string `gorm:"column:rule_key;size:128;not null;default:'';uniqueIndex:idx_alert_rules_rulekey"`
    Kind    string `gorm:"column:kind;size:32;not null;default:'metric_raw';index:idx_alert_rules_kind"`
    // ...
    NotifyChannelIDsJSON *string `gorm:"column:notify_channel_ids_json;type:text"`
    NotifyWindowSeconds   int     `gorm:"column:notify_window_seconds;not null;default:0"`
    NotifyMinFires        int     `gorm:"column:notify_min_fires;not null;default:0"`
    // ...
}

type Channel struct {
    ID              uint64 `gorm:"column:id;primaryKey;autoIncrement"`
    Name            string `gorm:"column:name;size:128;not null;default:''"`
    ChannelType     string `gorm:"column:channel_type;size:32;not null;default:''"`
    Enabled         bool   `gorm:"column:enabled;not null;default:true"`
    ConfigJSON      string `gorm:"column:config_json;type:text;not null"`
    MatchSeverityMin string `gorm:"column:match_severity_min;size:16;not null;default:''"`
    MatchScopeTypes  string `gorm:"column:match_scope_types;size:128;not null;default:''"`
    // ...
}

type Delivery struct { /* ... */ }

type Labels map[string]string
type RuleCondition struct {
    Metric, Operator string
    Threshold        float64
    Window, For, Aggregator string
}
type SilenceMatcher struct {
    Field, Operator, Value string
}
```

Sentinel：`legacyKindAliases` map（pre-names / Phase-3 删除名 → `metric_raw`）。

## 4. 关键函数与流程

### `NormalizeKind`
- **签名**：`func NormalizeKind(k string) string`
- **职责**：归一化 kind 字符串
- **流程**：空 → `metric_raw`；命中 `legacyKindAliases` → 别名；否则原样返回
- **保留 metric_threshold**：API 接受 UI 友好形态输入；biz 层持久化前重写

### `IsKnownKind`
- **签名**：`func IsKnownKind(k string) bool`
- **职责**：判断系统是否识别该 kind（包含 UI-only 与 evaluator 未就绪的）
- **用途**：caller 写规则时拒绝拼写错误

### `IsEvaluableKind`
- **签名**：`func IsEvaluableKind(k string) bool`
- **职责**：判断 kind 是否有可用 evaluator
- **用途**：UI 可保存 log/trace kind（evaluator 待就绪）；engine 拒绝触发时返回 "coming soon" 日志而非报错
- **排除 metric_threshold**：到 evaluator 时已被重写为 metric_raw

### `Incident.Labels / Incident.Annotations`
- **签名**：`func (i Incident) Labels() (Labels, error)` / `Annotations()`
- **职责**：解析 JSON 为 Labels map；空返回空 map；解析失败返回 error

### `Rule.Conditions / Rule.Labels / Rule.Annotations`
- **签名**：`func (r Rule) Conditions() ([]RuleCondition, error)`
- **职责**：解析 JSON 为结构化字段；空字符串返回 nil slice

### `Channel.Config`
- **签名**：`func (c Channel) Config() (map[string]string, error)`
- **职责**：解析 ConfigJSON 为 map；空返回空 map

### `Silence.Matchers`
- **签名**：`func (s Silence) Matchers() ([]SilenceMatcher, error)`

### `parseLabels / parseLabelsPtr`
- 私有 helper：分别处理 `string` 与 `*string` 输入；nil 指针返回空 map

## 5. 依赖关系

- **内部包**：无（纯 schema 包）
- **外部库**：`gorm.io/gorm`、`encoding/json`、`time`
- **被调用方**：`manager/biz/alert` 下的 evaluator / notifier / router；`manager/data/alert/store` 的 Migrate

## 6. 并发与资源管理

- 本文件仅定义 schema，无锁 / channel / 缓存
- `gorm.DeletedAt` 提供 soft-delete，默认查询自动过滤

## 7. 设计模式与亮点

- **RuleKind 体系 Phase-3-final 收敛**：`metric_threshold` 仅 UI 输入；保存时 biz 重写为 `metric_raw`；DB / evaluator 永不见 metric_threshold
- **legacyKindAliases 别名表**：edge_absence / health_ingest / event_internal / 旧名都映射到 metric_raw；迁移脚本 + 别名双重保险
- **Phase-A vs Phase-B 区分**：metric 系列 evaluable；log/trace 系列 known 但不 evaluable（UI 可保存，engine 跳过并日志）
- **ChannelTypeWeCom 2026-05 新增**：Bot endpoint shape 与 DingTalk 一致（plain webhook + text payload），无需签名
- **ChannelTypeSlack removed `log`**：manager stdout 易失 + alert_events 已是审计源；UI 提示查"设置 → 告警事件"
- **NotifyChannelIDsJSON per-rule 渠道子集**：nil/empty → 走全局 severity/scope 路由；非空 → 仅指定 channel（仍受各 channel.enabled 约束）
- **NotifyWindowSeconds + NotifyMinFires dampening**：窗口内 < N 次触发不通知，仍记录 `repeat_suppressed` event；mixed (一零一非零) 在 biz 层拒绝
- **MatchSeverityMin 地板**：空 = 任意；warning = warning+critical；critical = 仅 critical
- **MatchScopeTypes 逗号分隔 allowlist**：空 = 任意；如 "host,monitoring_pipeline"
- **Event.SnapshotJSON 审计快照**：事件发生时 incident 状态快照，便于回溯
- **Incident.DedupeKey uniqueIndex**：相同 dedupe key 复用同一 incident，避免重复开单
- **EventCount 累计**：每次 firing 递增；UI 显示告警次数

## 8. 注意事项

- **RuleKey 唯一**：跨未软删行唯一；builtin 用 canonical key，用户自创可自选
- **Kind 默认 metric_raw**：迁移脚本 backfill NULL/'' 行；旧 metric_threshold 行 sweep 到 metric_raw
- **ConditionsJSON TEXT NOT NULL**：MySQL 禁 TEXT DEFAULT；biz 总写值
- **NotifyWindowSeconds / NotifyMinFires**：必须同时为 0 或同时 >0；mixed biz 拒绝
- **DedupeKey size:191**：MySQL InnoDB utf8mb4 索引长度限制（191 = 768/4）
- **LabelsJSON / AnnotationsJSON**：用于路由 / 分组 / UI 显示；高基数 user_id 禁用
- **Silence.DeviceID**：May 2026 entity split 后从 EdgeID 改名；底层整数 1:1 复用
- **Channel.ConfigJSON**：plugin 特定配置（webhook URL / Slack token / 飞书 app secret）；biz 层按 channel_type 解析
- **Delivery.ProviderMessageID**：用于追踪外部平台 message id（如 Slack ts、飞片 message_id）
- **Event.ActorType**：system / user；AIInitialDiagnosis 是 system 写入
- **Removed `log` channel**：老部署若有 `log` channel 行需手动清理
