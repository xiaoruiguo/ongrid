# `metrics.go` 技术实现文档

> 源文件：`internal/edgeagent/k8s/metrics.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/k8s`

## 1. 概述

本文件实现 Kubernetes metrics 推送器 `MetricsPusher`：周期性从 kube-state-metrics、OTLP gateway、注解发现的应用 Pod 三个来源抓取 Prometheus 指标，分批经 tunnel 推送到 manager。它协调 scrape（抓取）、batch（分批）、push（推送）三个阶段，并集成 `metricsObserver` 做过程级 Prometheus 指标上报。

## 2. 包信息

- **包名**：`k8s`
- **所属模块**：`internal/edgeagent/k8s`（edgeagent 域的 Kubernetes 适配层）
- **依赖方向**：被 edgeagent 启动入口调用 `NewMetricsPusher` + `Run`；依赖 `internal/pkg/tunnel` 推送、`internal/edgeagent/plugins/metricscommon` 抓取与目标抽象、本包 `apiClient` 做应用发现、`metrics_batch.go` 做分批、`metrics_observer.go` 做指标观察。

## 3. 关键类型与接口

```go
// MetricsConfig 描述 metrics 抓取与推送的全部配置
type MetricsConfig struct {
    Endpoint         string        // kube-state-metrics URL
    GatewayEndpoint  string        // OTLP gateway metrics URL
    Interval         time.Duration
    Timeout          time.Duration
    PushTimeout      time.Duration
    SampleLimit      int
    BatchSampleLimit int
    BatchByteLimit   int
    DiscoverApps     bool          // 是否启用注解发现应用 metrics
}

// MetricsPusher 是 metrics 推送主结构
type MetricsPusher struct {
    client  tunnel.Client
    info    tunnel.KubernetesInfo
    edgeID  func() uint64
    cfg     MetricsConfig
    log     *slog.Logger
    api     *apiClient         // 仅 DiscoverApps=true 时构造
    metrics *metricsObserver
}

// 常量：默认参数与 source label
const (
    defaultK8sMetricsInterval         = 30 * time.Second
    defaultK8sMetricsTimeout          = 15 * time.Second
    defaultK8sMetricsPushTimeout      = 30 * time.Second
    defaultK8sMetricsLimit            = 250000
    defaultK8sMetricsBatchSampleLimit = 10000
    defaultK8sMetricsBatchByteLimit   = 4 << 20 // 4 MiB
    k8sMetricsSource                  = "k8s:kube-state-metrics"
    k8sAppMetricsSource               = "k8s:app-metrics"
    k8sGatewayMetricsSource           = "k8s:otlp-gateway-metrics"
)
```

## 4. 关键函数与流程

### `NewMetricsPusher`
- **签名**：`func NewMetricsPusher(client, info, edgeID, cfg, log, opts ...) (*MetricsPusher, error)`
- **职责**：构造推送器。
- **流程**：
  1. 校验 `client`、`ClusterID`。
  2. TrimSpace `Endpoint`/`GatewayEndpoint`；至少一个非空或 `DiscoverApps=true`。
  3. `metricscommon.ValidateURL` 校验 endpoint URL。
  4. `DiscoverApps=true` 时构造 `apiClient`。
  5. 填充默认值：interval/timeout（timeout 不超过 interval）/pushTimeout/sampleLimit/batchSampleLimit/batchByteLimit。
  6. 应用 `MetricsPusherOption`（目前仅 `WithMetricsRegisterer`）。
  7. `newMetricsObserver` 构造指标观察器。
- **错误处理**：任一必填缺失或 URL 非法返回明确错误。

### `Run`
- **签名**：`func (p *MetricsPusher) Run(ctx context.Context) error`
- **职责**：metrics 推送主循环。
- **流程**：
  1. 仅 controller role 运行。
  2. 启动期等待 `edgeID()!=0`，首次就绪后立即 `scrapeAndPush`。
  3. `time.NewTicker(interval)` 周期触发 `scrapeAndPush`。
  4. ctx.Done 退出。

### `scrapeAndPush`
- **签名**：`func (p *MetricsPusher) scrapeAndPush(ctx, edgeID)`
- **职责**：按配置依次抓取三个来源。
- **流程**：`Endpoint` 非空 → `scrapeTargetAndPush(kubeStateMetricsTarget, "k8s", k8sMetricsSource)`；`GatewayEndpoint` 非空 → `scrapeTargetAndPush(gatewayMetricsTarget, "k8s_otlp_gateway", k8sGatewayMetricsSource)`；`DiscoverApps` → `discoverAndPushAppMetrics`。

### `scrapeTargetAndPush`
- **签名**：`func (p *MetricsPusher) scrapeTargetAndPush(ctx, edgeID, target, plugin, source)`
- **职责**：单个 target 的 scrape → batch → push 主流程。
- **流程**：
  1. 构造 `metricsBatcher`，push 回调为 `p.pushWithTimeout`。
  2. 派生 `context.WithTimeout(ctx, cfg.Timeout)` 作为 scrape 超时。
  3. `metricscommon.ScrapeIncremental(rctx, target, func(samples){ batcher.Add(samples...) })` 增量抓取并喂给 batcher。
  4. 若 target 不上报 scrape 状态：仅补一个 `ScrapeUpSample` 后 Flush，记录 scrape/push 指标与日志后返回。
  5. 否则（`ReportScrapeStatus=true`）：先 Flush 数据批次，再构造 `scrapeStatusSamples`（含 partial 标记、accepted 计数、up 状态），单独推送状态批次。
  6. `metrics.observeScrape` 与 `metrics.observePush` 上报过程指标。
- **错误处理**：scrape 失败但有 accepted 样本时标记 partial；状态推送失败记 warn 日志。

### `scrapeStatusSamples`
- **签名**：`func scrapeStatusSamples(now, plugin, target, partial, accepted, up) []tunnel.PromSample`
- **职责**：构造状态样本（`ScrapeStatusSamples` + `ScrapeUpSample`），用于上报 scrape 健康度。

### `pushStatus`
- **签名**：`func (p *MetricsPusher) pushStatus(ctx, edgeID, source, samples, timeout) metricsBatchStats`
- **职责**：单独推送状态样本，返回 `metricsBatchStats`。

### `logScrapeOutcome`
- **签名**：`func (p *MetricsPusher) logScrapeOutcome(target, stats, pushStats, scrapeErr)`
- **职责**：按结果等级输出日志：scrape 失败 warn、limit 超限 warn、push 部分失败 warn、全部成功 debug。

### `discoverAndPushAppMetrics`
- **签名**：`func (p *MetricsPusher) discoverAndPushAppMetrics(ctx, edgeID)`
- **职责**：通过 K8s API 列出 pod，对带 `prometheus.io/scrape=true` 注解的 pod 调 `scrapeTargetAndPush`。
- **流程**：`api.listMetricPods(ctx, "")` → 遍历 pod → `appMetricsTarget(pod, cfg)` 转 target → `scrapeTargetAndPush(target, "k8s_app", k8sAppMetricsSource)`。
- **错误处理**：list 失败记 warn 并返回（不影响其他来源）。

### `pushWithTimeout`
- **签名**：`func (p *MetricsPusher) pushWithTimeout(ctx, edgeID, source, samples, timeout) error`
- **职责**：通过 `client.Call(MethodPushPromSamples)` 推送样本。
- **流程**：`context.WithTimeout` → 构造 `PushPromSamplesRequest` → 调用 → 校验 `resp.Accepted == len(samples)`。
- **错误处理**：RPC 失败包装 `push_prom_samples: %w`；accepted 数不符返回 `accepted %d of %d samples`。

### `kubeStateMetricsTarget` / `gatewayMetricsTarget`
- **职责**：构造 `metricscommon.Target`，含 `LabelDrop`（脱敏 uid/pod_uid/container_id 等高基数字段）。
- **设计**：`LabelDrop` 列表是安全策略，防止高基数字段进入 Prometheus。

### `(*apiClient).listMetricPods`
- **签名**：`func (c *apiClient) listMetricPods(ctx, namespace) ([]podItem, error)`
- **职责**：列出 pod（非分页，单次 GET）。与 `inventory.go` 的 `listPods` 不同（后者分页）。

### `appMetricsTarget`
- **签名**：`func appMetricsTarget(pod podItem, cfg MetricsConfig) (metricscommon.Target, bool)`
- **职责**：把带 scrape 注解的 pod 转成 target。
- **流程**：
  1. 检查 `prometheus.io/scrape` 注解为 true（`annotationBool` 兼容 true/1/yes/y）。
  2. 取 PodIP（非空）。
  3. port：注解 `prometheus.io/port` 优先，否则取首个 containerPort。
  4. scheme：注解优先，默认 http（仅允许 http/https）。
  5. path：注解优先，默认 /metrics，自动补 `/` 前缀。
  6. 构造 extraLabels：namespace/pod/node/workload_kind/workload_name。
  7. URL = `scheme://netJoinHostPort(podIP, port)+path`。
- **安全**：`LabelDrop` 包含 uid/pod_uid/container_id 等高基数字段。

### `annotationBool` / `firstContainerPort` / `firstMetricString`
- 工具函数：注解布尔解析、首个 containerPort、首个非空字符串。

## 5. 依赖关系

- **内部包**：
  - `github.com/ongridio/ongrid/internal/pkg/tunnel`：`Client`、`KubernetesInfo`、`PromSample`、`PushPromSamplesRequest/Response`、`MethodPushPromSamples`。
  - `github.com/ongridio/ongrid/internal/edgeagent/plugins/metricscommon`：`Target`、`ScrapeIncremental`、`ScrapeStats`、`ScrapeUpSample`、`ScrapeStatusSamples`、`ValidateURL`。
  - 本包 `inventory.go`：`apiClient`、`podItem`、`podList`、`controllerOwner`、`netJoinHostPort`。
  - 本包 `metrics_batch.go`：`metricsBatcher`、`metricsBatchStats`。
  - 本包 `metrics_observer.go`：`metricsObserver`、`newMetricsObserver`、`MetricsPusherOption`、`WithMetricsRegisterer`。
- **外部库**：标准库 `context`、`errors`、`fmt`、`log/slog`、`net/url`、`strings`、`time`。
- **被调用方**：edgeagent 启动入口。

## 6. 并发与资源管理

- **单 goroutine 主循环**：`Run` 是单 goroutine 顺序执行，scrape 与 push 在同一 goroutine 内串行。
- **无共享状态**：`MetricsPusher` 字段在构造后只读（除 `metrics` 观察器的内部状态，其并发安全由 Prometheus client 库保证）。
- **context 贯穿**：scrape 与 push 都派生超时 ctx；`scrapeTargetAndPush` 内 `defer cancel()`。
- **应用发现串行**：`discoverAndPushAppMetrics` 顺序抓取每个 pod，未并发——大集群下大量 scrape 注解 pod 可能成为瓶颈。

## 7. 设计模式与亮点

- **多源统一抽象**：kube-state-metrics、gateway、app 三种来源都抽象为 `metricscommon.Target`，统一走 `scrapeTargetAndPush` 流程。
- **数据批次与状态批次分离**：`ReportScrapeStatus=true` 的 target 把数据样本与状态样本（up/partial/accepted）分两批推送，便于上游分别处理。
- **partial 语义**：scrape 错误但有 accepted 样本、limit 超限、push 部分失败都标记 partial，让上游能区分完整 vs 部分数据。
- **LabelDrop 安全策略**：所有 target 都配置 `LabelDrop` 移除高基数/敏感 label（uid、container_id、instance、url 等），符合可观测性规范。
- **Option 模式**：`MetricsPusherOption` + `WithMetricsRegisterer` 提供可选的 Prometheus 注册器注入，不破坏默认构造路径。
- **过程指标**：通过 `metricsObserver` 上报 scrape samples、push batches、last success、scrape duration 等过程指标，便于自观测。
- **注解发现兼容**：`appMetricsTarget` 兼容 `prometheus.io/scrape`、`prometheus.io/port`、`prometheus.io/scheme`、`prometheus.io/path` 全套注解，与 Prometheus Operator 语义对齐。

## 8. 注意事项

- **应用发现串行抓取**：`discoverAndPushAppMetrics` 顺序处理每个 pod，大集群下可能超过 `cfg.Interval` 周期；可考虑并发抓取优化。
- **`listMetricPods` 非分页**：与 `inventory.go` 的 `listPods`（分页 500）不同，单次 GET；大集群下可能因 K8s API 默认 limit 截断而漏 pod。建议复用 `listAllK8sItems`。
- **`scrapeTargetAndPush` 的 batcher 失败处理**：batcher 初始化失败直接 return，不记指标；实际场景下极少发生（仅参数非法）。
- **状态推送失败不阻塞**：`pushStatus` 失败仅记 warn，不影响主流程；上游可能因此缺失 up 指标，需监控告警。
- **`pushWithTimeout` 的 accepted 校验**：严格要求 `resp.Accepted == len(samples)`，上游若部分拒绝会视为失败；若上游有合理丢弃策略可能产生误报。
- **`appMetricsTarget` 的 PodIP 依赖**：Pod 未就绪时 PodIP 为空会被跳过，符合预期但首次部署时可能无 target。
- **`LabelDrop` 重复定义**：`kubeStateMetricsTarget` 与 `gatewayMetricsTarget` 与 `appMetricsTarget` 各自定义 LabelDrop 列表，存在重复；可抽公共变量，但当前实现便于各 target 独立调整。
