# `find_outlier_edges_basetool.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/find_outlier_edges_basetool.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件是 `find_outlier_edges` 工具的 **BaseTool 形态**，镜像 `find_outlier_edges.go::executeFindOutlierEdges` 闭包路径。两者逻辑字节级一致：构造 z-score PromQL `((base) - on() group_left() avg(base)) / on() group_left() stddev(base) > sigma`，`QueryRange` 拉最近 5 分钟 30s step 数据，`decodeRankSeries` 解码后装饰 edge name。`cpu`/`mem`/`disk` 白名单显式校验，`sigma` 默认 2、上限 10。30s 超时。`WhenToUse` 强调 "OUTLIER"，反 guard 区分 `rank_edges`（top-N by raw value）与 `query_promql`（free-form stddev）。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 Registry BaseTool 路径注册调用；依赖 `basetool`、`edgebiz.Usecase`（edge name 装饰）、`PromQuerier`。与闭包路径 `find_outlier_edges.go` 并存。

## 3. 关键类型与接口

```go
type FindOutlierEdgesTool struct {
    promQuery PromQuerier
    edges     *edgebiz.Usecase
    log       *slog.Logger
}
```

复用 `find_outlier_edges.go` 定义的共享类型：`FindOutlierEdgesArgs`、`OutlierEdgeRow`、`FindOutlierEdgesSchema`、`ToolNameFindOutlierEdges`、`FindOutlierEdgesDescription`、`outlierCallTimeout`，以及包内 helper `rankMetricExpr` / `decodeRankSeries`。

## 4. 关键函数与流程

```go
func NewFindOutlierEdgesTool(promQuery PromQuerier, edges *edgebiz.Usecase, log *slog.Logger) *FindOutlierEdgesTool
func (t *FindOutlierEdgesTool) Info(_ context.Context) (*basetool.ToolInfo, error)
func (t *FindOutlierEdgesTool) InvokableRun(ctx, argsJSON, _ ...InvokeOption) (string, error)
```

`InvokableRun` 流程与 `Registry.executeFindOutlierEdges` 完全一致：
1. 守门 `promQuery != nil`、`edges != nil`。
2. Unmarshal → `FindOutlierEdgesArgs`。
3. `switch in.Metric` 白名单 `cpu`/`mem`/`disk`，否则 fail fast。
4. `rankMetricExpr(in.Metric)` 取 `base` + `label`。
5. `sigma` 默认 2，clamp 到 10。
6. 构造 z-score PromQL（`on() group_left()` 广播标量 avg/stddev）。
7. `end=now`，`start=end-5min`，`step=30s`；`context.WithTimeout(ctx, outlierCallTimeout=30s)`。
8. `promQuery.QueryRange` → `decodeRankSeries(res, label)` → 转 `[]OutlierEdgeRow`。
9. `edges.List(Limit:500)` best-effort 装饰 `EdgeName`（err 吞掉）。
10. Marshal `{"outliers": rows, "sigma": sigma, "metric": metric}` 返回。

## 5. 依赖关系

- **basetool**：`ToolInfo`（`Class="read"`）、`InvokeOption`（忽略）。
- **PromQuerier**：`QueryRange`，闭包路径与 BaseTool 路径共用同一接口。
- **edgebiz.Usecase**：`List(ctx, ListFilter{Limit:500})` 装饰 name。
- **rankMetricExpr / decodeRankSeries**（`rank_edges.go`）：与闭包路径共用。
- 不依赖 alertbiz / devicebiz / log / trace。

## 6. 并发与资源管理

- **无锁、无共享可变状态**：`FindOutlierEdgesTool` 仅持有不变依赖引用，多 goroutine 可并发调用。
- **无 goroutine**：串行 `QueryRange` → `decodeRankSeries` → `edges.List`，30s 超时覆盖。
- **`edges.List` best-effort**：err 吞掉，无 name 也能返回 outlier（EdgeName 空字符串）。

## 7. 设计模式与亮点

- **闭包/BaseTool 双轨镜像**：本文件是 BaseTool 形态，`find_outlier_edges.go` 是闭包形态。两套实现字节级一致，符合 ongrid closure path / basetool path 共存约定（迁移期双轨）。未来一行 type alias 即可切换。
- **WhenToUse 反 guard**：明确 "NOT for a top-N ranking by raw value (use rank_edges)"、"NOT for deviation on free-form metrics (use query_promql with stddev)"、"NOT for single-host details (use get_host_load)"，引导 LLM 正确路由到 outlier 工具而非近似工具。
- **复用 rank_edges helper**：`rankMetricExpr` + `decodeRankSeries` 跨 `rank_edges` / `find_outlier_edges`（闭包+BaseTool）共四处复用，避免重复实现。
- **metric 白名单显式 fail fast**：与闭包路径一致，`switch in.Metric` 而非依赖 `rankMetricExpr` ok-bool，确保 `load`/`composite` 被拒。

## 8. 注意事项

- **与闭包路径行为一致**：任何 PromQL / 白名单 / sigma clamp 修改需同步两处（`find_outlier_edges.go` + 本文件）。未来应抽出 `singleOutlier(ctx, promQuery, edges, in) (string, error)` 让两路径都代理调用，避免 drift。
- **sigma 下限无校验**：仅 clamp 上限到 10，0.1 这种极小值会通过，返回几乎所有 edge 为 outlier。依赖 schema `minimum: 0.5` 约束 LLM。
- **`edges.List(Limit:500)` 截断**：超过 500 edge 的 tenant 部分 outlier 无 name。当前规模够用。
- **无 tenant 过滤**：依赖 Prom 数据的 tenant 标签。z-score 本就是 fleet 级比较。
- **5min 窗口对慢漂移不敏感**：仅检测当下偏离 fleet，慢漂移需用 `rank_edges` 或 `query_promql` 长窗口。
- **`InvokeOption` 被忽略**：BaseTool 路径下 `opts` 不影响行为（无 tenant/session/llm_choice 等 ctx value 需要从 opts 读取）。
