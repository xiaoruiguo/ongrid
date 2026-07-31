# usecase.go 技术实现文档

## 1. 概述

`usecase.go` 是 `topology` 包的 biz 层 facade，统一编排四个 repo（`NodeRepo` / `RelationRepo` / `RelationTypeRepo` / `NodeTypeRepo`）。HTTP handler 与未来 AIOps tool 通过 `Usecase` 而非直接调 repo，让所有校验集中在一处。核心职责：

- Node/Relation 的 CRUD 与 props JSON 合法性校验
- RelationType/NodeType 的注册/列表/删除，含 built-in 保护与引用计数 guard
- Device → topology 镜像（`EnsureNodeForDevice` / `DeleteNodeForDevice`）
- Kubernetes cluster → topology 镜像（`EnsureKubernetesCluster` / `EnsureKubernetesNodeMembership` / `PruneKubernetesNodeMemberships` / `DeleteKubernetesCluster` / `PruneDeletedKubernetesClusters`）

## 2. 包信息

- 包名：`topology`
- 路径：`internal/manager/biz/topology/usecase.go`
- 导入依赖：`context` / `encoding/json` / `errors` / `fmt` / `log/slog` / `strings`、`model/topology`、`internal/pkg/errs`

## 3. 关键类型与接口

### `Usecase`

```go
type Usecase struct {
    nodes     NodeRepo
    relations RelationRepo
    types     RelationTypeRepo
    nodeTypes NodeTypeRepo
    log       *slog.Logger
}
```

`NewUsecase` 接受四个 repo（任一可为 nil）+ logger。方法触摸 nil repo 时返回 `errs.ErrNotWiredYet`，让调用方在装配/测试阶段能优雅降级。

## 4. 关键函数与流程

### Node CRUD

#### `CreateNode(typ, name, propsJSON)`

- `typ` / `name` 必填（trim 后非空）
- `propsJSON` 非空时必须 `json.Valid`——不 schema 校验 shape，operator 信任 props bag
- 写入后记录 info 日志（id/type/name）

#### `UpdateNode(id, name, propsJSON)`

- `name` 必填
- `propsJSON` 校验同 Create
- **`Type` 不可变**——修改 type 会破坏下游 `device.node_id` 等 FK 引用（FK 预设 `node.type='device'`）

#### `GetNode` / `ListNodes` / `DeleteNode`

`ListNodes` 同时返回 items 与 total（调 `Count`），用于分页。`DeleteNode` 不级联——调用方需先切断所有入/出 relation，因为具体实体表（device/service/...）可能仍有 `node_id` 指向此行，孤儿化 FK 比报错更糟。

### Relation CRUD

#### `CreateRelation(srcID, dstID, typ, propsJSON)`

校验链：

1. `srcID` / `dstID` / `typ` 必填
2. `srcID != dstID`（拒绝 self-edge）
3. `propsJSON` 合法性
4. **端点存在**：`nodes.GetMany([srcID, dstID])` 一次性查两个，缺失返回 `ErrNotFound`
5. **relation type 已注册**：`types.Get(typ)`，未注册返回包装 error
6. 创建并记录日志

#### `UpdateRelation(id, propsJSON)`

仅改 props——`(src, dst, type)` 三元组是 identity，改它就是"删除重建"。

#### `ListRelations` / `DeleteRelation`

`ListRelations` 返回 items + total。

### DeviceMirror

#### `EnsureNodeForDevice(deviceID, deviceName)`

device → topology 镜像入口，由 edge register flow 调用。

- 按 `(type='device', name=deviceName)` 查找现有 Node（用 `List` + `Q` 子串匹配，再 `EqualFold` 精确过滤）
- 找到则返回其 id
- 未找到则 Create

**关键决策**：lookup keying on `(type, name)` 而非 `device.id`。注释说明：迁移 backfill 可能与实时写竞争；name 是 operator 编辑的，重名即"operator 视作同一逻辑物"。

#### `DeleteNodeForDevice(deviceID, nodeID)`

device 删除时调用：

1. 校验 `node.Type == 'device'`，否则返回 `ErrInvalid`
2. `relations.List(SrcOrDstID: nodeID)` 列出所有相关边
3. 逐条 `Delete`，`ErrNotFound` 容忍（并发删除场景）
4. `nodes.Delete(nodeID)`
5. 日志记录 device_id/node_id/relations 数

### Kubernetes Mirror

#### `EnsureKubernetesCluster(clusterID, currentNodeID, name, uid, mode, status)`

镜像一个 onboarded K8s cluster 为 `type=cluster` node。

- 构造 `kubernetesClusterPropsJSON`（含 `source: "kubernetes"` / `k8s_cluster_id` / `k8s_cluster_uid` / `mode` / `status`）
- 若 `currentNodeID` 非空：`Get` 后校验 `Type==cluster` 且 props 匹配（`topologyPropsMatchKubernetesCluster`）；name/props 变化则 `Update`，否则直接返回
- 否则 `findKubernetesClusterNode`（按 name + props 匹配）
- 仍未找到则 Create

#### `EnsureKubernetesNodeMembership(clusterNodeID, deviceNodeID, clusterID, deviceID, nodeName, nodeUID)`

镜像 K8s node 的 backing device 为 `device --member_of--> cluster`。

- 构造 `kubernetesNodeMembershipPropsJSON`
- `relations.List(SrcID: deviceNodeID, DstID: clusterNodeID, Type: RelMemberOf, Limit: 1)` 查现有边
- 存在且 props 不同则 `Update`；存在且相同则 skip
- 不存在则 `CreateRelation`

#### `PruneKubernetesNodeMemberships(clusterNodeID, clusterID, keepDeviceNodeIDs)`

清理 stale K8s-owned `member_of` 边。

- `keep` set 构造
- `relations.List(DstID: clusterNodeID, Type: RelMemberOf)` 列出所有 member_of 边
- 跳过 keep 集合中的
- 对其余边检查 props 是否 K8s-owned 且 clusterID 匹配，是则 `Delete`

**关键**：手动 `member_of` 边的 props 无 `source=kubernetes`，不会被误删。

#### `DeleteKubernetesCluster(clusterID, currentNodeID)`

删除 K8s cluster 的 topology 行：

- `findOwnedKubernetesClusterNodes` 找到所有匹配 clusterID 的 cluster node
- 对每个 node：列出所有 SrcOrDstID 关系并删除，再删除 node 本身
- **Device node 保留**——只删 cluster node 与附着的关系

#### `PruneDeletedKubernetesClusters(activeClusterIDs)`

清理 K8s inventory 中已不存在的 cluster 对应的 topology node：

- 分页（pageSize=200）列出所有 `type=cluster` node
- 用 `topologyKubernetesClusterProps` 解析 props，跳过非 K8s-owned 或无效 clusterID
- 在 `active` 集合中的跳过
- 收集 stale 列表，逐个调用 `DeleteKubernetesCluster`

### RelationType / NodeType 注册

#### `RegisterRelationType(rt)`

校验：

- `name` / `direction` / `semantics_tag` 必填
- `direction` 必须是 `src_to_dst` / `dst_to_src` / `bidirectional`
- `semantics_tag` 必须是 canonical bucket（`hard_dep` / `runtime_dep` / `aggregation` / `redundancy` / `observation` / `traffic` / `annotation`）
- 拒绝覆盖 built-in（`existing.Builtin == true` 返回 `ErrConflict`）
- 强制 `rt.Builtin = false`（operator 注册的永不 builtin）

#### `DeleteRelationType(name)`

- built-in 拒绝（`ErrConflict`）
- `CountRelationsByType > 0` 拒绝（避免孤儿化）
- 否则 `Delete`

#### `RegisterNodeType(nt)` / `DeleteNodeType(name)`

类似逻辑。`RegisterNodeType` 的 `Tier` 默认 99（catch-all bottom band），`DisplayName` 空时 fallback 到 `name`。

## 5. 依赖关系

- **四个 repo 接口**（`repo.go`）：唯一持久化依赖
- **`model/topology`**：`Node` / `Relation` / `RelationType` / `NodeType` / `NodeTypeDevice` / `NodeTypeCluster` / `RelMemberOf` / `IsValidDirection` / `IsValidSemanticsTag`
- **`errs`**：`ErrNotWiredYet` / `ErrInvalid` / `ErrNotFound` / `ErrConflict`
- **被依赖方**：HTTP handler（`internal/manager/server/topology`）、edge register flow（`EnsureNodeForDevice`）、K8s inventory 同步（`EnsureKubernetesCluster` 等）

## 6. 并发与资源管理

- `Usecase` 构造后字段只读，并发安全
- 无共享可变状态、无 goroutine
- **未加事务锁**：`CreateRelation` 的"端点存在 + type 注册 + 创建"跨多次 repo 调用，中间可能出现并发删除导致状态变化。当前依赖 repo 层（DB 约束）兜底，但 usecase 层未显式事务
- `PruneDeletedKubernetesClusters` / `findOwnedKubernetesClusterNodes` 分页迭代，单页 200 条，避免一次性加载全表

## 7. 设计模式与亮点

### props JSON 的"信任 bag"语义

`CreateNode` / `UpdateNode` / `CreateRelation` / `UpdateRelation` 对 `propsJSON` 仅做 `json.Valid` 校验，不 schema 验证 shape。注释明确"operators are trusted on the props bag"。这让 topology 表对自定义元数据完全开放——operator 可在 props 里放任何 JSON，无需迁移 schema。

### identity 字段不可变

Node 的 `Type`、Relation 的 `(src, dst, type)` 三元组均不可通过 `Update` 修改。这是为了保护下游 FK 引用的语义稳定性——`device.node_id` 预设 `node.type='device'`，若允许改 type，FK 语义会被破坏。

### built-in 保护

`RegisterRelationType` / `RegisterNodeType` 拒绝覆盖 built-in 行；`DeleteRelationType` / `DeleteNodeType` 拒绝删除 built-in 行。built-in 由 migrator 在每次启动时 seed，operator 只能新增自定义类型。这避免了 operator 误删导致系统不可用。

### 引用计数 guard

`DeleteRelationType` / `DeleteNodeType` 在删除前查 `CountRelationsByType` / `CountNodesByType`，有引用则拒绝。这是"防御性删除"——宁可拒绝也不孤儿化 `relations.type` 引用。

### K8s mirror 的"source=kubernetes" 标记

K8s 镜像的 props 都含 `source: "kubernetes"`。`PruneKubernetesNodeMemberships` 据此区分 K8s-owned 边与手动 `member_of` 边——只清理前者，保留后者。这让 operator 手动建立的关系不会被 K8s 同步逻辑误删。

### `EnsureKubernetesCluster` 的多路径查找

查找现有 cluster node 的顺序：

1. `currentNodeID`（k8s_clusters.node_id 指针）——最快路径
2. `findKubernetesClusterNode`（按 name + props 匹配）——backfill/迁移场景
3. Create

这种多路径容错让"node_id 指针丢失"或"name 变更"等异常状态能自愈。

### `PruneDeletedKubernetesClusters` 的分页迭代

用 `pageSize=200` 分页迭代所有 `type=cluster` node，避免一次性加载全表。`seen` map 防止跨页重复处理。这是处理大图的典型模式。

### DeviceMirror 的 name-keying

`EnsureNodeForDevice` 按 `(type, name)` 而非 `device.id` 查找。注释说明：迁移 backfill 可能与实时写竞争；name 是 operator 编辑的，重名即"operator 视作同一逻辑物"。这种"逻辑 id 优先于物理 id"的设计让迁移期的不一致能自然收敛。

### nil-repo 的 `ErrNotWiredYet` 降级

每个方法首行检查对应 repo 是否 nil，是则返回 `ErrNotWiredYet`。这让装配阶段（部分 repo 未注入）与测试场景（只注入被测 repo）能优雅降级，而非 panic。

## 8. 注意事项

- **无事务保护**：跨 repo 的复合操作（如 `CreateRelation` 的端点校验 + type 校验 + 创建）未包在事务中。并发场景下可能出现"端点刚被删除但 usecase 仍创建关系"的窗口期。当前依赖 DB 约束兜底，但严格场景需考虑加事务
- **`DeleteNode` 不级联**：调用方需先切断所有 relation。若直接 `DeleteNode` 一个仍被 relation 引用的 node，会留下孤儿 relation。当前 usecase 未提供"安全删除"helper
- **`EnsureNodeForDevice` 的 name 精确匹配**：`List(Q: deviceName)` 是子串匹配，再用 `EqualFold` 精确过滤。若设备名含特殊字符（如正则元字符），`Q` 子串匹配行为依赖 data 层实现
- **K8s mirror 的 `currentNodeID` 双重校验**：`Get(*currentNodeID)` 后校验 `Type==cluster` 与 props 匹配，若不匹配则 fall through 到 `findKubernetesClusterNode`。这种"不信任指针"的防御是必要的——node_id 指针可能因数据修复而失效
- **`PruneDeletedKubernetesClusters` 的全表扫描**：每次调用扫描所有 `type=cluster` node。cluster 数量大时可能有性能压力——当前 pageSize=200 分页缓解，但若 cluster > 10k 需考虑索引优化
- **`jsonUint64` 的多类型处理**：`jsonUint64` 处理 `float64` / `int` / `uint64` / `string` 四种类型，因为 JSON unmarshal 到 `map[string]any` 时数字默认是 `float64`，但 operator 手填的 props 可能是字符串——这种容错是必要的
- **`RegisterNodeType` 的 `Tier` 默认 99**：注释提示"operators meaning 'top tier' should pass tier=0 explicitly"。若 operator 不传 Tier，会落入 catch-all 底部 band，UI 排序时排在最后——这是 fail-safe 但可能不符合 operator 预期
