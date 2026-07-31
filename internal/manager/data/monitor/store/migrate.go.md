# monitor/store/migrate.go

## 1. 概述

本文件实现 `monitor_panels` 表的 schema 迁移逻辑。仅暴露一个 `Migrate` 函数，被 `cmd/ongrid/main.go` 的启动期迁移列表（经 `dbx.RunMigrations` 调度）调用。设计遵循 gospec「expand-contract / 滚动发布兼容」原则：只调用 GORM 的 `AutoMigrate`，**只增不删**——新增列/索引安全，绝不删除或收窄既有列，因此每次启动重复执行是幂等且安全的。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/monitor/store`（data 层）
- **依赖方向**：`cmd → data/monitor/store → model/monitor → gorm`，符合 `cmd → web → controlplane → repo → model` 单服务分层。

## 3. 关键类型与接口

本文件无类型/接口/常量定义，仅一个函数。

```go
// 函数签名
func Migrate(db *gorm.DB) error
```

## 4. 关键函数与流程

### `Migrate(db *gorm.DB) error`

- **职责**：注册 `monitor_panels` 表到 GORM AutoMigrate。
- **流程**：
  1. 调用 `db.AutoMigrate(&model.Panel{})`，GORM 比对数据库实际 schema 与 `model.Panel` 结构体，差异处自动 `ALTER TABLE ADD COLUMN / ADD INDEX`。
  2. 返回错误（若有）。
- **错误处理**：直接透传 `AutoMigrate` 的错误，由上层 `dbx.RunMigrations` 决定是否中止启动。

## 5. 依赖关系

- **内部包**：
  - `github.com/ongridio/ongrid/internal/manager/model/monitor`——`Panel` 结构体（表 schema 来源）。
- **外部库**：`gorm.io/gorm`——`*gorm.DB` 入参。
- **被调用方**：`cmd/ongrid/main.go` 的迁移编排（同 setting/store 等约定一致）。

## 6. 并发与资源管理

- 无并发状态，无锁、无 channel、无缓存。
- 无 `context.Context`（迁移在启动期单线程顺序执行，无需取消）。
- 不持有任何需要在退出时释放的资源。

## 7. 设计模式与亮点

- **方言无关**：依赖 GORM 抽象，MySQL / SQLite 双方言均可用，无需写方言分支。
- **与 setting/store 一致的极简迁移约定**：注释明确指出「mirrors the conventions of internal/manager/data/setting/store」。
- **幂等性**：AutoMigrate 本身的设计语义保证重复执行无副作用，适合每次启动都跑。

## 8. 注意事项

- **不会删列/删索引**：如果需要下线某列，必须走独立 migration 文件 + expand-contract 流程，本函数帮不上忙。
- **新列必须有零值默认或可空**：AutoMigrate 加列时不填默认值，依赖 GORM 模型 tag 处理可空性；新增必填列要先在模型上加 default tag 或先 backfill。
- **调用顺序**：必须在数据库连接建立后、HTTP server 起来之前执行，由 `cmd/ongrid/main.go` 编排。
- **错误即启动失败**：返回错误会让进程启动中断，符合「迁移失败绝不让服务带着半套 schema 跑」的红线。
