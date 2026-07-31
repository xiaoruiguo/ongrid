# period.go

## 1. 概述

`period.go` 实现 report 包的周期计算 —— `PeriodFor` 根据 schedule kind + fire time + 时区算报告应覆盖的时间窗口；`TitleFor` 生成操作员可见的报告标题。

边界语义：`Period` 是 `[Start, End)` 半开，但持久化时 End materialise 为窗口最后瞬间（23:59:59.999...）让"上周日 23:59:59"读起来自然；数据收集 SQL 用 `[Start, End]` 闭区间（实际 nanosecond 边界不会命中）。

所有时间运算在 schedule 时区做，让"weekly"指操作员的周一而非 UTC 周一。

## 2. 包信息

- 包名：`report`
- 路径：`internal/manager/biz/report`

## 3. 关键类型与接口

### Period

```go
type Period struct {
    Start time.Time
    End   time.Time
}
```

注释：`[Start, End)` 半开；持久化 End 为最后瞬间；SQL 用 `[Start, End]` 闭区间。

## 4. 关键函数与流程

### PeriodFor

```go
func PeriodFor(kind string, fireAt time.Time, loc *time.Location, prevFireAt time.Time) (Period, error)
```

1. `loc == nil` → UTC
2. `f = fireAt.In(loc)`
3. switch kind：
   - `KindDaily`：`end = startOfDay(f)`（今天 00:00），`start = end - 1d`（昨天 00:00）
   - `KindWeekly`：`thisMon = startOfISOWeek(f)`，`start = thisMon - 7d`，`end = thisMon`
   - `KindMonthly`：`firstThis = startOfMonth(f)`，`start = firstThis - 1month`，`end = firstThis`
   - `KindCustom`：`end = f`，`start = prevFireAt.In(loc)`；`prevFireAt.IsZero() || !start.Before(end)` → `start = end - 1d`（首跑或时钟异常，默认 trailing 24h）
   - default → error

### TitleFor

```go
func TitleFor(kind string, p Period, locale string) string
```

按 kind + locale 生成标题：
- `daily` → `日报 · 2026-07-30` / `Daily · 2026-07-30`
- `weekly` → `周报 · 2026 W30 (07-22 – 07-28)` / `Weekly · 2026 W30 (07-22 – 07-28)`
- `monthly` → `月报 · 2026-07` / `Monthly · 2026-07`
- `custom` → `报告 · 2026-07-22 09:00 – 2026-07-30 09:00`
- default → `报告 · start – end`

注释：language-neutral structure（数字 + ISO），kind word 由 SPA render 时 localize。

### 日期辅助函数

- `startOfDay(t)` —— `t` 的 00:00:00
- `startOfMonth(t)` —— `t` 的 1 号 00:00:00
- `startOfISOWeek(t)` —— `t` 所在 ISO 周的周一 00:00:00。注释：Go `Weekday()` 让 Sunday=0，remap 到 Monday=0 distance：`(weekday + 6) % 7`

## 5. 依赖关系

### 外部包

- `fmt` / `strings` / `time`

### 内部包

- `model "github.com/ongridio/ongrid/internal/manager/model/report"` —— `KindDaily` / `KindWeekly` / `KindMonthly` / `KindCustom`

### 被谁调用

- `usecase.go` 的 `FireSchedule` / `RunNow` / `GenerateNow` 调 `PeriodFor` 算周期
- `task.go` 的 `CreateOneoffTaskAndRun` / `RerunOneoffTask` 调 `PeriodFor`
- `generator.go` 的 `generate` 调 `previousPeriod(period)`（本文件定义）
- `usecase.go` / `generator.go` 调 `TitleFor` 生成报告标题

## 6. 并发与资源管理

不适用（纯函数，无共享状态）。

## 7. 设计模式与亮点

### 半开区间 + 持久化最后瞬间

`Period` 是 `[Start, End)` 半开，但持久化 End 为 23:59:59.999...。注释：让"上周日 23:59:59"读起来自然；SQL 用 `[Start, End]` 闭区间，nanosecond 边界实际不命中。这是数据建模与人类可读性的折中。

### 时区感知

所有运算在 schedule 时区做。注释：让"weekly"指操作员的周一而非 UTC 周一。返回 time 携带 `loc`。

### ISO 周计算

`startOfISOWeek` 用 `(weekday + 6) % 7` remap Sunday=0 到 Monday=0 distance。注释明确 Go `Weekday()` 的 quirk。

### Custom 首跑 fallback

`KindCustom` 首跑（`prevFireAt.IsZero()`）或时钟异常（`!start.Before(end)`）默认 trailing 24h。注释：永不 emit 空/反向窗口。

### Title locale-aware 但 structure-neutral

`TitleFor` 用 ISO 周/月/日数字，kind word 双语。注释：structure 是 locale-agnostic，SPA render 时再 localize kind word。

### previousPeriod 等长

`previousPeriod` 返回等长前一窗口（`p.Start.Add(-d)`）。用于 period-over-period delta 计算。

## 8. 注意事项

- **`Period` 半开但 SQL 闭区间**：数据收集 SQL 用 `[Start, End]`。若 End 恰好命中数据点（罕见），会同时进当前与下一周期。注释明确 nanosecond 边界实际不命中
- **`PeriodFor` 时区必须传**：传 nil 用 UTC。生产应传 schedule 的 timezone
- **`KindCustom` 首跑 24h fallback**：首跑无 `prevFireAt`，默认 trailing 24h。若 schedule 是周级，首跑只覆盖 24h —— 操作员应知道
- **`TitleFor` weekly 用 ISO 周**：`p.Start.ISOWeek()` 返回 (year, week)。ISO 周跨年时 year 可能是上一年
- **`startOfISOWeek` Sunday=0 quirk**：Go `Weekday()` Sunday=0，remap `(weekday + 6) % 7`。若改用其它语言移植注意
- **`TitleFor` custom 含时间**：`custom` 标题含 `15:04`（时分），其它只到日/月。custom 窗口可能小于一天
- **`PeriodFor` default error**：未知 kind 返回 error。调用方应校验 kind 在调用前
