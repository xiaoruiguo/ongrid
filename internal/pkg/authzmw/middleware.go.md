# `middleware.go` 技术实现文档（authzmw）

> 源文件：`internal/pkg/authzmw/middleware.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/authzmw`

## 1. 概述

该文件实现 manager 侧的 casbin 授权（authorization）中间件，将 iam 的 `authz.Enforcer` 包装为 chi 友好的 handler 装饰器。核心设计是**对超管硬短路**——即使 casbin 策略被错误配置甚至损坏，系统管理员也永远不会被锁在外面。中间件依赖上游 `auth.Middleware` 已完成认证并把 `tenantctx.Tenant` 写入 context。

## 2. 包信息

- **包名**：`authzmw`
- **所属模块**：`internal/pkg/`（基础设施层）
- **依赖方向**：被 `cmd/ongrid` 路由装配调用；依赖 `internal/pkg/errs`、`internal/pkg/tenantctx`；通过 `Authorizer` 接口间接消费 iam 的 `authz.Enforcer`。

## 3. 关键类型与接口

### `Authorizer`
窄接口，使导入 `authzmw` 的包不必拉入 iam 的 BC。

```go
type Authorizer interface {
    Allow(ctx context.Context, userID, orgID uint64, obj, act string) bool
    AllowAnyOrg(ctx context.Context, userID uint64, obj, act string) bool
}
```

iam/biz/authz 的 `*Enforcer` 满足该接口。

### `Middleware`
持有 Authorizer 与 logger 的中间件容器。

```go
type Middleware struct {
    z   Authorizer
    log *slog.Logger
}
```

## 4. 关键函数与流程

### `New`
- **签名**：`func New(z Authorizer, log *slog.Logger) *Middleware`
- **职责**：构造中间件。`log` 为 nil 时退化为 `slog.Default()`。

### `Require`
- **签名**：`func (m *Middleware) Require(obj, act string) func(http.Handler) http.Handler`
- **职责**：返回针对 `(obj, act)` 强制授权的 chi 中间件。
- **流程**（解析顺序）：
  1. `tenantctx.From(r.Context())` 取租户；缺失 → 401 `ErrUnauthorized`。
  2. `t.IsSuperuser` 为真 → **直接放行**（硬短路，跳过 casbin）。
  3. `m.z == nil`（legacy / 测试 wiring） → 放行（保证未部署 iam Phase-1 时系统仍可用）。
  4. `m.z.AllowAnyOrg(ctx, userID, obj, act)` 为真 → 放行。
  5. 否则记录 `authz: denied` 日志（user/obj/act），返回 403 `ErrForbidden`。
- **错误处理**：401 / 403 使用 `errs` 包的 sentinel error 文本，与全局错误映射一致。
- **Phase 2 路径**：未来接受 `X-Active-Org` header，走 `Allow(user, orgID, obj, act)` 针对特定 org；目前资源尚未 org-scoped，过早做反而错。

### 对象命名约定（Phase 1）
`edge:*` / `knowledge:doc` / `knowledge:repo` / `alert:rule` / `alert:incident` / `agent:custom` / `monitor:panel` / `org:*` / `user:*`。动作词：`read` / `write` / `delete` / `manage`。

## 5. 依赖关系

- **内部包**：`internal/pkg/errs`（sentinel error）、`internal/pkg/tenantctx`（租户上下文）。
- **外部库**：`log/slog`、`net/http`、`context`。
- **被调用方**：`cmd/ongrid` 路由装配；典型用法 `r.With(mw.Require("edge:*", "write")).Post("/v1/edges", ...)`。

## 6. 并发与资源管理

无并发控制。`Middleware` 字段在构造后不变；`Authorizer` 实现自身的线程安全由 iam 侧保证（casbin Enforcer 内部加锁）。

## 7. 设计模式与亮点

- **窄接口隔离 BC**：`Authorizer` 在消费方定义，避免 `authzmw` 反向依赖 iam BC，符合 gospec 接口在消费方定义的红线。
- **超管硬短路**：超管绕过 casbin 是安全网，防策略损坏锁死管理员；这是产品决策而非漏洞。
- **优雅降级**：`m.z == nil` 时不报错而是放行，兼容未启用 iam 的部署形态。
- **日志结构化**：denied 日志带 `user`/`obj`/`act`，便于审计与策略调优。

## 8. 注意事项

- **依赖上游认证**：`Require` 假设请求已被 `auth.Middleware` 装饰；裸用 `Require` 而无认证会得到 401，而非 403。
- **`AllowAnyOrg` 粒度**：当前仅校验"任一 org"权限，未做资源级 org 隔离；Phase 2 引入 `X-Active-Org` 时需同步改造。
- **nil Authorizer 放行风险**：`m.z == nil` 时全部放行，部署时务必确认生产环境注入了真实 Enforcer，否则授权形同虚设。
- **超管短路不可绕过**：超管一旦 token 泄露将拥有全部权限，超管 token 的 TTL 与审计需更严格。
