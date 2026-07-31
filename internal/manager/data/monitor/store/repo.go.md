# monitor/store/repo.go

## 1. 概述

本文件实现 `monitor_panels` 表的 GORM-backed CRUD 仓储。`Repo` 是无状态结构体，仅持有 `*gorm.DB` 句柄；每个方法都用 `WithContext` 开新 session，确保并发调用方不会共享事务状态。覆盖的能力：列表面板、按 id 读取、求最大 ordinal、创建、按字段映射更新、记录异步 Grafana 同步结果、删除。

设计要点：
- **稳定排序**：`List` 按 `(ordinal asc, id asc)` 排序，保证 SPA 跨刷新渲染确定性。
- **三态区分**：`Update` / `Delete` 通过 `RowsAffected` 区分「行不存在」与「无操作更新」，缺失行统一映射为 `errs.ErrNotFound`。
- **同步结果非致命**：`SetSyncResult` 失败仅写日志，不影响 200 响应；行依然可用。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/monitor/store`
- **依赖方向**：`controlplane → biz/monitor → data/monitor/store → model/monitor`；接口在消费方 `biz/monitor` 定义（wire-ready 构造器返回具体 `*Repo`，由 biz 层接口断言）。

## 3. 关键类型与接口

```go
// Repo 是 monitor_panels 的 GORM 持久化实现；无状态、线程安全。
type Repo struct {
    db *gorm.DB
}

// 构造器
func NewRepo(db *gorm.DB) *Repo
```

无 sentinel error，依赖 `github.com/ongridio/ongrid/internal/pkg/errs` 包提供的标准错误（`ErrInvalid`、`ErrNotFound`）。

## 4. 关键函数与流程

### `NewRepo(db *gorm.DB) *Repo`

- **职责**：构造 Repo。
- **流程**：直接包装 `*gorm.DB` 返回。

### `List(ctx context.Context) ([]*model.Panel, error)`

- **职责**：返回所有 panel，按 `(ordinal asc, id asc)` 稳定排序。
- **流程**：`r.db.WithContext(ctx).Order("ordinal asc").Order("id asc").Find(&out)`。
- **错误处理**：透传 GORM 错误。

### `Get(ctx, id uint64) (*model.Panel, error)`

- **职责**：按 id 取单行。
- **流程**：
  1. `id == 0` → `errs.ErrInvalid` 包装。
  2. `First(&p, id)`；若 `gorm.ErrRecordNotFound` → 返回 `errs.ErrNotFound`；其余错误透传。
- **返回**：`*model.Panel` 指针。

### `MaxOrdinal(ctx) (int, error)`

- **职责**：返回表中最大 ordinal；空表返回 0。
- **用途**：`Create` 时新 panel 排到末尾，无需调用方先读 List。
- **流程**：`Select("COALESCE(MAX(ordinal), 0)").Row().Scan(&n)`。
- **错误处理**：扫描错误透传。

### `Create(ctx, p *model.Panel) (*model.Panel, error)`

- **职责**：插入新 panel，返回已持久化的行（id/时间戳已填）。
- **流程**：
  1. `p == nil` → `errs.ErrInvalid`。
  2. `r.db.WithContext(ctx).Create(p)`，GORM 会回填主键与时间戳。

### `Update(ctx, id uint64, fields map[string]any) (*model.Panel, error)`

- **职责**：按字段名→值映射更新指定行，返回更新后的行。
- **流程**：
  1. `id == 0` → `errs.ErrInvalid`。
  2. `fields` 为空 → 等价于 `Get`（避免无意义 UPDATE）。
  3. `Model(&Panel{}).Where("id = ?", id).Updates(fields)`。
  4. `RowsAffected == 0` 时**仍调用 `Get`** 以区分「行不存在」与「值未变」——`gorm.Updates` 对二者都报 0 行。
  5. 最终都返回 `Get(ctx, id)` 的结果。
- **设计权衡**：多一次 SELECT 换取准确的 ErrNotFound 语义，可接受。

### `SetSyncResult(ctx, id uint64, errMsg string) error`

- **职责**：记录异步 Grafana mirror 同步结果；`errMsg == ""` 表示成功；`last_sync_at` 总是更新。
- **流程**：
  1. `id == 0` → `errs.ErrInvalid`。
  2. `now := time.Now().UTC()`。
  3. `Updates(map[string]any{"last_sync_error": errMsg, "last_sync_at": &now})`。
- **非致命语义**：失败仅写日志，调用方仍返回 200 给 operator，行依然可用。

### `Delete(ctx, id uint64) error`

- **职责**：物理删除指定行（panel 无软删）。
- **流程**：
  1. `id == 0` → `errs.ErrInvalid`。
  2. `Delete(&model.Panel{}, id)`。
  3. `RowsAffected == 0` → `errs.ErrNotFound`。
- **注意**：本表无 `deleted_at`，是物理删除；如需软删需在 model 上加 gorm.DeletedAt。

## 5. 依赖关系

- **内部包**：
  - `model/monitor`——`Panel` 结构体。
  - `internal/pkg/errs`——`ErrInvalid`、`ErrNotFound` 标准错误。
- **外部库**：
  - `gorm.io/gorm`——ORM、`ErrRecordNotFound` sentinel。
  - 标准库 `context`、`errors`、`fmt`、`time`。
- **被调用方**：`biz/monitor`（业务层），可能由 controlplane 的 monitor handler 触发。

## 6. 并发与资源管理

- **无共享状态**：Repo 仅持有不可变的 `*gorm.DB` 句柄，可被多 goroutine 并发使用。
- **每次调用新 session**：`r.db.WithContext(ctx)` 创建独立 session，不共享事务/查询状态。
- **ctx 透传**：所有方法首参 `context.Context`，符合 gospec「IO 函数首参 ctx」红线。
- **无锁、无 channel、无缓存**：状态完全在数据库。
- **无资源释放**：不开连接、不持游标，GORM 内部连接池管理生命周期。

## 7. 设计模式与亮点

- **三态更新区分**：`Update` 在 `RowsAffected == 0` 时跟随 `Get` 确认，是处理 GORM「0 行」歧义的标准模式。
- **MaxOrdinal 助创**：让 `Create` 不必先读 List，简化调用方。
- **SetSyncResult 解耦异步副作用**：Grafana mirror 是异步操作，其成败不应阻塞主流程，单独一个写字段的方法把副作用隔离。
- **稳定排序**：双键 `(ordinal, id)` 排序保证 SPA 渲染确定性。
- **errs 标准错误传播**：上层可用 `errors.Is(err, errs.ErrNotFound)` 统一处理，无需感知 GORM sentinel。

## 8. 注意事项

- **物理删除**：`Delete` 是硬删除，删了不可恢复；如需保留审计需改用软删模型。
- **fields map 限制**：`Update` 用 `map[string]any` 直传列名，调用方需保证列名拼写正确，且不能传结构体字段零值跳过更新（map 形式会写入零值）。
- **SetSyncResult 不做行存在校验**：`RowsAffected == 0` 不返回错误，调用方应自行确保 id 存在（通常 id 来自刚 Get/Create 的行）。
- **last_sync_at 时区**：使用 UTC 写入，读取展示时由前端按用户时区转换。
- **MySQL vs SQLite**：所有查询均方言无关；`COALESCE(MAX(...), 0)` 两边都支持。
- **并发 Create**：两个并发 Create 都基于 `MaxOrdinal` 算新 ordinal 会有竞态（可能同 ordinal）；本表用 `(ordinal, id)` 排序兜底，但若要求 ordinal 唯一需加唯一索引或排他事务。
