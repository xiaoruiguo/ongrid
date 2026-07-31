# graph/callbacks/metrics.go

## 1. 概述

本文件实现 `MetricsHandler`——一个 eino callback handler，从 graph 视角观测工具调用、ChatModel 调用、graph 终态，输出 Prometheus 指标。

**双观察契约**：PR-3 的工具 decorator 链已经在 per-call seam 记录 `ongrid_tool_invocations_total`，本 handler 复用**同一个 collector**（通过 registry-keyed cache 避免双注册），counter 自增是 best-effort 重复——这是允许的："callback 视角 + 装饰器视角 双观察 OK"。

## 2. 包信息

- **包名**：`callbacks`
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/biz/aiops/graph/callbacks`
- **角色**：eino callback handler；Prometheus 指标观测
- **依赖**：
  - 标准库 `context`、`errors`、`strings`、`sync`、`sync/atomic`、`time`
  - `github.com/cloudwego/eino/callbacks`、`components`、`schema`
  - `github.com/prometheus/client_golang/prometheus`

## 3. 关键类型与接口

### `MetricsDeps`

```go
type MetricsDeps struct {
    Registerer prometheus.Registerer
}
```

生产传 `prometheus.DefaultRegisterer`，测试传 `prometheus.NewRegistry()` 避免冲突。

### `metricsCollectors`（未导出）

```go
type metricsCollectors struct {
    toolInvocations *prometheus.CounterVec   // labels: name, result
    toolDuration    *prometheus.HistogramVec // labels: name
    graphIterations *prometheus.CounterVec   // labels: result
    chatTurns       *prometheus.CounterVec   // labels: result
}
```

**基数红线**：labels 限定为 `{name, result}` / `{provider, result}` / `{result}`——绝不包含 user/tenant/session。

### `MetricsHandler`

```go
type MetricsHandler struct {
    collectors    *metricsCollectors
    chatTurns     atomic.Int64
    toolStartsMu  sync.Mutex
    toolStarts    map[string]time.Time
    terminated    atomic.Bool
}
```

- `chatTurns`：本 run ChatModel 调用次数原子计数
- `toolStarts`：mutex 保护的 map，记每个 tool_call 的开始时间
- `terminated`：graph 终态 CAS 标记，确保 `graphIterations` 只 +1 一次

## 4. 关键函数与流程

### `getMetricsCollectors(reg) *metricsCollectors`

registry-keyed cache，避免多次 graph run 重复注册触发 `AlreadyRegisteredError`：

```go
metricsRegMu.Lock()
defer metricsRegMu.Unlock()
if mc, ok := metricsRegMap[reg]; ok {
    return mc
}
// 创建 4 个 collector，全部走 regOrExist
mc := &metricsCollectors{
    toolInvocations: regOrExist(reg, tinv).(*prometheus.CounterVec),
    toolDuration:    regOrExist(reg, tdur).(*prometheus.HistogramVec),
    graphIterations: regOrExist(reg, gi).(*prometheus.CounterVec),
    chatTurns:       regOrExist(reg, ct).(*prometheus.CounterVec),
}
metricsRegMap[reg] = mc
return mc
```

**Help string MUST match `tools/decorators/metric.go` byte-for-byte**——Prom 在 AlreadyRegistered 时若 descriptor Help 不同会 panic。两层观察同一 counter，必须共享 desc。

### Histogram buckets

```go
prometheus.ExponentialBuckets(0.05, 2, 10)
// → [0.05, 0.1, 0.2, 0.4, 0.8, 1.6, 3.2, 6.4, 12.8, 25.6]
```

工具调用耗时指数桶，50ms 起步，覆盖 25.6s 内的调用（ToolTimeout 默认 15s）。

### `Needed(ctx, info, timing) bool`

| Component | Timing |
|---|---|
| ChatModel | OnEnd, OnError |
| Tool | OnStart, OnEnd, OnError |
| Graph/其他 | OnEnd, OnError |

→ ChatModel 不需要 OnStart（不需要记开始时间，只数次数）；Tool 需要 OnStart 记时间戳用于 duration。

### `OnStart(ctx, info, _)`

仅 Tool component 工作：

```go
if info.Component == components.ComponentOfTool {
    h.toolStartsMu.Lock()
    h.toolStarts[toolCallIDFromCtx(ctx, info)] = time.Now()
    h.toolStartsMu.Unlock()
}
```

### `OnEnd(ctx, info, output)`

按 component 分组：

**ChatModel**：
```go
h.chatTurns.Add(1)
h.collectors.chatTurns.WithLabelValues("success").Inc()
```

**Tool**：
```go
key := toolCallIDFromCtx(ctx, info)
h.toolStartsMu.Lock()
started, ok := h.toolStarts[key]
delete(h.toolStarts, key)
h.toolStartsMu.Unlock()
if ok {
    h.collectors.toolDuration.WithLabelValues(info.Name).Observe(time.Since(started).Seconds())
}
h.collectors.toolInvocations.WithLabelValues(info.Name, "success").Inc()
```

注释明确："We can't tell from CallbackOutput alone whether the tool returned an error payload (it's just a JSON string). Treat this hook as 'success' — the OnError path covers actual failures returned via tool.InvokableRun."

→ `OnEnd` 视作 success，`OnError` 才算 failure。

**Graph/其他**：
```go
if h.terminated.CompareAndSwap(false, true) {
    h.collectors.graphIterations.WithLabelValues("success").Inc()
}
```

CAS 保证 graphIterations 只 +1 一次（OnEnd/OnError 都可能触发，避免双计）。

### `OnError(ctx, info, err)`

**ChatModel**：`chatTurns.WithLabelValues("error").Inc()`

**Tool**：
```go
result := "error"
if isDeadlineErr(err) {
    result = "timeout"
}
// 同样 Observe duration
h.collectors.toolInvocations.WithLabelValues(info.Name, result).Inc()
```

**Graph/其他**：
```go
result := "error"
if isMaxIterations(err) {
    result = "max_iterations"
}
if h.terminated.CompareAndSwap(false, true) {
    h.collectors.graphIterations.WithLabelValues(result).Inc()
}
```

### `isDeadlineErr(err) bool`

```go
return errors.Is(err, context.DeadlineExceeded)
```

### `isMaxIterations(err) bool`

eino 不导出类型化错误，字符串匹配：

```go
msg := err.Error()
return strings.Contains(msg, "max steps") || strings.Contains(msg, "MaxRunSteps") || strings.Contains(msg, "max iterations")
```

注释明确："The match is loose on purpose — the metric's `max_iterations` bucket is a hint, not authoritative."

### `ChatTurns() int`

测试用导出方法，返回 `chatTurns.Load()`。

## 5. 依赖关系

### 上游
- `callbacks/chain.go::NewDefaultHandlers` 装配本 handler
- `toolCallIDFromCtx` 来自包内其他文件（`persistence.go` 中定义）

### 下游
- `prometheus.Registerer`/`CounterVec`/`HistogramVec`
- 与 `tools/decorators/metric.go` 共享 collector（通过 registry-keyed cache + byte-for-byte Help string）

## 6. 并发与资源管理

### `chatTurns atomic.Int64`

ChatModel 调用次数原子计数，无锁。

### `toolStartsMu sync.Mutex` 保护 map

```go
h.toolStartsMu.Lock()
h.toolStarts[key] = time.Now()
h.toolStartsMu.Unlock()
```

锁范围最小化——仅 map 操作。OnEnd 同样模式：lock → 取出 → delete → unlock，然后无锁 Observe。

### `terminated atomic.Bool` CAS

```go
if h.terminated.CompareAndSwap(false, true) {
    h.collectors.graphIterations.WithLabelValues(result).Inc()
}
```

CAS 保证只 +1 一次，无需锁。

### registry-keyed cache

`metricsRegMu sync.Mutex` 保护 `metricsRegMap`，避免并发创建同一 Registerer 的 collectors。每个 Registerer 只创建一次 collectors，后续 handler 实例共享。

## 7. 设计模式与亮点

### 双观察契约

注释三次解释：
- "the PR-3 decorator chain already records ongrid_tool_invocations_total at the per-call seam"
- "this handler re-uses the SAME collector via the registry-keyed cache so we never double-register"
- "the counter increments are best-effort duplicates (acceptable: 'callback 视角 + 装饰器视角 双观察 OK')"

→ decorator 视角和 callback 视角都对同一 counter 自增，结果是双倍计数。这是有意为之——两层观测互为冗余，单层失效不影响指标可用性。但**测试断言时需要注意**：当 decorator 链被绕过（直接调 handler）时只有一次自增。

### registry-keyed cache 防双注册

```go
metricsRegMap = map[prometheus.Registerer]*metricsCollectors{}
```

不同 Registerer 各自独立创建 collectors；同一 Registerer 复用。这是支持测试隔离（每测试用 `NewRegistry()`）+ 生产复用（共享 `DefaultRegisterer`）的关键设计。

### Help string byte-for-byte 一致

```go
prometheus.CounterOpts{
    Name: "ongrid_tool_invocations_total",
    Help: "Total ongrid tool invocations, split by name and result.",
}
```

注释强调："Help string MUST match tools/decorators/metric.go byte-for-byte — Prom panics on AlreadyRegistered if descriptors differ on Help."

→ 若 decorator 那边改了 Help，本处必须同步。建议未来抽取为共享常量。

### `terminated CAS` 防双计

graph 终态可能由 OnEnd 或 OnError 任一触发，CAS 保证 `graphIterations` 只 +1 一次。这是原子状态机的经典模式。

### `isMaxIterations` 字符串匹配的妥协

eino 不导出类型化 max-steps 错误，本文件用 `strings.Contains` 匹配三种可能的消息片段。注释明确这是 hint not authoritative——容忍 eino 改消息文本导致误分类。

### 基数红线

注释明确："Cardinality red line: labels are limited to {name, result} (tools) / {provider, result} (chat models) / {result} (graph terminations). Never include user / tenant / session."

→ 严格遵守"高基数字段禁止作为 Prometheus label"的红线。`user_id`/`session_id`/`tenant_id` 都不进 label。

## 8. 注意事项

### `toolCallIDFromCtx` 跨文件依赖

本文件调用 `toolCallIDFromCtx(ctx, info)` 但未定义——在 `persistence.go` 中定义。修改时注意跨文件依赖。

### `isDeadlineErr` 在本文件定义

`isDeadlineErr` 在本文件定义（不在 persistence.go），但被 audit.go、sse.go 等同包其他文件调用。本文件是包内共享工具函数的"主家"。

### Tool OnEnd 视作 success 的局限

```go
// We can't tell from CallbackOutput alone whether the tool
// returned an error payload (it's just a JSON string). Treat
// this hook as "success" — the OnError path covers actual
// failures returned via tool.InvokableRun.
```

→ 工具返回的 JSON 内含 `"error": "..."` 字段时，本 handler 仍记 `success`。只有 Go-level error（`tool_adapter.go` 把它转成 JSON envelope，不返回 error）才会触发 OnError。这是当前实现的盲区——工具业务错误不会进 `error` label。

### `toolStarts` map 孤儿 entry

若 Tool OnStart 成功但 OnEnd/OnError 未调用（eino bug 或异常），`toolStarts` 留下孤儿 entry。当前实现可接受（handler per-run 创建，GC 自动回收），但长期运行服务可能积累。

### Streaming 路径 drain+close

`OnStartWithStreamInput`/`OnEndWithStreamOutput` 仅 drain+close stream，不观测指标。streaming 模式下的 ChatModel 调用次数、tool duration 不会进 metrics。这是待补完的缺口。

### `chatTurns` 是 per-handler 实例

每个 `MetricsHandler` 实例独立计数 `chatTurns`。由于 handler per-request 创建，每请求计数从 0 开始。`ChatTurns()` 方法返回的是当前请求的 ChatModel 调用次数，**不是**全局累计。

### Histogram buckets 固定

`ExponentialBuckets(0.05, 2, 10)` 是硬编码。若 ToolTimeout 调整到 30s 以上，25.6s 上限的桶会丢失长尾数据。修改时需要同步 buckets。

### 与 decorator 视角双计数的运维影响

`ongrid_tool_invocations_total` 会被 decorator 和 callback 各自 +1，实际值是真实调用次数的 2 倍。运维 dashboards / alerts 需要除以 2，或区分 `source="decorator"` / `source="callback"` label（当前未区分）。
