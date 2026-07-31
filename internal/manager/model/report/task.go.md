# `task.go` 技术实现文档

> 源文件：`internal/manager/model/report/task.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/model/report`

## 1. 概述

本文件是统一"任务"脊柱（HLD-022 Phase 2）的 schema。`Task` 实体是用户在"任务"页管理的对象。物理表仅存非 recurring 任务（今日：`kind="oneoff"`——从任务侧立即触发的 one-shot 生成）；recurring 任务仍走 `report_schedules`，在 API 层 union 进任务列表（id=`report-schedule:<id>`），避免对 schedule path 做破坏性迁移。Artifacts（reports）通过 `report.task_id` 反向链：recurring → `report-schedule:<schedule_id>`；oneoff → `oneoff:<task_id>`，让 task 详情列出所有产出的 artifact。

## 2. 包信息

- **包名**：`report`（与 model.go 同包）
- **所属模块**：`internal/manager/model/`
- **依赖方向**：被 `manager/biz/report` 的 task service / task list 调用；依赖 `time`

## 3. 关键类型与接口

```go
// TaskKind 常量
const (
    TaskKindOneoff    = "oneoff"            // 立即从任务侧触发的 one-shot
    TaskKindRecurring = "recurring_report"  // report_schedules 上的视图（不存此表）
)

type Task struct {
    ID string `gorm:"primaryKey;type:char(36);column:id"`

    // Kind 任务类型；今日存储行仅 "oneoff"；常量集为 chat_todo 等留位不需 schema 变更
    Kind string `gorm:"column:kind;size:32;not null;index"`

    Title string `gorm:"column:title;size:255;not null"`

    // ReportKind / ScopeJSON 快照 oneoff task 生成的 report 配置
    ReportKind string `gorm:"column:report_kind;size:16;not null;default:''"`
    ScopeJSON  string `gorm:"column:scope_json;type:text;not null"`

    // Status: active | done | failed — 镜像最新 run 的结果
    Status    string `gorm:"column:status;size:16;not null;default:'active'"`
    CreatedBy uint64 `gorm:"column:created_by;not null"`

    CreatedAt time.Time  `gorm:"column:created_at;autoCreateTime"`
    UpdatedAt time.Time  `gorm:"column:updated_at;autoUpdateTime"`
    DeletedAt *time.Time `gorm:"column:deleted_at;index"`
}

// OneoffTaskRef 为 oneoff task id 构建 artifact 反向链字符串
func OneoffTaskRef(taskID string) string { return "oneoff:" + taskID }
```

## 4. 关键函数与流程

### `Task.TableName`
- **签名**：`func (Task) TableName() string`
- **职责**：固定表名 `tasks`

### `OneoffTaskRef`
- **签名**：`func OneoffTaskRef(taskID string) string`
- **职责**：为 oneoff task id 构建 artifact 反向链字符串
- **流程**：返回 `"oneoff:" + taskID`
- **用途**：写入 `report.task_id` 让 report 反向链回 task

## 5. 依赖关系

- **内部包**：`report` 包的 `Report.TaskID` 反向链
- **外部库**：`time`
- **被调用方**：`manager/biz/report` 的 task service / task list；report 生成路径（写 TaskID）

## 6. 并发与资源管理

- 本文件仅定义 schema，无锁 / channel / 缓存
- `DeletedAt` 软删（普通 `*time.Time`，非 `soft_delete.DeletedAt`，不参与 unique 约束）

## 7. 设计模式与亮点

- **统一"任务"脊柱**：物理表仅存 oneoff；recurring 走 report_schedules 在 API 层 union，避免破坏性迁移
- **TaskKind 常量集预留**：今日 "oneoff" + "recurring_report"；为 chat_todo 等留位不需 schema 变更
- **ReportKind / ScopeJSON 快照**：让 oneoff task 可复现（即便 schedule scope 已改或 schedule 已删）
- **Status 三态镜像最新 run**：active / done / failed；任务列表 badge 用
- **OneoffTaskRef helper**：统一反向链字符串格式 `"oneoff:<task_id>"`
- **与 recurring 反向链对称**：recurring 用 `report-schedule:<schedule_id>`；oneoff 用 `oneoff:<task_id>`
- **ID 是 UUID**：caller 预生成或 biz 层 Create 前填
- **DeletedAt 软删**：不参与 unique；仅审计时间戳

## 8. 注意事项

- **ID 必填**：UUID；caller 需预生成
- **Kind 必填**：今日仅 "oneoff"；"recurring_report" 是视图不存此表
- **Title 必填**：任务列表显示用
- **ReportKind 默认空字符串**：oneoff task 不生成 report 时为空
- **ScopeJSON 必填**：biz 总写至少 "{}"
- **Status 默认 active**：新建任务 active；run 完成后更新为 done/failed
- **CreatedBy 必填**：任务 owner；0 = anonymous（仅测试）
- **DeletedAt 软删**：仅审计时间戳；不参与 unique 约束（与 soft_delete.DeletedAt 不同）
- **OneoffTaskRef 字符串格式**：`"oneoff:" + taskID`；report.task_id 字段写入此值
- **recurring 反向链格式**：`"report-schedule:" + scheduleID`；不在本文件，由 report schedule path 生成
- **未来扩展 chat_todo**：TaskKind 常量集已留位；新增 kind 不需 schema 变更
