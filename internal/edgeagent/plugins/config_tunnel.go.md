# `config_tunnel.go` 技术实现文档

> 源文件：`internal/edgeagent/plugins/config_tunnel.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/plugins`

## 1. 概述

`TunnelConfigFetcher` 是 PR-C2 阶段的生产配置源：通过 tunnel RPC `MethodGetPluginConfigs` 从 manager 拉取每边缘的插件配置。与 `EnvConfigFetcher` 组合为"主 + fallback"——tunnel 不可达（冷启动 / 网络分区）时回退到 env 快照，保证已配置插件不被误杀。本文件还包含 Kubernetes 场景下的默认值注入逻辑（full-node agent / telemetry gateway 两种角色）。

## 2. 包信息

- **包名**：`plugins`
- **所属模块**：`internal/edgeagent/plugins`
- **依赖方向**：被 main 调用注入 Supervisor；调用 `internal/pkg/tunnel.Client` 与本包 `EnvConfigFetcher`

## 3. 关键类型与接口

```go
type TunnelConfigFetcher struct {
    client       tunnel.Client
    knownPlugins []string
    fallback     *EnvConfigFetcher

    // 凭证 / edge_id 一次构造时从 env 物化，避免每次 Fetch 重复读 env
    authUser string
    authPass string
    edgeID   uint64

    // Kubernetes 角色与上下文
    k8sRole          string
    k8sMode          string
    k8sClusterID     uint64
    k8sNodeName      string
    k8sNamespace     string
    k8sTLSInsecure   bool
    k8sGateway       bool
    managerPublicURL string
}
```

实现 `ConfigFetcher.Fetch`；附带 `MarshalJSON`/`AssertKnown` 辅助方法。

## 4. 关键函数与流程

### `NewTunnelConfigFetcher(client, knownPlugins) *TunnelConfigFetcher`
- 简单包装，转调 `NewTunnelConfigFetcherWithCredentials(client, knownPlugins, "", "")`。

### `NewTunnelConfigFetcherWithCredentials(client, knownPlugins, accessKey, secretKey) *TunnelConfigFetcher`
- **职责**：构造 fetcher，从 env 物化凭证与 K8s 上下文。
- **流程**：authUser 取 `ONGRID_EDGE_PLUGIN_DATAPLANE_USER` > `ONGRID_EDGE_ACCESS_KEY` > 入参 accessKey；authPass 类似。K8s 字段读 `ONGRID_K8S_ROLE/MODE/CLUSTER_ID/NODE_NAME/POD_NAMESPACE/ENROLL_TLS_INSECURE/TELEMETRY_GATEWAY_ENABLED` 与 `ONGRID_MANAGER_PUBLIC_URL`。

### `Fetch(ctx) (map[string]PluginConfig, error)`
- **职责**：调 tunnel RPC 拉配置，转成 PluginConfig；失败回退 env。
- **流程**：
  1. `client == nil` → 直接 fallback env + applyKubernetesDefaults 返回
  2. `client.Call(ctx, MethodGetPluginConfigs, struct{}{}, &resp)` → 失败则 fallback env + applyKubernetesDefaults 返回（不向 supervisor surface 错误，避免 "config fetch failed" 永不恢复）
  3. edgeID 取 env > resp.EdgeID（env 优先因为操作员可能在 dev 用单 edge 对多 manager）
  4. 遍历 `resp.Configs`，过滤 knownPlugins，构造 PluginConfig（AuthUser/AuthPass 用本 fetcher 物化的凭证，不从 wire 取——避免凭证上线）
  5. manager 没提到的已知插件置为 `Enabled=false`
  6. 对每条配置应用 `withKubernetesDefaults`
- **错误处理**：fallback 自身失败才返回错误；tunnel 错误被吞掉。

### `withKubernetesDefaults(name, cfg) PluginConfig`
- **职责**：根据 K8s 角色注入插件级默认配置。
- **流程**：
  - 若 `isKubernetesTelemetryGatewayAgent()`（k8sGateway && role=controller）：仅 traces 走 `withKubernetesGatewayTracesDefaults`，其余原样返回
  - 否则若 `isFullNodeKubernetesAgent()`（role=node && mode=full-node）：按 name 分发到 logs/traces/metrics/hostmetrics/procmetrics 的 K8s 默认注入函数
  - 否则原样返回

### K8s 默认注入函数族
- `withKubernetesLogsDefaults`：把 endpoint 回环地址替换为 `managerPublicURL + /loki/api/v1/push`；spec 注入 `mode=kubernetes`/`cluster_id`/`node_name`/`pod_log_path=/var/log/pods/*/*/*.log`/`enable_journald=false`
- `withKubernetesTracesDefaults`：endpoint 同上替换为 `/v1/traces`；extra_attrs 注入 cluster_id/node_name；可选 tls_insecure_skip_verify
- `withKubernetesGatewayTracesDefaults`：gateway 模式专属，注入 grpc/http_endpoint、`omit_device_id=true`、`enable_k8sattributes=true`、`enable_logs=true`、`enable_metrics=true`、`metrics_export_endpoint=127.0.0.1:9464`、extra_attrs 注入 `telemetry_gateway=kubernetes`/`gateway_namespace`
- `withKubernetesMetricsDefaults`：注入 `dedupe_filesystems_by_device=true`
- `withKubernetesHostMetricsDefaults`：注入 `--path.procfs=/proc`、`--path.sysfs=/sys`、`--path.rootfs=/`、`--collector.filesystem.mount-points-exclude=...`
- `withKubernetesProcMetricsDefaults`：注入 `procfs=/proc`

### 辅助函数
- `copySpec` / `copyStringMapSpec`：深拷贝 spec map，避免修改 manager 推送的原对象
- `isLoopbackEndpoint`：解析 URL，判断 host 是否为 localhost/::1/127.*
- `appendMissingCLIArgs`：合并 CLI 参数，跳过已存在的（按 `--key=` 前缀去重）
- `stringValues` / `hasCLIArg`：CLI 参数数组处理
- `MarshalJSON`：诊断 dump（known_plugins/edge_id/has_client），当前未使用
- `AssertKnown(names)`：测试 / debug 用，列出未知名

## 5. 依赖关系

- **内部包**：`internal/pkg/tunnel`（Client / MethodGetPluginConfigs / GetPluginConfigsResponse / 各种 Request/Response 类型）；本包 `EnvConfigFetcher`、`PluginConfig`、`firstNonEmpty`、`envBool`、`envUint`
- **外部库**：标准库 `context`/`encoding/json`/`fmt`/`net/url`/`os`/`strings`
- **被调用方**：main.go

## 6. 并发与资源管理

无锁。`TunnelConfigFetcher` 构造后字段只读；`Fetch` 每次创建新 map，调用方拿到独立快照可自由修改（`copySpec` 进一步保证 spec 深拷贝）。`client.Call` 自身线程安全由 tunnel 包保证。

## 7. 设计模式与亮点

- **主 + fallback 组合**：tunnel 失败静默回退 env，分区期间维持已配置插件运行，避免"config fetch failed; keeping previous state"导致永不恢复。
- **凭证不上线**：AuthUser/AuthPass 由 edge 从本地 env / 入参物化，wire 只携带非敏感的 Endpoint 与 Spec；manager 即使被攻破也拿不到数据面凭证。
- **edge ID 优先级**：env > tunnel response——dev 场景单 edge 对多 manager 时 env 的 ID 是 Loki 标签的真实来源。
- **K8s 角色感知**：通过 ONGRID_K8S_ROLE/MODE 区分 full-node agent 与 telemetry gateway，对每种插件注入不同默认（pod 日志路径、procfs 路径、gateway 端口等），减少 operator 配置负担。
- **回环地址重写**：`isLoopbackEndpoint` 检测 Endpoint 是 localhost/127.* 时替换为 managerPublicURL，避免 K8s 环境里 edge 配置指向容器内回环导致数据发不出去。

## 8. 注意事项

- K8s 默认值注入仅在 spec 缺失对应 key 时填入（`if _, ok := spec[k]; !ok`），operator 显式设置优先。
- `withKubernetesLogsDefaults` 中若 spec.mode 已设且非 "kubernetes"，直接返回不注入——尊重 operator 选 host 模式。
- `appendMissingCLIArgs` 按 `--key=` 前缀去重，对 `--collector.textfile.directory` 这类带 `=` 的参数有效，但对空格分隔的 `--flag value` 形式不适用（hostmetrics 用 `=` 形式故 OK）。
- `MarshalJSON` 标注"currently unused"——若长期不用可删，避免维护负担。
- K8s 字段从 env 一次性物化，运行时改 env 不会生效；这与"配置热更新靠 TriggerReload"的设计一致，但需在文档中明确。
- `Fetch` 在 tunnel 失败时不返回 tunnel 错误，运维侧只能从日志 "reconcile snapshot" 看到统计；可考虑在 health 暴露 last fetch error。
