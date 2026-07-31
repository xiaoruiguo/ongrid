# query_k8s_snapshot.go

## 1. 概述

本文件实现 `query_k8s_snapshot` 工具，查询 manager DB 里的 Kubernetes 快照（cluster / node / workload / pod / event inventory）。用于 counts、lists、status summaries、namespace fault triage、K8s object correlation——**不调 live Kubernetes API**。

**设计意图**：LLM 问 "当前 pod 数"、"异常 pod 有哪些"、"deployment ready 状态"、"namespace 故障分析"时调它。WhenToUse 明示：namespace fault triage 时一次性拿 workloads + pods + warning Events，再用 `describe_k8s_resource` 只对 ambiguous Pods 深挖，`query_k8s_logs` 只对真正 start/restart 的容器 sample 日志。

**NOT for**：live describe / logs / exec / restart / scale / delete——那些走 live K8s/controller 工具。

## 2. 包信息

- **包名**：`tools`
- **路径**：`internal/manager/biz/aiops/tools/query_k8s_snapshot.go`
- **导入**：
  - `basetool`
  - `k8sbiz`（`internal/manager/biz/k8s`）—— `ListClustersFilter` / `ListWorkloadsFilter` 等 + `EffectiveClusterStatus`
  - `k8smodel`（`internal/manager/model/k8s`）—— Cluster/Node/Workload/Pod/Event 类型
  - `k8sredact`（`internal/pkg/k8sredact`）—— Event message 脱敏
- **Class**：`read`

## 3. 关键类型与接口

### `K8sSnapshotReader`（接口）

```go
type K8sSnapshotReader interface {
    ListClusters(ctx, f) ([]*Cluster, error)
    GetCluster(ctx, id) (*Cluster, error)
    ListNodes(ctx, clusterID) ([]*Node, error)
    CountNodes(ctx, clusterID) (int64, error)
    ListWorkloads(ctx, f) ([]*Workload, error)
    CountWorkloads(ctx, f) (int64, error)
    ListPods(ctx, f) ([]*Pod, error)
    CountPods(ctx, f) (int64, error)
    ListEvents(ctx, f) ([]*Event, error)
    CountEvents(ctx, f) (int64, error)
}
```

每个 List 都有对应 Count，支持"精确总数 + bounded 样本"模式。

### `QueryK8sSnapshotArgs`

```go
type QueryK8sSnapshotArgs struct {
    Resource      string `json:"resource,omitempty"`       // summary/clusters/nodes/workloads/pods/events
    ClusterID     uint64 `json:"cluster_id,omitempty"`
    ClusterName   string `json:"cluster_name,omitempty"`   // 子串过滤
    ClusterStatus string `json:"cluster_status,omitempty"` // online/offline/degraded
    ClusterMode   string `json:"cluster_mode,omitempty"`
    Namespace     string `json:"namespace,omitempty"`
    Kind          string `json:"kind,omitempty"`            // workload kind
    NodeName      string `json:"node_name,omitempty"`
    Phase         string `json:"phase,omitempty"`           // pod phase
    EventType     string `json:"event_type,omitempty"`      // Normal/Warning
    Reason        string `json:"reason,omitempty"`          // CrashLoopBackOff/OOMKilled/...
    InvolvedKind  string `json:"involved_kind,omitempty"`
    InvolvedName  string `json:"involved_name,omitempty"`
    Limit         int    `json:"limit,omitempty"`           // 默认 50，cap 200
    Offset        int    `json:"offset,omitempty"`
}
```

### `k8sSnapshotResponse`

```go
type k8sSnapshotResponse struct {
    Resource string         `json:"resource"`
    Source   string         `json:"source"`     // "manager_db_snapshot"
    Filters  map[string]any `json:"filters,omitempty"`
    Total    int64          `json:"total"`      // 精确 DB 总数
    Limit    int            `json:"limit,omitempty"`
    Offset   int            `json:"offset,omitempty"`

    Clusters   []k8sClusterSnapshotRow   `json:"clusters,omitempty"`
    Nodes      []k8sNodeSnapshotRow      `json:"nodes,omitempty"`
    Workloads  []k8sWorkloadSnapshotRow  `json:"workloads,omitempty"`
    Pods       []k8sPodSnapshotRow       `json:"pods,omitempty"`
    Events     []k8sEventSnapshotRow     `json:"events,omitempty"`
    Totals     *k8sClusterCountSnapshot  `json:"totals,omitempty"`      // summary 用
    PerCluster []k8sClusterCountSnapshot `json:"per_cluster,omitempty"` // summary / 各 resource 通用
    Truncated  bool                      `json:"truncated,omitempty"`
}
```

每个 row 类型（`k8sClusterSnapshotRow` / `k8sNodeSnapshotRow` / `k8sWorkloadSnapshotRow` / `k8sPodSnapshotRow` / `k8sEventSnapshotRow`）都是 trimmed envelope，不含敏感字段。

## 4. 关键函数与流程

### `InvokableRun(ctx, argsJSON, _ ...)`

1. 校验 `reader` 非 nil。
2. Unmarshal args（空串跳过），调 `normalizeQueryK8sSnapshotArgs` 归一化。
3. `context.WithTimeout(ctx, 10s)`。
4. 调 `t.run(callCtx, in)`，Marshal 返回。

### `run(ctx, in)`

1. `selectClusters(ctx, in)` 选出目标 cluster 列表。
2. 构造 `k8sSnapshotResponse{Resource, Source: "manager_db_snapshot", Filters, Limit, Offset}`。
3. 按 `in.Resource` 分派：
   - `summary` → `querySummary`：每 cluster 算 node/workload/pod/event 计数 + 总计
   - `clusters` → 切片后填 `Clusters` 行
   - `nodes` → `queryNodes`：逐 cluster count + list
   - `workloads` → `queryWorkloads`：逐 cluster count + list（支持 namespace/kind 过滤）
   - `pods` → `queryPods`：逐 cluster count + list（支持 namespace/node/phase/reason 过滤）
   - `events` → `queryEvents`：逐 cluster count + list（支持 type/reason/involved_* 过滤）
   - 其他 → error

### `selectClusters(ctx, in)`

- `ClusterID > 0` → `GetCluster` + `filterClustersByEffectiveStatus` 过滤
- 否则：分页 `ListClusters`（pageSize=200）拉全量 + `filterClustersByEffectiveStatus`

`filterClustersByEffectiveStatus` 用 `k8sbiz.EffectiveClusterStatus(c, now)` 计算实时状态（基于 last_seen_at），匹配 `in.ClusterStatus`。

### `querySummary` / `countCluster`

`querySummary` 遍历所有 cluster，`countCluster` 对每个 cluster 调 `CountNodes/CountWorkloads/CountPods/CountEvents`（workloads/pods/events 支持 namespace 等过滤），累加到 `Totals`，按 offset/limit 切片填 `PerCluster`。

### `queryNodes/Workloads/Pods/Events` 共同模式

跨 cluster 分页：维护 `remainingOffset` / `remainingLimit`，逐 cluster count（累加 `Total` + `PerCluster`），若 `remainingLimit > 0` 且 `remainingOffset < total` 则 list + 切片。最后 `Truncated = len(rows) < Total`。

### `normalizeQueryK8sSnapshotArgs`

- `Resource` trim + lower，空 → "summary"
- 所有字符串字段 trim
- `Limit ≤ 0 → 50`，`> 200 → 200`
- `Offset < 0 → 0`

### `eventSnapshotRow`

```go
Message: k8sredact.Text(e.Message)
```

Event message 用 `k8sredact.Text` 脱敏（去除 secret token 等敏感信息）——这是唯一应用 redact 的字段。

### `executeQueryK8sSnapshot`（闭包入口）

```go
func (r *Registry) executeQueryK8sSnapshot(ctx, args) (ExecuteResult, error) {
    tool := NewQueryK8sSnapshotTool(r.k8sSnapshot, r.log)
    out, err := tool.InvokableRun(ctx, string(args))
    ...
}
```

闭包路径复用 BaseTool 实例（与 `query_k8s_logs` 同模式）。

## 5. 依赖关系

| 方向 | 依赖 | 说明 |
|------|------|------|
| 上游 | `basetool.BaseTool` / `ToolInfo` | 实现 BaseTool 接口 |
| 下游 | `K8sSnapshotReader`（接口，由 `*k8sbiz.Usecase` 实现） | 数据源 |
| 类型 | `k8sbiz.List*Filter` | 过滤参数 |
| 类型 | `k8sbiz.EffectiveClusterStatus` | cluster 实时状态计算 |
| 类型 | `k8sredact.Text` | Event message 脱敏 |

## 6. 并发与资源管理

- `context.WithTimeout(ctx, 10s)` per call。
- `Limit` cap 200 + `Offset` 分页——Total 是精确 DB count，rows 是 bounded 样本。
- `selectClusters` 分页拉全量 cluster（pageSize=200），大 fleet 场景可能拉多次 DB。
- `querySummary` 对每个 cluster 调 4 次 Count（nodes/workloads/pods/events），N cluster = 4N 次 DB 调用——大 fleet 注意性能。

## 7. 设计模式与亮点

- **"精确总数 + bounded 样本"模式**：`Total` 是 DB 精确 count，`rows` 是 `Limit` 截断的样本。LLM 既能知道总量（"有 1234 个 pod"），又能看具体样本（前 50 个），不会因为 limit 截断而误以为总量就 50。
- **跨 cluster 分页**：`remainingOffset` / `remainingLimit` 在 cluster 间传递，实现"跨 cluster 的全局 offset/limit"——LLM 翻页时不需要按 cluster 拆分。
- **`EffectiveClusterStatus` 实时计算**：cluster 状态不是 DB 字段，而是基于 `last_seen_at` 实时计算（online/offline/degraded），保证 snapshot 数据即使滞后状态判断仍准确。
- **Event message 脱敏**：`k8sredact.Text(e.Message)` 是唯一应用 redact 的字段——Event message 常含 secret token 等敏感信息，必须脱敏再回 LLM。
- **`PerCluster` 通用字段**：summary 和各 resource 都填 `PerCluster`，让 LLM 在任何 resource 查询里都能看到 per-cluster 分布，便于定位"哪个 cluster 有问题"。
- **`Filters` 透传**：响应里回显实际生效的 filter（非空字段），便于 LLM 调试"为什么没数据"。
- **闭包复用 BaseTool 实例**：`executeQueryK8sSnapshot` 直接 `NewQueryK8sSnapshotTool(...).InvokableRun(...)`，避免双份实现 drift。

## 8. 注意事项

- **10s 超时**：相对紧，大 fleet（多 cluster × 多 resource）的 summary 查询可能超时——`querySummary` 是 N×4 次 DB 调用，N=20 cluster 就 80 次。
- **`Limit` cap 200**：相对其他工具（500）偏小，因为 K8s 资源行字段多，200 行已经接近 LLM 上下文舒适区。
- **`selectClusters` 全量拉取**：没 cluster_id 时分页拉全量 cluster 再内存过滤 status——大 fleet（>200 cluster）会有多次 DB 调用。
- **`queryNodes` 不支持过滤**：`ListNodes(ctx, clusterID)` 只按 cluster 拉，没有 namespace/phase 等过滤（node 没这些概念），但 `CountNodes` 也只按 cluster——如果未来要按 node role 过滤需要扩接口。
- **Event `Message` 脱敏后可能丢信息**：`k8sredact.Text` 是 best-effort，可能误脱敏或漏脱敏；LLM 看到 `***` 时知道是脱敏结果，不要当成真实内容。
- **`Source = "manager_db_snapshot"`**：明确告诉 LLM 这是 DB 快照不是 live 数据，snapshot 有滞后（controller 周期性上报），最新状态可能不在 snapshot 里——这种场景走 `describe_k8s_resource`。
- **无 batch 协议**：一次查一个 resource 类型，多 resource 要 LLM 多次调用（namespace fault triage 典型 3 次：workloads + pods + events）。
