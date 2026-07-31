# report/store/read.go

## 1. 概述

本文件扩展 `Repo`（定义在 `repo.go`）的读取与删除能力，实现 `bizreport.ReadRepo` 接口。覆盖：报告列表过滤、报告删除、调度列表（含 owner scope）、调度删除、按 share token 取报告。

设计要点：
- **三态删除**：`DeleteReport` / `DeleteSchedule` 通过 `RowsAffected == 0` 区分「行不存在」并映射 `errs.ErrNotFound`。
- **owner scope**：`ListSchedules` 的 `all=false` 时按 `created_by = ownerID` 过滤，是非 admin 用户的权限裁剪边界。
- **share token 解析**：`GetReportByShareToken` 只查 token 对应行，TTL 由 biz 层校验（不在 data 层做时间判断）。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/report/store`
- **依赖方向**：`controlplane → biz/report → data/report/store → model/report + pkg/errs`；接口 `bizreport.ReadRepo` 在消费方 biz 层定义。

## 3. 关键类型与接口

本文件无类型定义，所有方法挂在 `Repo`（`repo.go` 中定义）上。

```go
// 编译期接口断言：Repo 同时满足 bizreport.ReadRepo
var _ bizreport.ReadRepo = (*Repo)(nil)

// 涉及的方法签名
func (r *Repo) ListReports(ctx context.Context, f bizreport.ReportFilter) ([]*model.Report, error)
func (r *Repo) DeleteReport(ctx context.Context, id string) error
func (r *Repo) ListSchedules(ctx context.Context, ownerID uint64, all bool) ([]*model.ReportSchedule, error)
func (r *Repo) DeleteSchedule(ctx context.Context, id uint64) error
func (r *Repo) GetReportByShareToken(ctx context.Context, token string) (*model.Report, error)
```

`bizreport.ReportFilter` 字段（隐含）：`Status`、`Kind`、`ScheduleID *uint64`、`TaskID string`、`Limit`、`Offset`。

## 4. 关键函数与流程

### `ListReports(ctx, f) ([]*model.Report, error)`

- **职责**：按过滤器返回报告列表，最新优先。
- **流程**：
  1. 基础 `Model(&model.Report{})`。
  2. 逐字段叠加 WHERE：
     - `f.Status != ""` → `status = ?`
     - `f.Kind != ""` → `kind = ?`
     - `f.ScheduleID != nil` → `schedule_id = ?`
     - `f.TaskID != ""` → `task_id = ?`（HLD-022 引入，按统一任务脊柱反查报告）
  3. `limit = f.Limit`；若 `<=0` 用 `bizreport.DefaultListLimit` 兜底。
  4. `Order("created_at DESC").Limit(limit).Offset(f.Offset).Find(&rows)`。
- **错误处理**：透传 GORM 错误。
- **设计权衡**：多个过滤条件用零值判断跳过，让单一接口支持灵活查询；分页用 limit+offset（数据量不大，无需 cursor）。

### `DeleteReport(ctx, id string) error`

- **职责**：删除报告行（物理删除）。
- **流程**：`Where("id = ?", id).Delete(&model.Report{})`。
- **错误处理**：
  - `res.Error` 透传。
  - `RowsAffected == 0` → `errs.ErrNotFound`。
- **注意**：`id` 是 string 类型（报告 id 非自增整型），调用方需保证非空。

### `ListSchedules(ctx, ownerID, all) ([]*model.ReportSchedule, error)`

- **职责**：返回调度列表；`all=false` 时仅返回 `ownerID` 自己的（非 admin scope）。
- **流程**：
  1. 基础 `Model(&model.ReportSchedule{})`。
  2. `!all` → `Where("created_by = ?", ownerID)`。
  3. `Order("created_at DESC").Find(&rows)`。
- **权限语义**：`all=true` 由上层 biz 层在确认 admin 角色后传入；data 层只负责按 flag 裁剪，不做角色判断。

### `DeleteSchedule(ctx, id uint64) error`

- **职责**：删除调度行。
- **流程**：`Where("id = ?", id).Delete(&model.ReportSchedule{})`。
- **错误处理**：同 `DeleteReport`——`RowsAffected == 0` → `errs.ErrNotFound`。
- **注意**：删调度不会自动停掉已派发的报告任务，biz 层需另行处理（如 cancel 对应 task）。

### `GetReportByShareToken(ctx, token) (*model.Report, error)`

- **职责**：按 share token 取报告（用于公开分享链接）。
- **流程**：`First(&rpt, "share_token = ?", token)`。
- **错误处理**：`gorm.ErrRecordNotFound` → `errs.ErrNotFound`；其余透传。
- **TTL 边界**：注释明确「TTL is enforced in the biz layer」——data 层不检查 token 是否过期，biz 层在调用前后校验时间窗。

## 5. 依赖关系

- **内部包**：
  - `github.com/ongridio/ongrid/internal/manager/biz/report`（bizreport）——`ReadRepo` 接口、`ReportFilter`、`DefaultListLimit` 常量。
  - `github.com/ongridio/ongrid/internal/manager/model/report`——`Report`、`ReportSchedule` 结构体。
  - `github.com/ongridio/ongrid/internal/pkg/errs`——`ErrNotFound` 标准错误。
- **外部库**：`gorm.io/gorm`（含 `ErrRecordNotFound` sentinel）。
- **标准库**：`context`、`errors`。
- **被调用方**：`biz/report` 的读取/删除 usecase。

## 6. 并发与资源管理

- **无共享状态**：`Repo` 仅持有不可变 `*gorm.DB`，可并发调用。
- **每次新 session**：`r.db.WithContext(ctx)` 隔离事务状态。
- **ctx 透传**：所有方法首参 `context.Context`。
- **无锁、无 channel、无缓存**。
- **无资源释放**：GORM 连接池管理。

## 7. 设计模式与亮点

- **三态删除统一模式**：`DeleteReport` / `DeleteSchedule` 用 `RowsAffected == 0` → `ErrNotFound`，与项目其他 repo（如 monitor/store）一致，便于上层统一错误处理。
- **owner scope 在 data 层落地**：`ListSchedules` 的 `all` 参数让权限裁剪下沉到 SQL，避免取全量再过滤的性能浪费。
- **share token 与 TTL 解耦**：data 层只管 token 查询，TTL 留给 biz 层；这是「接口单一职责」的体现——data 层不应感知业务时间规则。
- **零值跳过过滤**：`ListReports` 用零值判断决定是否加 WHERE，让单一方法支持任意子集组合查询，避免写多个变体方法。
- **DefaultListLimit 兜底**：调用方未传 limit 时用 biz 层常量兜底，防止无限制查询拖垮数据库。
- **编译期接口断言**：`var _ bizreport.ReadRepo = (*Repo)(nil)` 让接口变更在编译期暴露，避免运行时才发现未实现方法。

## 8. 注意事项

- **物理删除**：`DeleteReport` / `DeleteSchedule` 都是硬删除；如需审计追溯需在 biz 层先归档。
- **`id` 类型不一致**：`DeleteReport(id string)` vs `DeleteSchedule(id uint64)`——报告 id 是 string（可能是 ULID/UUID），调度 id 是自增整型；调用时别搞混。
- **share token 唯一性**：依赖 `share_token` 列有唯一索引；若重复 token 存在，`First` 只返回第一条，需在 model 层保证唯一约束。
- **TTL 校验位置**：`GetReportByShareToken` 不校验过期，biz 层必须自行校验，否则过期链接仍可访问。
- **ListSchedules 不分页**：直接 `Find` 全量；若调度数量增长需加分页。
- **ListReports offset 分页**：大数据量下 offset 性能差，目前数据量可接受；如成瓶颈改 cursor 分页。
- **TaskID 过滤依赖 HLD-022 迁移**：`task_id` 列是 HLD-022 Phase 2 新增，迁移完成前该过滤条件不会命中（旧数据 task_id 为空）。
- **跨方言**：所有查询方言无关，MySQL/SQLite 均可用。
