# `readonly.go` 技术实现文档

> 源文件：`internal/edgeagent/k8s/readonly.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/k8s`

## 1. 概述

本文件实现 Kubernetes 只读与管控操作的 tunnel handler 注册，以及 `describe`、`pod logs` 两个只读操作的具体实现。它把 manager 下发的 `tunnel` RPC 请求转交给 `apiClient`，并对响应做敏感字段脱敏、参数校验、cluster_id 鉴权。同时定义 `k8sDescribeSpec`（资源 kind → API path 映射）与 `sanitizeK8sObject`（对象脱敏）等基础设施，供 `actions.go` 复用。

## 2. 包信息

- **包名**：`k8s`
- **所属模块**：`internal/edgeagent/k8s`（edgeagent 域的 Kubernetes 适配层）
- **依赖方向**：被 `InventoryPusher.RegisterHandlers` 暴露给 tunnel 层；调用 `internal/pkg/tunnel` 的请求/响应类型、`internal/pkg/k8sredact` 做脱敏、本包 `apiClient` 与 `actions.go` 的 `executeAction`。

## 3. 关键类型与接口

```go
// k8sDescribeSpec 描述一种 K8s 资源的 API 元信息，用于构造 path
type k8sDescribeSpec struct {
    kind       string
    apiVersion string
    resource   string
    group      string
    namespaced bool
}

// 常量：pod logs 默认参数与上限
const (
    defaultPodLogTailLines    = 100
    defaultPodLogLimitBytes   = 16 * 1024
    defaultPodLogSinceSeconds  = int64(3600)
    maxPodLogTailLines        = 500
    maxPodLogLimitBytes       = 64 * 1024
    maxPodLogSinceSeconds     = int64(24 * 3600)
)
```

## 4. 关键函数与流程

### `RegisterHandlers`
- **签名**：`func (p *InventoryPusher) RegisterHandlers()`
- **职责**：向 tunnel client 注册三个 K8s handler。
- **流程**：
  1. nil/空 client/空 api 检查后直接返回。
  2. 注册 `MethodDescribeK8sResource`：解码 `KubernetesDescribeResourceRequest` → 校验 cluster_id（不匹配则报错，未带则用本控制器 cluster_id 覆盖）→ `api.describeResource` → marshal 响应。
  3. 注册 `MethodQueryK8sLogs`：解码 `KubernetesPodLogsRequest` → cluster_id 校验 → `api.queryPodLogs` → marshal。
  4. 注册 `MethodExecuteK8sAction`：解码 `KubernetesActionRequest` → cluster_id 校验 → `api.executeAction` → marshal。
- **错误处理**：解码失败包装 `decode: %w`；cluster_id 不匹配返回明确错误。
- **注释**：明确说明 mutating 请求仅限 `execute_k8s_action`，且由 manager 侧 gating 后才 dispatch。

### `(*apiClient).describeResource`
- **签名**：`func (c *apiClient) describeResource(ctx, req) (*tunnel.KubernetesDescribeResourceResponse, error)`
- **职责**：获取单个 K8s 资源的完整对象（脱敏后）+ 可选关联事件。
- **流程**：
  1. `describeSpecFor(req.Kind, req.APIVersion)` 解析 spec。
  2. 校验 name 必填；namespaced 资源校验 namespace 必填。
  3. `spec.path(namespace, name)` 构造 API path。
  4. `getRaw` 拉取原始 JSON。
  5. `sanitizeK8sObject` 脱敏并提取 uid/rv。
  6. 构造响应（含 FetchedAt）。
  7. `IncludeEvents=true` 时调 `relatedEvents` 拉关联事件（limit 默认 20，上限 100），事件拉取失败若为 optional error（forbidden/notfound）则忽略，其他错误返回。
- **错误处理**：每步用 `describe_k8s_resource: ...: %w` 包装。

### `(*apiClient).queryPodLogs`
- **签名**：`func (c *apiClient) queryPodLogs(ctx, req) (*tunnel.KubernetesPodLogsResponse, error)`
- **职责**：获取 Pod 日志（脱敏后）。
- **流程**：
  1. 校验 namespace 与 pod 必填。
  2. 参数归一化与上限裁剪：TailLines（默认 100，上限 500）、LimitBytes（默认 16KB，上限 64KB）、SinceSeconds（默认 3600s，上限 24h）。
  3. 构造 `/api/v1/namespaces/{ns}/pods/{pod}/log` path，附加 query 参数（tailLines/limitBytes/sinceSeconds/container/previous/timestamps）。
  4. `getRaw` 拉取日志 body。
  5. 若 body 超过 LimitBytes 则截断。
  6. `k8sredact.Text(string(body))` 脱敏日志文本。
  7. 构造响应（含 Bytes、LineCount、Truncated）。
- **错误处理**：`query_k8s_logs: get pod logs %s/%s: %w`。

### `describeSpecFor`
- **签名**：`func describeSpecFor(kind, apiVersion string) (k8sDescribeSpec, error)`
- **职责**：kind 名称（支持单数/复数）→ spec 映射。
- **支持的 kind**：Pod、Node、Namespace、Service、Deployment、StatefulSet、DaemonSet、ReplicaSet、Job、CronJob、Event。
- **拒绝的 kind**：Secret、ConfigMap（明确返回 `kind %q is not allowed`，防止泄露敏感数据）。
- **错误处理**：不支持的 kind 返回 `unsupported kind %q`。

### `(*k8sDescribeSpec).path`
- **签名**：`func (s k8sDescribeSpec) path(namespace, name string) string`
- **职责**：根据 group/namespaced 构造 API path。
- **流程**：group 空走 `/api/v1/...`，非空走 `/apis/{group}/v1/...`；namespaced 时插入 `/namespaces/{ns}`。name 用 `url.PathEscape` 转义。

### `sanitizeK8sObject`
- **签名**：`func sanitizeK8sObject(raw []byte) (json.RawMessage, string, string, error)`
- **职责**：脱敏 K8s 对象 JSON。
- **流程**：
  1. unmarshal 到 `map[string]any`。
  2. 从 metadata 删除 `managedFields`（体积大且含敏感信息）与 `annotations`（可能含 token/密码）。
  3. `redactK8sValue(obj, "")` 递归脱敏。
  4. 提取 uid 与 resourceVersion。
  5. marshal 回 JSON。
- **返回**：脱敏后的 JSON、uid、rv、error。

### `redactK8sValue`
- **签名**：`func redactK8sValue(value any, key string) any`
- **职责**：递归脱敏。
- **规则**：
  - key 是敏感 key（`k8sredact.IsSensitiveKey`）→ 替换为 `[REDACTED]`。
  - map 中若有 `name` 字段是敏感 key 且存在 `value` 字段 → 把 `value` 替换为 `[REDACTED]`（处理 Secret data 结构）。
  - map 递归每个子 key。
  - array 递归每个元素。
  - string 走 `k8sredact.Text` 脱敏（可能含 secret/token 的文本）。
- **设计**：兼顾 key-based 与结构化脱敏两种策略。

### `(*apiClient).relatedEvents`
- **签名**：`func (c *apiClient) relatedEvents(ctx, kind, namespace, name string, limit int) ([]tunnel.KubernetesEventSnapshot, error)`
- **职责**：列出与指定对象关联的事件。
- **流程**：`listEvents(ctx, namespace)` 拉全部事件 → 过滤 `InvolvedKind==kind && InvolvedName==name`（namespace 非空时还校验 InvolvedNamespace）→ 截断到 limit。
- **错误处理**：list 失败直接返回；上层对 optional error 做降级处理。

### `stringField` / `firstNonEmpty` / `countLogLines` / `minInt`
- 工具函数：从 map 取字符串、取首个非空字符串、统计日志行数、取较小整数。

## 5. 依赖关系

- **内部包**：
  - `github.com/ongridio/ongrid/internal/pkg/tunnel`：所有 `Kubernetes*Request/Response` 类型、`Method*` 常量、`Session`。
  - `github.com/ongridio/ongrid/internal/pkg/k8sredact`：`IsSensitiveKey`、`Text` 脱敏。
  - 本包 `inventory.go`：`InventoryPusher`、`apiClient`、`objectMeta`、`listEvents`、`eventItem`、`podList`、`getRaw`。
  - 本包 `actions.go`：`executeAction`（被 `MethodExecuteK8sAction` handler 调用）。
- **外部库**：标准库 `context`、`encoding/json`、`errors`、`fmt`、`net/url`、`strconv`、`strings`、`time`。
- **被调用方**：edgeagent 启动入口（构造 `InventoryPusher` 后调 `RegisterHandlers`）。

## 6. 并发与资源管理

- **handler 并发**：tunnel 层会并发调用注册的 handler，每个 handler 在独立 goroutine 中执行。`apiClient` 的 HTTP client 并发安全；`describeResource`/`queryPodLogs`/`executeAction` 都是无状态函数，仅依赖参数与 ctx，天然并发安全。
- **无共享状态**：`RegisterHandlers` 闭包捕获 `p`（`InventoryPusher`），但 `p` 字段在构造后只读，无需加锁。
- **context 贯穿**：所有 IO 都带 ctx；`queryPodLogs` 的日志拉取可能因大日志体而耗时，依赖 ctx 超时控制。

## 7. 设计模式与亮点

- **handler 注册模式**：`RegisterHandlers` 集中注册三个 handler，每个 handler 用闭包捕获 `p`，避免全局变量。
- **cluster_id 鉴权**：所有 handler 都校验 `req.ClusterID`，未带则用控制器自身 cluster_id 覆盖，防止跨集群请求。
- **kind 白名单**：`describeSpecFor` 明确拒绝 Secret/ConfigMap，从源头防止敏感数据泄露。
- **双重脱敏**：`sanitizeK8sObject` 既删除已知敏感字段（managedFields/annotations），又递归 `redactK8sValue` 处理未知敏感字段，深度防御。
- **结构化脱敏**：`redactK8sValue` 对 Secret data 结构（`{name: sensitive, value: ...}`）特殊处理，把 value 替换为 `[REDACTED]` 而非删除整个字段，保留结构可读性。
- **日志参数上限**：`queryPodLogs` 对 TailLines/LimitBytes/SinceSeconds 都设默认值与上限，防止拉取超大日志导致 OOM 或网络压力。
- **optional error 降级**：`relatedEvents` 失败若是 forbidden/notfound 则忽略，保证 describe 主流程不被事件拉取失败阻断。
- **path 转义**：`spec.path` 用 `url.PathEscape` 转义 name 与 namespace，防止路径注入。

## 8. 注意事项

- **`relatedEvents` 拉全部事件再过滤**：`listEvents` 拉取整个 namespace 的事件列表后客户端过滤，大集群下事件量大时效率低。K8s API 支持 `fieldSelector=involvedObject.kind=...,involvedObject.name=...`，可服务端过滤减少传输。当前实现优先简单性。
- **`sanitizeK8sObject` 删除 annotations**：注释说明 annotations 可能含 token/密码，但也会删除合法 annotations（如 `kubectl.kubernetes.io/last-applied-configuration`），调用方需知悉。
- **`redactK8sValue` 的 string 脱敏**：所有字符串都走 `k8sredact.Text`，可能对非敏感文本做不必要的扫描，性能开销在大对象上明显。
- **`queryPodLogs` 的 body 截断**：`body[:req.LimitBytes]` 按字节截断，可能截断到多字节 UTF-8 字符中间，导致日志文本乱码。脱敏后再截断会更安全，但当前先截断后脱敏。
- **`describeSpecFor` 的 kind 白名单**：缺少 PersistentVolume/PersistentVolumeClaim/Ingress/NetworkPolicy 等常见资源，若需要 describe 这些资源需扩展。
- **`RegisterHandlers` 的 nil 检查**：若 `p.api==nil`（如构造时 API client 失败但未返回错误），handler 不会注册，调用方可能不知情。当前 `NewInventoryPusher` 在 API client 失败时直接返回错误，保证不会出现 nil api。
- **`executeAction` handler 的 mutating 性质**：注释明确说明 mutating 请求仅限 `execute_k8s_action`，由 manager 侧 gating。edge 侧不再做额外权限校验，依赖 manager 的 RBAC。
