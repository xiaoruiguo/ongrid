# `repo.go` 技术实现文档

> 源文件：`internal/iam/biz/user/repo.go`
> 包路径：`github.com/ongridio/ongrid/internal/iam/biz/user`

## 1. 概述

本文件定义 IAM BC 用户子域的持久化契约 `Repo` 接口，是消费方定义的窄口径数据契约。实现位于 `internal/iam/data/user/sqlite`，符合 gospec「接口在消费方定义」红线。

## 2. 包信息

- **包名**：`user`
- **所属模块**：`internal/iam/biz/user` —— biz 层用户子域的持久化抽象
- **依赖方向**：被同包 `usecase.go` 依赖；由 `internal/iam/data/user/sqlite.Repo` 实现

## 3. 关键类型与接口

```go
// Repo is the iam/user persistence contract.
type Repo interface {
	Create(ctx context.Context, u *model.User) error
	GetByEmail(ctx context.Context, email string) (*model.User, error)
	GetByID(ctx context.Context, id uint64) (*model.User, error)
	List(ctx context.Context) ([]*model.User, error)
	Count(ctx context.Context) (int64, error)
	Delete(ctx context.Context, id uint64) error
	UpdateRole(ctx context.Context, id uint64, role string) error
	// UpdateProfile sets display_name + phone. ErrNotFound on missing row.
	UpdateProfile(ctx context.Context, id uint64, displayName, phone string) error
	// UpdateStatus toggles active/disabled (= soft delete via status).
	UpdateStatus(ctx context.Context, id uint64, status string) error
	// UpdateSuperuser sets is_superuser. Reserved for migrations + the
	// superuser-promotes-superuser flow.
	UpdateSuperuser(ctx context.Context, id uint64, isSuperuser bool) error
	// UpdatePassHash sets a new argon2id hash. Used by self-service
	// password reset and admin reset.
	UpdatePassHash(ctx context.Context, id uint64, passHash string) error
}
```

- `Repo`：涵盖用户 CRUD、角色 / 状态 / 超管位 / 密码哈希 / profile 等全部持久化操作的接口契约。

## 4. 关键函数与流程

无独立函数；仅定义接口。每个方法的语义由注释标明（如 `UpdateStatus` 为软删除等价、`UpdateSuperuser` 保留用于迁移与超管互推流程）。

## 5. 依赖关系

- **内部包**：`internal/iam/model`（取 `model.User`）、`context`
- **外部库**：无
- **被调用方**：`internal/iam/biz/user/usecase.go`（Usecase 持有 Repo）；实现位于 `internal/iam/data/user/sqlite/user.go`

## 6. 并发与资源管理

无并发控制（接口定义）。

## 7. 设计模式与亮点

- **消费方接口**：在 biz 层定义，data 层实现，避免 biz 反向依赖具体 ORM。
- **窄口径**：仅暴露 usecase 实际需要的 11 个方法，不含任意 query builder。
- **语义注释**：每个 Update* 方法均附注释说明用途与缺失行行为（`ErrNotFound`），降低实现者歧义。

## 8. 注意事项

- 接口方法签名固定后变更需同步所有实现；当前唯一实现为 `data/user/sqlite.Repo`，编译期 `var _ biz.Repo = (*Repo)(nil)` 校验。
- `UpdateSuperuser` 标注「Reserved for migrations」—— 业务流程不应直接调用，应通过 `SetRole` 间接同步。
- `UpdateProfile` 同时写两个字段；若仅需更新其一，调用方需先读取旧值再传入（HTTP 层 `updateUser` 即此模式）。
