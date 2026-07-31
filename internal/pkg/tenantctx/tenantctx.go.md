# `tenantctx.go` 技术实现文档

> 源文件：`internal/pkg/tenantctx/tenantctx.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/tenantctx`

## 1. 概述

本文件在 `context.Context` 上承载每请求的调用方身份（Tenant）。auth middleware 解码 JWT claims 后调用 `With` 写入；下游 service / biz / data 层通过 `From` 读取以校验角色与归属。命名中的 "tenant" 是历史遗留 — 单租户 MVP 中只有一个用户命名空间，Tenant 实际就是已认证调用方。文件还实现了"可变 slot"机制，让外层 middleware（audit）能看到内层 middleware（auth）写入的值。

## 2. 包信息

- **包名**：`tenantctx`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被 auth middleware（写入）、audit middleware（读取）、service/biz/data 层（读取）调用；仅依赖标准库 `context`

## 3. 关键类型与接口

```go
type Tenant struct {
    UserID      uint64
    Email       string
    Role        string // "admin" / "user"（legacy）
    IsSuperuser bool   // 系统管理员权威标志
}

type ctxKey struct{}   // 直接值 context key
type slotKey struct{}  // 可变 slot context key

type slot struct {
    t   Tenant
    set bool
}
```

无导出接口；导出函数 `With` / `From` / `WithSlot` / `SetOnSlot` 是公共 API。

## 4. 关键函数与流程

### `With`
- **签名**：`func With(ctx context.Context, t Tenant) context.Context`
- **职责**：把 Tenant 附着到 ctx，供下游 handler 读取
- **流程**：`context.WithValue(ctx, ctxKey{}, t)` 返回派生 ctx
- **错误处理**：无

### `From`
- **签名**：`func From(ctx context.Context) (Tenant, bool)`
- **职责**：从 ctx 提取 Tenant；bool=false 表示无 Tenant（公共端点或缺失 middleware）
- **流程**：
  1. 优先检查 `slotKey{}` 上的 `*slot`：若存在且 `set==true` → 返回 `slot.t, true`
  2. 否则回退到 `ctxKey{}` 上的 `Tenant`
- **错误处理**：bool 表达存在性；无 error

### `WithSlot`
- **签名**：`func WithSlot(ctx context.Context) context.Context`
- **职责**：在 ctx 上安装一个空的"可变 Tenant slot"，供后续 `SetOnSlot` 写入
- **流程**：`context.WithValue(ctx, slotKey{}, &slot{})` — 注意是 `*slot` 指针，让后续修改对持有该 ctx 的代码可见
- **错误处理**：无

### `SetOnSlot`
- **签名**：`func SetOnSlot(ctx context.Context, t Tenant)`
- **职责**：把 t 写入已安装的 slot；无 slot 时 no-op（防御性，公共端点不安装 slot 也不应崩溃 auth middleware）
- **流程**：从 ctx 取 `*slot`，非空则 `s.t = t; s.set = true`
- **错误处理**：无 slot 时静默返回

## 5. 依赖关系

- **内部包**：无
- **外部库**：无
- **被调用方**：
  - `auth` middleware：JWT 验证后 `WithSlot` + `SetOnSlot`（或直接 `With`）
  - `audit` middleware：`WithSlot` 在外层安装 slot，handler 后通过 `From` 读取
  - service / biz / data 层：`From` 读取做权限/归属校验

## 6. 并发与资源管理

- **`*slot` 指针共享**：外层 middleware 持有的 ctx 与内层 `r.WithContext()` 产生的新 ctx 共享同一个 `*slot` 指针，这是 slot 机制能跨 ctx 边界传递修改的关键
- **无锁**：slot 的写入（`SetOnSlot`）发生在 auth middleware 的请求处理路径，单 goroutine 内完成；audit middleware 在 handler 返回后读取，时序上有 happens-before 保证（HTTP 框架的 ServeHTTP 同步调用）。若未来有异步 middleware 链需重新评估
- **`context.WithValue`**：标准库线程安全

## 7. 设计模式与亮点

- **可变 slot 模式**：解决"外层 middleware 看不到内层 middleware 写入"的经典 Go HTTP 难题。`r.WithContext()` 产生新 ctx 但外层 request 引用仍是旧 ctx；用 `*slot` 指针让两边共享同一存储。注释明示"Mirrors the auditSlot pattern in the audit middleware"
- **优先 slot，回退 ctxKey**：`From` 先查 slot 再查 ctxKey，让旧调用方（未用 slot）与新调用方（用 slot）兼容共存
- **`IsSuperuser` 权威化**：注释说明 `IsSuperuser` 是系统管理员权威标志，从 JWT claim 读取，回退到 `Role=="admin"`（旧 token 兼容）
- **Email 内嵌**：避免 audit middleware 为打标签而额外查 DB（2026-05-21 修复 audit_view 空用户字段）
- **防御性 SetOnSlot**：无 slot 时 no-op，让公共端点不安装 slot 也不会让 auth middleware 崩溃

## 8. 注意事项

- **`*slot` 不加锁的前提**：当前 HTTP middleware 链是同步的；若引入异步 middleware（如 goroutine 内的 auth）需加锁
- **slot 与 ctxKey 双路径**：`From` 优先 slot，但若调用方混用（部分代码 `With`、部分代码 `WithSlot`+`SetOnSlot`）可能产生不一致；建议项目内统一用 slot 模式
- **`Tenant` 是值类型**：从 ctx 取出后修改不会影响 ctx 内的值；如需更新应重新 `With` 或 `SetOnSlot`
- **多租户扩展**：注释提到"public multi-tenant lands the field will be added" — 当前无 OrgID，未来多租户需扩展 Tenant 结构
- **`Role` 字段 legacy**：`IsSuperuser` 是权威，`Role` 仅作旧 token 兼容；新代码应优先检查 `IsSuperuser`
