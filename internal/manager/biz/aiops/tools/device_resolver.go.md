# `device_resolver.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/device_resolver.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件定义 `DeviceResolver` 接口与生产实现 `junctionDeviceResolver`：把 chat 输入的 `device_id`（即 SPA @-mention chip / Prom 样本上的 id）翻译成 tunnel 可寻址的 `edge_id`。是所有 `ScopeHost` 工具（host_files / host_load / host_processes 等）与 `skill_bridge` 的共享 seam。PR-9 把原先 copy-paste 在 `host_files_basetool.go::usecaseDeviceResolver` 与 `skill_bridge.go::Registry.resolveEdgeForDeviceID` 的两份实现合并成单一可测 helper，确保 resolution 规则变更只需改一处。

解析规则（与历史行为字节级一致）：
1. **Junction 查询**：`devices.Links().LookupEdgeForDevice(type=host)` 命中 → 返回。
2. **Device 行存在但无 junction 链接**：返回 `(0, nil)`，让调用方友好提示 "device has no host link"。
3. **Legacy 回退**：把输入当 raw edge_id 调 `edges.Get`，命中 → 返回。兼容 device split 之前的 prompt。
4. 都没命中：`(0, nil)`。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 host_files / host_load / host_processes / skill_bridge 等所有 ScopeHost 工具消费；依赖 `devicebiz.Usecase`（junction + Get）、`edgebiz.Usecase`（legacy 回退）、`devicemodel.EdgeDeviceRelationHost`。

## 3. 关键类型与接口

```go
type DeviceResolver interface {
    // ResolveEdgeID 把 device_id (或 legacy edge_id) 解析为 host edge_id。
    // 当 device 存在但既无 host-edge 链接也无回退 edge 行匹配时返回 (0, nil)，
    // 调用方应据此输出 "no host link" 友好错误。
    ResolveEdgeID(ctx context.Context, deviceID uint64) (uint64, error)
}

// 生产实现，struct 不导出，强制走 NewDeviceResolver 工厂。
type junctionDeviceResolver struct {
    devices *devicebiz.Usecase
    edges   *edgebiz.Usecase
}
```

`junctionDeviceResolver` 不导出是为了后续可替换底层 repo（如加 cache）而不动调用方。

## 4. 关键函数与流程

```go
func NewDeviceResolver(devices *devicebiz.Usecase, edges *edgebiz.Usecase) DeviceResolver
func (r junctionDeviceResolver) ResolveEdgeID(ctx, deviceID uint64) (uint64, error)
```

`ResolveEdgeID` 流程：
1. `deviceID == 0` → 直接 `(0, nil)`（不视为错误）。
2. 若 `r.devices != nil`：
   - 若 `r.devices.Links()` 非空，`LookupEdgeForDevice(ctx, deviceID, EdgeDeviceRelationHost)`。`err == nil && eid != 0` → 返回。**注意**：`err != nil` 在此被视为 "未链接" 而非 DB 错误上抛——resolver 负责消歧路由，不负责 surface DB 错误（注释明确）。
   - 否则 `r.devices.Get(ctx, deviceID)`：若 device 行存在，返回 `(0, nil)`（device 存在但无 junction → 不做 fallback，避免误路由到陌生 edge）。
3. 若 `r.edges != nil`：`edges.Get(ctx, deviceID)` 命中 → 返回 `edge.ID`（legacy 回退，把 deviceID 当 edge_id）。
4. 都没命中 → `(0, nil)`。

## 5. 依赖关系

- **devicebiz.Usecase**：`Links()` 返回 junction 查询器（`LookupEdgeForDevice`），`Get(ctx, id)` 返回 device 行用于存在性检查。
- **edgebiz.Usecase**：`Get(ctx, id)` 用于 legacy 回退（id 直接当 edge_id 查）。
- **devicemodel.EdgeDeviceRelationHost**：junction 查询的 relation type 常量，确保只匹配 host 关系（不是其他 relation type 如 db / cache）。
- 任一 usecase 为 nil 都会优雅降级：nil devices 跳过 junction + device 行检查；nil edges 跳过 legacy 回退。

## 6. 并发与资源管理

- **无锁、无共享可变状态**：`junctionDeviceResolver` 仅持有不变 usecase 指针，多个 goroutine 可并发调用 `ResolveEdgeID`。
- **无 ctx 超时**：本函数不自己 `WithTimeout`，由调用方（host_*_basetool）按需加；DB 查询本身受 ctx 控制。
- **无 goroutine**：纯同步 IO。

## 7. 设计模式与亮点

- **接口在消费方定义**：`DeviceResolver` 定义在 tools 包（消费方），不在 devicebiz/edgebiz 包，符合架构红线"接口在消费方定义"。
- **单点 seam**：4 步规则的任何修改（如未来加 cache 层、加 "prefer junction, then row, then legacy" 的新策略）只改 `ResolveEdgeID` 一处，所有 ScopeHost 工具同时受益——这是 PR-9 的核心动机。
- **nil-safe 降级**：构造时 `devices` 或 `edges` 为 nil 不 panic，对应分支被跳过；测试可只注入需要的 usecase。
- **错误语义分层**：DB 错误（`err != nil` from LookupEdgeForDevice）被降级为 "未链接"，让 resolver 只回答路由问题；上层若需区分 "DB 故障" vs "确实无链接" 需另查。这是有意的 trade-off，注释明确说明。
- **(0, nil) 双义**：device 存在但无 link / device 不存在 / legacy edge 不存在都返回 (0, nil)，调用方需用 "device has no host link" 这种友好措辞覆盖所有情形。

## 8. 注意事项

- **不区分 "DB 故障" 与 "无链接"**：`LookupEdgeForDevice` 返回 err 时被当作 "未链接" 吞掉，DB 真正故障下也会返回 (0, nil)；上层无法感知 DB 错误。如需精确区分需扩展接口（返回第三态或上抛 err），但目前 ScopeHost 工具的 UX 不需要——友好提示即可。
- **Legacy 回退是兼容性包袱**：device split 之前的 prompt 把 edge_id 直接当 device_id 传，第 3 步 `edges.Get(deviceID)` 让这些老 prompt 仍能工作。新 prompt 应统一用 device_id。若未来想下线 legacy 回退，把 `r.edges` 注入 nil 即可（优雅降级生效）。
- **deviceID == 0 是合法输入**：直接返回 (0, nil)，调用方应在调用前/后自行判断 0 是否可接受。
- **构造函数可注入 nil**：`NewDeviceResolver(nil, nil)` 返回的 resolver 对任何非零 deviceID 都走完所有分支返回 (0, nil)——可用于"什么都没配"的测试场景。
- **host_files_basetool.go 的 usecaseDeviceResolver 与 skill_bridge.go 的 Registry.resolveEdgeForDeviceID 现在是薄包装**：它们委托给 `DeviceResolver`，不应再有独立 resolution 逻辑。
