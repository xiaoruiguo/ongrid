# `databasemetrics/plugin.go` 技术实现文档

> 源文件：`internal/edgeagent/plugins/databasemetrics/plugin.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/plugins/databasemetrics`

## 1. 概述

`databasemetrics` 是边缘端数据库指标子插件：为每个配置的数据库源启动一个 exporter 子进程（mysqld_exporter/postgres_exporter/redis_exporter/mongodb_exporter），抓取其 `/metrics` 端点后通过 tunnel `push_prom_samples` 上送。manager 通过 tunnel 一次性下发凭证，edge 写入本地受管 secret 文件（见 `secrets.go`），exporter 启动时读取。源失败时上送 `up=0` 并指数退避重启 exporter。

## 2. 包信息

- **包名**：`databasemetrics`
- **所属模块**：`internal/edgeagent/plugins/databasemetrics`
- **依赖方向**：被 main 注册到 Supervisor；调用 `plugins`/`metricscommon`/`tunnel`，子进程化各 DB exporter 二进制

## 3. 关键类型与接口

```go
const Name = "databasemetrics"

type Pusher interface {
    Call(ctx context.Context, method string, req, resp any) error
}

type EdgeIDProvider func() uint64

type Plugin struct {
    pusher  Pusher
    edgeID  EdgeIDProvider
    binDir  string
    workDir string
    log     *slog.Logger

    mu          sync.Mutex
    cfg         plugins.PluginConfig
    wantRunning bool
    cancelRun   context.CancelFunc
    stoppedCh   chan struct{}
    health      plugins.PluginHealth
    sources     map[string]plugins.TargetHealth
}
```

实现 `plugins.Plugin`。

## 4. 关键函数与流程

### `New(binDir, workDir, pusher, edgeID, log) *Plugin`
- **职责**：构造插件，workDir 拼接 `Name` 子目录。
- **流程**：log/edgeID 兜底；sources 初始化空 map；health 初始化 Stopped。

### `Configure(cfg) error`
- 解析 spec 为 sources → 加锁存 cfg → `resetSourceHealthLocked`。

### `Start(ctx) error`
- 加锁检查 wantRunning → 派生 runCtx/cancelRun/stoppedCh → 拷贝 cfg → 启 `go p.run(...)` → setPluginState(Running)。

### `Stop(ctx) error`
- 置 wantRunning=false → cancel → select stopped/ctx.Done/15s → setPluginState(Stopped)。

### `HealthSnapshot() PluginHealth`
- 加锁拷贝 health + sources → 按 ID 排序 targets。

### `run(ctx, cfg, stopped)`
- **职责**：为每个 enabled source 启 goroutine。
- **流程**：`defer close(stopped)`；defer recover 设 Crashed → parseSpec → `os.MkdirAll(workDir, 0o755)` → 遍历 sources：disabled 置状态，enabled 启 goroutine `runSource` → `wg.Wait()`。

### `runSource(ctx, source)`
- **职责**：单 source 的 exporter 启停 + 抓取循环，崩溃指数退避重启。
- **流程**：defer recover 设 failed → backoff 从 1s 起封顶 5min → 循环：
  1. select ctx.Done 返回
  2. `runExporterAndScraper(ctx, source)` → 失败且 ctx 未取消：上送 `up=0` + setSourceState(failed) + Warn 日志
  3. select ctx.Done / time.After(backoff) → backoff *= 2 封顶 5min
- **错误处理**：单次失败不退出，退避后重试；ctx 取消才退出。

### `runExporterAndScraper(ctx, source) error`
- **职责**：启动 exporter 子进程，立即抓一次，按 interval 周期抓取。
- **流程**：
  1. `readSecretFile(source.Connection.Path)` 校验 secret 文件存在且权限 ≤ 0600
  2. `source.command(binDir, secretPath, secret)` 得到 binary/args/env
  3. `os.Stat(binary)` 校验 exporter 二进制存在
  4. `exec.CommandContext(ctx, binary, args...)` → `cmd.Env = os.Environ() + env` → `cmd.Dir = workDir` → `cmd.Cancel = SIGTERM`、`cmd.WaitDelay = 10s`
  5. 打开 `workDir/<source.ID>.log` 追加 → `cmd.Stdout/Stderr = logFile`
  6. `cmd.Start()` → 启 goroutine `waitCh <- cmd.Wait()`
  7. setSourceState(running) → 立即 `scrapeAndPush` 一次
  8. `ticker = source.Interval` → select `ctx.Done` / `waitCh`（exporter 退出报错）/ `tick.C`（再次 scrapeAndPush）
- **错误处理**：exporter 退出（无论是否带错误）都返回 error 触发 runSource 退避重启。

### `scrapeAndPush(ctx, source, target)`
- **职责**：抓取 exporter 的 /metrics 并上送。
- **流程**：
  1. `metricscommon.Scrape(ctxWithTimeout=source.Timeout)` → 失败：上送 up=0 + setSourceState(failed, 0, err) + 返回
  2. 成功：append `ScrapeUpSample(up=1)` → `pushPromSamples` → 失败 setSourceState(failed, scraped, err) → 成功 setSourceState(running, scraped, nil)

### `pushPromSamples(ctx, target, samples) error`
- 取 edgeID（=0 报错 waiting register_edge）→ 15s timeout → `pusher.Call(MethodPushPromSamples, {EdgeID, Source=target.SourceLabel, Samples})`。

### `readSecretFile(path) (string, error)`
- **职责**：读取受管 secret 文件，强校验权限。
- **流程**：path 必填 → `os.Stat` → 不能是目录 → `info.Mode().Perm() & 0o077 != 0` 报"permissions too open: want 0600 or stricter" → `os.ReadFile` → trim → 空报错。

### `resetSourceHealthLocked(sources)` / `setPluginState` / `setSourceState`
- 状态更新助手，加锁或假定持锁。

## 5. 依赖关系

- **内部包**：`internal/edgeagent/plugins`、`internal/edgeagent/plugins/metricscommon`（Scrape/ScrapeUpSample/Target/DefaultInterval/DefaultTimeout）、`internal/pkg/tunnel`（PromSample/PushPromSamplesRequest/Response/MethodPushPromSamples）
- **外部库**：标准库 `context`/`fmt`/`log/slog`/`os`/`os/exec`/`path/filepath`/`runtime/debug`/`sort`/`strings`/`sync`/`syscall`/`time`
- **外部二进制**：`mysqld_exporter`/`postgres_exporter`/`redis_exporter`/`mongodb_exporter`（由 `binDir` 提供）
- **被调用方**：main.go

## 6. 并发与资源管理

- `mu sync.Mutex` 保护 cfg/wantRunning/cancelRun/stoppedCh/health/sources。
- `run` goroutine 内为每个 source 启一个 `runSource` goroutine，`sync.WaitGroup` 等待全部退出。
- 每个 `runExporterAndScraper` 内启一个 `waitCh` goroutine 监听 `cmd.Wait()`，主循环 select 在 ctx/tick/waitCh 间——exporter 退出能立即感知。
- `cmd.Cancel = SIGTERM` + `WaitDelay = 10s` 保证 exporter 优雅退出，10s 后 SIGKILL 兜底。
- `runSource` defer recover 防止单 source panic 拉垮其他 source。
- exporter 日志文件 `workDir/<source.ID>.log` 在 `runExporterAndScraper` 内 `defer close`。

## 7. 设计模式与亮点

- **每 source 独立 exporter 子进程**：不同 DB 类型用不同 exporter 二进制，端口隔离（spec.go 中默认端口 mysql 19104/pg 19187/redis 19121/mongo 19216），避免共享进程导致一类 DB 故障影响其他。
- **secret 文件权限强校验**：`readSecretFile` 拒绝 group/other 可读的 secret 文件，强制 0600 或更严，防凭证泄露。
- **exporter 崩溃自愈**：`runSource` 指数退避重启 exporter，与 `SubprocessPlugin.runLoop` 类似但每个 source 独立循环。
- **失败仍上送 up=0**：与 custommetrics 一致，让 manager 侧能直接 `up{source_id=...} == 0` 告警。
- **secret 通过 env 注入 exporter**：postgres/redis/mongo 用 `DATA_SOURCE_NAME`/`REDIS_ADDR`/`MONGODB_URI` env；mysql 用 `--config.my-cnf=<path>` 文件——适配各 exporter 的凭证接收方式。
- **exporter 日志分文件**：每 source 独立日志文件，便于按 DB 实例排查。

## 8. 注意事项

- `runSource` 中 exporter 退出（即便 exit 0）都视为失败触发重启——exporter 应常驻，意外退出需要重启。但若 exporter 因配置错误立即退出，会陷入快速重启循环（backoff 1s 起），日志会刷屏；可考虑检测"启动后 N 秒内退出"延长 backoff。
- `readSecretFile` 在 `runExporterAndScraper` 每次重启都重新读 secret——secret 文件由 manager 通过 `MethodWriteDatabaseMetricsSecret` 异步更新，重启时能拿到最新凭证，但 secret 文件被删后 source 会持续失败。
- `scrapeAndPush` 内 `metricscommon.Scrape` 失败时 setSourceState(failed) 但 exporter 子进程仍在运行——状态语义是"source 抓取失败"而非"exporter 死亡"，manager UI 需区分。
- `runExporterAndScraper` 中 `waitCh` goroutine 在 exporter 退出后 send 到 channel，主循环 select 命中后 return；但若 ctx 取消与 exporter 退出同时发生，可能优先 select 到 ctx.Done 返回 nil，waitCh goroutine 仍会完成（channel 容量 1 不会泄漏）。
- exporter 二进制缺失（`os.Stat(binary)` 失败）会让该 source 持续 failed，但不影响其他 source——main.go 需确保 binDir 部署完整。
- 端口冲突由 spec.go 在 Configure 期校验（`seenListenPorts` + `reservedListenPorts`），避免运行时 exporter 启动失败。
