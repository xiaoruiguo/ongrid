# `user_agent.go` 技术实现文档

> 源文件：`internal/manager/data/aiops/store/user_agent.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/aiops/store`

## 1. 概述

本文件实现 user-defined persona（Phase 3 引入）的持久化层。用户可在 UI 自定义 agent persona（系统提示、工具白/黑名单、permission mode、model、max_turns 等），与磁盘加载的 persona 在 API 层合并。红线：name 字段唯一索引保证不撞名；删除 persona 后，已绑定该 agent 的 session（`Session.AgentID`）运行时回退到全局默认（见 runtime.go::Handle）。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/aiops`
- **依赖方向**：被 `internal/manager/biz/aiops` 与 chatruntime AgentRegistry boot 装配；依赖 `internal/manager/model/aiops`、`internal/pkg/errs`、`gorm.io/gorm`。

## 3. 关键类型与接口

```go
// UserAgentRepo 是用户自定义 persona 的持久化层。
// 同 SessionRepo 包在同一 Repo 中共享 gorm 连接池。
type UserAgentRepo struct {
    db *gorm.DB
}
```

## 4. 关键函数与流程

### `NewUserAgentRepo`
- **签名**：`func NewUserAgentRepo(db *gorm.DB) *UserAgentRepo`
- **职责**：构造 repo。

### `List`
- **签名**：`func (r *UserAgentRepo) List(ctx) ([]*model.UserAgent, error)`
- **职责**：按 name 升序返回全部 user-defined persona。用于启动时 hydrate chatruntime AgentRegistry，以及 `/v1/agents` listing 端点（在 API 层与磁盘 persona 合并）。

### `GetByName`
- **签名**：`func (r *UserAgentRepo) GetByName(ctx, name string) (*model.UserAgent, error)`
- **职责**：按 name 取单条；`gorm.ErrRecordNotFound` → `errs.ErrNotFound`。

### `Create`
- **签名**：`func (r *UserAgentRepo) Create(ctx, ua *model.UserAgent) error`
- **职责**：插入新 persona。caller 已校验 name 不与磁盘 persona 撞名。
- **错误处理**：name 唯一索引冲突 → gorm UniqueViolation；service 层映射为 ErrInvalid 或" name already exists"。

### `Update`
- **签名**：`func (r *UserAgentRepo) Update(ctx, name string, ua *model.UserAgent) error`
- **职责**：覆盖 persona 行的所有可编辑列。`RowsAffected == 0` → `ErrNotFound`。
- **更新字段**：description / when_to_use / system_prompt / critical_reminder / allowed_tools_json / disallowed_tools_json / permission_mode / model / max_turns / updated_at。

### `Delete`
- **签名**：`func (r *UserAgentRepo) Delete(ctx, name string) error`
- **职责**：按 name 删除 persona。`RowsAffected == 0` → `ErrNotFound`。
- **行为**：绑定到已删除 agent 的 session（`Session.AgentID`）在运行时回退全局默认。

## 5. 依赖关系

- **内部包**：`internal/manager/model/aiops`、`internal/pkg/errs`
- **外部库**：`gorm.io/gorm`
- **被调用方**：`internal/manager/biz/aiops` usecase；chatruntime AgentRegistry 启动 hydrate；`/v1/agents` API handler。

## 6. 并发与资源管理

- **无显式锁**：依赖 gorm 与 DB 唯一索引。
- **ctx 透传**：所有方法首参 ctx，`db.WithContext(ctx)` 透传。
- **连接共享**：与 `SessionRepo`、`MutatingProposalRepo` 共享 `*gorm.DB`。

## 7. 设计模式与亮点

- **name 唯一索引 + service 层映射**：DB 层用唯一索引保证不撞名，service 层把 UniqueViolation 翻译为友好错误。
- **persona 与 session 解耦**：删除 persona 不级联影响 session，运行时回退默认 persona，保证历史 session 可继续运行。
- **List 用于双源合并**：DB persona + 磁盘 persona 在 API 层合并，repo 层只关心 DB 部分。
- **Update 全字段覆盖**：caller 传完整 row，避免部分更新导致的字段漂移。

## 8. 注意事项

- **name 不可改**：Update 按 name 定位，不支持改名；如需改名需删 + 建。
- **caller 校验撞名**：Create 前需由 service 层校验不与磁盘 persona 撞名，repo 仅保证 DB 内唯一。
- **deleted persona 行为**：session 运行时回退默认，但 UI 上可能仍显示已删除 agent 的历史 session；caller 需处理显示降级。
- **更新字段列表需同步**：扩展 UserAgent 模型字段时需同步更新 Update 的 updates map，否则字段被静默丢弃。
