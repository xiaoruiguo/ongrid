# setting/store/repo.go

## 1. 概述

本文件实现 `system_settings` 表的 GORM-backed 持久化。覆盖能力：按 (category, key) 读取、单行 upsert、批量 upsert（事务）、按 category 列表、删除。配置项以 `(category, key)` 复合业务键寻址，唯一索引保证 upsert 幂等。

设计要点：
- **复合业务键**：`(category, key)` 是寻址单位，`key` 是 SQL 保留字故用反引号转义。
- **跨方言原子 upsert**：`clause.OnConflict` 让 GORM 自动生成 MySQL `INSERT ... ON DUPLICATE KEY UPDATE` 或 SQLite `ON CONFLICT DO UPDATE`，无需写方言分支。
- **批量 upsert 事务化**：`SetBatch` 用 `Transaction` 包裹，要么全部成功要么全部回滚，保证配置元组一致性。
- **Set 后 reload**：`Set` 在 upsert 后调 `Get` 重新查询，保证返回的行有完整的 id/timestamps（部分驱动 update 路径不回填 id）。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/setting/store`
- **依赖方向**：`controlplane → biz/setting → data/setting/store → model/setting + pkg/errs`；接口在消费方 biz 层定义（本文件未显式写编译期断言，但 Repo 由 wire 注入 biz）。

## 3. 关键类型与接口

```go
// Repo 是 system_settings 的 GORM 持久化；并发安全（每次调用独立 session）。
type Repo struct {
    db *gorm.DB
}

func NewRepo(db *gorm.DB) *Repo

// 局部 helper（未导出）
func upsertSetting(db *gorm.DB, row *model.Setting) error
```

`model.Setting` 结构体字段（隐含）：`id uint64`、`category string`、`key string`、`value string`、`sensitive bool`、`created_at`、`updated_at`。

## 4. 关键函数与流程

### `NewRepo(db *gorm.DB) *Repo`

构造器，直接包装 `*gorm.DB`。

### `Get(ctx, category, key) (*model.Setting, error)`

- **职责**：按复合业务键取单行；缺失行返回 `(nil, errs.ErrNotFound)` 让调用方区分「不存在」与「空值」。
- **流程**：
  1. `category == "" || key == ""` → `errs.ErrInvalid` 包装「category/key required」。
  2. `Where("category = ? AND `key` = ?", category, key).First(&s)`——`key` 用反引号转义（SQL 保留字）。
  3. `gorm.ErrRecordNotFound` → `errs.ErrNotFound`；其余透传。
- **设计意图**：让调用方能区分「配置项未设置」与「配置项值为空字符串」，对配置语义很重要。

### `Set(ctx, category, key, value, sensitive) (*model.Setting, error)`

- **职责**：upsert 单行；缺失则插入，存在则更新 `value` + `sensitive`。
- **流程**：
  1. 参数校验同 `Get`。
  2. 构造 `row := model.Setting{Category, Key, Value, Sensitive}`。
  3. 调 `upsertSetting(r.db.WithContext(ctx), &row)`。
  4. 调 `r.Get(ctx, category, key)` 重新查询返回——保证 id/timestamps 完整（部分驱动 update 路径不回填 id）。
- **幂等性**：`(category, key)` 唯一索引 + `ON CONFLICT DO UPDATE` 保证重复调用结果一致。

### `SetBatch(ctx, settings []model.Setting) error`

- **职责**：事务化批量 upsert；要么全部成功要么全部回滚。
- **流程**：
  1. 空切片 → `errs.ErrInvalid` 包装「at least one setting required」。
  2. 逐项校验 `category` / `key` 非空。
  3. `r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { ... })`：
     - 遍历 `settings`，对每项调 `upsertSetting(tx, &row)`。
     - 任一失败 → `fmt.Errorf("upsert %s.%s: %w", ...)` 返回，事务回滚。
  4. 事务失败 → `fmt.Errorf("set settings transaction: %w", err)`。
- **设计意图**：配置往往是元组（如一组告警阈值需同时生效），部分写入会导致配置不一致；事务保证原子性。

### `upsertSetting(db, row) error`

- **职责**：跨方言原子 upsert 单行。
- **实现**：
  ```go
  db.Clauses(clause.OnConflict{
      Columns:   []clause.Column{{Name: "category"}, {Name: "key"}},
      DoUpdates: clause.AssignmentColumns([]string{"value", "sensitive", "updated_at"}),
  }).Create(row)
  ```
- **方言映射**：
  - MySQL → `INSERT ... ON DUPLICATE KEY UPDATE value=..., sensitive=..., updated_at=...`
  - SQLite → `INSERT ... ON CONFLICT(category, key) DO UPDATE SET value=..., sensitive=..., updated_at=...`
- **更新列**：`value`、`sensitive`、`updated_at`——**不更新 `created_at`**（保留首次插入时间）。

### `List(ctx, category) ([]*model.Setting, error)`

- **职责**：列出配置；`category=""` 返回全部，否则按 category 过滤。
- **流程**：`Model(&Setting{})` + 可选 `Where("category = ?", category)` + `Order("category asc").Order("`key` asc").Find(&out)`。
- **排序**：双键 `(category, key)` 升序，便于前端分组展示。

### `Delete(ctx, category, key) error`

- **职责**：删除单行；缺失行返回 `errs.ErrNotFound`。
- **流程**：
  1. 参数校验同 `Get`。
  2. `Where("category = ? AND `key` = ?", category, key).Delete(&model.Setting{})`。
  3. `RowsAffected == 0` → `errs.ErrNotFound`。
- **物理删除**：硬删除（model 无 `gorm.DeletedAt`）。

## 5. 依赖关系

- **内部包**：
  - `github.com/ongridio/ongrid/internal/manager/model/setting`——`Setting` 结构体。
  - `github.com/ongridio/ongrid/internal/pkg/errs`——`ErrInvalid`、`ErrNotFound` 标准错误。
- **外部库**：
  - `gorm.io/gorm`——ORM、`ErrRecordNotFound` sentinel、`Transaction`。
  - `gorm.io/gorm/clause`——`OnConflict`、`Column`、`AssignmentColumns`。
- **标准库**：`context`、`errors`、`fmt`。
- **被调用方**：`biz/setting` 的配置管理 usecase；多个 biz 域（alert、aiops、metric 等）通过 biz/setting 读写配置。

## 6. 并发与资源管理

- **无共享状态**：`Repo` 仅持有不可变 `*gorm.DB`，可并发调用。
- **每次新 session**：`r.db.WithContext(ctx)` 隔离事务状态。
- **ctx 透传**：所有方法首参 `context.Context`，符合 gospec 红线。
- **无锁、无 channel、无缓存**：配置缓存应在 biz 层（data 层只管读写）。
- **无资源释放**：GORM 连接池管理。
- **并发 Set 同键**：`ON CONFLICT DO UPDATE` 保证最后写赢；无乐观锁，调用方需自行处理并发冲突（如先 Get 比较再 Set）。

## 7. 设计模式与亮点

- **复合业务键 + 唯一索引**：`(category, key)` 是寻址单位，唯一索引既是查询索引也是 upsert 守卫。
- **跨方言 OnConflict**：`clause.OnConflict` 是 GORM 对 upsert 的方言无关抽象，避免手写 `ON DUPLICATE KEY UPDATE` / `ON CONFLICT DO UPDATE` 分支。
- **Set 后 reload**：`Set` 在 upsert 后调 `Get` 保证返回完整行——这是处理「部分驱动 update 路径不回填 id」的稳健模式。
- **SetBatch 事务化**：保证配置元组原子性，避免部分写入导致配置不一致。
- **三态删除**：`Delete` 用 `RowsAffected == 0` → `ErrNotFound`，与项目其他 repo 一致。
- **`key` 反引号转义**：`key` 是 SQL 保留字，用 `` `key` `` 转义；跨方言兼容（MySQL/SQLite 都支持反引号）。
- **更新列白名单**：`DoUpdates: clause.AssignmentColumns([]string{"value", "sensitive", "updated_at"})` 明确只更新这三列，不碰 `created_at` / `category` / `key`，防止误覆盖。
- **参数校验下沉**：每个方法都校验 `category` / `key` 非空，避免无效查询打到数据库。

## 8. 注意事项

- **`key` 反引号转义**：所有涉及 `key` 列的 SQL 都用反引号；若换 Postgres 需改双引号（Postgres 用 `"key"`）。
- **Set 不返回原始 row**：`Set` 内部构造的 `row` 被丢弃，最终返回 `Get` 的结果；调用方应使用返回值而非传入参数。
- **SetBatch 无返回值**：只返回 error，不返回 upsert 后的行；调用方若需完整行需另行查询。
- **物理删除**：`Delete` 是硬删除；如需审计配置变更历史应在 biz 层先归档。
- **并发 Set 不保证顺序**：两个并发 Set 同一键，最后写赢；无乐观锁，若需强一致应在 biz 层加版本号。
- **sensitive 标记不加密**：`sensitive bool` 只是标记，data 层不加密；敏感值的加密责任在 biz 层（如调 secret/store 存 sealed blob）。
- **无缓存**：data 层不缓存配置；biz 层若缓存需处理失效（如配置变更后 invalidate）。
- **List 全量返回**：`List` 不分页；配置数量通常不大，可接受，若增长需加分页。
- **跨方言**：所有查询方言无关；`clause.OnConflict` 自动适配 MySQL/SQLite。
- **`updated_at` 自动更新**：`DoUpdates` 显式包含 `updated_at`，确保 upsert 时更新该列；依赖模型层 `autoUpdateTime` tag。
