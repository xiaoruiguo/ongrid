# query_traceql_basetool.go

## 1. 概述

本文件实现 `query_traceql` 工具的 BaseTool 形态。镜像闭包路径（见 `query_traceql.go`）。注释明示 "Mirrors the closure executor in query_traceql.go"。

WhenToUse 明示：用 trace 查询（span chains across services、latency outliers、specific trace IDs、"which call took 5 seconds"）。NOT for log lines（用 query_logql）/ metric trends（用 query_promql）/ live host stats（用 get_host_load）。并强调至少一个 filter 必填——Tempo unfiltered search 太贵。

## 2. 包信息

- **包名**：`tools`
- **路径**：`internal\manager\biz\aiops\tools\query_traceql_basetool.go`
- **导入**：
  - `basetool`
  - `tracequery`（同闭包路径）
  - 额外 `log/slog`
- **Class**：`read`

## 3. 关键类型与接口

### `QueryTraceQLTool`

```go
type QueryTraceQLTool struct {
    traceQuery TraceQuerier  // 复用 query_traceql.go 声明的接口
    log        *slog.Logger
}
```

`TraceQuerier` / `QueryTraceQLArgs` / `QueryTraceQLSchema` / `QueryTraceQLDescription` / `ToolNameQueryTraceQL` / `queryTraceqlCallTimeout` 均复用 `query_traceql.go` 的定义。

## 4. 关键函数与流程

### `NewQueryTraceQLTool(tq, log)`

`log == nil` → `slog.Default()`。

### `queryTraceQLWhenToUse`（常量）

英文 LLM-facing 文案，反 guard 明确：

- 用途：TRACES——span chains across services、latency outliers、specific trace IDs、"which call took 5 seconds"
- NOT for：log lines（用 query_logql）/ metric trends（用 query_promql）/ live host stats（用 get_host_load）
- **强制 filter 提示**：至少一个 filter（query / service / operation / duration）必填——Tempo unfiltered search 太贵

### `Info(_ context.Context)`

返回 `ToolInfo`：Name=`query_traceql`，Class=`read`，含 WhenToUse。

### `InvokableRun(ctx, argsJSON, _ ...)`

主流程与 `executeQueryTraceQL` 完全镜像：

1. 校验 `traceQuery == nil` → error。
2. Unmarshal `QueryTraceQLArgs`。
3. **强制过滤校验**：5 字段全空 → error。
4. 时间解析（RFC3339 only）：
   - `end = now`，`start = end.Add(-time.Hour)`
   - `in.End` 非空 → `time.Parse(RFC3339)` 覆盖 end
   - `in.Start` 非空 → `time.Parse(RFC3339)` 覆盖 start
   - else if `in.End != ""` → `start = end.Add(-time.Hour)`
5. `Limit ≤ 0 → 50`。
6. Duration 解析（`time.ParseDuration`）。
7. tags 构造（`service.name` / `name`），空时 `tags = nil`。
8. `context.WithTimeout(ctx, 30s)`，调 `traceQuery.SearchTraces`。
9. Marshal `*SearchResult` 原样返回。

## 5. 依赖关系

| 方向 | 依赖 | 说明 |
|------|------|------|
| 上游 | `basetool.BaseTool` / `ToolInfo` | 实现 BaseTool 接口 |
| 下游 | `TraceQuerier.SearchTraces`（复用自 `query_traceql.go`） | 数据源 |
| 共享 | `query_traceql.go` 中的所有类型 / 常量 | 避免重复定义 |

## 6. 并发与资源管理

- `context.WithTimeout(ctx, 30s)` per call。
- `Limit` cap 1000。
- 无共享状态，并发安全。

## 7. 设计模式与亮点

- **镜像承诺**：注释明示 "Mirrors the closure executor"，与闭包路径语义等价。共享所有类型/常量避免 schema drift。
- **WhenToUse 三重反 guard + 强制 filter 提示**：明确三个 NOT for（log/metric/host stats），并在文案里强调 filter 必填——LLM 看到 WhenToUse 就知道既选对工具又必须带 filter。
- **复用 `TraceQuerier` 接口**：不在 BaseTool 文件重复声明，直接 import 自 `query_traceql.go`，避免接口 drift。
- **零行为差**：所有校验（强制过滤）、时间解析、duration 解析、tags 构造、SearchTraces 调用都与闭包路径一致。

## 8. 注意事项

- **drift 风险**：与 `query_traceql.go` 是两份并行实现，任何一边改逻辑必须同步另一边。
- **未走 batch refactor（N+15）**：与 `get_edge_summary_basetool.go` 不同，这个工具本身就是 search 语义（一次返回多 trace），不需要 `trace_ids[]` batch 协议。
- **30s 超时**：与闭包路径一致，Tempo 大查询可能不够。
- **强制过滤继承**：和闭包路径同样的 5 字段全空校验，同样的 error 文案。
- **`_ ...basetool.InvokeOption` 忽略 opts**：同其他 query 工具，不需要 tenant_bind / locale 等 ctx value。
- **RFC3339 only**：与闭包路径同样不支持 `now-1h` 相对时间，LLM 要算时间戳——这与 `query_logql`（支持 now-1h）不对称，是历史遗留。
