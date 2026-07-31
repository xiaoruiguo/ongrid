# topology/store/relation_type_repo.go

## 1. 概述

本文件实现 `relation_types` 表的 GORM-backed 持久化，满足 `biz/topology.RelationTypeRepo` 接口。覆盖能力：upsert（按 name 主键）、按 name 读取、列表（按 name 排序）、删除、按类型计数关系（删除前安全守卫）。

设计要点：
- **name 作为主键**：`RelationType` 用 `name` 作主键（非自增整型），是业务语义标识（如 `connects` / `depends_on` / `monitors`）。
- **upsert 复用**：`Upsert` 是 operator 注册自定义关系类型的入口；内置种子初始化在 `migrate.go` 用相同的 `OnConflict` 模式（但不经过本 repo，直接用 `db.Clauses`）。
- **删除前计数守卫**：`CountRelationsByType` 让 biz 层在删除类型前检查是否还有关系引用，避免悬挂引用。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/topology/store`
- **依赖方向**：`controlplane → biz/topology → data/topology/store → model/topology + pkg/errs`；接口 `biz.RelationTypeRepo` 在消费方 biz 层定义，编译期断言 `var _ biz.RelationTypeRepo = (*RelationTypeRepo)(nil)`。

## 3. 关键类型与接口

```go
// RelationTypeRepo 是 biz/topology.RelationTypeRepo 的 GORM 实现。
type RelationTypeRepo struct{ db *gorm.DB }

func NewRelationTypeRepo(db *gorm.DB) *RelationTypeRepo

// 编译期接口断言
var _ biz.RelationTypeRepo = (*RelationTypeRepo)(nil)
```

`model.RelationType` 结构体字段（隐含）：`name string`（主键）、`display_name string`、`display_name_en string`、`propagates_failure bool`、`direction string`、`semantics_tag string`、`description string`、`updated_at`。

## 4. 关键函数与流程

### `NewRelationTypeRepo(db *gorm.DB) *RelationTypeRepo`

构造器，直接包装 `*gorm.DB`。

### `Upsert(ctx, rt *model.RelationType) error`

- **职责**：按 name 主键 upsert；冲突时刷新展示名与语义字段。
- **流程**：
  ```go
  r.db.WithContext(ctx).Clauses(clause.OnConflict{
      Columns: []clause.Column{{Name: "name"}},
      DoUpdates: clause.AssignmentColumns([]string{
          "display_name", "display_name_en", "propagates_failure", "direction",
          "semantics_tag", "description", "updated_at",
      }),
  }).Create(rt)
  ```
- **使用场景**：operator 注册自定义关系类型；内置种子初始化在 `migrate.go` 用相同模式但直接调 `db.Clauses`（不经过本 repo）。
- **更新列白名单**：`display_name` / `display_name_en` / `propagates_failure` / `direction` / `semantics_tag` / `description` / `updated_at`——不碰 `name`（主键）与 `created_at`。
- **语义字段同步**：`propagates_failure` / `direction` / `semantics_tag` 是关系类型的语义定义，upsert 时会刷新，保证 operator 修改能生效。

### `Get(ctx, name string) (*model.RelationType, error)`

- **职责**：按 name 主键取单行。
- **流程**：`Where("name = ?", name).First(&rt)`；`gorm.ErrRecordNotFound` → `errs.ErrNotFound`。

### `List(ctx) ([]*model.RelationType, error)`

- **职责**：返回所有关系类型，按 name 升序。
- **流程**：`Order("name ASC").Find(&out)`。
- **排序语义**：按 name 字典序，便于前端展示与查找。

### `Delete(ctx, name string) error`

- **职责**：删除关系类型。
- **流程**：`Where("name = ?", name).Delete(&model.RelationType{})`；`RowsAffected == 0` → `errs.ErrNotFound`。
- **安全守卫**：biz 层应在调用 Delete 前先调 `CountRelationsByType` 检查是否还有关系引用，避免悬挂引用。

### `CountRelationsByType(ctx, name string) (int64, error)`

- **职责**：统计 `relations` 表中 `type = name` 的行数。
- **流程**：`Model(&model.Relation{}).Where("type = ?", name).Count(&n)`。
- **使用场景**：`Usecase.DeleteRelationType` 调此方法作为安全守卫，若 `> 0` 则拒绝删除并提示「类型仍被 N 个关系引用」。
- **跨表查询**：查的是 `relations` 表（不是 `relation_types` 表），用 `model.Relation` 作 Model。

## 5. 依赖关系

- **内部包**：
  - `github.com/ongridio/ongrid/internal/manager/biz/topology`（biz）——`RelationTypeRepo` 接口。
  - `github.com/ongridio/ongrid/internal/manager/model/topology`——`RelationType`、`Relation` 结构体。
  - `github.com/ongridio/ongrid/internal/pkg/errs`——`ErrNotFound` 标准错误。
- **外部库**：
  - `gorm.io/gorm`（含 `ErrRecordNotFound` sentinel）。
  - `gorm.io/gorm/clause`——`OnConflict`、`Column`、`AssignmentColumns`。
- **标准库**：`context`、`errors`。
- **被调用方**：`biz/topology` 的关系类型管理 usecase。

## 6. 并发与资源管理

- **无共享状态**：`RelationTypeRepo` 仅持有不可变 `*gorm.DB`，可并发调用。
- **每次新 session**：`r.db.WithContext(ctx)` 隔离事务状态。
- **ctx 透传**：所有方法首参 `context.Context`，符合 gospec 红线。
- **无锁、无 channel、无缓存**：关系类型缓存应在 biz 层。
- **无资源释放**：GORM 连接池管理。
- **并发 Upsert 同 name**：`ON CONFLICT DO UPDATE` 保证最后写赢；无乐观锁。

## 7. 设计模式与亮点

- **name 作主键**：业务语义标识作主键，便于 operator 注册与引用（如 `Relation.Type = "connects"` 直接引用 name）。
- **删除前计数守卫**：`CountRelationsByType` 让 biz 层在删除前检查引用，避免悬挂引用，是「安全删除」的标准模式（与 `node_type_repo.go` 的 `CountNodesByType` 对称）。
- **upsert 语义字段同步**：`propagates_failure` / `direction` / `semantics_tag` 等语义字段在 upsert 时刷新，保证 operator 修改生效。
- **编译期接口断言**：`var _ biz.RelationTypeRepo = (*RelationTypeRepo)(nil)` 让接口变更编译期暴露。
- **更新列白名单**：`DoUpdates` 明确列出更新列，不碰主键与 `created_at`。
- **三态删除**：`Delete` 用 `RowsAffected == 0` → `ErrNotFound`，与项目其他 repo 一致。
- **与 NodeTypeRepo 对称设计**：两个 type repo 的方法集几乎对称（Upsert / Get / List / Delete / CountXxxByType），降低认知负担。

## 8. 注意事项

- **`Upsert` 会覆盖数据库手动修改**：若管理员手动改了 `display_name`，下次 operator 调 Upsert 会覆盖；内置类型由 `migrate.go` 的 `seedBuiltinRelationTypes` 同步代码定义。
- **`Delete` 不检查引用**：本方法不检查是否还有关系引用；biz 层必须先调 `CountRelationsByType`，否则会留下悬挂 `relations.type` 引用。
- **`CountRelationsByType` 查的是 relations 表**：用 `model.Relation` 作 Model，不是 `model.RelationType`；跨表查询，但同域内 import 合规。
- **`List` 不分页**：关系类型数量通常很少（十几个），全量返回可接受。
- **并发 Upsert 无乐观锁**：两个并发 Upsert 同 name，最后写赢；无版本号字段。
- **内置类型不可删**：biz 层通常禁止删除 `builtin = true` 的关系类型；本 data 层不强制，需 biz 层守卫。
- **跨方言**：所有查询方言无关；`OnConflict` 自动适配 MySQL/SQLite。
- **name 大小写敏感**：`Where("name = ?", name)`，MySQL 默认大小写不敏感（取决于 collation），SQLite 大小写敏感；如需统一行为应在 biz 层 normalize。
- **`migrate.go` 内置种子不经过本 repo**：`seedBuiltinRelationTypes` 直接用 `db.Clauses`，与本 repo 的 `Upsert` 逻辑相同但路径不同；改 Upsert 逻辑时记得同步 migrate.go。
