# `edge_device.go` 技术实现文档

> 源文件：`internal/manager/biz/device/edge_device.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/device`

## 1. 概述

本文件定义 `EdgeDeviceRepo` 接口 —— edge 与 device 之间 M:N 关联表（`edge_devices`）的持久化契约。承载两条核心通信路径：**Push**（edge → manager 的 metric/log/trace 通过 edge_id 反查 host device_id 打标）与 **Pull**（manager 通过 device_id 反查 owning edge_id 发起 tunnel RPC）。实现位于 `internal/manager/data/device/store`。

## 2. 包信息

- **包名**：`device`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 edge biz / metric ingest path / aiops 工具调用；依赖 `model/device`（`EdgeDeviceRelationType` 等）

## 3. 关键类型与接口

```go
type EdgeDeviceRepo interface {
    Link(ctx, edgeID, deviceID uint64, t model.EdgeDeviceRelationType) error
    Unlink(ctx, edgeID, deviceID uint64, t model.EdgeDeviceRelationType) error
    LookupHostDevice(ctx, edgeID uint64) (uint64, error)
    LookupEdgeForDevice(ctx, deviceID uint64, t model.EdgeDeviceRelationType) (uint64, error)
    ListDevicesForEdge(ctx, edgeID uint64) ([]*model.EdgeDevice, error)
    ListEdgesForDevice(ctx, deviceID uint64) ([]*model.EdgeDevice, error)
}
```

`model.EdgeDeviceRelationType` 关键值：`EdgeDeviceRelationHost`（Type=Host 是常见情形）。

## 4. 关键函数与流程

### `Link`
- **签名**：`Link(ctx, edgeID, deviceID uint64, t model.EdgeDeviceRelationType) error`
- **职责**：upsert (edge_id, device_id, type) 行
- **流程**：纯接口；实现负责幂等（重复同 triple 是 no-op）
- **错误处理**：由实现定义

### `Unlink`
- **签名**：`Unlink(ctx, edgeID, deviceID uint64, t model.EdgeDeviceRelationType) error`
- **职责**：删除 (edge_id, device_id, type) 行
- **流程**：纯接口；幂等（无匹配行返回 nil）
- **错误处理**：由实现定义

### `LookupHostDevice`
- **签名**：`LookupHostDevice(ctx, edgeID uint64) (uint64, error)`
- **职责**：**Push 路径核心** —— 解析 edge → host device_id；edge tunnel 来的所有 metric/log/trace 都按 host device_id 打标
- **流程**：纯接口
- **错误处理**：edge 尚无 Type=Host 关联（注册竞态）→ 返回 `ErrNotFound`

### `LookupEdgeForDevice`
- **签名**：`LookupEdgeForDevice(ctx, deviceID uint64, t model.EdgeDeviceRelationType) (uint64, error)`
- **职责**：**Pull 路径核心** —— 解析 device → owning edge_id；tool 想对 device 发 tunnel RPC 时需 edge_id 寻址 geminio session
- **流程**：纯接口；Type=Host 是常见 case
- **错误处理**：由实现定义

### `ListDevicesForEdge`
- **签名**：`ListDevicesForEdge(ctx, edgeID uint64) ([]*model.EdgeDevice, error)`
- **职责**：枚举 edge 关联的所有 device（任意 type）；edge 详情页"this edge sees N devices"面板用
- **流程**：纯接口

### `ListEdgesForDevice`
- **签名**：`ListEdgesForDevice(ctx, deviceID uint64) ([]*model.EdgeDevice, error)`
- **职责**：枚举 device 关联的所有 edge（任意 type）；device 详情页"this device is seen by N edges"面板用
- **流程**：纯接口

## 5. 依赖关系

- **内部包**：`model/device`（`EdgeDevice`、`EdgeDeviceRelationType`、`EdgeDeviceRelationHost`）
- **被调用方**：`devicebiz.Usecase`（host lookup / link 管理）、`edgebiz.Usecase`（注册时建 host 链接、删除时检查 last edge）、metric ingest path
- **实现方**：`internal/manager/data/device/store`

## 6. 并发与资源管理

- **纯接口**：无共享状态；并发安全由实现负责（DB 事务）
- **ctx 透传**：所有方法第一参 context

## 7. 设计模式与亮点

- **M:N junction 抽象**：edge 与 device 是 M:N（一台主机可被多个 edge 看到，一个 edge 也能挂多个 device 角色）；关联表 + Type 区分关系类型
- **Push / Pull 双向 lookup**：两个方向的 lookup 都暴露；用 Type 参数过滤关系类型，host 是主用例
- **Link/Unlink 双向幂等**：注释明示"second call with same triple is no-op"；"no row matched returns nil" —— 删除侧也幂等
- **接口隔离**：独立于 `Repo`（device 主表）；edge 包通过 `devicebiz.EdgeDeviceRepo` 窄接口消费，不 import device 全集
- **注释明示通信路径**：文件 doc block 画出 Push/Pull 两条路径的数据流，方便维护者理解为何要这两个 lookup

## 8. 注意事项

- **Type=Host 是常见但非唯一**：未来 Type 可扩展（如 Kubernetes controller edge）；调用方传 Type 不能假设
- **LookupHostDevice 竞态**：edge 注册流程中可能先于 junction 写入到达；Push 路径必须处理 ErrNotFound
- **不软删**：junction 行通过 Unlink 物理删除；device 主表软删时调用方需先 Unlink 清理 junction
- **EdgeDeviceRepo nil-safe**：`devicebiz.Usecase` 接受 nil links；junction-aware 方法返回 `ErrNotWiredYet`
- **不在接口里**：事务边界（如"删 device + 删 junction"原子性）由 device 主 Repo 的 `DeleteOfflineWithLinkedEdges` 承担，不在此接口
