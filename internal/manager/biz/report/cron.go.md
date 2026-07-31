# cron.go

## 1. 概述

`cron.go` 是 report 包的 cron 工具文件，封装 `robfig/cron/v3` 的解析器为三个窄函数：

- `CronNext(spec, loc, after)` —— 计算下一次触发时刻
- `CronSpecForKind(kind)` —— 返回预设类型（daily/weekly/monthly）的默认 cron spec
- `ValidateCronSpec(spec)` —— 校验 spec 可解析

注释明确：只用 robfig/cron 的 parser，不用它的 in-process scheduling machinery —— scheduler 跑自己的 ticker（HLD-014 §调度器）。

## 2. 包信息

- 包名：`report`
- 路径：`internal/manager/biz/report`

## 3. 关键类型与接口

无导出类型。文件只暴露三个函数。

## 4. 关键函数与流程

### CronNext

```go
func CronNext(spec string, loc *time.Location, after time.Time) (time.Time, error)
```

1. `loc == nil` → UTC
2. `cron.ParseStandard(spec)` 解析 5-field cron
3. 失败 → `errors.Join(ErrInvalid, err)`
4. `sched.Next(after.In(loc))` 返回严格大于 `after` 的下一次触发时刻（携带 `loc`）

### CronSpecForKind

```go
func CronSpecForKind(kind string) (string, error)
```

返回预设类型的默认 5-field cron：
- `daily` → `"0 9 * * *"`（每天 09:00）
- `weekly` → `"0 9 * * 1"`（每周一 09:00）
- `monthly` → `"0 9 1 * *"`（每月 1 号 09:00）
- `custom` → `ErrInvalid`（caller 必须显式给 CronSpec）
- 其它 → `ErrInvalid: unknown report kind`

注释：前端可覆盖 time-of-day；这些是 chat-created 路径与测试用的合理默认。

### ValidateCronSpec

```go
func ValidateCronSpec(spec string) error
```

`cron.ParseStandard(spec)`，失败 `errors.Join(ErrInvalid, err)`。API 层（PR-4）在 create 时调用，拒绝坏 custom cron 而非静默永不触发。

## 5. 依赖关系

### 外部包

- `errors` / `time`
- `github.com/robfig/cron/v3` —— cron parser

### 内部包

- `model "github.com/ongridio/ongrid/internal/manager/model/report"` —— `KindDaily` / `KindWeekly` / `KindMonthly` / `KindCustom` 常量
- `github.com/ongridio/ongrid/internal/pkg/errs` —— `ErrInvalid`

### 被谁调用

- `usecase.go` 的 `CreateSchedule` / `UpdateSchedule` / `SetScheduleEnabled` 调 `CronNext` + `CronSpecForKind`
- `scheduler.go` 的 `fireOne` 调 `CronNext` 重 arm
- HTTP handler（PR-4）调 `ValidateCronSpec` 在 create 时校验

## 6. 并发与资源管理

不适用（纯函数，无共享状态）。

## 7. 设计模式与亮点

### Parser only, no scheduler

注释明确：只用 robfig/cron 的 parser，不用它的 in-process scheduling。这让 scheduler 完全自控 ticker 逻辑，robfig/cron 的 goroutine 不介入，避免双重调度。

### 时区携带

`CronNext` 返回的 time 携带 `loc`，让 caller 能存 UTC 同时本地渲染。`after.In(loc)` 确保计算在正确时区做。

### 预设默认值

`CronSpecForKind` 给 daily/weekly/monthly 合理默认（09:00），让 chat-created 路径与测试不必每次构造 spec。前端可覆盖 time-of-day。

### Custom 显式要求 spec

`CronSpecForKind(KindCustom)` 返回 error 而非空字符串。注释：caller 必须显式给 CronSpec。这防 custom 类型静默用空 spec 永不触发。

### 5-field 标准

`cron.ParseStandard` 是 5-field（min hour dom mon dow），非 7-field。注释与代码一致。

## 8. 注意事项

- **robfig/cron 仅用于解析**：不要在此包引入 robfig/cron 的 scheduler。如需替换 parser 库，三个函数都需同步改
- **`CronNext` 严格大于 after**：`sched.Next` 返回严格大于输入的时刻。若 after 恰好是触发时刻，返回下一次
- **`CronSpecForKind` 默认 09:00**：是合理默认但非配置。运维若想改默认时间需在前端覆盖
- **`ValidateCronSpec` 不返回 parsed spec**：只校验。若需 parsed spec 应直接调 `cron.ParseStandard`
- **`errors.Join` 包装**：错误是 `ErrInvalid` + 原始 err 的 join。调用方 `errors.Is(err, ErrInvalid)` 成立
- **5-field 不支持秒**：若需秒级精度，需换 7-field parser
