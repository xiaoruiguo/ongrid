# graph/tool_adapter.go

## 1. 概述

本文件是 ongrid 自家 `basetool.BaseTool` 与 cloudwego/eino 的 `einotool.InvokableTool` 之间**唯一的胶水层**。eino 的 `ToolsNode` 只认 `einotool.BaseTool` 接口，而 ongrid 工具生态建立在 `basetool.BaseTool` 之上（PR-3 故意 mirror-shaped 对齐 eino，但保留自有扩展字段如 `WhenToUse`、`Class`）。

除了纯粹的接口适配，本文件还实现了三套运行时机制：

1. **`toolMemo`**：per-run identical-call 缓存（同工具同 args 秒级内不重复执行）
2. **预算上限**：`maxToolCallsPerRun` 截断模型对同一工具的"换 args 重试"循环
3. **`draft_config_change` 预检**：metric 类告警草案必须先调 `list_metric_catalog`

## 2. 包信息

- **包名**：`graph`
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/biz/aiops/graph`
- **角色**：工具适配 + 运行时调用治理
- **依赖**：
  - 标准库 `context`、`encoding/json`、`fmt`、`strings`、`sync`
  - `github.com/cloudwego/eino/components/tool`（`einotool.BaseTool`/`InvokableTool`/`Option`）
  - `github.com/cloudwego/eino/schema`（`schema.ToolInfo`）
  - `github.com/eino-contrib/jsonschema`（JSON Schema 解析）
  - `github.com/ongridio/ongrid/internal/manager/biz/aiops/tools/basetool`

## 3. 关键类型与接口

### `toolMemo` 结构体（未导出）

```go
type toolMemo struct {
    mu     sync.Mutex
    m      map[string]string // (tool\x00args) -> result，identical-call 缓存
    counts map[string]int    // tool name -> distinct 执行次数（本轮）
    last   map[string]string // tool name -> 最近一次成功结果
}
```

per-run 共享，所有 `einoToolAdapter` 实例（同一 `WrapBaseTools` 调用产生）共用一个 memo。生命周期与 graph 运行一致（graph per-request 重建）。

### `einoToolAdapter` 结构体（未导出）

```go
type einoToolAdapter struct {
    inner     basetool.BaseTool
    memo      *toolMemo       // nil = 关闭 memo（单工具 WrapBaseTool 路径，测试用）
    infoOnce  sync.Once
    cacheName string          // Info() 解析出的工具名
    cacheable bool            // Class == "read" 才缓存
}
```

实现 `einotool.InvokableTool` 接口（`Info` + `InvokableRun`）。

### `einoInvokeOptKey` 结构体（未导出）

```go
type einoInvokeOptKey struct {
    opts []basetool.InvokeOption
}
```

eino `tool.Option` slot 的内部载体，未导出强制调用方走 `WithInvokeOpts`。

### `draftConfigChangeGateArgs` 结构体（未导出）

`draft_config_change` 工具入参的部分反序列化结构，用于 metric catalog 预检。

## 4. 关键函数与流程

### `WrapBaseTool(t) einotool.InvokableTool`

单工具适配，**memo=nil**（不参与缓存/预算/预检），仅做接口转换。测试和非 graph 调用方使用。

### `WrapBaseTools(tools) []einotool.BaseTool`

批量适配，**共享一个 memo**。生产路径，`BuildReActGraph` 调用此函数。nil 入口跳过。

### `WithInvokeOpts(opts...) einotool.Option`

把 `basetool.InvokeOption`（如 `WithUserID`/`WithTenant`）通过 eino `tool.Option` slot 透传到 ToolsNode。典型用法：

```go
runnable.Invoke(ctx, in,
    compose.WithToolsNodeOption(
        compose.WithToolOption(graph.WithInvokeOpts(
            basetool.WithUserID(uid),
            basetool.WithTenant(tenantID),
        ))))
```

### `Info(ctx) (*schema.ToolInfo, error)`

1. 调用 `inner.Info(ctx)` 拿 ongrid `ToolInfo`
2. 把 `WhenToUse` 字段以 "When to use: ..." 形式追加到 `Desc` 末尾
3. 把 `info.Parameters`（raw JSON）反序列化为 `*jsonschema.Schema`，包装成 `schema.NewParamsOneOfByJSONSchema`
4. JSON Schema 解析失败 → 返回错误，graph 编译期拒绝（防止运行时崩）

### `InvokableRun(ctx, argumentsInJSON, opts...) (string, error)`

核心调度逻辑，6 步：

```go
func (a *einoToolAdapter) InvokableRun(ctx, argumentsInJSON, opts...) (string, error) {
    // 1. nil 防御
    // 2. memo 命中（read 工具 + 同 args）→ 返回缓存
    // 3. 预算上限触发 → 返回 toolBudgetExceeded(name, n)
    // 4. draft_config_change 预检 → 返回 metricCatalogRequiredResult()
    // 5. 调用 inner.InvokableRun(ctx, args, resolved.opts...)
    // 6. 失败 → 计数 + 转换为 JSON envelope（不返回 error）
    //    成功 → 计数 + putLast + 写缓存
}
```

**关键设计：tool 错误转换为 JSON envelope，不返回 Go error**。eino `ToolsNode` 把 Go-level error 视作 graph-fatal（终止整个 invoke + SSE stream），但 ongrid 不变式是"tool 失败是 LLM 可恢复的事实"——LLM 应该看到错误文本作为 tool result 决定重试/换路/问用户，而不是对话被中止。

错误转换示例：

```go
envelope, _ := json.Marshal(map[string]any{
    "error":  msg,    // 截断到 2048 字符
    "status": "failed",
})
return string(envelope), nil
```

### `maxCallsForTool(name) int`

```go
switch name {
case "draft_config_change":
    return 1  // 一个用户轮次只能产生一个 confirmable config_draft
default:
    return maxToolCallsPerRun  // 30
}
```

`draft_config_change` 限制为 1 次——只有 confirmable draft（`kind==config_draft` + 非空 `draft_hash`）才占用预算，`config_validation_failed` 仍可重试（让模型在同一轮修复结构化配置错误）。

### `draftMetricCatalogPreflight(argumentsInJSON) (string, bool)`

`draft_config_change` 调用前的 metric catalog 预检：

1. 仅当工具是 `draft_config_change` 时检查
2. `draftConfigChangeNeedsMetricCatalog(args)` 判断规则 kind 是否为 metric 类
3. 若需要但 `memo.lastResult("list_metric_catalog")` 不存在 → 返回 `metric_catalog_required` 阻断结果

### `toolBudgetExceeded(name, n) string`

合成 `call_budget_exceeded` JSON：

```go
{
  "status": "call_budget_exceeded",
  "tool": "<name>",
  "calls": <n>,
  "scope": "current_user_turn",
  "final_answer_required": true,
  "instruction": "TERMINAL TOOL BUDGET RESULT. ..."
}
```

`query_promql` 有专门的 instruction 文案，引导模型使用 `sum/topk` + `by(device_id, mountpoint, fstype)` 聚合。

## 5. 依赖关系

### 上游
- `graph/react.go::BuildReActGraph` 通过 `WrapBaseTools` 把工具注入 eino `react.AgentConfig.ToolsConfig.Tools`
- chatruntime（NEXT PR）通过 `WithInvokeOpts` 注入 tenant/user/device id

### 下游
- `basetool.BaseTool`（被适配方）
- `einotool.InvokableTool`/`BaseTool`/`Option`（适配目标）
- `jsonschema.Schema`（eino-contrib，JSON Schema 解析）

## 6. 并发与资源管理

### `toolMemo` 锁粒度

`sync.Mutex` 保护三个 map（`m`/`counts`/`last`）。所有读写都通过 `get`/`put`/`count`/`bump`/`putLast`/`lastResult` 方法，**锁范围最小化**（仅 map 操作，不包含 `inner.InvokableRun` 调用）。

### `infoOnce` 惰性解析

```go
a.infoOnce.Do(func() {
    if info, err := a.inner.Info(ctx); err == nil && info != nil {
        a.cacheName = info.Name
        a.cacheable = info.Class == "read"
    }
})
```

`Info()` 在 `resolveInfo` 中只解析一次，后续 `InvokableRun` 复用缓存值。`sync.Once` 保证并发安全。

### per-run 生命周期

`WrapBaseTools` 每次调用创建一个新 memo，scope 自然限定到一次 graph 运行（graph per-request 重建）。无需显式清理，GC 自动回收。

## 7. 设计模式与亮点

### 适配器模式（Adapter）

`einoToolAdapter` 把 `basetool.BaseTool` 接口翻译成 eino `InvokableTool` 接口。`basetool` 故意 mirror-shaped 对齐 eino，所以本适配器很薄：`Info` 是字段拷贝 + JSON Schema 反序列化，`InvokableRun` 是参数透传。

### 装饰器叠加

memo / budget / preflight 三个机制不是分散在三层装饰器，而是**内联在 `InvokableRun` 单方法中**。这避免了多层装饰器的调用栈开销和调试困难，但牺牲了可组合性——若未来要单独关闭某一机制，需要改代码而不是改装配顺序。

### 错误降级：tool error → JSON envelope

```go
out, err := a.inner.InvokableRun(ctx, argumentsInJSON, resolved.opts...)
if err != nil {
    // ...
    envelope, _ := json.Marshal(map[string]any{"error": msg, "status": "failed"})
    return string(envelope), nil  // 不返回 error
}
```

这是与 eino 默认行为的关键分歧：eino `ToolsNode` 把 Go error 视作 graph-fatal，但 ongrid 把 tool 失败视作"LLM 可恢复的事实"。错误文本被截断到 2048 字符（防止 stack trace 撑爆上下文）。

### `draft_config_change` 预算策略不对称

`countFailedToolCall("draft_config_change") == false`：失败不计入预算，让模型可重试修复结构化配置错误。
`countSuccessfulToolCall("draft_config_change", result)`：只有 `kind==config_draft` + 非空 `draft_hash` 才算成功并占用预算。

→ 模型可以在同一用户轮次内多次 `config_validation_failed` 修复，但只能产出 1 个 confirmable draft。

### metric catalog 预检的"先决条件"模式

`draftMetricCatalogPreflight` 实现了一种"先决条件工具"模式：metric 类 `draft_config_change` 必须先调用 `list_metric_catalog`。如果模型试图跳过这一步直接 draft，会被合成 `metric_catalog_required` 阻断结果引导回去。

## 8. 注意事项

### `WrapBaseTool`（单工具）vs `WrapBaseTools`（批量）的 memo 差异

- `WrapBaseTool`：memo=nil，**不参与**缓存/预算/预检。仅做接口转换。测试和非 graph 调用方使用。
- `WrapBaseTools`：创建共享 memo，**全部启用**。生产路径。

→ 在 graph 路径必须用 `WrapBaseTools`，否则预算停止机制失效。

### JSON Schema 解析失败的编译期拒绝

`Info()` 中 JSON Schema 解析失败返回 error，`react.NewAgent` 在 build 时调用 `Info` 会因此失败，整个 graph 编译拒绝。这是好事——防止运行时 tool schema 损坏导致 LLM 拿到错误工具定义。

### 错误截断阈值 2048 字符

`const cap = 2048` 是硬编码常量。若 LLM 错误信息含完整 stack trace 可能被截断，但这是有意为之——防止错误文本撑爆上下文窗口。

### `maxToolCallsPerRun=30` 是经验值

注释明确："Generous enough that normal multi-step investigation isn't clipped." 30 次调用上限对正常多步调查够用，但极端复杂场景可能被截断。`toolBudgetExceeded` 合成结果会引导模型基于已收集数据作答。

### `WithInvokeOpts` 必须配合 `compose.WithToolOption` 使用

直接传给 `InvokableRun` 不会生效——必须通过 `compose.WithToolsNodeOption(compose.WithToolOption(...))` 在 `Invoke` 调用时传入，eino 才会把它分发到每个 tool 的 `opts...` 参数。这是 eino option 系统的约定。

### `query_promql` 的特殊 instruction

`toolBudgetExceeded` 对 `query_promql` 给了专门文案，引导模型用 `sum/topk` + `by(device_id, mountpoint, fstest)` 聚合——这针对的是模型"一个 device/metric/mountpoint 一次 PromQL 调用"的反模式。
