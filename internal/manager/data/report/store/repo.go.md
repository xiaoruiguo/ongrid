# report/store/repo.go

## 1. 概述

本文件定义 `Repo`——`report_schedules` + `reports` 两张表的 GORM-backed 持久化实现，实现 `bizreport.Repo` 接口。覆盖核心写入与读取能力：报告 CRUD、调度 CRUD、到期调度查询。`read.go` 在此基础上扩展读取与删除方法，`task.go` 扩展任务脊柱相关方法。

设计要点：
- **唯一索引冲突转语义错误**：`CreateReport` 把 `(schedule_id, period_start)` 唯一索引冲突翻译为 `errs.ErrConflict`，让 Usecase 把「重复调度触发」当 no-op 处理，而非报错。
- **DueSchedules 按 next_fire_at 排序**：让最过期的调度先 fire，避免堆积。
- **跨方言 duplicate key 检测**：`isDuplicateKey` 用字符串匹配识别 MySQL `Error 1062` 与 SQLite `UNIQUE constraint failed`，**故意复制**以避免跨域 import。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/report/store`
- **依赖方向**：`controlplane → biz/report → data/report/store → model/report + pkg/errs`；接口 `bizreport.Repo` 在消费方 biz 层定义，编译期断言 `var _ bizreport.Repo = (*Repo)(nil)`。

## 3. 关键类型与接口

```go
// Repo 是 report_schedules + reports 的 GORM 持久化实现。
type Repo struct {
    db *gorm.DB
}

func NewRepo(db *gorm.DB) *Repo

// 编译期接口断言
var _ bizreport.Repo = (*Repo)(nil)

// 跨方言 duplicate key 检测（局部 helper，故意复制避免跨域 import）
func isDuplicateKey(err error) bool
```

## 4. 关键函数与流程

### `NewRepo(db *gorm.DB) *Repo`

构造器，直接包装 `*gorm.DB`。

### `CreateReport(ctx, rpt *model.Report) error`

- **职责**：插入报告行。
- **流程**：
  1. `r.db.WithContext(ctx).Create(rpt)`。
  2. 错误检测：若 `isDuplicateKey(err)` → 返回 `errs.ErrConflict`；否则透传。
- **设计意图**：`(schedule_id, period_start)` 唯一索引保证同一调度同一周期不会生成两份报告；调度器重试触发时 Usecase 收到 `ErrConflict` 即当作 no-op 跳过，避免重复生成。
- **注意**：`rpt` 的 id 应由调用方或 model 钩子预填（如 ULID/UUID），不依赖自增。

### `GetReport(ctx, id string) (*model.Report, error)`

- **职责**：按 id 取报告。
- **流程**：`First(&rpt, "id = ?", id)`；`gorm.ErrRecordNotFound` → `errs.ErrNotFound`。
- **id 类型**：string（与 `read.go` 的 `DeleteReport` 一致）。

### `UpdateReport(ctx, rpt *model.Report) error`

- **职责**：全量保存报告（GORM `Save` 会写所有字段）。
- **流程**：`r.db.WithContext(ctx).Save(rpt)`。
- **注意**：`Save` 会覆盖所有列，包括零值字段；调用方需先 `GetReport` 拿到完整对象再改字段，避免误清空。

### `CreateSchedule(ctx, s *model.ReportSchedule) error`

- **职责**：插入调度行。
- **流程**：`r.db.WithContext(ctx).Create(s)`。
- **注意**：无唯一索引冲突翻译（调度本身允许同 owner 创建多个相似调度）。

### `GetSchedule(ctx, id uint64) (*model.ReportSchedule, error)`

- **职责**：按 id 取调度。
- **流程**：`First(&s, "id = ?", id)`；`gorm.ErrRecordNotFound` → `errs.ErrNotFound`。
- **id 类型**：uint64（自增整型，与 `read.go` 的 `DeleteSchedule` 一致）。

### `UpdateSchedule(ctx, s *model.ReportSchedule) error`

- **职责**：全量保存调度。
- **流程**：`r.db.WithContext(ctx).Save(s)`。
- **注意**：同 `UpdateReport`，`Save` 覆盖所有列。

### `DueSchedules(ctx, now time.Time) ([]*model.ReportSchedule, error)`

- **职责**：返回已启用且 `next_fire_at <= now` 的调度，按 `next_fire_at ASC` 排序（最过期先 fire）。
- **流程**：
  ```go
  r.db.WithContext(ctx).
      Where("enabled = ? AND next_fire_at IS NOT NULL AND next_fire_at <= ?", true, now).
      Order("next_fire_at ASC").
      Find(&rows)
  ```
- **设计意图**：
  - `enabled = true` 跳过被禁用的调度。
  - `next_fire_at IS NOT NULL` 排除尚未排程的调度。
  - `next_fire_at <= now` 取所有到期。
  - 按 `next_fire_at ASC` 排序——堆积时最过期的先处理，减少延迟偏差。
- **被调用方**：调度器（scheduler tick）周期性调用，取出后逐个生成报告并更新 `next_fire_at`。

### `isDuplicateKey(err error) bool`

- **职责**：跨方言检测唯一索引冲突。
- **实现**：`strings.Contains` 匹配三个 marker：
  - `"Error 1062"`（MySQL `ER_DUP_ENTRY`）
  - `"UNIQUE constraint failed"`（SQLite）
  - `"duplicate key"`（Postgres 通用，预留）
- **设计权衡**：注释明确「mirrors the alert store helper — duplicated to avoid a cross-domain import」——故意复制而非抽到共享包，因为 `internal/<domain>` 之间禁止直接 import（gospec monorepo 红线）。

## 5. 依赖关系

- **内部包**：
  - `github.com/ongridio/ongrid/internal/manager/biz/report`（bizreport）——`Repo` 接口。
  - `github.com/ongridio/ongrid/internal/manager/model/report`——`Report`、`ReportSchedule` 结构体。
  - `github.com/ongridio/ongrid/internal/pkg/errs`——`ErrConflict`、`ErrNotFound` 标准错误。
- **外部库**：`gorm.io/gorm`（含 `ErrRecordNotFound` sentinel）。
- **标准库**：`context`、`errors`、`strings`、`time`。
- **被调用方**：`biz/report` 的报告生成与调度 usecase。

## 6. 并发与资源管理

- **无共享状态**：`Repo` 仅持有不可变 `*gorm.DB`，可并发调用。
- **每次新 session**：`r.db.WithContext(ctx)` 隔离事务状态。
- **ctx 透传**：所有方法首参 `context.Context`。
- **无锁、无 channel、无缓存**。
- **无资源释放**：GORM 连接池管理。
- **调度并发**：多个 scheduler 实例并发跑 `DueSchedules` 可能取到相同行；依赖 `CreateReport` 的唯一索引冲突保护避免重复生成（冲突 → `ErrConflict` → no-op）。

## 7. 设计模式与亮点

- **唯一索引 + 冲突翻译 = 幂等调度触发**：`(schedule_id, period_start)` 唯一索引是物理屏障，`CreateReport` 把冲突翻译为 `ErrConflict` 让 Usecase 优雅处理，是「让数据库做并发守卫」的最佳实践。
- **跨方言错误检测故意复制**：`isDuplicateKey` 在 alert store 也有副本，注释明确说明是 monorepo 红线下的妥协；如需统一应抽到 `internal/pkg/dbx`。
- **DueSchedules 排序策略**：`next_fire_at ASC` 让最过期先处理，减少调度延迟累积。
- **编译期接口断言**：`var _ bizreport.Repo = (*Repo)(nil)` 让接口变更编译期暴露。
- **Save 全量更新的取舍**：`UpdateReport` / `UpdateSchedule` 用 `Save` 而非 `Updates(map)`，简化调用方（直接传 model 对象），代价是零值字段也会被写入——调用方需先 Get 再改。
- **id 类型分流**：报告 id 用 string（ULID/UUID 适合分享 token 场景），调度 id 用 uint64（自增，内部引用），是合理的类型选择。

## 8. 注意事项

- **`Save` 覆盖所有列**：调用 `UpdateReport` / `UpdateSchedule` 前必须先 `GetReport` / `GetSchedule` 拿完整对象，否则零值字段会被写入数据库。
- **`isDuplicateKey` 用字符串匹配**：依赖数据库错误消息文本，跨版本/跨语言环境可能失效；如 MySQL 改了错误消息格式需同步更新。更稳健的做法是用 `errors.As` 检测特定错误类型，但 GORM 对 SQLite 的错误包装不统一，字符串匹配是当前可行方案。
- **故意复制 helper**：`isDuplicateKey` 在 alert store 也有副本，改一处别忘同步另一处；如需统一应抽到 `internal/pkg/dbx`（但当前 gospec monorepo 规则下跨域 import 受限）。
- **DueSchedules 并发安全**：多实例 scheduler 并发调用会取到相同行，依赖 `CreateReport` 唯一索引兜底；若调度本身有副作用（如发邮件），需在 biz 层加分布式锁。
- **`next_fire_at` 更新时机**：调用方生成报告后必须更新 `next_fire_at`（通常 = 下一个周期），否则下次 `DueSchedules` 会重复取到该调度。
- **`enabled = true` 过滤**：禁用调度不会被取到，但已派发的报告不会被回收；biz 层需自行决定是否取消进行中的任务。
- **跨方言**：所有查询方言无关；`isDuplicateKey` 同时覆盖 MySQL 与 SQLite marker。
- **id 类型一致性**：本文件 `GetReport(id string)` 与 `read.go` `DeleteReport(id string)` 一致；`GetSchedule(id uint64)` 与 `read.go` `DeleteSchedule(id uint64)` 一致；调用方需注意类型别搞混。
