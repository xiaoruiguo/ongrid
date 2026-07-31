# `soft_delete.go` 技术实现文档

> 源文件：`internal/pkg/dbx/soft_delete.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/dbx`

## 1. 概述

该文件提供 GORM `AutoMigrate` 时代遗留的索引 / 软删除辅助工具：删除已存在的命名索引、检测是否需要 `delete_marker` 迁移、回填历史软删除行。所有 SQL 标识符与值表达式都经过白名单校验后再 quote，防止 SQL 注入。文件本身不依赖任何 dialect，由 GORM `Migrator()` 抽象层屏蔽差异。

## 2. 包信息

- **包名**：`dbx`
- **所属模块**：`internal/pkg/`（基础设施层）
- **依赖方向**：被各 BC 数据层 migration 代码调用；依赖 `gorm.io/gorm` + 标准库。

## 3. 关键类型与接口

无显著类型定义（仅顶层函数）。

## 4. 关键函数与流程

### `DropIndexes`
- **签名**：`func DropIndexes(db *gorm.DB, model any, names ...string) error`
- **职责**：删除已存在的命名索引。用于 AutoMigrate 不会重写同名索引的场景。
- **流程**：
  1. nil db / nil model → error。
  2. 遍历 names：空串 → error。
  3. `db.Migrator().HasIndex(model, name)` 为真才 `DropIndex`。
  4. Drop 失败 → `%w` 包装 `drop <name>` 错误。
- **错误处理**：任一索引操作失败立即返回，剩余索引不执行。

### `NeedsDeleteMarkerMigration`
- **签名**：`func NeedsDeleteMarkerMigration(db *gorm.DB, table string) bool`
- **职责**：判断遗留表是否需要 `delete_marker` 列迁移。
- **流程**：`db == nil` 或表不存在 → false；表存在但缺 `delete_marker` 列 → true。

### `BackfillDeleteMarker`
- **签名**：`func BackfillDeleteMarker(db *gorm.DB, table string) error`
- **职责**：把遗留软删除行挪出 `delete_marker=0` 槽位。委托给 `BackfillDeleteMarkerWithValue(db, table, "id")`。

### `BackfillDeleteMarkerWithValue`
- **签名**：`func BackfillDeleteMarkerWithValue(db *gorm.DB, table, valueExpr string) error`
- **职责**：用显式 SQL 值表达式回填 delete_marker。
- **流程**：
  1. nil db → error。
  2. 表不存在 / 缺 `delete_marker` 列 / 缺 `deleted_at` 列 → 静默返回 nil（幂等）。
  3. `quoteIdentifier(table)` 校验表名只含 `[a-zA-Z0-9_]`。
  4. `quoteValueExpr(valueExpr)` 白名单：仅接受 `"1"`（数值常量，用于"legacy 唯一性已保证至多一行删除"的表）或 `"id"`（用主键 id 作为 delete_marker 值，用于数值主键表）。
  5. 构造 `UPDATE <table> SET \`delete_marker\` = <expr> WHERE \`deleted_at\` IS NOT NULL AND \`delete_marker\` = 0` 并 `db.Exec`。
- **错误处理**：执行失败用 `%w` 包装。

### `quoteIdentifier`（私有）
- **签名**：`func quoteIdentifier(name string) (string, error)`
- **职责**：白名单校验标识符只含字母数字下划线，再包裹反引号。
- **错误处理**：含其他字符或空 → error `unsafe identifier`。

### `quoteValueExpr`（私有）
- **签名**：`func quoteValueExpr(expr string) (string, error)`
- **职责**：仅允许 `"1"` 或 `"id"` 两种值表达式；`"id"` 转为 `` `id` ``。
- **错误处理**：其他值 → error `unsafe delete marker expression`。

## 5. 依赖关系

- **内部包**：无。
- **外部库**：`gorm.io/gorm` + 标准库 `fmt`。
- **被调用方**：各 BC 数据层 migration 代码（如 iam.user、manager.edge）在引入 `delete_marker` 列时调用。

## 6. 并发与资源管理

无并发控制。所有操作通过 GORM `Migrator()` 与 `db.Exec` 同步执行，并发安全由底层连接池保证。调用方应确保 migration 期间无并发写入目标表（通常 migration 在启动时单线程执行）。

## 7. 设计模式与亮点

- **白名单防注入**：`quoteIdentifier` 与 `quoteValueExpr` 双重校验，禁止任何非预期字符进入 SQL，符合 gospec "SQL 全部参数化，禁止字符串拼接" 红线。表名因 GORM `Migrator()` 不接受参数化而必须拼接，故用白名单替代。
- **幂等设计**：`NeedsDeleteMarkerMigration` + `BackfillDeleteMarker` 多次执行安全——表不存在 / 列不存在 / 已回填均静默返回 nil。
- **双值策略**：`"id"` vs `"1"` 区分两种表语义——数值主键表用 id 保证每行 delete_marker 唯一；legacy 唯一性已约束至多一行删除的表用 1 即可。
- **委托模式**：`BackfillDeleteMarker` 委托给 `BackfillDeleteMarkerWithValue`，提供默认值 `"id"`，简化常见调用。

## 8. 注意事项

- **白名单局限**：`quoteIdentifier` 仅允许字母数字下划线，不含点号；带 schema 前缀（如 `mydb.users`）的表名会被拒，需先 split。
- **valueExpr 仅两种**：新增策略需修改 `quoteValueExpr`；不要直接绕过该校验拼接 SQL。
- **回填不修复唯一索引**：回填仅更新 `delete_marker` 值，若遗留数据已导致唯一索引冲突，需先 `DropIndexes` 重建。
- **UPDATE 无 LIMIT**：`UPDATE ... WHERE ...` 一次性更新全表匹配行，超大表可能锁表过久；生产大表应改用分批 UPDATE。
- **依赖 `deleted_at` 列存在**：表必须有 `deleted_at` 列才能回填；新增的纯 `delete_marker` 表（无 `deleted_at`）走不到回填逻辑。
- **MySQL 与 SQLite 行为差异**：`UPDATE` 的锁行为 / 隔离级别在两个 dialect 下不同，但本函数语义幂等，重跑安全。
