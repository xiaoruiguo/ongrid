# `model.go` 技术实现文档

> 源文件：`internal/manager/model/report/model.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/model/report`

## 1. 概述

本文件是 report 子域的 schema：用户调度的运维报告（daily/weekly/monthly/custom-cron），由 reporter agent persona 生成，可应用内查看并通过通知渠道投递。两实体（HLD-014）：`ReportSchedule`（用户配置行：周期 / 范围 / 渠道 / persona，cron evaluator 按 NextFireAt 触发）+ `Report`（一个生成的 artifact，镜像 alert.InvestigationReport 约定——char(36) UUID 主键 + 状态机 + JSON content + worker backfill + 应用内下钻）。设计要点：MySQL 约定继承 alert/device——config 用 uint64 autoIncrement；artifact 用 char(36) UUID；TEXT/longtext NOT NULL 无 default（MySQL Error 1101）；无 org_id（单租户 MVP，owner = created_by）。红线：`(schedule_id, period_start)` UNIQUE 防止同一 schedule 同窗口重复生成；`ShareToken` 30 天 TTL 外部只读分享。

## 2. 包信息

- **包名**：`report`
- **所属模块**：`internal/manager/model/`
- **依赖方向**：被 `manager/biz/report` 的 cron evaluator / reporter worker 调用；依赖 `gorm.io/gorm`、`gorm.io/plugin/soft_delete`、`time`

## 3. 关键类型与接口

```go
// Kind 调度 cadence preset
const (
    KindDaily   = "daily"
    KindWeekly  = "weekly"
    KindMonthly = "monthly"
    KindCustom  = "custom"
)

// Report.Status 状态机
const (
    StatusPending    = "pending"
    StatusGenerating = "generating"
    StatusReady      = "ready"
    StatusFailed     = "failed"
)

const DefaultReporterPersona = "reporter"

type ReportSchedule struct {
    ID        uint64 `gorm:"primaryKey;autoIncrement;column:id"`
    CreatedBy uint64 `gorm:"column:created_by;not null;index:idx_rsched_owner"`

    Name        string `gorm:"column:name;size:128;not null"`
    Description string `gorm:"column:description;size:255;not null;default:''"`

    Kind     string `gorm:"column:kind;size:16;not null"`
    CronSpec string `gorm:"column:cron_spec;size:64;not null"`
    Timezone string `gorm:"column:timezone;size:64;not null;default:'UTC'"`

    ScopeJSON string `gorm:"column:scope_json;type:text;not null"`

    ChannelIDsJSON string `gorm:"column:channel_ids_json;type:text;not null"`
    InAppVisible   bool   `gorm:"column:in_app_visible;not null;default:true"`

    AgentPersona   string  `gorm:"column:agent_persona;size:64;not null;default:'reporter'"`
    PromptOverride *string `gorm:"column:prompt_override;type:text"` // NULL = persona default

    Enabled      bool       `gorm:"column:enabled;not null;default:true;index:idx_rsched_enabled_next,priority:1"`
    NextFireAt   *time.Time `gorm:"column:next_fire_at;index:idx_rsched_enabled_next,priority:2"`
    LastFireAt   *time.Time `gorm:"column:last_fire_at"`
    LastReportID *string    `gorm:"column:last_report_id;size:36"`

    CreatedAt time.Time      `gorm:"column:created_at;autoCreateTime"`
    UpdatedAt time.Time      `gorm:"column:updated_at;autoUpdateTime"`
    DeletedAt gorm.DeletedAt `gorm:"column:deleted_at;index"`
}

type Report struct {
    ID string `gorm:"primaryKey;type:char(36);column:id"`

    // ScheduleID: cron 触发设置；NULL = 手动 "generate now"
    ScheduleID *uint64 `gorm:"column:schedule_id;uniqueIndex:uniq_report_sched_period,priority:1"`

    // TaskID 拥有 task 反向链（HLD-022），如 "report-schedule:42"
    TaskID string `gorm:"column:task_id;size:128;index:idx_report_task;not null;default:''"`

    // RunID trigger/run 反向链（HLD-022 三级模型：task → run → artifact）
    RunID     string `gorm:"column:run_id;size:64;index:idx_report_run;not null;default:''"`
    CreatedBy uint64 `gorm:"column:created_by;not null"`

    Title       string    `gorm:"column:title;size:255;not null"`
    Kind        string    `gorm:"column:kind;size:16;not null"`
    PeriodStart time.Time `gorm:"column:period_start;not null;uniqueIndex:uniq_report_sched_period,priority:2;index:idx_report_period"`
    PeriodEnd   time.Time `gorm:"column:period_end;not null"`
    Timezone    string    `gorm:"column:timezone;size:64;not null"`

    Locale string `gorm:"column:locale;size:8;not null;default:''"`
    ScopeJSON string `gorm:"column:scope_json;type:text;not null"`

    Status   string `gorm:"column:status;size:16;not null;default:'pending';index:idx_report_status_created,priority:1"`
    ErrorMsg string `gorm:"column:error_msg;type:text;not null"`

    ContentJSON string `gorm:"column:content_json;type:longtext;not null"`
    ContentMD   string `gorm:"column:content_md;type:longtext;not null"`
    SummaryText string `gorm:"column:summary_text;size:512;not null;default:''"`

    GeneratedAt      *time.Time `gorm:"column:generated_at"`
    GeneratedByModel string     `gorm:"column:generated_by_model;size:64;not null;default:''"`
    PromptTokens     uint64     `gorm:"column:prompt_tokens;not null;default:0"`
    CompletionTokens uint64     `gorm:"column:completion_tokens;not null;default:0"`
    AuditSessionID   *string    `gorm:"column:audit_session_id;size:36;index"`
    WorkerID         *string    `gorm:"column:worker_id;size:64"`

    ShareToken     *string    `gorm:"column:share_token;size:32;uniqueIndex:idx_report_share,priority:1"`
    ShareExpiresAt *time.Time `gorm:"column:share_expires_at"`

    DeliveryJSON string `gorm:"column:delivery_json;type:text;not null"`

    CreatedAt    time.Time             `gorm:"column:created_at;autoCreateTime"`
    UpdatedAt    time.Time             `gorm:"column:updated_at;autoUpdateTime"`
    DeletedAt    *time.Time            `gorm:"column:deleted_at;index"`
    DeleteMarker soft_delete.DeletedAt `gorm:"column:delete_marker;not null;default:0;softDelete:milli,DeletedAtField:DeletedAt;uniqueIndex:uniq_report_sched_period,priority:3;uniqueIndex:idx_report_share,priority:2"`
}
```

## 4. 关键函数与流程

### `ReportSchedule.TableName`
- **签名**：`func (ReportSchedule) TableName() string`
- **职责**：固定表名 `report_schedules`

### `Report.TableName`
- **签名**：`func (Report) TableName() string`
- **职责**：固定表名 `reports`

## 5. 依赖关系

- **内部包**：`aiops` 包（通过 AuditSessionID 反查 reporter worker transcript）；`alert` 包（通过 ChannelIDsJSON 引用 notification_channels）
- **外部库**：`gorm.io/gorm`、`gorm.io/plugin/soft_delete`、`time`
- **被调用方**：`manager/biz/report` 的 cron evaluator / reporter worker；SPA report viewer

## 6. 并发与资源管理

- 本文件仅定义 schema，无锁 / channel / 缓存
- `ReportSchedule.DeletedAt` 软删除（gorm.DeletedAt）
- `Report.DeleteMarker` 加入 unique index 让软删后同 (schedule_id, period_start) 可重建
- `Report.ShareToken` 与 DeleteMarker 联合唯一

## 7. 设计模式与亮点

- **Kind 是 UI preset + period 推导 hint**：daily/weekly/monthly 由前端生成对应 cron；custom 用户直接给 cron；evaluator 总用 CronSpec 触发
- **ScopeJSON v1 字段**：`{fleet_tags:[], edge_ids:[], severity_min:""}`；空对象 = 全覆盖
- **InAppVisible 默认 true**：可关闭仅走 IM 投递
- **AgentPersona 默认 reporter**：可 per-schedule 覆盖；PromptOverride NULL = 用 persona 默认 prompt
- **(enabled, next_fire_at) 复合索引**：cron evaluator `WHERE enabled AND next_fire_at <= now` 高效
- **(schedule_id, period_start) UNIQUE**：防同一 schedule 同窗口重复生成；DeleteMarker 让软删后可重建
- **ScheduleID NULL = 手动 generate**：手动报告不参与 cron 去重
- **TaskID 三级模型**：HLD-022 task → run → artifact；scheduled → `report-schedule:<id>`；oneoff → `oneoff:<task_id>`
- **RunID 三级模型**：task fire 多 run（每 cron tick / run-now = 一 run）；一 run 可产多 artifact（report + page）共享 RunID
- **Locale 输出语言**：手动 = 请求者 Accept-Language；调度 = "" → fallback ONGRID_DEFAULT_LOCALE
- **ScopeJSON snapshot**：从 schedule 复制或手动直接给；artifact 持久化 scope 使 report 可复现 / 可解读（即便 schedule scope 已改或 schedule 已删）
- **ContentJSON + ContentMD 双源**：JSON 是结构化卡片渲染 source of truth；MD 是 export / IM / search fallback
- **SummaryText IM 预览**：list 副标题 / share blurb
- **GeneratedByModel provenance**：审计哪个模型生成；UI 显示
- **PromptTokens / CompletionTokens**：预算追踪
- **AuditSessionID 反查 transcript**：reporter worker 全 tool-loop transcript
- **WorkerID 重启续接**：manager 重启时按 WorkerID 取消 / 重新挂起
- **ShareToken 30 天 TTL**：外部只读分享；ShareExpiresAt 过期
- **DeliveryJSON 投递结果**：`[{channel_id, channel_type, status, sent_at, error, fallback_used}]`

## 8. 注意事项

- **ScheduleID NULL = 手动**：手动报告不参与 (schedule_id, period_start) 去重
- **TaskID 必填**：scheduled = `report-schedule:<id>`；oneoff = `oneoff:<task_id>`；空 = unattached（legacy）
- **RunID 必填**：scheduled = cron tick id；oneoff = run-now id；空 = legacy
- **PeriodStart / PeriodEnd 必填**：报告覆盖的时间窗口
- **Locale 空 = fallback**：调度生成时 biz 层用 ONGRID_DEFAULT_LOCALE
- **ScopeJSON 必填**：biz 总写至少 "{}"
- **ContentJSON / ContentMD 必填**：biz 总写值；未生成时为空字符串
- **Status 状态机**：pending → generating → ready/failed
- **ErrorMsg 必填**：健康时为空字符串
- **GeneratedByModel 默认空字符串**：未生成时为 ""
- **ShareToken 可空**：未分享时 NULL；32 字符
- **ShareExpiresAt 可空**：未分享或永久分享时 NULL
- **DeliveryJSON 必填**：biz 总写至少 "[]"
- **DeleteMarker 在 unique**：软删后同 (schedule_id, period_start) 可重建
- **ReportSchedule 软删用 gorm.DeletedAt**：无 DeleteMarker（仅 Report 有）
- **NextFireAt 可空**：disabled 时 NULL；从未触发时 NULL
- **LastReportID 可空**：从未生成时 NULL
