# webshell/store/migrate.go

## 1. 概述

本文件实现 webshell 子域的 schema 迁移**与**审计仓储（`Migrate` + `Repo` 合并在同一文件）。`webshell_sessions` 表记录 webshell 会话的审计信息（开始/结束时间、流量统计、退出码等），是安全审计的关键数据源。

设计要点：
- **Migrate + Repo 同文件**：webshell 子域较小，迁移与仓储合并到一个文件，减少文件碎片。
- **append-only 写入模式**：`Insert` 在会话开启时写一行，`Close` 在会话结束时更新该行的终端统计；中间不修改，便于审计追溯。
- **List 默认 50 条**：`List` 的 `limit <= 0` 时默认 50，避免无限制查询。
- **List 容错**：`gorm.ErrRecordNotFound` 被当作「无记录」返回空切片（虽然 `Find` 通常不返回此错误，但作为防御性处理）。

## 2. 包信息

- **包名**：`store`（包注释明确「backs the webshell audit table」）
- **所属模块**：`internal/manager/data/webshell/store`
- **依赖方向**：`controlplane → biz/webshell → data/webshell/store → model/webshell + pkg/errs`；接口在消费方 biz 层定义（本文件未显式写编译期断言，但 Repo 由 wire 注入 biz）。

## 3. 关键类型与接口

```go
// Migrate 函数
func Migrate(db *gorm.DB) error

// Repo 是 GORM-backed 审计仓储。
type Repo struct{ db *gorm.DB }

func NewRepo(db *gorm.DB) *Repo

// CloseInput 封装会话关闭时的终端统计。
type CloseInput struct {
    EndedAt      any    // 结束时间（用 any 兼容 nil 与 time.Time）
    BytesStdin   uint64
    BytesStdout  uint64
    ExitCode     int
    TerminatedBy string
}
```

`webshell.Session` 结构体字段（隐含）：`id string`（manager 生成的 uuid）、`started_at time.Time`、`ended_at time.Time`、`bytes_stdin uint64`、`bytes_stdout uint64`、`exit_code int`、`terminated_by string`。

## 4. 关键函数与流程

### `Migrate(db *gorm.DB) error`

- **职责**：注册 `webshell_sessions` 表到 GORM AutoMigrate。
- **流程**：`db.AutoMigrate(&webshell.Session{})`，只增不删，幂等。
- **错误处理**：透传 AutoMigrate 错误。

### `NewRepo(db *gorm.DB) *Repo`

构造器，直接包装 `*gorm.DB`。

### `Insert(ctx, s *webshell.Session) error`

- **职责**：插入新会话审计行（append-only 写入）。
- **流程**：`r.db.WithContext(ctx).Create(s).Error`。
- **id 来源**：注释明确「ID is the manager-generated uuid」——`s.ID` 由调用方预填（uuid），不依赖自增。
- **StartedAt**：注释明确「StartedAt should be set by caller」——调用方负责设置开始时间。
- **错误处理**：直接透传 GORM 错误。

### `Close(ctx, id string, in CloseInput) error`

- **职责**：更新审计行的终端统计（会话关闭时调用）。
- **流程**：
  1. `Model(&Session{}).Where("id = ?", id).Updates(map[string]any{...})`：
     - `ended_at` = `in.EndedAt`
     - `bytes_stdin` = `in.BytesStdin`
     - `bytes_stdout` = `in.BytesStdout`
     - `exit_code` = `in.ExitCode`
     - `terminated_by` = `in.TerminatedBy`
  2. `res.Error` 透传。
  3. `RowsAffected == 0` → `errs.ErrNotFound`。
- **map 形式 Updates**：用 `map[string]any` 只写指定列，避免 `Save` 覆盖 `started_at` / `id` 等不可变字段。
- **EndedAt 类型为 any**：兼容 `nil`（未正常关闭）与 `time.Time`（正常关闭）；map 形式可以接受任意类型。

### `List(ctx, limit int) ([]*webshell.Session, error)`

- **职责**：返回最近 N 条会话，最新优先。
- **流程**：
  1. `limit <= 0` → `limit = 50`（默认值）。
  2. `Order("started_at desc").Limit(limit).Find(&out)`。
  3. **容错**：`gorm.ErrRecordNotFound` → 返回 `nil, nil`（空切片，不报错）。
- **排序**：`started_at DESC`，最近会话在前。
- **默认 50 条**：避免无限制查询拖垮数据库。

## 5. 依赖关系

- **内部包**：
  - `github.com/ongridio/ongrid/internal/manager/model/webshell`——`Session` 结构体。
  - `github.com/ongridio/ongrid/internal/pkg/errs`——`ErrNotFound` 标准错误。
- **外部库**：`gorm.io/gorm`（含 `ErrRecordNotFound` sentinel）。
- **标准库**：`context`、`errors`。
- **被调用方**：`biz/webshell` 的会话管理 usecase；`cmd/ongrid/main.go` 的迁移编排调 `Migrate`。

## 6. 并发与资源管理

- **无共享状态**：`Repo` 仅持有不可变 `*gorm.DB`，可并发调用。
- **每次新 session**：`r.db.WithContext(ctx)` 隔离事务状态。
- **ctx 透传**：所有方法首参 `context.Context`，符合 gospec 红线。
- **无锁、无 channel、无缓存**。
- **无资源释放**：GORM 连接池管理。
- **并发 Close 同一 id**：最后写赢；无乐观锁，但同一会话通常只关闭一次，竞态概率低。

## 7. 设计模式与亮点

- **Migrate + Repo 同文件**：webshell 子域较小，合并减少文件碎片，符合「不过度工程化」原则。
- **append-only 写入模式**：`Insert` 写入 + `Close` 更新统计，中间不修改，便于审计追溯。
- **EndedAt 用 any 类型**：兼容 `nil`（未正常关闭）与 `time.Time`，是处理「可选时间字段」的实用模式。
- **map 形式 Updates**：`Close` 用 `map[string]any` 只写指定列，避免 `Save` 覆盖不可变字段。
- **三态删除映射**：`Close` 用 `RowsAffected == 0` → `ErrNotFound`，与项目其他 repo 一致。
- **List 默认 limit**：`limit <= 0` 时默认 50，避免无限制查询。
- **List 容错**：`gorm.ErrRecordNotFound` 当作空切片返回，防御性处理（虽然 `Find` 通常不返回此错误）。
- **id 用 uuid**：会话 id 由 manager 生成 uuid，适合分布式场景与外部引用。

## 8. 注意事项

- **append-only 模式**：审计行一旦 `Insert` 后只有 `Close` 能修改；中间不应有其他更新，保证审计完整性。
- **`Close` 的 EndedAt 用 any**：调用方传 `nil` 会写入 NULL，传 `time.Time` 会写入时间；需保证 biz 层传值一致。
- **`List` 容错可能掩盖错误**：`gorm.ErrRecordNotFound` 被当作空切片返回，可能掩盖真实错误；但 `Find` 通常不返回此错误，影响小。
- **`List` 默认 50**：调用方若需更多需显式传 limit；审计查询通常只看最近会话，50 条够用。
- **无软删**：审计表通常不删数据，保留全量历史；如需清理旧数据应走独立 retention 任务。
- **id 用 uuid**：`Insert` 时 `s.ID` 必须由调用方预填；如未填会写入空字符串，可能导致后续 `Close` / `List` 异常。
- **StartedAt 由调用方设置**：`Insert` 不自动填 `StartedAt`，调用方必须设置；如未填会写入零值时间。
- **跨方言**：所有查询方言无关；`Order("started_at desc")` 在 MySQL/SQLite 均可用。
- **审计安全**：本表是安全审计关键数据源，应限制直接访问权限；data 层不做权限控制，由 biz 层守卫。
- **流量统计精度**：`bytes_stdin` / `bytes_stdout` 依赖前端上报，可能不精确；审计时应考虑此因素。
