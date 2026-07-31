# ingester.go

## 1. 概述

`ingester.go` 实现 metric 包的异步批量 ingest —— edge 推上来的 host metric 样本先进 buffer channel，flusher goroutine 按 batch size 或 flush interval 落盘。

设计要点：
- **Push 永不阻塞、永不返回 error**：metrics 是 lossy-OK，tunnel handler 不重试
- **buffer 满时 drop-oldest**：丢最老样本而非新样本（新样本更接近实时，对监控更有价值）
- **retry + dead-letter**：写失败按 100ms/500ms/2s 重试 3 次，仍失败转 dead-letter 表
- **prometheus 自观测**：`ongrid_ingest_writes_total` / `ongrid_ingest_dropped_total` / `ongrid_ingest_flush_failures_total` / `ongrid_ingest_batch_size`

## 2. 包信息

- 包名：`metric`
- 路径：`internal/manager/biz/metric`

## 3. 关键类型与接口

### Ingester

```go
type Ingester struct {
    writer  Writer
    log     *slog.Logger
    metrics *ingestMetrics
    bufCh   chan model.Point       // 容量 = batchSz * 4
    batchSz int
    flushAt time.Duration
    bufMu   sync.Mutex             // 序列化 drop-oldest 路径
}
```

### IngestService 接口

```go
type IngestService interface {
    Push(ctx, edgeID uint64, points []tunnel.HostMetricPoint) error
}
```

tunnel-side handler 消费的窄面。service 包 re-export 同方法集。

### 默认值

```go
const (
    defaultBatchSize    = 500
    defaultFlushAt      = 5 * time.Second
    bufferCapMultiplier = 4
)

var flushBackoffs = []time.Duration{
    100 * time.Millisecond,
    500 * time.Millisecond,
    2 * time.Second,
}
```

### ingestMetrics

```go
type ingestMetrics struct {
    writes     *prometheus.CounterVec   // label: result=success|fail
    flushFails *prometheus.CounterVec   // label: reason
    dropped    *prometheus.CounterVec   // label: reason
    batchSize  prometheus.Histogram
}
```

## 4. 关键函数与流程

### NewIngester

构造 + 注册 prom 指标。`reg == nil` 回退 `prometheus.DefaultRegisterer` + WARN。注册错误（如重复）降级为 WARN 不 crash（让 double-wired 测试不挂进程）。

`bufCh` 容量 = `batchSz * bufferCapMultiplier` = 2000。

### Start（flusher loop）

```go
func (i *Ingester) Start(ctx) error
```

1. `batch := make([]Point, 0, batchSz)` + `ticker := time.NewTicker(flushAt)`
2. select 循环：
   - `bufCh` 来点 → append，满 batchSz 立即 flush + 清空
   - `ticker.C` → 非空 batch flush + 清空
   - `ctx.Done` → 非阻塞 drain bufCh + 用 fresh 5s timeout context 最终 flush + return nil

### Push

```go
func (i *Ingester) Push(_ context.Context, edgeID uint64, points []tunnel.HostMetricPoint) error
```

对每个 point 调 `enqueue(model.FromTunnelPoint(edgeID, p))`。**永不返回 error**（lossy-OK）。

### enqueue

```go
func (i *Ingester) enqueue(p model.Point)
```

1. `select { case bufCh <- p: return; default: }` —— 非阻塞发送，成功即返
2. 失败 → 加 `bufMu` 锁
3. 锁内：`select { case <-bufCh: dropped.Inc("buffer_full"); default: }` 丢最老
4. 再 `select { case bufCh <- p: default: dropped.Inc("buffer_full") }` 重试发送，仍失败丢 p 自己

注释：锁让 drop-oldest + re-send 原子，防两个并发 Push 都试图从空 channel evict。

### flush

```go
func (i *Ingester) flush(ctx, batch []Point)
```

1. `batchSize.Observe(len(batch))`
2. **防御性拷贝**：`payload := make([]Point, len(batch)); copy(payload, batch)` —— 调用方复用 slice
3. retry 循环（attempt 0..3）：
   - attempt > 0 → `time.After(flushBackoffs[attempt-1])` 或 ctx.Done
   - `writer.WriteRaw(ctx, payload)` 成功 → `writes.Inc("success")` + return
   - 失败 → warn log + continue
4. 全部失败 → `writes.Inc("fail")` + `flushFails.Inc(classifyReason(err))` + `WriteDeadLetter(ctx, payload, dlReason)`

### classifyReason

```go
func classifyReason(err) string
```

粗粒度分桶：`context.Canceled` / `context.DeadlineExceeded` → `"ctx_cancel"`；其它 → `"write_error"`。注释明确：保持低基数，避免 err string 当 label（基数爆炸）。

## 5. 依赖关系

### 外部包

- `context` / `log/slog` / `sync` / `time`
- `github.com/prometheus/client_golang/prometheus` —— Counter/Histogram

### 内部包

- `model "github.com/ongridio/ongrid/internal/manager/model/metric"` —— `Point` / `FromTunnelPoint`
- `github.com/ongridio/ongrid/internal/pkg/tunnel` —— `HostMetricPoint`

### 依赖接口（在 repo.go 定义）

- `Writer.WriteRaw` / `WriteDeadLetter`

### 被谁调用

- `cmd/ongrid` 启动 `go ingester.Start(ctx)` 长跑
- tunnel-side handler 调 `Push` 入队

## 6. 并发与资源管理

- **多生产者单消费者**：多个 tunnel goroutine 并发 `Push` → `enqueue`；单个 `Start` goroutine 消费
- **`bufMu` 序列化 drop-oldest**：防两个并发 Push 都从空 channel evict
- **`bufCh` 容量 2000**：batchSz * 4，给 Push 足够缓冲
- **ticker defer Stop**：防 goroutine 泄漏
- **ctx.Done 最终 flush 用 fresh context**：parent 已 done，但 writer 可能仍可用；5s timeout 保证 shutdown 有界
- **防御性 copy**：flush 内 `copy(payload, batch)`，因为调用方（Start loop）复用 `batch` slice

## 7. 设计模式与亮点

### Lossy-OK 契约

`Push` 永不返回 error。tunnel handler 不重试。这是监控数据的合理取舍 —— 丢点不影响整体趋势，重试会反压上游。

### Drop-oldest 而非 drop-newest

buffer 满时丢最老样本。新样本更接近实时，对监控更有价值。`dropped.Inc("buffer_full")` 让运维知道有丢弃。

### 防御性 copy

`flush` 内 `copy(payload, batch)`。注释：调用方复用 slice 后 flush 返回，若不 copy，retry 时 batch 内容已被改。

### Dead-letter 兜底

3 次重试仍失败 → `WriteDeadLetter(ctx, payload, dlReason)`。dlReason 截断到 256 字符防 column 溢出。这保证数据不丢，可后续 replay。

### 低基数 label

`classifyReason` 粗粒度分桶（`ctx_cancel` / `write_error`），注释明确避免 err string 当 label（基数爆炸，违反 gospec "高基数字段禁止作为 Prometheus label"）。

### 注册错误降级

prom 注册失败（如 double-wired 测试）降级为 WARN 不 crash。注释明确意图。

### 优雅 shutdown

`ctx.Done` 后非阻塞 drain bufCh + fresh 5s context 最终 flush。保证 shutdown 时 in-flight 数据尽量落盘，且 shutdown 时间有界。

## 8. 注意事项

- **`Push` 忽略 ctx**：`Push(_ context.Context, ...)` 不检查 ctx。lossy-OK 契约下，enqueue 失败也只丢点不报错
- **`enqueue` 锁内双重 select**：第一次 select 丢最老，第二次 select 重试发送。若都失败，p 自己被丢。锁保证原子性
- **`flush` retry 不恢复 ctx**：ctx.Done 后 `lastErr = ctx.Err()` 直接 break。若 ctx 已取消，后续 retry 无意义
- **`WriteDeadLetter` 失败只 log**：dead-letter 写失败不再重试，只 `log.Error`。数据可能真丢，但已尽力
- **`batchSize` histogram buckets 固定**：`[10, 50, 100, 250, 500]`。若 batch 经常超 500 应调整 buckets
- **`bufCh` 容量 2000 是硬编码**：`batchSz * bufferCapMultiplier`。高负载场景应可配
- **`Start` 单 goroutine 消费**：flush 串行，慢 writer 会拖慢整个 ingest。若 writer 成为瓶颈，考虑多 consumer
- **`flushBackoffs` 是 var 不是 const**：测试可覆盖。生产不应改
