# `host_processes_basetool.go` 技术实现文档

> 源文件：`internal/manager\biz\aiops\tools\host_processes_basetool.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件是 `get_host_processes` 工具的 **BaseTool 形态**，N+15 batch refactor 后支持 `device_ids[]`（1..16）。`runBatch` 4 并发 fan-out，每个 inner 调用走与闭包路径相同的 `MethodGetProcessList`。**`top_n` / `sort_by` 是标量，对所有 device 共享**（一个口子调一次）——注释明示 "the typical use case is 'top 10 cpu procs on each of these 5 nodes' and asking for per-id top_n would clutter the schema for negligible benefit"。per-id 15s 超时（复用 `processListCallTimeout`）。`Class="read"`。`WhenToUse` 中文，强调 "fleet 比对（typical 5-10 device 一次）"。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 Registry BaseTool 路径注册调用；依赖 `basetool`、`devicebiz.Usecase` / `edgebiz.Usecase`（via `DeviceResolver` + `deviceResolverAdapter`）、`pkg/tunnel`、`Caller`、`runBatch` / `validateBatchIDs`。与闭包路径 `host_processes.go` 并存。

## 3. 关键类型与接口

```go
type GetProcessListTool struct {
    caller   Caller
    edges    *edgebiz.Usecase                          // legacy fallback
    resolver hostFilesDeviceResolver
    log      *slog.Logger
}

type GetProcessListBatchArgs struct {
    DeviceIDs []uint64 `json:"device_ids"`              // 1..16
    TopN      uint32   `json:"top_n"`                   // 标量，共享，默认 10
    SortBy    string   `json:"sort_by"`                 // 标量，共享，cpu|mem，默认 cpu
}

type ProcessListResultEntry struct {
    DeviceID    uint64                          `json:"device_id"`
    ProcessList *tunnel.GetProcessListResponse  `json:"process_list,omitempty"`
    Error       string                          `json:"error,omitempty"`
}

type ProcessListBatchResponse struct {
    SuccessCount, ErrorCount int
    Results                  []ProcessListResultEntry
}
```

`GetProcessListBatchSchema`：`device_ids` maxItems 16，`top_n` / `sort_by` 标量。description 中文明示 "fleet 视角看进程对比应该一次性给所有 id"。

复用 `host_processes.go` 定义的共享类型：`ToolNameGetProcessList`、`GetProcessListDescription`、`processListCallTimeout`。

## 4. 关键函数与流程

```go
func NewGetProcessListTool(caller, edges, devices, log) *GetProcessListTool
func (t *GetProcessListTool) Info(_ context.Context) (*basetool.ToolInfo, error)
func (t *GetProcessListTool) singleProcessList(ctx, deviceID, topN, sortBy) ProcessListResultEntry
func (t *GetProcessListTool) InvokableRun(ctx, argsJSON, _ ...InvokeOption) (string, error)
```

**`InvokableRun` 流程**：
1. 守门 `caller != nil`。
2. Unmarshal → `GetProcessListBatchArgs`。
3. `validateBatchIDs("device_ids", DeviceIDs)` 校验。
4. `TopN == 0` → 默认 10。
5. `SortBy` 校验：`ProcessSortByCPU` / `ProcessSortByMem` 通过；空 → 默认 `ProcessSortByCPU`；其他 → 报错。
6. `runBatch(ctx, DeviceIDs, func(ctx, id) { return t.singleProcessList(ctx, id, in.TopN, in.SortBy) })` 4 并发 fan-out。
7. 统计 `SuccessCount` / `ErrorCount`，Marshal `ProcessListBatchResponse` 返回。

**`singleProcessList` 流程**：
1. `deviceID == 0` → `Error = "device_id must be > 0"`。
2. `resolver.LookupHostEdge(ctx, deviceID)` → `edgeID`。err / `edgeID == 0` → `Error`。
3. Marshal `tunnel.GetProcessListRequest{TopN, SortBy}`。
4. `context.WithTimeout(ctx, processListCallTimeout=15s)`。
5. `caller.Call(callCtx, edgeID, MethodGetProcessList, body)` → `respBody`。err → `Error`。
6. Unmarshal `tunnel.GetProcessListResponse`。err → `Error`。
7. `entry.ProcessList = &resp`。

## 5. 依赖关系

- **basetool**：`ToolInfo`（`Class="read"`）、`InvokeOption`（忽略）。
- **DeviceResolver**（`device_resolver.go`）：`ResolveEdgeID`，通过 `deviceResolverAdapter` 适配。
- **Caller**：`Call(ctx, edgeID, method, body)`。
- **pkg/tunnel**：`GetProcessListRequest{TopN, SortBy}` / `GetProcessListResponse` / `MethodGetProcessList` / `ProcessSortByCPU` / `ProcessSortByMem`。
- **runBatch** / **validateBatchIDs**（`batch_helper.go`）。

## 6. 并发与资源管理

- **`runBatch` 4 并发**：`batchConcurrency=4`，最多 4 个 `singleProcessList` 同时跑。16 个 device 最坏 `⌈16/4⌉=4` 轮，每轮 15s → 60s。
- **per-id 15s 超时**：`singleProcessList` 内 `context.WithTimeout(ctx, processListCallTimeout=15s)`。
- **无锁**：`GetProcessListTool` 字段不变，`singleProcessList` 内变量局部。
- **失败 fold 进 `Error`**：resolver / marshal / dispatch / decode 失败均转 `entry.Error`。

## 7. 设计模式与亮点

- **`top_n` / `sort_by` 标量共享**：注释明示 "asking for per-id top_n would clutter the schema for negligible benefit"。典型用例 "top 10 cpu procs on each of these 5 nodes" 只需一个 `top_n=10, sort_by=cpu` 应用于所有 device。
- **Batch-first refactor（N+15）**：从单 `edge_name` 升级到 `device_ids[]`。fleet 视角看进程对比一次性拉，省 LLM 轮次。
- **`runBatch` 4 并发 fan-out**：有序切片，结果顺序与输入一致。
- **`SuccessCount` / `ErrorCount` envelope**：统一信封。
- **复用闭包路径常量**：`processListCallTimeout` / `ToolNameGetProcessList` / `GetProcessListDescription` 都来自 `host_processes.go`。
- **`WhenToUse` 中文 + 反 guard**：明确 "NOT for: 单设备深查（直接 host_bash 'ps aux' 更灵活）/ 历史进程数据 / 日志（用 query_logql）/ 指标趋势（用 query_promql）"。
- **闭包路径是 residue**：注释明示 "The closure path (host_processes.go::executeGetProcessList) is untouched — see host_load_basetool.go for the same rationale"。

## 8. 注意事项

- **闭包路径是 residue**：与 `host_load_basetool.go` 一样，`host_processes.go::executeGetProcessList` 是 PR-7 residue，graph kernel 不调用。cutover PR 退役闭包路径时，`processListCallTimeout` 等共享常量需迁移。
- **`top_n` / `sort_by` 标量共享**：无法为不同 device 传不同 top_n/sort_by。若需 "device A top 10 cpu + device B top 20 mem" 需分两次调用。
- **`TopN` 未 clamp 上限**：与闭包路径一致，代码只默认 0→10，未校验 >100。依赖 schema `maximum: 100` 约束 LLM。
- **`device_ids` 必填**：schema `minItems: 1`，不支持 "all devices" 模式。
- **`InvokeOption` 被忽略**。
- **batch 顺序保证**：`results[i]` 对应 `DeviceIDs[i]`。
- **per-id 15s × 4 轮 = 60s**：16 个 device 最坏 60s。
- **无 `ExecuteResult.DeviceID` 回传**：BaseTool 返回纯字符串，依赖 audit 装饰器从 args 解析。
