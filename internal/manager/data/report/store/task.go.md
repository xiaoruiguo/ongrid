# report/store/task.go

## 1. 概述

本文件扩展 `Repo`（定义在 `repo.go`），为 HLD-022 Phase 2 引入的**统一任务脊柱表**（`tasks`）提供持久化方法。**只存非周期性（oneoff）任务**——周期性任务是 `report_schedules` 的视图，在 biz 层 union 起来形成完整任务列表。

设计要点：
- **职责单一**：本文件只管 oneoff task 的 CRUD，不涉及调度逻辑（调度由 `report_schedules` + `DueSchedules` 处理）。
- **软删**：`DeleteTask` 走 GORM 软删（model 上有 `gorm.DeletedAt`），与 `reports` 表的 delete_marker 模式不同——本表用传统 `deleted_at`。
- **status 单字段更新**：`UpdateTaskStatus` 用 `Update("status", ...)` 只写一列，避免 `Save` 覆盖全字段。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/report/store`
- **依赖方向**：`controlplane → biz/report → data/report/store → model/report`；方法挂在 `Repo` 上，由 biz 层调用。
- **HLD 关联**：HLD-022 Phase 2 统一任务脊柱。

## 3. 关键类型与接口

本文件无类型/接口定义，所有方法挂在 `Repo`（`repo.go` 中定义）上。

```go
// 涉及的方法签名
func (r *Repo) CreateTask(ctx context.Context, t *model.Task) error
func (r *Repo) GetTask(ctx context.Context, id string) (*model.Task, error)
func (r *Repo) ListTasks(ctx context.Context) ([]*model.Task, error)
func (r *Repo) UpdateTaskStatus(ctx context.Context, id, status string) error
func (r *Repo) DeleteTask(ctx context.Context, id string) error
```

`model.Task` 结构体（在 `model/report` 中定义）：含 `id string`、`status string`、`created_at`、`deleted_at`（软删）等字段。

## 4. 关键函数与流程

### `CreateTask(ctx, t *model.Task) error`

- **职责**：插入 oneoff 任务行。
- **流程**：`r.db.WithContext(ctx).Create(t).Error`。
- **错误处理**：直接透传 GORM 错误（未做唯一索引冲突翻译，因为 task id 通常是 ULID/UUID，冲突概率极低）。
- **id 来源**：`t.ID` 应由调用方或 model 钩子预填（ULID/UUID），不依赖自增。

### `GetTask(ctx, id string) (*model.Task, error)`

- **职责**：按 id 取单行；软删行被 GORM 自动排除。
- **流程**：`r.db.WithContext(ctx).Where("id = ?", id).First(&t)`。
- **错误处理**：直接透传 GORM 错误（包括 `gorm.ErrRecordNotFound`）——**未翻译为 `errs.ErrNotFound`**，调用方需自行处理。
- **注意**：与其他 repo 方法（如 `GetReport`）不一致，后者会翻译为 `errs.ErrNotFound`；这里可能是疏漏或调用方依赖原始错误。

### `ListTasks(ctx) ([]*model.Task, error)`

- **职责**：返回所有 oneoff 任务，最新优先。
- **流程**：`r.db.WithContext(ctx).Order("created_at DESC").Find(&rows)`。
- **软删过滤**：GORM 自动加 `WHERE deleted_at IS NULL`。
- **不分页**：直接返回全量；若任务数量增长需加分页。
- **设计意图**：biz 层将此列表与 `report_schedules`（周期任务视图）union，形成完整任务列表展示给用户。

### `UpdateTaskStatus(ctx, id, status string) error`

- **职责**：更新任务状态（反映最近一次运行结果）。
- **流程**：`r.db.WithContext(ctx).Model(&model.Task{}).Where("id = ?", id).Update("status", status)`。
- **错误处理**：透传 GORM 错误；**未检查 `RowsAffected`**，因此 id 不存在时不会返回 `ErrNotFound`，而是静默无操作。
- **设计权衡**：用 `Update("status", ...)` 单列更新比 `Save` 更安全——不会覆盖其他字段（如 `created_at`、`deleted_at`）。

### `DeleteTask(ctx, id string) error`

- **职责**：软删任务行。
- **流程**：`r.db.WithContext(ctx).Where("id = ?", id).Delete(&model.Task{})`。
- **软删机制**：GORM 检测到 model 上的 `gorm.DeletedAt` 字段，自动改为 `UPDATE ... SET deleted_at = NOW() WHERE id = ? AND deleted_at IS NULL`。
- **错误处理**：透传 GORM 错误；**未检查 `RowsAffected`**，id 不存在时静默无操作。

## 5. 依赖关系

- **内部包**：
  - `github.com/ongridio/ongrid/internal/manager/model/report`——`Task` 结构体。
- **外部库**：无直接 import（`context` 是标准库，GORM 通过 `Repo.db` 间接使用）。
- **被调用方**：`biz/report` 的任务管理 usecase；调度器在派发 oneoff 任务时调 `CreateTask`，运行结束后调 `UpdateTaskStatus`。

## 6. 并发与资源管理

- **无共享状态**：`Repo` 仅持有不可变 `*gorm.DB`，可并发调用。
- **每次新 session**：`r.db.WithContext(ctx)` 隔离事务状态。
- **ctx 透传**：所有方法首参 `context.Context`。
- **无锁、无 channel、无缓存**。
- **无资源释放**：GORM 连接池管理。
- **并发 status 更新**：多个 worker 并发更新同一 task status 时，最后一个写赢；biz 层若需严格状态机（如 pending → running → done）应加乐观锁或 WHERE 守卫。

## 7. 设计模式与亮点

- **职责分离**：oneoff task 与 recurring schedule 分表存储，biz 层 union 展示，避免单表用 `kind` 字段区分带来的查询复杂度。
- **软删保留审计**：`DeleteTask` 软删而非物理删除，保留历史任务记录便于追溯。
- **单列更新**：`UpdateTaskStatus` 用 `Update("status", ...)` 只写一列，避免 `Save` 覆盖全字段的风险。
- **id 用 string**：与 `reports.id` 一致（ULID/UUID），适合分布式生成与外部引用。
- **极简实现**：本文件方法都很短，无额外抽象，符合「不过度工程化」原则。

## 8. 注意事项

- **`GetTask` 未翻译 `ErrNotFound`**：与 `GetReport` / `GetSchedule` 不一致，调用方需自行 `errors.Is(err, gorm.ErrRecordNotFound)` 处理；可能是疏漏，统一时应改成返回 `errs.ErrNotFound`。
- **`UpdateTaskStatus` / `DeleteTask` 未检查 `RowsAffected`**：id 不存在时静默无操作，调用方无法区分「成功」与「id 不存在」；如需严格语义应加 `RowsAffected == 0 → ErrNotFound` 检查。
- **软删 vs delete_marker**：本表用传统 `gorm.DeletedAt`（`deleted_at` 列），而 `reports` 表用 `delete_marker` 模式（见 `migrate.go`）；同一域内两种软删模式并存，需注意查询时 GORM 自动过滤行为差异。
- **`ListTasks` 不分页**：任务数量增长后会拖慢；如成瓶颈加 `Limit`/`Offset` 或 cursor 分页。
- **status 状态机**：data 层不校验 status 取值合法性，biz 层需保证只传入合法状态（如 `pending` / `running` / `done` / `failed`）。
- **HLD-022 迁移依赖**：`tasks` 表由 `migrate.go` 的 `AutoMigrate(&model.Task{})` 创建；迁移完成前调用本文件方法会表不存在报错。
- **跨方言**：所有查询方言无关；软删 `UPDATE ... SET deleted_at = ?` 在 MySQL/SQLite 均可用。
- **id 类型**：`GetTask(id string)` / `DeleteTask(id string)` / `UpdateTaskStatus(id string)`——id 是 string，调用时别传 uint64。
