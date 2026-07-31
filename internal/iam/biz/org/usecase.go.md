# `usecase.go` 技术实现文档

> 源文件：`internal/iam/biz/org/usecase.go`
> 包路径：`github.com/ongridio/ongrid/internal/iam/biz/org`

## 1. 概述

本文件实现 IAM BC 中 Org（组织）用例层，提供扁平组织（1 层）的 CRUD 与启动期 seed「默认组织」。关键不变量：平台仅允许单一顶级组织（seed），新 org 若未显式指定 parent 将自动 reparent 到 seed 之下。权限校验位于 manager middleware，本包仅做输入校验与持久化。

## 2. 包信息

- **包名**：`org`
- **所属模块**：`internal/iam/biz/org` —— IAM BC biz 层组织子域
- **依赖方向**：被 `internal/iam/service` 调用；依赖 `internal/iam/model`、`internal/pkg/errs`

## 3. 关键类型与接口

```go
const defaultSeedName = "默认组织"

type Repo interface {
	Create(ctx context.Context, o *model.Org) error
	GetByID(ctx context.Context, id uint64) (*model.Org, error)
	GetByName(ctx context.Context, name string) (*model.Org, error)
	List(ctx context.Context) ([]*model.Org, error)
	Update(ctx context.Context, id uint64, name, description string, parentID *uint64) error
	Delete(ctx context.Context, id uint64) error
	Count(ctx context.Context) (int64, error)
	CountChildren(ctx context.Context, id uint64) (int64, error)
}

type MembershipCleaner interface {
	DeleteByOrg(ctx context.Context, orgID uint64) error
}

type CasbinHook interface {
	RevokeAllForOrg(ctx context.Context, orgID uint64) error
}

type Service struct {
	repo        Repo
	memberships MembershipCleaner
	authz       CasbinHook
}

type CreateInput struct {
	Name        string
	Description string
	ParentID    *uint64
}

type UpdateInput struct {
	Name        string
	Description string
	SetParent   bool
	ParentID    *uint64
}
```

- `defaultSeedName`：平台根 org 的规范名常量，seed 侧与 auto-reparent 侧共用，避免漂移。
- `Repo`：消费方定义的持久化契约。
- `MembershipCleaner` / `CasbinHook`：窄口径依赖契约，分别用于级联清理成员与 casbin `g` 规则。
- `Service`：构造时注入 repo + memberships + authz（后两者可 nil）。

## 4. 关键函数与流程

### `New`
- **签名**：`func New(repo Repo, memberships MembershipCleaner, authz CasbinHook) *Service`
- **职责**：依赖注入构造 Service。

### `Create`
- **签名**：`func (s *Service) Create(ctx context.Context, in CreateInput) (*model.Org, error)`
- **职责**：校验输入并持久化新 org，保证「单一顶级 org」不变量。
- **流程**：
  1. TrimSpace name，空或 >128 字符返回 `ErrInvalid`。
  2. 若指定 ParentID：`GetByID` 校验存在；未指定：`GetByName(defaultSeedName)`，若 seed 存在则 reparent 到 seed，否则允许顶级（首次 EnsureSeed 路径）。
  3. 组装 `model.Org` → `repo.Create`。
- **错误处理**：所有校验失败包装为 `errs.ErrInvalid`；repo 错误透传。
- **注释要点**：May 2026 出现 ongridio 顶级 org 与「默认组织」并立的 bug，此分支专门修复。

### `EnsureSeed`
- **签名**：`func (s *Service) EnsureSeed(ctx context.Context, name, description string) (*model.Org, error)`
- **职责**：幂等创建「默认组织」；已存在则返回现有行。
- **流程**：`GetByName` 命中即返回；否则 `Create`；Create 失败时 race-safe 再查一次。
- **错误处理**：Create 失败后 GetByName 成功则返回该行，否则返回原错误。

### `Get` / `List` / `Count`
- 纯透传 repo 方法。

### `Update`
- **签名**：`func (s *Service) Update(ctx context.Context, id uint64, in UpdateInput) (*model.Org, error)`
- **职责**：校验并更新 org，含 parent 迁移与环检测。
- **流程**：
  1. name 校验。
  2. `GetByID` 取当前行。
  3. 若 `SetParent`：ParentID 非空时校验「不能自指」「父存在」并做环检测（沿候选父祖链回溯，最多 1024 跳，遇自身 ID 即拒绝）。
  4. `repo.Update` → `GetByID` 返回最新行。
- **错误处理**：所有不变量违反包装为 `ErrInvalid`。

### `Delete`
- **签名**：`func (s *Service) Delete(ctx context.Context, id uint64) error`
- **职责**：删除 org 并级联清理成员与 casbin。
- **流程**：
  1. `CountChildren`，>0 拒绝（需先迁移子 org）。
  2. `memberships.DeleteByOrg` 清成员关系。
  3. `authz.RevokeAllForOrg` 清 casbin `g`。
  4. `repo.Delete`。
- **错误处理**：每步包装错误；has-children 返回 `ErrInvalid`。
- **注释要点**：保守策略——拒绝级联删除子 org，避免数据丢失与递归 reparent 意外。

## 5. 依赖关系

- **内部包**：`internal/iam/model`、`internal/pkg/errs`
- **外部库**：无（标准库 `context` / `fmt` / `strings`）
- **被调用方**：`internal/iam/service.Service`（持有 `*org.Service`）；其 `MembershipCleaner` 实际由 `membership.Service` 提供（`DeleteByOrg` 方法）

## 6. 并发与资源管理

无显式锁；依赖 repo 的并发安全。所有方法首参为 `context.Context`。

## 7. 设计模式与亮点

- **单一顶级 org 不变量**：通过 `defaultSeedName` 常量 + `GetByName` 反查而非硬编码 ID，避免 fixture 漂移。
- **环检测**：祖先链回溯 + 跳数上限 1024，防止畸形数据导致死循环。
- **保守删除**：拒绝级联删除子 org，强制操作者显式迁移，降低数据丢失风险。
- **三段式依赖注入**：repo + memberships + authz 三组窄接口，避免 monolith 依赖。
- **race-safe EnsureSeed**：Create 失败后再次 GetByName 兜底并发创建。

## 8. 注意事项

- 多步操作（Delete 中三步清理）非事务，部分失败会留下不一致状态；当前无补偿/回滚。
- 环检测 1024 跳上限对极深嵌套可能误判为通过；当前 org 扁平（1 层）无实际风险，未来若放开层级需调整。
- `EnsureSeed` 的 race-safe 兜底依赖 `name` 唯一索引；若数据库未强制唯一，并发 Create 会产生重复 seed。
- auto-reparent 在 seed 不存在时静默放行顶级，依赖 `EnsureSeed` 先于其他 Create 调用；启动顺序在 `cmd/ongrid` 中需保证。
