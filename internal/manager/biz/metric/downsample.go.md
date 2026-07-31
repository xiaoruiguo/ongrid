# downsample.go

## 1. 概述

`downsample.go` 实现 metric 包的下采样器 —— 把 raw 样本按 5 分钟桶聚合，再把 5m 桶按 1 小时桶聚合。`Loop` 在单 goroutine 里跑两个 cadence，先对齐到下一个 wall-clock 边界避免与正在填充的桶竞争。

聚合规则：gauge 类（CPU/mem/load/disk）取 avg + max；counter 类（net rx/tx）取 sum。1h 聚合对 5m 的 avg 字段做"等权重 avg"（不按样本数加权，注释说明这是 Prometheus downsampling 的同款取舍）。

## 2. 包信息

- 包名：`metric`
- 路径：`internal/manager/biz/metric`

## 3. 关键类型与接口

### Downsampler

```go
type Downsampler struct {
    writer Writer
    reader Reader
    log    *slog.Logger
}
```

### Cadence 常量

```go
const (
    interval5m = 5 * time.Minute
    interval1h = time.Hour
)
```

包私有，防止测试 race against wall-clock scheduling。

## 4. 关键函数与流程

### Run5m

```go
func (d *Downsampler) Run5m(ctx, bucketEnd time.Time) error
```

1. `bucketEnd.Truncate(5m).UTC()` 对齐
2. `bucketStart = bucketEnd - 5m`
3. `reader.ScanRawForDownsample(ctx, bucketStart, bucketEnd - 1ns)` —— 注释：`[from, to]` 闭区间，传 `to-1ns` 防下一桶首样本泄漏
4. 空集 → 返回 nil（无数据不写）
5. `aggregate5m(pts, bucketStart)` 纯函数聚合
6. `writer.Write5m(ctx, buckets)`

### Run1h

同 Run5m 但聚合 5m 桶为 1h 桶，调 `Scan5mForDownsample` + `aggregate1h` + `Write1h`。

### Loop

```go
func (d *Downsampler) Loop(ctx) error
```

1. `sleepUntil(ctx, nextBoundary(now, 5m))` 对齐到下一个 5m 边界
2. 起 `t5` (5m ticker) + `t1h` (1h ticker)，defer Stop
3. 立即跑一次 `Run5m` catch up 刚完成的桶（注释）
4. select 循环：
   - `t5.C` → `Run5m(ctx, now)`，失败 warn 不退出
   - `t1h.C` → `Run1h(ctx, now)`，失败 warn 不退出
   - `ctx.Done()` → return nil

### aggregate5m（纯函数）

按 `EdgeID` 分组，每组合计 `cpuSum`/`memSum`/`l1Sum`/`l5Sum`/`l15Sum`/`diskSum` + 跟踪 `cpuMax`/`memMax`/`l1Max`/`l5Max`/`l15Max`/`diskMax`，net rx/tx 取 sum。输出 `[]Bucket5m`：avg = sum/n，max 直接取，net 是 sum。`Ts = bucketStart`。

### aggregate1h（纯函数）

同 aggregate5m 但输入是 `[]Bucket5m`。对 avg 字段做 avg-of-avg（等权重，不按 n 加权），max 字段取 max，sum 字段取 sum。输出 `[]Bucket1h`。

### nextBoundary

```go
func nextBoundary(now, interval) time.Time
```

返回严格大于 now 的下一个 interval 对齐时刻。

### sleepUntil

```go
func sleepUntil(ctx, t) error
```

阻塞到 t 或 ctx.Done。用 `time.NewTimer` + select，defer Stop。

## 5. 依赖关系

### 外部包

- `context` / `log/slog` / `time`

### 内部包

- `model "github.com/ongridio/ongrid/internal/manager/model/metric"` —— `Point` / `Bucket5m` / `Bucket1h`

### 依赖接口（在 repo.go 定义）

- `Writer.Write5m` / `Write1h`
- `Reader.ScanRawForDownsample` / `Scan5mForDownsample`

### 被谁调用

- `cmd/ongrid` 启动时 `go downsampler.Loop(ctx)` 长跑

## 6. 并发与资源管理

- **单 goroutine**：`Loop` 在一个 goroutine 里串行跑 5m + 1h，避免并发写同一桶
- **对齐到边界**：先 sleep 到下一个 5m 边界，确保第一次 Run5m 处理的是已完成的桶而非正在填充的桶
- **ticker defer Stop**：防 goroutine 泄漏
- **ctx.Done 优雅退出**：select 监听 ctx，return nil
- **无锁**：Downsampler 无共享可变状态，并发安全由 Writer/Reader 实现保证

## 7. 设计模式与亮点

### 纯函数聚合

`aggregate5m` / `aggregate1h` 是纯函数（无 IO、无副作用），输入输出明确。这让聚合逻辑极易测试 —— 不需 mock DB，直接构造 `[]Point` 检查输出 `[]Bucket5m`。

### 闭区间 to-1ns 防泄漏

`ScanRawForDownsample(ctx, bucketStart, bucketEnd.Add(-time.Nanosecond))`。注释解释：Reader 是闭区间 `[from, to]`，传 `to-1ns` 让下一桶的首样本不泄漏进当前桶。这种细节是数据正确性的护身符。

### 等权重 1h 聚合

`aggregate1h` 对 5m 的 avg 字段做等权重 avg，不按 5m 桶的样本数 n 加权（5m 桶已丢失 n 信息）。注释明确这是 Prometheus downsampling 的同款取舍 —— 精度换简单。

### Cadence 常量包私有

`interval5m` / `interval1h` 不导出，注释："防止测试 race against wall-clock scheduling"。测试通过 `Run5m(ctx, fixedTime)` 直接调功能核心，绕过 Loop 的 wall-clock 调度。

### Loop 启动立即 catch up

`Loop` 在对齐后立即跑一次 `Run5m`，处理对齐期间已完成的桶。注释明确意图。

### 失败 warn 不退出

`Run5m` / `Run1h` 失败只 warn log，不退出 Loop。下个 tick 会重试。这让临时 DB 故障不杀掉下采样 goroutine。

## 8. 注意事项

- **`Run5m` / `Run1h` 是功能核心**：测试应直接调它们而非 Loop。Loop 是 wall-clock 调度包装
- **`aggregate1h` 不按 n 加权**：精度有限，但 5m 桶已丢失 n。若未来需要精确加权，需让 `Bucket5m` 携带 `n` 字段
- **`ScanRawForDownsample` 跨所有 edge**：返回所有 edge 的 raw 点，按 (edge_id, ts) 升序。大部署下可能内存紧张，应考虑流式聚合（当前一次性 load）
- **`nextBoundary` 严格大于 now**：若 now 恰好对齐边界，返回下一个边界而非 now。这避免 0 sleep
- **Loop 退出不返回错误**：ctx.Done 返回 nil 而非 ctx.Err()。调用方无法区分"正常退出"与"ctx 取消"，但 cmd/ongrid 通常不关心
- **5m + 1h 在同 goroutine**：1h tick 时 5m tick 可能延迟。但 5m 跑得快（聚合小），影响可忽略
- **`Ts = bucketStart`**：桶的 ts 是开始时间。查询时 `[from, to]` 闭区间，注意边界
