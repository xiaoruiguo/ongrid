# `custommetrics/plugin.go` 技术实现文档

> 源文件：`internal/edgeagent/plugins/custommetrics/plugin.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/plugins/custommetrics`

## 1. 概述

`custommetrics` 是边缘端"自定义 Prometheus 端点抓取"子插件：操作员提供一组已存在的 `/metrics` URL，edge 本地抓取后通过 tunnel `push_prom_samples` RPC 上送。本插件**不管理 exporter 子进程、不持有数据库凭证**——只做抓取与转发。每个 target 在独立 goroutine 中按自己的 interval 周期抓取，失败时仍上送 `up=0` 合成样本以便告警。

## 2. 包信息

- **包名**：`custommetrics`
- **所属模块**：`internal/edgeagent/plugins/custommetrics`（plugins 域下的多目标指标子插件）
- **依赖方向**：被 main 注册到 Supervisor；调用 `plugins`（接口/类型）、`metricscommon`（Scrape/Target/ScrapeUpSample）、`internal/pkg/tunnel`（PromSample/PushPromSamples）

## 3. 关键类型与接口

```go
const Name = "custommetrics"

type Pusher interface {
    Call(ctx context.Context, method string, req, resp any) error
}

type EdgeIDProvider func() uint64

type Plugin struct {
    pusher Pusher
    edgeID EdgeIDProvider
    log    *slog.Logger

    mu          sync.Mutex
    cfg         plugins.PluginConfig
    wantRunning bool
    cancelRun   context.CancelFunc
    stoppedCh   chan struct{}
    health      plugins.PluginHealth
    targets     map[string]plugins.TargetHealth // 源级健康
}
```

实现 `plugins.Plugin` 接口。

## 4. 关键函数与流程

### `New(pusher, edgeID, log) *Plugin`
- **职责**：构造插件，初始化 health 为 `StateStopped`、targets 为空 map。
- **流程**：log/edgeID 兜底为 default / 返回 0 的 stub。

### `Configure(cfg) error`
- **职责**：解析 spec 为 targets 列表，重置 target 健康。
- **流程**：`parseSpec(cfg.Spec)` → 失败返回错误 → 加锁存 cfg → `resetTargetHealthLocked(targets)`。
- **错误处理**：spec 不合法直接返回，Supervisor 据此决定是否标记 crashed。

### `Start(ctx) error`
- **职责**：启动抓取主 goroutine；幂等。
- **流程**：加锁检查 `wantRunning` → 派生 runCtx/cancelRun → 创建 stoppedCh → 拷贝 cfg → 启 `go p.run(runCtx, cfgCopy, stopped)` → `setPluginState(Running)`。

### `Stop(ctx) error`
- **职责**：取消 runCtx，等 run 退出；10s 超时。
- **流程**：置 wantRunning=false → cancel → select `stopped`/`ctx.Done`/10s 超时 → setPluginState(Stopped)。

### `HealthSnapshot() PluginHealth`
- **职责**：返回当前插件健康 + 所有 target 健康。
- **流程**：加锁拷贝 health → 拷贝 targets 切片 → `sortTargetHealth` 按 ID 排序。

### `run(ctx, cfg, stopped)`
- **职责**：为每个 enabled target 启 goroutine 抓取，wg 等待全部退出。
- **流程**：`defer close(stopped)`；defer recover 记录 panic 并 setPluginState(Crashed) → parseSpec → 遍历 targets：disabled 直接置状态，enabled 启 goroutine `runTarget` → `wg.Wait()`。
- **错误处理**：parseSpec 失败 setPluginState(Crashed) 并 return。

### `runTarget(ctx, target)`
- **职责**：单个 target 的抓取循环。
- **流程**：defer recover 记录 panic 并 setTargetState(failed) → 立即 `scrapeAndPush` 一次 → `time.NewTicker(target.Interval)` 循环 → select `ctx.Done`/`tick.C`。
- **错误处理**：panic 不退出循环（recover 后函数 return，等下次 Start 才会重启该 target）。

### `scrapeAndPush(ctx, target)`
- **职责**：抓一次 + 上送一次。
- **流程**：
  1. `metricscommon.Scrape(ctxWithTimeout)` → 失败：上送 `up=0`（pushPromSamples 失败仅 Warn）→ setTargetState(failed, 0, err) → 返回
  2. 成功：append `ScrapeUpSample(up=1)` → `pushPromSamples` → 失败 setTargetState(failed, scraped, err) → 成功 setTargetState(running, scraped, nil)

### `pushPromSamples(ctx, target, samples) error`
- **职责**：调 tunnel RPC。
- **流程**：取 edgeID（=0 时返回错误"waiting for register_edge"）→ 15s timeout ctx → `pusher.Call(MethodPushPromSamples, PushPromSamplesRequest{EdgeID, Source=target.SourceLabel, Samples})`。

### `resetTargetHealthLocked(targets)`
- **职责**：Configure 时重建 target 健康表（持锁调用）。
- **流程**：每个 target 初始 state="running"（disabled 置 "disabled"），写入新 map 替换旧。

### `setPluginState(st, err)` / `setTargetState(target, state, samples, err)`
- 加锁更新对应字段；err 非 nil 时写 LastError，state=running 时清空并更新 LastSuccessAt。

### `sortTargetHealth(items)`
- O(n²) 选择排序按 ID 升序。target 数量小可接受，但建议改 `sort.Slice`。

## 5. 依赖关系

- **内部包**：`internal/edgeagent/plugins`（Plugin/PluginConfig/PluginHealth/TargetHealth/State 常量）、`internal/edgeagent/plugins/metricscommon`（Scrape/Target/ScrapeUpSample/DefaultInterval/DefaultTimeout）、`internal/pkg/tunnel`（PromSample/PushPromSamplesRequest/Response/MethodPushPromSamples）
- **外部库**：标准库 `context`/`fmt`/`log/slog`/`runtime/debug`/`strings`/`sync`/`time`
- **被调用方**：main.go 注册到 Supervisor

## 6. 并发与资源管理

- `mu sync.Mutex` 保护 cfg/wantRunning/cancelRun/stoppedCh/health/targets。
- `run` goroutine 内为每个 target 启一个子 goroutine（`runTarget`），`sync.WaitGroup` 等待全部退出。
- `runCtx` 派生自 Start 的 ctx；Stop 调 cancelRun 传播取消到所有 target goroutine。
- `scrapeAndPush` 内 `context.WithTimeout(ctx, target.Timeout)` 限定单次抓取；`pushPromSamples` 内 `context.WithTimeout(ctx, 15s)` 限定单次 RPC。
- defer recover 包裹 run 与每个 runTarget，防止 panic 拉垮整个插件或影响其他 target。

## 7. 设计模式与亮点

- **每 target 独立 goroutine + 独立 interval**：不同 target 抓取频率解耦，慢 target 不阻塞快 target。
- **失败仍上送 up=0**：target 不可达时合成 `up=0` 样本上送，让 manager 侧 PromQL 能直接 `up{target_id=...} == 0` 告警，无需从"无样本"推断。
- **panic 隔离**：单 target panic 不影响其他 target（recover 后该 target 退出，其他继续）。
- **target 健康可见性**：`TargetHealth` 携带 Samples/LastSuccessAt，manager UI 可展示每个 target 的抓取量与时延。
- **edgeID 延迟解析**：用 `EdgeIDProvider func() uint64` 而非构造期固定值，避免 register_edge 未完成时丢首批样本（scrapeAndPush 内 edgeID=0 时返回错误，下次 tick 自动重试）。

## 8. 注意事项

- `runTarget` 中 panic 后函数 return，该 target 永久停止直到下次 Configure/Start；若希望自愈可在 recover 后重启 backoff 循环（参考 databasemetrics 的 `runSource`）。
- `sortTargetHealth` 用 O(n²) 选择排序，target 数量大时建议改 `sort.Slice`。
- `pushPromSamples` 失败时仅 setTargetState(failed) 但不重试，下一 tick 才会再次尝试——对短暂网络抖动可接受，但持续失败会让样本 gap 一个 interval。
- `scrapeAndPush` 中 scrape 失败仍尝试上送 up=0，若此时 pushPromSamples 也失败（如 edge 未注册），up=0 永远到不了 manager——manager 侧应通过"长期无样本"告警兜底。
- target.URL 等敏感信息仅记日志（log.Warn 时记 url），不上送为 metric label（ScrapeUpSample 的 labels 仅含 target_id/name/kind），符合"高基数字段禁止做 label"。
