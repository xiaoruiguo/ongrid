# `service.go` 技术实现文档

> 源文件：`internal/manager/service/k8s/service.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/service/k8s`

## 1. 概述

本文件是 Kubernetes 集群管理的 manager 应用服务层 shim，纯委托 `biz/k8s.Usecase`，无自身业务逻辑。通过 type alias 把 biz 层的输入/输出类型重新导出，让 HTTP handler 依赖 service 包而非 biz 包，保持分层。红线：service 层不做校验、不做状态机 —— 所有业务规则在 biz；service 仅是 thin wrapper。

## 2. 包信息

- **包名**：`k8s`
- **所属模块**：`internal/manager/service/k8s`
- **依赖方向**：被 HTTP handler + frontierbound handlers 调用；依赖 `biz/k8s`、`model/k8s`、`internal/pkg/tunnel`

## 3. 关键类型与接口

```go
// Type aliases — 重新导出 biz 类型，让 handler 不依赖 biz 包
type CreateClusterInput = biz.CreateClusterInput
type ClusterRegistration = biz.ClusterRegistration
type ListClustersFilter = biz.ListClustersFilter
type DeleteClusterInput = biz.DeleteClusterInput
type ListNodesFilter = biz.ListNodesFilter
type ListPodsFilter = biz.ListPodsFilter
type ListWorkloadsFilter = biz.ListWorkloadsFilter
type ListEventsFilter = biz.ListEventsFilter
type EnrollInput = biz.EnrollInput
type EnrollResult = biz.EnrollResult
type InventoryResult = biz.InventoryResult
type NodeCoverage = biz.NodeCoverage
type ClusterHealthSummary = biz.ClusterHealthSummary
type EdgeAttachment = biz.EdgeAttachment

type Service struct {
    uc *biz.Usecase
}
```

## 4. 关键函数与流程

### 构造

- **`New(uc *biz.Usecase) *Service`**：唯一构造器；uc 必非 nil（无 stub 变体）。

### 集群管理

- **`CreateCluster(ctx, in)`** → `uc.CreateCluster`：返回 `*ClusterRegistration`（含 bootstrap token）。
- **`ListClusters(ctx, f)`** / **`CountClusters(ctx, f)`** / **`GetCluster(ctx, id)`**：查询类纯委托。
- **`RotateBootstrapToken(ctx, id)`**：返回新 `*ClusterRegistration`。
- **`DeleteCluster(ctx, in DeleteClusterInput)`**：删除集群。
- **`UpgradeCommand(cluster)`**：返回升级命令字符串（非 IO，纯计算）。

### 节点 / 工作负载 / Pod / Event 查询

- **`ListNodes(ctx, clusterID)`** / **`CountNodes(ctx, clusterID)`** / **`GetNodeCoverage(ctx, clusterID)`** / **`GetNodeCoverageByClusterIDs(ctx, clusterIDs)`** / **`ListNodesPage(ctx, f)`**：节点查询。
- **`ListWorkloads(ctx, f)`** / **`CountWorkloads(ctx, f)`**：工作负载。
- **`ListPods(ctx, f)`** / **`CountPods(ctx, f)`**：Pod 查询。
- **`ListEvents(ctx, f)`** / **`CountEvents(ctx, f)`**：K8s event 查询。
- **`ListEdgeAttachments(ctx, limit, offset)`**：edge 与 cluster 的关联（分页）。
- **`GetClusterHealth(ctx, clusterID)`**：返回 `ClusterHealthSummary`。

### Enroll / Inventory / Telemetry

- **`Enroll(ctx, in EnrollInput)`** → `*EnrollResult`：edge 注册到 cluster。
- **`IngestInventory(ctx, edgeID, in tunnel.KubernetesInventoryRequest)`**：
  - 调 `uc.IngestInventory` 返回 `*InventoryResult`。
  - err → 返回 `(0,0,0,0,err)`。
  - 否则展开 `out.AcceptedNodes/Workloads/Pods/Events` 为四个返回值。
  - 注：签名返回 4 个 int + error，适配 `KubernetesInventoryIngester` 接口。
- **`RefreshTelemetryConfig(ctx, controllerEdgeID, proof biz.TelemetryCredentialProof)`** → `*biz.TelemetryConfig`：刷新遥测凭据配置。

### K8s Registry（被 frontierbound 调用）

- **`HandleRegister(ctx, edgeID, deviceID *uint64, info tunnel.KubernetesInfo)`**：注册 K8s 元信息；deviceID 可 nil（controller role）。
- **`LookupControllerCluster(ctx, edgeID)`** → `(uint64, error)`：查 edge 是否是某 cluster 的 controller。
- **`HandleControllerHeartbeat(ctx, edgeID)`**：controller 心跳。
- **`ManagedClusterIDForEdge(ctx, edgeID)`** → `(clusterID uint64, managed bool, err error)`：实现 `edge.ManagedEdgeGuard` 接口，用于 edge service 的 `rejectManagedMutation`。

## 5. 依赖关系

- **内部包**：`biz/k8s`、`model/k8s`、`internal/pkg/tunnel`
- **外部库**：仅 `context`
- **被调用方**：HTTP handler（cluster / node / workload / pod / event CRUD）；frontierbound handlers（HandleRegister / LookupControllerCluster / HandleControllerHeartbeat / IngestInventory / ManagedClusterIDForEdge）

## 6. 并发与资源管理

- **无共享可变状态**：Service 仅持 uc 引用；并发安全依赖 biz.Usecase。
- **ctx 透传**：所有方法首参 `context.Context`。

## 7. 设计模式与亮点

- **纯 thin shim**：注释明示"service-layer shim over biz.Usecase"；无校验、无 DTO 翻译、无状态机。
- **Type alias 重导出**：通过 `type X = biz.X` 让 HTTP handler 依赖 service 包类型而非 biz，保持依赖方向 `handler → service → biz`，避免 handler 直接 import biz。
- **IngestInventory 适配接口**：`KubernetesInventoryIngester.IngestInventory` 返回 4 个 int + error；本方法签名匹配，供 frontierbound Wiring 注入。
- **ManagedClusterIDForEdge 多角色**：同时满足 `edge.ManagedEdgeGuard` 与 frontierbound 的 K8s registry 需求。

## 8. 注意事项

- **无 stub 构造**：与 alert.Service 的 NewStub 不同，本包无 stub；测试需注入 fake biz.Usecase 或用接口替换。
- **IngestInventory 返回 4 int**：与 `tunnel.KubernetesInventoryResponse` 的 AcceptedNodes/Workloads/Pods/Events 对应；调用方 frontierbound 直接组装 response。
- **HandleRegister 的 deviceID 是 `*uint64`**：controller role 传 nil；node role 传解析后的 host device_id。
- **UpgradeCommand 非 IO**：纯计算，可在任意 ctx 调用（实际仍需 ctx 参数遵循规范）。
- **type alias 不隔离**：alias 后 handler 仍可访问 biz 类型的所有字段；若需隔离应改用 wrapper struct（当前未做）。
- **依赖 biz 的具体类型 `*biz.Usecase`**：非接口，测试需 mock 整个 Usecase 或用 fake biz 实现。
