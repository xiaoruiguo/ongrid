# `upgrade_prepare.go` 技术实现文档

> 源文件：`internal/edgeagent/k8s/upgrade_prepare.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/k8s`

## 1. 概述

本文件实现 Helm pre-upgrade hook 的核心逻辑：在 Helm 应用新 release manifest 前对集群中的 data-plane Deployment 做安全切换准备。它处理 controller 与 metrics scraper 两个 Deployment 的有序变更（停旧 scraper、给 controller 加 telemetry 标签、删除/修改环境变量），并轮询等待 rollout 完成，保证 Helm release 在 Kubernetes 任意 reconcile 顺序下都安全。

## 2. 包信息

- **包名**：`k8s`
- **所属模块**：`internal/edgeagent/k8s`（edgeagent 域的 Kubernetes 适配层）
- **依赖方向**：被 Helm pre-upgrade hook 入口（`cmd` 侧）调用 `PrepareUpgrade`；调用本包 `apiClient`（`newInClusterAPIClient`、`get`、`doRaw`）。

## 3. 关键类型与接口

```go
// UpgradePreparationConfig 描述一次升级准备的配置
type UpgradePreparationConfig struct {
    Namespace                string
    ControllerDeployment     string
    MetricsScraperDeployment string
    TargetGatewayMode        string        // "embedded" 或 "deployment"
    TargetMetricsMode        string        // "controller" 或 "scraper"
    PollInterval             time.Duration
}

// upgradeDeployment 是 Deployment 的轻量解码模型，只取升级所需的字段
type upgradeDeployment struct {
    Metadata struct {
        Generation int64 `json:"generation"`
    } `json:"metadata"`
    Spec struct {
        Replicas *int `json:"replicas"`
        Template struct {
            Metadata struct {
                Labels map[string]string `json:"labels"`
            } `json:"metadata"`
            Spec struct {
                Containers []upgradeContainer `json:"containers"`
            } `json:"spec"`
        } `json:"template"`
    } `json:"spec"`
    Status struct {
        ObservedGeneration  int64 `json:"observedGeneration"`
        UpdatedReplicas     int   `json:"updatedReplicas"`
        ReadyReplicas       int   `json:"readyReplicas"`
        AvailableReplicas   int   `json:"availableReplicas"`
        UnavailableReplicas int   `json:"unavailableReplicas"`
    } `json:"status"`
}

// upgradeContainer 是容器轻量模型，只取 name/ports/env
type upgradeContainer struct {
    Name  string `json:"name"`
    Ports []struct {
        Name string `json:"name"`
    } `json:"ports"`
    Env []struct {
        Name      string          `json:"name"`
        Value     string          `json:"value"`
        ValueFrom json.RawMessage `json:"valueFrom"`
    } `json:"env"`
}

// 常量
const (
    telemetryBackendLabel                = "ongrid.io/telemetry-backend"
    kubernetesStrategicMergePatchContent = "application/strategic-merge-patch+json"
    defaultUpgradePollInterval           = time.Second
)
```

## 4. 关键函数与流程

### `PrepareUpgrade`
- **签名**：`func PrepareUpgrade(ctx context.Context, cfg UpgradePreparationConfig) error`
- **职责**：公开入口，构造 in-cluster client 后委托给 `prepareUpgradeWithClient`。
- **流程**：`newInClusterAPIClient()` → `prepareUpgradeWithClient(ctx, client, cfg)`。
- **错误处理**：client 构造失败包装 `prepare kubernetes upgrade client: %w`。

### `prepareUpgradeWithClient`
- **签名**：`func prepareUpgradeWithClient(ctx, client, cfg) error`
- **职责**：升级准备主流程。
- **流程**：
  1. 校验并归一化 cfg：
     - Namespace：trim，空则用 `client.namespace`，仍空则报错。
     - ControllerDeployment / MetricsScraperDeployment：必填。
     - TargetGatewayMode：仅 `embedded`/`deployment`。
     - TargetMetricsMode：仅 `controller`/`scraper`。
     - PollInterval：默认 1s。
  2. **TargetMetricsMode==controller**：调 `stopDeploymentIfPresent` 停掉旧 metrics scraper（避免与 controller 内嵌 metrics 冲突）。
  3. 获取 controller Deployment（`getUpgradeDeployment`）。
  4. 找到 `edge-controller` 容器（`controllerContainer`），找不到则报错。
  5. 计算需要的 patch：
     - 若容器有 `otlp-grpc` port 且 labels 中 `ongrid.io/telemetry-backend != "true"` → 加该 label 为 `"true"`（为 Service selector 切换做准备）。
     - 若 TargetMetricsMode==scraper：
       - 容器有 `ONGRID_K8S_METRICS_ENDPOINT` env → 加 `$patch: delete` 移除（让新 scraper 接管 metrics）。
       - 容器有 `ONGRID_K8S_APP_METRICS_DISCOVERY` 且值非 `false` → 改为 `false`（关闭 controller 的 app 发现）。
  6. 若无变更（`changed=false`）直接返回（幂等）。
  7. 构造 strategic-merge-patch 并 `patchUpgradeDeployment`。
  8. `waitUpgradeDeploymentReady` 等待 rollout 完成。
- **错误处理**：每步用 `prepare kubernetes upgrade: ...: %w` 或 `get controller deployment: %w` 包装。

### `stopDeploymentIfPresent`
- **签名**：`func stopDeploymentIfPresent(ctx, client, namespace, name, pollInterval) error`
- **职责**：若指定 Deployment 存在且 replicas>0，则 scale 到 0 并等待 rollout。
- **流程**：
  1. `getUpgradeDeployment`：`errNotFound` 视为不存在直接返回 nil。
  2. replicas==0 视为已停止返回 nil。
  3. `patchUpgradeDeployment` patch `spec.replicas=0`。
  4. `waitUpgradeDeploymentReady` 等待 rollout。
- **幂等性**：不存在或已停止都安全返回。

### `getUpgradeDeployment`
- **签名**：`func getUpgradeDeployment(ctx, client, namespace, name) (*upgradeDeployment, error)`
- **职责**：GET Deployment，解码为 `upgradeDeployment`。
- **流程**：`client.get(ctx, deploymentAPIPath(namespace, name), &deployment)`。

### `patchUpgradeDeployment`
- **签名**：`func patchUpgradeDeployment(ctx, client, namespace, name, patch) (*upgradeDeployment, error)`
- **职责**：strategic-merge-patch 更新 Deployment 并返回更新后的对象。
- **流程**：
  1. `json.Marshal(patch)`。
  2. `client.doRaw(ctx, http.MethodPatch, deploymentAPIPath, kubernetesStrategicMergePatchContent, body)`。
  3. unmarshal 响应为 `upgradeDeployment` 返回。
- **错误处理**：marshal 失败 `marshal deployment patch: %w`；解码失败 `decode patched deployment: %w`。

### `waitUpgradeDeploymentReady`
- **签名**：`func waitUpgradeDeploymentReady(ctx, client, namespace, name, generation, pollInterval) error`
- **职责**：轮询直到 Deployment 达到指定 generation 的 ready 状态。
- **流程**：
  1. `time.NewTicker(pollInterval)` 定期轮询。
  2. 循环：`getUpgradeDeployment` → `deploymentReady(deployment, generation)` 为 true 则返回。
  3. select：ctx.Done → `deployment %s/%s rollout: %w`（包装 ctx.Err）；ticker.C → 继续轮询。
- **设计**：基于 generation 等待，确保等到本次 patch 触发的 rollout 完成，而非历史状态。

### `deploymentReady`
- **签名**：`func deploymentReady(deployment, generation) bool`
- **职责**：判断 Deployment 是否已 reconcile 到指定 generation 且全部就绪。
- **判定条件**：
  - `ObservedGeneration >= generation`（controller 已观察到本次变更）。
  - `UpdatedReplicas == desired`（所有副本已更新到新版本）。
  - `ReadyReplicas == desired`（所有副本就绪）。
  - `AvailableReplicas == desired`（所有副本可用）。
  - `UnavailableReplicas == 0`（无不可用副本）。

### `deploymentReplicas`
- **签名**：`func deploymentReplicas(deployment) int`
- **职责**：返回期望副本数，`Spec.Replicas==nil` 时默认 1（K8s 默认值）。

### `deploymentAPIPath`
- **签名**：`func deploymentAPIPath(namespace, name string) string`
- **职责**：构造 `/apis/apps/v1/namespaces/{ns}/deployments/{name}`，用 `url.PathEscape` 转义。

### `controllerContainer`
- **签名**：`func controllerContainer(deployment) (upgradeContainer, bool)`
- **职责**：从 Deployment spec 中找到名为 `edge-controller` 的容器。

### `containerHasPort` / `containerHasEnv` / `containerEnvValue`
- **职责**：检查容器是否有指定 port/env，或取指定 env 的值。
- **设计**：纯函数，便于测试。

## 5. 依赖关系

- **内部包**：
  - 本包 `inventory.go`：`apiClient`、`newInClusterAPIClient`、`get`、`doRaw`、`errNotFound`（sentinel error）。
- **外部库**：标准库 `context`、`encoding/json`、`errors`、`fmt`、`net/http`、`net/url`、`strings`、`time`。
- **被调用方**：Helm pre-upgrade hook 的 `cmd` 入口（`PrepareUpgrade` 是包级公开函数）。

## 6. 并发与资源管理

- **无并发控制**：`prepareUpgradeWithClient` 是顺序执行的单 goroutine 流程，无共享状态。
- **ticker 资源管理**：`waitUpgradeDeploymentReady` 用 `time.NewTicker` + `defer ticker.Stop()` 保证资源释放。
- **context 贯穿**：所有 K8s API 调用都带 ctx；`waitUpgradeDeploymentReady` 的 select 在 ctx.Done 时返回，避免无限等待。
- **无锁**：所有操作都是只读（GET）或基于 generation 的乐观并发（PATCH + 等 generation reconcile），依赖 K8s controller 的最终一致性。

## 7. 设计模式与亮点

- **strategic-merge-patch**：使用 `application/strategic-merge-patch+json`（K8s 原生 patch 类型），可以按容器名定位 env 修改（`$patch: delete` 删除指定 env），比 merge-patch 更精准。
- **generation 等待**：`waitUpgradeDeploymentReady` 基于 generation 而非简单轮询 status，确保等到的是本次 patch 触发的 rollout，而非历史状态，避免误判。
- **幂等设计**：`stopDeploymentIfPresent` 对不存在/已停止都安全返回；`prepareUpgradeWithClient` 在 `changed=false` 时直接返回，保证 hook 可重复执行（Helm hook 可能因失败重试）。
- **轻量模型**：`upgradeDeployment` 与 `upgradeContainer` 只解码升级所需字段（generation/replicas/labels/containers/ports/env/status），避免拉取整个 Deployment 对象（可能很大）。
- **容器名硬编码**：`controllerContainer` 查找 `edge-controller` 容器，与 Helm chart 的容器命名约定对齐。
- **telemetry-backend 标签切换**：通过先给 controller 加 `ongrid.io/telemetry-backend=true` 标签，让后续 Service selector 切换时不会断开旧 endpoint，实现零中断切换。
- **env patch 的 `$patch: delete`**：用 strategic-merge-patch 的删除语法移除指定 env，避免重写整个 env 列表，减少 patch 体积与出错风险。

## 8. 注意事项

- **`waitUpgradeDeploymentReady` 无超时上限**：依赖父 ctx 控制总时长，若 ctx 无超时且 Deployment 长期不 ready（如镜像拉取失败），会无限轮询。Helm hook 通常有自己的超时，需确保 ctx 传入超时。
- **`defaultUpgradePollInterval=1s`**：默认 1s 轮询，对 K8s API 有一定压力；大集群或 API server 负载高时可调大。
- **`controllerContainer` 硬编码容器名 `edge-controller`**：若 Helm chart 修改容器名，需同步更新此函数，否则升级准备会失败。
- **`deploymentReady` 的判定标准严格**：要求 UpdatedReplicas/ReadyReplicas/AvailableReplicas 全部等于 desired 且 UnavailableReplicas==0。若 Deployment 有 maxUnavailable>0 的滚动更新策略，中间状态会满足 ReadyReplicas<desired 但 UnavailableReplicas 在容忍范围内——此时 `deploymentReady` 返回 false，会等待到完全就绪。这是保守设计，确保升级完成。
- **`stopDeploymentIfPresent` 仅在 TargetMetricsMode==controller 时调用**：TargetMetricsMode==scraper 时不停旧 scraper（因为新 release 会用新 scraper 替换），依赖 Helm 的 Deployment 更新语义。
- **`telemetryBackendLabel` 切换的条件**：仅当容器有 `otlp-grpc` port 时才加标签。若旧 chart 未暴露该 port，不会加标签，可能影响 Service selector 切换——但这种情况意味着不需要切换，逻辑自洽。
- **`patchUpgradeDeployment` 不做 RV 前置条件**：与 `actions.go` 不同，这里 PATCH 不带 resourceVersion precondition，依赖 Helm hook 的串行执行保证不会并发修改。若 hook 被并发触发（不应发生），可能产生冲突。
- **`ValueFrom json.RawMessage`**：`upgradeContainer.Env` 的 `ValueFrom` 用 `json.RawMessage` 保留原始 JSON，便于在 patch 时透传或修改，避免丢失 `secretKeyRef`/`configMapKeyRef` 等引用结构。
