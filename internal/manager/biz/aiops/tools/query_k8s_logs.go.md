# query_k8s_logs.go

## 1. 概述

本文件实现 `query_k8s_logs` 工具，通过 cluster controller edge 实时读取单个 Kubernetes Pod 的 bounded 日志切片。是 Loki / log gateway 没数据时的小型 troubleshooting fallback——只读一个 Pod 的 bounded 日志，不是大流量日志搜索（那是 `query_logql` 的活）。

**设计意图**：当 K8s fault triage 在 snapshot / events / describe 之后仍无法确认根因时，sample 一段 bounded live logs 作为物证。CrashLoopBackOff、restart_count>0、probe 失败、init-container 失败等场景才考虑用它。

**安全约束**：never for kubectl exec / Secret 读 / 任意文件 / 大流量日志导出。

## 2. 包信息

- **包名**：`tools`
- **路径**：`internal/manager/biz/aiops/tools/query_k8s_logs.go`
- **导入**：
  - `basetool`
  - `tunnel`（`internal/pkg/tunnel`）—— `MethodQueryK8sLogs` + `KubernetesPodLogsRequest/Response`
- **Class**：`read`

## 3. 关键类型与接口

### 常量与默认值

```go
const ToolNameQueryK8sLogs = "query_k8s_logs"

const (
    defaultPodLogTailLines    = 100
    defaultPodLogLimitBytes   = 16 * 1024       // 16KB
    defaultPodLogSinceSeconds = int64(3600)     // 1h
    maxPodLogTailLines        = 500
    maxPodLogLimitBytes       = 64 * 1024       // 64KB
    maxPodLogSinceSeconds     = int64(24 * 3600) // 24h
)

const queryK8sLogsCallTimeout = 20 * time.Second
```

### `QueryK8sLogsArgs`

```go
type QueryK8sLogsArgs struct {
    ClusterID    uint64 `json:"cluster_id"`
    Namespace    string `json:"namespace"`
    Pod          string `json:"pod"`
    Container    string `json:"container,omitempty"`  // multi-container Pod 时指定
    Previous     bool   `json:"previous,omitempty"`   // CrashLoopBackOff 读上次终止容器
    SinceSeconds int64  `json:"since_seconds,omitempty"`
    TailLines    int    `json:"tail_lines,omitempty"`
    LimitBytes   int    `json:"limit_bytes,omitempty"`
    Timestamps   *bool  `json:"timestamps,omitempty"` // 默认 true
}
```

### `queryK8sLogsResponse`

```go
type queryK8sLogsResponse struct {
    Source           string                           `json:"source"`
    Status           string                           `json:"status"`            // ok | unavailable
    ErrorKind        string                           `json:"error_kind,omitempty"` // rbac_forbidden | not_found
    Error            string                           `json:"error,omitempty"`
    Advice           string                           `json:"advice,omitempty"`  // LLM-facing 处置建议
    ControllerEdgeID uint64                           `json:"controller_edge_id"`
    Result           tunnel.KubernetesPodLogsResponse `json:"result"`
}
```

### `QueryK8sLogsTool`

```go
type QueryK8sLogsTool struct {
    caller Caller              // tunnel caller
    reader K8sSnapshotReader   // 用于查 cluster.ControllerEdgeID
    log    *slog.Logger
}
```

## 4. 关键函数与流程

### `InvokableRun(ctx, argsJSON, _ ...)`

1. 校验 `caller` 和 `reader` 非 nil。
2. Unmarshal args，调 `normalizeQueryK8sLogsArgs` 校验 + 填默认值。
3. `context.WithTimeout(ctx, 20s)`。
4. `reader.GetCluster(callCtx, req.ClusterID)`，拿 cluster。
5. 校验 `cluster.ControllerEdgeID != nil && *!= 0`，否则 "no online controller edge"。
6. Marshal req 为 body，调 `caller.Call(callCtx, *ControllerEdgeID, tunnel.MethodQueryK8sLogs, body)`。
7. **错误降级**：若 dispatch 失败，调 `k8sLogsUnavailableResponse` 尝试生成结构化 error response（识别 RBAC forbidden / not_found 两类）；不匹配则返回原始 error。
8. 成功则 Unmarshal `tunnel.KubernetesPodLogsResponse`，包成 `queryK8sLogsResponse{Source: "kubernetes_pods_log", Status: "ok", ControllerEdgeID, Result}`，Marshal 返回。

### `normalizeQueryK8sLogsArgs(in)`

- `ClusterID == 0` → error
- `Namespace` trim 后空 → error
- `Pod` trim 后空 → error
- `SinceSeconds` 用 `boundedInt64`（默认 3600，max 86400）
- `TailLines` 用 `boundedInt`（默认 100，max 500）
- `LimitBytes` 用 `boundedInt`（默认 16384，max 65536）
- `Timestamps` 默认 true，`in.Timestamps != nil` 时覆盖

### `k8sLogsUnavailableResponse(controllerEdgeID, req, err)`

错误分类器：

- `"kubernetes api forbidden"` 或 `"pods/log" + "forbidden"` → `errorKind = "rbac_forbidden"`，advice 提示 "grant get on pods/log or use Loki/log gateway"
- `"not found"` → `errorKind = "not_found"`，advice 提示 "refresh snapshot before retrying"
- 其他 → 返回 `("", false)`，由上层返回原始 error

返回的 response 里 `Status = "unavailable"`，`Result` 字段回填请求参数 + `FetchedAt = time.Now().Unix()`，保持 wire shape 一致。

### `boundedInt` / `boundedInt64`

通用 helper：`v <= 0 → fallback`，`v > max → max`，否则 `v`。

### `executeQueryK8sLogs`（闭包入口）

```go
func (r *Registry) executeQueryK8sLogs(ctx context.Context, args json.RawMessage) (ExecuteResult, error) {
    tool := NewQueryK8sLogsTool(r.caller, r.k8sSnapshot, r.log)
    out, err := tool.InvokableRun(ctx, string(args))
    ...
}
```

闭包路径直接复用 BaseTool 实例，避免双份实现——这是与其他工具（如 query_logql）不同的"单实现共享"模式。

## 5. 依赖关系

| 方向 | 依赖 | 说明 |
|------|------|------|
| 上游 | `basetool.BaseTool` / `ToolInfo` | 实现 BaseTool 接口 |
| 下游 | `Caller.Call` | tunnel 派发到 controller edge |
| 下游 | `K8sSnapshotReader.GetCluster` | 查 controller_edge_id |
| 类型 | `tunnel.KubernetesPodLogsRequest/Response` | wire 协议 |

## 6. 并发与资源管理

- `context.WithTimeout(ctx, 20s)` per call——比 snapshot 的 10s 长，因为是 live API 调用，要等 K8s API server 返回日志流。
- `LimitBytes` cap 64KB + `TailLines` cap 500 双重 bound，防止日志撑爆 LLM 上下文。
- 无共享状态，并发安全。

## 7. 设计模式与亮点

- **闭包路径直接复用 BaseTool 实例**：`executeQueryK8sLogs` 不重复实现，直接 `NewQueryK8sLogsTool(...).InvokableRun(...)`。这是新工具的推荐模式，避免 drift——与 `query_logql.go`（闭包/BaseTool 双实现）形成对比。
- **错误降级结构化**：`k8sLogsUnavailableResponse` 把 RBAC forbidden 和 not_found 两类常见错误转成 `Status=unavailable + ErrorKind + Advice` 结构化响应，让 LLM 能基于 `advice` 字段决定下一步（grant RBAC / refresh snapshot / 走 Loki fallback）。
- **`Previous` 字段**：读上次终止容器的日志，CrashLoopBackOff 场景关键——容器重启后当前日志可能是空的，上次崩溃前的日志才有物证。
- **`Timestamps *bool` 默认 true**：用指针区分"未设"和"false"，未设时强制 true 给日志加时间戳，便于 LLM 对齐事件时序。
- **`Result` 字段回填请求参数**：错误响应里也回填 `ClusterID/Namespace/Pod/Container/...`，让 LLM 不必回看 args 就能知道是哪个 Pod 失败。
- **`Source = "kubernetes_pods_log"`**：明确标注数据源是 live K8s pods/log API（不是 Loki），让 LLM 知道这是 controller edge 实时拉取的，不是历史索引。

## 8. 注意事项

- **20s 超时**：live K8s API 调用，pod 日志流可能慢；但 20s 仍是上限，超大 Pod 日志会截断到 `LimitBytes`/`TailLines`。
- **`SinceSeconds` 与 `TailLines` 互斥语义**：K8s API 会同时应用两个过滤，先按 since 过滤再按 tail 取最后 N 行——LLM 可能误以为二者是 OR 关系。
- **`Container` 可选**：multi-container Pod 不指定 container 时，K8s API 会报错或返回第一个容器日志（取决于版本）；建议 LLM 在 snapshot 里先确认 container 列表。
- **`Previous` 仅 CrashLoopBackOff 有意义**：常态 Pod 读 previous 会返回空或 error，LLM 不要盲目加 `previous=true`。
- **依赖 `K8sSnapshotReader`**：和 `query_k8s_snapshot` 共享 reader，意味着 snapshot 数据滞后时可能选错 controller edge（cluster 已经换 controller 但 snapshot 未更新）。
- **无 batch 协议**：一次只读一个 Pod，多 Pod 日志要 LLM 多次调用或改用 `query_logql`（Loki 那边支持 `{pod=~"..."}` 多 Pod 查询）。
