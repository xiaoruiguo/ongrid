# `inventory.go` 技术实现文档

> 源文件：`internal/edgeagent/k8s/inventory.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/k8s`

## 1. 概述

本文件是 edge agent Kubernetes inventory 子系统的核心：定义 `InventoryPusher`，负责周期性采集集群资源快照（nodes / pods / events / workloads）、按需启动 watch 增量推送、把快照/增量切分为 chunk 并通过 tunnel 推送到 manager。同时定义 in-cluster `apiClient`、所有 K8s 资源的本地 JSON 模型、分页 list、watch 解码等基础设施。

## 2. 包信息

- **包名**：`k8s`
- **所属模块**：`internal/edgeagent/k8s`（edgeagent 域的 Kubernetes 适配层，是本包的"主文件"）
- **依赖方向**：被 edgeagent 启动入口调用 `NewInventoryPusher` + `Run`；调用 `internal/pkg/tunnel` 推送 inventory；调用 `internal/pkg/k8sredact` 做敏感字段脱敏；为本包其他文件（actions/readonly/metrics 等）提供 `apiClient` 与全部 JSON 模型。

## 3. 关键类型与接口

```go
// InventoryPusher 是 inventory 推送器的主结构
type InventoryPusher struct {
    client   tunnel.Client
    info     tunnel.KubernetesInfo
    edgeID   func() uint64
    interval time.Duration
    log      *slog.Logger
    api      *apiClient
    watch    bool
}

// inventorySnapshot 是一次完整采集的内存快照
type inventorySnapshot struct {
    scope             string // "cluster" 或 "namespace"
    namespace         string
    resourceVersion   string
    resourceVersions  map[string]string
    collectDurationMS int64
    collectedAt       int64
    nodes             []tunnel.KubernetesNodeSnapshot
    workloads         []tunnel.KubernetesWorkloadSnapshot
    pods              []tunnel.KubernetesPodSnapshot
    events            []tunnel.KubernetesEventSnapshot
}

// inventoryWatchTrigger 描述一次 watch 事件累积成的增量推送内容
type inventoryWatchTrigger struct {
    reason           string
    observedAt       time.Time
    count            int
    syncType         string
    fullResync       bool
    resourceVersion  string
    resourceVersions map[string]string
    nodes            []tunnel.KubernetesNodeSnapshot
    workloads        []tunnel.KubernetesWorkloadSnapshot
    pods             []tunnel.KubernetesPodSnapshot
    events           []tunnel.KubernetesEventSnapshot
    deletedNodes     []tunnel.KubernetesNodeRef
    deletedWorkloads []tunnel.KubernetesWorkloadRef
    deletedPods      []tunnel.KubernetesPodRef
    deletedEvents    []tunnel.KubernetesEventRef
}

// inventoryWatchSpec 描述一个 watch 目标（name/apiPath/resourceVersion/resource/workloadKind）
type inventoryWatchSpec struct {
    name            string
    apiPath         string
    resourceVersion string
    resource        string
    workloadKind    string
}

// apiClient 是 in-cluster Kubernetes API 的轻量 HTTP 客户端
type apiClient struct {
    baseURL   string
    token     string
    namespace string
    http      *http.Client
}

// 一系列 K8s 资源的本地 JSON 解码模型：objectMeta / listMeta / ownerRef /
// k8sCondition / nodeItem / workloadItem / podItem / eventItem / containerStatus 等
```

## 4. 关键函数与流程

### `NewInventoryPusher`
- **签名**：`func NewInventoryPusher(client, info, edgeID, interval, watchEnabled, log) (*InventoryPusher, error)`
- **职责**：构造推送器，做必填校验与默认值兜底。
- **流程**：校验 `client`、`ClusterID`；`interval<=0` 用 `defaultInventoryInterval(30s)`；`edgeID==nil` 兜底返回 0；`log==nil` 用 `slog.Default()`；调用 `newInClusterAPIClient()` 构造 API 客户端。
- **错误处理**：任一必填缺失返回明确错误。

### `Run`
- **签名**：`func (p *InventoryPusher) Run(ctx context.Context) error`
- **职责**：inventory 推送主循环。
- **流程**：
  1. 仅 controller role 运行（`isControllerRole`）。
  2. 启动期等待 `edgeID()!=0`，首次就绪后立即做一次 full sync（`pushOnceWithSnapshot`）。
  3. 创建 `inventoryCache` 与 `inventoryWatchAccumulator`；用 `sync.Once` 启动 watch goroutine（`runWatchTriggers`）。
  4. 主 select：watch trigger 通知（先 `waitForWatchDebounce` 防抖 2s，fullResync 走 full sync 否则 `pushDelta`）/ ticker 周期 full sync。
- **错误处理**：非 `context.Canceled` 错误记 warn 日志并 continue；watch 错误在 `watchResourceLoop` 内重试。

### `pushOnceWithSnapshot`
- **签名**：`func (p *InventoryPusher) pushOnceWithSnapshot(ctx, edgeID, trigger) (*inventorySnapshot, error)`
- **职责**：完整采集 → 切 chunk → 逐 chunk 推送。
- **流程**：`collect` → `buildInventorySnapshotChunks`（在 `inventory_chunk.go`）→ 对每个 chunk `client.Call(MethodPushK8sInventory)` 带 20s 超时 → 累加 accepted 计数 → info 日志。
- **错误处理**：单 chunk 失败包装 `push kubernetes inventory chunk %d/%d: %w`。

### `pushDelta`
- **签名**：`func (p *InventoryPusher) pushDelta(ctx, edgeID, snap, trigger) error`
- **职责**：把 watch 增量打包成单次 `MethodPushK8sInventory` 调用（SyncType=delta）。
- **流程**：trigger 为空直接返回；构造 request 带 `SyncType=inventorySyncDelta` 与 deleted* 字段；20s 超时调用 → info 日志。
- **错误处理**：失败直接返回 err（由 Run 决定是否记日志并 continue）。

### `runWatchTriggers`
- **签名**：`func (p *InventoryPusher) runWatchTriggers(ctx, snap, cache, triggers)`
- **职责**：并发 watch 多个资源。
- **流程**：`watchSpecs(snap)` 生成 specs → 每个 spec 起 goroutine 跑 `watchResourceLoop` → `WaitGroup` 等待全部退出。
- **错误处理**：defer recover 防止 panic 整个推送器；每个 worker 内也 recover 防止单个资源 watch panic 拖垮全部。

### `watchResourceLoop`
- **签名**：`func (p *InventoryPusher) watchResourceLoop(ctx, spec, cache, triggers)`
- **职责**：对单个资源做长连接 watch，事件经 `cache.applyWatchEvent` 后 `triggers.add`。
- **流程**：
  1. 维护本地 `resourceVersion`，调用 `api.watch`，回调中更新 RV、处理 BOOKMARK、把 event 应用到 cache 并生成 trigger。
  2. 正常退出/ctx 取消：重置 retry。
  3. `errForbidden`/`errNotFound`：该资源永久禁用 watch，返回。
  4. `errResourceExpired`：清空 RV，发 fullResync trigger。
  5. 其他错误：指数退避（`defaultWatchRetry` 2s 起，翻倍至 `maxWatchRetry` 30s）。
- **错误处理**：用 `time.NewTimer` + select ctx.Done 控制退避并避免 timer 泄漏。

### `watchSpecs` / `workloadWatchSpecs`
- 根据 snapshot scope（cluster/namespace）生成 nodes/pods/events/workloads 的 watch 路径与初始 RV。

### `collect`
- **职责**：完整采集一次快照。
- **流程**：listNodes → listPods（集群级 forbidden 则降级到 namespace 级）→ listEvents（同样降级）→ collectWorkloads（同降级）。
- **错误处理**：`errForbidden` 时把 scope 改为 namespace 并重试；其他错误直接返回。
- **defer**：记录采集耗时与 `resourceVersion`（取所有 RV 的最大值）。

### `listNodes` / `listPods` / `listEvents` / `listWorkloads`
- 各自调用 `listAllK8sItems[T]` 分页 list，再把本地模型映射为 `tunnel.Kubernetes*Snapshot`，并对敏感字段做 `k8sredact.StringMap` / `k8sredact.Text` 脱敏。

### `listAllK8sItems[T]`
- **签名**：`func listAllK8sItems[T any](ctx, c, apiPath) ([]T, string, error)`
- **职责**：泛型分页 list，使用 `limit=500` + `continue` token 翻页，返回全部 items 与首页 RV。
- **流程**：循环 `c.get` 直到 `metadata.continue` 为空；只记录首次 RV（list 的 RV 在分页期间保持不变）。

### `(*apiClient).watch`
- **签名**：`func (c *apiClient) watch(ctx, apiPath, resourceVersion string, onEvent func(k8sWatchEvent) error) (string, error)`
- **职责**：长连接 watch，流式 JSON 解码每个 event，回调 onEvent。
- **流程**：`watchAPIPath` 拼 `?watch=1&allowWatchBookmarks=true&timeoutSeconds=10&resourceVersion=...` → GET → 状态码映射（403/404/410=sentinel）→ `json.NewDecoder(resp.Body).Decode` 循环 → EOF 正常返回 latest RV。
- **错误处理**：每个 event 调 `watchEventError` 把 `ERROR` 类型 event 映射成 sentinel 错误。

### `newInClusterAPIClient`
- **职责**：从环境变量与 ServiceAccount 目录构造 in-cluster 客户端。
- **流程**：读 `KUBERNETES_SERVICE_HOST/PORT`（默认 `kubernetes.default.svc:443`）；读 token；读 ca.crt 构造 cert pool；克隆 `http.DefaultTransport` 设置 TLSConfig（MinVersion TLS1.2）；读 namespace 文件。

### 资源模型与辅助函数
- `objectMeta` / `listMeta` / `ownerRef` / `k8sCondition` / `nodeItem` / `workloadItem` / `podItem` / `podStatus` / `eventItem` / `containerStatus`：本地 JSON 解码模型。
- `controllerOwner`：从 OwnerReferences 找 controller（`*controller=true`），否则取第一个。
- `podRestartCount` / `podReason` / `desiredReplicas` / `readyReplicas`：从原始模型派生业务字段（如 DaemonSet 用 `DesiredNumberScheduled`/`NumberReady`，Job 用 `Succeeded`）。
- `recordResourceVersion` / `resourceVersionKey` / `latestResourceVersion` / `newerResourceVersion`：RV 字典管理与比较（数字优先比较，非数字降级为字符串比较）。

## 5. 依赖关系

- **内部包**：
  - `github.com/ongridio/ongrid/internal/pkg/tunnel`：`Client`、`KubernetesInfo`、所有 `Kubernetes*Snapshot/Ref` 类型、`MethodPushK8sInventory`、`KubernetesInventoryRequest/Response`。
  - `github.com/ongridio/ongrid/internal/pkg/k8sredact`：`StringMap`、`Text` 做敏感字段脱敏。
- **外部库**：标准库 `net/http`、`crypto/tls`、`crypto/x509`、`encoding/json`、`io`、`log/slog`、`net/url`、`os`、`path/filepath`、`strconv`、`strings`、`sync`、`time`、`context`、`errors`、`fmt`。
- **被调用方**：
  - edgeagent 启动入口（构造并 Run）。
  - 本包 `actions.go` / `readonly.go` / `metrics.go` 等复用 `apiClient`、JSON 模型、辅助函数。

## 6. 并发与资源管理

- **多 goroutine watch**：`runWatchTriggers` 为每个 spec 起一个 goroutine，通过 `sync.WaitGroup` 等待；每个 goroutine 内 defer recover 隔离 panic。
- **`sync.Once` 启动 watch**：`Run` 中 `startWatch` 用 `sync.Once` 保证 watch 只启动一次（即使后续 full sync 重新设置 snap，也不重复起 watch）。
- **accumulator 与 cache 自带锁**：`inventoryWatchAccumulator` 与 `inventoryCache`（在 `inventory_delta.go` 中）使用 `sync.Mutex` 保护自身状态，watch goroutine 与主循环安全共享。
- **HTTP client 共享**：`apiClient.http` 是 `*http.Client`，标准库保证并发安全，被 list/watch/action 共用。
- **context 贯穿**：所有 IO 都带 ctx；watch 用 `context.WithTimeout` 派生单次 watch 超时（`watchTimeoutSeconds=10`），drain/delta 推送各自派生超时。
- **timer 资源管理**：`watchResourceLoop` 的退避 timer 在 ctx 取消路径显式 Stop 并 drain `timer.C`。

## 7. 设计模式与亮点

- **泛型分页 list**：`listAllK8sItems[T]` 用 Go 1.18+ 泛型统一所有资源的分页拉取，消除重复代码。
- **full sync + watch delta 混合模型**：周期 full sync 作为基线，watch 提供低延迟增量；resource expired 时自动触发 fullResync 保证一致性。
- **降级策略**：`collect` 在集群级 list forbidden 时自动降级到 namespace 级，适配 RBAC 受限的部署形态。
- **RV 一致性管理**：每个资源独立维护 RV（`resourceVersions` map），用 `resourceVersionKey(resource, namespace)` 区分；`latestResourceVersion` 提供聚合视图。
- **脱敏内嵌**：list 阶段就调用 `k8sredact` 对 labels/annotations/events message 脱敏，避免敏感数据进入上游。
- **accumulator + debounce**：watch 事件先入 accumulator 合并，主循环用 `waitForWatchDebounce` 做防抖，避免高频事件打爆推送通道。
- **sentinel error 体系**：`errForbidden`/`errNotFound`/`errResourceExpired` 三个 sentinel 配合 `errors.Is`，让上层能针对不同失败做不同决策（永久禁用/重置 RV/退避重试）。

## 8. 注意事项

- **首次 edgeID 就绪前 busy-wait**：`Run` 启动期用 `time.After(time.Second)` 轮询 `edgeID()`，未就绪前不进入主循环；若 edgeID 长期为 0 会持续空转，需上层确保 edgeID 最终可获取。
- **watch 永久禁用后无恢复**：`errForbidden`/`errNotFound` 会直接 return，该资源后续不再 watch，需要等下一次 full sync 周期重新尝试启动。
- **`listAllK8sItems` 只记录首页 RV**：这是 K8s API 的语义（list RV 在分页期间不变），但若期间资源发生变更，watch 用该 RV 起点可能丢失事件——K8s 的 watch 通过 `resourceVersionExpired` 机制兜底，本代码也处理了 410。
- **`collect` 的降级仅触发一次**：从 cluster 降到 namespace 后，后续资源 list 都用 namespace，但若首个资源 list 成功而第二个 forbidden，scope 会被改写但已成功资源不会重新 list，可能出现 scope 字段与实际数据不完全一致的边缘情况（实际场景下 RBAC 通常一致）。
- **`newInClusterAPIClient` 读 token 失败即返回错误**：在非 Pod 环境或 ServiceAccount 未挂载时直接失败，调用方需处理。
- **HTTP client 超时 15s**：`apiClient.http.Timeout=15s`，对长连接 watch 不构成影响（watch 用 ctx 控制），但常规 list 在大集群下可能超时。
- **`maxWatchRetry=30s`**：退避上限 30s，长时间网络故障时 watch 恢复较慢，可考虑配置化。
