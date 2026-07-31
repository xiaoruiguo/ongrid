# `doc.go` 技术实现文档（dbx）

> 源文件：`internal/pkg/dbx/doc.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/dbx`

## 1. 概述

该文件是 `dbx` 包的文档声明文件，无任何代码实现，仅以 Go 包注释形式说明 ongrid 的 schema 管理策略：使用 GORM `AutoMigrate`（dialect-agnostic）而非手写 `.up.sql` / `.down.sql` 文件，每个数据层 package 暴露一个 `Migrator` 函数注册自身模型，由 cloud 二进制按启动顺序组合后交给 `RunMigrations` 执行。同一份 migrator 列表同时适用于 MySQL（生产默认）与 SQLite（本地 dev opt-in）。

## 2. 包信息

- **包名**：`dbx`
- **所属模块**：`internal/pkg/`（基础设施层）
- **依赖方向**：纯文档，无依赖。

## 3. 关键类型与接口

无显著类型定义（文件仅含包注释，无任何声明）。

## 4. 关键函数与流程

无关键函数。文档示例展示了 migrator 函数的典型形态：

```go
// internal/iam/data/user/sqlite/user.go
func Migrate(db *gorm.DB) error { return db.AutoMigrate(&User{}) }
```

以及 cloud 二进制（`cmd/ongrid`）的装配形态：

```go
if err := dbx.RunMigrations(db, log,
    iamdatauser.Migrate,
    manageredgedata.Migrate,
    managermetricdata.Migrate,
    manageraiopsdata.Migrate,
); err != nil { ... }
```

## 5. 依赖关系

- **内部包**：无。
- **外部库**：无。
- **被调用方**：无（仅作为包文档被 `go doc` / IDE 读取）。

## 6. 并发与资源管理

无并发控制（无代码）。

## 7. 设计模式与亮点

- **声明式 schema 演进**：每个 BC 自治管理自身模型，避免全局 migration 文件夹的合并冲突与所有权混乱，符合 monorepo 多 BC 协作场景。
- **dialect-agnostic**：`AutoMigrate` 在 GORM 层屏蔽 dialect 差异，MySQL / SQLite 共用一份 migrator 列表，零额外维护成本。
- **顺序组装**：migrator 列表在 `cmd/ongrid` 显式按依赖顺序排列（如 iam.user 先于 manager.edge，因 edge 可能引用 user 外键），顺序错误会被 GORM FK 报错暴露。
- **命名遗留说明**：注释提到 `internal/iam/data/user/sqlite` 中的 `sqlite` 是历史命名，函数本身 dialect-agnostic——避免新开发者误解为仅支持 SQLite。

## 8. 注意事项

- **AutoMigrate 的局限**：仅做加法（新增表 / 列 / 索引），不删除字段、不修改列类型、不重命名；schema 重构需配合 `dbx.DropIndexes` 或手写 SQL。
- **生产 schema 变更红线**：gospec 要求生产 schema 变更走 migration 文件 + 在线 DDL 工具；当前 AutoMigrate 路径主要服务 dev / 单租户部署，大规模生产需评估是否切换到 gh-ost / pt-online-schema-change 等工具。
- **顺序敏感**：migrator 列表顺序硬编码在 `cmd/ongrid`，新增 BC 时需手工插入正确位置。
- **无回滚机制**：AutoMigrate 不生成 down migration，回滚需手工 SQL。
- **文档与实现可能漂移**：示例路径（`internal/iam/data/user/sqlite/user.go`）若被重构需同步更新本注释，否则误导新开发者。
