# `model.go` 技术实现文档

> 源文件：`internal/manager/model/k8s/model.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/model/k8s`

## 1. 概述

本文件是 Kubernetes 集群 onboarding 的 schema：`Cluster`（集群注册）、`Node`（节点快照）、`Workload`（工作负载快照）、`Pod`（Pod 快照）、`Event`（事件快照）、`Installation`（chart/controller 安装范围）、`TelemetryCredential`（数据面专用凭据）。设计要点：所有 inventory 表用 (cluster_id, ...) 唯一约束；`TelemetryCredential` 故意与 Edge 凭据分离——tunnel authenticator 仅读 edges 表，此凭据不能建立 controller tunnel 或执行动作。红线：`BootstrapTokenHash` / `NodeBootstrapTokenHash` / `SecretKeyHash` 都用 hash 存储（明文禁入）；`TelemetryCredential` 写入即只用于 data-plane 工作负载认证。

## 2. 包信息

- **包名**：`k8s`
- **所属模块**：`internal/manager/model/`
- **依赖方向**：被 `manager/biz/k8s` 与 controller inventory sync 调用；依赖 `gorm.io/plugin/soft_delete`、`time`

## 3. 关键类型与接口

```go
// Status / Mode / Role 常量
const (
    ClusterStatusOnline   = "online"
    ClusterStatusOffline  = "offline"
    ClusterStatusDegraded = "degraded"

    ModeFullNode = "full-node"

    RoleNode       = "node"
    RoleController = "controller"
)

type Cluster struct {
    ID     uint64  `gorm:"primaryKey;autoIncrement"`
    Name   string  `gorm:"size:128;not null;column:name;index:idx_k8s_clusters_name"`
    UID    *string `gorm:"size:128;column:uid;uniqueIndex:idx_k8s_clusters_uid_deleted,priority:1"`
    Mode   string  `gorm:"size:32;not null;default:'full-node';column:mode"`
    Status string  `gorm:"size:16;not null;default:'offline';column:status;index:idx_k8s_clusters_status_seen,priority:1"`

    BootstrapTokenHash            string     `gorm:"size:512;not null;column:bootstrap_token_hash"`
    NodeBootstrapTokenHash        string     `gorm:"size:512;not null;default:'';column:node_bootstrap_token_hash"`
    ControllerEdgeID              *uint64    `gorm:"column:controller_edge_id;index"`
    ControllerNodeName            string     `gorm:"size:255;not null;default:'';column:controller_node_name"`
    ControllerNamespace           string     `gorm:"size:255;not null;default:'';column:controller_namespace"`
    ControllerPodName             string     `gorm:"size:255;not null;default:'';column:controller_pod_name"`
    Version                       string     `gorm:"size:64;not null;default:'';column:version"`
    LastSeenAt                    *time.Time `gorm:"column:last_seen_at;index:idx_k8s_clusters_status_seen,priority:2"`
    NodeID                        *uint64    `gorm:"column:node_id;index"`
    InventoryResourceVersion      string     `gorm:"size:128;not null;default:'';column:inventory_resource_version"`
    InventoryResourceVersionsJSON string     `gorm:"type:text;column:inventory_resource_versions_json"`
    InventoryScope                string     `gorm:"size:32;not null;default:'';column:inventory_scope"`
    InventoryNamespace            string     `gorm:"size:255;not null;default:'';column:inventory_namespace"`
    InventorySyncDurationMS       int64      `gorm:"not null;default:0;column:inventory_sync_duration_ms"`
    InventoryWatchLagSeconds      int64      `gorm:"not null;default:0;column:inventory_watch_lag_seconds"`
    InventorySyncedAt             *time.Time `gorm:"column:inventory_synced_at"`
    CreatedBy                     *uint64    `gorm:"column:created_by"`

    CreatedAt    time.Time             `gorm:"column:created_at"`
    UpdatedAt    time.Time             `gorm:"column:updated_at"`
    DeletedAt    *time.Time            `gorm:"index;column:deleted_at"`
    DeleteMarker soft_delete.DeletedAt `gorm:"column:delete_marker;not null;default:0;softDelete:milli,DeletedAtField:DeletedAt;uniqueIndex:idx_k8s_clusters_uid_deleted,priority:2"`
}

type Node struct {
    ID        uint64 `gorm:"primaryKey;autoIncrement"`
    ClusterID uint64 `gorm:"not null;column:cluster_id;uniqueIndex:idx_k8s_nodes_cluster_uid,priority:1;index:idx_k8s_nodes_cluster_seen,priority:1"`
    NodeName  string `gorm:"size:255;not null;default:'';column:node_name"`
    NodeUID   string `gorm:"size:128;not null;column:node_uid;uniqueIndex:idx_k8s_nodes_cluster_uid,priority:2"`
    // ... ProviderID, EdgeID, DeviceID, LabelsJSON, TaintsJSON, ConditionsJSON, CapacityJSON, AllocatableJSON, KubeletVersion, LastSeenAt
}

type Workload struct {
    ID        uint64 `gorm:"primaryKey;autoIncrement"`
    ClusterID uint64 `gorm:"not null;column:cluster_id;uniqueIndex:idx_k8s_workloads_key,priority:1;index:idx_k8s_workloads_cluster_seen,priority:1"`
    Namespace string `gorm:"size:255;not null;default:'';column:namespace;uniqueIndex:idx_k8s_workloads_key,priority:3"`
    Kind      string `gorm:"size:64;not null;column:kind;uniqueIndex:idx_k8s_workloads_key,priority:2"`
    Name      string `gorm:"size:255;not null;column:name;uniqueIndex:idx_k8s_workloads_key,priority:4"`
    UID       string `gorm:"size:128;not null;default:'';column:uid"`
    // ... DesiredReplicas, ReadyReplicas, LabelsJSON, AnnotationsJSON, ConditionsJSON, LastSeenAt
}

type Pod struct {
    ID        uint64 `gorm:"primaryKey;autoIncrement"`
    ClusterID uint64 `gorm:"not null;column:cluster_id;uniqueIndex:idx_k8s_pods_key,priority:1;index:idx_k8s_pods_cluster_seen,priority:1"`
    Namespace string `gorm:"size:255;not null;default:'';column:namespace;uniqueIndex:idx_k8s_pods_key,priority:2"`
    Name      string `gorm:"size:255;not null;column:name;uniqueIndex:idx_k8s_pods_key,priority:3"`
    UID       string `gorm:"size:128;not null;column:uid;uniqueIndex:idx_k8s_pods_key,priority:4"`
    NodeName     string     `gorm:"size:255;not null;default:'';column:node_name;index"`
    Phase        string     `gorm:"size:32;not null;default:'';column:phase"`
    OwnerKind    string     `gorm:"size:64;not null;default:'';column:owner_kind"`
    OwnerName    string     `gorm:"size:255;not null;default:'';column:owner_name"`
    RestartCount int        `gorm:"not null;default:0;column:restart_count"`
    Reason       string     `gorm:"size:255;not null;default:'';column:reason;index:idx_k8s_pods_reason"`
    // ... LastSeenAt
}

type Event struct { /* ClusterID, Namespace, Name, UID, Type, Reason, Message, Involved*, Source*, Reporting*, Action, Count, *Timestamp, EventTime, LastSeenAt */ }
type Installation struct { /* ClusterID, Mode, ScopeType, Namespace, ControllerEdgeID, CapabilitiesJSON, LastSeenAt */ }

type TelemetryCredential struct {
    ClusterID     uint64 `gorm:"primaryKey;column:cluster_id"`
    AccessKeyID   string `gorm:"size:128;not null;uniqueIndex:idx_k8s_telemetry_credentials_access_key;column:access_key_id"`
    SecretKeyHash string `gorm:"size:512;not null;column:secret_key_hash"`
    CreatedAt time.Time `gorm:"column:created_at"`
    UpdatedAt time.Time `gorm:"column:updated_at"`
}
```

## 4. 关键函数与流程

本文件定义 7 个 struct 与对应 TableName 方法（`Cluster` / `Node` / `Workload` / `Pod` / `Event` / `Installation` / `TelemetryCredential`）。无 GORM hook，无业务方法。

## 5. 依赖关系

- **内部包**：`edge` 包（通过 ControllerEdgeID）、`device` 包（通过 DeviceID）、`topology` 包（通过 NodeID）
- **外部库**：`gorm.io/plugin/soft_delete`、`time`
- **被调用方**：`manager/biz/k8s` 的 controller inventory sync；tunnel authenticator（仅读 edges 不读 TelemetryCredential）

## 6. 并发与资源管理

- 本文件仅定义 schema，无锁 / channel / 缓存
- `Cluster.DeleteMarker` 加入 unique index 让软删后可重建同 UID
- 其他 inventory 表无软删（snapshot 表，更新覆盖）

## 7. 设计模式与亮点

- **Inventory 表用 (cluster_id, ...) 唯一约束**：保证同 cluster 内同 UID 仅一行；snapshot 更新覆盖
- **TelemetryCredential 与 Edge 凭据分离**：data-plane 工作负载专用；tunnel authenticator 仅读 edges 表，此凭据不能建立 controller tunnel 或执行动作（权限最小化）
- **BootstrapTokenHash / NodeBootstrapTokenHash 双 hash**：分别用于 controller bootstrap 和 node bootstrap；明文禁入
- **ClusterStatus 三态**：online/offline/degraded；degraded 表示 controller 在但部分节点失联
- **InventoryResourceVersion**：K8s resourceVersion 跟踪，避免 stale snapshot 覆盖更新
- **InventoryWatchLagSeconds**：watch 滞后秒数；监控 sync 健康度
- **ControllerEdgeID 反向链**：cluster 与 controller edge 的关联；nullable 表示未部署 controller
- **NodeID 反向链 topology**：cluster 可关联 topology node；nullable 迁移窗口
- **复合索引设计**：(cluster_id, last_seen_at) 支持按时间扫描； (cluster_id, uid) 唯一； (namespace, reason) 支持事件查询
- **JSON 字段 NOT NULL**：LabelsJSON / TaintsJSON / ConditionsJSON / CapacityJSON / AllocatableJSON 都 NOT NULL；biz 总写至少 "{}"

## 8. 注意事项

- **Cluster.UID 可空**：未上报或 legacy 行；unique index 与 DeleteMarker 联合让多 NULL 共存
- **BootstrapTokenHash 必填**：cluster 注册时必须生成
- **NodeBootstrapTokenHash 默认空字符串**：未启用 node bootstrap 时为 ""
- **InventoryResourceVersionsJSON 可空**：多 resource version 跟踪时填；单 RV 用 InventoryResourceVersion
- **InventoryWatchLagSeconds / InventorySyncDurationMS**：监控 sync 健康度；大值需告警
- **TelemetryCredential.AccessKeyID 唯一**：跨 cluster 全局唯一
- **TelemetryCredential 无软删**：cluster 删除时由 biz 层显式 DELETE
- **Pod.Reason 索引**：常见查询（Failed pods）
- **Event.Involved* 三索引**：(involved_kind, involved_namespace, involved_name) 支持按对象查事件
- **Mode 当前仅 "full-node"**：未来可能扩展 edge-native 等
