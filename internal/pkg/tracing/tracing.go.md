# `tracing.go` 技术实现文档

> 源文件：`internal/pkg/tracing/tracing.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/tracing`

## 1. 概述

本文件把 OpenTelemetry 接入 manager + edge agent，让 Tempo 的 spanmetrics generator 有数据可派生 `traces_spanmetrics_latency_bucket` / `traces_spanmetrics_calls_total`。没有 OTel exporter，Tempo receiver 收不到任何 span，trace_latency / trace_error_rate evaluator 会永远查空矩阵。提供 `Init(ctx, Config) (Shutdown, error)` 在进程启动时构造全局 TracerProvider。

## 2. 包信息

- **包名**：`tracing`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被 `cmd/ongrid/main.go` 与 edge agent 启动代码调用；依赖 OpenTelemetry SDK 系列

## 3. 关键类型与接口

```go
type Config struct {
    ServiceName    string  // resource.service.name；spanmetrics generator 按此分 series
    Endpoint       string  // OTLP HTTP collector，host:port；空则禁用导出
    Insecure       bool    // true=http，false=https（生产 TLS 代理后可关）
    SamplingRatio  float64 // 0..1，根 span 采样比例；默认 1.0
}

type Shutdown func(context.Context) error
```

## 4. 关键函数与流程

### `Init`
- **签名**：`func Init(ctx context.Context, cfg Config) (Shutdown, error)`
- **职责**：构造并注册全局 TracerProvider；endpoint 空时返回 no-op shutdown 让 caller 可无条件 defer
- **流程**：
  1. endpoint trim 后空 → 返回 `func(context.Context) error { return nil }`
  2. SamplingRatio <=0 或 >1 → 重置为 1.0
  3. 构造 OTLP HTTP exporter options：`WithEndpoint(cfg.Endpoint)`；`Insecure=true` 时加 `WithInsecure()`
  4. `otlptracehttp.New(ctx, opts...)` 创建 exporter
  5. `resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName(cfg.ServiceName))` — 显式跳过 `resource.Default()` 合并以避免不同 otel 模块版本的 schema-URL 冲突
  6. `sdktrace.NewTracerProvider`：
     - `WithBatcher(exporter, WithBatchTimeout(2*time.Second))` — 快刷批次，事件 span 不会憋 1 分钟才进 Tempo
     - `WithResource(res)`
     - `WithSampler(ParentBased(TraceIDRatioBased(cfg.SamplingRatio)))` — 基于父 span 决策，根 span 按 ratio 采样
  7. `otel.SetTracerProvider(tp)`
  8. `otel.SetTextMapPropagator(CompositeTextMapPropagator(TraceContext, Baggage))` — W3C tracecontext + baggage
  9. 返回 `tp.Shutdown` 作为 Shutdown 函数
- **错误处理**：
  - exporter 构造失败：`tracing: build exporter: %w`
  - 注释提到 `resource.NewWithAttributes` 不会 error，`_ = err` 是 legacy alias 占位

## 5. 依赖关系

- **内部包**：无
- **外部库**：
  - `go.opentelemetry.io/otel`
  - `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp`
  - `go.opentelemetry.io/otel/propagation`
  - `go.opentelemetry.io/otel/sdk/resource`
  - `go.opentelemetry.io/otel/sdk/trace`
  - `go.opentelemetry.io/otel/semconv/v1.27.0`
- **被调用方**：`cmd/ongrid/main.go`（manager 启动）、edge agent 启动代码

## 6. 并发与资源管理

- **全局注册**：`otel.SetTracerProvider` / `otel.SetTextMapPropagator` 是全局状态，应在进程启动时只调一次
- **Batcher 异步导出**：SDK 内部 goroutine 周期性 flush（2s）；`Shutdown` 同步排空剩余 span
- **`context.Context` 透传**：exporter 与 sampler 都尊重 ctx
- **`ParentBased` 采样器**：子 span 跟随父决策，避免链路中断

## 7. 设计模式与亮点

- **no-op shutdown 模式**：endpoint 空时返回空 func，让 caller 可无条件 `defer shutdown(context.Background())` 而无需 nil 检查
- **2s batch timeout**：注释解释"Fast batch flush so an incident's spans aren't held for a minute before showing up in Tempo" — 故意牺牲少量批次效率换事故可观测性
- **跳过 `resource.Default()`**：避免不同 otel 模块版本的 schema-URL 冲突；只用 load-bearing 的 ServiceName，其他属性（host、pid）nice-to-have
- **`ParentBased(TraceIDRatioBased)`**：根 span 按 ratio 采样，子 span 跟随，保证采到的 trace 完整
- **CompositeTextMapPropagator(TraceContext, Baggage)**：W3C 标准传播，跨服务透传 trace context 与 baggage

## 8. 注意事项

- **全局状态**：`otel.Set*` 是全局的，多次调用 `Init` 会覆盖；测试需自行隔离
- **SamplingRatio 默认 1.0**：当前规模可接受；span 量增大时调到 0.1
- **`Insecure=true` 仅适合内网**：Docker 网络内 Tempo 用 http；生产 TLS 代理后必须 `Insecure=false`
- **`Shutdown` 必须调用**：进程退出前调用以排空 span buffer，否则最后 2s 的 span 丢失
- **ServiceName 是 spanmetrics 分 series 的关键**：manager 与 edge 必须用不同 ServiceName，否则 series 混在一起
- **`_ = err` 注释**：`resource.NewWithAttributes` 实际不会 error，但保留 legacy alias；若未来 SDK 改签名需重新评估
- **未暴露 sampler 自定义**：仅 `TraceIDRatioBased`；若需基于属性采样需扩展 Config
