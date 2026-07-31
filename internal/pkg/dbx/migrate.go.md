# `migrate.go` 技术实现文档

> 源文件：`internal/pkg/dbx/migrate.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/dbx`

## 1. 概述

该文件实现 ongrid 的 schema 迁移执行器：定义 `Migrator` 函数类型（每个数据层 package 暴露一个），并提供 `RunMigrations` 顺序调用每个 migrator，首个错误中止并返回带索引的包装错误。每个 migrator 的墙钟耗时被记录到日志，便于发现慢的 auto-migration。

## 2. 包信息

- **包名**：`dbx`
- **所属模块**：`internal/pkg/`（基础设施层）
- **依赖方向**：被 `cmd/ongrid` / `cmd/ongrid-edge` 启动时调用；依赖 `gorm.io/gorm` + 标准库。

## 3. 关键类型与接口

### `Migrator`
函数类型，典型实现为各 BC 数据层的 `Migrate(db *gorm.DB) error`，内部调用 `db.AutoMigrate(&User{}, ...)`。

```go
type Migrator func(db *gorm.DB) error
```

注释明确指出 `internal/iam/data/user/sqlite` 中的 `sqlite` 是历史命名，函数本身 dialect-agnostic，对 MySQL 同样有效。

## 4. 关键函数与流程

### `RunMigrations`
- **签名**：`func RunMigrations(db *gorm.DB, log *slog.Logger, migrators ...Migrator) error`
- **职责**：按顺序执行每个 migrator；首个错误中止并返回。
- **流程**：
  1. `db == nil` → error `nil db`。
  2. 遍历 migrators（index 从 0 开始）：
     - `m == nil` → error `migrator #i+1 is nil`（1-based 索引便于诊断）。
     - log 记录 `migration start` + index/total。
     - `start := time.Now()`。
     - `m(db)` 失败 → `%w` 包装 `migrator #i+1: <err>` 返回。
     - log 记录 `migration done` + index/elapsed。
- **错误处理**：每个错误都用 1-based 索引标注位置，方便在 migrators 列表中定位故障点。

## 5. 依赖关系

- **内部包**：无。
- **外部库**：`gorm.io/gorm` + 标准库 `errors` / `fmt` / `log/slog` / `time`。
- **被调用方**：`cmd/ongrid` / `cmd/ongrid-edge` 启动装配，配合 `dbx.Open` + 各 BC 的 `Migrate` 函数。

## 6. 并发与资源管理

无并发控制。migrators 严格顺序执行——这是有意为之，因为表之间可能存在外键依赖，并发迁移会触发 FK 检查失败。

## 7. 设计模式与亮点

- **可变参数 migrators**：`migrators ...Migrator` 让调用方在 `cmd/ongrid` 内联列出，顺序显式可控。
- **1-based 索引诊断**：错误信息中索引从 1 开始，对应运维视角的"第 N 个迁移"，避免 0-based 困惑。
- **墙钟日志**：每个 migrator 单独记录 elapsed，慢迁移一眼可见，便于性能排查。
- **fail-fast**：首个错误立即返回，避免后续 migrator 在脏 schema 上执行造成更大损坏。
- **nil 检查严格**：`db == nil` 与 `m == nil` 都显式报错，避免 panic 在 GORM 内部更难诊断。

## 8. 注意事项

- **顺序硬编码**：migrators 顺序由调用方决定，BC 间外键依赖需调用方自行理顺；新增 BC 时易因顺序错乱导致 FK 报错。
- **无事务包裹**：每个 migrator 内部 `AutoMigrate` 自身可能多语句，但跨 migrator 无统一事务；中途失败会留下半完成的 schema，重启后再次执行可能因"表已存在"等问题失败（GORM AutoMigrate 大部分情况是幂等的，但 index 重命名等不幂等操作需注意）。
- **无回滚**：失败后不自动回滚已执行的 migrator；运维需手工 SQL 修复或重置数据库。
- **日志依赖调用方**：`log == nil` 时静默执行，开发期忘了传 logger 会丢失 elapsed 信息。
- **AutoMigrate 局限继承**：本执行器不解决 AutoMigrate 的"不做字段删除 / 列类型变更 / 重命名"局限，需配合 `soft_delete.go` 中的 `DropIndexes` / `BackfillDeleteMarker` 等工具。
