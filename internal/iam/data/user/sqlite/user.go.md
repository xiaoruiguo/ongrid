# `user.go` 技术实现文档

> 源文件：`internal/iam/data/user/sqlite/user.go`
> 包路径：`github.com/ongridio/ongrid/internal/iam/data/user/sqlite`

## 1. 概述

本文件是 IAM BC 用户子域的 GORM 仓储实现，实现 `internal/iam/biz/user.Repo` 接口（编译期断言 `var _ biz.Repo = (*Repo)(nil)`）。覆盖用户的 CRUD 与全部字段更新方法（role / profile / status / superuser / pass_hash）。所有单行 Update/Delete 按 RowsAffected 映射 `ErrNotFound`。

## 2. 包信息

- **包名**：`sqlite`
- **所属模块**：`internal/iam/data/user/sqlite` —— data 层用户持久化
- **依赖方向**：被 `internal/iam/biz/user.Usecase` 通过接口调用；依赖 `internal/iam/model`、`internal/pkg/errs`

## 3. 关键类型与接口

```go
type Repo struct {
	db *gorm.DB
}

// compile-time interface check.
var _ biz.Repo = (*Repo)(nil)
```

- `Repo`：仅持有 `*gorm.DB`，无状态。
- `var _ biz.Repo = (*Repo)(nil)`：编译期断言，保证本类型实现 biz 层接口，避免接口漂移。

## 4. 关键函数与流程

### `NewRepo`
- **签名**：`func NewRepo(db *gorm.DB) *Repo`
- **职责**：构造 Repo。

### `Create`
- **签名**：`func (r *Repo) Create(ctx, u *model.User) error`
- **职责**：插入用户，GORM 回填 ID + 时间戳。
- **错误处理**：`u == nil` 返回 `ErrInvalid`（包装）；DB 错误透传。

### `GetByEmail`
- **签名**：`func (r *Repo) GetByEmail(ctx, email string) (*model.User, error)`
- **职责**：按 email 唯一索引查询。
- **错误处理**：`gorm.ErrRecordNotFound` → `errs.ErrNotFound`。

### `GetByID`
- **签名**：`func (r *Repo) GetByID(ctx, id uint64) (*model.User, error)`
- **职责**：按主键查询。
- **错误处理**：同 GetByEmail。

### `List`
- **签名**：`func (r *Repo) List(ctx) ([]*model.User, error)`
- **职责**：返回全部用户（id asc）。

### `Count`
- **签名**：`func (r *Repo) Count(ctx) (int64, error)`
- **职责**：返回用户总数。供 `BootstrapAdmin` 判断首启。

### `Delete`
- **签名**：`func (r *Repo) Delete(ctx, id uint64) error`
- **职责**：硬删除用户。
- **错误处理**：`RowsAffected == 0` → `ErrNotFound`。

### `UpdateRole`
- **签名**：`func (r *Repo) UpdateRole(ctx, id uint64, role string) error`
- **职责**：更新 role 列。
- **错误处理**：`RowsAffected == 0` → `ErrNotFound`。

### `UpdateProfile`
- **签名**：`func (r *Repo) UpdateProfile(ctx, id uint64, displayName, phone string) error`
- **职责**：同时更新 display_name + phone。
- **流程**：`Updates(map[string]any{...})` 使用 map 确保零值也被写入。
- **错误处理**：`RowsAffected == 0` → `ErrNotFound`。

### `UpdateStatus`
- **签名**：`func (r *Repo) UpdateStatus(ctx, id uint64, status string) error`
- **职责**：切换 active/disabled（软删除等价）。
- **错误处理**：`RowsAffected == 0` → `ErrNotFound`。

### `UpdateSuperuser`
- **签名**：`func (r *Repo) UpdateSuperuser(ctx, id uint64, isSuperuser bool) error`
- **职责**：切换 is_superuser 列。
- **错误处理**：`RowsAffected == 0` → `ErrNotFound`。

### `UpdatePassHash`
- **签名**：`func (r *Repo) UpdatePassHash(ctx, id uint64, passHash string) error`
- **职责**：设置新的 argon2id 编码 hash。
- **错误处理**：`RowsAffected == 0` → `ErrNotFound`。

## 5. 依赖关系

- **内部包**：`internal/iam/biz/user`（取 `biz.Repo` 接口，alias 为 `biz`）、`internal/iam/model`、`internal/pkg/errs`
- **外部库**：`gorm.io/gorm`、标准库 `context` / `errors` / `fmt`
- **被调用方**：`internal/iam/biz/user.Usecase`（通过 Repo 接口）

## 6. 并发与资源管理

无显式锁；依赖 GORM/DB 并发安全。所有方法首参 `context.Context`，经 `WithContext` 透传。

## 7. 设计模式与亮点

- **编译期接口断言**：`var _ biz.Repo = (*Repo)(nil)` 在编译期保证接口实现，避免运行时才发现方法缺失。
- **RowsAffected 统一映射**：所有 Update* 与 Delete 按 0 行 → `ErrNotFound`，语义一致。
- **map 更新规避零值忽略**：`UpdateProfile` 使用 map 而非 struct，确保空字符串也能写入。
- **薄仓储**：仅持久化，不做业务校验（角色合法性、状态合法性等由 biz 层 `usecase.go` 保障）。

## 8. 注意事项

- 硬删除不可恢复；`UpdateStatus(disabled)` 是软删除等价但行仍在；真正的 GDPR 删除需走 `Delete`。
- `UpdateProfile` 用 map 会绕过 GORM 钩子与 zero-value 处理，需确保调用方传入完整新值（HTTP `updateUser` 即先读后写）。
- 所有 Update* 单列更新，若需事务性多列更新应引入事务封装。
- 无分页，`List` 返回全部用户；大规模部署需分页。
- `UpdateSuperuser` 标注「Reserved for migrations」，业务流程不应直接调用。
