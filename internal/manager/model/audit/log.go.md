# `log.go` 技术实现文档

> 源文件：`internal/manager/model/audit/log.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/model/audit`

## 1. 概述

本文件是 HLD-010 审计 trail 的持久化实体 + 规范常量集合。一行 = 一个 (actor, action, resource, outcome) 观察记录。设计核心是"不可篡改的记录"——本包代码绝不修改或删除行，仅保留 retention job 可清理。红线：`PayloadJSON` 是唯一 free-form 字段，biz 层必须在序列化前 redact secrets（LLM key / SSH key / 密码 / token），仅记录 change 的 shape 不记录 values；MySQL 禁 TEXT DEFAULT（Error 1101），故 TEXT 列无 default 但 Go 零值已满足 NOT NULL。

## 2. 包信息

- **包名**：`audit`
- **所属模块**：`internal/manager/model/`
- **依赖方向**：被 `manager/biz/audit` 的写入 middleware 与 list service 调用；依赖 `time`

## 3. 关键类型与接口

```go
type Log struct {
    ID            uint64    `gorm:"primaryKey;autoIncrement"`
    OccurredAt    time.Time `gorm:"not null;index:idx_audit_occurred"`
    UserID        *uint64   `gorm:"index:idx_audit_user,priority:1"`
    UserEmail     string    `gorm:"size:255;not null"`
    Role          string    `gorm:"size:16;not null"`
    IP            string    `gorm:"size:45;not null"`
    UserAgent     string    `gorm:"size:512;not null"`
    Action        string    `gorm:"size:64;not null;index:idx_audit_action,priority:1"`
    ResourceType  string    `gorm:"size:32;not null;index:idx_audit_resource,priority:1"`
    ResourceID    string    `gorm:"size:128;not null;index:idx_audit_resource,priority:2"`
    ResourceName  string    `gorm:"size:256;not null"`
    Status        string    `gorm:"size:16;not null;index:idx_audit_status,priority:1"`
    ErrorCode     string    `gorm:"size:64;not null"`
    ErrorMessage  string    `gorm:"size:512;not null"`
    PayloadJSON   string    `gorm:"type:text"`
    RequestID     string    `gorm:"size:64;not null"`
    CreatedAt     time.Time `gorm:"autoCreateTime"`
}

// Status 枚举
const (
    StatusSuccess = "success"
    StatusFailure = "failure"
    StatusDenied  = "denied"
)

// Action 常量（节选）
const (
    ActionAuthLoginFailed = "auth_login_failed"
    ActionUserCreate      = "user_create"
    ActionUserUpdate      = "user_update"
    ActionUserDelete      = "user_delete"
    ActionUserExport      = "user_export"
    ActionDeviceUpdate    = "device_update"
    ActionDeviceDelete    = "device_delete"
    ActionRuleCreate      = "rule_create"
    ActionRuleUpdate      = "rule_update"
    ActionRuleDelete      = "rule_delete"
    ActionIncidentAck     = "incident_ack"
    ActionIncidentResolve = "incident_resolve"
    ActionIncidentSilence = "incident_silence"
    ActionSettingUpdate   = "setting_update"
    ActionSettingDelete   = "setting_delete"
    ActionChannelCreate   = "channel_create"
    ActionChannelUpdate   = "channel_update"
    ActionChannelDelete    = "channel_delete"
    ActionRepoCreate      = "repo_create"
    ActionRepoDelete      = "repo_delete"
    ActionRepoSync        = "repo_sync"
    ActionSkillInstall    = "skill_install"
    ActionSkillUninstall  = "skill_uninstall"
)

// ResourceType 桶（节选）
const (
    ResourceUser     = "user"
    ResourceDevice   = "device"
    ResourceIncident = "incident"
    ResourceSetting  = "setting"
    ResourceRule     = "rule"
    ResourceChannel  = "channel"
    ResourceRepo     = "repo"
    ResourceSkill    = "skill"
    ResourceLLM      = "llm"
    ResourceGitKey   = "git_ssh_key"
    ResourceGrafana  = "grafana"
    ResourceRAG      = "rag"
    ResourceAudit    = "audit"
    ResourceAuth     = "auth"
)
```

## 4. 关键函数与流程

### `Log.TableName`
- **签名**：`func (Log) TableName() string`
- **职责**：固定表名 `audit_logs`，避免包重命名后误创建平行空表

## 5. 依赖关系

- **内部包**：无
- **外部库**：`time`（仅标准库，不引入 gorm，因无 GORM hook）
- **被调用方**：`manager/biz/audit` 写入 middleware、list service；retention job

## 6. 并发与资源管理

- 本文件仅定义 schema，无锁 / channel / 缓存
- 无 `BeforeCreate` hook，主键 autoIncrement 由 DB 分配
- 无 soft-delete 列（审计数据不允许软删，仅 retention job 物理清理）

## 7. 设计模式与亮点

- **不可篡改设计**：本包代码不修改 / 删除；retention job 独占删除权
- **UserID `*uint64`**：未认证路径（failed login / anon endpoint）写 NULL，满足未来 FK 语义
- **TEXT 列无 default**：MySQL 8 拒 DEFAULT on TEXT（Error 1101）；Go 零值 "" 已满足 NOT NULL
- **PayloadJSON 唯一 free-form**：shape of change 不 values；如 `{"field":"openai_api_key","action":"set","hint":"sk-...XYZ"}`
- **RequestID 跨系统 join**：与 chi RequestID middleware 同 id；可 join slog / 未来 tracing
- **Action 命名扁平 `verb_resource`**：2026-05-20 操作员反馈后收敛 38-action 枚举；sub-flavour（enable/disable、role-change/password-reset、single/bulk）走 payload 不走 action 名
- **ResourceType 平铺**：UI 分组而非数据分组
- **IP size:45**：兼容 IPv6
- **UserAgent size:512**：兼容长 UA
- **复合索引设计**：(action, occurred_at) / (resource_type, resource_id) / (status, occurred_at) / (user_id, occurred_at) 多组合索引覆盖常见审计查询
- **Denied 状态独立**：与 failure 区分——denied 是权限拒绝；failure 是执行出错

## 8. 注意事项

- **PayloadJSON 必须 redact**：biz 写入前清除 LLM key / SSH key / 密码 / token
- **RequestID 必填**：无 upstream 时 audit middleware 自生成
- **Role 必填**：失败 login 也要写 role（如 "anonymous"）
- **UserEmail 必填**：未认证路径写空字符串或 "anonymous"
- **Action 枚举严格**：UI action dropdown 短小；新增 action 需同步前端过滤器
- **Removed Actions（2026-05-21）**：`auth_login` / `auth_logout` / `audit_view` 删除（操作员反馈：只读 / 会话 bookkeeping 行淹没 mutation signal）；保留 `auth_login_failed` 用于暴力破解可见性
- **ResourceType vs Action 解耦**：同一 resource 可有多个 action；同一 action 可跨 resource（如 `setting_update` 覆盖 LLM key / Grafana / SSH key）
- **ErrorCode / ErrorMessage 必填**：success 时空字符串；failure/denied 时填具体错误
- **Retention 不在本包**：retention job 独立实现，物理 DELETE；本包仅暴露 schema
