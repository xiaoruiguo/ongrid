# toolbag.go

## 1. 概述

本文件实现 `ToolBag` —— AIOps 工具体系中的"延迟工具加载包装器"（deferred-tool-loading wrapper）。核心动机是 LLM 的 prompt budget：随着 marketplace 落地、工具数量突破 ~30 个，每次 `ChatModel` 调用累积的 JSON Schema 体积会膨胀（每个工具的 parameters 块可能数百 token）。

Anthropic 风格的 "ToolSearch" 延迟加载方案：只有一小部分 always-useful 的工具（core 层）对外暴露完整 schema，其余工具（specialty 层）只暴露 `(name, description, when_to_use)`，LLM 必须先调用 `ToolSearch` 拉取真实 schema 才能调用。

本文件还实现：
- **two-tier 分类**：通过 `tierByName` map 按 name 硬编码分类（v1 设计：把 policy 排除在 `ToolInfo` 之外，保持 `basetool` 接口纯净）
- **threshold gating**：当 `len(all) > threshold`（default 30，env `ONGRID_TOOLBAG_DEFERRAL_THRESHOLD`）时 bag 分区
- **`redactedTool` wrapper**：包装 specialty 工具使其 `Info()` 返回空 schema，但 `InvokableRun` 透传

## 2. 包信息

- **包名**：`tools`（`internal/manager/biz/aiops/tools`）
- **文件**：`toolbag.go`
- **行数**：约 367 行
- **导入**：`context`、`encoding/json`、`fmt`，以及 `basetool` 子包
- **导出符号**：`IsCoreToolName`、`CoreToolNames`、`ToolBag`、`NewToolBag`

## 3. 关键类型与接口

### `tierByName` map（包级变量）

```go
var tierByName = map[string]string{
    "get_host_load":           "core",
    // ... 26 个 core 工具
    "rank_edges":              "specialty",
    // ... 10 个 specialty 工具
}
```

按工具名硬编码分类。**`ToolSearch` 本身故意不在此 map 中**——当 deferral 启用时，`BuildBaseTools` 通过 `WithExtra` 把 ToolSearch 强制以 full schema 注册（LLM 需要 ToolSearch 的 schema 才能调用 ToolSearch，这是 chicken-and-egg）。低于 threshold 时 bag 不分区，ToolSearch 与其他工具一起平铺在同一 slice 中。

未知名默认归为 `specialty`（更安全——避免给未审慎评估的工具发"幽灵 full-schema"）。

### `ToolBag`（结构体）

```go
type ToolBag struct {
    core      []basetool.BaseTool
    deferred  []basetool.BaseTool
    extras    []basetool.BaseTool
    threshold int
    deferring bool
}
```

- `core`：always full schema
- `deferred`：specialty 层，被 `redactedTool` 包装后呈现给 LLM
- `extras`：`WithExtra` slot，典型为 `ToolSearch` 本身，无条件附加到 core
- `threshold`：高水位线
- `deferring`：是否进入 deferral 模式

`core + deferred` 等于工具全集。`extras` 在 deferral off 时为空（避免重复列出），在 deferral on 时附加到 core 之后。

### `redactedTool`（未导出 wrapper）

```go
type redactedTool struct {
    inner basetool.BaseTool
}
```

包装一个 `BaseTool`，使 `Info()` 返回空 JSON schema。`Description` / `WhenToUse` / `Name` / `Class` 仍照常对外——LLM 看到足够信息判断是否值得 pull schema，但不足以在不调用 `ToolSearch` 的情况下直接调用。**未导出**：包外不应直接构造（应通过 `NewToolBag`）。

## 4. 关键函数与流程

### `toolTier(t)`

按 `tierByName[info.Name]` 分类。`Info()` 出错的工具归为 `specialty`——"无法产出自己 metadata 的工具不应获得 core 特权"。nil 工具归 `specialty`。

### `IsCoreToolName(name)` / `CoreToolNames(all)`

导出的 routing helpers：
- `IsCoreToolName`：纯查 `tierByName` map
- `CoreToolNames`：从 `[]BaseTool` 提取所有 core 工具名（去重、保序）。**即便 bag 低于 threshold 也按注册 tier 分类**——routing policy 不应随 prompt-budget 设置变化。Callers 应使用此 registry-owned table 而非维护自己的 stale copy。

### `NewToolBag(all, threshold)`

构造与分区决策：

1. `len(all) <= threshold`：所有工具留在 `core`，`deferred = nil`，`deferring = false`。`SchemasForLLM` 行为与"return all"字节一致（PR-7 行为）
2. 否则进入 deferral，按 `toolTier` 分桶

`threshold <= 0` 视为"always defer"（适合测试）。生产环境 `main.go` 通过 `envIntDefault` 传值，0 不可达。

注意："等于 threshold"仍视为 flat——threshold 读作 high-water mark（"up to N tools is fine"），不是严格 ceiling。

### `SchemasForLLM()`

graph node 喂给 `ChatModel` 的 BaseTool slice，三种行为：

- **deferral off**：`core + extras`（full schema，PR-7 parity）。extras 中可能仍持有 ToolSearch（`BuildBaseTools` 无条件注册），deferral off 时 ToolSearch 返回 full schema 是合法 no-op affordance
- **deferral on**：`core + extras + 每个 deferred 工具包装为 redactedTool`（空 schema）
- **nil bag**：返回 nil

### `AllTools()` / `DeferredTools()`

- `AllTools`：返回 `core + extras + deferred` 全部 UNREDACTED 形式。`ToolSearch` 用此实现 `select:...` 精确名匹配（即便工具已经是 core 也能命中）
- `DeferredTools`：仅返回 redacted-by-default 层。`ToolSearch` 用此偏向 keyword 查询，避免 surface 已经 full-loaded 的工具

### `IsDeferring()` / `Threshold()`

Logging-only introspection。运维希望从 manager log 中看到 LLM 是否在走 ToolSearch 路径。

### `WithExtra(t)`

附加工具到 always-loaded slot。`BuildBaseTools` 用此在构造后注册 `ToolSearch`（chicken-and-egg：ToolSearch 需要 bag 作为 `DeferredToolBagProvider`，bag 必须先于 ToolSearch 构造）。返回同一 bag 支持链式调用。

### `Append(t)`

构造后向已分区的 bag 注入工具。`AppendHostFilesTools` 用此把 host_files trio 正确分桶（specialty）而不重建 bag。

**关键约束：不重新评估 deferral toggle**——一个 8 工具的 bag（低于 threshold）append 3 个后变成 11 工具不会突然进入 deferral。deferral 决策在 `NewToolBag` 时一次性做出。`main.go` 在 `BuildBaseTools` 之后立即链式调用 `AppendHostFilesTools`，正确切换 deferral 的方式是调高 env 或等 marketplace 工具直接进入 `NewToolBag` 的 input slice。

deferral off 时直接 append 到 `core`；deferral on 时按 `toolTier` 分桶。

### `redactedTool.Info(ctx)`

返回 inner tool 的 metadata，但 `Parameters` 替换为空-schema stub，stub 描述同时是 hint 字符串：

```go
hint := fmt.Sprintf(
    `{"type":"object","properties":{},"description":"Schema redacted; call ToolSearch with query='select:%s' to load the full schema before invoking."}`,
    info.Name,
)
```

保证即便没读 system prompt 中 deferral notice 的模型也能在 schema 位置看到提示。

### `redactedTool.InvokableRun(ctx, argsJSON, opts...)`

透传给 inner tool。这是 LLM 未先 fetch schema 就调用工具的 fallback——inner tool 的 argsJSON unmarshal 会以清晰错误失败，由 agent loop 分类为 tool failure。**不短路自家错误**：defend malformed args 是 inner tool 的职责，未来的 schema-cache 层可能允许 call through（如果 LLM 恰好知道参数 shape）。

## 5. 依赖关系

- **`basetool`**：`BaseTool`、`ToolInfo`
- **`registry_basetool.go`**：`BuildBaseTools` 是 `NewToolBag` + `WithExtra(ToolSearch)` 的编排点
- **`tool_search_tool.go`**：`ToolSearchTool` 实现 `DeferredToolBagProvider` 接口的消费方
- **`host_files_register.go`**：通过 `Append` 注入 host_files trio
- **`registry_basetool.go`**：`envIntDefault` 读取 `ONGRID_TOOLBAG_DEFERRAL_THRESHOLD` env

## 6. 并发与资源管理

- `ToolBag` 在 boot 期一次性构造，之后**只读**——无需锁
- `redactedTool` 仅持有 inner 指针，每次 `Info()` 合成 redacted ToolInfo——分配轻量（"allocation-light"）
- `SchemasForLLM` 每次返回新 slice（避免外部修改影响内部状态）
- 无 goroutine、无 IO、无 timeout

## 7. 设计模式与亮点

### Two-tier classification 保持在 package 私有 map
`tierByName` 不暴露在 `ToolInfo` 上——v1 设计选择：保持 `basetool.BaseTool` 接口纯净，policy 集中在本文件可一处审阅。

### Threshold as high-water mark
"等于 threshold" 仍 flat，让 threshold 读作"up to N is fine"。语义清晰，运维调参直觉。

### Chicken-and-egg 通过 WithExtra 解决
`ToolSearch` 需要 bag 作为 provider，bag 又需先存在。`WithExtra` 提供 post-construction 注入点，返回同一 bag 支持链式调用。`extras` 在 deferral off 时为空避免重复列出。

### Append 不重评估 toggle
deferral 决策一次性做出，避免 runtime 行为不可预测。文档明确指引：要切换 deferral 应调 env 或让工具直接进入 `NewToolBag` input。

### Defensive `Parameters` 替换而非清空
`redactedTool.Info()` 用 hint 字符串作为 stub description，LLM 即便没读 system prompt 也能在 schema 位置看到引导。这是 defense in depth：不依赖 LLM "应该知道"的 deferral 约定。

### InvokableRun 不短路
不在 wrapper 层加错误，让 inner tool 自己 defend malformed args。为未来 schema-cache 留出 call-through 可能性。

### 导出 routing helpers
`IsCoreToolName` / `CoreToolNames` 让外部 routing 逻辑直接查 registry-owned table，避免 stale copy 在各处滋生。

## 8. 注意事项

- **`tierByName` 是 single source of truth**：新增工具时必须在此 map 中分类，否则会被默认归为 `specialty`。core/specialty 分类需要审慎评估（specialty 的 `when_to_use` 通常较大，deferral 收益明显）
- **ToolSearch 必须显式 `WithExtra`**：本文件不自动注册 ToolSearch，`BuildBaseTools` 必须在 `NewToolBag` 后链式调用 `WithExtra(NewToolSearchTool(...))`
- **`Append` 不切换 deferral**：post-construction 注入的工具不会触发重新评估。`main.go` 必须在 `BuildBaseTools` 后立即 `AppendHostFilesTools`，否则可能产生未分桶的 specialty 工具
- **`redactedTool` 未导出**：包外不应直接构造。要修改 deferral 行为应通过 `NewToolBag` threshold 或新增导出 API
- **空 schema 不等于阻止调用**：`InvokableRun` 透传，LLM 若已知参数 shape 仍可调用（inner tool 的 argsJSON 校验是最后防线）
- **deferral off 时 extras 仍存在**：`BuildBaseTools` 无条件注册 ToolSearch，deferral off 时 ToolSearch 在 `SchemasForLLM` 中仍会出现——这是合法的 no-op affordance，不是 bug
- **`toolTier` 调 `Info()` 可能失败**：失败归 `specialty`，但 boot 期频繁 `Info()` 失败说明工具实现有问题，应排查而非依赖此 fallback
- **26 core + 10 specialty = 36 工具**：当 marketplace 工具进入 `NewToolBag` input slice，total 会突破 threshold（30），deferral 自动启用——这是设计意图，无需手动切换
