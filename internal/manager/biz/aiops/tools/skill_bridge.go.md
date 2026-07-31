# skill_bridge.go

## 1. 概述

本文件实现 `skill_bridge`——自动把每个 safe skill（`internal/skill` registry）注册为 OpenAI function-calling Tool 到 aiops Registry。是 `inventory_bridge.go` 的反向：skill_bridge 是 skill registry → aiops Tool registry（让 LLM 把 skills 当 function-calling tools 用）。

**Wiring 设计**：

- Tool name = skill metadata Key（lower_snake，已 unique）
- Tool description = skill Description
- Tool schema = `skill.ParamSchema.ToJSONSchema()` + 额外 `edge_id`（uint64）prepended——每个 skill execution targets specific edge，LLM 必须提供
- Tool execute = unmarshal args → `{edge_id, ...skillParams}`，调 `manager/biz/skill.Service.Execute`，返回 raw result JSON

**只注册 ClassSafe skills**：mutating / dangerous 需要 human-in-the-loop workflow（PR-G4）才能让 agent 调用。

## 2. 包信息

- **包名**：`tools`
- **路径**：`internal/manager/biz/aiops/tools/skill_bridge.go`
- **导入**：
  - `skillsvc`（`internal/manager/biz/skill`）—— `Caller` / `ExecuteInput` / `ExecuteOutput` / `Service`
  - `skillcore`（`internal/skill`）—— `Executor` / `Metadata` / `ClassSafe` / `ScopeHost` / `ScopeManager` / `BuildSchema` / `AllByClass` / `SubprocessSkill`
- **Class**：N/A（这是 bridge，不是工具）

## 3. 关键类型与接口

### `SkillRunner`（接口）

```go
type SkillRunner interface {
    Execute(ctx context.Context, caller skillsvc.Caller, in skillsvc.ExecuteInput) (*skillsvc.ExecuteOutput, error)
}
```

窄 contract，`*skillsvc.Service` 满足；测试 inject fake。

### `skillsvc.Caller`（system identity）

```go
// agent caller 透传 system identity（UserID=0, Role="system"）
// 因为 tool calls 来自 LLM 而非 human；audit row 记录该区别
skillsvc.Caller{Role: "system"}
```

## 4. 关键函数与流程

### `RegisterSafeSkills(svc SkillRunner)`

`svc == nil` → return。

遍历 `skillcore.AllByClass(skillcore.ClassSafe)`：

1. `meta = e.Metadata()`
2. `schema, err = buildSkillToolSchema(e)`——失败 log Warn 跳过（不 crash）
3. `desc = meta.Description`，若 `meta.ResultPreview != ""` 追加 `\n\nReturns: <preview>`
4. `name = meta.Key`
5. `r.Register(Tool{Name, Description: desc, Schema: schema, Execute: r.newSkillExecutor(svc, name, meta.EffectiveScope())})`

注释明示 idempotent——re-registration overwrites。

### `buildSkillToolSchema(e) (json.RawMessage, error)`

按 scope 生成 schema：

1. **SubprocessSkill 特殊处理**：`if ss, ok := e.(*skillcore.SubprocessSkill); ok && len(ss.Schema) > 0`——直接 unmarshal `ss.Schema` 为 `base`，确保 `type` 字段存在。SubprocessSkill 的 raw schema 在 struct field（skill.json 的 "schema"），不在接口方法。
2. **其他 skill**：`skillcore.BuildSchema(e)`——honors `RawSchemaProvider` extension（inventory bridge 用于 hand-written BaseTools 的 schemas with arrays / nested objects）。Unmarshal 为 `base`。
3. **ScopeHost 注入 `device_id`**：
   - `base["properties"]["device_id"] = {type: integer, description: "Device id to run this skill on (required)..."}`
   - `base["required"]` 追加 `"device_id"`（若不存在）
   - 注释解释：schema-level identifier 是 `device_id`（匹配 @-mention chip id 和 Prom label），executor 解析为 edge_id 给 tunnel call。`edge_id` 作为 confusing alias 在 device split 前 lingered，现在完全从 schema 排除防止 prompts latching back。
4. **ScopeManager 不注入 edge_id**：manager-scoped skills 运行 in-process，不需要 edge target。

### `newSkillExecutor(svc, key, scope) func(ctx, args) (ExecuteResult, error)`

返回 Tool.Execute 闭包：

1. Unmarshal args 为 `map[string]json.RawMessage` envelope。
2. **ScopeHost 处理**：
   - 拉 `device_id`（integer），delete from envelope
   - **legacy alias**：`device_id == 0` 时拉 `edge_id`，delete from envelope
   - `device_id == 0` → error "device_id required"
   - `resolveEdgeForDeviceID(ctx, deviceID)` 解析 device_id → host edge_id via junction
   - `edgeID == 0` → error "device_id=%d has no host-edge link (try query_devices to list available device ids)"
3. **ScopeManager 处理**：
   - `delete(envelope, "edge_id")` / `delete(envelope, "device_id")`——silently strip 而非 erroring（注释："model tends to recover better from silent ignore"）
4. Re-marshal envelope 为 params。
5. `svc.Execute(ctx, Caller{Role: "system"}, ExecuteInput{Key, EdgeID, Params})`。
6. **错误处理**：ScopeHost 错误带 edge_id（"skill %q on edge %d: %w"），ScopeManager 不带。
7. Marshal `{result: out.Result, error: out.Error}`——LLM 看到 structured output even on skill-side errors。
8. `ExecuteResult{ResultJSON: body}`，若 `edgeID != 0` 设 `DeviceID = &eid`（audit 列）。

### `resolveEdgeForDeviceID(ctx, deviceID) uint64`

```go
func (r *Registry) resolveEdgeForDeviceID(ctx context.Context, deviceID uint64) uint64 {
    eid, _ := NewDeviceResolver(r.devices, r.edges).ResolveEdgeID(ctx, deviceID)
    return eid
}
```

PR-9 路由 through shared `DeviceResolver`，让 host_files / skill_bridge / 任何 future ScopeHost tool 用同一规则。Registry-method shape 保留让 call sites 一行。

注释明示 fallback：device row 不存在时 treat input as raw edge_id——preserves back-compat with prompts that already think in edge ids。

## 5. 依赖关系

| 方向 | 依赖 | 说明 |
|------|------|------|
| 上游 | `Registry`（闭包持有者） | `r.devices` / `r.edges` / `r.log` |
| 下游 | `SkillRunner.Execute`（接口，由 `*skillsvc.Service` 实现） | skill 执行 |
| 下游 | `skillcore.AllByClass` / `BuildSchema` | skill registry 遍历 + schema 构建 |
| 共享 | `DeviceResolver`（与 host_files / restart_service 共享） | device_id → edge_id 解析 |

## 6. 并发与资源管理

- `RegisterSafeSkills` 是 boot 期单线程注册，运行期只读。
- 每个 skill executor 闭包无共享状态，并发安全（依赖 `SkillRunner.Execute` 的线程安全）。
- 无 per-call timeout——依赖外层 ctx。

## 7. 设计模式与亮点

- **Bridge pattern**：skill registry → aiops Tool registry，让 LLM 通过 function-calling 用 skills——避免为每个 skill 手写 BaseTool。
- **ClassSafe 过滤**：只注册 safe skills，mutating / dangerous 需要 PR-G4 human-in-the-loop——权限边界清晰。
- **ScopeHost vs ScopeManager schema 分流**：ScopeHost 注入 `device_id`（required），ScopeManager 不注入——匹配 skill 执行语义（edge-targeted vs in-process）。
- **`device_id` 而非 `edge_id`**：post-split 后 schema 用 `device_id`（匹配 @-mention chip id 和 Prom label），`edge_id` 作为 legacy alias 在 executor 内部接受但不进 schema——防止 prompts latching back onto `edge_id`。
- **legacy alias `edge_id` 兼容**：executor 内部 `device_id == 0` 时 fallback 到 `edge_id`——让旧 prompt 仍能工作，平滑迁移。
- **SubprocessSkill raw schema 优先**：`skill.json` 的 "schema" 字段 verbatim 使用（manifests should already be in function-calling shape），不走 `ParamSchema.ToJSONSchema()`——避免 down-conversion 损失 arrays / nested objects。
- **`RawSchemaProvider` extension**：`skillcore.BuildSchema` honors 该接口（inventory bridge 用于 hand-written BaseTools）——统一 schema 构建路径。
- **system identity 透传**：`Caller{Role: "system"}` 让 audit row 记录 tool call 来自 LLM 而非 human——审计追溯清晰。
- **silently strip manager-scope edge_id**：LLM confused 时送 edge_id 给 manager-scope skill，不 erroring 而 strip——"model tends to recover better from silent ignore"。
- **structured error envelope**：`{result, error}` 即使 skill-side error 也返回 structured output——LLM 看到 error 字段而非 raw error string。
- **shared `DeviceResolver`**：PR-9 路由 through shared resolver，让 host_files / skill_bridge / future ScopeHost tools 用同一 device_id → edge_id 规则——避免规则 drift。

## 8. 注意事项

- **`resolveEdgeForDeviceID` fallback 是 back-compat**：device row 不存在时 treat input as raw edge_id——老部署 junction rows 缺失时工作，但新部署应该依赖 junction。
- **`device_id` required for ScopeHost**：LLM 必须提供，否则 error——LLM 应该先调 `query_devices` 拿 device_id。
- **schema build 失败跳过**：`buildSkillToolSchema` 失败 log Warn 跳过该 skill——不会 crash 整个注册，但 skill 不可用，LLM 看不到。
- **`SubprocessSkill` 类型断言**：`e.(*skillcore.SubprocessSkill)` 是 concrete type 断言——若未来有其他 raw-schema skill 类型需要扩展。
- **`edgeID == 0` error 带 hint**：error message 含 "try query_devices to list available device ids"——引导 LLM 自我修正。
- **无 per-call timeout**：skill execution 可能慢（edge RPC），依赖外层 ctx。
- **ClassSafe 过滤是软约束**：若未来 mutating skill 误注册为 ClassSafe，会绕过 human-in-the-loop——skill registry 的 class 标记要准确。
- **idempotent re-registration**：`Register` 静默 overwrite——若 skill 更新后重新注册，old executor 被覆盖，无 warning。
- **`Caller{Role: "system"}` 无 UserID**：audit row 记录 system identity，但某些 skill 可能有 user-scoped 副作用（如写文件到 user 目录）——这种 skill 不应该 ClassSafe。
