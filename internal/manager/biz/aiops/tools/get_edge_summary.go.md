# `get_edge_summary.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/get_edge_summary.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件实现 `get_edge_summary` 工具（闭包路径，挂在 `Registry.executeGetEdgeSummary`）：一次性返回单台 edge 的快照——注册 metadata + best-effort 实时 host_load（cpu/mem/load）+ 24h 内 incidents（任何状态，severity ≥ warning）。30s 超时。每个子调用 best-effort：host_load 失败（edge offline）则该字段 nil，仍返回 meta + incidents。`device_id` 是首选标识（匹配 @-mention chip 与 Prom label），`edge_id` / `edge_name` 是 legacy alias，executor 通过 device→edge junction 把两者归一到同一 edge 行。`resolveEdgeForDevice` 实现 4 步解析（device by id → junction → edge by id legacy fallback → device by name → edge by name）。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 Registry 闭包路径调用；依赖 `alertbiz.Usecase`（ListIncidents）、`devicebiz.Usecase`（Get / Links / List）、`edgebiz.Usecase`（Get / GetByName）、`devicemodel.DecodeRoles`、`edgemodel.StatusOnline`、`tunnel.MethodGetHostLoad`。与 `get_edge_summary_basetool.go`（BaseTool 镜像）并存。

## 3. 关键类型与接口

```go
type GetEdgeSummaryArgs struct {
    DeviceID   uint64 `json:"device_id,omitempty"`   // 首选
    DeviceName string `json:"device_name,omitempty"`
    EdgeID     uint64 `json:"edge_id,omitempty"`     // legacy alias for device_id
    EdgeName   string `json:"edge_name,omitempty"`   // legacy alias for device_name
}

type EdgeSummaryIncidentRow struct {
    ID, Title, Severity, Status, Rule, RuleName string/int
    FirstFiredAt, LastFiredAt                   time.Time
}

const edgeSummaryCallTimeout = 30 * time.Second
```

`resolveEdgeForDevice(ctx, r *Registry, deviceID, deviceName)` 是包级 helper，被本工具与 `host_load.go` / `host_processes.go` / `host_files_*` 等 ScopeHost 工具复用（注：PR-9 后部分工具改用 `DeviceResolver` 接口，但本闭包路径仍用 `resolveEdgeForDevice`）。

## 4. 关键函数与流程

```go
func (r *Registry) executeGetEdgeSummary(ctx, args json.RawMessage) (ExecuteResult, error)
func resolveEdgeForDevice(ctx, r *Registry, deviceID uint64, deviceName string) (*edgemodel.Edge, error)
func devicebizListByName(name string) devicebiz.ListFilter
```

**`executeGetEdgeSummary` 流程**：
1. 守门 `r.edges != nil`。
2. Unmarshal → `GetEdgeSummaryArgs`。
3. **Coalesce legacy alias**：`DeviceID == 0 && EdgeID != 0` → `DeviceID = EdgeID`；`DeviceName == "" && EdgeName != ""` → `DeviceName = EdgeName`。
4. 校验 `DeviceID == 0 && DeviceName == ""` → 报错。
5. `context.WithTimeout(ctx, edgeSummaryCallTimeout=30s)`。
6. `resolveEdgeForDevice(callCtx, r, DeviceID, DeviceName)` → `*edgemodel.Edge`。
7. **Roles 装饰**：`r.devices.Get(*edge.DeviceID)` → `devicemodel.DecodeRoles(d.Roles)`。missing device → `roles = []string{}`（非 nil）。
8. 构造 `out["edge"] = {id, device_id, name, status, roles, last_seen_at, created_at}`。
9. **Best-effort host_load**：仅当 `r.caller != nil && edge.Status == StatusOnline` 时，`caller.Call(edge.ID, MethodGetHostLoad, GetHostLoadRequest{})` → unmarshal `GetHostLoadResponse` → `out["host_load"] = resp`。任一 step err 静默跳过。
10. **Recent incidents**：`r.alertUC.ListIncidents(IncidentFilter{DeviceID: &edgeID, Limit: 100})`（无 Status filter，要所有 lifecycle 状态）。`cutoff = now - 24h`，过滤 `inc.LastFiredAt.Before(cutoff)` 跳过，`inc.Severity == "info"` 跳过。转 `[]EdgeSummaryIncidentRow` → `out["recent_incidents"]`。
11. `out["plugin_status"] = "unsupported"`（注释明示 PluginConfigUC 未 wire，显式暴露 gap 防 LLM 误称覆盖）。
12. Marshal 返回 `ExecuteResult{ResultJSON: body, DeviceID: &edge.ID}`。

**`resolveEdgeForDevice` 4 步解析**：
1. **By id**：`tryEdgeForDeviceID(deviceID)`——`devices.Get(id)` → `devices.Links().LookupEdgeForDevice(id, EdgeDeviceRelationHost)` → `edges.Get(eid)`。任一失败返回 nil/err。
2. **By id legacy fallback**：若 step 1 失败，`edges.Get(ctx, deviceID)` 把 deviceID 当 edge_id 查（兼容 device split 前的 prompt）。
3. **By name**：`devices.List(ListFilter{Name: deviceName, Limit: 5})` → 对每个 candidate 跑 `tryEdgeForDeviceID`。
4. **By name legacy fallback**：`edges.GetByName(ctx, deviceName)`。
5. 都没命中 → 报错带 hint "try query_devices first to list available device ids"。

## 5. 依赖关系

- **alertbiz.Usecase**：`ListIncidents(ctx, IncidentFilter{DeviceID, Limit})`。`AlertUsecase` 接口在 `correlate_incident.go` 定义。
- **devicebiz.Usecase**：`Get` / `Links()` / `List(ListFilter{Name, Limit})`。
- **edgebiz.Usecase**：`Get(ctx, id)` / `GetByName(ctx, name)`。
- **devicemodel**：`DecodeRoles([]byte) []string`、`EdgeDeviceRelationHost` 常量。
- **edgemodel**：`StatusOnline` 常量。
- **tunnel**：`GetHostLoadRequest` / `GetHostLoadResponse` / `MethodGetHostLoad`。
- **Caller**：`Call(ctx, edgeID, method, body)` 派发 host_load 请求。

## 6. 并发与资源管理

- **无锁、无共享可变状态**：`Registry` 字段不变，`executeGetEdgeSummary` 内变量局部。多 goroutine 可并发调用。
- **无 goroutine**：串行 `resolveEdgeForDevice` → `devices.Get` (roles) → `caller.Call` (host_load) → `alertUC.ListIncidents`。30s 超时覆盖全部。注释明示 `edgeSummaryCallTimeout` 必须 > `hostLoadCallTimeout` 以防慢 edge 拖垮 outer ctx 在 incident 查询前。
- **best-effort 子调用**：host_load 的 `marshalErr` / `callErr` / `unmarshal` err 全部静默吞掉，incidents 的 `listErr` 也静默——只有 `resolveEdgeForDevice` 失败才上抛 error。这保证 "edge 能识别但某子查询失败" 仍返回部分摘要。

## 7. 设计模式与亮点

- **One-shot stitch**：一次调用拼合 edge meta + host_load + 24h incidents + plugin_status，省 LLM 多轮调用。比 `get_host_load` + `get_incident_detail` 各自调用更省 roundtrip。
- **Legacy alias coalesce**：`device_id` / `edge_id` / `device_name` / `edge_name` 四种输入归一到 device_id/device_name，兼容 device split 前后的 prompt。注释明示 `edge_id` 是 "accepted for back-compat"。
- **`resolveEdgeForDevice` 4 步解析**：device by id → edge by id legacy → device by name → edge by name legacy。每步失败带 actionable hint "try query_devices first"，让 LLM 有下一步可走。
- **best-effort 子调用**：host_load 失败（offline edge）→ `host_load=null`；incidents 失败 → 无 `recent_incidents` 字段。LLM 仍能基于 meta 推理。这与 `correlate_incident` 的 `Skipped` map 设计同源——partial result 优于 error。
- **24h cutoff + severity ≥ warning**：注释明示 "must include acknowledged / silenced / resolved rows, not just currently-open ones"——目标是 "what's been firing on this host"，所有 lifecycle 状态都有价值。
- **`plugin_status = "unsupported"` 显式暴露 gap**：不假装覆盖，防 LLM 误称。注释明示 "When PluginConfigUC gets wired in, swap this for the live list-by-edge call"。
- **`ExecuteResult.DeviceID = &edge.ID`**：把 edge_id 回传给上层 graph/audit，便于关联审计与 device 维度统计。

## 8. 注意事项

- **`resolveEdgeForDevice` 与 `DeviceResolver` 接口并存**：PR-9 引入 `DeviceResolver`（`device_resolver.go`）统一 resolution 规则，但本闭包路径仍用 `resolveEdgeForDevice`。两者规则应保持一致；未来应让 `resolveEdgeForDevice` 委托给 `DeviceResolver` 避免 drift。
- **`plugin_status` 是占位**：当前固定 "unsupported"。若 PluginConfigUC wire 后需替换为真实状态查询。
- **`edges.List` 未在本工具用**：`resolveEdgeForDevice` 的 by-name 路径用 `devices.List` + `edges.GetByName`，不涉及 `edges.List`。与 `get_edge_summary_basetool.go::allEdgeDeviceIDs` 的 `edges.List` 不同。
- **24h cutoff 用 `LastFiredAt` 而非 `FirstFiredAt`**：`inc.LastFiredAt.Before(cutoff)` 跳过——意味着 "24h 内未再触发" 的老 incident 会被排除。若想看 "24h 内首次触发的" 应改用 `FirstFiredAt`。
- **`Limit: 100` 截断**：超过 100 条 incident 的 edge 会丢数据。当前规模够用；若需更多应分页。
- **`severity == "info"` 跳过**：仅保留 warning/error/critical。info 级别被认为是噪音。
- **`edge.Status == StatusOnline` 才查 host_load**：offline edge 跳过 tunnel 调用，避免必然超时。`host_load=null` 让 LLM 知道 "offline，无实时负载"。
- **闭包路径独有**：本文件是 `Registry.executeGetEdgeSummary`。BaseTool 形态在 `get_edge_summary_basetool.go`，后者支持 batch（`device_ids[]`）。
