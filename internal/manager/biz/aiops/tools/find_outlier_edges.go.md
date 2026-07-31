# `find_outlier_edges.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/find_outlier_edges.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件实现 `find_outlier_edges` 工具（闭包路径，挂在 `Registry.executeFindOutlierEdges`）：用 z-score PromQL 找出 metric 值偏离 fleet 均值超过 N 个标准差的 edge。仅支持 `cpu` / `mem` / `disk` 三个闭集 metric（显式白名单，不依赖 `rankMetricExpr` 的 ok-bool），默认 `sigma=2`、上限 10。PromQL 形如 `((base) - on() group_left() avg(base)) / on() group_left() stddev(base) > sigma`，`on() group_left()` 把标量 avg/stddev 广播到每条 per-edge 样本。`QueryRange` 拉最近 5 分钟、30s step 的数据，`decodeRankSeries` 解码后装饰 edge name，30s 调用超时。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 Registry 闭包路径调用；依赖 `edgebiz.Usecase`（edge name 装饰）、`PromQuerier`（QueryRange）。**未提供 BaseTool 形态**——仅闭包路径（与 `find_outlier_edges_basetool.go` 并存，后者是 BaseTool 镜像）。

## 3. 关键类型与接口

```go
type FindOutlierEdgesArgs struct {
    Metric string  `json:"metric"`            // cpu|mem|disk，必填
    Sigma  float64 `json:"sigma,omitempty"`   // 默认 2，[0.5, 10]
}

type OutlierEdgeRow struct {
    EdgeID   uint64  `json:"edge_id"`
    EdgeName string  `json:"edge_name"`
    ZScore   float64 `json:"z_score"`
    Metric   string  `json:"metric"`
}

const outlierCallTimeout = 30 * time.Second
```

依赖包内共享的 `rankMetricExpr(metric) (base, label string, _ bool)`（在 `rank_edges.go` 定义），返回 metric 的 PromQL 基础表达式与 edge_id label 名。

## 4. 关键函数与流程

```go
func (r *Registry) executeFindOutlierEdges(ctx, args json.RawMessage) (ExecuteResult, error)
```

流程：
1. 守门：`r.promQuery != nil`、`r.edges != nil`，缺一报错。
2. Unmarshal → `FindOutlierEdgesArgs`。
3. **白名单校验** `in.Metric`：必须 `cpu` / `mem` / `disk`，否则报 "metric must be cpu, mem or disk; got %q"。注释明示 `rankMetricExpr` 虽然也接受 `load` / `composite`（给 `rank_edges` 用），但 z-score 检测不在 scope，fail fast 而非依赖 ok-bool。
4. `rankMetricExpr(in.Metric)` 取 `base`（PromQL 表达式）与 `label`（edge_id label 名）。
5. `sigma` 默认 2，超过 10 clamp 到 10。（无下限 0.5 校验，但 schema 约束 `minimum: 0.5`，依赖 LLM 遵守 schema。）
6. **构造 z-score PromQL**：
   ```
   ((base) - on() group_left() avg(base)) / on() group_left() stddev(base) > sigma
   ```
   注释解释：`on() group_left()` 把标量 avg/stddev 广播到每条 per-edge 样本，否则 Prom 因 label set 不匹配会丢掉整个 vector。
7. `end = time.Now()`，`start = end.Add(-5*time.Minute)`，`step = 30*time.Second`。
8. `context.WithTimeout(ctx, outlierCallTimeout=30s)`。
9. `r.promQuery.QueryRange(callCtx, expr, start, end, step)`。
10. `decodeRankSeries(res, label)` 复用 `rank_edges` 的解码逻辑，返回 `[]rankRow{EdgeID, Value, Metric}`。
11. 转成 `[]OutlierEdgeRow{EdgeID, ZScore: rr.Value, Metric: rr.Metric}`。
12. **edge name 装饰**：`r.edges.List(callCtx, ListFilter{Limit:500})` 拉最多 500 条 edge，建 `nameByID map`，回填 `EdgeName`。err 被忽略（best-effort，无 name 也能返回）。
13. Marshal `{"outliers": rows, "sigma": sigma, "metric": metric}` 返回 `ExecuteResult{ResultJSON: out}`。

## 5. 依赖关系

- **PromQuerier**：`QueryRange(ctx, expr, start, end, step)`，返回 `*PromResponse`。
- **edgebiz.Usecase**：`List(ctx, ListFilter{Limit:500})` 用于 name 装饰。
- **rankMetricExpr**（`rank_edges.go`）：返回 metric → PromQL 表达式 + label 名。本工具白名单 cpu/mem/disk 是 `rankMetricExpr` 支持集的子集（后者还支持 load/composite）。
- **decodeRankSeries**（`rank_edges.go`）：解码 Prom matrix 为 `[]rankRow`，复用避免重复实现。
- 不依赖 alertbiz / devicebiz / log / trace。

## 6. 并发与资源管理

- **无锁、无共享可变状态**：`Registry` 字段不变，`executeFindOutlierEdges` 内变量局部。多 goroutine 可并发调用。
- **无 goroutine**：串行 `promQuery.QueryRange` → `decodeRankSeries` → `edges.List`，30s 超时覆盖。
- **`edges.List` best-effort**：err 被吞，无 name 也能返回 `OutlierEdgeRow`（EdgeName 空字符串）。不影响主流程。

## 7. 设计模式与亮点

- **z-score 一次性 PromQL**：用 `on() group_left()` 在 PromQL 端完成 "per-edge - fleet_avg / fleet_stddev" 计算，避免拉全量样本回 Go 端算。一次 `QueryRange` 即得 outlier 集合，省带宽与 roundtrip。
- **metric 白名单显式 fail fast**：不依赖 `rankMetricExpr` 的 ok-bool，而是 `switch in.Metric { case "cpu","mem","disk": ...; default: error }`。这样 `load` / `composite` 即便 `rankMetricExpr` 支持也会被本工具拒绝——z-score on load/composite 语义不明确，不在 scope。
- **复用 rank_edges 解码**：`rankMetricExpr` + `decodeRankSeries` 两个 helper 都来自 `rank_edges.go`，避免重复实现。outlier 与 rank 共享同一套 metric → PromQL 翻译表。
- **edge name 装饰 best-effort**：`edges.List` 失败不阻断，outlier 数据仍返回（无 name）。这与其他 metric 工具的 "edge name 是增强，不是必需" 一致。
- **5min 窗口 + 30s step**：足够平滑瞬时抖动，又不至于拉太久。z-score 对窗口长度敏感，5min 是经验值。
- **闭包路径独有**：本文件是 `Registry.executeFindOutlierEdges`，无 BaseTool 形态。`find_outlier_edges_basetool.go` 是 BaseTool 镜像，两套并存符合 ongrid closure path / basetool path 双轨约定。

## 8. 注意事项

- **sigma 下限无校验**：代码只 clamp 上限到 10，未校验下限 0.5。若 LLM 不遵守 schema 传 0 或负数，`sigma <= 0` 会被默认 2 覆盖（`if in.Sigma <= 0 { in.Sigma = 2 }`），但 0.1 这种极小值会通过，返回几乎所有 edge 为 outlier。依赖 schema `minimum: 0.5` 约束 LLM。
- **`edges.List(Limit:500)` 截断**：超过 500 条 edge 的 tenant 会丢 name 映射，部分 outlier 的 `EdgeName` 为空。当前 tenant 规模够用；若扩大需分页或按 outlier EdgeID 批量查。
- **无 tenant 过滤**：`promQuery.QueryRange` 用全局 PromQL，依赖 Prom 那边的 tenant label 过滤（或全 fleet 视角）。z-score 本就是 fleet 级比较，tenant 维度由 Prom 数据决定。
- **无 BaseTool 形态**：本文件仅闭包路径。若需在 BaseTool registry 注册，用 `find_outlier_edges_basetool.go`（镜像实现）。
- **`decodeRankSeries` 错误上抛**：Prom 返回的 matrix 若解码失败，整工具报错 "decode: %w"。不像 `edges.List` 那样 best-effort。
- **PromQL 注入风险低**：`base` 来自 `rankMetricExpr` 闭集映射，`sigma` 是 `%g` 格式化的 float，LLM 无法注入任意 PromQL。
- **5min 窗口对慢漂移不敏感**：z-score 检测的是 "当下偏离 fleet"，对缓慢漂移（如某 edge 一周内 cpu 持续升高）不敏感，需用 `rank_edges` 或 `query_promql` 长窗口查询。
