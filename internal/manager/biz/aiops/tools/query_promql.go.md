# query_promql.go

## 1. 概述

本文件实现 `query_promql` 工具的闭包路径，对集群 Prometheus 跑 PromQL range query。是 metric 信号的总入口——当 host-load / process-list 工具太窄时用它。

**fleet 优化 nudge**：描述明示"fleet 或 multi-device 问题，写一个 vectorized PromQL（`by(device_id, ...)` / regex selectors / topk），不要每个 device 或 metric 一次查询"。返回 Prom HTTP API 原始响应。

是 promql/logql/traceql 三 signal 对称设计的 metric 那一个。

## 2. 包信息

- **包名**：`tools`
- **路径**：`internal/manager/biz/aiops/tools/query_promql.go`
- **导入**：
  - `promquery`（`internal/pkg/promquery`）—— `InstantResult`
- **Class**：`read`
- **超时**：`queryPromqlCallTimeout = 30 * time.Second`

## 3. 关键类型与接口

### `QueryPromQLArgs`

```go
type QueryPromQLArgs struct {
    Expr            string `json:"expr"`            // PromQL 表达式
    LookbackSeconds int    `json:"lookback_seconds,omitempty"` // 默认 300(5min)，cap 604800(7d)
}
```

注意：args 极简，只有 expr + lookback。时间窗口用 `lookback_seconds` 而非 `start/end`——因为 LLM 倾向于"过去 N 秒"语义而非绝对时间戳。step 由 `stepFor` 自动算，LLM 不可控。

### `PromQuerier`（接口）

```go
type PromQuerier interface {
    QueryRange(ctx, expr, start, end, step) (*InstantResult, error)
    Query(ctx, expr, ts) (*InstantResult, error)  // instant 形式，correlate_incident.go 用
}
```

注释明示：这是 `r.promQuery` 的类型，`*promquery.Client` 满足。声明在这里让测试能 inject fake。`Query` 方法虽然不在 `query_promql` 用，但其他工具（correlate_incident、find_outlier_edges、metric_catalog、rank_edges）复用此接口。

## 4. 关键函数与流程

### `stepFor(lookbackSeconds) time.Duration`

step 自动选择，目标 ~30 datapoints per range：

| lookback | step |
|----------|------|
| ≤ 5min | 15s |
| ≤ 1h | 1m |
| ≤ 6h | 5m |
| ≤ 24h | 15m |
| ≤ 7d / else | 1h |

注释明示："model can override lookback but not step; that keeps the cost envelope predictable"——step 不可控是有意的，防止 LLM 写 `step=1s` 拉爆 Prom。

### `executeQueryPromQL(ctx, args) (ExecuteResult, error)`

1. 校验 `r.promQuery == nil` → error（防御性）。
2. Unmarshal `QueryPromQLArgs`。
3. `Expr == ""` → error。
4. `LookbackSeconds ≤ 0 → 300`，`> maxQueryPromQLLookbackSeconds(7d) → 7d`。
5. `end = time.Now()`，`start = end.Add(-lookback)`，`step = stepFor(lookback)`。
6. `context.WithTimeout(ctx, 30s)`，调 `r.promQuery.QueryRange(callCtx, expr, start, end, step)`。
7. Marshal `*InstantResult` 原样返回（Prom 响应直传）。

注释明示："EdgeID is intentionally left nil — query_promql is not bound to a specific edge"——这是 tenant-agnostic 工具，audit 行不绑定 device。

## 5. 依赖关系

| 方向 | 依赖 | 说明 |
|------|------|------|
| 上游 | `Registry`（闭包持有者） | `r.promQuery` |
| 下游 | `PromQuerier.QueryRange` | 数据源 |
| 共享 | `PromQuerier` 接口被多个工具复用 | correlate_incident / find_outlier_edges / metric_catalog / rank_edges |

## 6. 并发与资源管理

- `context.WithTimeout(ctx, 30s)` per call——Prom range query 可能慢（大窗口 + 高 cardinality）。
- `LookbackSeconds` cap 7d + `stepFor` 自动选 step，双重 bound，防止 Prom 返回海量 datapoints 撑爆 LLM 上下文。
- 无共享状态，并发安全。

## 7. 设计模式与亮点

- **`stepFor` 自动 step**：目标 ~30 datapoints，LLM 不可控——这是有意的 cost 控制。LLM 只能决定"看多久"，不能决定"多密"。
- **`lookback_seconds` 而非 `start/end`**：LLM 倾向"过去 N 秒"语义，绝对时间戳容易算错（时区、格式）。这种简化降低 LLM 出错率。
- **fleet nudge in description**：描述里直接告诉 LLM "prefer one vectorized PromQL with by(device_id, ...) / topk over repeated per-device queries"——这是 ongrid agent 的核心 prompt 工程，避免 N+1 query 反模式。
- **响应原样透传**：不裁剪、不重构，直接 Marshal `*InstantResult`——Prom 的 matrix/vector 结构对 LLM 透明。
- **`PromQuerier` 接口含 `Query`（instant）**：虽然 `query_promql` 只用 `QueryRange`，但接口包含 `Query` 让其他工具（correlate_incident 等）复用同一接口——避免多个窄接口。
- **`EdgeID = nil` 显式声明**：audit 行不绑定 device，因为 query_promql 是 tenant-agnostic，可能查集群级 metric（非 device-scoped）。

## 8. 注意事项

- **30s 超时**：大 lookback（7d）+ 高 cardinality 查询可能不够；超时后 LLM 可能重试，要注意 Prom 侧查询取消。
- **`LookbackSeconds` cap 7d**：7d 是 Prom 默认 retention 的下限，超过可能数据不全；LLM 要更长趋势应该用专门的长期存储工具（未来）。
- **无 expr 白名单**：PromQL 是图灵完备的（rate/derive/predict/histogram_quantile），LLM 可能写危险查询（如全量 `node_exporter_*` 扫描）——依赖 Prom 侧 query frontend 限流。
- **响应原样 Marshal**：Prom matrix 响应可能很大（多 series × 多 datapoints），LLM 上下文成本要注意；`stepFor` 的 ~30 datapoints 设计是主要 bound。
- **闭包路径与 BaseTool 路径并存**：见 `query_promql_basetool.go`，两路径 byte-for-byte 等价（注释明示 "identical bytes for equivalent inputs"），是 PR-3 试点 BaseTool 模式的第一个工具。
- **`maxQueryPromQLLookbackSeconds = 7 * 24 * 3600`**：常量定义在闭包文件，BaseTool 路径复用——避免 drift。
