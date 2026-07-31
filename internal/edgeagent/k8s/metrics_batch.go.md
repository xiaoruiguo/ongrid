# `metrics_batch.go` 技术实现文档

> 源文件：`internal/edgeagent/k8s/metrics_batch.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/k8s`

## 1. 概述

本文件实现 metrics 样本的分批器 `metricsBatcher`：在抓取过程中累积样本，按"最大样本数"与"最大字节数"双约束自动切分批次，通过回调推送每个批次，并统计成功/失败/拒绝计数。它是 `metrics.go` 与 `remote_write_scraper.go` 共用的批次管理组件。

## 2. 包信息

- **包名**：`k8s`
- **所属模块**：`internal/edgeagent/k8s`（edgeagent 域的 Kubernetes 适配层）
- **依赖方向**：被 `metrics.go` 的 `scrapeTargetAndPush` 与 `remote_write_scraper.go` 的 `scrapeTargetAndWrite` 调用；依赖 `internal/pkg/tunnel` 的 `PromSample` 与 `PushPromSamplesRequest` 类型。

## 3. 关键类型与接口

```go
// metricsBatchStats 汇总一次抓取的批次推送统计
type metricsBatchStats struct {
    SuccessfulBatches int
    FailedBatches     int
    SuccessfulSamples int
    FailedSamples     int
    RejectedSamples   int
    FirstError        error
}

// metricsBatcher 是批次管理器
type metricsBatcher struct {
    maxSamples       int               // 单批最大样本数
    maxBytes         int               // 单批最大字节数
    baseEncodedBytes int               // 空 request 的编码字节数（header 开销）
    encodedBytes     int               // 当前批次已编码字节数
    samples          []tunnel.PromSample
    push             func([]tunnel.PromSample) error  // 推送回调
    stats            metricsBatchStats
}
```

## 4. 关键函数与流程

### `newMetricsBatcher`
- **签名**：`func newMetricsBatcher(edgeID uint64, source string, maxSamples, maxBytes int, push func([]tunnel.PromSample) error) (*metricsBatcher, error)`
- **职责**：构造 batcher。
- **流程**：
  1. 校验 `maxSamples>0`、`maxBytes>0`、`push!=nil`。
  2. marshal 一个空 `PushPromSamplesRequest{EdgeID, Source, Samples:[]}` 计算 `baseBytes`（header 开销，含空数组 `[]` 的两个字符）。
  3. 校验 `baseBytes < maxBytes`（否则任何样本都超限）。
  4. 预分配 samples 切片 `make([]PromSample, 0, min(maxSamples, 1024))`，避免频繁扩容。
- **错误处理**：参数非法返回明确错误。

### `Add`
- **签名**：`func (b *metricsBatcher) Add(samples ...tunnel.PromSample)`
- **职责**：追加样本，按需自动 Flush。
- **流程**（逐个 sample 处理）：
  1. `json.Marshal(sample)` 计算编码字节数。
  2. marshal 失败 → `rejectSample`（计数 +1，记 FirstError），跳过。
  3. 计算分隔符：当前批次已有样本则 `separatorBytes=1`（JSON 数组逗号）。
  4. 预估 `nextBytes = encodedBytes + separatorBytes + len(encoded)`。
  5. 若 `len(samples) >= maxSamples` 或 `nextBytes > maxBytes` → `Flush()`，重置 separatorBytes=0，重算 nextBytes。
  6. 若单样本就超 `maxBytes` → `rejectSample`（说明该样本本身过大）。
  7. 追加到 `b.samples`，更新 `encodedBytes`。
- **设计**：先 marshal 再判断，保证字节预估精确（不依赖样本字段数估算）。

### `Flush`
- **签名**：`func (b *metricsBatcher) Flush()`
- **职责**：推送当前批次并重置。
- **流程**：
  1. 空批次直接返回。
  2. `batch := append([]PromSample(nil), b.samples...)` 拷贝（避免回调持有引用期间被修改）。
  3. 调用 `b.push(batch)`：
     - 成功：`SuccessfulBatches++`、`SuccessfulSamples += count`。
     - 失败：`FailedBatches++`、`FailedSamples += count`、记 FirstError。
  4. `clear(b.samples)` 清理元素（GC 友好），`b.samples = b.samples[:0]` 重置长度。
  5. `encodedBytes = baseEncodedBytes` 重置字节计数。

### `Stats`
- **签名**：`func (b *metricsBatcher) Stats() metricsBatchStats`
- **职责**：返回当前累计统计（值拷贝）。

### `rejectSample`
- **签名**：`func (b *metricsBatcher) rejectSample(err error)`
- **职责**：记录被拒绝的样本。`RejectedSamples++`，记 FirstError。

## 5. 依赖关系

- **内部包**：`github.com/ongridio/ongrid/internal/pkg/tunnel`（`PromSample`、`PushPromSamplesRequest`）。
- **外部库**：标准库 `encoding/json`、`fmt`。
- **被调用方**：
  - `metrics.go` 的 `scrapeTargetAndPush`：push 回调为 `p.pushWithTimeout`（经 tunnel 推送）。
  - `remote_write_scraper.go` 的 `scrapeTargetAndWrite`：push 回调为 `s.writeWithRetry`（经 remote_write 推送）。

## 6. 并发与资源管理

- **无并发控制**：`metricsBatcher` 设计为单 goroutine 使用（scrape 回调在单 goroutine 内调用 Add/Flush）。字段无锁保护，多 goroutine 并发使用需调用方自行串行化。
- **样本切片拷贝**：`Flush` 时拷贝 samples 切片再传给回调，避免回调异步持有期间被修改。
- **`clear(b.samples)`**：Go 1.21+ 的 builtin，把切片元素置零，帮助 GC 释放大样本内存。

## 7. 设计模式与亮点

- **双约束切分**：同时考虑样本数与字节数，先到先触发 Flush，保证批次既不超 count 也不超 byte。
- **精确字节预估**：每个样本先 marshal 取真实字节数，而非按字段数估算，保证 byte 约束精确。
- **header 开销预计算**：构造时 marshal 空 request 得到 `baseEncodedBytes`，后续每批都从该基数开始计数，包含 JSON 数组括号等固定开销。
- **逗号 accounting**：`separatorBytes` 精确计算 JSON 数组的逗号分隔符，体现对编码细节的严谨。
- **单样本超限拒绝**：Flush 后若单样本仍超 maxBytes，拒绝该样本而非无限 Flush，避免死循环。
- **统计内嵌**：`metricsBatchStats` 内嵌在 batcher 中，无需额外收集，Flush 后即可 `Stats()` 取累计值。
- **拷贝隔离**：Flush 拷贝切片，使回调可以安全持有或异步处理 batch，不与后续 Add 竞争。

## 8. 注意事项

- **每样本双重 marshal**：`Add` 中 marshal 一次用于计数，`push` 回调最终 marshal 整个 request 时会再次 marshal 该样本。大样本量下 CPU 开销显著，但保证了字节约束的精确性。可考虑缓存 marshal 结果优化，但当前实现优先正确性。
- **`baseEncodedBytes` 的 `Samples: []` 开销**：空 request 编码含 `[]` 两字符，实际填充样本时这两个字符被复用（数组括号），因此 `separatorBytes` 的 +1 是样本间逗号，与括号不冲突。注释已说明这一设计。
- **`maxBytes` 必须大于 `baseEncodedBytes`**：若配置的 maxBytes 小于 header 开销，构造直接失败。调用方需确保 maxBytes 足够大（默认 4MiB 远大于 header）。
- **`rejectSample` 不影响后续**：被拒绝的样本不进入批次，但 FirstError 被记录；调用方通过 `Stats().FirstError` 与 `RejectedSamples` 感知拒绝。
- **`Flush` 的失败处理**：批次推送失败后，`FailedBatches/Samples` 计数但不重试。重试策略由回调实现决定（如 `remote_write_scraper.go` 的 `writeWithRetry` 自带重试，而 `metrics.go` 的 `pushWithTimeout` 不重试）。
- **非并发安全**：若未来需要在并发 scrape 中共享 batcher，需加锁或改用 channel 串行化。
