# `model.go` 技术实现文档

> 源文件：`internal/manager/model/device/model.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/model/device`

## 1. 概述

本文件是 device 子域的 schema：定义 `Device`（被监控主机）与 `EdgeDevice`（Edge↔Device 多对多 junction）。May 2026 entity split 后，主机事实（hostname / OS / 硬件 / 角色 / 实时 usage）从 Edge 迁到 Device，Edge 仅保留 agent 身份。设计要点：Device 按 Fingerprint 稳定标识；Roles 4-bit 字段（server/storage/network/database）支持多角色；通过 `edge_devices` junction（Type=host|discovered）支持一 Edge 多 Device 与一 Device 多 Edge。红线：RoleBit 常量数值不可重新编号（操作员存量值会静默重分类）；`MatchingRoleValues` 返回 `[]int` 而非 `[]uint8` 因 GORM 把 `[]byte` 当 BLOB。

## 2. 包信息

- **包名**：`device`
- **所属模块**：`internal/manager/model/`
- **依赖方向**：被 `manager/biz/device` 与 `manager/biz/edge` 调用；依赖 `gorm.io/plugin/soft_delete`、`time`

## 3. 关键类型与接口

```go
type Device struct {
    ID          uint64 `gorm:"primaryKey;autoIncrement"`
    Fingerprint string `gorm:"size:128;not null;column:fingerprint;uniqueIndex:idx_devices_fingerprint,priority:1"`
    UserID      *uint64 `gorm:"column:user_id"`

    Name        string `gorm:"size:255;not null;default:''"`
    Description string `gorm:"size:255;not null;default:''"`

    Hostname      string `gorm:"size:255;not null"`
    OS            string `gorm:"size:64;not null"`
    OSVersion     string `gorm:"size:128;not null;default:'';column:os_version"`
    Arch          string `gorm:"size:32;not null"`
    KernelVersion string `gorm:"size:128;not null;column:kernel_version"`
    IPAddress     string `gorm:"size:45;not null;default:'';column:ip_address"`

    // 容量事实
    CPUCount       int    `gorm:"not null;column:cpu_count"`
    MemTotalBytes  uint64 `gorm:"not null;column:mem_total_bytes"`
    DiskTotalBytes uint64 `gorm:"not null;default:0;column:disk_total_bytes"`

    // 实时使用率
    CPUUsagePct  float32 `gorm:"not null;default:0;column:cpu_usage_pct"`
    MemUsagePct  float32 `gorm:"not null;default:0;column:mem_usage_pct"`
    DiskUsagePct float32 `gorm:"not null;default:0;column:disk_usage_pct"`

    // Roles 4-bit 字段
    Roles uint8 `gorm:"not null;default:0;index:idx_devices_roles;check:roles BETWEEN 0 AND 15;column:roles"`

    // 在线状态（denormalised from Edge）
    Online     bool       `gorm:"not null;default:false"`
    LastSeenAt *time.Time `gorm:"column:last_seen_at"`

    // 链接到 nodes 表
    NodeID *uint64 `gorm:"column:node_id;uniqueIndex:idx_devices_node_id,priority:1"`

    CreatedAt    time.Time             `gorm:"column:created_at"`
    UpdatedAt    time.Time             `gorm:"column:updated_at"`
    DeletedAt    *time.Time            `gorm:"index;column:deleted_at"`
    DeleteMarker soft_delete.DeletedAt `gorm:"column:delete_marker;not null;default:0;softDelete:milli,DeletedAtField:DeletedAt;uniqueIndex:idx_devices_fingerprint,priority:2;uniqueIndex:idx_devices_node_id,priority:2"`
}

// Role bit 常量
const (
    RoleBitServer   uint8 = 1 << 0 // 0b0001
    RoleBitStorage  uint8 = 1 << 1 // 0b0010
    RoleBitNetwork  uint8 = 1 << 2 // 0b0100
    RoleBitDatabase uint8 = 1 << 3 // 0b1000
    RolesAllKnownBits uint8 = RoleBitServer | RoleBitStorage | RoleBitNetwork | RoleBitDatabase
)

// Role 字符串
const (
    RoleServer   = "server"
    RoleStorage  = "storage"
    RoleNetwork  = "network"
    RoleDatabase = "database"
    RoleUnknown  = "unknown"
)

type EdgeDeviceRelationType int
const (
    EdgeDeviceRelationHost       EdgeDeviceRelationType = 1
    EdgeDeviceRelationDiscovered EdgeDeviceRelationType = 2
)

type EdgeDevice struct {
    ID           uint64                 `gorm:"primaryKey;autoIncrement"`
    EdgeID       uint64                 `gorm:"not null;column:edge_id;uniqueIndex:idx_edge_device_unique,priority:1;index:idx_edge_device_edge"`
    DeviceID     uint64                 `gorm:"not null;column:device_id;uniqueIndex:idx_edge_device_unique,priority:2;index:idx_edge_device_device"`
    Type         EdgeDeviceRelationType `gorm:"not null;default:1;column:type;uniqueIndex:idx_edge_device_unique,priority:3"`
    CreatedAt    time.Time              `gorm:"column:created_at"`
    UpdatedAt    time.Time              `gorm:"column:updated_at"`
    DeletedAt    *time.Time             `gorm:"index;column:deleted_at"`
    DeleteMarker soft_delete.DeletedAt  `gorm:"column:delete_marker;not null;default:0;softDelete:milli,DeletedAtField:DeletedAt;uniqueIndex:idx_edge_device_unique,priority:4"`
}
```

## 4. 关键函数与流程

### `Device.TableName / EdgeDevice.TableName`
- 固定表名分别为 `devices` / `edge_devices`

### `IsValidRoleName`
- **签名**：`func IsValidRoleName(s string) bool`
- **职责**：判断是否为合法 role name；`RoleUnknown` 也算合法（表示清空）

### `IsValidRoles`
- **签名**：`func IsValidRoles(r uint8) bool`
- **职责**：判断 bit field 是否仅含已知 bit
- **流程**：`r &^ RolesAllKnownBits == 0`

### `EncodeRoles`
- **签名**：`func EncodeRoles(names []string) uint8`
- **职责**：role name slice → bit set
- **流程**：
  1. 遍历 names
  2. 命中 `RoleUnknown` 立即返回 0（清空，其他 name 失效）
  3. 否则 OR 对应 bit
  4. 未知 name 静默跳过
- **用途**：PATCH wire 输入解析

### `DecodeRoles`
- **签名**：`func DecodeRoles(r uint8) []string`
- **职责**：bit set → role name slice（canonical 顺序 server/storage/network/database）
- **流程**：按固定顺序遍历 4 个 bit；命中则 append
- **空集行为**：返回空 slice；caller 自行渲染"未分类"

### `MatchingRoleValues`
- **签名**：`func MatchingRoleValues(mask uint8) []int`
- **职责**：枚举所有合法存储值（0..15）中 bit set 与 mask 重叠的值
- **用途**：把非 sargable `WHERE roles & ? != 0` 转为 sargable `WHERE roles IN (...)` 命中 `idx_devices_roles`
- **流程**：
  1. `mask &= RolesAllKnownBits`（忽略未知 bit）
  2. mask == 0 → 返回 `[]int{0}`（"未分类 only"）
  3. 遍历 v=1..15；v & mask != 0 → append
- **关键：返回 `[]int`**：`[]uint8` 是 `[]byte` 别名，GORM 会当 BLOB 单值而非 splat 到 IN 列表

## 5. 依赖关系

- **内部包**：`edge` 包（通过 EdgeDevice junction）；`topology` 包（通过 NodeID）
- **外部库**：`gorm.io/plugin/soft_delete`、`time`
- **被调用方**：`manager/biz/device`、`manager/biz/edge`、AI prompt 路由（按 Role 分流）

## 6. 并发与资源管理

- 本文件仅定义 schema，无锁 / channel / 缓存
- `soft_delete.DeletedAt` 实现 milli 精度软删除；`DeleteMarker` 加入 unique index 避免软删后无法重建同名行

## 7. 设计模式与亮点

- **Fingerprint 稳定标识**：当前来自 Hostname 或 machine-id；未来 agent 升级用 `/etc/machine-id` 或 platform UUID 跨 hostname 重命名保持稳定
- **Roles 4-bit 字段**：1 字节存储 4 bit used + 4 bit reserved；多角色 hyper-converged / NAS / edge gateway 一次承载
- **RoleBit 不可重新编号**：操作员存量值会静默重分类；新增 role 用 reserved bit
- **MatchingRoleValues sargable 转换**：`[]int` 而非 `[]uint8` 绕开 GORM BLOB 误解
- **Online / LastSeenAt denormalised from Edge**：device 列表渲染快无需 JOIN edge 表；任一 Edge 状态变化时同步更新 Device
- **NodeID nullable 迁移窗口**：legacy 行由 topology Migrate 回填；新行由 edge register flow 经 NodeMirror hook 写
- **EdgeDeviceRelationType 枚举**：Host (edge 跑此 device) vs Discovered (edge 扫描 LAN 发现但不跑)；v1 仅 Host；schema 已为未来"edge scans devices"流铺路
- **Junction 唯一约束**：(edge_id, device_id, type, delete_marker) 四元 unique，单一 edge 不能重复注册同 device 同类型；但一 device 可在多 edge 下（host + discovered 共存）
- **uint64 byte 字段**：MemTotalBytes / DiskTotalBytes 用 uint64 避免大主机（>2TiB RAM / >8PiB disk）溢出；CPUCount 保持 int 匹配 wire shape

## 8. 注意事项

- **Fingerprint 唯一**：跨未软删行 + delete_marker 联合唯一；软删后可重新注册同 fingerprint
- **Roles CHECK 约束**：`BETWEEN 0 AND 15` 防止未知 bit 误写入
- **RoleUnknown 不设 bit**：仅是"未分类"label；EncodeRoles 命中即清空
- **EncodeRoles 未知 name 静默跳过**：caller 应先 IsValidRoleName 校验
- **NodeID 迁移期可空**：cutover 后将改 NOT NULL；当前 nullable 让 migration reentrant
- **IPAddress 可空字符串**：agent 未上报或未收集时为 ""
- **CPUUsagePct / MemUsagePct / DiskUsagePct denormalised**：ticker-driven 聚合更新，避免每次渲染 JOIN host_metrics
- **DeleteMarker 在 unique index**：软删后同 fingerprint 可重新注册
- **EdgeDevice.Type 默认 1 (Host)**：discovered 关系需显式写 2
- **DeviceID 与 legacy EdgeID 1:1**：May 2026 split 后整数复用，迁移不丢历史
