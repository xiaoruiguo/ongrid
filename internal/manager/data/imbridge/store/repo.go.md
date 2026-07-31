# `repo.go` 技术实现文档

> 源文件：`internal/manager/data/imbridge/store/repo.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/imbridge/store`

## 1. 概述

本文件实现 IM bridge 的持久化层，覆盖 `im_apps`（平台 bot 凭据）与 `im_threads`（IM 会话 → ongrid session 映射）。核心设计：`GetAppByAppID` 在 webhook 入口解析 platform app_id → ImApp 以验签；`FindThread` 按 (im_app_id, im_chat_id, im_thread_id) 查找，一个 chat 共享一个 session，Feishu reply thread_id 是唯一区分维度；`RotateThreadSession` 在 session 老化或用户 `/new` 时覆盖 ongrid_session_id 并 bump last_seen_at。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/imbridge`
- **依赖方向**：被 `internal/manager/biz/imbridge` 装配；依赖 `internal/manager/model/imbridge`、`internal/pkg/errs`、`gorm.io/gorm`。

## 3. 关键类型与接口

```go
// Repo 是 IM bridge 表的 gorm 持久化层。
// 镜像其他 manager 子系统的 (data) → (biz interface) 分离模式。
type Repo struct {
    db *gorm.DB
}
```

## 4. 关键函数与流程

### ImApp

#### `New`
- **签名**：`func New(db *gorm.DB) *Repo`

#### `ListApps`
- **签名**：`func (r *Repo) ListApps(ctx, provider string) ([]*model.ImApp, error)`
- **职责**：按 provider 过滤列出 ImApp，`id DESC` 排序。provider 空表示列全部。

#### `ListEnabledStreamApps`
- **签名**：`func (r *Repo) ListEnabledStreamApps(ctx) ([]*model.ImApp, error)`
- **职责**：返回全部 (enabled, mode=stream) ImApp，`id ASC` 排序。
- **被调用方**：StreamSupervisor 在 boot 与每次 reconcile tick 调用。

#### `GetApp` / `GetAppByAppID`
- `GetApp`：按 PK 取；`gorm.ErrRecordNotFound` → `ErrNotFound`。
- `GetAppByAppID`：按 (provider, app_id) 取。
- **用途**：`GetAppByAppID` 在 webhook 入口解析 platform app_id → ImApp，决定用哪个 app 的 secret 验签。

#### `CreateApp` / `UpdateApp` / `DeleteApp`
- `CreateApp`：Create。
- `UpdateApp`：`Save(app)` 全字段更新。
- `DeleteApp`：软删 by id。

### ImThread

#### `FindThread`
- **签名**：`func (r *Repo) FindThread(ctx, imAppID uint64, imChatID, imThreadID string) (*model.ImThread, error)`
- **职责**：按 (im_app_id, im_chat_id, im_thread_id) 查找 thread 映射。
- **关键约束**：一个 chat 共享一个 session（group / DM）；唯一区分维度是 Feishu reply 的 thread_id（用户在 thread 内回复时）。ImSenderID 记录在行但不参与 lookup key。
- **错误处理**：`gorm.ErrRecordNotFound` → `ErrNotFound`。

#### `CreateThread`
- 插入新 thread 映射。

#### `TouchThread`
- **签名**：`func (r *Repo) TouchThread(ctx, id uint64) error`
- **职责**：bump `last_seen_at = now`。

#### `RotateThreadSession`
- **签名**：`func (r *Repo) RotateThreadSession(ctx, threadID uint64, newSessionID string) error`
- **职责**：覆盖 thread 的 `ongrid_session_id`。用于：
  - 上一 session 老化（LastSeenAt > IdleTimeout）
  - 用户显式 `/new` 请求新 session
- **关键约束**：同时 bump `last_seen_at`，防止新 session 立即 re-rotate。

## 5. 依赖关系

- **内部包**：`internal/manager/model/imbridge`、`internal/pkg/errs`
- **外部库**：`gorm.io/gorm`、`context`、`errors`、`time`
- **被调用方**：`internal/manager/biz/imbridge` usecase；StreamSupervisor（ListEnabledStreamApps）；webhook 入口（GetAppByAppID）

## 6. 并发与资源管理

- **无显式锁**：依赖 gorm 与 DB 唯一索引。
- **ctx 透传**：所有方法首参 ctx。
- **RotateThreadSession 非事务**：覆盖 ongrid_session_id + bump last_seen_at 在单 Updates 中原子完成。

## 7. 设计模式与亮点

- **GetAppByAppID webhook 验签入口**：webhook 入口解析 app_id → ImApp，决定验签 secret。
- **FindThread 三键 lookup**：(im_app_id, im_chat_id, im_thread_id) 唯一区分；Feishu thread_id 是 chat 内细粒度区分。
- **RotateThreadSession 同时 bump last_seen_at**：防止新 session 立即 re-rotate，匹配 idle timeout 语义。
- **ListEnabledStreamApps 供 StreamSupervisor**：boot + reconcile tick 单查询拉全部启用 stream app。
- **session 创建属 biz 层**：注释明示 ImThread 不创建 chat_session，session 创建属 aiops biz 层（封装 owner_user_id 解析、model 默认值等）。

## 8. 注意事项

- **FindThread ImSenderID 不参与 lookup**：一个 chat 共享 session，sender 仅记录；如需 per-sender session 需扩展 lookup key。
- **RotateThreadSession 非事务**：覆盖 + bump 在单 Updates 原子完成；如需跨表事务（如同时作废旧 session）需 caller 包事务。
- **UpdateApp 用 Save**：全字段更新；caller 需传完整 row。
- **DeleteApp 软删**：gorm 默认 DeletedAt；硬删需 Unscoped。
- **ListApps provider 空列全部**：caller 不传 provider 时列全部平台 app。
