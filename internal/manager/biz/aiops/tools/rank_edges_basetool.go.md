# rank_edges_basetool.go

## 1. 概述

本文件实现 `rank_edges` 工具的 BaseTool 形态。镜像 `executeRankEdges`（见 `rank_edges.go`）。注释明示 "Mirrors executeRankEdges in rank_edges.go"。

WhenToUse 明示：top/bottom-N pivot for closed-set host metrics（cpu/mem/disk/load/composite）。NOT for outlier detection（用 find_outlier_edges）/ general PromQL ranking on free-form metrics（用 query_promql with topk/bottomk）/ single host stats（用 get_host_load）。

## 2. 包信息

- **包名**：`tools`
- **路径**：`internal/manager/biz/aiops/tools/rank_edges_basetool.go`
- **导入**：
  - `basetool`
  - `edgebiz`（同闭包路径，用于 edge name 装饰）
  - 额外 `log/slog`
- **Class**：`read`

## 3. 关键类型与接口

### `RankEdgesTool`

```go
type RankEdgesTool struct {
    promQuery PromQuerier
    edges     *edgebiz.Usecase
    log       *slog.Logger
}
```

注意：BaseTool 形态直接持有 `*edgebiz.Usecase` 具体类型（与 `QueryEdgesTool` 同模式），保证与闭包路径 `r.edges` 类型一致。

`RankEdgesArgs` / `RankEdgeRow` / `RankEdgesSchema` / `RankEdgesDescription` / `ToolNameRankEdges` / `rankEdgesCallTimeout` / `rankMetricExpr` / `decodeRankSeries` / `numericLabel` / `lastSampleValue` / `promSampleFloat` 均复用 `rank_edges.go` 的定义。

## 4. 关键函数与流程

### `NewRankEdgesTool(promQuery, edges, log)`

`log == nil` → `slog.Default()`。

### `rankEdgesWhenToUse`（常量）

英文 LLM-facing 文案，反 guard 明确：

- 用途：TOP-N 或 BOTTOM-N hosts ranked by closed-set metric（cpu/mem/disk/load/composite）。示例："帮我找出最忙的 5 台机器"
- NOT for：outlier detection / who's anomalously deviating（用 find_outlier_edges）
- NOT for：general PromQL ranking on free-form metrics（用 query_promql with topk/bottomk）
- NOT for：single host's stats（用 get_host_load）

### `Info(_ context.Context)`

返回 `ToolInfo`：Name=`rank_edges`，Class=`read`，含 WhenToUse。

### `InvokableRun(ctx, argsJSON, _ ...)`

主流程与 `executeRankEdges` 完全镜像：

1. 校验 `promQuery` / `edges` 非 nil。
2. Unmarshal `RankEdgesArgs`。
3. `By == ""` → error。`rankMetricExpr(By)` 失败 → error。
4. `Limit ≤ 0 → 5`，`> 50 → 50`。
5. `Direction`：空/"top" → topk，"bottom" → bottomk，其他 → error。
6. 构造 `expr = fmt.Sprintf("%s(%d, %s)", op, limit, base)`。
7. `end = now`，`start = end.Add(-5min)`，`step = 30s`。
8. `context.WithTimeout(ctx, 30s)`，调 `promQuery.QueryRange`。
9. `decodeRankSeries(res, label)` 解析响应。
10. `edges.List(callCtx, ListFilter{Limit: 500})` 装饰 edge name（best-effort，err 时跳过）。
11. Marshal `{results, metric, by, direction: op}` 返回。

## 5. 依赖关系

| 方向 | 依赖 | 说明 |
|------|------|------|
| 上游 | `basetool.BaseTool` / `ToolInfo` | 实现 BaseTool 接口 |
| 下游 | `PromQuerier.QueryRange` | 数据源 |
| 下游 | `*edgebiz.Usecase.List` | 装饰 edge name |
| 共享 | `rank_edges.go` 中的所有类型 / 常量 / helper（`rankMetricExpr` / `decodeRankSeries` 等） | 避免重复定义 |

## 6. 并发与资源管理

- `context.WithTimeout(ctx, 30s)` per call。
- `Limit` cap 50。
- `edges.List(Limit: 500)` 一次拉宽列表构建 map。
- 无共享状态，并发安全。

## 7. 设计模式与亮点

- **镜像承诺**：注释明示 "Mirrors executeRankEdges"，与闭包路径语义等价。
- **共享所有 helper**：`rankMetricExpr` / `decodeRankSeries` / `numericLabel` / `lastSampleValue` / `promSampleFloat` 都在 `rank_edges.go` 声明，BaseTool 路径直接复用——这是相对健康的共享模式，drift 风险低于完全独立实现。
- **WhenToUse 三重反 guard**：明确三个 NOT for（outlier / free-form PromQL / single host），引导 LLM 选对排名工具。
- **直接持有 `*edgebiz.Usecase`**：与 `QueryEdgesTool` 同模式，保证与闭包路径 `r.edges` 类型对齐。

## 8. 注意事项

- **drift 风险较低**：与 `rank_edges.go` 共享所有 helper，业务逻辑（PromQL 构造、series 解码、edge name 装饰）都在共享 helper 里，BaseTool 路径只是 wrapper——这种共享模式 drift 风险低于 query_logql/query_promql 等完全独立实现的工具。
- **30s 超时**：与闭包路径一致。
- **`direction` 字段响应里是 `op`**：与闭包路径同样的语义不对称（响应 `direction` 字段返回 `topk`/`bottomk` 而非 `top`/`bottom`）。
- **未走 batch refactor（N+15）**：这个工具本身就是 fleet-wide rank（一次返回多 device），不需要 `device_ids[]` batch 协议。
- **`_ ...basetool.InvokeOption` 忽略 opts**：同其他 query 工具，不需要 tenant_bind / locale 等 ctx value。
