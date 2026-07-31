# `model.go` 技术实现文档

> 源文件：`internal/manager/model/aiops/model.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/model/aiops`

## 1. 概述

本文件是 AIOps 子域的持久化实体定义：承载多轮会话（Session）、消息（Message）、工具调用（ToolCall）三类核心行的 GORM schema。设计要点是使用 UUID 作为主键避免路由可枚举、用 `user_id` 而非 `org_id` 做作用域、为子 agent / 调查任务保留可空审计列。关键红线：role=tool 消息必须能配对到 assistant 的 tool_calls，否则 DeepSeek v4+ 等严格 provider 会 400 拒绝；`ToolCalls []ToolCall` 用 `gorm:"-"` 标记为瞬态字段不落 schema。

## 2. 包信息

- **包名**：`aiops`
- **所属模块**：`internal/manager/model/`
- **依赖方向**：被 `manager/biz/aiops` 的 SessionRepo / MessageRepo 等 repo 调用；依赖 `gorm.io/gorm`、`github.com/google/uuid`

## 3. 关键类型与接口

```go
// ChatMessage.Role / ChatToolCall.Status 的常量集合
const (
    RoleUser      = "user"
    RoleAssistant = "assistant"
    RoleTool      = "tool"
    RoleSystem    = "system"

    StatusPending = "pending"
    StatusSuccess = "success"
    StatusError   = "error"
    StatusTimeout = "timeout"
)

type Session struct {
    ID                string  `gorm:"primaryKey;type:char(36);column:id"`
    UserID            uint64  `gorm:"index;not null;column:user_id"`
    Title             string  `gorm:"size:256;not null"`
    ScopeJSON         *string `gorm:"type:text;column:scope_json"`
    AgentID           *string `gorm:"size:128;index;column:agent_id"`
    ParentSessionID   *string `gorm:"size:36;index;column:parent_session_id"`
    Background        bool    `gorm:"not null;default:false;column:background"`
    RelatedIncidentID *uint64 `gorm:"index;column:related_incident_id"`
    Kind              string  `gorm:"size:16;not null;default:'user';column:kind;index:idx_session_kind_created,priority:1"`
    CreatedAt         time.Time
    UpdatedAt         time.Time
    ClosedAt          *time.Time `gorm:"column:closed_at"`
}

const (
    SessionKindUser          = "user"
    SessionKindInvestigation = "investigation"
)

type Message struct {
    ID               string  `gorm:"primaryKey;type:char(36);column:id"`
    SessionID        string  `gorm:"index:idx_session_msg,priority:1;type:char(36);not null;column:session_id"`
    Role             string  `gorm:"size:16;not null;check:role IN ('user','assistant','tool','system')"`
    Content          *string `gorm:"type:text"`
    ToolCallID       *string `gorm:"size:64;column:tool_call_id"`
    ToolName         *string `gorm:"size:64;column:tool_name"`
    Model            *string `gorm:"size:64"`
    PromptTokens     *int    `gorm:"column:prompt_tokens"`
    CompletionTokens *int    `gorm:"column:completion_tokens"`
    CreatedAt        time.Time
    ToolCalls        []ToolCall `gorm:"-"` // 瞬态：SessionRepo.ListMessages 填充
}

type ToolCall struct {
    ID            string  `gorm:"primaryKey;type:char(36);column:id"`
    MessageID     string  `gorm:"index;type:char(36);not null;column:message_id"`
    ToolName      string  `gorm:"size:64;not null;column:tool_name"`
    ArgumentsJSON string  `gorm:"type:text;not null;column:arguments_json"`
    ResultJSON    *string `gorm:"type:text;column:result_json"`
    Status        string  `gorm:"size:16;not null;default:pending;check:status IN ('pending','success','error','timeout')"`
    Error         *string `gorm:"size:512;column:error"`
    StartedAt     time.Time
    EndedAt       *time.Time `gorm:"column:ended_at"`
    DeviceID      *uint64    `gorm:"column:device_id"`
    LLMCallID     *string    `gorm:"size:64;column:llm_call_id"`
    CreatedAt     time.Time
}
```

## 4. 关键函数与流程

### `Session.TableName`
- **签名**：`func (Session) TableName() string`
- **职责**：固定 SQLite 表名为 `chat_sessions`，避免包重命名后 GORM 误创建新表

### `Session.BeforeCreate`
- **签名**：`func (s *Session) BeforeCreate(*gorm.DB) error`
- **职责**：在 caller 未预填 ID 时自动生成 UUIDv4
- **流程**：检查 `s.ID == ""` → `uuid.NewString()`；否则不动；返回 nil
- **设计意图**：把 ID 生成收敛到一处，repo / 测试不必手动赋值

### `Message.TableName / Message.BeforeCreate`
- 同 Session 模式：表名 `chat_messages`；ID 空则填 UUIDv4

### `ToolCall.TableName / ToolCall.BeforeCreate`
- 同上：表名 `chat_tool_calls`；ID 空则填 UUIDv4

## 5. 依赖关系

- **内部包**：无（纯 schema 包）
- **外部库**：`github.com/google/uuid`、`gorm.io/gorm`
- **被调用方**：`manager/biz/aiops` 下的 SessionRepo / MessageRepo / ToolCallRepo；SPA 工作流 worker-tree 视图（按 ParentSessionID fan-out）

## 6. 并发与资源管理

- 本文件仅定义 schema，无锁 / channel / 缓存
- `BeforeCreate` 是 GORM hook，由 GORM 在插入前同步调用
- `ToolCalls []ToolCall` 是 `gorm:"-"` 瞬态字段，由 repo 在 ListMessages 后填充，不参与 DB 读写

## 7. 设计模式与亮点

- **UUID 主键防枚举**：Session/Message/ToolCall 用 char(36) UUID；客户端可在服务端确认前先持 id 用于乐观导航
- **UserID 而非 OrgID**：post-pivot 后按 user_id 作用域；user_id 仍为 uint64 FK
- **Worker / 子 agent 审计列**：AgentID / ParentSessionID / Background 支持多 agent 编排；均加索引以便 SPA worker-tree fan-out O(log N)
- **Kind 区分用户/调查会话**：`/chat` 列表过滤 `kind='user'`，避免每个 alert RCA 自动派生的调查会话淹没操作员
- **RelatedIncidentID 反向链**：IncidentDetail "深入诊断" 按钮创建的会话回指告警事件
- **Message.Content 用 `*string`**：让"只有 tool_calls 无文本"的 assistant 消息以 NULL 表示，避免空字符串与缺失混淆
- **Model 字段 per-message provenance**：role=assistant 才设；让 SPA 显示"此回答来自 glm-4-plus"的中途切换审计
- **LLMCallID 配对**：DeepSeek v4+ 拒绝孤儿 tool 消息；NULL 时回退按顺序配对
- **ToolCalls 瞬态字段**：解决 role=assistant content=NULL 但 tool_calls 非空的历史回放
- **CHECK 约束**：role / status 都加 DB-level CHECK，防非法值落库
- **DeviceID 整数复用**：May 2026 entity split 后与 legacy edge_id 1:1 对齐

## 8. 注意事项

- **ScopeJSON 可空**：nil 指针表示"不限制"；空数组 `[]` 也是合法值
- **Background 默认 false**：迁移时旧行需显式补默认，保证子 agent 路径不混淆
- **LLMCallID 历史 NULL**：旧行回退按顺序配对；新增行务必写入 LLM 返回的 call id
- **PromptTokens / CompletionTokens 可空**：仅 assistant 消息可能填，用于预算追踪
- **ClosedAt 可空**：会话未关闭时为 NULL；关闭时由 biz 更新
- **Kind 取值受约束**：当前 `user` / `investigation`；新增 kind 需同步 `/chat` 过滤逻辑
- **CHECK 约束跨方言**：SQLite / MySQL 8 均支持；老 MySQL 5.7 需注意 CHECK 仅语法接受不生效
