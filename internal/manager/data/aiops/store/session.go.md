# `session.go` 技术实现文档

> 源文件：`internal/manager/data/aiops/store/session.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/aiops/store`

## 1. 概述

本文件实现 `biz.SessionRepo` 接口的 GORM 落地，覆盖 AIOps chat 的会话生命周期（创建 / 列表 / 软关闭 / 重命名 / 硬删除）、消息追加与历史回放、工具调用记录与终结、token 用量聚合。核心设计：消息回放必须 hydrate 关联的 ToolCall（否则 tool-call-only 的 assistant turn 会被丢弃，后续 role=tool 消息变孤儿，strict LLM provider 报 HTTP 400 "tool must follow tool_calls"）。红线：DeleteSession 用单事务级联清理（无 FK 声明，cascade 手动）。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/aiops`
- **依赖方向**：被 `internal/manager/biz/aiops` 装配；依赖 `internal/manager/biz/aiops`（接口与 `TokenSums`）、`internal/manager/model/aiops`（GORM 模型及 Role/Status/Kind 常量）、`internal/pkg/errs`、`gorm.io/gorm`。

## 3. 关键类型与接口

```go
// SessionRepo 是 biz/aiops.SessionRepo 的 GORM 实现。
type SessionRepo struct {
    db *gorm.DB
}

// 编译期接口断言
var _ biz.SessionRepo = (*SessionRepo)(nil)
```

biz 层 `TokenSums`（由 biz 包定义，本文件填充）：

```go
// 聚合字段：prompt_tokens / completion_tokens / requests
type TokenSums struct {
    PromptTokens, CompletionTokens int
    Requests                       int64
}
```

## 4. 关键函数与流程

### 构造器

- `NewSessionRepo(db) *SessionRepo`：直接持有 db。
- `NewBizRepo(db) biz.SessionRepo`：wire-ready 构造器，cmd/ongrid 装配时绑定，避免暴露具体类型给 composition root。

### `CreateSession`
- **签名**：`func (r *SessionRepo) CreateSession(ctx, s *model.Session) error`
- **职责**：插入 s；`s == nil` → `errs.ErrInvalid`。

### `GetSession`
- **签名**：`func (r *SessionRepo) GetSession(ctx, id string) (*model.Session, error)`
- **职责**：按 id 取 session；`gorm.ErrRecordNotFound` → `errs.ErrNotFound`。

### `ListByParent`
- **签名**：`func (r *SessionRepo) ListByParent(ctx, parentID string) ([]*model.Session, error)`
- **职责**：返回 `parent_session_id == parentID` 的所有 session，按 `created_at ASC, id ASC` 排序（chronological spawn order）。供 SPA worker-tree 视图与 coordinator fan-out 审计查询使用。空 parentID 返回空切片（非 nil）。

### `ListSessions`
- **签名**：`func (r *SessionRepo) ListSessions(ctx, userID uint64, limit, offset int, relatedIncidentID *uint64) ([]*model.Session, error)`
- **职责**：列出 userID 的会话，按 `created_at DESC` 排序。
- **关键约束**：仅返回 `kind = SessionKindUser` 的会话；investigation 会话由 alert RCA 自动生成，存同表用于审计，但 chat-list 视图不应展示（IncidentDetail 页面直接读 `investigation_reports`，不走此 list）。
- **流程**：`user_id` + `kind=user` Where；`relatedIncidentID != nil` 时加过滤；`limit > 0` 加 Limit；`offset > 0` 加 Offset；Order + Find。

### `CloseSession`
- **签名**：`func (r *SessionRepo) CloseSession(ctx, id string) error`
- **职责**：软关闭，`closed_at = now`。幂等：重复关闭覆盖时间戳，符合 soft-close 语义。`RowsAffected == 0` → `ErrNotFound`。

### `RenameSession`
- **签名**：`func (r *SessionRepo) RenameSession(ctx, id, title string) error`
- **职责**：更新 title，bump `updated_at` 使列表重排序到顶部（符合"编辑带回最近"的用户预期）。

### `DeleteSession`
- **签名**：`func (r *SessionRepo) DeleteSession(ctx, id string) error`
- **职责**：硬删除 session 及其所有 messages、tool_calls。UI"删除聊天"动作触发；想保留审计的 caller 用 `CloseSession`。
- **流程**（单事务）：
  1. 删除 `chat_tool_calls` 中 `message_id IN (SELECT id FROM chat_messages WHERE session_id = ?)` 的行
  2. 删除 `chat_messages` 中 `session_id = ?` 的行
  3. 删除 `chat_sessions` 中 `id = ?` 的行；`RowsAffected == 0` → `ErrNotFound`
- **关键约束**：schema 无 FK 声明，cascade 完全手动；依赖 `chat_tool_calls(message_id) → chat_messages` 与 `chat_messages(session_id) → chat_sessions` 的删除顺序。

### `AppendMessage`
- **签名**：`func (r *SessionRepo) AppendMessage(ctx, m *model.Message) error`
- **职责**：插入 m；`m == nil` → `ErrInvalid`。

### `ListMessages`
- **签名**：`func (r *SessionRepo) ListMessages(ctx, sessionID string, limit int) ([]*model.Message, error)`
- **职责**：返回 sessionID 的消息，按时间正序回放。
- **流程**：
  1. `limit <= 0`：全量，`Order("created_at ASC, id ASC")`
  2. `limit > 0`：取最近 N 条，`Order("created_at DESC, id DESC").Limit(limit)`，再反转回正序
  3. `hydrateToolCalls(ctx, out)` 批量 hydrate ToolCall
- **关键约束**：必须 hydrate ToolCall，否则 tool-call-only 的 assistant turn（Content 为 NULL）会被丢弃，后续 role=tool 消息变孤儿，strict LLM provider 报 400。

### `hydrateToolCalls`
- **签名**：`func (r *SessionRepo) hydrateToolCalls(ctx, msgs []*model.Message) error`
- **职责**：批量 SELECT `chat_tool_calls` keyed on assistant message ids，原位挂载到对应 message。
- **流程**：
  1. 收集 `Role == RoleAssistant` 的 message id + 建索引 byID
  2. 无 assistant → 直接返回
  3. `Where("message_id IN ?", assistantIDs).Order("created_at ASC, id ASC").Find(&tcs)`
  4. 遍历 tcs，按 byID 挂载到 `m.ToolCalls`
- **关键约束**：单次批量查询避免 N+1；同 turn 内顺序由 `(created_at, id)` 保证（agent 串行持久化 tool_calls，created_at 单调，id 为稳定 tiebreak）。

### `CreateToolCall`
- **签名**：`func (r *SessionRepo) CreateToolCall(ctx, tc *model.ToolCall) error`
- **职责**：插入 tc；`tc == nil` → `ErrInvalid`。

### `SumTokensSince`
- **签名**：`func (r *SessionRepo) SumTokensSince(ctx, since time.Time) (biz.TokenSums, error)`
- **职责**：聚合 `created_at >= since` 的所有 assistant message 的 prompt_tokens / completion_tokens / request 数。NULL 列按 0 计（COALESCE）。
- **实现**：单条 raw SQL，注释明示"single raw query so the gorm chain stays cheap and the SQL is auditable"。

### `UpdateToolCallResult`
- **签名**：`func (r *SessionRepo) UpdateToolCallResult(ctx, id, status string, resultJSON, errStr *string, endedAt time.Time) error`
- **职责**：填充 pending tool-call 行的 status / result / error / ended_at。status 应为 `StatusSuccess` / `StatusError` / `StatusTimeout`。`RowsAffected == 0` → `ErrNotFound`。

### `FinalizePendingToolCalls`
- **签名**：`func (r *SessionRepo) FinalizePendingToolCalls(ctx, sessionID string, resultJSON, errStr string, endedAt time.Time) (int64, error)`
- **职责**：把 session 内所有 `status = StatusPending` 的 tool-call 行标为 error。PersistenceHandler 已写 role=tool 自愈 stub 保证回放正确；此 repo 级清扫保证审计/UI 行状态终态化（处理 callback end event 丢失场景）。返回 heal 行数。

## 5. 依赖关系

- **内部包**：`internal/manager/biz/aiops`（接口、`TokenSums`、`MutatingProposalFilter`）、`internal/manager/model/aiops`（模型及 Role/Status/Kind 常量）、`internal/pkg/errs`
- **外部库**：`gorm.io/gorm`
- **被调用方**：`internal/manager/biz/aiops` usecase；`cmd/ongrid` 通过 `NewBizRepo` 装配

## 6. 并发与资源管理

- **无显式锁**：所有读写通过 GORM；DeleteSession 用 `db.Transaction` 保证级联删除原子性。
- **ctx 透传**：所有方法首参 ctx，`db.WithContext(ctx)` 透传。
- **批量查询避免 N+1**：`hydrateToolCalls` 单次 SELECT 处理整批 assistant message。
- **连接共享**：与同包其他 repo 共享 `*gorm.DB`。

## 7. 设计模式与亮点

- **hydrateToolCalls 修复 400 bug**：注释详细描述了"tool-call-only turn 被丢弃 → role=tool 孤儿 → provider 400"的故障链；批量 hydrate 是根本修复。
- **limit 取最近 N 再反转**：`Order DESC + Limit + 反转` 实现"最近 N 条但仍正序回放"，避免子查询。
- **ListSessions 排除 investigation 会话**：通过 `kind = SessionKindUser` 过滤，把审计表与用户视图分离。
- **DeleteSession 手动 cascade**：因 schema 无 FK，事务内按依赖顺序删除。
- **FinalizePendingToolCalls 双层自愈**：PersistenceHandler 写 role=tool stub（回放层）+ repo 级清扫（审计/UI 层），分别处理"消息序列正确"与"行状态终态"两个独立问题。
- **SumTokensSince raw SQL**：可审计、cheap，避免 gorm 链复杂化。
- **RenameSession bump updated_at**：让重命名会话回到列表顶部，符合用户预期。

## 8. 注意事项

- **limit 语义**：`ListMessages` 的 `limit <= 0` 表示全量；`limit > 0` 表示最近 N 条。caller 需注意区分。
- **DeleteSession 不可逆**：硬删除；保留审计用 `CloseSession`。
- **CloseSession 幂等但覆盖时间戳**：重复关闭会更新 `closed_at`，符合 soft-close 语义但失去原始关闭时间。
- **ListByParent 空 parentID 返回空切片**：非 nil，caller 可直接 range。
- **FinalizePendingToolCalls 仅扫 StatusPending**：终态行（success/error/timeout）不受影响。
- **SumTokensSince 仅算 assistant**：role=tool / user 的 token 不计（它们无 prompt_tokens/completion_tokens 列）。
- **时间排序 tiebreak**：所有 Order 都用 `(created_at, id)` 双字段，避免同时间戳行顺序不确定。
- **无 FK 约束**：所有级联删除依赖代码顺序，新增挂载到 session 的表需同步更新 DeleteSession。
