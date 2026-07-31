# topology/store/node_repo.go

## 1. 概述

本文件实现 `nodes` 表的 GORM-backed 持久化，满足 `biz/topology.NodeRepo` 接口。覆盖能力：创建、按字段更新（name + props_jsonb）、按 id 读取、批量读取、列表（含过滤/分页/计数）、删除。

设计要点：
- **wire-ready 构造器**：`NewNodeRepo` 返回具体 `*NodeRepo`，由 biz 层接口断言 `var _ biz.NodeRepo = (*NodeRepo)(nil)` 收口。
- **map 形式 Updates**：`Update` 用 `map[string]any` 只写 `name` + `props_jsonb` 两列，避免 `Save` 覆盖 `type` / `created_at` 等不可变字段。
- **GetMany 批量读取**：用 `WHERE id IN ?` 一次取回，返回 `map[uint64]*model.Node` 便于调用方按 id 查找。
- **List + Count 共享 filter**：`applyNodeFilter` 让分页列表与总数统计用同一套过滤条件，保证一致性。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/topology/store`
- **依赖方向**：`controlplane → biz/topology → data/topology/store → model/topology + pkg/errs`；接口 `biz.NodeRepo` 在消费方 biz 层定义，编译期断言 `var _ biz.NodeRepo = (*NodeRepo)(nil)`。

## 3. 关键类型与接口

```go
// NodeRepo 是 biz/topology.NodeRepo 的 GORM 实现。
type NodeRepo struct{ db *gorm.DB }

func NewNodeRepo(db *gorm.DB) *NodeRepo

// 编译期接口断言
var _ biz.NodeRepo = (*NodeRepo)(nil)

// 局部 helper
func applyNodeFilter(q *gorm.DB, f biz.NodeListFilter) *gorm.DB
```

`biz.NodeListFilter` 字段（隐含）：`Type string`、`Q string`（名称模糊搜索）、`Limit int`、`Offset int`。

## 4. 关键函数与流程

### `NewNodeRepo(db *gorm.DB) *NodeRepo`

构造器，直接包装 `*gorm.DB`。

### `Create(ctx, n *model.Node) error`

- **职责**：插入节点行。
- **流程**：`r.db.WithContext(ctx).Create(n).Error`。
- **错误处理**：直接透传 GORM 错误（未做唯一索引冲突翻译）。
- **id 来源**：依赖 model 自增或预填。

### `Update(ctx, id uint64, name, propsJSON string) error`

- **职责**：更新节点的 `name` 与 `props_jsonb`（属性 JSON）。
- **流程**：
  1. `Model(&Node{}).Where("id = ?", id).Updates(map[string]any{"name": name, "props_jsonb": propsJSON})`。
  2. `res.Error` 透传。
  3. `RowsAffected == 0` → `errs.ErrNotFound`。
- **设计权衡**：用 `map[string]any` + `Updates` 而非 `Save`，避免覆盖 `type` / `created_at` 等不可变字段。
- **注意**：`name` 与 `propsJSON` 都会被写入（即使 `propsJSON` 为空也会清空属性）；调用方若想「只改 name」需先 Get 原 propsJSON。

### `Get(ctx, id uint64) (*model.Node, error)`

- **职责**：按 id 取单行。
- **流程**：`Where("id = ?", id).First(&n)`；`gorm.ErrRecordNotFound` → `errs.ErrNotFound`。
- **软删过滤**：GORM 自动加 `WHERE deleted_at IS NULL`（若 model 有 `gorm.DeletedAt`）。

### `GetMany(ctx, ids []uint64) (map[uint64]*model.Node, error)`

- **职责**：批量按 id 取节点，返回 `map[id]*Node`。
- **流程**：
  1. 空切片 → 返回空 map（避免 `WHERE id IN ()` SQL 错误）。
  2. `Where("id IN ?", ids).Find(&rows)`。
  3. 遍历结果填充 `out[n.ID] = n`。
- **设计意图**：一次查询取回多个节点，避免 N+1；返回 map 便于调用方按 id 查找。
- **缺失 id 不报错**：若某 id 不存在，map 里就没有该 key；调用方需自行检测。

### `List(ctx, f biz.NodeListFilter) ([]*model.Node, error)`

- **职责**：按 filter 列表节点，id 降序。
- **流程**：
  1. `Model(&Node{})` + `applyNodeFilter(q, f)`。
  2. `Order("id DESC")`。
  3. `f.Limit > 0` → `Limit(f.Limit)`；`f.Offset > 0` → `Offset(f.Offset)`。
  4. `Find(&out)`。
- **排序**：id 降序，最新节点在前。

### `Count(ctx, f biz.NodeListFilter) (int64, error)`

- **职责**：按 filter 计数（与 List 共享 filter，保证分页一致性）。
- **流程**：`Model(&Node{})` + `applyNodeFilter(q, f)` + `Count(&n)`。

### `Delete(ctx, id uint64) error`

- **职责**：删除节点。
- **流程**：`Where("id = ?", id).Delete(&model.Node{})`；`RowsAffected == 0` → `errs.ErrNotFound`。
- **软删 vs 物理删**：取决于 model 是否有 `gorm.DeletedAt`；若 model 用 delete_marker 模式则需配合 `migrate.go` 的 delete_marker 迁移。

### `applyNodeFilter(q, f) *gorm.DB`

- **职责**：把 `NodeListFilter` 翻译为 WHERE 子句。
- **流程**：
  1. `f.Type != ""` → `Where("type = ?", f.Type)`。
  2. `f.Q != ""` → `like := "%" + strings.ToLower(f.Q) + "%"` + `Where("LOWER(name) LIKE ?", like)`。
- **大小写不敏感搜索**：`LOWER(name) LIKE LOWER(?)` 模式，跨方言兼容。
- **复用**：List 与 Count 共享此函数，保证过滤一致性。

## 5. 依赖关系

- **内部包**：
  - `github.com/ongridio/ongrid/internal/manager/biz/topology`（biz）——`NodeRepo` 接口、`NodeListFilter`。
  - `github.com/ongridio/ongrid/internal/manager/model/topology`——`Node` 结构体。
  - `github.com/ongridio/ongrid/internal/pkg/errs`——`ErrNotFound` 标准错误。
- **外部库**：`gorm.io/gorm`（含 `ErrRecordNotFound` sentinel）。
- **标准库**：`context`、`errors`、`strings`。
- **被调用方**：`biz/topology` 的节点管理 usecase。

## 6. 并发与资源管理

- **无共享状态**：`NodeRepo` 仅持有不可变 `*gorm.DB`，可并发调用。
- **每次新 session**：`r.db.WithContext(ctx)` 隔离事务状态。
- **ctx 透传**：所有方法首参 `context.Context`，符合 gospec 红线。
- **无锁、无 channel、无缓存**。
- **无资源释放**：GORM 连接池管理。
- **并发 Update 同一节点**：最后写赢；无乐观锁，调用方需自行处理并发冲突。

## 7. 设计模式与亮点

- **wire-ready 构造器 + 编译期断言**：`NewNodeRepo` 返回具体类型，`var _ biz.NodeRepo = (*NodeRepo)(nil)` 让接口变更编译期暴露。
- **GetMany 返回 map**：批量读取返回 `map[id]*Node` 而非切片，便于调用方按 id 查找，避免线性扫描。
- **List + Count 共享 filter**：`applyNodeFilter` 抽出过滤逻辑，保证列表与计数一致，是分页查询的标准模式。
- **map 形式 Updates**：`Update` 用 `map[string]any` 只写指定列，避免 `Save` 覆盖不可变字段。
- **三态删除**：`Delete` 用 `RowsAffected == 0` → `ErrNotFound`，与项目其他 repo 一致。
- **空切片守卫**：`GetMany` 空切片返回空 map，避免 `WHERE id IN ()` SQL 错误。
- **大小写不敏感搜索**：`LOWER(name) LIKE` 模式，跨方言兼容。
- **id 降序排序**：List 按 `id DESC`，最新节点在前，符合常见列表展示习惯。

## 8. 注意事项

- **`Update` 会清空 propsJSON**：`propsJSON=""` 会写入空字符串，清空属性；调用方若想「只改 name」需先 Get 原 propsJSON。
- **`GetMany` 缺失 id 不报错**：返回 map 里没有不存在的 id key；调用方需自行检测 `_, ok := map[id]`。
- **`List` 分页用 limit+offset**：大数据量下 offset 性能差；目前数据量可接受，如成瓶颈改 cursor 分页。
- **`LOWER(name) LIKE` 全表扫描**：`LOWER(name)` 函数索引在 MySQL 上需函数索引支持，SQLite 无此问题；大数据量需在 model 层加 `LOWER(name)` 索引或用 `ILIKE`（Postgres）。
- **并发 Update 无乐观锁**：两个并发 Update 同一节点，最后写赢；若需强一致应在 model 加版本号字段。
- **`Delete` 软删或物理删取决于 model**：本文件不感知软删模式；若 model 用 `gorm.DeletedAt` 则软删，用 delete_marker 则需 model 层处理。
- **`applyNodeFilter` 是局部 helper**：仅在 node_repo.go 内使用；若其他 repo 需类似过滤应各自定义，避免跨域共享。
- **跨方言**：所有查询方言无关；`LOWER(name) LIKE` 在 MySQL/SQLite 均可用。
- **id 类型**：`uint64`（自增整型），与 `NodeRepo` 全部方法一致。
