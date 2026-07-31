# `migrate.go` 技术实现文档

> 源文件：`internal/manager/data/device/store/migrate.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/device/store`

## 1. 概述

本文件是 device store 的 schema 入口，AutoMigrate `devices` + `edge_devices` 两张表，并执行 May 2026 entity split 的 backfill：从 legacy `edges`-only 世界回填到 `devices` + `edge_devices`。关键设计：**复用 integer id**（`device.id == edge.id` for backfilled rows），让既有 `edge_id=N` Prom label 数值等于新 `device_id=N` label，dashboard / saved alert filter 无需 value remap。同时 `detachKubernetesControllerHosts` 清理 controller-only edge 误绑的 host。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/device`
- **依赖方向**：被 `cmd/ongrid` 启动调用；依赖 `gorm.io/gorm`、`internal/manager/model/device`、`internal/pkg/dbx`。

## 3. 关键类型与接口

```go
// 内部辅助类型，不导出
type edgeRow struct {
    ID         uint64
    Name       string
    DeviceID   *uint64
    Roles      uint8
    LastSeenAt *string // ISO8601 / driver-specific，直接透传给 UPDATE
    Status     string
}
```

## 4. 关键函数与流程

### `Migrate`
- **签名**：`func Migrate(db *gorm.DB) error`
- **流程**：
  1. `dbx.NeedsDeleteMarkerMigration(devices)` → `dbx.DropIndexes(idx_devices_fingerprint, idx_devices_node_id)`
  2. `dbx.NeedsDeleteMarkerMigration(edge_devices)` → `dbx.DropIndexes(idx_edge_device_unique)`
  3. `AutoMigrate(Device, EdgeDevice)`
  4. `dbx.BackfillDeleteMarker(devices)` + `dbx.BackfillDeleteMarker(edge_devices)`
  5. `detachKubernetesControllerHosts(db)`
  6. `backfillFromEdges(db)`

### `detachKubernetesControllerHosts`
- **职责**：移除 controller-only edge 误绑的 host 关联。Kubernetes controller 是 cluster access path，非物理设备；node edge 不受影响。
- **流程**（单事务）：
  1. `kubernetesControllerEdgeIDs` 取 controller edge id 列表
  2. 空 list 或 edges 表不存在 → no-op
  3. Pluck edges.device_id（controller edge 的 device pointer）
  4. Pluck edge_devices.device_id（type=Host 的 junction）
  5. 合并 candidateDeviceIDs（去重）
  6. 删 controller edge 的 type=Host junction
  7. 清 controller edge 的 device_id pointer
  8. 对每个 candidate device：Count 剩余 links + 剩余 pointers；都为 0 → 软删 device（孤立清理）

### `kubernetesControllerEdgeIDs`
- **职责**：从 `k8s_clusters.controller_edge_id` 取 controller edge id 列表。
- **流程**：检查表与列存在；按 `delete_marker = 0` 或 `deleted_at IS NULL` 过滤；Distinct Pluck。

### `backfillFromEdges`
- **职责**：遍历 legacy `edges` 行，确保：
  - Device 行存在（缺失时创建 `id = edge.id`，让 legacy `edge_id=N` Prom 样本对齐 `device_id=N`）
  - `device.roles` 从 `edge.roles` 拷贝
  - `edge_devices(edge_id=N, device_id=device.id, type=host)` junction 存在
  - `edge.device_id` pointer 指向 host device id
- **流程**：
  1. edges 表不存在 → no-op
  2. `kubernetesControllerEdgeIDs` 取 controller 列表，从 backfill 排除
  3. 探测 `edges.roles` 列是否存在（首次存在，拷贝后 drop；后续不存在）
  4. Find 所有非软删 edges（排除 controller）
  5. 每行：
     - 确定 hostDeviceID（edge.device_id 已设则复用，否则用 edge.id）
     - Device 存在 → Updates（补 roles / name 兜底）
     - Device 不存在 → Create（id = hostDeviceID, Fingerprint = `legacy:edge:<id>`, Hostname = name, Roles, Online 按 status）
     - junction 存在 → skip；不存在 → Create
     - 同步 edge.device_id pointer
  6. 打印 createdDevices / createdJunctions 数
  7. **destructive cleanup**：drop `edges.roles` 列（source of truth 已移到 devices）
- **幂等性**：每步 guarded write，二次启动 skip。
- **错误处理**：Device Create 撞 UNIQUE/Duplicate → continue（operator 已有 Device 指向此 edge，让 edge migration 的 SetDeviceID 步骤链接）。

### `appendUniqueUint64s`
- 辅助函数，去重合并 uint64 切片。

## 5. 依赖关系

- **内部包**：`internal/manager/model/device`、`internal/pkg/dbx`
- **外部库**：`gorm.io/gorm`、`fmt`、`strings`
- **被调用方**：`cmd/ongrid` 启动序列（通过 `dbx.RunMigrations`）
- **执行顺序约束**：manager 启动时 edge migrator 必须先于 device migrator；测试中单 migrate 时 backfill 优雅 skip（edges 表不存在）。

## 6. 并发与资源管理

- **无锁**：启动期串行执行。
- **事务**：`detachKubernetesControllerHosts` 单事务。
- **幂等性**：backfill 每步 guarded；二次启动 skip。

## 7. 设计模式与亮点

- **复用 integer id 保持 Prom label 对齐**：`device.id == edge.id` for backfilled rows，让 `edge_id=N` Prom 样本数值等于 `device_id=N`，dashboard / alert filter 无需 remap。
- **roles 列迁移**：从 edge 拷贝到 device 后 drop，source of truth 单一化。
- **controller-only edge 清理**：Kubernetes controller 非物理设备，host 关联是误绑；事务清理 + 孤立 device 软删。
- **edgeRow 松类型**：不 import edge model 避免循环依赖（device migration → edge model → device model）。
- **UNIQUE/Duplicate 容忍**：Device Create 撞唯一索引时 continue，让 edge migration 链接。
- **destructive cleanup drop roles**：pre-launch 决策，drop legacy 列让 schema 单一化。

## 8. 注意事项

- **执行顺序**：edge migrator 必须先于 device migrator；否则 backfill 找不到 edges 表（测试中 graceful skip）。
- **复用 integer id 是 pre-launch 决策**：post-launch 需迁至独立 id 生成策略。
- **drop edges.roles 是 destructive**：pre-launch 决策；post-launch 需 expand-contract。
- **UNIQUE/Duplicate 容忍**：operator 已有 Device 指向 edge 时 backfill Create 跳过，依赖 edge migration SetDeviceID 链接。
- **k8s_clusters 表可选**：表或列不存在时 controller 清理 no-op。
- **dbx.NeedsDeleteMarkerMigration**：检测 legacy soft-delete 列，需先 drop 受影响索引再 AutoMigrate 重建。
- **edgeRow LastSeenAt 松类型**：`*string` 透传 driver-specific 格式，避免 time.Time 跨方言解析问题。
- **打印用 fmt.Printf**：启动日志，非结构化；生产应迁 slog。
