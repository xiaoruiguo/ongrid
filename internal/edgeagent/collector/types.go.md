# `types.go` 技术实现文档

> 源文件：`internal/edgeagent/collector/types.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/collector`

## 1. 概述

本文件定义 `collector` 包的核心契约：`CollectorSource` 类型别名 + `SourceEmbedded` / `SourceScrapePrefix` 常量、`CollectorOutput` 结构、`Collector` 接口。是 embedded / scrape / composite / noop-push 四种实现共同遵守的接口。

## 2. 包信息

- **包名**：`collector`
- **所属模块**：edgeagent metric 采集层（类型中心）
- **依赖方向**：被同包所有实现文件引用；被 `biz.Agent` 通过 `Collector` 接口消费

## 3. 关键类型与接口

```go
// CollectorSource 是 push_prom_samples 携带的 Source 字符串类型别名
type CollectorSource = string

const (
    SourceEmbedded     CollectorSource = "embedded"     // gopsutil 进程内采集
    SourceScrapePrefix = "scrape:"                     // + target name 拼接
)

// CollectorOutput 单次采集结果（一个源一个 tick）
// HostPoint 是 legacy 8 字段快路径（push_host_metrics）
// Samples 是开放集 rich 路径（push_prom_samples）
type CollectorOutput struct {
    Source         CollectorSource
    HostPoint      tunnel.HostMetricPoint
    HostPointValid bool           // false 时 agent 不发 push_host_metrics
    Samples        []tunnel.PromSample
}

// Collector 是 metric 源契约
// 多目标源（scrape）返回多元素 slice；单源（embedded）返回单元素
type Collector interface {
    CollectAll(ctx context.Context) ([]CollectorOutput, error)
    HostInfo(ctx context.Context) (tunnel.HostInfo, error)
    GetHostLoad(ctx context.Context) (tunnel.GetHostLoadResponse, error)
    GetProcessList(ctx context.Context, topN int, sortBy string) (tunnel.GetProcessListResponse, error)
}
```

## 4. 关键函数与流程

无函数定义。仅类型声明。

## 5. 依赖关系

- **内部包**：`internal/pkg/tunnel`（用其 HostMetricPoint / HostInfo / GetHostLoadResponse / GetProcessListResponse / PromSample 类型）
- **外部库**：标准库 `context`
- **被调用方**：同包 `embedded.go` / `scrape.go` / `composite.go` / `noop_push.go`；`biz.Agent` 通过接口消费

## 6. 并发与资源管理

无并发控制。类型本身是数据结构 / 接口契约，无行为。

## 7. 设计模式与亮点

- **接口在消费方定义**：`Collector` 接口在 `collector` 包定义（同包实现满足），但语义上是 `biz.Agent` 的契约——Go 惯例「接口在消费方定义」此处折衷，因为多个实现同包共享
- **双路径输出**：`CollectorOutput` 同时携带 HostPoint（快路径）和 Samples（rich 路径）——让 agent 一次 tick 推两路 RPC，平滑迁移 legacy dashboard
- **HostPointValid 标志位**：区分「快路径有效但全零」（如 idle 主机）和「快路径不适用」（如 component-role scrape 目标）——agent 仅在 valid 时发 push_host_metrics
- **CollectAll 返回 slice**：支持多目标源（scrape 一次返回多个 target 的 output）；单源源（embedded）返回单元素 slice
- **空 slice 合法**：注释明确「scrape warming up」可返回空 slice，agent 跳过 push——让 scraper 启动期不必伪造数据
- **CollectorSource 类型别名**：`type CollectorSource = string` 而非 `type CollectorSource string`——是类型别名而非新类型，让 `Source` 字段可直接赋值 string；代价是失去类型安全，但简化了调用点

## 8. 注意事项

- `CollectorSource = string` 是别名而非新类型——`Source` 字段可接收任意 string；调用方需自律用常量
- `CollectorOutput.Samples` 可能是 nil（如 host-role target 无 samples）——agent 侧 `len(out.Samples)==0` 跳过 push_prom_samples
- `Collector` 接口的 4 个方法都必须实现——noop 源（如测试桩）需返回合理零值
- `CollectAll` 返回 `error` 但 agent 侧仅 Warn 不中断——实现可选择永远返回 nil error（如 composite）
- `GetProcessList` 的 `topN` / `sortBy` 参数语义由实现定义——embedded 用 topN 截断 + CPU/Mem 排序；其他实现可能不同
- 新增 Collector 实现需保证 `HostInfo` 返回一致的主机身份（fingerprint / hardwareFingerprint / hostname），否则 cloud 侧会创建新 device row
- `Source` 字符串是 cloud 侧 Prom series 的标签值——修改会影响历史查询；应保持稳定
