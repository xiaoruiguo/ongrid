# usecase.go

## 1. 概述

`usecase.go` 是 report 包的核心用例文件，定义 `Usecase` 结构与 schedule CRUD + report 生成入口。PR-1 落地 skeleton：schedule CRUD passthrough + period 计算 + dedup-protected report 行创建。cron 评估器（PR-3）与 manual generate API（PR-4）都调 `FireSchedule` / `GenerateNow`。

`Generator` 是 seam 接口（PR-1 nopGenerator，PR-2 workerGenerator），`Usecase` 异步调 `Generate` 让慢 LLM 不阻塞 evaluator tick。

## 2. 包信息

- 包名：`report`
- 路径：`internal/manager/biz/report`
- 包注释：明确 PR-1 skeleton + Generator seam

## 3. 关键类型与接口

### Repo 接口

```go
type Repo interface {
    CreateReport(ctx, *model.Report) error  // (schedule_id, period_start) unique 冲突须 surface 为 ErrConflict
    GetReport(ctx, id string) (*model.Report, error)
    UpdateReport(ctx, *model.Report) error

    CreateSchedule(ctx, *model.ReportSchedule) error
    GetSchedule(ctx, id uint64) (*model.ReportSchedule, error)
    UpdateSchedule(ctx, *model.ReportSchedule) error
    DueSchedules(ctx, now time.Time) ([]*model.ReportSchedule, error)  // next_fire_at <= now
}
```

### IDGen

```go
type IDGen func() string
```

注入让测试 deterministic，不硬依赖 uuid lib。

### Generator 接口

```go
type Generator interface {
    Generate(ctx, reportID string)
}
```

PR-1 nopGenerator（report 留 pending）；PR-2 workerGenerator。`Usecase` 异步调让慢 LLM 不阻塞 evaluator tick。

### readyChecker（内部接口）

```go
type readyChecker interface {
    Ready(ctx context.Context) error
}
```

类型断言检查 Generator 是否实现，让 `unavailableGenerator` 能在 fire 前 fail-fast。

### nopGenerator / unavailableGenerator

降级占位。`nopGenerator.Generate` no-op；`unavailableGenerator.Generate` no-op 但 `Ready` 返回 error。

### Usecase

```go
type Usecase struct {
    repo          Repo
    read          ReadRepo  // WithReadRepo 注入；scheduler-only 路径 nil
    gen           Generator
    idGen         IDGen
    defaultLocale string    // scheduled (headless) 报告语言；manual override 用 Accept-Language
}
```

## 4. 关键函数与流程

### NewUsecase

```go
func NewUsecase(repo Repo, gen Generator, idGen IDGen) *Usecase
```

`repo == nil` / `idGen == nil` panic（wiring error）。`gen == nil` 回退 `nopGenerator`。

### WithDefaultLocale

Builder 链。`defaultLocale` stamp scheduled reports' title + content language。main.go 用 `ONGRID_DEFAULT_LOCALE` 设。

### CreateSchedule

```go
func (u *Usecase) CreateSchedule(ctx, s *model.ReportSchedule, now time.Time) error
```

1. `CronSpec` 空 + `Kind != Custom` → `CronSpecForKind` 填默认
2. `loadLocation(s.Timezone)` 失败 → error
3. `CronNext(s.CronSpec, loc, now)` 算 initial `next_fire_at`
4. `ScopeJSON` 空 → `"{}"`；`ChannelIDsJSON` 空 → `"[]"`；`AgentPersona` 空 → `DefaultReporterPersona`
5. `s.NextFireAt = &next`
6. `repo.CreateSchedule`

### FireSchedule（核心）

```go
func (u *Usecase) FireSchedule(ctx, s *model.ReportSchedule, fireAt, nextFireAt time.Time) (*model.Report, error)
```

1. `ensureGeneratorReady(ctx)` —— 类型断言 `readyChecker`，失败返回 error
2. `loadLocation(s.Timezone)` + `PeriodFor(s.Kind, fireAt, loc, prevFire)` 算 period
3. `rpt = u.buildPendingReport(s, period)`
4. `createErr := repo.CreateReport(ctx, rpt)` —— 可能 `ErrConflict`（dedup）
5. **无论 createErr 如何，re-arm schedule**：`s.LastFireAt = &fireAt` + `s.NextFireAt = &nextFireAt` + `repo.UpdateSchedule`。注释：duplicate 仍 means "this window is handled"，必须 advance `next_fire_at` 否则 evaluator 永远 re-select
6. `createErr == ErrConflict` → return nil, nil（duplicate window，非 error）
7. `createErr` 其它 → return error
8. `go u.gen.Generate(context.WithoutCancel(ctx), rpt.ID)` —— 异步触发生成
9. return rpt

### GenerateNow

```go
func (u *Usecase) GenerateNow(ctx, createdBy uint64, kind, tz, scopeJSON, locale, taskRef string, period Period) (*model.Report, error)
```

1. `loadLocation(tz)` 校验
2. `ensureGeneratorReady(ctx)`
3. `locale` 空 → `u.defaultLocale`
4. 构造 `model.Report`：`ScheduleID = nil`（manual，dedup key 不适用）、`TaskID = taskRef`、`RunID = u.idGen()`、`Title = TitleFor(...)`、`Status = StatusPending`
5. `repo.CreateReport`
6. `go u.gen.Generate(context.WithoutCancel(ctx), rpt.ID)`

### buildPendingReport

```go
func (u *Usecase) buildPendingReport(s *model.ReportSchedule, p Period) *model.Report
```

构造 scheduled fire 的 pending 行：`ScheduleID = &s.ID`、`TaskID = "report-schedule:<id>"`、`RunID = u.idGen()`、`Title = TitleFor(s.Kind, p, u.defaultLocale)`、`Locale = u.defaultLocale`。

### ensureGeneratorReady

```go
func (u *Usecase) ensureGeneratorReady(ctx) error
```

类型断言 `u.gen.(readyChecker)`。若实现 `Ready`，调它；否则 nil。让 `unavailableGenerator` 在 fire 前 fail-fast。

### loadLocation

```go
func loadLocation(tz string) (*time.Location, error)
```

`tz` 空 → UTC；`time.LoadLocation` 失败 → `errors.Join(ErrInvalid, err)`。

## 5. 依赖关系

### 外部包

- `context` / `errors` / `fmt` / `time`

### 内部包

- `model "github.com/ongridio/ongrid/internal/manager/model/report"` —— `Report` / `ReportSchedule` / `Status*` / `Kind*` / `DefaultReporterPersona`
- `github.com/ongridio/ongrid/internal/pkg/errs` —— `ErrInvalid` / `ErrConflict` / `ErrNotWiredYet`
- 同包：`ReadRepo`（在 `query.go`）、`CronSpecForKind` / `CronNext` / `PeriodFor` / `TitleFor`（在 `cron.go` / `period.go`）

### 被谁调用

- `scheduler.go` 的 `fireOne` 调 `FireSchedule`
- `query.go` 的 `RunNow` 调 `GenerateNow`
- `task.go` 的 `CreateOneoffTaskAndRun` / `RerunOneoffTask` 调 `GenerateNow`
- HTTP handler 调 `CreateSchedule` / `UpdateSchedule` 等

## 6. 并发与资源管理

- **无锁**：`Usecase` 无共享可变状态；并发安全由 repo 保证
- **异步 Generate**：`FireSchedule` / `GenerateNow` 用 `go u.gen.Generate(context.WithoutCancel(ctx), rpt.ID)` 异步触发，不阻塞 caller
- **WithoutCancel**：用 `context.WithoutCancel(ctx)` 让请求 ctx 取消后生成仍继续
- **`idGen` 并发安全**：uuid lib 通常线程安全；测试注入的 fake 应也安全

## 7. 设计模式与亮点

### Generator seam

`Generator` 是 seam 接口，PR-1 nopGenerator，PR-2 workerGenerator。`Usecase` 不依赖具体实现，让 PR-1 能 ship skeleton 而 PR-2 替换实现。

### Dedup-protected report 创建

`FireSchedule` 的 `CreateReport` 可能 `ErrConflict`（`(schedule_id, period_start)` unique key）。注释：两个 evaluator tick race 同 schedule，第二个 hit conflict，skip generation 但仍 re-arm。这是 idempotent fire 的关键。

### Re-arm 无论 create 成功与否

注释：duplicate 仍 means "this window is handled"，必须 advance `next_fire_at` 否则 evaluator 永远 re-select。re-arm 失败 surface error —— 留 stale `next_fire_at` 会 repeated re-fires。

### 异步 Generate + WithoutCancel

`go u.gen.Generate(context.WithoutCancel(ctx), rpt.ID)` 异步触发。`WithoutCancel` 让请求 ctx 取消后生成仍继续。这让 evaluator tick 不阻塞 LLM，且 API 请求返回后生成继续。

### ensureGeneratorReady fail-fast

`ensureGeneratorReady` 类型断言 `readyChecker`。`unavailableGenerator` 实现 `Ready` 返回 error，让 fire 前 fail-fast 而非留 pending 行。注释：let report routes stay mounted while generation unavailable。

### buildPendingReport vs GenerateNow

`buildPendingReport` 给 scheduled fire（`ScheduleID = &s.ID`，dedup key 适用）；`GenerateNow` 给 manual（`ScheduleID = nil`，dedup key 不适用，每次 fresh row）。注释：manual trigger produces a fresh row。

### defaultLocale 双轨

scheduled (headless) 报告用 `u.defaultLocale`；manual generate / run-now 用 requester's Accept-Language（`GenerateNow` 的 `locale` 参数）。注释：feedback_ai_output_locale。

### nopGenerator 显式非 nil

`gen == nil` 回退 `nopGenerator` 而非 nil。注释：surfaced explicitly so callers don't have to nil-check on every fire。

## 8. 注意事项

- **`FireSchedule` re-arm 失败 surface error**：`UpdateSchedule` 失败返回 error。注释：留 stale `next_fire_at` 会 repeated re-fires。但报告（若已 create）仍 fine
- **`GenerateNow` `ScheduleID = nil`**：manual 报告不参与 dedup。每次 trigger fresh row —— 高频 manual 会产多份报告
- **`ensureGeneratorReady` 类型断言**：若 Generator 不实现 `readyChecker`，返回 nil（认为 ready）。`workerGenerator` 通过 `WithReadyCheck` 注入 ready fn
- **`defaultLocale` 空 = 中文标题默认**：注释明确。`WithDefaultLocale` 在 main.go 用 `ONGRID_DEFAULT_LOCALE` 设
- **`NewUsecase` panic on nil repo/idGen**：wiring error。main.go 必须传非 nil
- **`FireSchedule` 异步 Generate**：caller 无法知道生成是否成功 —— 依赖 `generator.go` 的 `fail` 持久化 status
- **`buildPendingReport` `TaskID = "report-schedule:<id>"`**：scheduled 报告的 task 回引。`ListReports` filter by `TaskID` 能列 schedule 的所有报告
- **`errExpiredShare` 是包私有**：`errors.Join(ErrNotFound, ...)`。HTTP handler `errors.Is(err, ErrNotFound)` 成立，返回 404
- **`loadLocation` 是包私有 helper**：`tz` 空 → UTC。多个文件复用
