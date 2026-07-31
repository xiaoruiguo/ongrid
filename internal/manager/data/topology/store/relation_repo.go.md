# topology/store/relation_repo.go

## 1. 概述

本文件实现 `relations` 表（拓扑节点间有向边）的 GORM-backed 持久化，满足 `biz/topology.RelationRepo` 接口。覆盖能力：创建、更新 props_jsonb、按 id 读取、列表（含过滤/分页/计数）、删除。

设计要点：
- **单列更新**：`Update` 只写 `props_jsonb` 一列，用 `Update("props_jsonb", propsJSON)` 而非 `Save`，避免覆盖 `src_id` / `dst_id` / `type` 等不可变字段。
- **SrcOrDstID 互斥过滤**：`applyRelationFilter` 中 `SrcOrDstID` 与 `SrcID` / `DstID` 互斥——设了 `SrcOrDstID` 就忽略更具体的端点过滤，简化 SQL。
- **List + Count 共享 filter**：`applyRelationFilter` 让分页列表与总数统计用同一套过滤条件，保证一致性。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/topology/store`
- **依赖方向**：`controlplane → biz/topology → data/topology/store → model/topology + pkg/errs`；接口 `biz.RelationRepo` 在消费方 biz 层定义，编译期断言 `var _ biz.RelationRepo = (*RelationRepo)(nil)`。

## 3. 关键类型与接口

```go
// RelationRepo 是 biz/topology.RelationRepo 的 GORM 实现。
type RelationRepo struct{ db *gorm.DB }

func NewRelationRepo(db *gorm.DB) *RelationRepo

// 编译期接口断言
var _ biz.RelationRepo = (*RelationRepo)(nil)

// 局部 helper
func applyRelationFilter(q *gorm.DB, f biz.RelationListFilter) *gorm.DB
```

`biz.RelationListFilter` 字段（隐含）：`SrcID uint64`、`DstID uint64`、`SrcOrDstID uint64`（互斥守卫）、`Type string`、`Limit int`、`Offset int`。

`model.Relation` 结构体字段（隐含）：`id uint64`、`src_id uint64`、`dst_id uint64`、`type string`、`props_jsonb string`、`created_at`、`updated_at`。

## 4. 关键函数与流程

### `NewRelationRepo(db *gorm.DB) *RelationRepo`

构造器，直接包装 `*gorm.DB`。

### `Create(ctx, rel *model.Relation) error`

- **职责**：插入关系行（有向边）。
- **流程**：`r.db.WithContext(ctx).Create(rel).Error`。
- **错误处理**：直接透传 GORM 错误（未做唯一索引冲突翻译）。
- **id 来源**：依赖 model 自增或预填。

### `Update(ctx, id uint64, propsJSON string) error`

- **职责**：更新关系的 `props_jsonb`（属性 JSON）。
- **流程**：
  1. `Model(&Relation{}).Where("id = ?", id).Update("props_jsonb", propsJSON)`。
  2. `res.Error` 透传。
  3. `RowsAffected == 0` → `errs.ErrNotFound`。
- **设计权衡**：用 `Update("props_jsonb", propsJSON)` 单列更新，避免 `Save` 覆盖 `src_id` / `dst_id` / `type` 等不可变字段。
- **不可变字段**：关系的端点（`src_id` / `dst_id`）与类型（`type`）创建后不可改；要改请删旧建新。

### `Get(ctx, id uint64) (*model.Relation, error)`

- **职责**：按 id 取单行。
- **流程**：`Where("id = ?", id).First(&rel)`；`gorm.ErrRecordNotFound` → `errs.ErrNotFound`。
- **软删过滤**：GORM 自动加 `WHERE deleted_at IS NULL` 或 `delete_marker = 0`（取决于 model 软删模式；本表用 delete_marker，见 `migrate.go`）。

### `List(ctx, f biz.RelationListFilter) ([]*model.Relation, error)`

- **职责**：按 filter 列表关系，id 降序。
- **流程**：
  1. `Model(&Relation{})` + `applyRelationFilter(q, f)`。
  2. `Order("id DESC")`。
  3. `f.Limit > 0` → `Limit(f.Limit)`；`f.Offset > 0` → `Offset(f.Offset)`。
  4. `Find(&out)`。
- **排序**：id 降序，最新关系在前。

### `Count(ctx, f biz.RelationListFilter) (int64, error)`

- **职责**：按 filter 计数（与 List 共享 filter，保证分页一致性）。
- **流程**：`Model(&Relation{})` + `applyRelationFilter(q, f)` + `Count(&n)`。

### `Delete(ctx, id uint64) error`

- **职责**：删除关系。
- **流程**：`Where("id = ?", id).Delete(&model.Relation{})`；`RowsAffected == 0` → `errs.ErrNotFound`。
- **软删 vs 物理删**：本表用 delete_marker 模式（见 `migrate.go` 的 delete_marker 迁移），GORM 的 `Delete` 会根据 model 的软删字段自动改为 `UPDATE ... SET delete_marker = 1`。

### `applyRelationFilter(q, f) *gorm.DB`

- **职责**：把 `RelationListFilter` 翻译为 WHERE 子句。
- **流程**：
  1. **SrcOrDstID 互斥守卫**：
     - `f.SrcOrDstID != 0` → `Where("src_id = ? OR dst_id = ?", f.SrcOrDstID, f.SrcOrDstID)`——查「与某节点相关的所有关系」（任一端匹配）。
     - 否则分别处理 `f.SrcID`（`src_id = ?`）与 `f.DstID`（`dst_id = ?`）。
  2. `f.Type != ""` → `Where("type = ?", f.Type)`。
- **互斥设计**：注释明确「SrcOrDstID is processed exclusively (mutually exclusive with SrcID/DstID — if the caller sets it, we ignore the more-specific endpoints to keep the SQL straightforward)」——避免 `OR` 与 `AND` 混用导致 SQL 语义混乱。
- **复用**：List 与 Count 共享此函数，保证过滤一致性。

## 5. 依赖关系

- **内部包**：
  - `github.com/ongridio/ongrid/internal/manager/biz/topology`（biz）——`RelationRepo` 接口、`RelationListFilter`。
  - `github.com/ongridio/ongrid/internal/manager/model/topology`——`Relation` 结构体。
  - `github.com/ongridio/ongrid/internal/pkg/errs`——`ErrNotFound` 标准错误。
- **外部库**：`gorm.io/gorm`（含 `ErrRecordNotFound` sentinel）。
- **标准库**：`context`、`errors`。
- **被调用方**：`biz/topology` 的关系管理 usecase。

## 6. 并发与资源管理

- **无共享状态**：`RelationRepo` 仅持有不可变 `*gorm.DB`，可并发调用。
- **每次新 session**：`r.db.WithContext(ctx)` 隔离事务状态。
- **ctx 透传**：所有方法首参 `context.Context`，符合 gospec 红线。
- **无锁、无 channel、无缓存**。
- **无资源释放**：GORM 连接池管理。
- **并发 Update 同一关系**：最后写赢；无乐观锁。

## 7. 设计模式与亮点

- **SrcOrDstID 互斥守卫**：`SrcOrDstID` 与 `SrcID` / `DstID` 互斥，避免 `OR` 与 `AND` 混用导致 SQL 语义混乱，是处理「双向查询」的清晰模式。
- **单列更新**：`Update` 用 `Update("props_jsonb", propsJSON)` 只写一列，避免 `Save` 覆盖不可变字段（端点与类型）。
- **List + Count 共享 filter**：`applyRelationFilter` 抽出过滤逻辑，保证列表与计数一致。
- **三态删除**：`Delete` 用 `RowsAffected == 0` → `ErrNotFound`，与项目其他 repo 一致。
- **delete_marker 软删**：本表用 delete_marker 模式（见 `migrate.go`），GORM 自动处理软删过滤。
- **编译期接口断言**：`var _ biz.RelationRepo = (*RelationRepo)(nil)` 让接口变更编译期暴露。
- **id 降序排序**：List 按 `id DESC`，最新关系在前。

## 8. 注意事项

- **端点不可改**：`Update` 只能改 `props_jsonb`；要改端点（`src_id` / `dst_id`）或类型（`type`）需删旧建新。
- **SrcOrDstID 互斥**：调用方设了 `SrcOrDstID` 后，`SrcID` / `DstID` 会被忽略；如需精确端点过滤不要设 `SrcOrDstID`。
- **`List` 分页用 limit+offset**：大数据量下 offset 性能差；目前数据量可接受，如成瓶颈改 cursor 分页。
- **`Delete` 软删**：本表用 delete_marker，`Delete` 是软删（`UPDATE ... SET delete_marker = 1`）；如需物理删除需绕过 GORM 软删机制。
- **并发 Update 无乐观锁**：两个并发 Update 同一关系，最后写赢；无版本号字段。
- **跨方言**：所有查询方言无关；`OR` 与 `IN` 在 MySQL/SQLite 均可用。
- **id 类型**：`uint64`（自增整型），与 `RelationRepo` 全部方法一致。
- **`applyRelationFilter` 是局部 helper**：仅在 relation_repo.go 内使用。
- **关系类型引用**：`type` 列引用 `relation_types.name`；本 repo 不做外键校验，biz 层需保证 type 存在。
