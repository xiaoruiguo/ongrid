# `mutating_proposal.go` 技术实现文档

> 源文件：`internal/manager/model/aiops/mutating_proposal.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/model/aiops`

## 1. 概述

本文件定义 `MutatingProposal` 实体：每次拦截到的 mutating tool_call 单独落表审计。设计核心是把"审查决策"与"工具执行"分离——`chat_tool_calls` 表只能记执行成功，rejected 的 proposal 无对应执行行；独立表保证审计完整、SPA 审批 UI 免 JSON parse 渲染、reviewer 可按 session 范围执行"5 分钟无并行 mutating"SOP。红线：每个 mutating proposal 必留一行（approve 或 reject 都记），reviewer_task_id 不是 FK（worker 表 PR-7 仍为内存态）。

## 2. 包信息

- **包名**：`aiops`（与 model.go 同包）
- **所属模块**：`internal/manager/model/`
- **依赖方向**：被 `manager/biz/aiops/tools/decorators/review_gate.go`（ReviewGate 装饰器）写入 / 读出；依赖 `gorm.io/gorm`、`github.com/google/uuid`

## 3. 关键类型与接口

```go
type MutatingProposal struct {
    ID string `gorm:"primaryKey;type:char(36);column:id"`

    // 链回 coordinator chat
    SessionID   string  `gorm:"index;type:char(36);not null;column:session_id"`
    MessageID   *string `gorm:"index;type:char(36);column:message_id"`
    ToolCallID  *string `gorm:"index;type:char(36);column:tool_call_id"`

    // 记录 WHAT was proposed
    ToolName  string `gorm:"size:64;not null;index:idx_chat_mutating_tool_created,priority:1;column:tool_name"`
    ArgsJSON  string `gorm:"type:text;not null;column:args_json"`
    ToolClass string `gorm:"size:16;not null;column:tool_class"` // "write" | "destructive"

    // Reviewer 信息
    ReviewerAgent  string `gorm:"size:64;not null;column:reviewer_agent"`
    ReviewerTaskID string `gorm:"size:64;not null;column:reviewer_task_id"`

    // 决策
    Decision       string  `gorm:"size:16;not null;default:pending;check:decision IN ('pending','approve','reject');index:idx_chat_mutating_decision_created,priority:1;column:decision"`
    DecisionReason *string `gorm:"type:text;column:decision_reason"`

    // 用户
    OperatorUserID uint64  `gorm:"index;not null;default:0;column:operator_user_id"`
    ApproverUserID *uint64 `gorm:"column:approver_user_id"` // 预留 SPA dual-sign

    // 时间
    CreatedAt   time.Time  `gorm:"index:idx_chat_mutating_tool_created,priority:2;index:idx_chat_mutating_decision_created,priority:2"`
    DecidedAt   *time.Time `gorm:"column:decided_at"`
    ExecutedAt  *time.Time `gorm:"column:executed_at"`
}

// Decision 常量
const (
    DecisionPending  = "pending"
    DecisionApprove  = "approve"
    DecisionReject  = "reject"
)
```

## 4. 关键函数与流程

### `MutatingProposal.TableName`
- **签名**：`func (MutatingProposal) TableName() string`
- **职责**：固定表名 `chat_mutating_proposals`（SQLite / MySQL 通用）

### `MutatingProposal.BeforeCreate`
- **签名**：`func (p *MutatingProposal) BeforeCreate(*gorm.DB) error`
- **职责**：caller 未预填 ID 时自动生成 UUIDv4
- **流程**：`p.ID == ""` → `uuid.NewString()`；返回 nil

## 5. 依赖关系

- **内部包**：无
- **外部库**：`github.com/google/uuid`、`gorm.io/gorm`
- **被调用方**：`manager/biz/aiops/tools/decorators/review_gate.go`（ReviewGate 装饰器）；SPA approval UI

## 6. 并发与资源管理

- 本文件仅定义 schema，无锁 / channel / 缓存
- `BeforeCreate` 是 GORM hook，同步执行

## 7. 设计模式与亮点

- **独立审计表**：rejected 的 proposal 不会污染 `chat_tool_calls` 执行表；避免合成执行行
- **SPA 审批 UI 友好**：`decision_reason` / `gates_passed` / `missing_gates` 直接列存储，无需 parse `result_json`
- **per-session SOP 实施**：reviewer 查此表判断"5 分钟内无并行 mutating"
- **ToolClass 落库**：`write` | `destructive`，避免审计查询时重新加载 tool registry
- **ReviewerAgent 显式存储**：当前固定 "reviewer"；为未来 per-tool reviewer（如 drop_table 专用 db-reviewer）留 trail
- **ApproverUserID 预留**：当前 reviewer 是 software approver；列为 NULL 直到 SPA dual-sign 落地
- **双时间戳**：CreatedAt = 拦截时刻，DecidedAt = reviewer 返回时刻；SLO "reviewer round-trip duration" 无需 JOIN 即可计算
- **复合索引设计**：按 (tool_name, created_at) 与 (decision, created_at) 双索引，分别支撑 tool 类别审计与 pending 队列扫描

## 8. 注意事项

- **MessageID / ToolCallID 可空**：legacy 路径 / 直接调用测试可能不带 session 上下文；fallback-empty
- **OperatorUserID 默认 0**：表示 "anonymous"，仅用于测试；生产路径必须填 user_id
- **ReviewerTaskID 非 FK**：当前 worker 表在内存中（PR-7）；未来持久化时再加 FK
- **Decision CHECK 约束**：仅 pending / approve / reject 三态合法
- **DecisionReason 可空**：pending 状态无说明；ready 后由 reviewer 写入 verbatim markdown
- **ExecutedAt 可空**：approve 但 executor 尚未跑或无 executor 时为 NULL
- **ToolClass 取值**：当前 `write` / `destructive`；新增 class 需同步 ReviewGate 装饰器逻辑
- **SessionID 必填**：所有 proposal 必源自 chat session；message/tool_call 可空以兼容老路径
