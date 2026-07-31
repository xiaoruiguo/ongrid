# rank_edges.go

## 1. 概述

本文件实现 `rank_edges` 工具的闭包路径，按 closed-set host metric（cpu/mem/disk/load/composite）对 edge 排名，返回 top-N 或 bottom-N。用于"找最忙/最闲的 N 台机器"类问题。

**closed-set 设计**：`rankMetricExpr` 的 5 个 metric（cpu/mem/disk/load/composite）必须与 `manager/biz/alert.metricExprFor` 保持同步——LLM-driven rank 用与 alert threshold 相同的词汇表，避免 LLM 学两套 metric 定义。

**composite 语义**：cpu_pct + mem_pct + disk_used_pct 的无权重均值，wrapped with `avg by(edge_id)` 对齐不同 label shape。

## 2. 包信息

- **包名**：`tools`
- **路径**：`internal\manager\biz\aiops\tools\rank_edges.go`
- **导入**：
  - `edgebiz`（`internal/manager/biz/edge`）—— `ListFilter`（装饰 edge name）
- **Class**：`read`
- **超时**：`rankEdgesCallTimeout = 30 * time.Second`

注意：**不**直接 import `promquery`——通过 `r.promQuery`（`PromQuerier` 接口，声明在 `query_promql.go`）调用。

## 3. 关键类型与接口

### `RankEdgesArgs`

```go
type RankEdgesArgs struct {
    By        string `json:"by"`           // cpu/mem/disk/load/composite
    Limit     int    `json:"limit,omitempty"`     // 默认 5，cap 50
    Direction string `json:"direction,omitempty"` // top（默认）/bottom
}
```

### `RankEdgeRow`

```go
type RankEdgeRow struct {
    EdgeID   uint64  `json:"edge_id"`
    EdgeName string  `json:"edge_name"`   // best-effort，label 无 edge_id 时为空
    Value    float64 `json:"value"`
    Metric   string  `json:"metric"`      // cpu_pct/mem_pct/disk_used_pct/load1/composite_pct
}
```

### `promRangeSeries`（内部）

```go
type promRangeSeries struct {
    Metric map[string]string `json:"metric"`
    Values [][2]any          `json:"values"`
}
```

## 4. 关键函数与流程

### `rankMetricExpr(by) (expr, label, ok)`

返回 per-edge scalar 的 PromQL 片段 + metric label 名：

| by | expr | label |
|----|------|-------|
| cpu | `100 * (1 - avg by (edge_id) (rate(node_cpu_seconds_total{mode="idle"}[5m])))` | cpu_pct |
| mem | `100 * (1 - node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes)` | mem_pct |
| disk | `100 * (1 - node_filesystem_avail_bytes{mountpoint="/"} / node_filesystem_size_bytes{mountpoint="/"})` | disk_used_pct |
| load | `node_load1` | load1 |
| composite | `(avg by(edge_id)(cpu) + avg by(edge_id)(mem) + avg by(edge_id)(disk)) / 3` | composite_pct |

注释明示 closed-set 必须与 `alert.metricExprFor` 同步。

### `executeRankEdges(ctx, args) (ExecuteResult, error)`

1. 校验 `r.promQuery == nil` / `r.edges == nil` → error。
2. Unmarshal `RankEdgesArgs`。
3. `By == ""` → error。`rankMetricExpr(By)` 解析失败 → error。
4. `Limit ≤ 0 → 5`，`> 50 → 50`。
5. `Direction`：空/"top" → `topk`，"bottom" → `bottomk`，其他 → error。
6. 构造 `expr = fmt.Sprintf("%s(%d, %s)", op, limit, base)`，如 `topk(5, 100 * (1 - avg by (edge_id) (rate(...))))`。
7. `end = now`，`start = end.Add(-5min)`，`step = 30s`（5min 窗口，10 个 datapoint，取 last value）。
8. `context.WithTimeout(ctx, 30s)`，调 `r.promQuery.QueryRange(callCtx, expr, start, end, step)`。
9. `decodeRankSeries(res, label)` 解析 matrix/vector 响应为 `[]RankEdgeRow`。
10. **装饰 edge name**：`r.edges.List(callCtx, ListFilter{Limit: 500})` 拉宽列表，构建 `id→name` map，遍历 rows 填 `EdgeName`。注释："cheaper than per-row GetByID for typical fleets"。
11. Marshal `{results, metric, by, direction}` 返回。

### `decodeRankSeries(res, metricLabel) ([]RankEdgeRow, error)`

接受 `interface{}`（注释明示 satisfied by `*promquery.InstantResult`），通过 json re-encode/decode 解析——注释解释："rather than hard-binding to that concrete type here (which would create a dependency cycle issue if the package layout shifts), re-encode and re-decode through json. This costs one extra marshal; the payload is small (<= 50 rows)"。

按 `ResultType` 分派：

- `matrix`：解析 `[]promRangeSeries`，每 series 取 `lastSampleValue(Values)`，跳过无 numeric `edge_id` label 的 series。
- `vector`：解析 `[]{Metric, Value}`，取 `Value[1]`，同样跳过无 edge_id。
- 其他：返回 `nil, nil`（空结果）。

### `numericLabel(m, key) (uint64, bool)`

从 Prom metric map 拉 uint64 label，非数字或缺失返回 `ok=false`。

### `lastSampleValue(values) (float64, bool)`

取 matrix series 最后一个 sample 的 value。

### `promSampleFloat(v) (float64, bool)`

Prom sample value 可能是 string（matrix）或 float64（vector），switch 处理两种。

## 5. 依赖关系

| 方向 | 依赖 | 说明 |
|------|------|------|
| 上游 | `Registry`（闭包持有者） | `r.promQuery` / `r.edges` |
| 下游 | `PromQuerier.QueryRange`（复用自 `query_promql.go`） | 数据源 |
| 下游 | `edgebiz.Usecase.List` | 装饰 edge name |
| 共享 | `rankMetricExpr` 被 `find_outlier_edges.go` 复用 | z-score 查询基础 |

## 6. 并发与资源管理

- `context.WithTimeout(ctx, 30s)` per call。
- `Limit` cap 50，防止 topk 返回过多行。
- `edges.List(Limit: 500)` 一次拉宽列表，构建 map——典型 fleet（<500 edge）一次 DB 调用搞定。
- 无共享状态，并发安全。

## 7. 设计模式与亮点

- **closed-set 与 alert 同步**：`rankMetricExpr` 的 5 个 metric 必须与 `alert.metricExprFor` 同步——LLM-driven rank 用与 alert threshold 相同的词汇表，避免 LLM 学两套 metric 定义。这是 ongrid agent 的核心一致性设计。
- **composite 无权重均值**：cpu/mem/disk 三指标等权平均，wrapped with `avg by(edge_id)` 对齐不同 label shape（cpu 有 mode label 聚合掉，mem 无，disk 有 mountpoint filter）——注释详细解释为何要 `avg by(edge_id)` 二次聚合。
- **`topk`/`bottomk` PromQL 原生**：rank 在 Prom 侧完成，工具层只解码——避免拉全量 series 再在内存排序（O(N) DB → O(N log N) 排序 vs Prom 侧 O(N) heap）。
- **edge name best-effort 装饰**：`edges.List(Limit: 500)` 一次拉宽列表构建 map，比 per-row `GetByID` 便宜——典型 fleet 一次 DB 调用搞定。失败时（`err != nil`）静默跳过，`EdgeName` 留空，不影响 rank 结果。
- **`decodeRankSeries` 用 `interface{}` + json re-encode**：避免硬绑定 `*promquery.InstantResult` 具体类型，防止 package layout shift 时循环依赖——代价是多一次 marshal，但 payload 小（≤50 rows）可忽略。
- **matrix/vector 双解析**：Prom 可能返回 matrix（range query）或 vector（instant query），两种都处理——`topk` over range 通常返回 matrix，但防御性处理 vector。
- **跳过无 `edge_id` label 的 series**：`numericLabel(s.Metric, "edge_id")` 失败时 `continue`——无法映射回 Edge 行的 series 直接丢弃，不报错。

## 8. 注意事项

- **5min 窗口 + 30s step**：相对短，长趋势排名应该用 `query_promql` 自己写 PromQL；这个工具是"当前 snapshot 排名"。
- **`Limit` cap 50**：`topk(50, ...)` 在大 fleet 可能仍返回 50 行，LLM 上下文要注意。
- **`edges.List(Limit: 500)` 假设**：fleet >500 edge 时 name 装饰会漏掉部分 edge（map 不全）——`EdgeName` 留空，不影响 `EdgeID`。大 fleet 场景应改用 `GetByID` per-row 或提高 Limit。
- **closed-set 同步是软约束**：注释明示"must stay in sync with alert.metricExprFor"，但没编译期保证——alert 侧改 metric 定义时 rank_edges 不会自动同步，drift 风险。
- **`decodeRankSeries` 性能**：json re-encode/decode 在大 payload 时有成本，但 ≤50 rows 可忽略；如果未来 Limit 提高到 500+，需要重构为直接类型断言。
- **`composite` PromQL 复杂**：三段 `avg by(edge_id)(...)` 嵌套，Prom 解析成本高；大 fleet 可能慢，30s 超时要注意。
- **闭包路径与 BaseTool 路径并存**：见 `rank_edges_basetool.go`，两路径共享 `rankMetricExpr` / `decodeRankSeries` / `numericLabel` / `lastSampleValue` / `promSampleFloat` 等 helper（这些在闭包文件声明），BaseTool 路径复用——这是相对健康的共享模式，drift 风险低于完全独立实现。
- **`direction` 字段响应里是 `op`（topk/bottomk）**：响应 `direction` 字段返回的是 `op`（"topk"/"bottomk"）而非 `in.Direction`（"top"/"bottom"）——LLM 看到的是 PromQL 操作符，不是 args 原值，注意语义对齐。
