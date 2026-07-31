# `scrape.go` 技术实现文档

> 源文件：`internal/edgeagent/collector/scrape.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/collector`

## 1. 概述

本文件实现 `Scraper`：多目标 HTTP scraper，每个目标起一个 goroutine 周期 `GET /metrics`，用 `expfmt.TextParser` 解析 Prometheus 文本格式为 `*dto.MetricFamily`，存内存快照。`CollectAll` 每个目标产出独立 `CollectorOutput`，host-role 目标还派生 `HostMetricPoint` 快路径。`HostInfo` / `GetProcessList` 仍走 gopsutil（scraper 不从任意 metric 名派生 host load）。

## 2. 包信息

- **包名**：`collector`
- **所属模块**：edgeagent metric 采集层（scrape 模式）
- **依赖方向**：被 `cmd/ongrid-edge` 或 `CompositeCollector` 构造；调用 `expfmt`、`gopsutil`

## 3. 关键类型与接口

```go
// Scraper 多目标 HTTP scraper
type Scraper struct {
    cfg     *ScrapeConfig
    log     *slog.Logger
    clients map[string]*http.Client  // per-target client（TLS / bearer auth）
    mu       sync.RWMutex
    snapshot map[string]targetSnapshot
    mappers  map[string]*Mapper       // per-target Mapper（counter 状态独立）
}

// targetSnapshot 单次 scrape 结果
type targetSnapshot struct {
    families []*dto.MetricFamily
    at       time.Time
    source   string  // "scrape:<name>"
    role     string   // host / component
}
```

## 4. 关键函数与流程

### `NewScraper`
- **签名**：`func NewScraper(cfg *ScrapeConfig, log *slog.Logger) *Scraper`
- **职责**：构造 scraper，为每个 target 建独立 http.Client + Mapper
- **流程**：遍历 `cfg.Targets`，每目标 `newHTTPClient(t)` + `NewMapper()`；返回结构体字面量
- **错误处理**：无错误返回

### `Run`
- **签名**：`func (s *Scraper) Run(ctx context.Context) error`
- **职责**：阻塞直到 ctx 取消，每目标一个 goroutine
- **流程**：`errgroup.WithContext` 起 N 个 `runTarget` goroutine；`eg.Wait()` 直到全部退出
- **错误处理**：`context.Canceled` 返回 nil；其他错误透传

### `runTarget`
- **签名**：`func (s *Scraper) runTarget(ctx context.Context, t ScrapeTarget)`
- **职责**：单目标周期 scrape 循环
- **流程**：
  1. 立即执行一次 `scrapeOnce`（首次 tick 不必等 Interval）
  2. `time.NewTicker(t.Interval)` 周期 scrape
  3. ctx 取消退出
- **错误处理**：scrapeOnce 内部错误仅 Warn，不退出循环

### `scrapeOnce`
- **签名**：`func (s *Scraper) scrapeOnce(ctx context.Context, t ScrapeTarget)`
- **职责**：单次 HTTP scrape + 解析 + 存快照
- **流程**：
  1. `context.WithTimeout(ctx, t.Timeout)`
  2. `http.NewRequestWithContext(GET, t.URL)`
  3. `Accept: text/plain`（expfmt format）
  4. `BearerTokenFile` 非空时读 token 加 `Authorization` header
  5. `s.clients[t.Name].Do(req)`；非 2xx 丢弃 body 并 Warn
  6. `expfmt.TextParser.TextToMetricFamilies(resp.Body)` 解析
  7. `familiesToSlice` 按 name 排序（测试稳定）
  8. `mu.Lock` 更新 `s.snapshot[t.Name]`
- **错误处理**：每步失败 Warn 后 return；不存部分结果

### `CollectAll`
- **签名**：`func (s *Scraper) CollectAll(ctx context.Context) ([]CollectorOutput, error)`
- **职责**：返回每目标的 CollectorOutput
- **流程**：
  1. `mu.RLock` 拷贝 snapshot 列表
  2. 每目标：`FlattenSamples(now, source, families, target.StaticLabels)` 产 samples
  3. host-role 目标额外 `mp.MapToHostPoint(now, families)` + `HostPointValid=true`
  4. 顺序追加到 out
- **错误处理**：无目标返回 nil, nil；不返回错误（per-target 错误在 scrapeOnce 已 Warn）

### `HostInfo`
- **签名**：`func (s *Scraper) HostInfo(ctx context.Context) (tunnel.HostInfo, error)`
- **职责**：与 EmbeddedCollector.HostInfo 一致——scraper 仍需 *本机* 身份，不来自 target
- **流程**：runtime.GOOS / GOARCH / NumCPU + host.InfoWithContext + hardwareFingerprint + primaryIPv4 + mem.VirtualMemory + cpu.Counts
- **错误处理**：每个 gopsutil 调用失败保留默认值

### `GetHostLoad`
- **签名**：`func (s *Scraper) GetHostLoad(ctx context.Context) (tunnel.GetHostLoadResponse, error)`
- **职责**：从已 scrape 的 host-role 目标派生 load
- **流程**：
  1. `mu.RLock` 取所有 snapshot key（按名排序）
  2. 遍历找 `role==host` 且 families 非空 且 mapper 非 nil
  3. `mp.MapToHostPoint(now, families)`
  4. 全零（CPU/Mem/Load 都 0）跳过；非零填 resp 返回
- **错误处理**：找不到合适目标返回全零响应

### `GetProcessList`
- **签名**：`func (s *Scraper) GetProcessList(ctx context.Context, topN int, sortBy string) (tunnel.GetProcessListResponse, error)`
- **职责**：委托 gopsutil（scraped targets 不携带标准进程表）
- **流程**：与 `EmbeddedCollector.GetProcessList` 完全一致

### helper 函数
- `newHTTPClient(t)`：per-target client，TLS MinVersion 1.2 + optional InsecureSkipVerify + IdleConnTimeout + keep-alive
- `readToken(path)` / `readFileTrim(path)`：读 bearer token 文件
- `familiesToSlice(in)`：map→slice 按 name 排序（测试稳定）

## 5. 依赖关系

- **内部包**：`internal/pkg/tunnel`
- **外部库**：`github.com/prometheus/common/expfmt`、`github.com/prometheus/client_model/go`、`github.com/shirou/gopsutil/v3`（cpu/host/mem/process）、`golang.org/x/sync/errgroup`、标准库 `crypto/tls`、`net/http`、`os`、`runtime`、`sort`、`strings`、`sync`、`time`
- **被调用方**：`cmd/ongrid-edge` 直接构造；或经 `CompositeCollector` 包装

## 6. 并发与资源管理

- **per-target goroutine**：`runTarget` 起 N 个 goroutine，errgroup 管理；ctx 取消全部退出
- **`sync.RWMutex`**：`scrapeOnce` 写 `s.snapshot` 用 Lock；`CollectAll` / `GetHostLoad` 读用 RLock
- **per-target Mapper**：每个 target 独立 Mapper，counter 缓存不混淆
- **per-target http.Client**：连接池隔离，TLS 配置独立
- **context 超时**：每次 scrape 用 `context.WithTimeout(ctx, t.Timeout)`

## 7. 设计模式与亮点

- **多目标多 goroutine**：一个 Scraper 管 N 个 target，errgroup 统一生命周期；单 target 失败不影响其他
- **立即首次 scrape**：`runTarget` 启动时立即执行一次，让首个 agent tick 不必等 Interval
- **per-target Mapper**：counter delta 状态独立，不同 target 的同 metric 不互相干扰
- **host-role 与 component-role 区分**：host-role 目标额外产 `HostMetricPoint`（dashboard / alert 快路径）；component-role 仅产 samples（rich path）
- **scrape 不派生 host load**：`HostInfo` / `GetProcessList` 仍走 gopsutil——scraper 不试图从任意 metric 名派生 host load，保持语义清晰
- **GetHostLoad 零值检测**：全零的 host snapshot 视为「无数据」跳过，避免显示空负载
- **familiesToSlice 排序**：map 迭代无序，按 name 排序让测试 fixture 稳定
- **BearerTokenFile 每次读**：不缓存 token，支持 token 轮换；但每次 IO，高频 scrape 时有微小开销

## 8. 注意事项

- `scrapeOnce` 失败时不更新 snapshot——下次 `CollectAll` 仍用旧 snapshot；旧数据可能过期，cloud 侧应通过 `at` 时间戳判断新鲜度（但当前 `CollectorOutput` 不带时间戳，仅 Samples 内每个 metric 有 TsMs）
- `GetHostLoad` 遍历所有 host-role target 取首个非零——多 host target 时行为未定义；operator 应只配一个 host-role target
- `BearerTokenFile` 每次读文件——若文件权限错误或被删，每次 scrape 都 Warn；应监控日志
- `TLSInsecure` 是 per-target 配置——operator 应在自签证书场景谨慎使用
- `newHTTPClient` 的 `MaxIdleConnsPerHost=2`——高频率 scrape 可能不够；但单 target 单 host 通常足够
- `HostInfo` 与 `EmbeddedCollector.HostInfo` 重复实现——可考虑提取到共享 helper（当前为代码重复）
- `GetProcessList` 与 `EmbeddedCollector.GetProcessList` 完全重复——同样可提取共享 helper
- `expfmt.TextParser.TextToMetricFamilies` 对 malformed 输入返回 partial families + error；当前实现遇到 error 直接 Warn 丢弃所有 families——可考虑保留 partial（当前丢弃）
