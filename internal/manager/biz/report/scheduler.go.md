# scheduler.go

## 1. 概述

`scheduler.go` 实现 report 包的 cron 评估器（HLD-014 §调度器）—— 每分钟 tick 一次，select due schedules 并 fire。

设计要点：
- **自己跑 ticker**：robfig/cron 仅用于算 next-fire time，不用于 schedule
- **单 goroutine**：一个 Scheduler per manager process，tick 串行 fire（防 stampede）
- **panic contain**：单 schedule panic 不杀整个 loop
- **bad config disable**：坏 timezone / cron spec 的 schedule 自动 disable（防每分钟 re-select 永远）

## 2. 包信息

- 包名：`report`
- 路径：`internal/manager/biz/report`

## 3. 关键类型与接口

### Scheduler

```go
type Scheduler struct {
    uc   *Usecase
    tick time.Duration
    log  *slog.Logger
}
```

### 常量

```go
const defaultTick = time.Minute
```

注释：1 分钟匹配 cron 最小粒度 —— schedule 永不晚于 cron 时间 ~1 分钟 fire。

## 4. 关键函数与流程

### NewScheduler

```go
func NewScheduler(uc *Usecase, log *slog.Logger) *Scheduler
```

`tick = defaultTick`，log nil 回退 default + `With("comp", "report-scheduler")`。

### Start

```go
func (s *Scheduler) Start(ctx context.Context)
```

`go s.loop(ctx)`。立即返回，loop 跑到 ctx cancelled。

### loop

```go
func (s *Scheduler) loop(ctx context.Context)
```

1. `time.NewTicker(s.tick)` + defer Stop
2. `log.Info("report scheduler started", tick)`
3. select 循环：
   - `ctx.Done` → `log.Info("stopped")` + return
   - `now := <-t.C` → `s.runOnce(ctx, now.UTC())`

### runOnce

```go
func (s *Scheduler) runOnce(ctx context.Context, now time.Time)
```

1. `due, err := s.uc.repo.DueSchedules(ctx, now)` —— 查 `next_fire_at <= now` 的 enabled schedules
2. 失败 → warn log + return
3. 对每个 due schedule：`s.fireOne(ctx, sched, now)`

### fireOne

```go
func (s *Scheduler) fireOne(ctx context.Context, sched *model.ReportSchedule, now time.Time)
```

1. `defer recover()` —— panic containment，log error + schedule_id + panic
2. `loadLocation(sched.Timezone)` 失败 → warn + `s.disable(ctx, sched)` + return
3. `CronNext(sched.CronSpec, loc, now)` 失败 → warn + `s.disable(ctx, sched)` + return
4. `s.uc.FireSchedule(ctx, sched, now, next)` 失败 → warn（FireSchedule 已 re-arm）

### disable

```go
func (s *Scheduler) disable(ctx context.Context, sched *model.ReportSchedule)
```

`sched.Enabled = false` + `sched.NextFireAt = nil` + `repo.UpdateSchedule`。注释：bad config 的 schedule disable 比每分钟 re-select 永远好。操作员在 UI 看到 `enabled=false` 修 spec。

## 5. 依赖关系

### 外部包

- `context` / `log/slog` / `time`

### 内部包

- `model "github.com/ongridio/ongrid/internal/manager/model/report"` —— `ReportSchedule`
- 同包：`Usecase` / `Repo.DueSchedules` / `FireSchedule` / `loadLocation` / `CronNext`

### 被谁调用

- `cmd/ongrid` 启动 `scheduler.Start(ctx)` 长跑

## 6. 并发与资源管理

- **单 goroutine**：`loop` 在一个 goroutine 跑，串行 fire schedule
- **ticker defer Stop**：防 goroutine 泄漏
- **panic recover**：`fireOne` defer recover，单 schedule panic 不杀 loop
- **串行 fire**：注释：each fire is synchronous within the tick —— 慢 `CreateReport`/`UpdateSchedule` 串行而非 stampede。Generator 跑 LLM 在自己 goroutine（`FireSchedule` spawns it），tick loop 永不 block on LLM

## 7. 设计模式与亮点

### 自己跑 ticker 而非 robfig/cron scheduler

注释：owns its own ticker —— robfig/cron 仅用于算 next-fire time，不用于 schedule。这让 Scheduler 完全自控，robfig/cron 的 goroutine 不介入，避免双重调度。

### 单 goroutine 串行 fire

注释：one Scheduler per manager process，single goroutine drives ticks，each fire synchronous。防 stampede —— 慢 `CreateReport` 串行而非并发。Generator 跑 LLM 在自己 goroutine，tick loop 不 block。

### Panic containment

`fireOne` defer recover。注释：a panic or error in one schedule is contained so the rest of the batch still fires and the loop survives。单 schedule panic 不杀整个 loop。

### Bad config disable

`disable` 把坏 timezone / cron spec 的 schedule 设 `Enabled = false` + `NextFireAt = nil`。注释：比每分钟 re-select 永远好。操作员在 UI 看到 `enabled=false` 修 spec。

### 1 分钟 tick

`defaultTick = time.Minute`。注释：匹配 cron 最小粒度，schedule 永不晚于 cron 时间 ~1 分钟 fire。

### ctx.Done 优雅退出

`loop` select `ctx.Done` → log + return。让 manager shutdown 优雅停止 scheduler。

### FireSchedule 已 re-arm

`fireOne` 调 `FireSchedule` 失败只 warn。注释：FireSchedule already re-armed where it could。不重复 re-arm。

## 8. 注意事项

- **单 goroutine 串行**：慢 `CreateReport` 会阻塞同 tick 的其它 schedule fire。若 DB 慢，下个 tick 可能堆积 due schedules
- **`DueSchedules` 返回所有 due**：若堆积多个，串行 fire 可能超 tick。下个 tick 会 re-select 已 fire 的（因 `next_fire_at` 已 re-arm，不再 due）
- **`disable` 持久化失败只 warn**：`UpdateSchedule` 失败 log warn，schedule 仍 `Enabled = true`，下个 tick 会 re-select。可能无限 re-select 坏 schedule
- **`fireOne` panic recover 不持久化失败**：panic 后 schedule 状态未变，下个 tick 会 re-fire。可能无限 panic
- **`defaultTick` 1 分钟硬编码**：不可配。若需更频繁调度需改常量
- **`Start` 立即返回**：loop 异步跑。caller 无法知道 loop 是否启动成功 —— 应有外部监控
- **`runOnce` 不返回 error**：所有错误 warn log。调用方无法知道 tick 是否成功
- **`FireSchedule` 在自己 goroutine 跑 Generator**：注释明确。tick loop 不 block on LLM，但 Generator goroutine 可能堆积 —— 应有并发限制
