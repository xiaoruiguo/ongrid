# `model.go` 技术实现文档

> 源文件：`internal/manager/model/approval/model.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/model/approval`

## 1. 概述

本文件定义 `Approval` 实体：人类 propose-confirm inbox（HLD-017）的通用审批原语。一行 = 一个待决危险动作，独立于产生方（agent shell 命令、restart_service、flow 审批节点都可复用）。设计要点是严格 additive——新表 + 新包，不触碰 `chat_mutating_proposals` 也不影响任何 live path，仅在 producer 显式 Propose 时流入。红线：`PayloadJSON` 必须由 producer 在写入前 redact secrets（本包不做）。

## 2. 包信息

- **包名**：`approval`
- **所属模块**：`internal/manager/model/`
- **依赖方向**：被 `manager/biz/approval` 调用；依赖 `gorm.io/gorm`、`github.com/google/uuid`、`time`

## 3. 关键类型与接口

```go
type Approval struct {
    ID string `gorm:"primaryKey;type:char(36);column:id" json:"id"`

    // Kind 路由 approve 后的执行（biz 注册 per-kind executor）
    Kind string `gorm:"size:64;not null;index" json:"kind"`

    // 人类标签 + 摘要 + 不透明 payload
    Title       string `gorm:"size:255;not null" json:"title"`
    Summary     string `gorm:"type:text" json:"summary"`
    PayloadJSON string `gorm:"type:text;not null" json:"payload"`

    // 来源 + session 反向链
    Source    string `gorm:"size:32;not null;default:agent" json:"source"`
    SessionID string `gorm:"size:64;index" json:"session_id,omitempty"`

    // 状态机
    Status string `gorm:"size:16;not null;default:pending;index" json:"status"`

    // 用户
    ProposedBy uint64  `gorm:"not null;default:0" json:"proposed_by"`
    ApprovedBy *uint64 `gorm:"" json:"approved_by,omitempty"`

    // 决策原因 + 执行结果
    Reason     *string `gorm:"type:text" json:"reason,omitempty"`
    ResultJSON *string `gorm:"type:text" json:"result,omitempty"`

    CreatedAt  time.Time  `json:"created_at"`
    DecidedAt  *time.Time `json:"decided_at,omitempty"`
    ExecutedAt *time.Time `json:"executed_at,omitempty"`
}

// Status 常量
const (
    StatusPending  = "pending"
    StatusApproved = "approved"
    StatusRejected = "rejected"
    StatusExecuted = "executed"
    StatusFailed   = "failed"
)

// Source 常量
const (
    SourceAgent = "agent"
    SourceFlow  = "flow"
)
```

## 4. 关键函数与流程

### `Approval.TableName`
- **签名**：`func (Approval) TableName() string`
- **职责**：固定表名 `approvals`

### `Approval.BeforeCreate`
- **签名**：`func (a *Approval) BeforeCreate(*gorm.DB) error`
- **职责**：未预填 ID 时生成 UUIDv4
- **流程**：`a.ID == ""` → `uuid.NewString()`；返回 nil

## 5. 依赖关系

- **内部包**：无
- **外部库**：`github.com/google/uuid`、`gorm.io/gorm`、`time`
- **被调用方**：`manager/biz/approval` inbox service；producer（agent cloud-shell / flow approval node）

## 6. 并发与资源管理

- 本文件仅定义 schema，无锁 / channel / 缓存
- `BeforeCreate` 是 GORM hook 同步执行

## 7. 设计模式与亮点

- **通用审批原语**：不绑定 producer；任何危险动作都可走此表
- **严格 additive**：新表新包，不污染 `chat_mutating_proposals` 也不影响 live path
- **Kind 路由执行**：biz 层 per-kind 注册 executor（shell_command / restart_service 等）
- **PayloadJSON 不透明**：producer-defined shape；executor 知道如何 decode；存储层无需理解
- **Source + SessionID 反向链**：UI 可显示"来自 agent 的 shell_command"；SessionID 可选
- **状态机 5 态**：pending → approved → executed / failed；或 pending → rejected
- **Reason / ResultJSON 分离**：Reason 是人类决策说明；ResultJSON 是 executor 执行结果
- **DecidedAt + ExecutedAt 双时间**：人类决策时刻 vs 执行完成时刻；SLO 分析可分离
- **ProposedBy 必填**：chat owner / flow author；0 = anonymous（仅测试）
- **ApprovedBy 可空**：未决策时 NULL；rejected 也填决策者

## 8. 注意事项

- **PayloadJSON secret 责任**：producer 必须在写入前 redact；本包不做
- **Kind 命名约定**：建议 `snake_case`，如 `shell_command`、`restart_service`
- **Status 状态机严格**：approved 后必须再走 executed / failed；不能直接 rejected
- **ProposedBy 0 = anonymous**：生产路径必填 user_id
- **Summary 可空**：UI inbox 显示用，无详情展开
- **ResultJSON 可空**：rejected / 未执行时 NULL；executor 跑完填
- **SessionID 跨 source 含义不同**：agent source = chat session；flow source = flow run
- **不替代 chat_mutating_proposals**：reviewer agent 软件审批仍走旧表；人类 dual-sign 走此表
