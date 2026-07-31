# `describe_k8s_resource.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/describe_k8s_resource.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件实现 `describe_k8s_resource` 工具：通过集群 controller edge 实时读取单个 Kubernetes 资源的 live state（Pod / Node / Namespace / Service / Deployment / StatefulSet / DaemonSet / ReplicaSet / Job / CronJob / Event）。只读，不执行 kubectl/exec/logs/scale/restart/delete。当 DB snapshot 新鲜度不够、用户明确要求 describe/latest/live 或想看某命名对象最近的 Events 时使用。15s 调用超时，依赖 `K8sSnapshotReader.GetCluster` 找到集群的 controller edge，再通过 `Caller.Call` 派发 `tunnel.MethodDescribeK8sResource`。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 Registry 闭包路径 (`Registry.executeDescribeK8sResource`) 调用；依赖 `basetool`、`pkg/tunnel`（KubernetesDescribeResourceRequest/Response、Method 常量）、`Caller`、`K8sSnapshotReader`

## 3. 关键类型与接口

```go
type DescribeK8sResourceTool struct {
    caller Caller           // tunnel 派发客户端
    reader K8sSnapshotReader // 集群元数据读取器
    log    *slog.Logger
}

type DescribeK8sResourceArgs struct {
    ClusterID     uint64 `json:"cluster_id"`
    Kind          string `json:"kind"`
    APIVersion    string `json:"api_version,omitempty"`
    Namespace     string `json:"namespace,omitempty"`
    Name          string `json:"name"`
    IncludeEvents *bool  `json:"include_events,omitempty"` // 默认 true
    EventsLimit   int    `json:"events_limit,omitempty"`   // 默认 20，上限 100
}

type describeK8sResourceResponse struct {
    Source           string                                    `json:"source"`              // 固定 "kubernetes_api"
    ControllerEdgeID uint64                                    `json:"controller_edge_id"`
    Result           tunnel.KubernetesDescribeResourceResponse `json:"result"`
}
```

`Caller` 与 `K8sSnapshotReader` 为本包内消费方定义的窄接口，分别在 `registry_basetool.go` / `query_k8s_snapshot.go` 附近定义；本工具只用到 `reader.GetCluster(ctx, id)` 与 `caller.Call(ctx, edgeID, method, body)`。

## 4. 关键函数与流程

```go
func (t *DescribeK8sResourceTool) InvokableRun(ctx, argsJSON, _ ...InvokeOption) (string, error)
func normalizeDescribeK8sResourceArgs(in DescribeK8sResourceArgs) (tunnel.KubernetesDescribeResourceRequest, error)
func (r *Registry) executeDescribeK8sResource(ctx, args json.RawMessage) (ExecuteResult, error)
```

执行流程（`InvokableRun`）：
1. 守门检查 `caller` / `reader` 是否注入，缺一即直接 error。
2. `json.Unmarshal` → `DescribeK8sResourceArgs`；`normalizeDescribeK8sResourceArgs` 校验 `cluster_id` / `kind` / `name` 必填，`events_limit` 默认 20 并 clamp 到 [1,100]，`include_events` 缺省 true，trim 所有字符串字段。
3. `context.WithTimeout(ctx, describeK8sResourceCallTimeout=15s)` 形成调用上下文。
4. `reader.GetCluster` 获取集群，校验 `ControllerEdgeID` 非空非零（否则报 "cluster has no online controller edge"）。
5. 序列化 normalized 请求 → `caller.Call(callCtx, *cluster.ControllerEdgeID, tunnel.MethodDescribeK8sResource, body)`。
6. 反序列化 `tunnel.KubernetesDescribeResourceResponse`，包装为 `describeK8sResourceResponse{Source:"kubernetes_api", ControllerEdgeID, Result}` 后回写 JSON 字符串。

`Registry.executeDescribeK8sResource` 是闭包路径入口：构造 `DescribeK8sResourceTool` 实例并代理到 `InvokableRun`，返回 `ExecuteResult{ResultJSON: json.RawMessage(out)}`。

## 5. 依赖关系

- **basetool**：`ToolInfo`、`InvokeOption`（本工具忽略 opts）。
- **pkg/tunnel**：`KubernetesDescribeResourceRequest/Response`、`MethodDescribeK8sResource` 常量。
- **Caller / K8sSnapshotReader**：包内窄接口；生产实现由 Registry 注入。
- 不依赖任何 alertbiz / edgebiz / devicebiz。

## 6. 并发与资源管理

- 单次调用，单次 `Caller.Call`，无 goroutine。
- 唯一资源是 `context.WithTimeout` 派生的 `callCtx` + `defer cancel()`，保证即使 tunnel hang 也会在 15s 后释放。
- 无状态字段（`DescribeK8sResourceTool` 仅持有不变依赖引用），天然并发安全。

## 7. 设计模式与亮点

- **Class=read**：明确标注只读，WhenToUse 写明 "NOT for counts/lists/overview (use query_k8s_snapshot)"、"does not execute kubectl, exec, logs, scale, restart, or delete"，引导 LLM 正确路由。
- **Tool 既是 BaseTool 又能被 Registry 闭包路径复用**：`NewDescribeK8sResourceTool` 构造的实例既可注册进 BaseTool registry，也能被 `Registry.executeDescribeK8sResource` 直接代理调用，避免两套实现。
- **normalize 与 dispatch 分离**：`normalizeDescribeK8sResourceArgs` 纯函数化校验 + 默认值填充，便于单测；dispatch 仅做 IO。
- **明确错误前缀**：所有 error 信息以 `ToolNameDescribeK8sResource` 开头，便于日志检索与 audit。
- **IncludeEvents 指针语义**：用 `*bool` 区分 "未传"（默认 true）与 "显式 false"，避免零值歧义。

## 8. 注意事项

- **ControllerEdgeID 缺失即报错**：集群若没有 online controller edge（`nil` 或 0），直接 fail，不会回退到 DB snapshot。
- **不鉴权**：本工具只读，未做 admin/tenant 校验；tenant 隔离由 `tenant_bind` 装饰器在 args 注入 `tenant_id` 完成（本工具 args 中没有 tenant_id 字段，依赖 cluster_id 自身归属）。
- **EventsLimit clamp 在 normalize 内**：调用方传 0 / 负数都会被纠正为 20，超过 100 截到 100。
- **APIVersion 可选**：缺省由 controller 按 Kind 推导（v1 / apps/v1 / batch/v1）；显式传入会覆盖默认。
- **闭包入口 `Registry.executeDescribeK8sResource` 不传 `opts`**：BaseTool 路径下 `InvokeOption` 也被忽略，两路行为一致。
