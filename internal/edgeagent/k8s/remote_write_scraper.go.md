# `remote_write_scraper.go` 技术实现文档

> 源文件：`internal/edgeagent/k8s/remote_write_scraper.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/k8s`

## 1. 概述

本文件实现 `RemoteWriteScraper`：单活（single-active）Kubernetes metrics 数据平面。与 `MetricsPusher`（经 tunnel 推送）不同，它直接把抓取的指标通过 Prometheus remote_write 协议写入后端，不依赖 tunnel client。支持 kube-state-metrics 主目标与注解发现的应用 Pod 目标，内置重试、就绪状态、过程指标观察。

## 2. 包信息

- **包名**：`k8s`
- **所属模块**：`internal/edgeagent/k8s`（edgeagent 域的 Kubernetes 适配层）
- **依赖方向**：被 edgeagent 启动入口调用 `NewRemoteWriteScraper` + `Run`；依赖 `internal/edgeagent/plugins/metricscommon` 抓取、`internal/pkg/promwrite` remote_write 协议、`internal/pkg/tunnel` 的 `PromSample` 类型、本包 `apiClient`、`metrics_batch.go`、`metrics_observer.go`。

## 3. 关键类型与接口

```go
// RemoteWriteWriter 是 remote_write 后端的抽象接口
type RemoteWriteWriter interface {
    Write(ctx context.Context, samples []pkgpromwrite.Sample) error
}

// RemoteWriteScraperConfig 是 scraper 的配置
type RemoteWriteScraperConfig struct {
    ClusterID         uint64
    Endpoint         string
    DiscoverApps     bool
    Interval         time.Duration
    Timeout          time.Duration
    PushTimeout      time.Duration
    SampleLimit      int
    BatchSampleLimit int
    BatchByteLimit   int
    MaxRetries       int
    RetryBackoff     time.Duration
}

// RemoteWriteScraper 是单活数据平面主结构
type RemoteWriteScraper struct {
    writer  RemoteWriteWriter
    cfg     RemoteWriteScraperConfig
    log     *slog.Logger
    api     *apiClient
    metrics *metricsObserver
    ready   atomic.Bool
}

// 常量
const (
    defaultRemoteWriteRetries = 3
    defaultRemoteWriteBackoff = 500 * time.Millisecond
    maxRemoteWriteRetries     = 10
)

var reservedKubernetesRemoteWriteLabels = map[string]struct{}{
    "__name__":      {},
    "cluster_id":    {},
    "device_id":     {},
    "edge_id":       {},
    "ongrid_source": {},
}
```

## 4. 关键函数与流程

### `NewRemoteWriteScraper`
- **签名**：`func NewRemoteWriteScraper(writer, cfg, log, registerer) (*RemoteWriteScraper, error)`
- **职责**：公开构造入口，委托给 `newRemoteWriteScraper`（多一个 `api` 参数用于测试注入）。
- **错误处理**：委托。

### `newRemoteWriteScraper`
- **签名**：`func newRemoteWriteScraper(writer, cfg, log, registerer, api) (*RemoteWriteScraper, error)`
- **职责**：构造 scraper。
- **流程**：
  1. 校验 `writer`、`ClusterID`；`Endpoint` 与 `DiscoverApps` 至少一个。
  2. `metricscommon.ValidateURL` 校验 endpoint。
  3. 填默认值：interval/timeout（timeout 不超 interval）/pushTimeout/sampleLimit/batchSampleLimit/batchByteLimit/MaxRetries（1–10）/RetryBackoff。
  4. `log==nil` 用 `slog.Default()`。
  5. `DiscoverApps=true` 且 `api==nil` 时构造 `newInClusterAPIClient`。
  6. `newMetricsObserver(registerer)` 构造指标观察器。
- **错误处理**：任一必填缺失或参数非法返回明确错误。

### `Ready`
- **签名**：`func (s *RemoteWriteScraper) Ready() bool`
- **职责**：报告就绪状态。
- **流程**：返回 `s.ready.Load()`（atomic.Bool）。
- **注释**：说明就绪语义——核心 endpoint 配置时，可选应用发现失败不影响就绪；仅发现模式下发现成功即就绪。

### `Run`
- **签名**：`func (s *RemoteWriteScraper) Run(ctx context.Context) error`
- **职责**：主循环。
- **流程**：先立即 `scrapeAndWrite`（不等 ticker），再 `time.NewTicker(interval)` 周期触发；ctx.Done 退出。
- **设计**：首次立即执行，让就绪状态尽快确立。

### `scrapeAndWrite`
- **签名**：`func (s *RemoteWriteScraper) scrapeAndWrite(ctx) bool`
- **职责**：单次抓取+写入周期，更新就绪状态。
- **流程**：
  1. `s.targets(ctx)` 获取目标列表与发现是否成功。
  2. `hasCoreTarget` = endpoint 非空；`cycleOK = discoveryOK || hasCoreTarget`。
  3. 遍历 targets 调 `scrapeTargetAndWrite`：
     - app target 失败且（无 core target 或非 app target）→ cycleOK=false。
     - core target 完成（非 app target）→ `s.ready.Store(cycleOK)`，尽早发布就绪。
  4. 无 targets 但 DiscoverApps 且 discoveryOK → 发一个 discovery 状态样本保证就绪可观测。
  5. 无 core target → 最后 `s.ready.Store(cycleOK)`。
- **返回**：cycleOK。
- **设计**：app target 降级不影响 core 就绪；discovery-only 模式发状态样本证明链路可用。

### `targets`
- **签名**：`func (s *RemoteWriteScraper) targets(ctx) ([]metricscommon.Target, bool)`
- **职责**：组装本次要抓取的目标列表。
- **流程**：
  1. `Endpoint` 非空 → 加 core target（`s.target()`）。
  2. `DiscoverApps=false` → 直接返回（discoveryOK=true）。
  3. `api==nil` → warn 并返回 discoveryOK=false。
  4. `context.WithTimeout(ctx, cfg.Timeout)` 调 `api.listMetricPods`。
  5. 遍历 pod 调 `appMetricsTarget` 转 target。
  6. debug 日志记录发现数。
- **错误处理**：list 失败 warn 并返回 discoveryOK=false。

### `scrapeTargetAndWrite`
- **签名**：`func (s *RemoteWriteScraper) scrapeTargetAndWrite(ctx, target) bool`
- **职责**：单 target 的 scrape → batch → write 主流程。
- **流程**：
  1. 记录 `startedAt`，确定 source 与 plugin。
  2. 构造 `metricsBatcher`，push 回调为 `s.writeWithRetry`。
  3. 派生 `context.WithTimeout(ctx, cfg.Timeout)` 做 scrape 超时。
  4. `metricscommon.ScrapeIncremental(scrapeCtx, target, func(samples){ batcher.Add })` 增量抓取。
  5. `batcher.Flush()` 推送剩余批次。
  6. `metrics.observeScrape` 记录 scrape 统计。
  7. `metrics.observePush` 记录数据批次推送统计。
  8. 计算 partial 标记（limit 超限/批次失败/拒绝/scrape 错但有 accepted）。
  9. 构造 `scrapeStatusSamples`（含 up/partial/accepted）单独 write。
  10. `metrics.observePush` 记录状态批次推送统计。
  11. `metrics.observeCycle` 记录端到端耗时与成功。
  12. `logOutcome` 日志。
- **返回**：success（scrape 无错、未超限、无失败批次、无拒绝、状态批次无失败）。

### `writeWithRetry`
- **签名**：`func (s *RemoteWriteScraper) writeWithRetry(ctx, source, samples) error`
- **职责**：带指数退避重试的 remote_write。
- **流程**：
  1. `context.WithTimeout(ctx, cfg.PushTimeout)`。
  2. `buildKubernetesRemoteWriteSamples` 把 `PromSample` 转为 `pkgpromwrite.Sample`（注入 cluster_id 与 ongrid_source label，过滤保留 label）。
  3. 循环 `MaxRetries` 次：`writer.Write(writeCtx, payload)` 成功即返回；失败记 lastErr。
  4. 非最后一次：计算退避 `RetryBackoff * 2^(attempt-1)`，上限 2s，`time.NewTimer` 等待，select ctx.Done 提前退出。
  5. 全部失败返回 `remote_write failed after %d attempts: %w`。
- **错误处理**：ctx 取消时 `errors.Join(lastErr, writeCtx.Err())` 合并错误。

### `s.target()`
- **签名**：`func (s *RemoteWriteScraper) target() metricscommon.Target`
- **职责**：构造 core（kube-state-metrics）target，含 LabelDrop 脱敏列表。

### `buildKubernetesRemoteWriteSamples`
- **签名**：`func buildKubernetesRemoteWriteSamples(clusterID, source, samples) []pkgpromwrite.Sample`
- **职责**：把 `tunnel.PromSample` 转为 `pkgpromwrite.Sample`。
- **流程**：
  1. 每个 sample 构造 labels：先加 `__name__` 与 `cluster_id`，source 非空加 `ongrid_source`。
  2. 遍历 `sample.Labels`，跳过 reserved label（防覆盖注入的 label）。
  3. `sort.Slice` 按 label name 排序（remote_write 协议要求排序）。
  4. 构造 `pkgpromwrite.Sample{Labels, Value, TsMs}`。
- **设计**：reserved label 集合保证注入的 cluster_id/ongrid_source 不被样本自身 label 覆盖。

### `logOutcome`
- **签名**：`func (s *RemoteWriteScraper) logOutcome(target, stats, dataStats, statusStats, scrapeErr)`
- **职责**：按结果等级输出日志（scrape 失败 warn、limit 超限 warn、remote_write 部分失败 warn）。

## 5. 依赖关系

- **内部包**：
  - `github.com/ongridio/ongrid/internal/edgeagent/plugins/metricscommon`：`Target`、`ScrapeIncremental`、`ScrapeStats`、`ScrapeUpSample`、`ScrapeStatusSamples`、`ValidateURL`。
  - `github.com/ongridio/ongrid/internal/pkg/promwrite`：`Sample`、`Label`（remote_write 协议数据结构）。
  - `github.com/ongridio/ongrid/internal/pkg/tunnel`：`PromSample`、`PushPromSamplesRequest`（复用类型，但不通过 tunnel 推送）。
  - 本包 `inventory.go`：`apiClient`、`listMetricPods`、`netJoinHostPort`、`podItem`、`controllerOwner`。
  - 本包 `metrics.go`：`MetricsConfig`、`appMetricsTarget`、`scrapeStatusSamples`、`k8sMetricsSource`/`k8sAppMetricsSource` 常量。
  - 本包 `metrics_batch.go`：`metricsBatcher`、`metricsBatchStats`。
  - 本包 `metrics_observer.go`：`metricsObserver`、`newMetricsObserver`。
- **外部库**：
  - `github.com/prometheus/client_golang/prometheus`：`Registerer`。
  - 标准库 `context`、`errors`、`fmt`、`log/slog`、`sort`、`strconv`、`strings`、`sync/atomic`、`time`。
- **被调用方**：edgeagent 启动入口。

## 6. 并发与资源管理

- **单 goroutine 主循环**：`Run` 是单 goroutine，`scrapeAndWrite` 串行执行。
- **`atomic.Bool` 就绪状态**：`ready` 字段用 `atomic.Bool`，允许其他 goroutine（如 healthz handler）并发读 `Ready()` 而无需锁。
- **context 贯穿**：scrape 与 write 都派生超时 ctx；`writeWithRetry` 的退避 timer 用 select ctx.Done 提前取消。
- **timer 资源管理**：`writeWithRetry` 的退避 timer 在 ctx.Done 路径调 `timer.Stop()`（未 drain `timer.C`，因为已经 return；理论上有轻微竞态但不影响正确性）。
- **无共享状态**：`RemoteWriteScraper` 字段在构造后只读（除 `ready`），`writer`/`api`/`metrics` 都由底层保证并发安全。

## 7. 设计模式与亮点

- **接口抽象**：`RemoteWriteWriter` 接口解耦 scraper 与具体 remote_write 实现，便于测试 mock。
- **构造函数重载**：`NewRemoteWriteScraper`（公开，无 api 参数）+ `newRemoteWriteScraper`（内部，有 api 参数）便于测试注入 mock apiClient。
- **就绪状态语义**：注释清晰说明 core 与 app 的就绪独立性——core 成功即就绪，app 降级不影响；discovery-only 模式发状态样本保证就绪可观测。
- **指数退避重试**：`writeWithRetry` 用 `RetryBackoff * 2^(attempt-1)` 指数退避，上限 2s，MaxRetries 限制（默认 3，上限 10），平衡恢复速度与退避压力。
- **reserved label 保护**：`buildKubernetesRemoteWriteSamples` 用 reserved set 防止样本自身 label 覆盖注入的 cluster_id/ongrid_source，保证多租户隔离。
- **label 排序**：remote_write 协议要求 label 排序，`sort.Slice` 保证合规。
- **状态样本分离**：数据批次与状态批次分别 write，便于后端分别处理；状态批次也走重试。
- **端到端指标**：`observeCycle` 记录 scrape+write 的端到端耗时与成功，是 `MetricsPusher` 未有的增强。
- **降级策略**：app target 失败不影响 core 就绪；无 target 时发 discovery 状态样本，保证就绪可观测。

## 8. 注意事项

- **`scrapeAndWrite` 的就绪发布时机**：core target 完成后立即 `ready.Store(cycleOK)`，但若后续 app target 失败且无 core，cycleOK 会被改写为 false——但此时 ready 已被设为 true（core 成功），不会被改回。这是有意的：core 成功即就绪，app 失败不影响。
- **`writeWithRetry` 的 timer drain 缺失**：ctx.Done 路径 `timer.Stop()` 后未 drain `timer.C`，若 timer 已触发则 `timer.C` 中有值，下次复用该 timer（实际未复用，因为 return）不会出问题。但若未来改为循环复用需注意。
- **`buildKubernetesRemoteWriteSamples` 的 label 内存分配**：每个 sample 都 `make([]Label, 0, len(labels)+3)`，大样本量下分配频繁，可考虑复用 buffer 优化。
- **`scrapeTargetAndWrite` 的 success 判定**：要求 scrape 无错、未超限、数据批次无失败、无拒绝、状态批次无失败。标准严格，任何部分失败都标记 not ready，可能过于敏感——实际运维中可能希望"数据成功但状态批次失败"仍算就绪。当前设计优先严格性。
- **`targets` 的 discovery 超时**：用 `cfg.Timeout`（默认 15s）作为 discovery 超时，与 scrape 超时共用，大集群下 list pods 可能较慢。
- **`listMetricPods` 非分页**：与 `metrics.go` 相同问题，单次 GET 大集群下可能截断。
- **`MaxRetries` 上限 10**：防止配置错误导致无限重试，但 10 次 × 2s 退避 = 最长 20s+，可能超过 PushTimeout，需注意配置合理性。
- **`reservedKubernetesRemoteWriteLabels` 全局变量**：是包级 var，不可变（map 内容未暴露修改接口），并发安全。
