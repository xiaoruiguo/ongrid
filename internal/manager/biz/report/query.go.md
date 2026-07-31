# query.go

## 1. 概述

`query.go` 实现 report 包的 API 读路径 + schedule 管理方法 —— `Usecase` 上的 `ListReports` / `GetReport` / `DeleteReport` / `ListSchedules` / `GetSchedule` / `UpdateSchedule` / `SetScheduleEnabled` / `DeleteSchedule` / `RunNow` / `ShareReport` / `GetSharedReport` 等方法。

`ReadRepo` 接口是 scheduler-only `Repo` 之外的读+删面，PR-4 API 路径通过 `WithReadRepo` 注入。`ShareTTL = 30 天` 控制分享链接有效期。

## 2. 包信息

- 包名：`report`
- 路径：`internal/manager/biz/report`

## 3. 关键类型与接口

### ReportFilter

```go
type ReportFilter struct {
    Status     string
    Kind       string
    ScheduleID *uint64
    TaskID     string  // HLD-022
    Limit      int     // 0 → DefaultListLimit
    Offset     int
}
```

### 常量

```go
const DefaultListLimit = 50
const ShareTTL = 30 * 24 * time.Hour
```

### ReadRepo 接口

```go
type ReadRepo interface {
    ListReports(ctx, f ReportFilter) ([]*model.Report, error)
    DeleteReport(ctx, id string) error
    ListSchedules(ctx, ownerID uint64, all bool) ([]*model.ReportSchedule, error)
    DeleteSchedule(ctx, id uint64) error
    GetReportByShareToken(ctx, token string) (*model.Report, error)

    // HLD-022 Phase 2 — unified-task spine (oneoff rows)
    CreateTask(ctx, t *model.Task) error
    GetTask(ctx, id string) (*model.Task, error)
    ListTasks(ctx) ([]*model.Task, error)
    UpdateTaskStatus(ctx, id, status string) error
    DeleteTask(ctx, id string) error
}
```

注释：split out so scheduler-only Repo (PR-1) stays minimal。

## 4. 关键函数与流程

### WithReadRepo

```go
func (u *Usecase) WithReadRepo(rr ReadRepo) *Usecase
```

Builder 链。scheduler 路径（PR-1/3）不需要；API 路径（PR-4）需要。

### ListReports / GetReport / DeleteReport

`ListReports` clamp `Limit` 到 `[1, 200]`，0 或 > 200 用 `DefaultListLimit`。`GetReport` 委派 `repo`（scheduler 也用）。`DeleteReport` 委派 `read`（仅 API）。

### ListSchedules / GetSchedule / UpdateSchedule / SetScheduleEnabled / DeleteSchedule

- `ListSchedules(ctx, ownerID, all)`：`all=false` 只返回 owner 的（非 admin 调用方）
- `GetSchedule` 委派 `repo`
- `UpdateSchedule`：re-validate + re-arm `next_fire_at`（cron/timezone 改了）。`now` anchor re-arm
- `SetScheduleEnabled`：disable 清 `next_fire_at`；enable 从 `now` re-arm
- `DeleteSchedule`：委派 `read`。注释：existing reports 保留（`schedule_id` dangles，harmless）

### RunNow

```go
func (u *Usecase) RunNow(ctx, scheduleID uint64, locale string, now time.Time) (*model.Report, error)
```

从 schedule config 立即生成报告，不打扰 `next_fire_at`。报告作为 manual（`nil schedule_id`）创建，永不与 scheduled window 的 dedup key 冲突。注释：run-now reports 属于 schedule 的 task（`ScheduleID` nil 但 `TaskID = "report-schedule:<id>"`）。

### ShareReport / GetSharedReport

- `ShareReport`：mint 30 天 share token（`randomToken` 32-char hex）+ 持久化
- `GetSharedReport`：按 token 解析 + TTL 检查。过期返回 `errExpiredShare`（`ErrNotFound` join）

### randomToken

```go
func randomToken() string {
    b := make([]byte, 16)
    _, _ = rand.Read(b)
    return hex.EncodeToString(b)
}
```

16 随机字节 → 32-char hex。注释：`_, _ = rand.Read(b)` 忽略 error —— `crypto/rand.Reader` 在正常系统不会失败；若失败 `b` 全零，token 仍唯一性低但不会 panic。

## 5. 依赖关系

### 外部包

- `context` / `crypto/rand` / `encoding/hex` / `fmt` / `time`

### 内部包

- `model "github.com/ongridio/ongrid/internal/manager/model/report"` —— `Report` / `ReportSchedule` / `Task`
- 同包：`Usecase` / `Repo` / `CronSpecForKind` / `CronNext` / `PeriodFor` / `TitleFor` / `GenerateNow` / `loadLocation`

### 被谁调用

- HTTP handler（`/v1/reports/*`）调所有公开方法
- `task.go` 调 `GenerateNow`

## 6. 并发与资源管理

- 无锁、无 goroutine，纯同步方法
- `Usecase` 无共享可变状态；并发安全由 repo 保证
- `ShareReport` 用 `crypto/rand` 生成 token，无锁竞争

## 7. 设计模式与亮点

### ReadRepo split out

`ReadRepo` 是 scheduler-only `Repo` 之外的读+删面。注释：split out so scheduler-only Repo (PR-1) stays minimal。这让 scheduler 注入只需最小接口，API 路径单独注入 `ReadRepo`。

### RunNow 不打扰 next_fire_at

`RunNow` 从 schedule config 生成报告，但 `ScheduleID = nil` 避免与 scheduled window dedup 冲突。注释：run-now reports 属于 schedule 的 task（`TaskID` 标记）。

### ShareTTL 30 天

`ShareTTL = 30 * 24 * time.Hour`。分享链接 30 天有效。`GetSharedReport` TTL 检查返回 `errExpiredShare`（`ErrNotFound` join），让 HTTP 返回 404。

### UpdateSchedule re-arm

`UpdateSchedule` re-validate + re-arm `next_fire_at`。注释：cron/timezone 改了需 re-arm。`now` anchor 注入让测试 deterministic。

### SetScheduleEnabled 双向

`SetScheduleEnabled` disable 清 `next_fire_at`；enable 从 `now` re-arm。让 disable 后 enable 不立即补跑漏掉的窗口。

### DeleteSchedule 保留 reports

注释：existing reports 保留，`schedule_id` dangles harmless —— artifact stands alone。这让删 schedule 不丢历史报告。

### ListSchedules owner scoping

`ListSchedules(ctx, ownerID, all)`：`all=false` 只返回 owner 的。非 admin 调用方传 `all=false`。

### Limit clamp

`ListReports` clamp `Limit` 到 `[1, 200]`。防超大 limit 打爆 DB。

## 8. 注意事项

- **`WithReadRepo` 必须在 API 路径调用前**：scheduler 路径不调，`u.read` 为 nil。`ListReports` 等方法若 `u.read == nil` 会 panic
- **`randomToken` 忽略 error**：`_, _ = rand.Read(b)`。gospec 红线"禁止 `_ = fn()` 忽略错误"—— 这里是有意的：`crypto/rand.Reader` 正常系统不失败，失败时 `b` 全零 token 仍唯一性低但不 panic。应加注释说明（注释解释了原因）
- **`ShareTTL` 30 天硬编码**：不可配。若需调整需改常量
- **`errExpiredShare` 是 `ErrNotFound` join**：HTTP handler `errors.Is(err, ErrNotFound)` 成立，返回 404
- **`RunNow` 用 `s.CreatedBy`**：run-now 报告的 `createdBy` 是 schedule 的创建者，非当前调用者。这是有意的 —— 报告归属 schedule owner
- **`UpdateSchedule` `now` 注入**：测试可传固定时间。生产传 `time.Now().UTC()`
- **`DeleteSchedule` 不删 reports**：`schedule_id` dangles。SPA 渲染报告时不应依赖 `schedule_id` join schedule 表
- **`ListReports` `TaskID` filter**：HLD-022 新增。让任务详情页能列该任务的所有报告
