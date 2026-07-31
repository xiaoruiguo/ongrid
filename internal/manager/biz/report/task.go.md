# task.go

## 1. 概述

`task.go` 实现 report 包的统一任务用例（HLD-022 Phase 2）—— 一次性任务（oneoff，立即触发）的 CRUD + 立即生成报告。

注释明确：stored tasks 是 oneoff（run-once，从 task 侧立即触发）；recurring tasks 仍是 `report_schedules`，在 server 层 union 进来。

## 2. 包信息

- 包名：`report`
- 路径：`internal/manager/biz/report`
- 文件注释：`task.go — the unified-task usecase (HLD-022 Phase 2)`

## 3. 关键类型与接口

本文件不定义新类型，全部是 `Usecase` 上的方法。依赖 `model.Task`（在 `model/report` 包定义）+ `ReadRepo` 的 task 方法（在 `query.go` 定义）。

## 4. 关键函数与流程

### CreateOneoffTaskAndRun

```go
func (u *Usecase) CreateOneoffTaskAndRun(ctx, createdBy uint64, title, kind, tz, scopeJSON, locale string, now time.Time) (*model.Task, error)
```

1. `u.read == nil` → error "task store not wired"
2. `tz` 空 → "UTC"
3. `loadLocation(tz)` 失败 → error
4. `PeriodFor(kind, now, loc, time.Time{})` —— 首跑 `prevFireAt = zero`
5. `title` 空 → `TitleFor(kind, period, locale)`
6. 构造 `model.Task`：`ID = u.idGen()`、`Kind = TaskKindOneoff`、`ReportKind = kind`、`ScopeJSON`、`Status = "active"`、`CreatedBy`
7. `u.read.CreateTask(ctx, t)`
8. `u.GenerateNow(ctx, createdBy, kind, tz, scopeJSON, locale, model.OneoffTaskRef(t.ID), period)` —— 立即生成，attributing artifact to task via `task_id="oneoff:<id>"`
9. 返回 `t`（即使 GenerateNow 失败也返回 task —— task 行已存在，用户可 rerun）

注释：best-effort —— task 行已存在，generation 失败仍留 visible task，用户可 rerun。

### RerunOneoffTask

```go
func (u *Usecase) RerunOneoffTask(ctx, taskID, locale string, now time.Time) (*model.Task, error)
```

1. `u.read == nil` → error
2. `u.read.GetTask(ctx, taskID)`
3. `loadLocation("UTC")` —— rerun 用 UTC（task 不存 tz，用 UTC 默认）
4. `PeriodFor(t.ReportKind, now, loc, time.Time{})` —— `prevFireAt = zero`，rerun 用当前窗口
5. `u.GenerateNow(ctx, t.CreatedBy, t.ReportKind, "UTC", t.ScopeJSON, locale, model.OneoffTaskRef(t.ID), period)`
6. 返回 `t`

### ListOneoffTasks / GetTask / DeleteTask

- `ListOneoffTasks`：`u.read == nil` → 返回 nil, nil（容错）；否则 `u.read.ListTasks(ctx)`
- `GetTask`：`u.read == nil` → error；否则 `u.read.GetTask(ctx, id)`
- `DeleteTask`：`u.read == nil` → error；否则 `u.read.DeleteTask(ctx, id)`。注释：reports 保留为 standalone artifacts

## 5. 依赖关系

### 外部包

- `context` / `fmt` / `strings` / `time`

### 内部包

- `model "github.com/ongridio/ongrid/internal/manager/model/report"` —— `Task` / `TaskKindOneoff` / `OneoffTaskRef`
- 同包：`Usecase` / `ReadRepo` / `PeriodFor` / `TitleFor` / `GenerateNow` / `loadLocation`

### 被谁调用

- HTTP handler（`/v1/tasks/*`）调 task CRUD
- `task.go` 的 `CreateOneoffTaskAndRun` 调 `GenerateNow`

## 6. 并发与资源管理

- 无锁、无 goroutine，纯同步方法
- `GenerateNow` 内部 `go u.gen.Generate(...)` 异步触发生成
- `Usecase` 无共享可变状态；并发安全由 `ReadRepo` 保证

## 7. 设计模式与亮点

### Oneoff vs recurring 分离

注释：stored tasks 是 oneoff（run-once，立即触发）；recurring tasks 仍是 `report_schedules`，server 层 union。这让 oneoff 与 recurring 数据模型分离，server 层负责合并视图。

### Task 行先存再生成

`CreateOneoffTaskAndRun` 先 `CreateTask` 再 `GenerateNow`。注释：best-effort —— task 行已存在，generation 失败仍留 visible task，用户可 rerun。这让生成失败不丢任务记录。

### TaskID 回引

`GenerateNow` 的 `taskRef` 参数 = `model.OneoffTaskRef(t.ID)`（即 `"oneoff:<id>"`）。报告的 `task_id` 字段回引 task，让任务详情页能列该任务的所有报告（`ListReports` filter by `TaskID`）。

### Rerun 用 UTC

`RerunOneoffTask` 用 UTC 而非原 task 的 tz。注释：task 不存 tz，用 UTC 默认。这是简化 —— rerun 的 period 可能与原 run 不同。

### ListOneoffTasks 容错 nil

`ListOneoffTasks` 在 `u.read == nil` 时返回 `nil, nil` 而非 error。让 server 层 union 时不必处理 error。但 `GetTask` / `DeleteTask` 返回 error —— 这些方法 nil read 是 wiring error。

### DeleteTask 保留 reports

注释：reports 保留为 standalone artifacts。删 task 不删其产生的报告，让历史报告可查。

### Title 默认从 period 生成

`CreateOneoffTaskAndRun` 的 `title` 空 → `TitleFor(kind, period, locale)`。让用户不必手填标题，自动从周期生成。

## 8. 注意事项

- **`u.read == nil` 检查**：`CreateOneoffTaskAndRun` / `RerunOneoffTask` / `GetTask` / `DeleteTask` 检查；`ListOneoffTasks` 容错返回 nil。scheduler-only 路径不调这些方法
- **`RerunOneoffTask` 用 UTC**：task 不存 tz。若需保留原 tz，应在 `model.Task` 加 `Timezone` 字段
- **`CreateOneoffTaskAndRun` 首跑 `prevFireAt = zero`**：`PeriodFor` 对 custom kind 首跑默认 trailing 24h。若 kind 是 daily/weekly/monthly，period 是 fireAt 的前一周期
- **`GenerateNow` 失败仍返回 task**：`CreateOneoffTaskAndRun` 即使 `GenerateNow` 失败也返回 task + error。调用方应检查 error 但 task 已存
- **`DeleteTask` 不删 reports**：报告 `task_id` dangles。SPA 渲染报告时不应依赖 `task_id` join task 表
- **`model.OneoffTaskRef(t.ID)`**：是 `"oneoff:<id>"` 格式。`ListReports` filter by `TaskID` 用此格式
- **`RerunOneoffTask` period 用当前窗口**：rerun 的 period 是 `now` 的前一周期，可能与原 run 不同。这是有意的 —— rerun 生成"当前"报告
- **`Status = "active"` 硬编码**：oneoff task 初始 active。无 `UpdateTaskStatus` 调用（接口存在但本文件未用）
