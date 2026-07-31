# `repo.go` 技术实现文档

> 源文件：`internal/manager/data/k8s/store/repo.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/k8s/store`

## 1. 概述

本文件实现 `biz/k8s.Repository` 的 GORM 落地，覆盖 Kubernetes onboarding 全部持久化：cluster 注册与 controller 绑定、telemetry credential 管理、node/workload/pod/event inventory upsert 与 stale 清理、topology 链接、edge attachment 聚合查询。核心设计：`BindClusterUID` 用 `WHERE uid IS NULL OR uid = ''` 乐观守卫 + 三态结果（bound / conflict / mismatch）；`BindControllerEnrollment` 单事务 upsert cluster + installation + telemetry_credential；`UpsertNode/Workloads/Pods/Events` 用 ON CONFLICT 批量 upsert；`ListEdgeAttachments` 用 raw SQL UNION ALL 聚合三种 edge 挂载来源。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/k8s`
- **依赖方向**：被 `internal/manager/biz/k8s` 装配；依赖 `internal/manager/biz/k8s`（接口与 filter）、`internal/manager/model/k8s`、`internal/pkg/errs`、`gorm.io/gorm`、`gorm.io/gorm/clause`。

## 3. 关键类型与接口

```go
// Repo 是 Kubernetes onboarding repository。
type Repo struct {
    db *gorm.DB
}

var _ biz.Repository = (*Repo)(nil)
```

## 4. 关键函数与流程

### Cluster

#### `CreateCluster` / `GetCluster` / `GetClusterByControllerEdge`
- `CreateCluster`：`c == nil` → `ErrInvalid`；Create。
- `GetCluster`：按 PK；`gorm.ErrRecordNotFound` → `ErrNotFound`。
- `GetClusterByControllerEdge`：按 `controller_edge_id` 取 cluster。

#### `ListClusters` / `CountClusters`
- **签名**：`ListClusters(ctx, f biz.ListClustersFilter) ([]*model.Cluster, error)` + `CountClusters(ctx, f) (int64, error)`
- **职责**：按 filter 列出 / 计数，`id DESC` 排序。
- **filter**：Status / Name(LIKE) / Mode；通过 `applyClusterFilters` 集中处理。

#### `BindClusterUID`
- **签名**：`func (r *Repo) BindClusterUID(ctx, id uint64, uid string) error`
- **职责**：把 cluster UID 绑定到 cluster 行。三态结果：
  - 成功 bound（`WHERE uid IS NULL OR uid = ''` 命中，Update）
  - conflict（UID 已被其他 cluster 绑定，unique index 冲突 → `ErrConflict`）
  - mismatch（cluster 已有不同 UID → `ErrConflict`）
- **流程**：
  1. TrimSpace；`id == 0 || uid == ""` → `ErrInvalid`
  2. `WHERE id = ? AND (uid IS NULL OR uid = '')` Update uid
  3. 错误检查 duplicate key → `ErrConflict`
  4. `RowsAffected > 0` → 成功 bound
  5. 否则 First cluster 查 current uid；不存在 → `ErrNotFound`；与 uid 相同 → 已 bound（no-op）；不同 → `ErrConflict` mismatch

#### `UpdateClusterTokens` / `UpdateClusterController` / `TouchClusterControllerHeartbeat`
- `UpdateClusterTokens`：替换 `bootstrap_token_hash` + `node_bootstrap_token_hash` + 清空 `controller_pod_name`。
- `UpdateClusterController`：写 controller edge_id / node_name / namespace / pod_name / last_seen_at / status=Online。
- `TouchClusterControllerHeartbeat`：按 controller_edge_id bump last_seen_at + status=Online。

#### `BindControllerEnrollment`
- **签名**：`func (r *Repo) BindControllerEnrollment(ctx, id uint64, registration biz.ClusterControllerRegistration, installation *model.Installation, telemetryCredential *model.TelemetryCredential) error`
- **职责**：单事务完成 controller 注册 + installation upsert + telemetry credential upsert。
- **流程**（单事务）：
  1. 校验 installation / credential 非 nil 且 cluster_id 匹配 → `ErrInvalid`
  2. Updates cluster（controller fields + status=Online）
  3. ON CONFLICT (cluster_id, mode, scope_type, namespace) DO UPDATE installation
  4. ON CONFLICT (cluster_id) DO UPDATE telemetry_credential

#### `GetTelemetryCredentialByAccessKey` / `UpsertTelemetryCredential`
- `GetTelemetryCredentialByAccessKey`：按 access_key_id 取；`gorm.ErrRecordNotFound` → `ErrNotFound`。
- `UpsertTelemetryCredential`：ON CONFLICT (cluster_id) DO UPDATE access_key_id / secret_key_hash / updated_at。校验 cluster_id / access_key_id / secret_key_hash 非空 → `ErrInvalid`。

#### `UpdateClusterInventorySync`
- 写 inventory 同步元数据：resource_version / versions_json / scope / namespace / sync_duration_ms / watch_lag_seconds / synced_at + status=Online + last_seen_at。

#### `UpdateClusterTopologyNode` / `UpdateDeviceTopologyNode`
- 写 topology node_id 链接。`WHERE id = ? AND (node_id IS NULL OR node_id <> ?)` 跳过相同值；`RowsAffected == 0` 时 Count 区分"不存在" vs "已相同"。
- `UpdateDeviceTopologyNode` 操作 `devices` 表（显式 `deleted_at IS NULL`，绕过 k8s repo 不直接管 device 软删 scope）。

#### `ListClusterEdgeIDs` / `GetClusterIDByEdgeID`
- `ListClusterEdgeIDs`：raw SQL UNION 三源（k8s_clusters.controller_edge_id + k8s_nodes.edge_id + k8s_installations.controller_edge_id），返回 cluster 关联的全部 edge_id。
- `GetClusterIDByEdgeID`：反向查询，UNION ALL 三源，按 edge_id 取 cluster_id；`cluster_id == 0` → `ErrNotFound`。

#### `DeleteCluster`
- **职责**：单事务级联软删 cluster + 全部子表（Node / Workload / Pod / Event / Installation / TelemetryCredential）。
- **流程**：循环删各子表 `WHERE cluster_id = ?`，最后删 cluster；`RowsAffected == 0` → `ErrNotFound`。

### Node

#### `GetNodeByClusterUID` / `GetNodeByEdgeID` / `GetNodeByClusterName` / `GetLinkedNodeByClusterName`
- 按 (cluster_id, node_uid) / edge_id / (cluster_id, node_name) 取 node。
- `GetLinkedNodeByClusterName`：仅返回 edge_id 或 device_id 非空的 node（已链接的）。
- 多行时 `Order("id DESC")` 取最新。

#### `ListNodesByRefs`
- 按 `[]biz.NodeRef`（UID 或 Name）批量查 node，OR 拼接 predicates。

#### `ListStaleNodes`
- 返回 `last_seen_at IS NULL OR last_seen_at < olderThan` 的 node，供 stale 清理。

#### `UpsertNode`
- ON CONFLICT (cluster_id, node_uid) DO UPDATE 全字段（含 edge_id / device_id 可选）。

#### `DeleteDuplicateNodesByName`
- 删同 (cluster_id, node_name) 但 node_uid <> keepUID 的重复行。

#### `UpdateNodeEdge`
- 写 node 的 edge_id + 可选 device_id + last_seen_at。`RowsAffected == 0` → `ErrNotFound`。

### Workload / Pod / Event

#### `UpsertWorkloads` / `UpsertPods` / `UpsertEvents`
- ON CONFLICT 复合列 DO UPDATE 指定列；`CreateInBatches(&items, 200)` 批量插入。
- Workload 冲突列：(cluster_id, kind, namespace, name)
- Pod 冲突列：(cluster_id, namespace, name, uid)
- Event 冲突列：(cluster_id, uid)

#### `DeleteNodes` / `DeleteWorkloads` / `DeletePods` / `DeleteEvents`
- 按 `[]biz.NodeRef` / `WorkloadRef` / `PodRef` / `EventRef` 批量删；每个 ref 单独 DELETE。

#### `DeleteStaleWorkloads` / `DeleteStalePods` / `DeleteStaleEvents`
- 按 `last_seen_at IS NULL OR last_seen_at < olderThan` 删 stale 行；可选 namespace 过滤。

#### `DeleteEventsBefore` / `DeleteOldestEvents`
- `DeleteEventsBefore`：删 `eventTimestampExpr() < cutoff` 的 event，limit 限制。
- `DeleteOldestEvents`：按 cluster_id 保留最新 keep 条，删更老的，limit 限制。
- 实现：先 Pluck id（按 eventTimestampExpr 排序），再 `WHERE id IN ?` Delete。
- `eventTimestampExpr`：`COALESCE(last_timestamp, event_time, first_timestamp, last_seen_at, created_at)` 跨方言时间戳 fallback。

### 查询

#### `ListNodes` / `ListNodesPage` / `CountNodesPage` / `CountNodes`
- `ListNodes`：按 cluster_id 列全部 node，`node_name ASC`。
- `ListNodesPage` / `CountNodesPage`：分页 + `applyNodeFilter`（Query LIKE 多列）。
- `CountNodes`：按 cluster_id 计数。

#### `ListTopologyNodeLinks`
- JOIN `k8s_nodes` + `devices`，返回 node → device 链接信息（node_name / node_uid / device_id / device_name / device_node_id）。

#### `GetNodeCoverage` / `GetNodeCoverageByClusterIDs`
- 聚合查询：total / edge_linked / device_linked 计数。
- `GetNodeCoverageByClusterIDs`：按 cluster_id 分组，返回 map；missing cluster_id 补零值。

#### `ListEdgeAttachments`
- **签名**：`func (r *Repo) ListEdgeAttachments(ctx, limit, offset int) ([]biz.EdgeAttachment, int64, error)`
- **职责**：聚合三种 edge 挂载来源（k8s-controller / k8s-node / k8s-controller-runtime），分页返回 + total count。
- **实现**：raw SQL `edgeAttachmentsQuery` UNION ALL 三源 + 外层 COUNT + 分页 ORDER BY。

#### `ListWorkloads` / `CountWorkloads` / `ListPods` / `CountPods` / `ListEvents` / `CountEvents`
- 分页列 + count，通过 `applyWorkloadFilter` / `applyPodFilter` / `applyEventFilter` 集中处理 filter。
- filter 含 Query LIKE 多列（`applyLikeAny` 辅助函数 OR 拼接 LIKE predicates）。
- Pod `IssueOnly`：phase IN (Pending, Failed) OR reason IN (CrashLoopBackOff, OOMKilled, ImagePullBackOff, ErrImagePull)。
- Event `IssueOnly`：type = Warning。

#### `UpsertInstallation`
- ON CONFLICT (cluster_id, mode, scope_type, namespace) DO UPDATE controller_edge_id / capabilities_json / last_seen_at / updated_at。

### 辅助函数

#### `applyClusterFilters` / `applyNodeFilter` / `applyWorkloadFilter` / `applyPodFilter` / `applyEventFilter`
- 集中处理各 filter 的 Where 链。

#### `applyLikeAny`
- **签名**：`func applyLikeAny(tx *gorm.DB, query string, columns []string) *gorm.DB`
- **职责**：把 query LIKE 多列 OR 拼接。空 query 或空 columns 直接返回。

#### `oldEventIDs` / `deleteEventsByIDs` / `eventTimestampExpr`
- event 删除辅助：Pluck id + `WHERE id IN ?` Delete + 跨方言时间戳 fallback 表达式。

## 5. 依赖关系

- **内部包**：`internal/manager/biz/k8s`（接口、各种 filter 与 ref 类型）、`internal/manager/model/k8s`、`internal/pkg/errs`
- **外部库**：`gorm.io/gorm`、`gorm.io/gorm/clause`、`context`、`errors`、`fmt`、`strings`、`time`
- **被调用方**：`internal/manager/biz/k8s` usecase；topology 镜像（UpdateClusterTopologyNode / UpdateDeviceTopologyNode）

## 6. 并发与资源管理

- **无显式锁**：依赖 ON CONFLICT 原子 upsert + 乐观 WHERE。
- **事务**：`BindControllerEnrollment` / `DeleteCluster` 单事务。
- **批量 upsert**：`CreateInBatches(&items, 200)` 限 200 行/批。
- **ctx 透传**：所有方法首参 ctx。

## 7. 设计模式与亮点

- **BindClusterUID 三态结果**：bound / conflict / mismatch，乐观 WHERE + duplicate key 检测 + First 验证。
- **BindControllerEnrollment 单事务三表 upsert**：cluster + installation + telemetry_credential 原子完成。
- **ON CONFLICT 批量 upsert**：Node/Workload/Pod/Event 用 ON CONFLICT 复合列 + CreateInBatches 200 行/批。
- **eventTimestampExpr 跨方言 fallback**：`COALESCE(last_timestamp, event_time, first_timestamp, last_seen_at, created_at)` 处理 event 时间戳字段不一致。
- **ListEdgeAttachments UNION ALL**：raw SQL 聚合三源 edge 挂载，外层 COUNT + 分页。
- **topology 镜像双写**：UpdateClusterTopologyNode + UpdateDeviceTopologyNode，k8s repo 跨表写 devices（显式 deleted_at 过滤）。
- **stale 清理**：ListStaleNodes + DeleteStaleWorkloads/Pods/Events 按 last_seen_at 清理。
- **IssueOnly filter**：Pod/Event 按 phase/reason/type 过滤问题行。
- **applyLikeAny 多列 OR LIKE**：Query 字段跨多列 OR 拼接，避免 N 次 LIKE 查询。

## 8. 注意事项

- **BindClusterUID 三态**：caller 需区分 bound / conflict / mismatch，错误信息不同。
- **BindControllerEnrollment 校验 cluster_id**：installation / credential 的 cluster_id 必须与 id 一致，否则 `ErrInvalid`。
- **UpsertNode edge_id/device_id 可选**：nil 时不写入 assignments，避免覆盖现有值。
- **DeleteCluster 级联**：子表全部按 cluster_id 删，无 FK；新增子表需同步更新。
- **ListEdgeAttachments raw SQL**：UNION ALL 三源，性能依赖各表 controller_edge_id / edge_id 索引。
- **GetClusterIDByEdgeID UNION ALL**：同上，多源聚合。
- **UpdateDeviceTopologyNode 跨表**：k8s repo 写 devices 表，显式 `deleted_at IS NULL` 绕过 scope；如 device 软删语义变化需同步更新。
- **eventTimestampExpr COALESCE**：event 时间戳字段优先级 last_timestamp > event_time > first_timestamp > last_seen_at > created_at；扩展 event 模型需同步。
- **CreateInBatches 200**：批量 upsert 限 200 行/批；超大 batch caller 需分块。
- **DeleteEventsBefore / DeleteOldestEvents limit**：limit ≤ 0 返回 0；caller 需传正 limit。
- **ListNodesByRefs OR 拼接**：refs 多时 OR predicate 长；大 refs 列表需考虑分块。
