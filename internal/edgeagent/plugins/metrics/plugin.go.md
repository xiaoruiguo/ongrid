# `metrics/plugin.go` 技术实现文档

> 源文件：`internal/edgeagent/plugins/metrics/plugin.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/plugins/metrics`

## 1. 概述

`metrics` 是边缘端指标插件的 in-process 实现：周期性抓取本地 `/metrics` 端点（默认同时抓 node_exporter:9102 与 process_exporter:9256），解析 Prometheus 文本格式，通过 tunnel `push_prom_samples` RPC 上送。与 logs/traces 子进程插件不同，本插件无子进程，全部在 ongrid-edge 进程内 goroutine 运行。走 tunnel 而非直接 remote_write 的原因：复用已认证的 tunnel 链路 + manager 侧 ingester 注入 canonical `device_id` label。

## 2. 包信息

- **包名**：`metrics`
- **所属模块**：`internal/edgeagent/plugins/metrics`
- **依赖方向**：被 main 注册到 Supervisor；调用 `internal/edgeagent/plugins`（接口）、`internal/pkg/tunnel`（RPC）、本包 `scrape.go`（scrapeOnce/parseSpec）

## 3. 关键类型与接口

```go
const Name = "metrics"

type Pusher interface {
    Call(ctx context.Context, method string, req, resp any) error
}

type EdgeIDProvider func() uint64

type Plugin struct {
    pusher Pusher
    edgeID EdgeIDProvider
    log    *slog.Logger

    mu sync.Mutex
    cfg plugins.PluginConfig
    wantRunning bool
    cancelRun context.CancelFunc
    stoppedCh chan struct{}
    health plugins.PluginHealth
    scrapeCount uint64
    failureCount uint64
}
```

实现 `plugins.Plugin`。

## 4. 关键函数与流程

### `New(pusher, edgeID, log) *Plugin`
- log/edgeID 兜底；health 初始化 Stopped。

### `Configure(cfg) error`
- **职责**：校验 spec 并存储。
- **流程**：`parseSpec(cfg.Spec)` → 失败返回错误 → 加锁存 cfg → Debug 日志。
- **错误处理**：spec 不合法直接返回，Supervisor 据此决定是否重启。

### `Start(ctx) error`
- 加锁检查 wantRunning → 派生 runCtx/cancelRun/stoppedCh → 拷贝 cfg → 启 `go runLoop(...)` → setState(Running)。

### `Stop(ctx) error`
- 置 wantRunning=false → cancel → select stopped/ctx.Done/10s 超时 → setState(Stopped)。

### `HealthSnapshot() PluginHealth`
- 加锁拷贝 health + 刷新 UpdatedAt。

### `runLoop(ctx, cfg, stopped)`
- **职责**：抓取主循环。
- **流程**：`defer close(stopped)` → `parseSpec(cfg.Spec)` 失败 setState(Crashed) return → 立即 `scrapeAndPush` 一次（首批样本不等完整 interval）→ `ticker = spec.Interval` → select `ctx.Done`/`tick.C`。
- **错误处理**：spec 解析失败 Crashed 退出；单次抓取失败不影响下一 tick。

### `scrapeAndPush(ctx, spec)`
- **职责**：遍历 spec.URLs 逐个抓取上送。
- **流程**：for 每个 targetURL 调 `scrapeAndPushOne`。

### `scrapeAndPushOne(ctx, spec, targetURL)`
- **职责**：单次抓取 + 上送。
- **流程**：
  1. `ctxWithTimeout = spec.Timeout` → `scrapeOnce(rctx, spec, targetURL)` → 返回 samples/source/err
  2. 加锁 `scrapeCount++`
  3. err 非 nil：`bumpFailure(err)` + Warn 日志 + return
  4. samples 空：Debug 日志 + return
  5. `edgeID = p.edgeID()`：=0 时 Debug 日志"deferring push until register_edge completes" + return（不计数失败）
  6. `pctx = 15s timeout` → `pusher.Call(MethodPushPromSamples, {EdgeID, Source, Samples})` → err 非 nil `bumpFailure` + Warn + return
  7. 成功 Debug 日志（samples/accepted）
- **错误处理**：scrape 失败与 push 失败都 bumpFailure 但不退出循环；edgeID=0 静默等待下次 tick。

### `setState(st, err)`
- 加锁更新 health.State/UpdatedAt/LastError；Running 时清 LastError + 设 StartedAt。

### `bumpFailure(err)`
- 加锁 `failureCount++` + 写 LastError + UpdatedAt。

## 5. 依赖关系

- **内部包**：`internal/edgeagent/plugins`（Plugin/PluginConfig/PluginHealth/State 常量）、`internal/pkg/tunnel`（PushPromSamplesRequest/Response/MethodPushPromSamples/PromSample）、本包 `scrape.go`（scrapeOnce/parseSpec/specView）
- **外部库**：标准库 `context`/`log/slog`/`sync`/`time`
- **被调用方**：main.go

## 6. 并发与资源管理

- `mu sync.Mutex` 保护 cfg/wantRunning/cancelRun/stoppedCh/health/scrapeCount/failureCount。
- `runLoop` 在独立 goroutine，`Stop` 通过 cancelRun 传播取消。
- `scrapeAndPushOne` 内 `context.WithTimeout(ctx, spec.Timeout)` 限定单次抓取；push 用 `context.WithTimeout(ctx, 15s)` 限定单次 RPC。
- `scrapeOnce`（scrape.go）内部 `newClient` 每次 new 一个 `*http.Client`——不池化，注释说明单 target 场景 keep-alive 收益不抵生命周期复杂度。

## 7. 设计模式与亮点

- **in-process vs 子进程权衡**：metrics 用 in-process 抓取 + tunnel RPC，避免引入 remote_write 客户端依赖与公网暴露；logs/traces 用子进程（promtail/otelcol 自带重试/队列）。
- **首批立即抓取**：`runLoop` 在 ticker 前先 `scrapeAndPush` 一次，让刚启用的插件迅速产生样本而非等满一个 interval。
- **edgeID 延迟解析**：用 `EdgeIDProvider func() uint64`，register_edge 完成前 edgeID=0，Debug 日志静默等待，不丢样本（下次 tick 重试）。
- **per-URL 隔离**：`scrapeAndPush` 遍历 URLs，单 URL 失败不影响其他 URL——node_exporter 挂了不影响 process_exporter 抓取。
- **失败计数器**：scrapeCount/failureCount 暴露运行健康度（虽未在 HealthSnapshot 暴露，仅 LastError）。
- **tunnel 复用**：上送走已有 tunnel，无需 manager 侧暴露公网 remote_write 端点 + 鉴权。

## 8. 注意事项

- `HealthSnapshot` 仅返回 health 不含 scrapeCount/failureCount——manager 侧无法看到抓取统计，可考虑在 health.LastError 或 Targets 字段暴露。
- `bumpFailure` 写 `health.LastError = err.Error()`，连续失败时 LastError 不断覆盖；若需保留首错误可加条件。
- `scrapeAndPushOne` 中 edgeID=0 静默 return 不计 failure——正确（不是抓取失败而是上送延后），但若 register_edge 长期不完成，会持续无样本上送，manager 侧需通过"长期无样本"告警兜底。
- `runLoop` 中 spec 解析失败立即 Crashed return，不会重试——需 Supervisor 重新 Configure（manager 推新配置）才能恢复；scrape.go 的 parseSpec 已在 Configure 期校验过，运行时再解析失败概率极低。
- `Stop` 10s 超时后 Warn 并返回，不强制 kill runLoop goroutine——若 runLoop 卡在 push RPC（15s timeout），Stop 已返回但 goroutine 仍在，下次 Start 会再启一个。SubprocessPlugin 用 stoppedCh 等，本插件也用 stoppedCh 但超时不阻塞——可能短暂多 goroutine。可考虑 Stop 超时后强制 cancelRun（已通过 cancel 传播，但 select 命中 time.After 后 cancel 已执行，runLoop 会在下次 ctx.Done 退出）。
- `scrapeOnce` 每次 new `*http.Client`，对 localhost 单 target 无性能问题；若未来扩展到多 target 远程抓取，连接池化收益显著。
- compile-time guard `var _ plugins.Plugin = (*Plugin)(nil)` 保证接口契约，好实践。
