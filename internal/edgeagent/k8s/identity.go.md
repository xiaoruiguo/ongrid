# `identity.go` 技术实现文档

> 源文件：`internal/edgeagent/k8s/identity.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/k8s`

## 1. 概述

本文件职责单一：发现并返回 Kubernetes 集群的不变标识（cluster UID）。它通过读取 `kube-system` 命名空间的 `metadata.uid` 作为物理集群的唯一标识，独立于 manager 侧的记录，用于在 edge 侧对集群身份做幂等校验。

## 2. 包信息

- **包名**：`k8s`
- **所属模块**：`internal/edgeagent/k8s`（edgeagent 域的 Kubernetes 适配层）
- **依赖方向**：对外暴露 `DiscoverClusterUID`，被 edge agent 启动流程调用；内部依赖本包 `apiClient`（`newInClusterAPIClient` + `clusterUID`）。

## 3. 关键类型与接口

无显著类型定义。仅使用一个常量：

```go
const clusterIdentityNamespace = "kube-system"
```

## 4. 关键函数与流程

### `DiscoverClusterUID`
- **签名**：`func DiscoverClusterUID(ctx context.Context) (string, error)`
- **职责**：包级公开入口，发现当前 in-cluster Kubernetes 的集群 UID。
- **流程**：
  1. 调用 `newInClusterAPIClient()` 构造基于 ServiceAccount token 的 in-cluster 客户端。
  2. 调用 `client.clusterUID(ctx)` 取 `kube-system` namespace 的 UID。
- **错误处理**：客户端构造失败直接返回；`clusterUID` 内部已包装错误。

### `(*apiClient).clusterUID`
- **签名**：`func (c *apiClient) clusterUID(ctx context.Context) (string, error)`
- **职责**：GET `/api/v1/namespaces/kube-system`，反序列化只取 `metadata`，返回 TrimSpace 后的 UID。
- **流程**：
  1. 定义匿名 struct 嵌入 `objectMeta`，仅解码 `metadata` 字段，避免拉取整个 namespace 对象的无关字段。
  2. `c.get(ctx, "/api/v1/namespaces/"+clusterIdentityNamespace, &namespace)` 调用底层 `get`（在 `inventory.go` 中定义）。
  3. 校验 UID 非空：空则返回 `kube-system namespace UID is empty`。
- **错误处理**：HTTP/解码错误用 `fmt.Errorf("get kubernetes cluster identity: %w", err)` 包装。

## 5. 依赖关系

- **内部包**：无外部包依赖；仅依赖本包内 `apiClient` 与 `objectMeta`（在 `inventory.go` 定义）。
- **外部库**：标准库 `context`、`fmt`、`strings`。
- **被调用方**：edge agent 启动/注册流程（用于把集群 UID 上报给 manager 做身份校验）。

## 6. 并发与资源管理

无并发控制。`apiClient` 自身是无状态可并发使用的（HTTP client 由底层 `http.Client` 保证线程安全）。

## 7. 设计模式与亮点

- **单一职责**：文件只做一件事——发现 cluster UID。
- **轻量反序列化**：用匿名 struct + `objectMeta` 嵌入，只解码 `metadata`，避免拉取整个 Namespace 对象（Namespace 对象可能携带大量 status 字段）。
- **不变性假设**：以 `kube-system` namespace UID 作为集群不变标识，是 Kubernetes 生态中常见做法（kube-system 是集群生命周期内最先创建且 UID 不变的 namespace）。

## 8. 注意事项

- **依赖 in-cluster 配置**：`newInClusterAPIClient` 依赖 ServiceAccount token 与 CA 证书（默认 `/var/run/secrets/kubernetes.io/serviceaccount`），在非 Pod 环境运行会失败。
- **UID 空校验**：理论上游牧 Kubernetes 发行版可能返回空 UID，此处显式报错而非返回空串，调用方需处理该错误。
- **无重试**：单次 HTTP 调用，失败即返回；若启动期 API server 短暂不可用，需要上层重试。
- **无 RBAC 提示**：若 ServiceAccount 无 `get namespaces kube-system` 权限，会返回 403 错误，调用方需自行排查 RBAC。
