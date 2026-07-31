# `model.go` 技术实现文档

> 源文件：`internal/manager/model/flow/model.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/model/flow`

## 1. 概述

本文件是 flow 子域的 schema：用户自创作工作流编排（HLD-016）。三实体：`Flow`（定义行，GraphJSON 存 React Flow canvas DAG）、`FlowRun`（一次执行，UUID 主键 + 状态机 + trigger 快照）、`FlowRunNode`（run 内一个执行节点，resolved input/output/status/timing）。设计要点：config 行用 uint64 autoIncrement；artifact 行用 char(36) UUID；TEXT 列 NOT NULL 无 default（MySQL Error 1101）；无 org_id 列（单租户 MVP，owner = created_by）。红线：GraphJSON 引擎在每次 run 重新校验，防止手编辑行 crash executor；FlowVersion 快照防止旧 run 视图被后续编辑混淆。

## 2. 包信息

- **包名**：`flow`
- **所属模块**：`internal/manager/model/`
- **依赖方向**：被 `manager/biz/flow` 的 ParseGraph / 引擎调用；依赖 `gorm.io/gorm`、`time`

## 3. 关键类型与接口

```go
// FlowRun.Status 状态机
const (
    RunStatusPending   = "pending"
    RunStatusRunning   = "running"
    RunStatusSucceeded = "succeeded"
    RunStatusFailed    = "failed"
    RunStatusCanceled  = "canceled"
)

// FlowRunNode.Status
const (
    NodeStatusRunning   = "running"
    NodeStatusSucceeded = "succeeded"
    NodeStatusFailed    = "failed"
)

// TriggerType
const (
    TriggerManual = "manual"
)

type Flow struct {
    ID          uint64 `gorm:"primaryKey;autoIncrement"`
    Name        string `gorm:"size:255;not null;index"`
    Description string `gorm:"size:1024;not null"`
    GraphJSON   string `gorm:"type:text;not null"` // biz 总写 "{}"
    Enabled     bool   `gorm:"not null;default:true"`
    Version     int    `gorm:"not null;default:1"` // 每次 graph save 递增
    CreatedBy   *uint64 `gorm:"index"`

    CreatedAt time.Time
    UpdatedAt time.Time
    DeletedAt gorm.DeletedAt `gorm:"index"`
}

type FlowRun struct {
    ID          string `gorm:"primaryKey;type:char(36)"`
    FlowID      uint64 `gorm:"not null;index"`
    FlowVersion int    `gorm:"not null;default:1"`
    Status      string `gorm:"size:16;not null;index"`
    TriggerType string `gorm:"size:32;not null"`
    TriggerJSON string `gorm:"type:text;not null"` // manual: 用户输入对象
    Error       string  `gorm:"size:2048;not null"`
    CreatedBy   *uint64 `gorm:"index"`

    StartedAt  *time.Time
    FinishedAt *time.Time
    CreatedAt  time.Time
    UpdatedAt  time.Time
}

type FlowRunNode struct {
    ID    uint64 `gorm:"primaryKey;autoIncrement"`
    RunID string `gorm:"type:char(36);not null;index"`
    // 执行时从 graph 快照，避免后续 graph 编辑混淆旧 run 视图
    NodeID   string `gorm:"size:64;not null"`
    NodeType string `gorm:"size:64;not null"`
    NodeName string `gorm:"size:255;not null"`
    Status   string `gorm:"size:16;not null"`
    InputJSON  string `gorm:"type:text;not null"` // 表达式 resolve 后
    OutputJSON string `gorm:"type:text;not null"`
    FiredPort string `gorm:"size:32;not null"` // next/true/false/error/...
    Error     string `gorm:"size:2048;not null"`

    StartedAt  *time.Time
    FinishedAt *time.Time
    CreatedAt  time.Time
}
```

## 4. 关键函数与流程

本文件仅定义 schema，无 GORM hook 或方法（Flow/FlowRun/FlowRunNode 均未定义 TableName 方法——会使用 GORM 默认 pluralization）。

## 5. 依赖关系

- **内部包**：无
- **外部库**：`gorm.io/gorm`、`time`
- **被调用方**：`manager/biz/flow` 的 ParseGraph / 引擎 / run viewer

## 6. 并发与资源管理

- 本文件仅定义 schema，无锁 / channel / 缓存
- `Flow.DeletedAt` 软删除（gorm.DeletedAt）；FlowRun / FlowRunNode 未带软删（artifact 行保留永久）

## 7. 设计模式与亮点

- **三实体分层**：Flow = 定义；FlowRun = 一次执行；FlowRunNode = 节点级 artifact（run viewer 下钻）
- **MySQL 约定继承 alert/device 模型**：config 用 uint64 autoIncrement；artifact 用 char(36) UUID；TEXT NOT NULL 无 default
- **FlowVersion 快照**：FlowRun 持有执行时的 FlowVersion；后续编辑 flow 不影响旧 run 视图
- **GraphJSON 引擎每次 run 重校验**：手编辑行不能 crash executor
- **TriggerJSON 暴露给表达式**：`{{trigger.*}}` 访问；manual 时是用户输入对象
- **FlowRunNode.NodeID/NodeType/NodeName 快照**：graph 后续编辑不混淆旧 run 视图
- **InputJSON = 表达式 resolve 后**：executor 实际收到的；OutputJSON = 数据输出
- **FiredPort 控制端口**：next/true/false/error/...；run viewer 据此给边着色
- **Error size:2048**：run-level / node-level 失败原因；健康时为 ""
- **无 org_id 列**：private-MVP 单租户；owner = created_by
- **stale running sweep**：manager 重启时把 running 行扫到 failed（engine in-process，不跨 crash 存活）

## 8. 注意事项

- **FlowRun.ID 是 UUID**：caller 需预生成或 biz 层在 Create 前填
- **FlowRunNode 未带软删**：artifact 行永久保留；如需清理另起 retention
- **GraphJSON TEXT NOT NULL**：biz 总写至少 "{}"
- **TriggerType 当前仅 manual**：未来扩展 webhook / schedule / incident
- **FlowVersion 必须每次 graph save 递增**：否则旧 run 视图会与新 flow 混淆
- **FlowRunNode 仅记 executed 节点**：skipped 分支不写行（未来 backfill 时填）
- **Error 字段空表示健康**：未失败时为 ""
- **无 TableName 方法**：依赖 GORM 默认 pluralization（flows / flow_runs / flow_run_nodes）；如需固定应补 TableName
