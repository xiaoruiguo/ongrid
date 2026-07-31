# `metric.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/decorators/metric.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools/decorators`

## 1. 概述

本文件实现 metric 装饰器：每次 `InvokableRun` 增 Prometheus 计数器 `ongrid_tool_invocations_total{name, result}` 并观察 `ongrid_tool_duration_seconds{name}` 直方图。红线：Prom label 严禁包含 user_id/tenant/device_id（高基数）；collectors 按 Registerer lazy 复用避免 "already registered" panic。

## 2. 包信息

- **包名**：`decorators`
- **所属模块**：`internal/manager/biz/aiops/tools/decorators`
- **依赖方向**：被 `chain.go` 的 `Wrap` 调用；依赖 `basetool`、`prometheus`

## 3. 关键类型与接口

```go
type metricCollectors struct {
    invocations *prometheus.CounterVec   // labels: name, result
    duration    *prometheus.HistogramVec // labels: name
}

var (
    metricRegMu  sync.Mutex
    metricRegMap = map[prometheus.Registerer]*metricCollectors{}
)

type MetricTool struct {
    inner      basetool.BaseTool
    collectors *metricCollectors
}
```

Sentinel：metric 没有 sentinel error；`regOrExist` 在非 AlreadyRegistered 错误时 panic。

## 4. 关键函数与流程

### `getMetricCollectors`
- **签名**：`func getMetricCollectors(reg prometheus.Registerer) *metricCollectors`
- **职责**：按 Registerer lazy 创建并注册 collectors
- **流程**：
  1. reg nil → DefaultRegisterer
  2. 加锁查 metricRegMap；命中返回 existing
  3. 否则 `NewCounterVec`（labels name, result）+ `NewHistogramVec`（labels name，Buckets Exponential 0.05..~25s）
  4. `regOrExist` 注册：成功返回新 collector；AlreadyRegisteredError 返回 ExistingCollector；其他错误 panic
  5. 存入 map 并返回
- **错误处理**：非 AlreadyRegistered 错误 panic（注册系统级失败）

### `regOrExist`
- **签名**：`func regOrExist(reg prometheus.Registerer, c prometheus.Collector) prometheus.Collector`
- **职责**：注册或返回已存在的 collector
- **流程**：`reg.Register(c)`；err nil → 返回 c；`errors.As(err, &are)` → 返回 `are.ExistingCollector`；其他 panic
- **说明**：本地的副本，避免与 llm/metrics.go 产生 import 循环

### `WithMetric`
- **签名**：`func WithMetric(inner basetool.BaseTool, reg prometheus.Registerer) basetool.BaseTool`
- **职责**：包装 inner emit Prom counter + histogram
- **流程**：返回 `&MetricTool{inner, getMetricCollectors(reg)}`

### `MetricTool.Info`
- **签名**：`func (m *MetricTool) Info(ctx) (*basetool.ToolInfo, error)`
- **职责**：透传 inner.Info

### `MetricTool.InvokableRun`
- **签名**：`func (m *MetricTool) InvokableRun(ctx, argsJSON string, opts ...basetool.InvokeOption) (string, error)`
- **职责**：跑 inner 后增计数器、观察 duration
- **流程**：
  1. `inner.Info(ctx)` 取 name（err 忽略，name 留空）
  2. `start := time.Now()`
  3. `inner.InvokableRun(ctx, argsJSON, opts...)`
  4. `dur := time.Since(start)`
  5. result = err != nil ? "error" : "success"
  6. `collectors.invocations.WithLabelValues(name, result).Inc()`
  7. `collectors.duration.WithLabelValues(name).Observe(dur.Seconds())`
  8. 返回 inner 的 out + err

## 5. 依赖关系

- **内部包**：`basetool`
- **外部库**：`github.com/prometheus/client_golang`、标准库 `errors`、`sync`、`time`
- **被调用方**：`chain.go` 的 `Wrap`

## 6. 并发与资源管理

- `metricRegMu`（Mutex）：保护 `metricRegMap` 全局缓存
- collectors 通过 prometheus 自身线程安全（CounterVec/HistogramVec 内部加锁）
- `MetricTool` 结构 immutable，多 goroutine 共享安全

## 7. 设计模式与亮点

- **Registerer 维度 collector 复用**：同一 Registerer 的多次 Wrap 共享 collectors，避免 "already registered" panic（多 chain 同 registry 测试场景关键）
- **Cardinality 红线**：labels 仅 `{name, result}`，禁 user_id/tenant/device_id（注释明示）
- **Histogram buckets**：`ExponentialBuckets(0.05, 2, 10)` 覆盖 50ms..~25s，匹配 tool 调用典型延迟范围
- **与 llm/metrics.go 同模式**：`regOrExist` 镜像 `registerOrExisting`，本地副本避免 import 循环
- **result 二值**：success/error；timeout 由 audit 装饰器写入 chat_tool_calls.status，metric 不区分

## 8. 注意事项

- **name 可能为空**：inner.Info 出错时 name 留空字符串，会进 label 值；生产工具应总返回有效 Info
- **panic on register error**：非 AlreadyRegistered 的注册错误视为系统级故障，panic 而非吞错
- **buckets 适用范围**：50ms..~25s 覆盖大多数 tool；超长 tool（如 60s+ review）会落到 +Inf bucket
- **metric 不计装饰器开销**：作为最内层装饰器，histogram 只测 inner 真实延迟
