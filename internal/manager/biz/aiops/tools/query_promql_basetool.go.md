# query_promql_basetool.go

## 1. 概述

本文件实现 `query_promql` 工具的 BaseTool 形态。**是 PR-3 试点 BaseTool 模式的第一个工具**——注释明示 "PR-3 of (this PR) migrates only this one tool to validate the pattern. The closure path stays so existing wiring + tests are unaffected; later PRs migrate the remaining 13 tools"。

BaseTool 形态把 `promQuery` 持有在 struct 上（而非闭包捕获），是"改进点 #1：每个 tool 是接口对象 — struct 持依赖"的具体实现。两路径并行运行验证模式可行性，后续 PR 迁移其余工具。

## 2. 包信息

- **包名**：`tools`
- **路径**：`internal/manager\biz/aiops/tools/query_promql_basetool.go`
- **导入**：
  - `basetool`
  - 额外 `log/slog`
  - 注意：**不**直接 import `promquery`——复用 `query_promql.go` 声明的 `PromQuerier` 接口
- **Class**：`read`

## 3. 关键类型与接口

### `QueryPromQLTool`

```go
type QueryPromQLTool struct {
    promQuery PromQuerier  // 复用 query_promql.go 声明的接口
    log       *slog.Logger
}
```

`PromQuerier` / `QueryPromQLArgs` / `QueryPromQLSchema` / `QueryPromQLDescription` / `ToolNameQueryPromQL` / `queryPromqlCallTimeout` / `maxQueryPromQLLookbackSeconds` / `stepFor` 均复用 `query_promql.go` 的定义。

## 4. 关键函数与流程

### `NewQueryPromQLTool(p, log)`

`log == nil` → `slog.Default()`。

### `queryPromQLWhenToUse`（常量）

英文 LLM-facing 文案，路由 hint：

- 用途：metric values、time-series trends、per-edge resource usage、任何 Prom range query
- NOT for：log content（用 query_logql）/ filesystem state（用 host-level tools）
- **fleet nudge**：multi-device / multi-mountpoint 问题，prefer 一个 PromQL with `by(device_id, ...)` 或 topk/ranking，over repeated per-device queries
- **优先级**：问题跨多 host 或要 derivatives/aggregates 时，prefer query_promql over 窄的 get_host_load / get_process_list

### `Info(_ context.Context)`

返回 `ToolInfo`：Name=`query_promql`，Class=`read`，含 WhenToUse。注释明示 Info is pure（no I/O），Class=read 让它豁免 destructive-action gating。

### `InvokableRun(ctx, argsJSON, _ ...)`

注释明示 "input/output shape matches the closure executor exactly — they consult the same QueryPromQLArgs / stepFor / queryPromqlCallTimeout — so the two paths return identical bytes for equivalent inputs"。

主流程与 `executeQueryPromQL` 完全镜像：

1. 校验 `promQuery == nil` → error。
2. Unmarshal `QueryPromQLArgs`。
3. `Expr == ""` → error。
4. `LookbackSeconds ≤ 0 → 300`，`> maxQueryPromQLLookbackSeconds → 7d`。
5. `end = now`，`start = end.Add(-lookback)`，`step = stepFor(lookback)`。
6. `context.WithTimeout(ctx, 30s)`，调 `promQuery.QueryRange`。
7. Marshal `*InstantResult` 返回。

注释明示 opts 被接受但忽略："query_promql is tenant-agnostic and not edge-scoped (DeviceID stays nil in the audit row). The decorator chain still consumes them upstream (tenant_bind for ratelimit/audit keying)"——opts 是给装饰器链用的，工具本身不需要。

## 5. 依赖关系

| 方向 | 依赖 | 说明 |
|------|------|------|
| 上游 | `basetool.BaseTool` / `ToolInfo` | 实现 BaseTool 接口 |
| 下游 | `PromQuerier.QueryRange`（复用自 `query_promql.go`） | 数据源 |
| 共享 | `query_promql.go` 中的所有类型 / 常量 / helper | 避免重复定义 |

## 6. 并发与资源管理

- `context.WithTimeout(ctx, 30s)` per call。
- 注释明示超时行为："Mirror the closure executor's per-call timeout so behaviour is identical when this tool is wired without the decorators.Timeout wrapper. When the wrapper IS present, whichever ctx deadline fires first wins — context.WithTimeout on a parent that already has a closer deadline keeps the closer one"——双 timeout 取更紧的，符合 context 语义。
- `LookbackSeconds` cap 7d + `stepFor` 自动 step。

## 7. 设计模式与亮点

- **PR-3 试点**：这是 BaseTool 模式的第一个工具，用于验证"struct 持依赖"模式可行。注释保留 PR-3 历史信息，便于追溯模式演进。
- **byte-for-byte 镜像承诺**：注释明示 "two paths return identical bytes for equivalent inputs"。所有逻辑（LookbackSeconds 修正、stepFor、QueryRange 调用、Marshal）都与闭包路径一致。
- **opts 显式忽略 + 注释解释**：`_ ...basetool.InvokeOption` 不用，但注释解释为何仍接受——装饰器链上游需要 opts（tenant_bind 用于 ratelimit/audit keying），工具本身不需要。这是接口契约的清晰表达。
- **WhenToUse fleet nudge**：英文文案明示 "prefer one PromQL call with by(device_id, ...) or topk/ranking over repeated per-device queries"——这是 ongrid agent 反 N+1 query 的核心 prompt 工程。
- **WhenToUse 优先级引导**：明示 "Prefer query_promql over the narrower get_host_load / get_process_list when the question spans more than one host or asks for derivatives / aggregates"——引导 LLM 在合适场景选泛化工具。
- **复用所有共享定义**：不在 BaseTool 文件重复声明任何类型/常量/helper，全部 import 自 `query_promql.go`，避免 drift。

## 8. 注意事项

- **drift 风险**：作为 PR-3 试点，与 `query_promql.go` 是两份并行实现，任何一边改逻辑必须同步另一边，否则 "identical bytes" 承诺破裂。这是试点模式的已知代价。
- **opts 被忽略**：未来如果 query_promql 需要 tenant 隔离或 edge scoping（如多租户 Prom），需要改用 opts——目前是 tenant-agnostic。
- **试点历史信息保留**：注释里的 "PR-3"、"改进点 #1"、"later PRs migrate the remaining 13 tools" 等历史信息保留下来，便于未来清理时追溯——但也意味着文件头比较啰嗦。
- **30s 超时**：与闭包路径一致，双 timeout 取更紧的（外层 decorators.Timeout + 内层 30s）。
- **无 batch 协议**：与 `get_edge_summary_basetool.go` 不同，这个工具本身就是 vectorized PromQL（一次查多 device），不需要 `device_ids[]` batch 协议——`by(device_id, ...)` 是 PromQL 层面的 vectorization。
