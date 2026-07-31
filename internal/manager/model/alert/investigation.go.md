# `investigation.go` 技术实现文档

> 源文件：`internal/manager/model/alert/investigation.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/model/alert`

## 1. 概述

本文件定义 `InvestigationReport` 实体：告警事件触发的自动根因分析报告。由 investigator usecase 在告警触发时创建，由 chatruntime worker（incident-investigator agent persona）填充，最终由第二次 LLM pass 从 worker transcript 提取结构化字段。报告是操作员面向产物：一行 = 一个告警 = 一个结论。完整 transcript 存于 `chat_sessions`（kind='investigation'），通过 `AuditSessionID` 反查。红线：MySQL 禁止 TEXT 列 DEFAULT（Error 1101），所以所有 text/longtext 列保持 NOT NULL 无 default，biz 层总提供值（空字符串为规范"暂无"）。

## 2. 包信息

- **包名**：`alert`
- **所属模块**：`internal/manager/model/`
- **依赖方向**：被 `manager/biz/alert` 下 investigator usecase 与 report worker 读写；依赖 `gorm.io/gorm`、`github.com/google/uuid`、`time`

## 3. 关键类型与接口

```go
type InvestigationReport struct {
    ID string `gorm:"primaryKey;type:char(36);column:id"`

    // 一个 incident 一个 report（首版 UNIQUE；未来 re-run 版本化可能放宽）
    IncidentID uint64 `gorm:"column:incident_id;not null;uniqueIndex:uniq_invreports_incident"`

    Status string `gorm:"column:status;size:16;not null;default:'pending';index:idx_invreports_status_created,priority:1"`
    // MySQL 禁 TEXT DEFAULT → NOT NULL，biz 必须给值
    StatusReason string `gorm:"column:status_reason;type:text;not null"`

    RootCause         string `gorm:"column:root_cause;size:1024;not null;default:''"`
    AffectedWindow    string `gorm:"column:affected_window;size:64;not null;default:''"`
    PinpointedTargetJSON string `gorm:"column:pinpointed_target_json;type:text;not null"`
    RelatedAlertsJSON    string `gorm:"column:related_alerts_json;type:text;not null"`
    EvidenceJSON         string `gorm:"column:evidence_json;type:text;not null"`
    SuggestedActionsJSON string `gorm:"column:suggested_actions_json;type:text;not null"`
    FindingsMD            string `gorm:"column:findings_md;type:longtext;not null"`

    Confidence            *float64 `gorm:"column:confidence"`
    ConfidenceFactorsJSON string   `gorm:"column:confidence_factors_json;type:text;not null"`

    AuditSessionID *string `gorm:"column:audit_session_id;size:36;index"`
    WorkerID       *string `gorm:"column:worker_id;size:64"`
    ToolCallCount  int     `gorm:"column:tool_call_count;not null;default:0"`

    CreatedAt time.Time      `gorm:"column:created_at;autoCreateTime"`
    ReadyAt   *time.Time     `gorm:"column:ready_at"`
    UpdatedAt time.Time      `gorm:"column:updated_at;autoUpdateTime"`
    DeletedAt gorm.DeletedAt `gorm:"column:deleted_at;index"`
}

// Status 常量
const (
    InvestigationStatusPending = "pending"
    InvestigationStatusRunning = "running"
    InvestigationStatusReady   = "ready"
    InvestigationStatusFailed  = "failed"
    InvestigationStatusSkipped = "skipped"
)
```

## 4. 关键函数与流程

### `InvestigationReport.TableName`
- **签名**：`func (InvestigationReport) TableName() string`
- **职责**：固定表名 `investigation_reports`

### `InvestigationReport.BeforeCreate`
- **签名**：`func (r *InvestigationReport) BeforeCreate(*gorm.DB) error`
- **职责**：caller 未预填 ID 时生成 UUIDv4
- **流程**：`r.ID == ""` → `uuid.NewString()`；返回 nil

## 5. 依赖关系

- **内部包**：`aiops` 包的 chat_sessions（通过 AuditSessionID 反查 transcript）
- **外部库**：`github.com/google/uuid`、`gorm.io/gorm`、`time`
- **被调用方**：`manager/biz/alert/investigator` usecase；report worker；IncidentDetail SPA 页面

## 6. 并发与资源管理

- 本文件仅定义 schema，无锁 / channel / 缓存
- `BeforeCreate` 是 GORM hook，同步执行
- `DeletedAt` 为 GORM 软删除标记，默认查询自动过滤已删除行

## 7. 设计模式与亮点

- **一行 = 一告警 = 一结论**：操作员面向产物，简洁；UNIQUE 约束防重复生成
- **Status 状态机**：pending → running → ready/failed/skipped，覆盖 gate 拒绝（severity 过低 / 去重 / 预算超限）
- **MySQL TEXT DEFAULT 兼容**：所有 text/longtext 列 NOT NULL 无 default，biz 层总写空字符串
- **AffectedWindow ISO-8601 range 字符串**：跨存储可移植；biz 层按需 parse
- **PinpointedTargetJSON 开放 schema**：`{device_id, pid, cmd, service}` 由 LLM 按可用信号填
- **EvidenceJSON 引用链**：每条 evidence 指向 `chat_tool_calls` 行可下钻原始工具调用
- **SuggestedActionsJSON 显示-only**：v1 无 one-click execute；操作员手动 deep-link 或复制
- **Confidence + ConfidenceFactorsJSON 透明化**：UI 可显示"为何 0.7"——是否含拓扑、日志、trace 关联
- **AuditSessionID 反查 transcript**：transcript 不冗余进 report；点击展开再走 chat_sessions
- **WorkerID 重启续接**：manager 重启时按 WorkerID 取消 / 重新挂起 worker
- **ToolCallCount UI header**：直接显示"7 tool calls"，无需 count(JOIN)

## 8. 注意事项

- **IncidentID UNIQUE**：首版严格一对一；未来 re-run 版本化需放宽
- **StatusReason 必填**：pending/running/ready 时空字符串；skipped/failed 时为人工可读原因
- **MySQL 禁 TEXT DEFAULT**：v0.7.43 investigation_report 迁移曾踩此坑；新加 text 列务必遵守
- **PinpointedTargetJSON 无 schema 校验**：LLM 自由填；biz 层需做 defensive parse
- **Confidence 可空**：未生成时 NULL；0-1 浮点
- **AuditSessionID pending 时 NULL**：worker 启动后由 biz 填
- **WorkerID 可空**：未派生 worker 时 NULL
- **Soft delete**：DeletedAt 软删；保留审计但默认查询过滤
- **ReadyAt vs UpdatedAt**：ReadyAt 是 status=ready 时刻；UpdatedAt 是任意更新
