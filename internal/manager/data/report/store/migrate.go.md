# report/store/migrate.go

## 1. 概述

本文件实现 report 子域（`report_schedules` + `reports` + 统一任务脊柱表 `tasks`）的 schema 迁移。被 `cmd/ongrid/main.go` 启动期迁移列表调用，遵循 gospec「expand-contract / 滚动发布兼容」原则——只增不删、可重复执行、对存量数据做幂等 backfill。

设计要点：
- **delete_marker 软删迁移**：在 `reports` 表上执行 dbx 标准 `NeedsDeleteMarkerMigration → DropIndexes → AutoMigrate → BackfillDeleteMarker` 四步流。
- **HLD-022 Phase 2 统一任务脊柱**：新增 `tasks` 表与 `reports.task_id` 列，承载 oneoff 任务。
- **HLD-022 backfill**：用 `UPDATE reports SET task_id = CONCAT('report-schedule:', schedule_id)` 为存量行回填 owner-task 反向引用，幂等且 additive。

## 2. 包信息

- **包名**：`store`（report sub-domain data layer，对应 HLD-014）
- **所属模块**：`internal/manager/data/report/store`
- **依赖方向**：`cmd → data/report/store → model/report + pkg/dbx`，符合单服务分层。

## 3. 关键类型与接口

无类型/接口定义，仅一个导出函数。

```go
func Migrate(db *gorm.DB) error
```

涉及的对象模型（在 `model/report` 中定义）：
```go
&model.ReportSchedule{} // 周期性调度配置
&model.Report{}         // 单次生成的报告实例
&model.Task{}           // HLD-022 Phase 2 统一任务脊柱（oneoff 行）
```

被 DropIndexes 显式删除的索引名：
- `uniq_report_sched_period`
- `idx_report_share`

## 4. 关键函数与流程

### `Migrate(db *gorm.DB) error`

- **职责**：注册 report 三张表到 AutoMigrate，并执行两次幂等 backfill。
- **流程**：
  1. **delete_marker 迁移前置**：
     - `dbx.NeedsDeleteMarkerMigration(db, Report{}.TableName())` 检测 `reports` 表是否还在用旧的 `deleted_at` 软删模式。
     - 若需要迁移，先 `dbx.DropIndexes(db, &model.Report{}, "uniq_report_sched_period", "idx_report_share")` 删除两个含 `deleted_at` 的旧复合索引（这些索引在新软删模式下不再适用，留着会冲突）。
  2. **AutoMigrate**：调用 `db.AutoMigrate(&ReportSchedule{}, &Report{}, &Task{})`。GORM 自动加新列/新索引，不动既有列。`Task` 是 HLD-022 Phase 2 引入的统一任务脊柱表（承载 oneoff 任务行）。
  3. **delete_marker backfill**：`dbx.BackfillDeleteMarkerWithValue(db, Report{}.TableName(), "1")`——给 `reports` 表存量行的 `delete_marker` 列填 `"1"`（表示「未删除」，与其他表默认填 `0` 不同，本表用字符串 `"1"` 表示存活）。
  4. **HLD-022 backfill（owner-task 反向引用）**：执行
     ```sql
     UPDATE reports
       SET task_id = CONCAT('report-schedule:', schedule_id)
     WHERE schedule_id IS NOT NULL
       AND (task_id IS NULL OR task_id = '')
     ```
     - **幂等**：`WHERE task_id IS NULL OR task_id = ''` 保证只填空值，重跑无副作用。
     - **additive**：`task_id` 是新增列，回填只是补默认值。
- **错误处理**：每步错误立即返回，中断迁移；上层 `dbx.RunMigrations` 决定是否中止启动。

## 5. 依赖关系

- **内部包**：
  - `github.com/ongridio/ongrid/internal/manager/model/report`——`ReportSchedule`、`Report`、`Task` 结构体（表 schema 来源）。
  - `github.com/ongridio/ongrid/internal/pkg/dbx`——`NeedsDeleteMarkerMigration`、`DropIndexes`、`BackfillDeleteMarkerWithValue` 三个迁移辅助函数。
- **外部库**：`gorm.io/gorm`——`*gorm.DB`、`AutoMigrate`、`Exec`。
- **被调用方**：`cmd/ongrid/main.go` 启动期迁移编排。

## 6. 并发与资源管理

- **无并发状态**：迁移在启动期单线程顺序执行，无锁、无 channel、无缓存。
- **无 ctx**：迁移函数签名不带 `context.Context`（启动期同步执行，无需取消）。
- **无资源释放**：不开游标、不持连接，连接池由 GORM 管理。

## 7. 设计模式与亮点

- **delete_marker 软删迁移四步法**：`检测 → 删旧索引 → AutoMigrate → backfill 默认值`，是 dbx 推荐的从 `deleted_at` 切换到 `delete_marker` 的标准模式；本文件是该模式的最小化使用范例。
- **BackfillDeleteMarkerWithValue("1")**：本表用 `"1"` 而非默认 `0` 表示「未删除」，与其他表语义相反；这是历史 schema 决策，迁移时必须尊重既有约定。
- **HLD-022 双重幂等**：`task_id IS NULL OR task_id = ''` 守卫保证 backfill 可重复执行；`additive`（仅填新列空值）保证滚动发布期间新旧版本代码都能工作。
- **expand-contract 落地**：本迁移只新增列与新表，不删旧字段，旧版本服务在滚动升级期间仍可读写（即便它不理解 `task_id`）。
- **索引前置清理**：含 `deleted_at` 的旧复合索引若不先删，AutoMigrate 加新索引时会因列名冲突失败；DropIndexes 在 AutoMigrate 之前是正确顺序。

## 8. 注意事项

- **`BackfillDeleteMarkerWithValue("1")` 是本表特殊点**：不要复制粘贴到其他表迁移；多数表用默认 `0` 表示未删除。
- **`task_id` 反向引用格式 `"report-schedule:<schedule_id>"`**：上游 biz 层依赖这个前缀格式区分任务来源；改格式需同步所有消费方。
- **`uniq_report_sched_period` 与 `idx_report_share` 被删**：迁移后这两个旧索引不再存在；如果业务层 SQL 显式 hint 这两个索引名会失败。AutoMigrate 不会自动重建等价新索引——若新 schema 模型里定义了等价索引，AutoMigrate 会加回去，但名字可能不同。
- **滚动发布兼容窗口**：`task_id` 新增列期间，旧版本代码不写该列，新版本 backfill 后会填值；旧版本读取时忽略该列，安全。
- **失败处理**：任何一步失败都会让进程启动失败（迁移是启动期阻塞步骤），符合「服务不应带着半套 schema 跑」红线。
- **重复执行安全**：所有步骤均幂等，每次启动都跑没问题。
- **不删列**：本迁移不删除任何旧列；如果将来要下线 `deleted_at`，必须走独立 contract 阶段（先停写 → 等所有节点升级 → 删列）。
