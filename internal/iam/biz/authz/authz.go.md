# `authz.go` 技术实现文档

> 源文件：`internal/iam/biz/authz/authz.go`
> 包路径：`github.com/ongridio/ongrid/internal/iam/biz/authz`

## 1. 概述

本文件是 IAM 业务能力（BC）的授权实现，封装 casbin `SyncedEnforcer`，对外暴露窄口径、强类型的鉴权接口。它持有 casbin 实例、隐藏策略文件/适配器细节，并通过 `Enforcer.Allow(ctx, userID, orgID, obj, act) bool` 提供核心授权决策；同时负责将 IAM 真值表（`OrgMembership`）镜像到 casbin 的 `g` 分组策略中。

## 2. 包信息

- **包名**：`authz`
- **所属模块**：`internal/iam/biz/authz` —— IAM BC 的 biz 层授权子域
- **依赖方向**：被 `internal/iam/service` 与 manager 侧 middleware 调用；依赖 `internal/iam/model` 与 casbin/gorm-adapter

## 3. 关键类型与接口

```go
// Enforcer wraps casbin.SyncedEnforcer with our typed accessors.
type Enforcer struct {
	mu sync.Mutex
	e  *casbin.SyncedEnforcer
	l  *slog.Logger
}
```

- `Enforcer`：casbin 同步执行器的封装，附加互斥锁与日志器；外部所有操作均经由其方法。
- `rolePolicies`（包级变量）：硬编码的 `p` 策略矩阵，三角色（`org_admin` / `member` / `viewer`）+ `superuser` 兜底。
- `DomainAny = "*"`：域通配符常量，用于跨域策略。
- `modelConf`（`//go:embed model.conf`）：内嵌的 casbin 模型文件。

## 4. 关键函数与流程

### `New`
- **签名**：`func New(db *gorm.DB, log *slog.Logger) (*Enforcer, error)`
- **职责**：构建 Enforcer，使用 gorm-adapter 在 db 上自动建 `casbin_rule` 表并加载策略。
- **流程**：默认化 log → NewAdapterByDB → NewModelFromString（嵌入的 model.conf）→ NewSyncedEnforcer → LoadPolicy。
- **错误处理**：每步以 `fmt.Errorf("authz: ...: %w", err)` 包装，返回失败。

### `SeedRolePolicies`
- **签名**：`func (a *Enforcer) SeedRolePolicies(ctx context.Context) error`
- **职责**：幂等注入 `rolePolicies` 中所有 `p` 行。
- **流程**：加锁 → 遍历 rolePolicies → `AddPolicy`（重复时返回 `false`，被忽略）。
- **错误处理**：失败包装返回；`ok=false` 不视为错误。

### `HydrateMemberships`
- **签名**：`func (a *Enforcer) HydrateMemberships(ctx context.Context, ms []iammodel.OrgMembership) error`
- **职责**：将所有 `OrgMembership` 行批量镜像为 casbin `g` 规则。幂等。
- **流程**：加锁 → 逐行 `AddGroupingPolicy(uidStr, role, oidStr)`。
- **错误处理**：单行失败立即返回包装错误。

### `SyncMembership`
- **签名**：`func (a *Enforcer) SyncMembership(ctx context.Context, userID, orgID uint64, role string) error`
- **职责**：upsert 单条 `g` 规则。被 `biz/membership.AddMember` / `ChangeRole` 调用。
- **流程**：加锁 → `GetFilteredGroupingPolicy(0, uid, "", oid)` 取现存 → 逐条移除匹配 `oid` 的旧分组 → `AddGroupingPolicy(uid, role, oid)`。
- **错误处理**：每步包装错误返回；casbin 不支持隐式替换，故需先删后加。

### `RevokeMembership`
- **签名**：`func (a *Enforcer) RevokeMembership(ctx context.Context, userID, orgID uint64) error`
- **职责**：移除指定 (user, org) 的全部 `g` 规则。
- **流程**：加锁 → 查过滤分组 → 逐条 `RemoveGroupingPolicy`。
- **错误处理**：包装错误返回。

### `RevokeAllForOrg` / `RevokeAllForUser`
- **签名**：`func (a *Enforcer) RevokeAllForOrg(ctx, orgID) error` / `RevokeAllForUser(ctx, userID) error`
- **职责**：批量清空某 org 或某 user 的全部 `g` 规则。分别在 org / user 删除时调用。
- **流程**：加锁 → `GetFilteredGroupingPolicy(2, oid)` 或 `(0, uid)` → 逐条移除。

### `Allow`
- **签名**：`func (a *Enforcer) Allow(ctx context.Context, userID, orgID uint64, obj, act string) bool`
- **职责**：执行 casbin 决策。
- **流程**：`e.Enforce(uidStr, oidStr, obj, act)`；出错时 `slog.Warn` 记录并返回 `false`（fail-closed）。

### `AllowAnyOrg`
- **签名**：`func (a *Enforcer) AllowAnyOrg(ctx context.Context, userID uint64, obj, act string) bool`
- **职责**：无 `X-Active-Org` 头时，遍历用户所属全部 org，首个放行即返回 true。
- **流程**：`userDomains(userID)` 取去重后的域列表 → 逐域 Enforce → 首个 ok 即返回。

### `UserOrgs`
- **签名**：`func (a *Enforcer) UserOrgs(ctx context.Context, userID uint64) ([]uint64, error)`
- **职责**：从 casbin `g` 策略解析用户所属的全部 org id。

### `userDomains` / `uidStr` / `oidStr`
- **职责**：私有辅助，分别解析用户的域列表与统一字符串化 id 约定（避免 "42" / "0042" / uint 混存）。

## 5. 依赖关系

- **内部包**：`github.com/ongridio/ongrid/internal/iam/model`（取 `OrgMembership` 与角色常量）
- **外部库**：`github.com/casbin/casbin/v2`、`github.com/casbin/gorm-adapter/v3`、`gorm.io/gorm`、`log/slog`、`embed`
- **被调用方**：`internal/iam/service`（持有 Enforcer）、`internal/iam/biz/membership`（注入 CasbinHook）、`internal/iam/biz/org`（注入 CasbinHook）、manager 侧 authz middleware（调用 `Allow`）

## 6. 并发与资源管理

- `Enforcer.mu sync.Mutex` 在所有写操作（Seed/Hydrate/Sync/Revoke*）上串行化；底层 `casbin.SyncedEnforcer` 自身亦线程安全，读路径（`Allow` / `UserOrgs`）不加锁。
- `context.Context` 仅作签名约定，未被显式超时控制（casbin 调用本身不感知 ctx）。
- 无 goroutine / channel。

## 7. 设计模式与亮点

- **真理表与策略表分离**：HR 真值（`OrgMembership`）为单一来源，casbin `g` 规则仅作镜像，每次 mutation 经 `SyncMembership` / `RevokeMembership` 双写，避免漂移。
- **角色策略硬编码 + 启动注入**：`rolePolicies` 包级常量，启动时 `SeedRolePolicies` 幂等注入，永不运行时变更。
- **域通配符策略**：使用 `*` 域使单条 `p` 行覆盖所有域；具体域绑定通过 `g` 规则完成。
- **defense in depth**：`superuser` 虽由 middleware 短路处理，仍保留兜底 `p` 行，防止未来未短路路径漏判。
- **fail-closed**：`Allow` 任何内部错误均返回 `false`。

## 8. 注意事项

- `mu sync.Mutex` 与 `SyncedEnforcer` 内部锁形成双层锁，未来若引入更细粒度并发需评估锁竞争。
- `HydrateMemberships` 对重复行采用 no-op insert 策略，注释指出对 ≪10k 成员部署合理；超大规模需改为 diff。
- `AllowAnyOrg` 在用户属多 org 时为 O(n) Enforce 调用，首个放行短路；成员数极多时建议缓存。
- `model.conf` 经 `//go:embed` 编译进二进制，模型变更需重新编译。
- 域字符串化约定（`uidStr` / `oidStr`）必须与 casbin `g`/`p` 行保持一致，任何旁路写入都将导致匹配失败。
