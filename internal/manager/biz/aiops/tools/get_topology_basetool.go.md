# `get_topology_basetool.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/get_topology_basetool.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件是 `get_topology` 工具的 **BaseTool 形态**，镜像 `get_topology.go::executeGetTopology` 闭包路径。两者逻辑字节级一致：返回部署级拓扑快照（manager version、edge fleet size + online count、configured Prom/Loki/Tempo URL、channel count、enabled rule count）。无参数。10s 超时。所有子查询 best-effort：缺 edge/alert dep 则对应字段不出现。`alertUC` 与 `edges` 可为 nil，工具降级到能填的部分。`Class="read"`。`WhenToUse` 反 guard 区分 `get_edge_summary`（单 host）/ `get_incident_detail`（单 incident）/ `query_promql`（实际 metric 值）。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 Registry BaseTool 路径注册调用；依赖 `basetool`、`edgebiz.Usecase`、`AlertUsecase`、`edgemodel.StatusOnline`、`TopologyInfo`（在 `get_topology.go` 定义）。与闭包路径并存。

## 3. 关键类型与接口

```go
type GetTopologyTool struct {
    edges    *edgebiz.Usecase   // 可 nil，降级
    alertUC  AlertUsecase       // 可 nil，降级
    topology TopologyInfo       // value type，构造时注入
    log      *slog.Logger
}
```

复用 `get_topology.go` 定义的共享类型：`TopologyInfo`、`ToolNameGetTopology`、`GetTopologyDescription`、`GetTopologySchema`、`topologyCallTimeout`。

## 4. 关键函数与流程

```go
func NewGetTopologyTool(edges *edgebiz.Usecase, alertUC AlertUsecase, topology TopologyInfo, log *slog.Logger) *GetTopologyTool
func (t *GetTopologyTool) Info(_ context.Context) (*basetool.ToolInfo, error)
func (t *GetTopologyTool) InvokableRun(ctx, _ string, _ ...InvokeOption) (string, error)
```

`InvokableRun` 流程（与 `Registry.executeGetTopology` 字节级一致）：
1. `context.WithTimeout(ctx, topologyCallTimeout=10s)`。
2. 初始化 `out map[string]any`：`manager_version` / `configured_prom_url` / `configured_loki_url` / `configured_tempo_url`（来自 `t.topology`）。
3. **Edge fleet**：`t.edges != nil` 时 `edges.List(callCtx, ListFilter{Limit:5000})`，err 静默。遍历统计 `online`，填 `edge_count` / `online_count`。
4. **Enabled rules**：`t.alertUC != nil` 时 `alertUC.ListRules(callCtx, "")`，err 静默。遍历统计 `enabled`，填 `enabled_rule_count` / `rule_count`。
5. **Channel count**：`t.topology.ChannelCounter != nil` 时调用，err 静默，填 `channel_count`。
6. Marshal 返回字符串（`InvokableRun` 签名，非 `ExecuteResult`）。

## 5. 依赖关系

- **basetool**：`ToolInfo`（`Class="read"`）、`InvokeOption`（忽略）。
- **edgebiz.Usecase**：`List(ctx, ListFilter{Limit:5000})`。
- **AlertUsecase**：`ListRules(ctx, "")`。
- **edgemodel.StatusOnline**：edge 在线状态常量。
- **TopologyInfo**：value type，构造时注入。
- 不依赖 devicebiz / prom / log / trace / tunnel。

## 6. 并发与资源管理

- **无锁、无共享可变状态**：`GetTopologyTool` 字段不变（`topology` 是 value type），`InvokableRun` 内变量局部。多 goroutine 可并发调用。
- **无 goroutine**：串行 `edges.List` → `alertUC.ListRules` → `ChannelCounter`，10s 超时覆盖。
- **best-effort 子调用**：每个子查询 err 静默吞掉，对应字段不出现。

## 7. 设计模式与亮点

- **闭包/BaseTool 双轨镜像**：本文件是 BaseTool 形态，`get_topology.go` 是闭包形态。两套实现字节级一致，符合 ongrid closure path / basetool path 共存约定。未来一行 type alias 即可切换。
- **nil-safe 降级**：`edges` / `alertUC` 可为 nil，对应字段不出现。注释明示 "alertUC and edges may be nil — the tool degrades to whatever it can populate"。
- **`topology` value type**：构造时注入 resolved deployment-level facts，mirrors `Registry.SetTopologyInfo`。value type 避免共享可变状态。
- **`WhenToUse` 反 guard**：明确 "NOT for a single host (use get_edge_summary). NOT for a specific incident (use get_incident_detail). NOT for actual metric values (use query_promql)."，引导 LLM 正确路由到部署级工具而非近似工具。
- **best-effort 子调用**：与闭包路径一致，缺 dep 仍返回部分拓扑。

## 8. 注意事项

- **与闭包路径行为一致**：任何字段 / 子查询逻辑修改需同步两处（`get_topology.go` + 本文件）。未来应抽 `singleTopology(ctx, edges, alertUC, topology) (map[string]any, error)` 让两路径都代理调用，避免 drift。
- **`edges.List(Limit:5000)` 截断**：与闭包路径一致，超过 5000 edge 的 tenant 会丢计数。
- **`ListRules("")` 拉全量**：与闭包路径一致，规则数可能较大。
- **`ChannelCounter` 是回调**：与闭包路径一致，若回调自身慢会触发 10s 超时。
- **`InvokeOption` 被忽略**：BaseTool 路径下 `opts` 不影响行为。
- **无 `ExecuteResult.DeviceID` 回传**：与闭包路径一致，部署级工具无 device 维度。BaseTool 路径返回纯字符串，无法回传 DeviceID。
- **`topology` 是 value type**：构造后修改 `TopologyInfo` 字段不会影响已注入的 tool。若需热更新需重新构造 tool 或改用指针。
