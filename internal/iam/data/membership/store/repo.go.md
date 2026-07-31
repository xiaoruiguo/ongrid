# `repo.go` 技术实现文档

> 源文件：`internal/iam/data/membership/store/repo.go`
> 包路径：`github.com/ongridio/ongrid/internal/iam/data/membership/store`

## 1. 概述

本文件是 IAM BC 成员关系子域的 GORM 仓储实现，满足 `biz/membership.Repo` 接口。提供 upsert / delete / 批量删除 / 双向列表（按 org 带 user、按 user 带 org）/ 全量查询。所有查询经 `WithContext` 透传 ctx，所有删除按 RowsAffected 映射 `ErrNotFound`。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/iam/data/membership/store` —— data 层成员关系持久化
- **依赖方向**：被 `internal/iam/biz/membership.Service` 通过接口调用；依赖 `internal/iam/model`、`internal/pkg/errs`

## 3. 关键类型与接口

```go
type Repo struct {
	db *gorm.DB
}

type MembershipWithUser struct {
	model.OrgMembership
	User model.User `gorm:"-"`
}

type MembershipWithOrg struct {
	model.OrgMembership
	Org model.Org `gorm:"-"`
}
```

- `Repo`：仅持有 `*gorm.DB`，无状态。
- `MembershipWithUser` / `MembershipWithOrg`：聚合 DTO，嵌入 `OrgMembership` + 关联的 User/Org。`gorm:"-"` 标记关联实体不映射列。

## 4. 关键函数与流程

### `NewRepo`
- **签名**：`func NewRepo(db *gorm.DB) *Repo`
- **职责**：构造 Repo。

### `Upsert`
- **签名**：`func (r *Repo) Upsert(ctx, userID, orgID uint64, role string) (*model.OrgMembership, error)`
- **职责**：按 (user, org) 唯一索引 upsert。
- **流程**：`Where(user_id AND org_id).First` → 命中且 role 变更则 `Update("role", role)` → 未命中则 `Create`。
- **错误处理**：非 NotFound 错误透传。
- **注释要点**：幂等，启动 seeder 可安全重复调用。

### `Delete`
- **签名**：`func (r *Repo) Delete(ctx, userID, orgID uint64) error`
- **职责**：按 (user, org) 删除。
- **错误处理**：`RowsAffected == 0` 返回 `ErrNotFound`。

### `DeleteByOrg` / `DeleteByUser`
- **签名**：分别按 org_id / user_id 批量删除。
- **错误处理**：透传 GORM 错误，不区分 RowsAffected（批量删除 0 行不算异常）。

### `ListByOrg`
- **签名**：`func (r *Repo) ListByOrg(ctx, orgID uint64) ([]MembershipWithUser, error)`
- **职责**：返回 org 内成员及其 user 信息。
- **流程**：按 org_id 查 memberships（id asc）→ 收集 userIDs → `Where("id IN ?", userIDs).Find(&users)` → map by ID 组装 DTO。
- **错误处理**：空集直接返回 nil；users 查询失败透传。
- **设计要点**：手动两段查询而非 GORM 预加载，避免 cross-table 自动 JOIN 的方言差异。

### `ListByUser`
- **签名**：`func (r *Repo) ListByUser(ctx, userID uint64) ([]MembershipWithOrg, error)`
- **职责**：返回 user 所属 org 及其 org 信息。流程与 ListByOrg 对称。

### `All`
- **签名**：`func (r *Repo) All(ctx) ([]model.OrgMembership, error)`
- **职责**：返回全部成员关系（id asc）。供 Authorizer 启动 hydrate casbin。

## 5. 依赖关系

- **内部包**：`internal/iam/model`、`internal/pkg/errs`
- **外部库**：`gorm.io/gorm`、标准库 `context` / `errors`
- **被调用方**：`internal/iam/biz/membership.Service`（通过 Repo 接口）

## 6. 并发与资源管理

无显式锁；依赖 GORM/底层 DB 的并发安全。所有方法首参 `context.Context`，经 `db.WithContext(ctx)` 透传，支持 ctx 取消与超时。

## 7. 设计模式与亮点

- **两段查询代替预加载**：ListByOrg / ListByUser 先查主表再 `IN` 查关联表，避免 GORM 预加载在不同方言下的 JOIN 行为差异，便于 SQLite/MySQL 双跑。
- **DTO 用 `gorm:"-"` 标记**：嵌入主模型 + 关联实体（非列），保持单一查询结果类型清晰。
- **RowsAffected 统一映射**：单行操作 0 行 → `ErrNotFound`，批量操作不映射，语义明确。

## 8. 注意事项

- `ListByOrg` / `ListByUser` 的关联查询使用 `IN` 子句，userIDs/orgIDs 数量极大时需注意 SQL 占位符上限（SQLite 默认 999）。
- `Upsert` 不是数据库层原子 upsert，存在 TOCTOU 竞态：并发同 (user, org) 两次 Create 可能违反唯一索引；依赖 DB 唯一约束兜底（`idx_user_org`）。
- `DeleteByOrg` / `DeleteByUser` 不返回影响行数，调用方无法感知是否真的清理过数据。
- 所有查询均按 `id asc` 排序，未分页；超大规模 org 成员列表需引入分页。
