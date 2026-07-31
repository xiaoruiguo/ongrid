# `tenant_bind.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/decorators/tenant_bind.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools/decorators`

## 1. 概述

本文件实现 tenant_bind 装饰器：从 ctx（`internal/pkg/tenantctx`）解析 tenant 身份，append `WithUserID`/`WithTenant` 到 opts 让下游 audit/ratelimit 看到身份；当工具 schema 声明 `tenant_id` 属性且 args 未设置时，把 `tenant_id` 注入到 argsJSON。红线：mutate args 而非只设 InvokeOption——让 LLM 的 tool_call.arguments 自包含便于 replay/audit。

## 2. 包信息

- **包名**：`decorators`
- **所属模块**：`internal/manager/biz/aiops/tools/decorators`
- **依赖方向**：被 `chain.go` 的 `Wrap` 调用；依赖 `basetool`、`internal/pkg/tenantctx`

## 3. 关键类型与接口

```go
type TenantBoundTool struct {
    inner basetool.BaseTool
}
```

无 sentinel error。

## 4. 关键函数与流程

### `WithTenantBind`
- **签名**：`func WithTenantBind(inner basetool.BaseTool) basetool.BaseTool`
- **职责**：包装 inner 注入 tenant 身份
- **流程**：返回 `&TenantBoundTool{inner}`

### `TenantBoundTool.Info`
- **签名**：`func (t *TenantBoundTool) Info(ctx) (*basetool.ToolInfo, error)`
- **职责**：透传 inner.Info（tenant binding 仅 invocation）

### `TenantBoundTool.InvokableRun`
- **签名**：`func (t *TenantBoundTool) InvokableRun(ctx, argsJSON string, opts ...basetool.InvokeOption) (string, error)`
- **职责**：解析 tenant + 注入 args
- **流程**：
  1. `tenantctx.From(ctx)`；!ok → pass-through（公开端点/单测无 tenant 时不拒绝）
  2. append `WithTenant(uidStr)` + `WithUserID(tenant.UserID)` 到 opts（让 audit/ratelimit 不必重新 derive）
  3. `inner.Info(ctx)` 取 Parameters；err/nil/empty → pass-through
  4. `!schemaHasTenantID(Parameters)` → pass-through（工具不声明 tenant_id 不注入）
  5. `injectTenantID(argsJSON, tenant.UserID)` 注入；err → `fmt.Errorf("tenant_bind: rewrite args: %w")`
  6. `inner.InvokableRun(ctx, merged, opts...)`

### `schemaHasTenantID`
- **签名**：`func schemaHasTenantID(schema json.RawMessage) bool`
- **职责**：检测 schema 顶层是否声明 `tenant_id` 属性
- **流程**：unmarshal 取 `properties` map，查 `tenant_id` key 是否存在
- **说明**：浅层检查，PR-3 仅一个工具用此特性

### `injectTenantID`
- **签名**：`func injectTenantID(argsJSON string, uid uint64) (string, error)`
- **职责**：当 argsJSON 未含 tenant_id 时注入
- **流程**：argsJSON 空 → 设 `"{}"`；unmarshal 成 `map[string]json.RawMessage`；已含 tenant_id → 返回原 argsJSON；否则 `json.Marshal(uid)` 写入 map，`json.Marshal(m)` 返回

## 5. 依赖关系

- **内部包**：`basetool`、`internal/pkg/tenantctx`
- **外部库**：标准库 `context`、`encoding/json`、`fmt`、`strconv`
- **被调用方**：`chain.go` 的 `Wrap`（作为最外层装饰器）

## 6. 并发与资源管理

- `TenantBoundTool` 结构 immutable，多 goroutine 共享安全
- inner 的并发契约由 inner 负责

## 7. 设计模式与亮点

- **mutate args 而非只设 opts**：让 tool 实现与闭包形式一致（tool 自描述 tenant_id 字段），LLM 的 tool_call.arguments 自包含便于 replay/audit
- **双重传递**：args 注入 + opts append，audit/ratelimit 不需再 parse args
- **nil-safe pass-through**：无 tenant ctx 时不拒绝（公开端点/单测路径）；schema 不声明 tenant_id 时不注入
- **shallow schema 检查**：仅看顶层 properties.tenant_id，避免复杂嵌套解析（PR-3 仅一个工具用此特性）

## 8. 注意事项

- **不拒绝无 tenant**：拒绝会强制每个测试 setup tenantctx.With()；pass-through 是公开端点/单测 canonical 路径
- **tenant_id 已存在不覆盖**：尊重 LLM 提供的值（如多租户管理场景显式指定）
- **浅层 schema 检查**：未来若有嵌套 tenant_id 需扩展为递归检查
- **作为最外层装饰器**：rewrite 后的 args 流入所有下游（audit 记 rewritten、ratelimit 按 resolved user）
