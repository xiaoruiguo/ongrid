# `metrics.go` 技术实现文档

> 源文件：`internal/pkg/llm/metrics.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/llm`

## 1. 概述

本文件封装 `llm` 包的 Prometheus 收集器：token 总量、请求计数、请求延迟直方图。严格遵守红线 — label 仅 `model` / `kind` / `result`，禁含 `user_id` / `org_id` / `session_id`（高基数）。提供 `registerOrExisting` 工具让测试在共享 registry 上重复注册时不 panic。

## 2. 包信息

- **包名**：`llm`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被 `client.go`（`openaiClient`）调用；依赖 `github.com/prometheus/client_golang`

## 3. 关键类型与接口

```go
type metrics struct {
    tokensTotal    *prometheus.CounterVec   // {model, kind=prompt|completion}
    requestsTotal  *prometheus.CounterVec   // {model, result=success|error|budget_exceeded}
    requestSeconds *prometheus.HistogramVec // {model}
}
```

无导出接口；导出函数 `newMetrics`（包内私有但被 `client.go` 调用）。

## 4. 关键函数与流程

### `newMetrics`
- **签名**：`func newMetrics(reg *prometheus.Registry, log *slog.Logger) *metrics`
- **职责**：构造并注册三个 collector；nil registry 回退 DefaultRegisterer
- **流程**：
  1. reg nil → log Warn + 用 `prometheus.DefaultRegisterer`
  2. 构造三个 collector：
     - `ongrid_llm_tokens_total`（CounterVec，labels: model, kind）
     - `ongrid_llm_requests_total`（CounterVec，labels: model, result）
     - `ongrid_llm_request_duration_seconds`（HistogramVec，labels: model；buckets 0.1s..~51s，ExponentialBuckets(0.1, 2, 10)）
  3. 对每个 collector 调 `registerOrExisting` 注册
  4. 返回 `&metrics{...}`
- **错误处理**：`registerOrExisting` 处理 AlreadyRegisteredError；其他注册错误 panic（注释明示"Any other registration failure is a programming error; panic as MustRegister would"）

### `registerOrExisting`
- **签名**：`func registerOrExisting(reg prometheus.Registerer, c prometheus.Collector, log *slog.Logger) prometheus.Collector`
- **职责**：注册 collector；已注册则复用现有
- **流程**：
  1. `reg.Register(c)`；成功返回 c
  2. `errors.As(err, &are)`（AlreadyRegisteredError）→ log Warn + 返回 `are.ExistingCollector`
  3. 其他错误 → panic
- **错误处理**：AlreadyRegistered 复用；其他 panic

## 5. 依赖关系

- **内部包**：无
- **外部库**：`github.com/prometheus/client_golang`
- **被调用方**：`client.go` 的 `New` / `NewWithResolver` 调用 `newMetrics`；`openaiClient.Chat` 在各路径调 `metrics.requestsTotal.WithLabelValues(...).Inc()` 等

## 6. 并发与资源管理

- **Prometheus collector 线程安全**：CounterVec / HistogramVec 内部用原子操作，可并发 Inc/Observe
- **`newMetrics` 注册期**：单次调用，无需锁
- **`registerOrExisting` 复用**：让测试在共享 registry 上多次调用 `newMetrics` 不 panic

## 7. 设计模式与亮点

- **label 基数严格控制**：注释明示"Label cardinality red line: never user_id / org_id / session_id"；仅 model/kind/result，防止 Prometheus 时序爆炸
- **ExponentialBuckets 0.1s..~51s**：覆盖 LLM 请求典型延迟范围（快模型 100ms，reasoning model 50s+）
- **`registerOrExisting` 测试友好**：让测试在共享 registry 上重复注册不 panic，复用已有 collector
- **nil registry 回退**：注释明示"fall back to prometheus.DefaultRegisterer and warn once"；让未配 registry 的 caller 仍能用
- **panic on 非 AlreadyRegistered 错误**：注释明示"We cannot continue with a broken collector"；与 `MustRegister` 同款姿态
- **result label 语义化**：`success` / `error` / `budget_exceeded` 三态，让 dashboard 直接按结果分组

## 8. 注意事项

- **model label 仍是潜在高基数**：若 operator 配置大量自定义 model 名（如 `gpt-5.6-sol`、`kimi-k2.5-custom-alias`），model label 可能膨胀；需 admin 侧约束 model 命名
- **kind 仅 prompt/completion**：未单独记 reasoning token、cached token 等；若未来需细分需扩展
- **result 仅三态**：`budget_exceeded` 与 `error` 不区分错误子类；若需细分（如 timeout / rate_limit）需扩展（注意 `router.go` 的 `llmStatusFor` 已有更细分类，但那是 `prom.ObserveLLMCall` 路径）
- **buckets 0.1..51s**：超过 51s 的请求落入最后 bucket；reasoning model 可能超 51s，需评估调高
- **nil log 不 Warn**：`newMetrics` 中 log nil 检查；caller 应传非 nil log
- **`registerOrExisting` panic 风险**：非 AlreadyRegistered 错误 panic；若 registry 状态异常可能崩进程；生产需确保 registry 健康
- **`ongrid_llm_*` 命名前缀**：与项目其他 metrics 命名约定一致；重命名需同步 dashboard
