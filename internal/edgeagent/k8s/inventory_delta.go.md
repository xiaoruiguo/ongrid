# `inventory_delta.go` 技术实现文档

> 源文件：`internal/edgeagent/k8s/inventory_delta.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/k8s`

## 1. 概述

本文件实现 inventory watch 增量的状态管理：`inventoryCache` 维护当前资源快照的内存索引，`applyWatchEvent` 把 watch 事件应用到 cache 并产出待推送的 `inventoryWatchTrigger`。同时定义 snapshot 与 ref 的 key 生成规则、从原始 K8s item 构造 `tunnel.Kubernetes*Snapshot` 的转换函数。它是 watch 增量推送路径的核心，保证 cache 与推送内容的一致性。

## 2. 包信息

- **包名**：`k8s`
- **所属模块**：`internal/edgeagent/k8s`（edgeagent 域的 Kubernetes 适配层）
- **依赖方向**：被 `inventory.go` 的 `watchResourceLoop` 调用 `applyWatchEvent`；依赖 `internal/pkg/tunnel` 的 snapshot/ref 类型与本包的 JSON 模型（`nodeItem`/`podItem`/`workloadItem`/`eventItem` 在 `inventory.go` 中定义）。

## 3. 关键类型与接口

```go
// inventoryCache 是 inventory 的内存索引，按资源类型分桶维护
type inventoryCache struct {
    mu        sync.Mutex
    scope     string
    namespace string
    nodes     map[string]tunnel.KubernetesNodeSnapshot
    workloads map[string]tunnel.KubernetesWorkloadSnapshot
    pods      map[string]tunnel.KubernetesPodSnapshot
    events    map[string]tunnel.KubernetesEventSnapshot
}

// watchObjectRef 是从 watch event metadata 提取的轻量引用
type watchObjectRef struct {
    namespace string
    name      string
    uid       string
}

// 常量：syncType 与 watch resource 分类
const (
    inventorySyncFull  = "full"
    inventorySyncDelta = "delta"
    watchResourceNodes     = "nodes"
    watchResourceWorkloads = "workloads"
    watchResourcePods      = "pods"
    watchResourceEvents    = "events"
)
```

## 4. 关键函数与流程

### `newInventoryCache` / `reset`
- **签名**：`func newInventoryCache(snap *inventorySnapshot) *inventoryCache`；`func (c *inventoryCache) reset(snap *inventorySnapshot)`
- **职责**：构造 cache；用一次 full snapshot 重建所有桶。
- **流程**：加锁 → 清空四个 map → 遍历 snap 中每种资源，用 `*SnapshotKey` 生成 key 写入 map。
- **错误处理**：key 为空的 item 被跳过（不写入）。

### `applyWatchEvent`
- **签名**：`func (c *inventoryCache) applyWatchEvent(spec inventoryWatchSpec, event k8sWatchEvent, observedAt time.Time) (inventoryWatchTrigger, error)`
- **职责**：watch 事件分发入口。
- **流程**：
  1. 过滤空 type 与 BOOKMARK（返回空 trigger）。
  2. 构造 trigger，reason=`<spec.name>:<eventType>`，syncType=delta，记录 RV 与 `resourceVersions[spec.name]`。
  3. ADDED/MODIFIED → `applyWatchUpsert`；DELETED → `applyWatchDelete`；其他返回空。
- **错误处理**：空 type/bookmark 不视为错误。

### `applyWatchUpsert`
- **签名**：`func (c *inventoryCache) applyWatchUpsert(spec, event, trigger) (inventoryWatchTrigger, error)`
- **职责**：按 `spec.resource` 分发到 nodes/pods/events/workloads 的 upsert 路径。
- **流程**（以 nodes 为例）：
  1. `unmarshalWatchObject(event, &item)` 解码为 `nodeItem`。
  2. `nodeSnapshotFromItem(item)` 转成 `tunnel.KubernetesNodeSnapshot`。
  3. 校验 key 非空。
  4. 加锁写 map。
  5. trigger.nodes = [snap]（单元素切片）。
- **错误处理**：unmarshal 失败包装为 `decode kubernetes watch object: %w`；key 为空返回空 trigger（不报错）。

### `applyWatchDelete`
- **签名**：`func (c *inventoryCache) applyWatchDelete(spec, event, trigger) (inventoryWatchTrigger, error)`
- **职责**：按 `spec.resource` 分发删除路径。
- **流程**：
  1. `watchObjectRefFromEvent` 从 event 中提取 namespace/name/uid。
  2. 构造对应的 `Kubernetes*Ref`。
  3. 校验 ref 非空（name 与 uid 都空则跳过）。
  4. 加锁 `delete(map, key)`。
  5. trigger.deleted* = [ref]。
- **错误处理**：ref 为空返回空 trigger；workloadRef 要求 `Kind` 与 `Name` 都非空。

### `watchObjectRefFromEvent` / `unmarshalWatchObject`
- **职责**：从 `k8sWatchEvent.Object`（`json.RawMessage`）提取 metadata 引用；统一解码入口。
- **错误处理**：Object 为空返回 `kubernetes watch event object is empty`。

### `nodeSnapshotFromItem` / `workloadSnapshotFromItem` / `podSnapshotFromItem` / `eventSnapshotFromItem`
- **职责**：把 K8s 原始 JSON 模型转换为 `tunnel.Kubernetes*Snapshot`。
- **流程**：直接字段映射；`workloadSnapshotFromItem` 复用 `desiredReplicas`/`readyReplicas`（在 `inventory.go` 中）；`podSnapshotFromItem` 复用 `controllerOwner`/`podRestartCount`/`podReason`。
- **注意**：这些转换函数与 `inventory.go` 中 list 路径的转换逻辑**重复**（list 内联转换，这里抽成函数），是已知的小幅重复，便于 watch 路径复用。

### Key 生成函数
- `nodeSnapshotKey` / `workloadSnapshotKey` / `podSnapshotKey` / `eventSnapshotKey`：snapshot → key，内部转成 ref 再调 refKey。
- `nodeRefKey`：`uid:<uid>` 优先，否则 `name:<name>`。
- `workloadRefKey`：`<kind>|<namespace>|<name>`（要求 kind 与 name 非空）。
- `podRefKey`：`<ns>|<name>|<uid>` 优先，否则 `<ns>|<name>`。
- `eventRefKey`：`uid:<uid>` 优先，否则 `<ns>|<name>`。

## 5. 依赖关系

- **内部包**：
  - `github.com/ongridio/ongrid/internal/pkg/tunnel`：所有 `Kubernetes*Snapshot` 与 `Kubernetes*Ref` 类型。
  - 本包 `inventory.go`：`inventorySnapshot`、`inventoryWatchSpec`、`inventoryWatchTrigger`、`newInventoryWatchTrigger`、`eventResourceVersion`、JSON 模型与 `conditionMaps`/`controllerOwner`/`podRestartCount`/`podReason`/`desiredReplicas`/`readyReplicas`。
  - 本包 `inventory_watch_accumulator.go`：`mergeInventoryWatchTrigger` 会消费 trigger 字段。
- **外部库**：标准库 `encoding/json`、`fmt`、`strings`、`sync`、`time`。
- **被调用方**：`inventory.go` 的 `watchResourceLoop` 回调。

## 6. 并发与资源管理

- **`sync.Mutex` 保护**：`inventoryCache.mu` 保护四个 map 与 scope/namespace 字段。每次 upsert/delete 都在临界区内完成（map 写入 + delete），粒度细到单个操作。
- **读路径无锁**：本文件不提供读 cache 的公共方法（仅 `reset` 与 `applyWatch*`），因此无需读写锁；上游若需要读 cache 需自行加锁或通过 trigger 数据。
- **单 producer 单 consumer**：watch goroutine 是唯一写者；主循环通过 accumulator 消费 trigger，不直接读 cache。

## 7. 设计模式与亮点

- **key 优先级**：所有 refKey 优先用 UID（稳定不变），UID 缺失时降级到 name（或 name+namespace）。这种设计兼顾了 K8s 资源 UID 普遍存在但理论上可能缺失的现实，保证 cache 命中率。
- **upsert 语义**：ADDED 与 MODIFIED 走相同路径（map 覆盖写），简化事件处理。
- **trigger 单元素切片**：每次 event 产出的 trigger 只含一个 item，由 `inventory_watch_accumulator.go` 的 `mergeInventoryWatchTrigger` 聚合后批量推送。
- **snapshot 与 ref 的对称设计**：每种资源都有 `*SnapshotKey(snapshot)` 与 `*RefKey(ref)` 两个 key 函数，且 snapshot key 复用 ref key，保证 upsert 与 delete 用同一 key 空间。
- **空 key 跳过**：所有 key 函数在信息不足时返回空字符串，调用方据此跳过，避免污染 cache。
- **kind-aware workload key**：workload key 包含 `kind`，使 Deployment/StatefulSet 等不同 kind 但同名的工作负载能共存于同一 map。

## 8. 注意事项

- **`inventory_delta.go` 与 `inventory.go` 的转换函数重复**：`nodeSnapshotFromItem` 等与 `inventory.go` 中 `listNodes` 内联转换逻辑重复，未来若字段变更需同步修改两处，存在维护风险。
- **`applyWatchDelete` 的 ref 提取**：仅从 metadata 提取 namespace/name/uid，不依赖完整对象反序列化，对 DaemonSet 等带 status 的资源删除事件更轻量；但若 event object 字段不完整（理论可能），可能丢失信息。
- **`podRefKey` 的降级策略**：UID 缺失时用 `ns|name`，但同名的不同 pod（理论上 K8s 不允许）会冲突。K8s 保证 namespace+name 唯一，实际安全。
- **锁粒度**：每个 upsert/delete 单独加锁，高频 watch 事件下锁竞争可能成为瓶颈；若 watch QPS 极高可考虑批量加锁优化。
- **`eventRefKey` 的降级**：UID 优先，否则 `ns|name`。K8s Event 的 name 通常是 `involvedObject-kind-involvedObject-name-timestamp`，namespace+name 在同 involvedObject 多次事件下可能重复（每次 Event 是独立对象，name 含随机后缀），实际冲突概率低。
- **无 cache 大小限制**：`inventoryCache` 不做内存上限保护，大集群下 cache 可能占用较多内存；依赖 full sync 周期性 reset 来释放（reset 会重建 map）。
