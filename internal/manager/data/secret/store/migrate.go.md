# secret/store/migrate.go

## 1. 概述

本文件实现 `secrets` 表（凭证保险库）的 schema 迁移。仅暴露一个 `Migrate` 函数，被 `cmd/ongrid/main.go` 启动期迁移列表调用。遵循 gospec「expand-contract / 滚动发布兼容」原则——只调用 GORM `AutoMigrate`，**只增不删**，重复执行幂等。

设计要点：
- **极简迁移**：单表 + AutoMigrate，无 backfill、无索引重建、无 delete_marker 迁移。
- **密钥不进迁移**：本文件不涉及密钥内容，仅保证表结构存在；密钥的加解密在 `biz/secret` 层完成，data 层只存 sealed blob。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/secret/store`
- **依赖方向**：`cmd → data/secret/store → model/secret → gorm`，符合单服务分层。

## 3. 关键类型与接口

本文件无类型/接口/常量定义，仅一个导出函数。

```go
func Migrate(db *gorm.DB) error
```

涉及的对象模型（在 `model/secret` 中定义）：
```go
&model.Secret{} // 凭证表（存 sealed blob + name + description）
```

## 4. 关键函数与流程

### `Migrate(db *gorm.DB) error`

- **职责**：注册 `secrets` 表到 GORM AutoMigrate。
- **流程**：
  1. `db.AutoMigrate(&model.Secret{})`，GORM 比对数据库实际 schema 与 `model.Secret` 结构体，差异处自动 `ALTER TABLE ADD COLUMN / ADD INDEX`。
  2. 返回错误（若有）。
- **错误处理**：直接透传 `AutoMigrate` 的错误，由上层 `dbx.RunMigrations` 决定是否中止启动。

## 5. 依赖关系

- **内部包**：
  - `github.com/ongridio/ongrid/internal/manager/model/secret`——`Secret` 结构体（表 schema 来源）。
- **外部库**：`gorm.io/gorm`——`*gorm.DB` 入参。
- **被调用方**：`cmd/ongrid/main.go` 的迁移编排（与其他 data 包迁移并列注册）。

## 6. 并发与资源管理

- **无并发状态**：迁移在启动期单线程顺序执行，无锁、无 channel、无缓存。
- **无 ctx**：迁移函数签名不带 `context.Context`（启动期同步执行，无需取消）。
- **无资源释放**：不开游标、不持连接，连接池由 GORM 管理。

## 7. 设计模式与亮点

- **方言无关**：依赖 GORM 抽象，MySQL / SQLite 双方言均可用。
- **极简迁移**：与 `monitor/store/migrate.go` 同款约定，单表 AutoMigrate 即可。
- **密钥安全边界**：迁移只管结构，不管内容；密钥 sealed blob 由 `biz/secret` 加密后写入，data 层永不接触明文。
- **幂等性**：AutoMigrate 本身保证重复执行无副作用。

## 8. 注意事项

- **不会删列/删索引**：下线列需走独立 migration 文件 + expand-contract 流程。
- **新列必须有零值默认或可空**：AutoMigrate 加列时不填默认值，新增必填列要先在模型上加 default tag 或先 backfill。
- **密钥列不可降级**：若 `value` 列（sealed blob）曾用更强算法加密，迁移不会重新加密；算法升级需 biz 层 lazily re-encrypt。
- **调用顺序**：必须在数据库连接建立后、HTTP server 起来之前执行。
- **错误即启动失败**：返回错误会让进程启动中断，符合「迁移失败绝不让服务带着半套 schema 跑」红线。
- **无 delete_marker 迁移**：本表用传统 `gorm.DeletedAt` 软删（若 model 定义了）；如未来要切 delete_marker 模式需参考 `report/store/migrate.go` 的四步法。
- **密钥不进日志**：迁移过程不读取 `value` 列内容，符合 gospec「敏感字段禁止明文入日志」红线。
