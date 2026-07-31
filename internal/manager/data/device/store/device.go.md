# `device.go` 技术实现文档

> 源文件：`internal/manager/data/device/store/device.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/device/store`

## 1. 概述

本文件实现 `biz/device.Repo` 的 GORM 落地，覆盖 device 实体的生命周期：fingerprint 查找或创建、host facts 刷新、usage gauges、roles 位掩码、node_id 链接、online/offline 状态机、列表与计数、软删与级联硬删。核心设计：`FindOrCreateByFingerprint` 用 `ON CONFLICT DO NOTHING` + follow-up select 跨方言避免 DB 级锁；`ReconcileOfflineOrphans` 用 raw SQL 相关子查询把无 live edge 的 online device 翻为 offline；`DeleteOfflineWithLinkedEdges` 单事务级联硬删 + 凭据 tombstone。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/device`
- **依赖方向**：被 `internal/manager/biz/device` 装配；依赖 `internal/manager/biz/device`（接口与 `HostFacts` / `Usage` / `ListFilter`）、`internal/manager/model/device`、`internal/manager/model/edge`、`internal/pkg/errs`、`gorm.io/gorm`、`gorm.io/gorm/clause`。

## 3. 关键类型与接口

```go
// Repo 是 biz/device.Repo 的 GORM 实现。
type Repo struct {
    db *gorm.DB
}

var _ biz.Repo = (*Repo)(nil)
```

## 4. 关键函数与流程

### 构造与查找

#### `NewRepo`
- **签名**：`func NewRepo(db *gorm.DB) *Repo`

#### `FindOrCreateByFingerprint`
- **签名**：`func (r *Repo) FindOrCreateByFingerprint(ctx, seed *model.Device) (*model.Device, error)`
- **职责**：按 fingerprint 查找或创建 device。
- **流程**：
  1. `seed == nil || Fingerprint == ""` → `ErrInvalid`
  2. `ON CONFLICT (fingerprint, delete_marker) DO NOTHING` Create
  3. follow-up `Where("fingerprint = ?").First(&out)`
  4. `gorm.ErrRecordNotFound` → `ErrNotFound`
- **关键约束**：ON CONFLICT DO NOTHING 跨 MySQL/SQLite 工作，无需 DB 级锁。

#### `RebindFingerprint`
- **签名**：`func (r *Repo) RebindFingerprint(ctx, oldFP, newFP string) error`
- **职责**：把 device 从 oldFP 原地改绑到 newFP。同 row 只改 fingerprint 列，device.ID / junction / history 全部保留。
- **流程**：
  1. `oldFP == "" || newFP == "" || oldFP == newFP` → no-op
  2. Count newFP 已存在 → no-op（v2 device 已赢）
  3. `Where("fingerprint = ?", oldFP).Update("fingerprint", newFP)`
- **幂等性**：oldFP 不存在时 update 0 行，无错。

### 字段更新

#### `UpdateHostFacts`
- 覆盖 hostname / os / os_version / arch / kernel_version / cpu_count / mem_total_bytes / disk_total_bytes / ip_address。**不含 online / last_seen_at**（独立生命周期，由 MarkOnline / MarkOffline 管）。

#### `UpdateUsage`
- 刷新 live usage gauges：cpu_usage_pct / mem_usage_pct / disk_usage_pct。

#### `UpdateRoles`
- 写 operator 分配的 role 位掩码。

#### `SetNodeID`
- 写 `Device.NodeID`（链接 topology nodes 表）。
- **关键约束**：`WHERE id = ? AND (node_id IS NULL OR node_id <> ?)` 跳过已相同值，让 NodeMirror hook 每次 register 安全调用而无冗余写。
- **歧义消解**：`RowsAffected == 0` 时 Count 一次区分"行不存在" vs "已相同值"。

#### `UpdateNameDescription`
- 写 operator 可编辑的 display 字段。

### 状态机

#### `MarkOnline` / `MarkOffline`
- `MarkOnline`：`online = true` + bump `last_seen_at = now`。
- `MarkOffline`：`online = false`，**不动 last_seen_at**（让 caller 仍可看"最后联系"时间）。

#### `ReconcileOfflineOrphans`
- **签名**：`func (r *Repo) ReconcileOfflineOrphans(ctx) (int64, error)`
- **职责**：把无 live linked edge 的 online device 翻为 offline。
- **实现**：raw SQL 相关子查询 NOT EXISTS over `edge_devices JOIN edges`，显式 `delete_marker = 0` 过滤（不依赖 gorm soft-delete scope），跨 MySQL/SQLite 可移植。
- **返回**：heal 行数。

### 列表与计数

#### `Get` / `GetMany`
- `Get`：按 PK 取；`gorm.ErrRecordNotFound` → `ErrNotFound`。
- `GetMany`：批量按 id 加载，missing id 不在 map 中。空 ids → 空 map。

#### `List`
- 按 `biz.ListFilter` 过滤：RolesUnknownOnly / RolesAny / Online / Hostname(LIKE) / Name(LIKE) / IPAddress(LIKE prefix) / Limit / Offset。
- **RolesAny**：`roles IN ?` + `model.MatchingRoleValues(f.RolesAny)` 展开位掩码为合法值集合。
- Order `id DESC`。

#### `Count`
- 非软删 device 数。

### 历史清理

#### `ListDeletedWithNodeID`
- **签名**：`func (r *Repo) ListDeletedWithNodeID(ctx, limit int) ([]*model.Device, error)`
- **职责**：返回软删 device 但其 linked topology node 仍 live 的行。boot 时 device usecase 探测清理历史 topology 泄漏。
- **关键**：`Unscoped()` 绕过软删 scope；JOIN `nodes ON nodes.id = devices.node_id AND nodes.deleted_at IS NULL`。

#### `ListWithoutLiveEdges`
- 返回 live device 但 host/discovered edge 链接不再指向任何 live edge 的行。历史 DELETE /edges 行为残留。
- **实现**：NOT EXISTS 子查询 over `edge_devices JOIN edges`，显式 `delete_marker = 0`。

### 删除

#### `Delete`
- 软删 by id。`RowsAffected == 0` → `ErrNotFound`。

#### `DeleteOfflineWithLinkedEdges`
- **签名**：`func (r *Repo) DeleteOfflineWithLinkedEdges(ctx, id uint64) error`
- **职责**：单事务级联软删 offline device + 其 junction 行 + 每个_linked edge identity。
- **流程**（单事务）：
  1. First device；`gorm.ErrRecordNotFound` → `ErrNotFound`
  2. `d.Online` → `ErrConflict`（device 必须 offline）
  3. 查 junction `EdgeDevice` links
  4. `uniqueEdgeIDs` 去重
  5. Count online edges → 若有 → `ErrConflict`（linked edge 必须 offline）
  6. **凭据 tombstone**：每个 edge `Unscoped().Updates(access_key_id="deleted-<id>", secret_key_hash="")`，让删除的安装无法保留可复用 access/secret
  7. 软删 edges（`Delete(&edgemodel.Edge{}, edgeIDs)`）
  8. 软删 junction（`Delete(&model.EdgeDevice{}, "device_id = ?")`）
  9. 软删 device；`RowsAffected == 0` → `ErrNotFound`
- **安全约束**：device 必须 offline + linked edge 必须 offline，双重守卫防止误删活实体。

#### `uniqueEdgeIDs`
- 辅助函数，从 `EdgeDevice` links 去重提取 edge id。

## 5. 依赖关系

- **内部包**：`internal/manager/biz/device`（接口、`HostFacts`、`Usage`、`ListFilter`）、`internal/manager/model/device`、`internal/manager/model/edge`、`internal/pkg/errs`
- **外部库**：`gorm.io/gorm`、`gorm.io/gorm/clause`
- **被调用方**：`internal/manager/biz/device` usecase；`cmd/ongrid` 装配

## 6. 并发与资源管理

- **无显式锁**：依赖 DB 唯一索引 + ON CONFLICT + 乐观 WHERE。
- **事务**：`DeleteOfflineWithLinkedEdges` 单事务保证级联原子性。
- **ctx 透传**：所有方法首参 ctx。
- **凭据 tombstone**：删 edge 前先清 access/secret，防止 deleted 安装保留可复用凭据。

## 7. 设计模式与亮点

- **ON CONFLICT DO NOTHING 跨方言**：`FindOrCreateByFingerprint` 避免 DB 级锁，MySQL/SQLite 通用。
- **RebindFingerprint 原地改绑**：同 row 只改 fingerprint，ID/junction/history 全保留；幂等。
- **SetNodeID 跳过相同值**：避免 NodeMirror hook 每次 register 的冗余写；歧义消解用 Count 区分"不存在" vs "已相同"。
- **ReconcileOfflineOrphans raw SQL**：相关子查询 NOT EXISTS，显式 delete_marker 过滤，跨方言可移植。
- **DeleteOfflineWithLinkedEdges 双重 offline 守卫**：device + linked edge 都必须 offline，防止误删活实体。
- **凭据 tombstone**：删 edge 前清 access/secret，安全约束。
- **ListDeletedWithNodeID / ListWithoutLiveEdges**：boot 时清理历史 topology 泄漏与 DELETE /edges 残留。
- **MatchingRoleValues 位掩码展开**：RolesAny 位掩码展开为合法值集合，让 SQL `IN` 工作。

## 8. 注意事项

- **FindOrCreateByFingerprint 返回 ErrNotFound**：ON CONFLICT DO NOTHING 后 follow-up First 仍找不到时返回 ErrNotFound，理论上不应发生（除非并发硬删）。
- **RebindFingerprint 不删旧行**：原地改绑，旧行不存在时 no-op；caller 需保证 newFP 不已存在。
- **SetNodeID 歧义消解**：`RowsAffected == 0` 时多一次 Count 查询，热路径上略有开销但保证语义清晰。
- **MarkOffline 不动 last_seen_at**：caller 仍可看"最后联系"时间；如需清空需另行处理。
- **ReconcileOfflineOrphans delete_marker 显式过滤**：raw SQL 不走 gorm soft-delete scope，必须显式 `delete_marker = 0`。
- **DeleteOfflineWithLinkedEdges 双重 offline 守卫**：caller 需先确保 device + edge 都 offline，否则 ErrConflict。
- **凭据 tombstone access_key_id 格式**：`deleted-<id>`，便于审计识别已删除安装。
- **ListWithoutLiveEdges NOT EXISTS 子查询**：性能依赖 edge_devices + edges 索引；大表需确保索引覆盖。
- **MatchingRoleValues 同步**：扩展 role 位掩码需同步更新 model 层 `MatchingRoleValues`。
