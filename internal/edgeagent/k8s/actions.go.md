# `actions.go` 技术实现文档

> 源文件：`internal/edgeagent/k8s/actions.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/k8s`

## 1. 概述

本文件实现 Kubernetes 集群操作（action）的执行器，承担 edge agent 侧对集群执行管控操作的职责。它把上层 `tunnel.KubernetesActionRequest` 转换成对 Kubernetes API 的具体 HTTP 调用（patch / delete / post eviction 等），覆盖 rollout restart、scale、delete/evict pod、cordon/uncordon、drain 等运维动作，并对参数进行严格校验、对冲突进行预检、对 dry-run 进行支持。

## 2. 包信息

- **包名**：`k8s`
- **所属模块**：`internal/edgeagent/k8s`（edgeagent 域下的 Kubernetes 适配层）
- **依赖方向**：被 `readonly.go` 中 `RegisterHandlers` 注册的 `MethodExecuteK8sAction` handler 调用；调用 `internal/pkg/tunnel` 中的请求/响应类型；调用本包 `apiClient` 的底层 HTTP 能力（`getRaw` / `doRaw`）。

## 3. 关键类型与接口

```go
// 动作常量，覆盖七种 Kubernetes 运维动作
const (
    k8sActionRolloutRestart = "rollout_restart"
    k8sActionScale          = "scale"
    k8sActionDeletePod      = "delete_pod"
    k8sActionEvictPod       = "evict_pod"
    k8sActionCordon         = "cordon"
    k8sActionUncordon       = "uncordon"
    k8sActionDrain          = "drain"
)

// drainOptions 聚合 drain 节点所需的全部参数（含超时、重试、DaemonSet 处理等）
type drainOptions struct {
    gracePeriodSeconds  *int
    timeoutSeconds      int
    retrySeconds        int
    ignoreDaemonSets    bool
    deleteEmptyDirData  bool
    force               bool
    disableEviction     bool
    dryRun              bool
    nodeResourceVersion string
}

// drainSummary 汇总一次 drain 的驱逐/删除/跳过计数及跳过原因列表
type drainSummary struct {
    evicted     int
    deleted     int
    skipped     int
    skippedPods []tunnel.KubernetesActionPodResult
}

// kubernetesAPIError 把非 2xx 响应包装成携带 Method/Path/StatusCode/Body 的错误
type kubernetesAPIError struct {
    Method     string
    Path       string
    StatusCode int
    Body       string
}
```

## 4. 关键函数与流程

### `executeAction`
- **签名**：`func (c *apiClient) executeAction(ctx context.Context, req tunnel.KubernetesActionRequest) (*tunnel.KubernetesActionResponse, error)`
- **职责**：单一入口，编排一个 Kubernetes action 的完整执行流程。
- **流程**：
  1. `normalizeK8sAction` 归一化 action 名称（兼容 `restart`、`delete`、`evict` 等别名及 `-`/`_` 变体）。
  2. `actionTarget` 解析目标对象：根据 action 推断默认 kind（Pod / Node），并通过 `describeSpecFor` 拿到 `k8sDescribeSpec`；按动作做 kind 校验（rollout_restart 仅 Deployment/StatefulSet/DaemonSet；scale 仅 Deployment/StatefulSet 且 replicas 在 0–10000；cordon/uncordon/drain 仅 Node 等）。
  3. 预检：`getRaw` 拉取对象原始 JSON，`k8sObjectMetadata` 提取 uid/resourceVersion；若请求带 `ExpectedResourceVersion`，则比对，不一致直接返回 conflict 错误。
  4. `normalizeGracePeriodSeconds` 校验优雅退出时长（0–3600）。
  5. 按 action 分支执行（`rolloutRestart` / `scaleWorkload` / `deletePod` / `evictPod` / `patchNodeUnschedulable` / `drainNode`）。
  6. 汇总结果：从返回体抽取 `ResultResourceVersion`，设置 `Applied`、`StartedAt`/`EndedAt`、`Message`（dry-run 时追加 "dry-run validated by Kubernetes API"）。
- **错误处理**：所有错误用 `fmt.Errorf("execute_k8s_action: ...: %w", err)` 包装，保留错误链；preflight 失败、metadata 解析失败、RV 冲突、参数越界都立即返回。

### `rolloutRestart`
- **签名**：`func (c *apiClient) rolloutRestart(ctx context.Context, apiPath, resourceVersion string, dryRun bool) ([]byte, error)`
- **职责**：通过给 pod template 注入 `kubectl.kubernetes.io/restartedAt` 注解触发滚动重启。
- **流程**：构造 merge-patch JSON → `addResourceVersionPrecondition` 注入 RV 前置条件 → `doRaw` PATCH `withDryRun(apiPath, dryRun)`。

### `scaleWorkload`
- **签名**：`func (c *apiClient) scaleWorkload(ctx context.Context, apiPath string, replicas *int, resourceVersion string, dryRun bool) ([]byte, error)`
- **职责**：通过 `spec.replicas` merge-patch 调整副本数。

### `deletePod` / `deleteOptionsBody`
- **签名**：`func (c *apiClient) deletePod(ctx, apiPath, uid, resourceVersion string, gracePeriodSeconds *int, dryRun bool) ([]byte, error)`
- **职责**：构造 `DeleteOptions`（含 uid/RV preconditions 与 gracePeriodSeconds），调用 `doRaw` DELETE。
- **错误处理**：marshal 失败包装为 `marshal delete options`。

### `evictPod`
- **签名**：`func (c *apiClient) evictPod(ctx, namespace, name, uid, resourceVersion string, gracePeriodSeconds *int, dryRun bool) ([]byte, error)`
- **职责**：构造 `policy/v1 Eviction` 对象 POST 到 `/api/v1/namespaces/{ns}/pods/{name}/eviction`，内嵌 `deleteOptions` 与 preconditions。
- **流程**：path 使用 `url.PathEscape` 防注入；支持 dryRun 查询参数。

### `patchNodeUnschedulable`
- **签名**：`func (c *apiClient) patchNodeUnschedulable(ctx, apiPath string, unschedulable bool, resourceVersion string, dryRun bool) ([]byte, error)`
- **职责**：cordon/uncordon 共用，patch `spec.unschedulable` 字段。

### `drainNode`
- **签名**：`func (c *apiClient) drainNode(ctx, apiPath, nodeName string, opts drainOptions) ([]byte, drainSummary, error)`
- **职责**：实现类 `kubectl drain` 语义。
- **流程**：
  1. `listPodsOnNode` 通过 `fieldSelector=spec.nodeName=...` 列出节点上所有 pod。
  2. 第一遍遍历做 `drainDecision`：遇到 abort 类（DaemonSet 但未设 ignoreDaemonSets）立即整体失败返回。
  3. 先 `patchNodeUnschedulable(true)` 把节点 cordon。
  4. 若 `timeoutSeconds>0`，派生 `context.WithTimeout` 作为 drainCtx。
  5. 第二遍遍历：跳过 terminal/mirror/daemonset/emptyDir 等不需要处理的 pod，并记录 `skippedPods`；按 `disableEviction` 走 `deleteNamespacedPod` 或 `evictPodWithRetry`。
- **错误处理**：drainCtx 超时返回 `eviction ... blocked by PDB until timeout: %w`。

### `drainDecision`
- **职责**：纯函数决策单个 pod 在 drain 中的处理方式。
- **规则**：Succeeded/Failed → skip；mirror/static pod → skip；DaemonSet pod → `ignoreDaemonSets=true` 时 skip，否则 abort；无 owner 且未 `force` → skip；含 emptyDir 且未 `deleteEmptyDirData` → skip；其余 drain。

### `evictPodWithRetry`
- **签名**：`func (c *apiClient) evictPodWithRetry(ctx, pod podItem, opts drainOptions) ([]byte, error)`
- **职责**：对 PDB 触发的 429 TooManyRequests 做指数式重试（固定 `retrySeconds` 间隔），直到 ctx 取消。
- **流程**：成功立即返回；非 429 立即返回；429 用 `time.NewTimer` 等待，select ctx.Done 或 timer.C。

### `doRaw`
- **签名**：`func (c *apiClient) doRaw(ctx, method, apiPath, contentType string, body []byte) ([]byte, error)`
- **职责**：所有 HTTP 调用的底层封装。
- **流程**：构造 `http.NewRequestWithContext` → 设置 `Authorization: Bearer <token>` 与 `Accept: application/json` → `c.http.Do` → 403 → `errForbidden`；404 → `errNotFound`；非 2xx → `kubernetesAPIError`（body 限 1KB）。
- **错误处理**：defer 关闭 body。

### `isKubernetesStatusError`
- 用 `errors.As` 判断 err 是否为 `*kubernetesAPIError` 且 StatusCode 匹配，给 `evictPodWithRetry` 用作 429 判定。

### `withDryRun` / `actionMessage` / `k8sObjectMetadata`
- 工具函数：dryRun 查询参数追加、人类可读 message 构造、从 raw JSON 提取 uid/RV。

## 5. 依赖关系

- **内部包**：
  - `github.com/ongridio/ongrid/internal/pkg/tunnel`：`KubernetesActionRequest/Response/Preflight/PodResult` 等协议类型。
  - 本包内的 `apiClient`、`describeSpecFor`、`sanitizeK8sObject`、`controllerOwner`、`podItem`、`podList`、`listPodsOnNode`、`stringField` 等（在 `inventory.go` / `readonly.go` 中定义）。
- **外部库**：标准库 `net/http`、`net/url`、`encoding/json`、`bytes`、`io`、`time`、`strings`、`errors`、`fmt`、`context`。
- **被调用方**：`readonly.go` 的 `RegisterHandlers` 注册的 `MethodExecuteK8sAction` handler。

## 6. 并发与资源管理

- 无 goroutine、无共享状态、无锁。
- 通过 `context.Context` 贯穿所有 IO，drain 流程派生 `context.WithTimeout` 控制总时长，并 `defer cancel()`。
- `evictPodWithRetry` 中 `time.NewTimer` 在 ctx 取消路径上显式 `timer.Stop()` 并 drain `timer.C`，避免 timer 泄漏。

## 7. 设计模式与亮点

- **策略分发**：`executeAction` 用 switch 分发到各 action 实现函数，每个函数自包含 marshal/HTTP/错误包装。
- **预检 + 前置条件**：所有写操作都先 GET 取 uid/RV，再用 `addResourceVersionPrecondition` 或 `DeleteOptions.preconditions` 保证乐观并发，避免覆盖更新的资源。
- **dry-run 透传**：通过 `withDryRun` 在 URL 上追加 `dryRun=All`，由 API server 做真正的校验，避免在客户端重复实现校验逻辑。
- **drain 决策与执行解耦**：`drainDecision` 是纯函数，便于测试；两遍遍历（abort 检测 + 实际处理）保证快速失败。
- **错误标准化**：`kubernetesAPIError` + `isKubernetesStatusError` 把 Kubernetes 状态码错误抽象成可判别的 Go error，配合 `errors.Is(err, errForbidden/errNotFound)` 形成统一的 sentinel 错误体系。

## 8. 注意事项

- **`drainNode` 中的 `drainCtx`**：当 `opts.timeoutSeconds > 0` 时才派生超时 ctx；若 `timeoutSeconds == 0` 则 drain 会一直等到所有 pod 处理完成或父 ctx 取消，大集群下可能长时间阻塞，需调用方合理设置超时。
- **`evictPodWithRetry` 的重试间隔是固定的**（`opts.retrySeconds`），并非指数退避；PDB 频繁触发 429 时可能产生较多请求。
- **`doRaw` 读取非 2xx 响应 body 上限 1KB**：超长错误信息会被截断，调用方在日志里看到的 `Body` 字段可能不完整。
- **`deletePod` / `evictPod` 的 preconditions**：仅当 uid/RV 非空时才注入；上层若未带 `ExpectedResourceVersion`，则不会做并发冲突保护。
- **`drainDecision` 对 `unmanaged pod requires force=true`**：默认 abort=false 仅 skip，但 `force=true` 时会强删无 owner 的 pod，存在误删风险，使用方需谨慎。
- **action 名称归一化**：`normalizeK8sAction` 接受多种别名（`restart`、`delete`、`evict`），文档与外部协议需明确 canonical 名称，避免歧义。
