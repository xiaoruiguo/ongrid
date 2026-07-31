# `migrate.go` 技术实现文档

> 源文件：`internal/iam/data/user/sqlite/migrate.go`
> 包路径：`github.com/ongridio/ongrid/internal/iam/data/user/sqlite`

## 1. 概述

本文件是 IAM BC 用户表的 GORM AutoMigrate 入口。除对 `model.User` 执行 AutoMigrate 外，还处理 legacy `chk_users_role` CHECK 约束的 drop + recreate，使角色枚举从旧 2 值集（admin/user）升级到 3 值集（admin/user/viewer）。

## 2. 包信息

- **包名**：`sqlite`
- **所属模块**：`internal/iam/data/user/sqlite` —— data 层用户迁移
- **依赖方向**：被 `cmd/ongrid` 通过 `dbx.RunMigrations` 调用；依赖 `internal/iam/model`

## 3. 关键类型与接口

无显著类型定义。

## 4. 关键函数与流程

### `Migrate`
- **签名**：`func Migrate(db *gorm.DB) error`
- **职责**：AutoMigrate User 表 + 修复 legacy CHECK 约束。
- **流程**：
  1. `db.AutoMigrate(&model.User{})` 创建/同步表结构。
  2. `db.Exec("ALTER TABLE users DROP CONSTRAINT chk_users_role")` 删除旧约束（错误被忽略）。
  3. `db.Exec("ALTER TABLE users ADD CONSTRAINT chk_users_role CHECK (role IN ('admin','user','viewer'))")` 重建新约束（错误被忽略）。
- **错误处理**：AutoMigrate 错误透传；两个 ALTER 错误以 `_ =` 忽略（注释说明：fresh deploy 无 legacy 约束，新约束已通过 struct tag 就位；SQLite 内联 check 无需 drop）。
- **注释要点**：解释了方言差异——MySQL DROP 成功、SQLite check 内联无 DROP 目标。

## 5. 依赖关系

- **内部包**：`internal/iam/model`
- **外部库**：`gorm.io/gorm`
- **被调用方**：`cmd/ongrid` 通过 `dbx.RunMigrations 组合调用`

## 6. 并发与资源管理

无并发控制。AutoMigrate 与 ALTER 自身负责 DDL 锁。

## 7. 设计模式与亮点

- **idempotent 约束迁移**：DROP 失败（不存在）+ ADD 失败（已存在）均被忽略，保证幂等。
- **方言感知注释**：明确说明 MySQL 与 SQLite 在 CHECK 约束上的行为差异，避免维护者误判。
- **struct tag 与显式 SQL 互补**：AutoMigrate 处理常规列，显式 SQL 处理 AutoMigrate 无法 ALTER 的 CHECK 约束。

## 8. 注意事项

- `_ =` 忽略 ALTER 错误是 gospec 红线「禁止忽略错误」的特例，本文件已通过注释说明理由；若需更严谨可改为 errors.Is 检测特定错误码。
- 约束重建期间存在短暂窗口期约束不存在，但仅启动期执行，风险可接受。
- 未来若再扩角色枚举，需同步更新 struct tag 的 `check:` 与本文件 ADD 语句的 IN 列表，否则 AutoMigrate 不会自动改 CHECK。
- 生产 MySQL 大表变更不应依赖此路径，需走 migration 文件（gospec 数据存储红线）。
