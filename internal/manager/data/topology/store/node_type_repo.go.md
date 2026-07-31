# topology/store/node_type_repo.go

## 1. 概述

本文件实现 `node_types` 表的 GORM-backed 持久化，满足 `biz/topology.NodeTypeRepo` 接口。覆盖能力：upsert（按 name 主键）、按 name 读取、列表（按 tier + name 排序）、删除、按类型计数节点（删除前安全守卫）。

设计要点：
- **name 作为主键**：`NodeType` 用 `name` 作主键（非自增整型），是业务语义标识（如 `device` / `service` / `app`）。
- **upsert 复用**：`Upsert` 同时被 `migrate.go` 的内置种子初始化与 operator 注册自定义类型使用，单一入口保证一致性。
- **tier 排序**：`List` 按 `(tier ASC, name ASC)` 排序，tier 表示拓扑层级（自上而下），保证展示顺序符合拓扑层级直觉。
- **删除前计数守卫**：`CountNodesByType` 让 biz 层在删除类型前检查是否还有节点引用，避免悬挂引用。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/topology/store`
- **依赖方向**：`controlplane → biz/topology → data/topology/store → model/topology + pkg/errs`；接口 `biz.NodeTypeRepo` 在消费方 biz 层定义，编译期断言 `var _ biz.NodeTypeRepo = (*NodeTypeRepo)(nil)`。

## 3. 关键类型与接口

```go
// NodeTypeRepo 是 biz/topology.NodeTypeRepo 的 GORM 实现。
type NodeTypeRepo struct{ db *gorm.DB }

func NewNodeTypeRepo(db *gorm.DB) *NodeTypeRepo

// 编译期接口断言
var _ biz.NodeTypeRepo = (*NodeTypeRepo)(nil)
```

`model.NodeType` 结构体字段（隐含）：`name string`（主键）、`display_name string`、`display_name_en string`、`tier int`、`description string`、`updated_at`。

## 4. 关键函数与流程

### `NewNodeTypeRepo(db *gorm.DB) *NodeTypeRepo`

构造器，直接包装 `*gorm.DB`。

### `Upsert(ctx, nt *model.NodeType) error`

- **职责**：按 name 主键 upsert；冲突时刷新展示名与语义字段。
- **流程**：
  ```go
  r.db.WithContext(ctx).Clauses(clause.OnConflict{
      Columns: []clause.Column{{Name: "name"}},
      DoUpdates: clause.AssignmentColumns([]string{
          "display_name", "display_name_en", "tier", "description", "updated_at",
      }),
  }).Create(nt)
  ```
- **使用场景**：
  - `migrate.go` 的 `seedBuiltinNodeTypes` 调此方法同步内置类型。
  - operator 注册自定义节点类型。
- **设计意图**：单一 upsert 入口保证内置种子与 operator 注册走相同路径，避免逻辑分叉。
- **更新列白名单**：`display_name` / `display_name_en` / `tier` / `description` / `updated_at`——不碰 `name`（主键）与 `created_at`。

### `Get(ctx, name string) (*model.NodeType, error)`

- **职责**：按 name 主键取单行。
- **流程**：`Where("name = ?", name).First(&nt)`；`gorm.ErrRecordNotFound` → `errs.ErrNotFound`。

### `List(ctx) ([]*model.NodeType, error)`

- **职责**：返回所有节点类型，按 `(tier ASC, name ASC)` 排序。
- **流程**：`Order("tier ASC, name ASC").Find(&out)`。
- **排序语义**：tier 表示拓扑层级（数字小者在上层），同 tier 内按 name 字典序，保证展示稳定。

### `Delete(ctx, name string) error`

- **职责**：删除节点类型。
- **流程**：`Where("name = ?", name).Delete(&model.NodeType{})`；`RowsAffected == 0` → `errs.ErrNotFound`。
- **安全守卫**：biz 层应在调用 Delete 前先调 `CountNodesByType` 检查是否还有节点引用，避免悬挂引用。

### `CountNodesByType(ctx, name string) (int64, error)`

- **职责**：统计 `nodes` 表中 `type = name` 的行数。
- **流程**：`Model(&model.Node{}).Where("type = ?", name).Count(&n)`。
- **使用场景**：`Usecase.DeleteNodeType` 调此方法作为安全守卫，若 `> 0` 则拒绝删除并提示「类型仍被 N 个节点引用」。
- **跨表查询**：查的是 `nodes` 表（不是 `node_types` 表），用 `model.Node` 作 Model。

## 5. 依赖关系

- **内部包**：
  - `github.com/ongridio/ongrid/internal/manager/biz/topology`（biz）——`NodeTypeRepo` 接口。
  - `github.com/ongridio/ongrid/internal/manager/model/topology`——`NodeType`、`Node` 结构体。
  - `github.com/ongridio/ongrid/internal/pkg/errs`——`ErrNotFound` 标准错误。
- **外部库**：
  - `gorm.io/gorm`（含 `ErrRecordNotFound` sentinel）。
  - `gorm.io/gorm/clause`——`OnConflict`、`Column`、`AssignmentColumns`。
- **标准库**：`context`、`errors`。
- **被调用方**：`biz/topology` 的节点类型管理 usecase；`migrate.go` 的 `seedBuiltinNodeTypes` 也调 `Upsert`（但 migrate 路径直接用 `db.Clauses`，不经过本 repo）。

## 6. 并发与资源管理

- **无共享状态**：`NodeTypeRepo` 仅持有不可变 `*gorm.DB`，可并发调用。
- **每次新 session**：`r.db.WithContext(ctx)` 隔离事务状态。
- **ctx 透传**：所有方法首参 `context.Context`，符合 gospec 红线。
- **无锁、无 channel、无缓存**：节点类型缓存应在 biz 层（data 层只管读写）。
- **无资源释放**：GORM 连接池管理。
- **并发 Upsert 同 name**：`ON CONFLICT DO UPDATE` 保证最后写赢；无乐观锁。

## 7. 设计模式与亮点

- **name 作主键**：业务语义标识作主键，便于 operator 注册与引用（如 `Node.Type = "device"` 直接引用 name）。
- **upsert 单一入口**：内置种子与 operator 注册走相同路径，避免逻辑分叉。
- **删除前计数守卫**：`CountNodesByType` 让 biz 层在删除前检查引用，避免悬挂引用，是「安全删除」的标准模式。
- **tier 排序**：`List` 按 `(tier, name)` 排序，符合拓扑层级展示直觉。
- **编译期接口断言**：`var _ biz.NodeTypeRepo = (*NodeTypeRepo)(nil)` 让接口变更编译期暴露。
- **更新列白名单**：`DoUpdates` 明确列出更新列，不碰主键与 `created_at`。
- **三态删除**：`Delete` 用 `RowsAffected == 0` → `ErrNotFound`，与项目其他 repo 一致。

## 8. 注意事项

- **`Upsert` 会覆盖数据库手动修改**：若管理员手动改了 `display_name`，下次 operator 调 Upsert 会覆盖；内置类型由 `migrate.go` 的 `seedBuiltinNodeTypes` 同步代码定义。
- **`Delete` 不检查引用**：本方法不检查是否还有节点引用；biz 层必须先调 `CountNodesByType`，否则会留下悬挂 `nodes.type` 引用。
- **`CountNodesByType` 查的是 nodes 表**：用 `model.Node` 作 Model，不是 `model.NodeType`；跨表查询，但同域内 import 合规。
- **`List` 不分页**：节点类型数量通常很少（十几个），全量返回可接受。
- **并发 Upsert 无乐观锁**：两个并发 Upsert 同 name，最后写赢；无版本号字段。
- **tier 语义**：tier 数字小者在上层；具体语义由 model 层定义（如 tier=1 是 internet 层，tier=5 是 device 层）。
- **跨方言**：所有查询方言无关；`OnConflict` 自动适配 MySQL/SQLite。
- **name 大小写敏感**：`Where("name = ?", name)`，MySQL 默认大小写不敏感（取决于 collation），SQLite 大小写敏感；如需统一行为应在 biz 层 normalize。
