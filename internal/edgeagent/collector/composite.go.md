# `composite.go` 技术实现文档

> 源文件：`internal/edgeagent/collector/composite.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/collector`

## 1. 概述

本文件实现 `CompositeCollector`：把 embedded 基线采集器与 scrape 多目标采集器组合成「auto 模式」。规则是：host-role scrape 目标成功时取代 embedded 的 host 快路径；否则回退到 embedded 基线。component-role scrape 目标只贡献 rich-path Prom 样本。

## 2. 包信息

- **包名**：`collector`
- **所属模块**：edgeagent metric 采集层
- **依赖方向**：被 `cmd/ongrid-edge` 构造；调用同包 `Scraper` + 外部 `Collector` 接口实现

## 3. 关键类型与接口

```go
// CompositeCollector 组合 embedded + scraper
type CompositeCollector struct {
    embedded Collector      // 内嵌基线（通常是 EmbeddedCollector）
    scraper  *Scraper       // 多目标 scrape
    log      *slog.Logger
}
```

## 4. 关键函数与流程

### `NewComposite`
- **签名**：`func NewComposite(embedded Collector, scraper *Scraper, log *slog.Logger) *CompositeCollector`
- **职责**：构造组合采集器
- **流程**：log 为 nil 时 fallback `slog.Default()`；直接返回结构体字面量
- **错误处理**：无错误返回

### `CollectAll`
- **签名**：`func (c *CompositeCollector) CollectAll(ctx context.Context) ([]CollectorOutput, error)`
- **职责**：合并 scrape + embedded 输出
- **流程**：
  1. 若 scraper 非 nil：调 `scraper.CollectAll`；失败仅 Warn；遍历输出，若任一 `HostPointValid=true` 置 `hostFromScrape=true`
  2. 若 `!hostFromScrape && embedded != nil`：调 `embedded.CollectAll` 追加其输出（host 快路径回退）
  3. 返回合并 slice
- **错误处理**：scraper 失败仅 Warn 继续；embedded 失败返回错误（host 快路径回退失败时）

### `HostInfo`
- **签名**：`func (c *CompositeCollector) HostInfo(ctx context.Context) (tunnel.HostInfo, error)`
- **职责**：返回主机身份
- **流程**：优先 embedded；embedded 为 nil 时用 scraper；都为 nil 返回空 HostInfo
- **错误处理**：直接透传

### `GetHostLoad`
- **签名**：`func (c *CompositeCollector) GetHostLoad(ctx context.Context) (tunnel.GetHostLoadResponse, error)`
- **职责**：返回当前主机负载
- **流程**：
  1. scraper 非 nil 时先尝试；若返回的 CPUPct/MemPct/Load1 任一非零直接返回
  2. 否则回退到 embedded
- **错误处理**：scraper 错误时回退 embedded，不返回错误

### `GetProcessList`
- **签名**：`func (c *CompositeCollector) GetProcessList(ctx context.Context, topN int, sortBy string) (tunnel.GetProcessListResponse, error)`
- **职责**：返回 topN 进程列表
- **流程**：优先 embedded（scraper 不携带标准进程表）；embedded 为 nil 时用 scraper；都为 nil 返回空响应

## 5. 依赖关系

- **内部包**：`internal/pkg/tunnel`（仅用类型）
- **外部库**：`log/slog`、标准库 `context`
- **被调用方**：`cmd/ongrid-edge` 主程序（auto 模式构造时）

## 6. 并发与资源管理

CompositeCollector 自身无锁；它依赖内部 embedded / scraper 的线程安全。`CollectAll` 调用顺序同步：先 scraper 再 embedded。被 `Agent.metricsLoop` 在 ticker goroutine 中串行调用。

## 7. 设计模式与亮点

- **回退优先级链**：host-role scrape 成功 → 取代 embedded 快路径；失败 → 回退 embedded 基线。让 operator 配置 scrape 后仍能在 scrape 失败时保持快路径可见
- **component-role 只贡献 samples**：scrape 输出 `HostPointValid=false`（非 host role），不会触发 `hostFromScrape=true`，embedded 基线照常工作
- **GetHostLoad 零值检测**：scraper 返回全零（如 scrape 还没拿到 CPU%）视为「无数据」回退 embedded，避免显示空负载
- **简单组合优于继承**：CompositeCollector 持有两个 Collector 引用，无继承层级；符合 Go「组合优于继承」惯例

## 8. 注意事项

- `CollectAll` 中 scraper 错误仅 Warn 不返回——下一 tick 用新数据重试；embedded 错误则返回，因为 host 快路径是关键路径
- `GetHostLoad` 的零值检测假设：真实负载不会三个字段同时为 0；边缘情况（idle 主机）会被误判为「无数据」回退 embedded——但 embedded 也会返回 0，最终用户看到 0%，无实质差异
- `HostInfo` / `GetProcessList` 都用 scraper 作为回退——但 scraper 不实现 `GetProcessList`（直接调 gopsutil），所以 scraper 字段实际只是类型兼容；行为上与 embedded 等价
- 若 embedded 和 scraper 都为 nil（构造时都传 nil），所有方法返回空响应——可用于测试桩
