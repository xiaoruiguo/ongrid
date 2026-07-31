# `usecase.go` 技术实现文档

> 源文件：`internal/manager/biz/k8s/usecase.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/k8s`

## 1. 概述

本文件是 K8s 限界上下文的核心用例：cluster 注册/注销、node/workload/pod/event inventory 同步、enrollment 凭据签发、telemetry 配置发布、topology 镜像。设计要点：bootstrap token 用 SHA-256 hash + constant-time compare 校验；enrollment per-cluster+role+node 串行锁；inventory atomic chunk state machine；telemetry 凭据 write-only（hash 存储，明文仅返回一次）；凭据轮换失败补偿删除已创建 edge。红线：telemetry 凭据 hash 存储，明文永不持久化；bootstrap token constant-time compare 防 timing 攻击。

## 2. 包信息

- **包名**：`k8s`
- **所属模块**：`internal/manager/biz/k8s`
- **依赖方向**：被 controlplane/http 调用；依赖 `model/k8s`、`pkg/errs`、`pkg/passwd`、`pkg/k8sredact`、`pkg/tunnel`

## 3. 关键类型与接口

```go
const (
    defaultEventRetention       = 24 * time.Hour
    defaultEventMaxPerCluster   = 5000
    defaultEventCleanupInterval = time.Hour
    eventRetentionBatchLimit    = 1000
    bootstrapTokenBytes         = 32
    defaultK8sChartRef          = "oci://helm.cnb.cool/ongridio/ongrid-edge"
    telemetryAuthModeTelemetry  = "telemetry"
    telemetryAuthModeBackend    = "backend"
)

// Repository 是 k8s 限界上下文持久化契约（96 个方法）
type Repository interface { /* cluster/node/workload/pod/event/installation/telemetry CRUD */ }

// EdgeIssuer 是 edge identity 域的窄桥接
type EdgeIssuer interface {
    CreateEdgeIdentity(ctx, name string, createdBy *uint64) (*EdgeCredential, error)
    RotateEdgeSecret(ctx, edgeID uint64) (*EdgeCredential, error)
}

// EdgeRemover 由 edge 限界上下文实现
type EdgeRemover interface {
    DeleteEdge(ctx, edgeID uint64) error
}

// TopologyMirror 是 K8s inventory 到通用 topology 图的可选桥接
type TopologyMirror interface { /* EnsureNodeForDevice / EnsureKubernetesCluster / ... */ }

type Usecase struct {
    repo               Repository
    edgeIssuer         EdgeIssuer
    edgeRemover        EdgeRemover
    topology           TopologyMirror
    remoteWrite        RemoteWriteResolver
    telemetryTargets   TelemetryTargetResolver
    cfg                Config
    enrollmentLocksMu  sync.Mutex
    enrollmentLocks    map[string]*enrollmentLock
    inventoryUploadsMu sync.Mutex
    inventoryUploads   map[uint64]inventoryUploadState
}

type enrollmentLock struct {
    mu    sync.Mutex
    users int
}
```

## 4. 关键函数与流程

### `NewUsecase`
- 构造 Usecase；cfg 默认值填充（EventRetention=24h 等）；edgeIssuer 若实现 EdgeRemover 则同时赋值 edgeRemover

### `Usecase.CreateCluster`
- **签名**：`func (u *Usecase) CreateCluster(ctx, in CreateClusterInput) (*ClusterRegistration, error)`
- **流程**：
  1. name 校验；mode 规范化（默认 ModeFullNode）
  2. `newBootstrapToken()` 生成 controller + node token（32 字节随机 + SHA-256 hash）
  3. 构造 Cluster（status=offline）+ `repo.CreateCluster`
  4. `reconcileTopology` 失败 → 回滚 DeleteCluster（errors.Join）
  5. 返回 ClusterRegistration（含 InstallCommand）
- **错误处理**：topology 失败回滚 cluster，errors.Join 拼接两个错误

### `Usecase.Enroll`
- **签名**：`func (u *Usecase) Enroll(ctx, in EnrollInput) (*EnrollResult, error)`
- **流程**：
  1. role 规范化；`lockEnrollment` 串行锁（per cluster+role+node）
  2. `repo.GetCluster`；`validBootstrapToken` constant-time compare
  3. clusterUID 校验 + `BindClusterUID`
  4. 分派 `enrollNode` / `enrollController`
- **错误处理**：token 不匹配 → ErrUnauthorized；role 不支持 → ErrInvalid

### `Usecase.enrollController`
- **流程**：
  1. controller 已注册（ControllerEdgeID 非 0 + PodName 非空）→ ErrConflict（需 rotate token 恢复）
  2. controller 首次：`CreateEdgeIdentity`；已注册但 PodName 空：`RotateEdgeSecret`（NotFound 则重新创建）
  3. `newTelemetryCredential` 生成 telemetry 凭据（access_key=`kt_` 前缀，secret=`ks_` 前缀，hash 存储）
  4. `resolveTelemetryConfig` 解析 traces/logs/remote_write 端点
  5. `BindControllerEnrollment`（事务：controller + installation + telemetry credential）
  6. 失败补偿：`compensateCreatedEdge` 删除已创建 edge
- **错误处理**：created=true 时失败补偿删除 edge，避免孤儿

### `Usecase.RefreshTelemetryConfig`
- **签名**：`func (u *Usecase) RefreshTelemetryConfig(ctx, controllerEdgeID uint64, proof TelemetryCredentialProof) (*TelemetryConfig, error)`
- **职责**：重发端点同时保留有效凭据；仅 controller 无可用凭据时轮换
- **流程**：
  1. `GetClusterByControllerEdge`
  2. proof 校验（access_key + secret_key 都有或都无）
  3. proof 凭据有效（DB 查到 + clusterID 匹配 + passwd.Verify）→ 直接 resolveTelemetryConfig 返回
  4. 否则 `newTelemetryCredential` 轮换 + `UpsertTelemetryCredential`
- **注释**：避免每次 controller 重启都轮换，防 Secret 投影/reload 401 窗口

### `Usecase.IngestInventory`
- **签名**：`func (u *Usecase) IngestInventory(ctx, edgeID uint64, in tunnel.KubernetesInventoryRequest) (*InventoryResult, error)`
- **流程**：
  1. controller edge 校验（edgeID 必须是 cluster 的 controller）
  2. `lockEnrollment(inventory)` 串行锁
  3. `prepareInventoryChunk` 校验 chunk + 返回 (startedAt, finalChunk)
  4. delta 模式：`deleteInventoryDeltas` 删除已删资源
  5. nodes/workloads/pods/events upsert（k8sredact 脱敏 labels/annotations/message）
  6. node 重名处理：placeholder UID 替换，非 placeholder 删除旧 node
  7. 最后 chunk：full 模式清理 stale 资源（DeleteStaleWorkloads/Pods/Events）
  8. `UpdateClusterInventorySync` 更新同步状态
  9. `reconcileTopology`（full 或 node 变化时）
  10. `completeInventoryChunk` 推进/清理 chunk state
- **错误处理**：每步 `%w` 包装；k8sredact 脱敏防 PII 泄漏

### `Usecase.CleanupEvents`
- **签名**：`func (u *Usecase) CleanupEvents(ctx, now time.Time) (EventRetentionStats, error)`
- **职责**：TTL + per-cluster 上限清理 events
- **流程**：
  1. TTL 清理：`DeleteEventsBefore(cutoff, 1000)` 循环直到 0
  2. per-cluster 上限：遍历 clusters，`DeleteOldestEvents(cluster, max, 1000)` 循环直到 0
- **错误处理**：ctx.Err() 中断返回

### `Usecase.ReconcileTopology`
- 遍历所有 cluster → `reconcileTopology`；最后 `PruneDeletedKubernetesClusters`

### `Usecase.reconcileTopology`
- **流程**：
  1. `topology.EnsureKubernetesCluster` 创建/更新 cluster topology node
  2. cluster.NodeID 不匹配 → `UpdateClusterTopologyNode`
  3. `ListTopologyNodeLinks` → 遍历 device link：
     - DeviceNodeID=0 → `EnsureNodeForDevice` + `UpdateDeviceTopologyNode`
     - `EnsureKubernetesNodeMembership`
     - 收集 keep
  4. `PruneKubernetesNodeMemberships` 删除非 keep 的 membership

### `Usecase.DeleteCluster`
- **流程**：
  1. 非 force 且 online → ErrConflict（先卸载 Helm）
  2. `ListClusterEdgeIDs` + `deleteClusterEdges`
  3. `repo.DeleteCluster`
  4. `topology.DeleteKubernetesCluster`（失败容忍，周期 reconcile 清理孤儿）

### `validBootstrapToken`
- **签名**：`func validBootstrapToken(token string, c *model.Cluster, role string) bool`
- **职责**：constant-time compare 校验 token
- **流程**：选 controller/node hash；`tokenDigest(token)` = SHA-256；`subtle.ConstantTimeCompare`

### `newBootstrapToken`
- 32 字节随机 → base64.RawURLEncoding → token；SHA-256 hash

### `newTelemetryCredential`
- access_key = `kt_` + 18 字节随机；secret_key = `ks_` + 32 字节随机；`passwd.Hash(secret_key)`

### `lockEnrollment`
- **签名**：`func (u *Usecase) lockEnrollment(clusterID uint64, role, nodeName string) func()`
- **职责**：per cluster+role+node 串行锁
- **流程**：
  1. key = `clusterID:role` +（node role 加 nodeName）
  2. `enrollmentLocksMu.Lock`；取/建 lock；`lock.users++`
  3. `lock.mu.Lock`；返回 unlock func：`lock.mu.Unlock` + `users--`（0 时 delete）

### `installCommand / UpgradeCommand`
- 生成 Helm install/upgrade 命令；`shellQuote` 转义；`installEndpoints` 推导 publicURL/tunnelAddr

## 5. 依赖关系

- **内部包**：`model/k8s`、`pkg/errs`、`pkg/passwd`（bcrypt/argon2id）、`pkg/k8sredact`（labels/annotations 脱敏）、`pkg/tunnel`
- **外部库**：仅标准库
- **被调用方**：controlplane/http handler

## 6. 并发与资源管理

- **`enrollmentLocksMu`（Mutex）**：保护 enrollmentLocks map；per-key `enrollmentLock.mu` 串行化
- **`inventoryUploadsMu`（Mutex）**：保护 inventoryUploads map（定义在 inventory_upload.go）
- **enrollmentLock 引用计数**：users==0 时 delete，防 map 膨胀
- **constant-time compare**：`subtle.ConstantTimeCompare` 防 timing 攻击
- **ctx 透传**：所有 repo 调用
- **补偿事务**：created edge 失败时 `compensateCreatedEdge` 删除

## 7. 设计模式与亮点

- **bootstrap token SHA-256 + constant-time**：防 timing 攻击
- **enrollment 串行锁**：per cluster+role+node，防并发 enrollment 竞争
- **inventory chunk state machine**：严格顺序校验，防乱序/重放
- **telemetry 凭据 write-only**：hash 存储，明文仅返回一次；RefreshTelemetryConfig 保留有效凭据避免 401 窗口
- **补偿事务**：edge 创建后失败补偿删除，防孤儿
- **k8sredact 脱敏**：labels/annotations/message 入库前脱敏，防 PII
- **placeholder node UID**：`name:<nodeName>` 允许 node 重名时保留 edge binding
- **Helm install/upgrade 命令**：shellQuote 转义，支持自定义 chart version
- **topology 镜像**：K8s inventory → 通用 topology 图，可选桥接

## 8. 注意事项

- **bootstrapTokenBytes=32**：256 位随机
- **defaultEventRetention=24h**：event TTL
- **defaultEventMaxPerCluster=5000**：per-cluster event 上限
- **telemetry 凭据前缀**：`kt_`/`ks_` 便于识别
- **enrollmentLock 引用计数**：users==0 delete，防 map 膨胀
- **DeleteCluster 非 force 需 offline**：防误删在线 cluster
- **topology 失败容忍**：DeleteCluster 时 topology 失败不阻塞，周期 reconcile 清理孤儿
- **RefreshTelemetryConfig 避免轮换**：proof 有效时保留凭据，防 401 窗口
- **installChartVersion 正则校验**：仅 semver 格式才加 --version
- **k8sredact 必须**：labels/annotations/message 脱敏红线
