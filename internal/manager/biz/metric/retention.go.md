# retention.go

## 1. 概述

`retention.go` 实现 metric 包的保留期执行器 —— 按层级 TTL 删除过期行：
- raw: 7 天
- 5m: 90 天
- 1h: 365 天

`RunOnce` 是功能核心，对三层依次循环分批删除（每批 1000 行）直到返回 0。`Loop` 每天在 UTC 03:00（传统低峰窗口）调度一次 `RunOnce`。

## 2. 包信息

- 包名：`metric`
- 路径：`internal/manager/biz/metric`

## 3. 关键类型与接口

### Retention

```go
type Retention struct {
    writer Writer
    log    *slog.Logger
    rawTTL time.Duration
    m5TTL  time.Duration
    h1TTL  time.Duration
}
```

### 默认值常量

```go
const (
    defaultRawTTL = 7 * 24 * time.Hour
    defaultM5TTL  = 90 * 24 * time.Hour
    defaultH1TTL  = 365 * 24 * time.Hour

    retentionBatchLimit = 1000
    retentionRunAtHour  = 3  // UTC 03:00 低峰窗口
)
```

## 4. 关键函数与流程

### NewRetention

构造 + 默认 TTL。log nil 回退 `slog.Default()`。

### RunOnce

```go
func (r *Retention) RunOnce(ctx) error
```

1. `now = time.Now().UTC()`
2. 构造三层 tier 列表：`{name, cutoff, del}` —— `cutoff = now.Add(-ttl)`，`del` 是 `r.writer.DeleteRawBefore` / `Delete5mBefore` / `Delete1hBefore`
3. 对每层：
   - 循环 `del(ctx, cutoff, 1000)` 直到返回 0
   - 每批前检查 `ctx.Err()`
   - 失败 warn + break 当前层（不影响下一层）
   - 完成后 `log.Info("tier complete", tier, deleted, cutoff)`

### Loop

```go
func (r *Retention) Loop(ctx) error
```

1. `next := nextDailyRun(now, 3)` 计算下一个 UTC 03:00
2. `sleepUntil(ctx, next)` —— ctx.Done 返回 nil
3. `RunOnce(ctx)` —— 失败 warn 不退出
4. 循环

### nextDailyRun

```go
func nextDailyRun(now time.Time, hour int) time.Time
```

返回下一个 UTC wall-clock `hour:00:00`。若今天此刻已过该点，返回明天的。

### sleepUntil

复用 `downsample.go` 的同函数：`time.NewTimer` + select ctx.Done。

## 5. 依赖关系

### 外部包

- `context` / `log/slog` / `time`

### 依赖接口（在 repo.go 定义）

- `Writer.DeleteRawBefore` / `Delete5mBefore` / `Delete1hBefore`

### 复用函数

- `sleepUntil` / `nextBoundary`（在 `downsample.go` 定义）

### 被谁调用

- `cmd/ongrid` 启动 `go retention.Loop(ctx)` 长跑
- 测试直接调 `RunOnce(ctx)` 绕过 wall-clock 调度

## 6. 并发与资源管理

- **单 goroutine**：`Loop` 在一个 goroutine 里跑
- **分批删除**：每批 1000 行，避免长事务锁全表（SQLite writer lock 注释提及）
- **ctx.Err() 检查**：每批前检查，支持优雅退出
- **无锁**：Retention 无共享可变状态

## 7. 设计模式与亮点

### 三层 tier 数据驱动

```go
tiers := []struct {
    name   string
    cutoff time.Time
    del    func(ctx, cutoff, limit) (int64, error)
}{
    {"raw", now.Add(-r.rawTTL), r.writer.DeleteRawBefore},
    {"5m", now.Add(-r.m5TTL), r.writer.Delete5mBefore},
    {"1h", now.Add(-r.h1TTL), r.writer.Delete1hBefore},
}
```

用 slice of struct 数据驱动，避免三段重复代码。新增 tier 只加一行。

### 分批删除 + ctx 检查

每批 1000 行 + 每批前 `ctx.Err()` 检查。这让长删除任务可被 ctx 取消，且不锁全表。

### 失败隔离

某层失败只 break 当前层，不影响下一层。raw 删失败仍会尝试删 5m / 1h。这让单点故障不阻断整个 retention pass。

### UTC 03:00 低峰窗口

`retentionRunAtHour = 3` 是 UTC 03:00，注释："traditional low traffic window"。避开业务高峰期跑批量删除。

### RunOnce 可独立测试

`RunOnce` 是功能核心，`Loop` 只是 wall-clock 调度包装。测试直接调 `RunOnce` + mock Writer，不涉 wall-clock。

### 复用 sleepUntil

`sleepUntil` 与 `nextBoundary` 定义在 `downsample.go`，本文件复用。避免重复。

## 8. 注意事项

- **TTL 是包私有常量**：`defaultRawTTL` 等不导出。若未来需运维可配，应加 Config 结构或 env 读取
- **`RunOnce` 不返回 tier 失败错误**：某层失败只 warn，`RunOnce` 仍返回 nil。调用方无法知道哪层失败 —— 应查 log
- **`Loop` 退出返回 nil**：ctx.Done 时 `sleepUntil` 返回 nil，Loop return nil。调用方无法区分"正常退出"与"ctx 取消"
- **`retentionBatchLimit = 1000` 是硬编码**：大表可能需要更大批量。当前 SQLite 友好
- **`nextDailyRun` UTC 固定**：不跟随时区。运维在 UTC 03:00 跑删除任务，本地时间可能是白天
- **三层 TTL 与 query.go maxWindow 对齐**：1h TTL 365d，`maxWindow` 也是 365d。改 TTL 应同步改 maxWindow
- **`sleepUntil` 复用 downsample.go 的**：若 downsample.go 删除 sleepUntil，本文件会编译失败。应考虑把 sleepUntil 提到独立 util 文件
- **`Retention` 无 `Now` 注入**：`RunOnce` 用 `time.Now().UTC()`，测试需 mock 时间时只能测 `Loop` 间接。未来加 `Now func() time.Time` 字段更友好
