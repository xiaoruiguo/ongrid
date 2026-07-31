# `repo.go` 技术实现文档

> 源文件：`internal/manager/biz/device/repo.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/device`

## 1. 概述

本文件定义 device 主表的持久化契约（`Repo` interface）、`ListFilter` 过滤器、`HostFacts`/`Usage` 子结构。承载 device 行的全部 CRUD + 指纹 upsert + host facts 刷新 + 在线状态翻转 + 软删 + 孤儿回收等操作。实现位于 `internal/manager/data/device`。

## 2. 包信息

- **包名**：`device`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 `devicebiz.Usecase` / `edgebiz.Usecase`（注册时 upsert device）调用；依赖 `model/device`

## 3. 关键类型与接口

```go
type ListFilter struct {
    RolesAny         uint8   // bit mask，与 row 的 roles 位集相交
    RolesUnknownOnly bool    // true 限定 roles==0；与 RolesAny 互斥
    Online           *bool
    Hostname, Name, IPAddress string  // 子串匹配
    Limit, Offset    int
}

type Repo interface {
    FindOrCreateByFingerprint(ctx, seed *model.Device) (*model.Device, error)
    RebindFingerprint(ctx, oldFP, newFP string) error
    UpdateHostFacts(ctx, id uint64, facts HostFacts) error
    UpdateUsage(ctx, id uint64, u Usage) error
    UpdateRoles(ctx, id uint64, roles uint8) error
    UpdateNameDescription(ctx, id uint64, name, description string) error
    SetNodeID(ctx, id, nodeID uint64) error
    MarkOnline(ctx, id uint64) error
    MarkOffline(ctx, id uint64) error
    Get(ctx, id uint64) (*model.Device, error)
    GetMany(ctx, ids []uint64) (map[uint64]*model.Device, error)
    List(ctx, f ListFilter) ([]*model.Device, error)
    Count(ctx) (int64, error)
    Delete(ctx, id uint64) error
    DeleteOfflineWithLinkedEdges(ctx, id uint64) error
    ReconcileOfflineOrphans(ctx) (int64, error)
}

type HostFacts struct {
    Hostname, OS, OSVersion, Arch, KernelVersion string
    CPUCount int
    MemTotalBytes, DiskTotalBytes uint64
    IPAddress string
}

type Usage struct {
    CPUPct, MemPct, DiskPct float32
}
```

Sentinel：`RolesAny` / `RolesUnknownOnly` 互斥；DB CHECK 约束 + `model.RolesAllKnownBits` 共同守护 roles 位集合法性。

## 4. 关键函数与流程

### `FindOrCreateByFingerprint`
- **签名**：`FindOrCreateByFingerprint(ctx, seed *model.Device) (*model.Device, error)`
- **职责**：按 Fingerprint key upsert；存在则返回现有 row，不存在则用 seed 字段创建
- **流程**：纯接口
- **关键约束**：UserID / Hostname / OS 等 seed 字段**仅在初次创建时写入**；后续调用**不覆盖** host facts（用 `UpdateHostFacts` 显式刷新）

### `RebindFingerprint`
- **签名**：`RebindFingerprint(ctx, oldFP, newFP string) error`
- **职责**：原地迁移 device 从 oldFP 到 newFP；保留 device.ID 和历史
- **流程**：纯接口
- **no-op 条件**：oldFP==newFP / 任一为空 / newFP 已存在（new device 已赢，无需迁移）

### `UpdateHostFacts`
- **签名**：`UpdateHostFacts(ctx, id uint64, facts HostFacts) error`
- **职责**：覆盖 Hostname / OS / Arch / KernelVersion / CPUCount / MemTotalBytes / DiskTotalBytes / OSVersion / IPAddress
- **流程**：edge register 流程在 fresh payload 到达时调用，保持最新 facts

### `UpdateUsage`
- **签名**：`UpdateUsage(ctx, id uint64, u Usage) error`
- **职责**：刷新实时使用率 gauge（CPU/Mem/Disk %）
- **流程**：metric ingest 路径调用，device 列表显示实时负载无需 JOIN host_metrics

### `UpdateRoles`
- **签名**：`UpdateRoles(ctx, id uint64, roles uint8) error`
- **职责**：写操作员分配的 device-roles 位掩码
- **流程**：调用方应已掩到 `model.RolesAllKnownBits`；DB CHECK 是第二道防线

### `MarkOnline / MarkOffline`
- **签名**：`MarkOnline/MarkOffline(ctx, id uint64) error`
- **职责**：翻转 device 级 online flag + 时间戳；由 edge online/offline 回调触发

### `Get / GetMany / List / Count`
- **签名**：读路径
- **职责**：Get 按 id；GetMany 批量加载（缺失 id 不出现在返回 map）；List 按 f 过滤（id DESC）；Count 总非软删数
- **流程**：GetMany 缺失不报错；List 默认按 id DESC

### `Delete`
- **签名**：`Delete(ctx, id uint64) error`
- **职责**：软删 device；**不触碰 junction 行**（调用方应先删 junction）
- **流程**：纯接口

### `DeleteOfflineWithLinkedEdges`
- **签名**：`DeleteOfflineWithLinkedEdges(ctx, id uint64) error`
- **职责**：单事务删除 offline device + 关联的 Edge 身份
- **流程**：纯接口
- **关键约束**：**必须拒绝 online device** 返回 `ErrConflict`，避免删 live host

### `ReconcileOfflineOrphans`
- **签名**：`ReconcileOfflineOrphans(ctx) (int64, error)`
- **职责**：把 online=true 但所有关联 edge 都不在线的 device 翻回 offline
- **流程**：纯接口；返回翻转行数
- **用途**：周期性由 presence reconciler 跑；治愈"幽灵 device"（edge 被删或 host 改指纹重新注册导致 device 永远 online）

## 5. 依赖关系

- **内部包**：`model/device`
- **被调用方**：`devicebiz.Usecase`（CRUD + reconciler）、`edgebiz.Usecase`（register 时 FindOrCreateByFingerprint + UpdateHostFacts + MarkOnline）
- **实现方**：`internal/manager/data/device`（sqlite / mysql）

## 6. 并发与资源管理

- **纯接口**：无共享状态；并发安全由实现负责
- **DeleteOfflineWithLinkedEdges 事务**：单事务删 device + edges，避免半删状态
- **ctx 透传**：所有方法第一参 context

## 7. 设计模式与亮点

- **指纹作为业务主键**：`FindOrCreateByFingerprint` 让同一硬件多次注册保持同一 row；ID 稳定便于历史追溯
- **`RebindFingerprint` 原地迁移**：从 legacy HostID-derived 指纹迁移到 v3 硬件指纹时保留 device.ID；no-op 条件覆盖各种边界
- **host facts 与 row 分离**：`UpdateHostFacts` 独立方法；FindOrCreateByFingerprint 不覆盖 facts，避免注册竞态覆盖更好数据
- **`RolesAny` sargable 设计**：bit mask 翻译成有限 IN-list（`model.MatchingRoleValues`）保持 SQL sargable
- **`DeleteOfflineWithLinkedEdges` 拒绝 online**：事务级守护，防止删 live host 的访问键
- **`ReconcileOfflineOrphans` 自愈**：注释明示"per-event MarkOnline/MarkOffline 路径看不到已不存在的 edge"——reconciler 是收敛保证
- **GetMany 缺失不报错**：调用方需处理 no-row case；注释明示

## 8. 注意事项

- **FindOrCreateByFingerprint 不覆盖 facts**：调用方需显式 UpdateHostFacts 刷新；edge register 流程同时调两者
- **Delete 不清 junction**：调用方应先 Unlink；否则 junction 留下指向软删 device 的悬空引用
- **DeleteOfflineWithLinkedEdges 拒绝 online**：调用方需先 MarkOffline 或检查 Online 字段
- **UpdateRoles 调用方需掩 known bits**：DB CHECK 是二线，业务层应主动 mask
- **ReconcileOfflineOrphans 周期性调用**：presence reconciler 启动 + ticker；不在此接口
- **GetMany 返回 map**：缺失 id 静默 absent；调用方必须处理 no-row case
- **ListFilter.RolesAny / RolesUnknownOnly 互斥**：调用方设其一，不设两个；行为未定义若同设
- **ListFilter 默认排序 id DESC**：最新创建在前；分页用 Limit/Offset
