# `edge_device.go` 技术实现文档

> 源文件：`internal/manager/data/device/store/edge_device.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/device/store`

## 1. 概述

本文件实现 `biz/device.EdgeDeviceRepo` 的 GORM 落地，管理 `edge_devices` 关联表（edge 与 device 的多对多 junction）。核心设计：`Link` 用 `ON CONFLICT DO NOTHING` 让 caller 每次 register 都可无脑调用；`type=Host` 关系唯一约束——Link 前先删同 edge 的其他 host 行，保证一个 edge 只有一个 host device；`LookupEdgeForDevice` 多 edge 共存时按 `id DESC` 取最新 junction 为准。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/device`
- **依赖方向**：被 `internal/manager/biz/device` 装配；依赖 `internal/manager/biz/device`（接口）、`internal/manager/model/device`、`internal/pkg/errs`、`gorm.io/gorm`、`gorm.io/gorm/clause`。

## 3. 关键类型与接口

```go
// EdgeDeviceRepo 是 biz/device.EdgeDeviceRepo 的 GORM 实现。
type EdgeDeviceRepo struct {
    db *gorm.DB
}

var _ biz.EdgeDeviceRepo = (*EdgeDeviceRepo)(nil)
```

## 4. 关键函数与流程

### `NewEdgeDeviceRepo`
- **签名**：`func NewEdgeDeviceRepo(db *gorm.DB) *EdgeDeviceRepo`

### `Link`
- **签名**：`func (r *EdgeDeviceRepo) Link(ctx, edgeID, deviceID uint64, t model.EdgeDeviceRelationType) error`
- **职责**：upsert (edge, device, type) junction 行。
- **流程**：
  1. `edgeID == 0 || deviceID == 0` → `ErrInvalid`
  2. `t == EdgeDeviceRelationHost` → 先删同 edge 的其他 host 行（`device_id <> ?`），保证 host 唯一
  3. `ON CONFLICT (edge_id, device_id, type, delete_marker) DO NOTHING` Create
- **关键约束**：Host 关系唯一——一个 edge 只能有一个 host device；其他 type（如 discovered）允许多对多。
- **幂等性**：重复 Link 同三元组 no-op。

### `Unlink`
- **签名**：`func (r *EdgeDeviceRepo) Unlink(ctx, edgeID, deviceID uint64, t model.EdgeDeviceRelationType) error`
- **职责**：软删 (edge, device, type) 行。幂等——不存在时无错。

### `LookupHostDevice`
- **签名**：`func (r *EdgeDeviceRepo) LookupHostDevice(ctx, edgeID uint64) (uint64, error)`
- **职责**：通过 `type=Host` junction 行解析 `edge_id → host device_id`。
- **多行处理**：`Order("id DESC")` 取最新 junction（理论上 Host 唯一，但防御性取最新）。
- **错误处理**：`gorm.ErrRecordNotFound` → `ErrNotFound`。

### `LookupEdgeForDevice`
- **签名**：`func (r *EdgeDeviceRepo) LookupEdgeForDevice(ctx, deviceID uint64, t model.EdgeDeviceRelationType) (uint64, error)`
- **职责**：解析 `device_id → owning edge_id`（按给定 relation type）。
- **多 edge 共存**：多 agent host 场景下同 device 多个 edge junction，`Order("id DESC")` 取最新创建者为准。
- **错误处理**：`gorm.ErrRecordNotFound` → `ErrNotFound`。

### `ListDevicesForEdge`
- **签名**：`func (r *EdgeDeviceRepo) ListDevicesForEdge(ctx, edgeID uint64) ([]*model.EdgeDevice, error)`
- **职责**：返回该 edge 的所有 junction 行，`Order("id ASC")`。

### `ListEdgesForDevice`
- **签名**：`func (r *EdgeDeviceRepo) ListEdgesForDevice(ctx, deviceID uint64) ([]*model.EdgeDevice, error)`
- **职责**：返回该 device 的所有 junction 行，`Order("id ASC")`。

## 5. 依赖关系

- **内部包**：`internal/manager/biz/device`（接口）、`internal/manager/model/device`（`EdgeDevice` 模型与 `EdgeDeviceRelationType` 常量）、`internal/pkg/errs`
- **外部库**：`gorm.io/gorm`、`gorm.io/gorm/clause`
- **被调用方**：`internal/manager/biz/device` usecase；edge register 流程

## 6. 并发与资源管理

- **无显式锁**：依赖 ON CONFLICT DO NOTHING 与 Host 唯一性约束（应用层 Delete-then-Create）。
- **Host 唯一性竞态**：Link 的 Delete-then-Create 非原子，理论上两并发 Link 可能短暂出现两个 Host 行；但 `LookupHostDevice` 取最新（`id DESC`）保证语义确定性。
- **ctx 透传**：所有方法首参 ctx。

## 7. 设计模式与亮点

- **ON CONFLICT DO NOTHING 幂等 Link**：caller 每次 register 无脑调用，无需先查存在性。
- **Host 唯一性应用层保证**：Link 前删同 edge 其他 host 行，比 DB 唯一索引更灵活（允许 discovered 等其他 type 多对多）。
- **多 edge 共存取最新**：`Order("id DESC")` 让多 agent host 场景语义确定。
- **Unlink 幂等**：不存在时无错，简化 caller 错误处理。

## 8. 注意事项

- **Host 唯一性竞态**：Link 的 Delete-then-Create 非原子；高并发场景需 caller 串行化或 DB 唯一索引兜底。
- **LookupHostDevice 防御性取最新**：理论上 Host 唯一，但防御性 `id DESC` 应对竞态残留。
- **Unlink 软删**：使用 gorm 默认软删（DeletedAt / delete_marker），保留审计；硬删需 Unscoped。
- **ListDevicesForEdge / ListEdgesForDevice 不分页**：返回全部 junction 行；单个 edge/device 的 junction 数量预期小。
- **EdgeDeviceRelationType 枚举**：扩展 type 需同步更新 model 常量与 Link 的 Host 特殊处理逻辑。
