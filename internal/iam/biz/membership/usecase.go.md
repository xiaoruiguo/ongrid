# `usecase.go` 技术实现文档

> 源文件：`internal/iam/biz/membership/usecase.go`
> 包路径：`github.com/ongridio/ongrid/internal/iam/biz/membership`

## 1. 概述

本文件实现 IAM BC 中 `OrgMembership` 用例层，提供 `AddMember` / `ChangeRole` / `RemoveMember` 等成员管理操作及列表查询。关键设计：每次 mutation 都通过注入的 `CasbinHook` 同步 casbin `g` 策略，使 HR 真值表与 `casbin_rule` 永不漂移。

## 2. 包信息

- **包名**：`membership`
- **所属模块**：`internal/iam/biz/membership` —— IAM BC biz 层成员关系子域
- **依赖方向**：被 `internal/iam/service` 调用；依赖 `internal/iam/data/membership/store`（接口）与 `internal/iam/model`

## 3. 关键类型与接口

```go
// Repo is the narrow data contract.
type Repo interface {
	Upsert(ctx context.Context, userID, orgID uint64, role string) (*model.OrgMembership, error)
	Delete(ctx context.Context, userID, orgID uint64) error
	DeleteByOrg(ctx context.Context, orgID uint64) error
	DeleteByUser(ctx context.Context, userID uint64) error
	ListByOrg(ctx context.Context, orgID uint64) ([]store.MembershipWithUser, error)
	ListByUser(ctx context.Context, userID uint64) ([]store.MembershipWithOrg, error)
	All(ctx context.Context) ([]model.OrgMembership, error)
}

// CasbinHook is the narrow authz contract.
type CasbinHook interface {
	SyncMembership(ctx context.Context, userID, orgID uint64, role string) error
	RevokeMembership(ctx context.Context, userID, orgID uint64) error
}

// Service is the public usecase.
type Service struct {
	repo  Repo
	authz CasbinHook
}
```

- `Repo`：消费方定义的窄口径持久化契约，实现位于 `data/membership/store.Repo`。
- `CasbinHook`：仅暴露成员级 sync/revoke 两个方法的 authz 契约，避免本包依赖 casbin 具体 Enforcer 类型。
- `Service`：公开用例门面，持有 repo + authz。

## 4. 关键函数与流程

### `New`
- **签名**：`func New(repo Repo, authz CasbinHook) *Service`
- **职责**：依赖注入构造 Service。

### `AddOrUpdate`
- **签名**：`func (s *Service) AddOrUpdate(ctx context.Context, userID, orgID uint64, role string) (*model.OrgMembership, error)`
- **职责**：upsert 成员关系并同步 casbin。
- **流程**：
  1. `model.IsValidMembershipRole(role)` 校验角色，失败返回 `errs.ErrInvalid`。
  2. `repo.Upsert` 写入真值表。
  3. 若 authz 非 nil，调用 `authz.SyncMembership` 同步 casbin `g`。
- **错误处理**：角色非法 → ErrInvalid；Upsert 错误透传；casbin 同步失败包装为 `sync casbin: %w`。

### `Remove`
- **签名**：`func (s *Service) Remove(ctx context.Context, userID, orgID uint64) error`
- **职责**：删除成员关系并撤销 casbin。
- **流程**：`repo.Delete` → `authz.RevokeMembership`。
- **错误处理**：Delete 错误透传；casbin 失败包装为 `revoke casbin: %w`。

### `ListByOrg` / `ListByUser`
- **签名**：分别返回 `[]store.MembershipWithUser` / `[]store.MembershipWithOrg`
- **职责**：纯透传到 repo 查询。

### `All`
- **签名**：`func (s *Service) All(ctx context.Context) ([]model.OrgMembership, error)`
- **职责**：返回全部成员关系，供 Authorizer 启动时 hydrate casbin。

## 5. 依赖关系

- **内部包**：`internal/iam/data/membership/store`（取 `MembershipWithUser` / `MembershipWithOrg` 类型）、`internal/iam/model`、`internal/pkg/errs`
- **外部库**：无（仅标准库 `context` / `fmt`）
- **被调用方**：`internal/iam/service.Service`（持有 `*membership.Service`）、`internal/iam/biz/org`（通过 `MembershipCleaner` 接口被 org.Delete 调用 `DeleteByOrg`）

## 6. 并发与资源管理

无显式并发控制；依赖底层 repo 与 authz 的内部并发安全。所有方法首参为 `context.Context`，符合 gospec 红线。

## 7. 设计模式与亮点

- **双写一致性**：每次 mutation 同时更新真值表与策略表，避免单一来源失效后授权漂移。
- **接口隔离**：`CasbinHook` 仅暴露本子域需要的两个方法，避免反向依赖 casbin 全集 API。
- **可选注入**：`authz` 可为 nil（`if s.authz != nil` 守卫），便于单测或灰度部署。

## 8. 注意事项

- 双写非事务：repo 写入成功但 casbin 同步失败时，真值表已变更但策略表未更新，下次 hydrate 才能纠正；当前未实现补偿机制。
- `All` 用于启动 hydrate，大规模部署下需评估一次性加载成本。
- `CasbinHook` 为 nil 时静默跳过同步，需在集成测试中显式覆盖该路径。
