# `dbx.go` 技术实现文档

> 源文件：`internal/pkg/dbx/dbx.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/dbx`

## 1. 概述

该文件是 ongrid 数据库基础设施共享入口，封装 GORM 打开 MySQL / SQLite 后端的逻辑。MySQL 为生产默认，SQLite 作为单用户本地调试的 opt-in 后端。打开时根据 dialect 注入对应 driver，并对 MySQL 执行 `Ping()` fail-fast；对 SQLite 注入 WAL + busy_timeout + foreign_keys 三条 pragma。DSN 密码在日志中被脱敏。

## 2. 包信息

- **包名**：`dbx`
- **所属模块**：`internal/pkg/`（基础设施层）
- **依赖方向**：被 `cmd/ongrid` / `cmd/ongrid-edge` 启动时调用；依赖 `internal/pkg/config`、gorm + 两个 driver。

## 3. 关键类型与接口

无显著类型定义（仅有顶层函数）。

## 4. 关键函数与流程

### `Open`
- **签名**：`func Open(cfg config.DBConfig, log *slog.Logger) (*gorm.DB, error)`
- **职责**：根据 `cfg.Dialect` 分发到 `openMySQL` 或 `openSQLite`；空 dialect 视为 `mysql`。
- **错误处理**：未知 dialect 返回 `unsupported dialect %q`。

### `openMySQL`
- **签名**：`func openMySQL(dsn string, log *slog.Logger) (*gorm.DB, error)`
- **流程**：
  1. DSN 空 → error。
  2. `gorm.Open(gormmysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Warn)})`：Warn 级别，避免正常查询流污染日志。
  3. `gdb.DB()` 取 `*sql.DB` → `Ping()` 验证可达性；失败 → `mysql ping failed`，**fail-fast**。
  4. 日志记录 `mysql opened`，DSN 经 `redactDSN` 脱敏。
- **错误处理**：每步用 `%w` 包装错误并加前缀 `dbx: ...`。

### `openSQLite`
- **签名**：`func openSQLite(path string, log *slog.Logger) (*gorm.DB, error)`
- **流程**：
  1. path 空 → error。
  2. 非 `:memory:` 时 `os.MkdirAll(filepath.Dir(path), 0o755)` 创建父目录。
  3. `buildSQLiteDSN(path)` 拼接 pragma query params。
  4. `gorm.Open(sqlite.Open(dsn), ...)`。
  5. 日志记录 `sqlite opened` + journal_mode / foreign_keys 状态。
- **特殊处理**：`:memory:` 路径直接传入 driver，不附加 pragma（driver 自身处理）。

### `buildSQLiteDSN`
- **签名**：`func buildSQLiteDSN(path string) string`
- **职责**：附加 `_pragma=journal_mode(WAL)` / `_pragma=busy_timeout(5000)` / `_pragma=foreign_keys(on)`。
- **流程**：`:memory:` 原样返回；否则用 `url.Values` 拼 query，根据 path 是否已含 `?` 选择 `?` 或 `&` 分隔符。

### `redactDSN`
- **签名**：`func redactDSN(dsn string) string`
- **职责**：剥离 go-sql-driver/mysql DSN 中的密码。
- **流程**：`LastIndex(dsn, "@")` 切分 userinfo / rest；userinfo 中若有 `:` 则把 `:password` 替换为 `:***`；拼接回 `user:***@host:port/db?params`。无 `@` 时原样返回。
- **设计理由**：让运维在日志里看到连接的目标 endpoint 而不泄露凭据。

## 5. 依赖关系

- **内部包**：`github.com/ongridio/ongrid/internal/pkg/config`。
- **外部库**：`gorm.io/gorm` + `gorm.io/driver/mysql` + `github.com/glebarez/sqlite`（纯 Go SQLite，无 CGO）。
- **被调用方**：`cmd/ongrid` / `cmd/ongrid-edge` 启动装配；测试代码通过 `:memory:` 复用。

## 6. 并发与资源管理

无显式并发控制。`*gorm.DB` 自身维护连接池并并发安全；`Ping` 在 Open 时同步执行，失败立即返回。SQLite 的并发由 WAL + busy_timeout=5000ms 保证多读单写不直接 `SQLITE_BUSY`。

## 7. 设计模式与亮点

- **dialect 切换零代码改动**：上层 GORM 模型 dialect-agnostic，切换后端只需改 `ONGRID_DB_DIALECT` env。
- **MySQL fail-fast**：`Ping()` 在 Open 时执行，配置错误立即暴露而非首条查询时。
- **SQLite pragma 标准化**：WAL + busy_timeout + foreign_keys 三件套覆盖最常见的 SQLite 部署陷阱（默认 FK 关闭、默认 rollback journal 阻塞并发读、`SQLITE_BUSY` 立即报错）。
- **纯 Go SQLite**：`glebarez/sqlite` 不需 CGO，简化构建链。
- **DSN 脱敏**：`redactDSN` 让日志可读而不泄密，符合 gospec "敏感字段禁止明文入日志" 红线。
- **Warn 级 logger**：默认 Warn，避免正常查询流淹没应用日志；需要时调用方 `db.Session(&gorm.Session{Logger: ...})` 临时调级。

## 8. 注意事项

- **MySQL DSN 默认值不安全**：`config.DBConfig.DSN` 默认 `ongrid:ongrid@tcp(127.0.0.1:3306)/ongrid...`，生产必须覆盖。
- **SQLite 不适合生产**：WAL 改善并发但仍是单机单文件，无网络访问、无横向扩展；仅 dev / 单用户场景。
- **`redactDSN` 局限**：仅识别 go-sql-driver 格式（`user:pass@host/db`）；若 DSN 含特殊字符（如 `@` 在密码中）可能误脱敏。
- **Ping 不验证权限**：仅验证连通性，不验证账号对目标 schema 的权限；权限错误在首条查询时才暴露。
- **无连接池配置**：未暴露 `SetMaxOpenConns` / `SetMaxIdleConns` / `SetConnMaxLifetime`，使用 driver 默认值；高并发场景需在上层调整。
- **glebarez/sqlite 替代 mattn/go-sqlite3**：纯 Go 但性能略低于 CGO 版本，大批量写入场景需评估。
