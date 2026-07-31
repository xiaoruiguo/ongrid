# `repo.go` 技术实现文档

> 源文件：`internal/manager/data/approval/store/repo.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/approval/store`

## 1. 概述

本文件实现 approval inbox 的持久化层。核心设计：`Decide` 用 `WHERE id = ? AND status = 'pending'` 乐观守卫防止双决策；`SetResult` 在 approved action 执行后记录结果；`CountPending` 供导航 badge 显示。审批生命周期：pending → approved/rejected（Decide）→ executed（SetResult）。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/approval`
- **依赖方向**：被 `internal/manager/biz/approval` 装配；依赖 `internal/manager/model/approval`、`internal/pkg/errs`、`gorm.io/gorm`。

## 3. 关键类型与接口

```go
// Repo 是 approvals 表的 GORM 持久化层。
type Repo struct{ db *gorm.DB }
```

## 4. 关键函数与流程

### `NewRepo`
- **签名**：`func NewRepo(db *gorm.DB) *Repo`

### `Create`
- **签名**：`func (r *Repo) Create(ctx, a *model.Approval) error`
- **职责**：插入新 proposal。

### `Get`
- **签名**：`func (r *Repo) Get(ctx, id string) (*model.Approval, error)`
- **职责**：按 id 取单条；`gorm.ErrRecordNotFound` → `errs.ErrNotFound`。

### `List`
- **签名**：`func (r *Repo) List(ctx, status string, limit int) ([]*model.Approval, error)`
- **职责**：按 status 过滤，最新优先，limit 默认 100。
- **流程**：`Order("created_at DESC").Limit(limit)`；`status != ""` 加 Where。

### `CountPending`
- **签名**：`func (r *Repo) CountPending(ctx) (int64, error)`
- **职责**：返回 pending proposal 数，供导航 badge 显示。

### `Decide`
- **签名**：`func (r *Repo) Decide(ctx, id string, fields map[string]any) error`
- **职责**：持久化 status / approver / reason / result / timestamps。
- **关键约束**：`WHERE id = ? AND status = 'pending'` 乐观守卫，仅转换仍为 pending 的行，防止双决策。`RowsAffected == 0` → `ErrNotFound`（行不存在或已被他人决策）。

### `SetResult`
- **签名**：`func (r *Repo) SetResult(ctx, id, status, resultJSON string, executedAt time.Time) error`
- **职责**：approved action 执行后记录结果。无 RowsAffected 校验，容忍并发覆盖。

## 5. 依赖关系

- **内部包**：`internal/manager/model/approval`、`internal/pkg/errs`
- **外部库**：`gorm.io/gorm`
- **被调用方**：`internal/manager/biz/approval` usecase

## 6. 并发与资源管理

- **无显式锁**：依赖 `WHERE status = 'pending'` 乐观守卫防双决策。
- **ctx 透传**：所有方法首参 ctx。

## 7. 设计模式与亮点

- **乐观守卫防双决策**：`Decide` 的 WHERE 子句保证仅 pending 行可被决策，DB 层原子性防竞态。
- **Decide vs SetResult 职责分离**：Decide 是人工审批转换，SetResult 是执行结果记录，两者独立调用。
- **CountPending 供 badge**：单独方法避免 List 全量拉取。

## 8. 注意事项

- **Decide 乐观守卫**：`RowsAffected == 0` 既可能是行不存在，也可能是已被他人决策；caller 需根据业务语义区分（通常重新 Get 一次判断）。
- **SetResult 无守卫**：容忍并发覆盖，caller 需自行保证幂等性。
- **fields map 由 caller 构造**：Decide 接受 map 而非结构体，caller 需确保字段名与列名一致。
- **List limit 默认 100**：caller 不传 limit 时兜底 100。
- **时区**：timestamp 由 caller / gorm autoCreateTime 处理，建议统一 UTC。
