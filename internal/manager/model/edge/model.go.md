# `model.go` 技术实现文档

> 源文件：`internal/manager/model/edge/model.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/model/edge`

## 1. 概述

本文件是 edge 子域的 schema：定义 `Edge` 实体（注册的 edge agent tunnel 身份）。May 2026 entity split 后，主机事实（hostname / OS / 硬件 / roles / usage）迁到 `model/device.Device`，Edge 仅保留 tunnel-y 字段（access_key / secret_key_hash / online / last_seen）。设备关联通过 `edge_devices` junction；`DeviceID` 字段作为只读便利指针保留以兼容旧 caller。红线：`SecretKeyHash` 用 argon2id（禁 MD5/SHA1）；Status 仅 online/offline 二态（"disabled" 已废弃，删除走 soft-delete）。

## 2. 包信息

- **包名**：`edge`
- **所属模块**：`internal/manager/model/`
- **依赖方向**：被 `manager/biz/edge` 与 tunnel 认证器调用；依赖 `gorm.io/plugin/soft_delete`、`time`

## 3. 关键类型与接口

```go
type Edge struct {
    ID uint64 `gorm:"primaryKey;autoIncrement"`
    Name          string `gorm:"size:128;not null;default:''"`
    AccessKeyID   string `gorm:"size:32;not null;column:access_key_id;uniqueIndex:idx_edges_access_key_id,priority:1"`
    SecretKeyHash string `gorm:"size:512;not null;column:secret_key_hash"` // argon2id
    Status        string `gorm:"size:16;default:offline;check:status IN ('online','offline')"`
    Description   string `gorm:"size:255;not null;default:''"`

    LastSeenAt  *time.Time `gorm:"column:last_seen_at"`
    AgentVersion string    `gorm:"size:32;not null;default:'';column:agent_version"`
    DeviceID     *uint64   `gorm:"index;column:device_id"`
    CreatedBy    *uint64   `gorm:"column:created_by"` // audit only

    CreatedAt    time.Time             `gorm:"column:created_at"`
    UpdatedAt    time.Time             `gorm:"column:updated_at"`
    DeletedAt    *time.Time            `gorm:"index;column:deleted_at"`
    DeleteMarker soft_delete.DeletedAt `gorm:"column:delete_marker;not null;default:0;softDelete:milli,DeletedAtField:DeletedAt;uniqueIndex:idx_edges_access_key_id,priority:2"`
}

// Status 常量
const (
    StatusOnline  = "online"
    StatusOffline = "offline"
)
```

## 4. 关键函数与流程

### `Edge.TableName`
- **签名**：`func (Edge) TableName() string`
- **职责**：固定表名 `edges`

## 5. 依赖关系

- **内部包**：`device` 包（通过 DeviceID 与 EdgeDevice junction）
- **外部库**：`gorm.io/plugin/soft_delete`、`time`
- **被调用方**：`manager/biz/edge`、tunnel authenticator（仅读 edges 表）、SPA Edges 页面

## 6. 并发与资源管理

- 本文件仅定义 schema，无锁 / channel / 缓存
- `soft_delete.DeletedAt` 实现 milli 精度软删除；DeleteMarker 加入 unique index 让软删后可重用同 access_key

## 7. 设计模式与亮点

- **Edge = tunnel 身份**：post-split 仅 tunnel-y 字段；host facts 在 Device
- **DeviceID 只读便利指针**：source of truth 是 `edge_devices` junction（Type=Host）；register flow 同步保持此字段以兼容旧 caller 读 `e.DeviceID`
- **AccessKeyID 唯一**：与 DeleteMarker 联合唯一；软删后可重新注册同 key
- **SecretKeyHash argon2id**：禁 MD5/SHA1；hash 长 512 字符容纳 argon2id 参数
- **Status 二态**：online/offline；post-pivot 删除"disabled"，删除走 DeletedAt soft-delete
- **AgentVersion 自报**：register_edge handshake 时上报；SPA Edges 页面显示版本漂移
- **Name 可空 create**：留空时 `edge.HandleRegister` 在首次 tunnel handshake 用上报 hostname 自动填
- **CreatedBy 审计**：nullable FK to users.id；仅审计用，不参与权限
- **gorm 自动过滤软删**：默认查询自动 filter `DeletedAt IS NULL` 的行外

## 8. 注意事项

- **AccessKeyID size:32**：与 enrollment 流程生成的 key 长度匹配
- **SecretKeyHash size:512**：argon2id 编码后长度容纳
- **Status CHECK 约束**：仅 online/offline 合法；写入 disabled 会失败
- **DeviceID nullable**：未关联 host device 时 NULL；register flow 后同步填
- **AgentVersion 空字符串**：旧 agent 未上报或拒绝上报时为 ""
- **CreatedBy 仅审计**：不用于权限判断；post-pivot 无 org_id
- **软删 vs Status**：删除走 DeletedAt；Status 仅在线状态
- **register handshake 自动填 Name**：避免 operator 必须先在 UI 命名
