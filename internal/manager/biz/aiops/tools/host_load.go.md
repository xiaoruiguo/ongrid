# `host_load.go` 技术实现文档

> 源文件：`internal/manager/biz\aiops/tools/host_load.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件实现 `get_host_load` 工具（闭包路径，挂在 `Registry.executeGetHostLoad`）：返回单台 edge host 的当前 CPU 百分比、内存百分比、1/5/15 分钟 load average。通过 `edge_name` 解析 `edge.ID`，`Caller.Call` 派发 `tunnel.MethodGetHostLoad` reverse call 到 frontier。15s 超时。**注意**：本闭包路径是 PR-7 residue，graph kernel 不再调用它——`host_load_basetool.go` 的 BaseTool 形态（batch `device_ids[]`）是当前活跃路径。本文件保留构建直到 cutover PR 退役它。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 Registry 闭包路径注册（但 graph kernel 不调用）；依赖 `edgebiz.Usecase`（`GetByName`）、`pkg/tunnel`（`GetHostLoadRequest/Response` / `MethodGetHostLoad`）、`Caller`。与 `host_load_basetool.go`（BaseTool 镜像，batch）并存。

## 3. 关键类型与接口

```go
type GetHostLoadArgs struct {
    EdgeName string `json:"edge_name"` // 必填
}

const hostLoadCallTimeout = 15 * time.Second
```

依赖 `Registry.edges.GetByName(ctx, name)` 与 `Registry.caller.Call(ctx, edgeID, method, body)`。

## 4. 关键函数与流程

```go
func (r *Registry) executeGetHostLoad(ctx, args json.RawMessage) (ExecuteResult, error)
```

流程：
1. Unmarshal → `GetHostLoadArgs`；`EdgeName == ""` → 报错。
2. `r.edges.GetByName(ctx, in.EdgeName)` → `*edgemodel.Edge`。err 上抛 "resolve edge: %w"。
3. Marshal `tunnel.GetHostLoadRequest{}`（空请求体）。
4. `context.WithTimeout(ctx, hostLoadCallTimeout=15s)`。
5. `r.caller.Call(callCtx, edge.ID, tunnel.MethodGetHostLoad, body)` → `respBody`。
6. Unmarshal `tunnel.GetHostLoadResponse`，Marshal 返回 `ExecuteResult{ResultJSON: out, DeviceID: &edge.ID}`。
7. 任一 step 失败仍回传 `ExecuteResult{DeviceID: &edge.ID}` + error（便于 audit 关联 device）。

## 5. 依赖关系

- **edgebiz.Usecase**：`GetByName(ctx, name)` 按 name 解析 edge。
- **Caller**：`Call(ctx, edgeID, method, body)` 派发到 frontier tunnel。
- **pkg/tunnel**：`GetHostLoadRequest`（空体）/ `GetHostLoadResponse`（含 CPU/mem/load 字段）/ `MethodGetHostLoad` 常量。
- 不依赖 devicebiz / alertbiz / prom / log / trace。

## 6. 并发与资源管理

- **无锁、无共享可变状态**：`Registry` 字段不变，`executeGetHostLoad` 内变量局部。多 goroutine 可并发调用。
- **无 goroutine**：单次 `GetByName` + 单次 `Caller.Call`，15s 超时覆盖。
- **`ExecuteResult.DeviceID` 始终回传**：即便 step 失败也回传 `&edge.ID`，便于上层 graph/audit 按 device 关联。

## 7. 设计模式与亮点

- **`edge_name` 而非 `device_id`**：本闭包路径用 `edge_name` 解析，是 device split 前的设计。BaseTool 路径（`host_load_basetool.go`）改用 `device_ids[]`，与 @-mention chip / Prom label 一致。
- **15s 超时**：frontier round-trip 上限，防 edge hang 拖垮 agent loop。
- **失败仍回传 DeviceID**：注释明示即便 marshal/dispatch/decode 失败也 `ExecuteResult{DeviceID: &edge.ID}`，让 audit 能关联到具体 device。
- **PR-7 residue**：注释明示 "graph kernel doesn't call it; it's a PR-7 residue that we keep building until the cutover PR retires it"。当前活跃路径是 BaseTool 形态。

## 8. 注意事项

- **graph kernel 不调用**：本闭包路径是 residue，BaseTool 路径（`host_load_basetool.go`）是当前活跃路径。LLM 通过 BaseTool registry 调用，不会走到 `Registry.executeGetHostLoad`。
- **`edge_name` 而非 `device_id`**：与 BaseTool 路径不一致（后者用 `device_ids[]`）。若 LLM 仍走闭包路径会传 `edge_name`，但当前 graph 不路由到此处。
- **无 batch**：本路径单设备，BaseTool 路径支持 batch 16 个 device_id。
- **无 resolver**：直接 `edges.GetByName`，不通过 `DeviceResolver`。与 BaseTool 路径（用 `deviceResolverAdapter`）不同。
- **15s 超时与 BaseTool 一致**：`hostLoadCallTimeout` 常量被 BaseTool 路径复用，确保两路径 per-call 超时一致。
- **退役时机**：cutover PR 退役本文件时，应同步移除 `Registry.executeGetHostLoad` 注册与 `hostLoadCallTimeout` 常量（后者被 BaseTool 复用，需迁移）。
