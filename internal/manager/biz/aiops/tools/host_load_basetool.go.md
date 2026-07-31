# `host_load_basetool.go` 技术实现文档

> 源文件：`internal/manager\biz\aiops\tools\host_load_basetool.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件是 `get_host_load` 工具的 **BaseTool 形态**，N+15 batch refactor（2026-05-07）后支持 `device_ids[]`（1..16）。`runBatch` 4 并发 fan-out，每个 inner 调用走与闭包路径相同的 `MethodGetHostLoad` + `GetHostLoadRequest/Response`。per-id 15s 超时（复用 `hostLoadCallTimeout`）。`Class="read"`。`WhenToUse` 中文，强调 "fleet 视角问题一次性给所有 id"，反 guard 区分 `query_promql`（历史趋势）/ `get_process_list`（进程）/ `query_logql`（日志）/ `host_bash`（单设备深查）。注释明示闭包路径 `host_load.go::executeGetHostLoad` 是 PR-7 residue，graph kernel 不调用，保留构建直到 cutover PR 退役。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 Registry BaseTool 路径注册调用；依赖 `basetool`、`devicebiz.Usecase` / `edgebiz.Usecase`（via `DeviceResolver` + `deviceResolverAdapter`）、`pkg/tunnel`、`Caller`、`runBatch` / `validateBatchIDs`（`batch_helper.go`）。与闭包路径 `host_load.go` 并存。

## 3. 关键类型与接口

```go
type GetHostLoadTool struct {
    caller   Caller
    edges    *edgebiz.Usecase                          // legacy fallback
    resolver hostFilesDeviceResolver                   // device_id → edge_id
    log      *slog.Logger
}

type GetHostLoadBatchArgs struct {
    DeviceIDs []uint64 `json:"device_ids"`              // 1..16
}

type HostLoadResultEntry struct {
    DeviceID uint64                      `json:"device_id"`
    HostLoad *tunnel.GetHostLoadResponse `json:"host_load,omitempty"` // 成功才填
    Error    string                      `json:"error,omitempty"`     // 失败才填
}

type HostLoadBatchResponse struct {
    SuccessCount, ErrorCount int
    Results                  []HostLoadResultEntry
}
```

`GetHostLoadBatchSchema`：`maxItems: 16`，description 中文明示 "fleet 视角问题（'哪台 cpu 最高'/'对比这几台 mem'）用此一次性拉"。

复用 `host_load.go` 定义的共享类型：`ToolNameGetHostLoad`、`GetHostLoadDescription`、`hostLoadCallTimeout`。

## 4. 关键函数与流程

```go
func NewGetHostLoadTool(caller, edges, devices, log) *GetHostLoadTool
func (t *GetHostLoadTool) Info(_ context.Context) (*basetool.ToolInfo, error)
func (t *GetHostLoadTool) singleHostLoad(ctx, deviceID uint64) HostLoadResultEntry
func (t *GetHostLoadTool) InvokableRun(ctx, argsJSON, _ ...InvokeOption) (string, error)
```

**`InvokableRun` 流程**：
1. 守门 `caller != nil`。
2. Unmarshal → `GetHostLoadBatchArgs`。
3. `validateBatchIDs("device_ids", DeviceIDs)` 校验（maxItems 16）。
4. `runBatch(ctx, DeviceIDs, t.singleHostLoad)` 4 并发 fan-out，返回有序 `[]HostLoadResultEntry`。
5. 统计 `SuccessCount` / `ErrorCount`，Marshal `HostLoadBatchResponse` 返回。

**`singleHostLoad` 流程**：
1. `deviceID == 0` → `Error = "device_id must be > 0"`。
2. `resolver.LookupHostEdge(ctx, deviceID)` → `edgeID`。err → `Error = "resolve device %d: %v"`；`edgeID == 0` → `Error = "device_id=%d has no host-edge link"`。
3. Marshal `tunnel.GetHostLoadRequest{}`（空体）。
4. `context.WithTimeout(ctx, hostLoadCallTimeout=15s)`。
5. `caller.Call(callCtx, edgeID, MethodGetHostLoad, body)` → `respBody`。err → `Error = "dispatch: %v"`。
6. Unmarshal `tunnel.GetHostLoadResponse`。err → `Error = "decode resp: %v"`。
7. `entry.HostLoad = &resp`。

## 5. 依赖关系

- **basetool**：`ToolInfo`（`Class="read"`）、`InvokeOption`（忽略）。
- **DeviceResolver**（`device_resolver.go`）：`ResolveEdgeID`，通过 `deviceResolverAdapter` 适配到 `hostFilesDeviceResolver`。
- **Caller**：`Call(ctx, edgeID, method, body)`。
- **pkg/tunnel**：`GetHostLoadRequest`（空体）/ `GetHostLoadResponse` / `MethodGetHostLoad`。
- **runBatch**（`batch_helper.go`）：泛型 fan-out，4 并发，有序切片。
- **validateBatchIDs**（`batch_helper.go`）：maxItems 16 校验。

## 6. 并发与资源管理

- **`runBatch` 4 并发**：`batchConcurrency=4`，最多 4 个 `singleHostLoad` 同时跑。16 个 device 最坏 `⌈16/4⌉=4` 轮，每轮 15s → 60s。
- **per-id 15s 超时**：`singleHostLoad` 内 `context.WithTimeout(ctx, hostLoadCallTimeout=15s)`。即便 outer 未到，单 id 也不会超 15s。
- **无锁**：`GetHostLoadTool` 字段不变，`singleHostLoad` 内变量局部。`runBatch` 内部用 semaphore + waitgroup。
- **失败 fold 进 `Error`**：resolver / marshal / dispatch / decode 失败均转 `entry.Error` 字符串，其他 id 仍正常返回。

## 7. 设计模式与亮点

- **Batch-first refactor（N+15）**：从单 `edge_name` 升级到 `device_ids[]` 一次最多 16 个。注释明示 "the LLM was burning 5+ rounds doing 'cpu on 5 nodes' because the schema took a single edge_name"——batch 直接省 5+ 轮。
- **fleet 视角 nudge**：`WhenToUse` 强调 "优先一次给多个 device_id 做横向对比（'哪台 cpu 最高'）；单设备调用是反模式"。schema description 也强调 fleet 视角。
- **`runBatch` 4 并发 fan-out**：semaphore + waitgroup + 有序切片，结果顺序与输入 `device_ids` 一致。
- **`SuccessCount` / `ErrorCount` envelope**：批量响应统一信封，LLM 能快速判断 "几个成功几个失败"。
- **`Error` 仅失败才填**：成功时 `HostLoad` 非空，`Error` 空；失败时 `HostLoad` 空，`Error` 非空。`omitempty` 让 JSON 紧凑。
- **复用闭包路径常量**：`hostLoadCallTimeout` / `ToolNameGetHostLoad` / `GetHostLoadDescription` 都来自 `host_load.go`，确保两路径 metadata 一致。
- **`WhenToUse` 中文 + 反 guard**：明确 "NOT for: 历史趋势（用 query_promql）/ 进程清单（用 get_process_list）/ 日志（用 query_logql）/ 单设备深查（用 host_bash）"。

## 8. 注意事项

- **闭包路径是 residue**：注释明示 `host_load.go::executeGetHostLoad` 是 PR-7 residue，graph kernel 不调用。本 BaseTool 路径是当前活跃路径。cutover PR 退役闭包路径时，`hostLoadCallTimeout` 等共享常量需迁移到本文件或独立文件。
- **`device_ids` 必填**：schema `minItems: 1`，不支持 "all devices" 模式（与 `get_edge_summary_basetool` 的 "all edges" 不同）。LLM 需先用 `query_devices` 拿 id 列表。
- **`InvokeOption` 被忽略**：BaseTool 路径下 `opts` 不影响行为。
- **batch 顺序保证**：`runBatch` 返回有序切片，`results[i]` 对应 `DeviceIDs[i]`。
- **per-id 15s × 4 轮 = 60s**：16 个 device 最坏 60s，LLM round-trip 预算需考虑。典型 batch 大部分 id 快速返回，60s 是兜底。
- **`resolver.LookupHostEdge` 返回 (0, nil)**：device 存在但无 host-edge link 时返回 0 + nil error，工具报 "device_id=%d has no host-edge link"。这是 `DeviceResolver` 的 (0, nil) 双义设计。
- **无 `ExecuteResult.DeviceID` 回传**：与闭包路径不同，BaseTool 返回纯字符串，无法回传 DeviceID 给上层 graph/audit。依赖 audit 装饰器从 args 解析。
