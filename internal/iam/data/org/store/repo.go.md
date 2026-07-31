# `repo.go` 技术实现文档

> 源文件：`internal/iam/data/org/store/repo.go`
> 包路径：`github.com/ongridio/ongrid/internal/iam/data/org/store`

## 1. 概述

本文件是 IAM BC 组织子域的 GORM 仓储实现，满足 `biz/org.Repo` 接口。提供 org CRUD、按名查询（用于 seed 查找）、子节点计数（用于删除前守卫）、总数计数。所有单行操作按 RowsAffected 映射 `ErrNotFound`，所有查询经 `WithContext` 透传 ctx。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/iam/data/org/store` —— data 层组织持久化
- **依赖方向**：被 `internal/iam/biz/org.Service` 通过接口调用；依赖 `internal/iam/model`、`internal/pkg/errs`

## 3. 关键类型与接口

```go
type Repo struct {
	db *gorm.DB
}
```

仅持有 `*gorm.DB`，无状态。

## 4. 关键函数与流程

### `NewRepo`
- **签名**：`func NewRepo(db *gorm.DB) *Repo`
- **职责**：构造 Repo。

### `Create`
- **签名**：`func (r *Repo) Create(ctx, o *model.Org) error`
- **职责**：插入 org，GORM 回填 ID + 时间戳。

### `GetByID`
- **签名**：`func (r *Repo) GetByID(ctx, id uint64) (*model.Org, error)`
- **职责**：按主键查询。
- **错误处理**：`gorm.ErrRecordNotFound` 映射为 `errs.ErrNotFound`。

### `GetByName`
- **签名**：`func (r *Repo) GetByName(ctx, name string) (*model.Org, error)`
- **职责**：按 name 唯一索引查询。
- **注释要点**：用于启动路径查找 seed「默认组织」，避免硬编码 ID。

### `List`
- **签名**：`func (r *Repo) List(ctx) ([]*model.Org, error)`
- **职责**：返回全部 org（id asc）。
- **注释要点**：调用方应为 superuser；非 superuser 应走 `MembershipRepo.ListByUser`。

### `Update`
- **签名**：`func (r *Repo) Update(ctx, id uint64, name, description string, parentID *uint64) error`
- **职责**：覆写 name + description + parent_id。
- **流程**：`Updates(map[string]any{...})` 使用 map 以确保零值（parent_id=NULL）也被写入。
- **错误处理**：`RowsAffected == 0` → `ErrNotFound`。
- **注释要点**：parent_id 校验（自指 / 不存在）是 biz 层职责。

### `CountChildren`
- **签名**：`func (r *Repo) CountChildren(ctx, id uint64) (int64, error)`
- **职责**：统计 `parent_id == id` 的子 org 数。
- **注释要点**：biz 层据此拒绝删除非空父 org。

### `Delete`
- **签名**：`func (r *Repo) Delete(ctx, id uint64) error`
- **职责**：硬删除 org。
- **错误处理**：`RowsAffected == 0` → `ErrNotFound`。
- **注释要点**：不级联成员关系（biz 层负责，需同时清 casbin）。

### `Count`
- **签名**：`func (r *Repo) Count(ctx) (int64, error)`
- **职责**：返回 org 总数。

## 5. 依赖关系

- **内部包**：`internal/iam/model`、`internal/pkg/errs`
- **外部库**：`gorm.io/gorm`、标准库 `context` / `errors`
- **被调用方**：`internal/iam/biz/org.Service`（通过 Repo 接口）

## 6. 并发与资源管理

无显式锁；依赖 GORM/DB 并发安全。所有方法首参 `context.Context`，经 `WithContext` 透传。

## 7. 设计模式与亮点

- **RowsAffected 统一映射**：所有单行操作（Update/Delete）按 `RowsAffected == 0` 返回 `ErrNotFound`，语义清晰。
- **map 更新规避零值忽略**：`Update` 使用 `map[string]any` 而非 struct，确保 `parent_id=NULL` 能正确写入（GORM struct 更新会忽略零值指针）。
- **职责分离**：data 层仅持久化，完整性约束（环检测 / 自指 / 级联清理）由 biz 层负责。

## 8. 注意事项

- 硬删除不可恢复；未来若需软删除需引入 `gorm.DeletedAt`（当前注释明确未启用）。
- `CountChildren` 与 `Delete` 非原子，并发删除父子 org 可能出现 biz 检查通过但实际已无父的中间态；依赖 biz 层调用方串行化。
- 无外键约束（注释说明 sqlite 方言漂移），数据完整性完全依赖 biz 层；建议在监控中加 orphan 检测。
- `Update` 接受 `parentID *uint64`，nil 表示置 NULL；调用方需明确语义，避免误传 nil 把顶级 org 改成无父。
