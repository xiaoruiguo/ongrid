# `get_edge_summary_basetool.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/get_edge_summary_basetool.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件是 `get_edge_summary` 工具的 **BaseTool 形态**，N+15 batch refactor 后支持 `device_ids[]` 批量。每个 inner 调用跑与闭包路径相同的 3-step stitch（edge meta + best-effort host_load + 24h incidents + plugin_status），外层用 `runBatch` 4 并发 fan-out，最多 16 个 device_id。90s 外层超时（`edgeSummaryBatchTimeout`，比 per-id 30s 宽，让 4 并发的 16 个能完成）。**支持 "all edges" 模式**：`device_ids` 留空 → `allEdgeDeviceIDs` 拉全部 edge（cap 16）→ 巡检所有设备。`Class="read"`。`WhenToUse` 中文，强调 "一次给多个 device_id" 与 "巡检所有设备"。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 Registry BaseTool 路径注册调用；依赖 `basetool`、`alertbiz.Usecase`、`devicebiz.Usecase`、`edgebiz.Usecase`、`devicemodel.DecodeRoles`、`edgemodel.StatusOnline`、`tunnel.MethodGetHostLoad`、`runBatch`（`batch_helper.go`）、`validateBatchIDs`。与闭包路径 `get_edge_summary.go` 并存。

## 3. 关键类型与接口

```go
type GetEdgeSummaryTool struct {
    caller  Caller
    edges   *edgebiz.Usecase
    devices *devicebiz.Usecase
    alertUC AlertUsecase
    log     *slog.Logger
}

type GetEdgeSummaryBatchArgs struct {
    DeviceIDs []uint64 `json:"device_ids"` // 可留空 = all edges (cap 16)
}

type EdgeSummaryResultEntry struct {
    DeviceID uint64         `json:"device_id"`
    Summary  map[string]any `json:"summary,omitempty"` // 与闭包路径同结构
    Error    string         `json:"error,omitempty"`   // 仅 resolver 失败才填
}

type EdgeSummaryBatchResponse struct {
    SuccessCount, ErrorCount int
    Results                  []EdgeSummaryResultEntry
}

const (
    edgeSummaryBatchTimeout = 90 * time.Second
    edgeSummaryAllCap       = 16 // "all edges" 模式的 cap
)
```

`GetEdgeSummaryBatchSchema` 允许 `minItems: 0`，schema description 中文明示 "省略或留空 = 汇总全部边端设备（最多 16 个）"。

## 4. 关键函数与流程

```go
func NewGetEdgeSummaryTool(caller, edges, devices, alertUC, log) *GetEdgeSummaryTool
func (t *GetEdgeSummaryTool) Info(_ context.Context) (*basetool.ToolInfo, error)
func (t *GetEdgeSummaryTool) singleEdgeSummary(ctx, deviceID uint64) EdgeSummaryResultEntry
func (t *GetEdgeSummaryTool) InvokableRun(ctx, argsJSON, _ ...InvokeOption) (string, error)
func (t *GetEdgeSummaryTool) allEdgeDeviceIDs(ctx) ([]uint64, error)
func (t *GetEdgeSummaryTool) resolveEdgeForDevice(ctx, deviceID, _ string) (*edgemodel.Edge, error)
```

**`InvokableRun` 流程**：
1. 守门 `t.edges != nil`。
2. Unmarshal → `GetEdgeSummaryBatchArgs`。
3. **"All edges" 模式**：`len(DeviceIDs) == 0` → `allEdgeDeviceIDs(ctx)` 拉 `edges.List({})` 全部 edge，优先取 `e.DeviceID`（若有）否则用 `e.ID`，cap 到 `edgeSummaryAllCap=16`。
4. 否则 `validateBatchIDs("device_ids", DeviceIDs)` 校验（maxItems 16）。
5. `context.WithTimeout(ctx, edgeSummaryBatchTimeout=90s)`。
6. `runBatch(batchCtx, DeviceIDs, t.singleEdgeSummary)` 4 并发 fan-out，返回有序 `[]EdgeSummaryResultEntry`。
7. 统计 `SuccessCount` / `ErrorCount`，Marshal `EdgeSummaryBatchResponse` 返回。

**`singleEdgeSummary` 流程**（与闭包路径 `executeGetEdgeSummary` 字节级一致）：
1. `deviceID == 0` → `Error = "device_id must be > 0"`。
2. `context.WithTimeout(ctx, edgeSummaryCallTimeout=30s)`（per-id 超时）。
3. `resolveEdgeForDevice(callCtx, deviceID, "")` → `*edgemodel.Edge`。失败 → `Error = err.Error()`。
4. Roles 装饰：`devices.Get(*edge.DeviceID)` → `DecodeRoles`。missing → `[]string{}`。
5. `out["edge"] = {id, device_id, name, status, roles, last_seen_at, created_at}`。
6. Best-effort host_load：`caller != nil && edge.Status == Online` → `caller.Call(edge.ID, MethodGetHostLoad, GetHostLoadRequest{})` → `out["host_load"]`。err 静默。
7. Recent incidents：`alertUC.ListIncidents(IncidentFilter{DeviceID: &edgeID, Limit: 100})` → 24h cutoff + severity != "info" 过滤 → `out["recent_incidents"]`。
8. `out["plugin_status"] = "unsupported"`。
9. `entry.Summary = out`。

**`allEdgeDeviceIDs`**：`edges.List({})` 拉全部，遍历优先取 `DeviceID`（非 nil 非 0）否则用 `e.ID`，cap 16 break。

**`resolveEdgeForDevice`**（BaseTool 版）：与闭包路径 helper 同逻辑，仅 by-id 路径（by-name 在新 schema 未暴露）。device by id → junction → edge by id legacy fallback。

## 5. 依赖关系

- **basetool**：`ToolInfo`（`Class="read"`）、`InvokeOption`（忽略）。
- **alertbiz.Usecase**：`ListIncidents`。
- **devicebiz.Usecase**：`Get` / `Links()`。
- **edgebiz.Usecase**：`Get` / `List({})`。
- **runBatch**（`batch_helper.go`）：泛型 fan-out，4 并发，有序切片。
- **validateBatchIDs**（`batch_helper.go`）：maxItems 16 校验。
- **tunnel**：`GetHostLoadRequest/Response` / `MethodGetHostLoad`。
- **Caller**：host_load 派发。

## 6. 并发与资源管理

- **`runBatch` 4 并发**：`batchConcurrency=4`（`batch_helper.go` 定义），最多 4 个 `singleEdgeSummary` 同时跑。16 个 device_id 最坏 `⌈16/4⌉=4` 轮，每轮 30s → 120s，outer 90s cap 实践中足够（注释明示典型 batch 大部分 id 快速返回）。
- **per-id 30s 超时**：`singleEdgeSummary` 内 `context.WithTimeout(ctx, edgeSummaryCallTimeout=30s)`。即便 outer 90s 未到，单 id 也不会超 30s。
- **best-effort 子调用**：host_load / incidents 的 err 静默吞掉，仅 resolver 失败填 `Error`。保证 "edge 能识别但某子查询失败" 仍返回 partial summary。
- **无锁**：`GetEdgeSummaryTool` 字段不变，`singleEdgeSummary` 内变量局部。`runBatch` 内部用 semaphore + waitgroup，无共享可变状态。

## 7. 设计模式与亮点

- **Batch-first refactor（N+15）**：从单 id 升级到 `device_ids[]` 一次最多 16 个，省 LLM 轮次。`WhenToUse` 强调 "一次给多个 device_id" 与 "省得逐台单独调"。
- **"All edges" 模式**：`device_ids` 留空 → `allEdgeDeviceIDs` 自动汇总全部 edge（cap 16）。schema description 中文明示 "巡检 / 体检所有设备"。这让 "巡检所有设备" 这类不指定 id 的请求无需先查设备清单。
- **`runBatch` 4 并发 fan-out**：semaphore + waitgroup + 有序切片，结果顺序与输入 `device_ids` 一致，便于 LLM 对位解读。
- **inner 与闭包路径字节级一致**：`singleEdgeSummary` 的 stitch 逻辑（edge meta + roles + host_load + incidents + plugin_status）与 `Registry.executeGetEdgeSummary` 完全相同。未来应抽出共享 helper 避免 drift。
- **`SuccessCount` / `ErrorCount` envelope**：批量响应统一信封，LLM 能快速判断 "几个成功几个失败"。
- **`Error` 仅 resolver 失败才填**：子查询失败仍算 success（partial summary）。这与 ongrid "partial result 优于 error" 设计哲学一致。
- **`WhenToUse` 中文**：包括 "NOT for: 单设备深查（用 host_bash + ps + journalctl）/ 集群级聚合（用 rank_edges）/ 诊断单个 incident 的 metric+log+trace 关联（用 correlate_incident）/ 列设备清单（用 query_devices）"，反 guard 丰富。

## 8. 注意事项

- **`edgeSummaryAllCap=16` 截断**：超过 16 台 edge 的 tenant 在 "all" 模式下会丢部分设备。schema description 明示 "最多 16 个"。如需全量巡检应分批或用 `query_devices` + 分页。
- **`edges.List({})` 无 Limit**：`allEdgeDeviceIDs` 调 `edges.List(ListFilter{})` 不传 Limit，依赖 edgebiz 默认 limit。若 tenant edge 数巨大可能拉全量再截 16，效率不高。生产应传 `Limit: edgeSummaryAllCap`。
- **`resolveEdgeForDevice` BaseTool 版仅 by-id**：by-name 路径在新 schema 未暴露（`device_ids[]` 都是数字 id）。若 LLM 传 name 会 unmarshal 失败。
- **inner 与闭包路径 drift 风险**：`singleEdgeSummary` 与 `executeGetEdgeSummary` 是两份 stitch 实现，任何 meta 字段 / incident 过滤逻辑修改需同步两处。未来应抽 `singleEdgeSummary(ctx, caller, edges, devices, alertUC, deviceID) (map[string]any, error)` 让两路径都代理调用。
- **90s outer 超时偏宽**：典型 batch 大部分 id 快速返回，90s 是兜底防慢 edge 拖垮 LLM round-trip。若 tenant 普遍慢 edge 需考虑缩短。
- **`plugin_status = "unsupported"`**：与闭包路径一致，占位。wire 后两处都要改。
- **`InvokeOption` 被忽略**：BaseTool 路径下 `opts` 不影响行为。
- **batch 顺序保证**：`runBatch` 返回有序切片，`results[i]` 对应 `DeviceIDs[i]`，LLM 可对位解读。
