# `metrics_observer.go` 技术实现文档

> 源文件：`internal/edgeagent/k8s/metrics_observer.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/k8s`

## 1. 概述

本文件实现 metrics 推送过程的 Prometheus 指标观察器 `metricsObserver`：把 scrape 与 push 阶段的统计（样本数、批次数、耗时、最近成功时间）暴露为进程级 Prometheus 指标，便于自观测与告警。同时定义 `MetricsPusherOption` 选项模式，允许调用方注入自定义的 `prometheus.Registerer`。

## 2. 包信息

- **包名**：`k8s`
- **所属模块**：`internal/edgeagent/k8s`（edgeagent 域的 Kubernetes 适配层）
- **依赖方向**：被 `metrics.go` 的 `NewMetricsPusher` 与 `remote_write_scraper.go` 的 `NewRemoteWriteScraper` 调用；依赖 `github.com/prometheus/client_golang` 注册指标；消费 `metrics_batch.go` 的 `metricsBatchStats`。

## 3. 关键类型与接口

```go
// metricsObserver 持有所有 Prometheus collector
type metricsObserver struct {
    scrapeSamples       *prometheus.CounterVec
    scrapeLimitExceeded *prometheus.CounterVec
    pushBatches         *prometheus.CounterVec
    pushSamples         *prometheus.CounterVec
    lastSuccess         *prometheus.GaugeVec
    scrapeDuration      *prometheus.HistogramVec
}

// metricsPusherOptions 是 MetricsPusher 的可选配置容器
type metricsPusherOptions struct {
    registerer prometheus.Registerer
}

// MetricsPusherOption 是选项函数类型
type MetricsPusherOption func(*metricsPusherOptions)

const (
    metricsResultSuccess  = "success"
    metricsResultFailure  = "failure"
    metricsResultRejected = "rejected"
)
```

## 4. 关键函数与流程

### `WithMetricsRegisterer`
- **签名**：`func WithMetricsRegisterer(reg prometheus.Registerer) MetricsPusherOption`
- **职责**：返回一个 option 函数，把 `reg` 注入 `metricsPusherOptions.registerer`。
- **注释**：导出函数，注释说明用于在 edge 进程的 registry 上注册 K8s scrape/push 计数器，label 仅用有界的 source/result。

### `newMetricsObserver`
- **签名**：`func newMetricsObserver(reg prometheus.Registerer) (*metricsObserver, error)`
- **职责**：构造观察器并注册所有 collector。
- **流程**：
  1. `reg==nil` 返回空 observer（所有字段 nil），后续 observe 调用都会因 nil 检查而 no-op。
  2. 构造 6 个 collector：
     - `ongrid_edge_k8s_metrics_scrape_samples_total`（CounterVec, label: source）
     - `ongrid_edge_k8s_metrics_scrape_limit_exceeded_total`（CounterVec, label: source）
     - `ongrid_edge_k8s_metrics_push_batches_total`（CounterVec, label: source, result）
     - `ongrid_edge_k8s_metrics_push_samples_total`（CounterVec, label: source, result）
     - `ongrid_edge_k8s_metrics_last_success_timestamp_seconds`（GaugeVec, label: source）
     - `ongrid_edge_k8s_metrics_scrape_duration_seconds`（HistogramVec, label: source；buckets: 0.1–30s）
  3. 遍历 collector 调 `reg.Register`，任一失败返回 `register k8s metrics observer: %w` 错误。
- **错误处理**：注册失败（如重复注册）立即返回错误。

### `observeCycle`
- **签名**：`func (o *metricsObserver) observeCycle(source string, startedAt, completedAt time.Time, success bool)`
- **职责**：记录一次 scrape+push 周期的耗时与最近成功时间。
- **流程**：
  1. nil 检查（observer 或 collector 为 nil 则 no-op）。
  2. 计算 duration（负数兜底为 0），`scrapeDuration.WithLabelValues(source).Observe(duration.Seconds())`。
  3. success 时 `lastSuccess.WithLabelValues(source).Set(completedAt.Unix())`。
- **注意**：仅 `remote_write_scraper.go` 调用此方法（`metrics.go` 未调用，因后者不跟踪端到端 cycle）。

### `observeScrape`
- **签名**：`func (o *metricsObserver) observeScrape(source string, accepted int, limitExceeded bool)`
- **职责**：记录 scrape 阶段统计。
- **流程**：`scrapeSamples` Add(accepted)；`limitExceeded=true` 时 `scrapeLimitExceeded.Inc()`。

### `observePush`
- **签名**：`func (o *metricsObserver) observePush(source string, stats metricsBatchStats)`
- **职责**：记录 push 阶段统计。
- **流程**：
  - `pushBatches` 按 success/failure result 分别 Add。
  - `pushSamples` 按 success/failure/rejected result 分别 Add。
  - rejected 用独立 result label，区分"推送失败"与"样本被拒绝"。

## 5. 依赖关系

- **内部包**：
  - 本包 `metrics_batch.go`：`metricsBatchStats`。
- **外部库**：
  - `github.com/prometheus/client_golang/prometheus`：`Registerer`、`CounterVec`、`GaugeVec`、`HistogramVec`、`CounterOpts`、`GaugeOpts`、`HistogramOpts`、`Collector`。
- **被调用方**：
  - `metrics.go` 的 `NewMetricsPusher`（通过 `newMetricsObserver`）。
  - `remote_write_scraper.go` 的 `newRemoteWriteScraper`（通过 `newMetricsObserver`）。
  - 两者都通过 `WithMetricsRegisterer` option 注入 registerer。

## 6. 并发与资源管理

- **Prometheus collector 并发安全**：`CounterVec`/`GaugeVec`/`HistogramVec` 的 `WithLabelValues` 返回的 metric 实现是并发安全的，由 Prometheus client 库保证。
- **nil observer 模式**：`reg==nil` 时返回空 observer，所有 observe 方法都做 nil 检查后 no-op，避免 nil panic。
- **无额外锁**：observer 本身无状态修改，所有状态都在 Prometheus collector 内部。

## 7. 设计模式与亮点

- **Option 模式**：`MetricsPusherOption` + `WithMetricsRegisterer` 提供可选注入，不破坏默认构造路径（reg=nil 时 no-op）。
- **nil-safe observer**：空 observer 模式让无 registry 的场景（如测试）无需特殊处理，所有 observe 调用自动 no-op。
- **有界 label 设计**：所有 label（source/result）都是低基数有界集合，符合可观测性规范（高基数字段如 user_id/url 禁止做 label）。source 仅几个固定值（k8s:kube-state-metrics 等），result 仅 success/failure/rejected。
- ** Histogram buckets 选择**：`scrapeDuration` 的 buckets `[0.1, 0.25, 0.5, 1, 2.5, 5, 10, 15, 30]` 覆盖 100ms 到 30s，适配 scrape 周期（默认 30s interval）。
- **result label 区分失败类型**：pushSamples 用三个 result 值区分成功/失败/拒绝，便于监控细分。
- **懒注册**：collector 在 `newMetricsObserver` 时一次性注册，构造失败立即报错，避免运行时才发现注册冲突。

## 8. 注意事项

- **`metrics.go` 不调 `observeCycle`**：`MetricsPusher.scrapeTargetAndPush` 仅调 `observeScrape` 与 `observePush`，不调 `observeCycle`，因此 `scrape_duration_seconds` 与 `last_success_timestamp` 仅在 `RemoteWriteScraper` 路径上报。若需 `MetricsPusher` 也暴露这两个指标，需补充调用。
- **注册失败不降级**：`newMetricsObserver` 中任一 collector 注册失败直接返回错误，导致整个 pusher 构造失败。若希望降级（部分指标缺失仍可运行），需改为收集错误并继续。当前设计优先可观测性完整性。
- **重复注册风险**：若同一 registerer 被多次传入（如多个 pusher 实例），第二次注册会失败。调用方需确保单例或使用不同 registerer。
- **`observeCycle` 的 duration 计算**：`completedAt.Sub(startedAt)` 若系统时钟回拨可能为负，代码兜底为 0，但 `lastSuccess` 仍可能因时钟回拨而回退，需注意。
- **label 命名规范**：指标名以 `ongrid_edge_k8s_` 前缀，符合 Prometheus 命名规范（namespace_subsystem_name），但未使用 `Subsystem` 字段拆分，全名硬编码在 `Name` 中，可读性略逊于拆分形式。
- **`metricsResultRejected` 仅用于 pushSamples**：batches 没有 rejected result（批次不存在"拒绝"语义，只有样本才被拒绝），设计正确。
