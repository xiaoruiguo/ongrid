# `host_processes.go` 技术实现文档

> 源文件：`internal/manager\biz\aiops\tools\host_processes.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件实现 `get_host_processes` 工具（闭包路径，挂在 `Registry.executeGetProcessList`）：返回单台 edge host 的 top-N 进程，按 CPU 或内存排序。通过 `edge_name` 解析 `edge.ID`，`Caller.Call` 派发 `tunnel.MethodGetProcessList`。`top_n` 默认 10，`sort_by` 默认 "cpu"（枚举 cpu/mem）。15s 超时。**注意**：与 `host_load.go` 一样是 PR-7 residue，graph kernel 不调用——`host_processes_basetool.go` 的 BaseTool 形态（batch `device_ids[]`）是当前活跃路径。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 Registry 闭包路径注册（但 graph kernel 不调用）；依赖 `edgebiz.Usecase`（`GetByName`）、`pkg/tunnel`（`GetProcessListRequest/Response` / `MethodGetProcessList` / `ProcessSortByCPU` / `ProcessSortByMem`）、`Caller`。与 `host_processes_basetool.go`（BaseTool 镜像，batch）并存。

## 3. 关键类型与接口

```go
type GetProcessListArgs struct {
    EdgeName string `json:"edge_name"`  // 必填
    TopN     uint32 `json:"top_n"`       // 默认 10，[1,100]
    SortBy   string `json:"sort_by"`     // cpu|mem，默认 cpu
}

const processListCallTimeout = 15 * time.Second
```

依赖 `tunnel.ProcessSortByCPU` / `tunnel.ProcessSortByMem` 常量（确保 sort_by 与 edge 端解析一致）。

## 4. 关键函数与流程

```go
func (r *Registry) executeGetProcessList(ctx, args json.RawMessage) (ExecuteResult, error)
```

流程：
1. Unmarshal → `GetProcessListArgs`；`EdgeName == ""` → 报错。
2. `TopN == 0` → 默认 10。
3. `SortBy` 校验：`ProcessSortByCPU` / `ProcessSortByMem` 通过；空 → 默认 `ProcessSortByCPU`；其他 → 报错 "sort_by must be cpu or mem"。
4. `r.edges.GetByName(ctx, in.EdgeName)` → `*edgemodel.Edge`。err 上抛 "resolve edge: %w"。
5. Marshal `tunnel.GetProcessListRequest{TopN, SortBy}`。
6. `context.WithTimeout(ctx, processListCallTimeout=15s)`。
7. `r.caller.Call(callCtx, edge.ID, tunnel.MethodGetProcessList, body)` → `respBody`。
8. Unmarshal `tunnel.GetProcessListResponse`，Marshal 返回 `ExecuteResult{ResultJSON: out, DeviceID: &edge.ID}`。
9. 任一 step 失败仍回传 `ExecuteResult{DeviceID: &edge.ID}` + error。

## 5. 依赖关系

- **edgebiz.Usecase**：`GetByName(ctx, name)`。
- **Caller**：`Call(ctx, edgeID, method, body)`。
- **pkg/tunnel**：`GetProcessListRequest{TopN, SortBy}` / `GetProcessListResponse` / `MethodGetProcessList` / `ProcessSortByCPU` / `ProcessSortByMem`。
- 不依赖 devicebiz / alertbiz / prom / log / trace。

## 6. 并发与资源管理

- **无锁、无共享可变状态**：`Registry` 字段不变，`executeGetProcessList` 内变量局部。多 goroutine 可并发调用。
- **无 goroutine**：单次 `GetByName` + 单次 `Caller.Call`，15s 超时覆盖。
- **`ExecuteResult.DeviceID` 始终回传**：即便 step 失败也回传 `&edge.ID`。

## 7. 设计模式与亮点

- **`edge_name` 而非 `device_id`**：与 `host_load.go` 一样是 device split 前设计。BaseTool 路径改用 `device_ids[]`。
- **`sort_by` 枚举校验**：用 `tunnel.ProcessSortByCPU` / `ProcessSortByMem` 常量而非硬编码字符串，确保与 edge 端解析一致。
- **`TopN` 默认 10**：schema `minimum: 1, maximum: 100`，但代码只默认 0→10，未 clamp 上限（依赖 schema 约束 LLM）。
- **15s 超时**：与 `hostLoadCallTimeout` 同理，frontier round-trip 上限。
- **失败仍回传 DeviceID**：与 `host_load.go` 一致。
- **PR-7 residue**：与 `host_load.go` 一样，graph kernel 不调用，BaseTool 路径是当前活跃路径。

## 8. 注意事项

- **graph kernel 不调用**：本闭包路径是 residue，BaseTool 路径（`host_processes_basetool.go`）是当前活跃路径。
- **`edge_name` 而非 `device_id`**：与 BaseTool 路径不一致。若 LLM 仍走闭包路径会传 `edge_name`，但当前 graph 不路由到此处。
- **无 batch**：本路径单设备，BaseTool 路径支持 batch 16 个 device_id。
- **无 resolver**：直接 `edges.GetByName`，不通过 `DeviceResolver`。
- **`TopN` 未 clamp 上限**：代码只默认 0→10，未校验 >100。依赖 schema `maximum: 100` 约束 LLM。若 LLM 传 200 会直接转发到 edge，edge 端可能拒绝或截断。
- **`processListCallTimeout` 被 BaseTool 复用**：常量定义在本文件，BaseTool 路径引用。cutover PR 退役本文件时需迁移常量。
- **退役时机**：cutover PR 退役本文件时，应同步移除 `Registry.executeGetProcessList` 注册与 `processListCallTimeout` 常量（后者被 BaseTool 复用，需迁移）。
