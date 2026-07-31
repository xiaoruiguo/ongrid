# repo.go 技术实现文档

## 1. 概述

`repo.go` 是 `topology` 包的持久化契约层。它声明四个 repo 接口（`NodeRepo` / `RelationRepo` / `RelationTypeRepo` / `NodeTypeRepo`）与两个 list filter 结构，对应图层的四个核心实体：节点、关系、关系类型、节点类型。接口在消费方定义（biz 层），具体实现位于 data 层，避免循环依赖。包注释明确：本 PR 只落地 persistence + CRUD，推理/遍历辅助（expand_topology、blast-radius 计算）将在 PR-5 随 AIOps tool 落地。

## 2. 包信息

- 包名：`topology`
- 路径：`internal/manager/biz/topology/repo.go`
- 包注释定位职责：自定义关系类型注册的校验规则 + 图编辑原语（Create/Update/List/Delete）
- 导入依赖：
  - 标准库：`context`
  - 内部包：`github.com/ongridio/ongrid/internal/manager/model/topology`（别名 `model`）

## 3. 关键类型与接口

### `NodeListFilter`

```go
type NodeListFilter struct {
    Type   string
    Q      string
    Limit  int
    Offset int
}
```

- `Type`：Node.Type 精确匹配，空 = 任意
- `Q`：Name 大小写不敏感子串匹配
- `Limit`/`Offset`：分页；`Limit==0` 表示无界（调用方应自行设上限）

### `RelationListFilter`

```go
type RelationListFilter struct {
    SrcID      uint64
    DstID      uint64
    SrcOrDstID uint64
    Type       string
    Limit      int
    Offset     int
}
```

- `SrcID`/`DstID`/`Type` 可组合，用于查找特定边
- `SrcOrDstID` 匹配任一端点等于此 id 的行——用于"按节点查邻居"场景
- 所有字段可选

### `NodeRepo`

```go
type NodeRepo interface {
    Create(ctx context.Context, n *model.Node) error
    Update(ctx context.Context, id uint64, name, propsJSON string) error
    Get(ctx context.Context, id uint64) (*model.Node, error)
    GetMany(ctx context.Context, ids []uint64) (map[uint64]*model.Node, error)
    List(ctx context.Context, f NodeListFilter) ([]*model.Node, error)
    Count(ctx context.Context, f NodeListFilter) (int64, error)
    Delete(ctx context.Context, id uint64) error
}
```

`Update` 仅接受 `name` 与 `propsJSON`——`Type` 不可变（避免破坏下游 `device.node_id` 等 FK 引用）。

### `RelationRepo`

```go
type RelationRepo interface {
    Create(ctx context.Context, r *model.Relation) error
    Update(ctx context.Context, id uint64, propsJSON string) error
    Get(ctx context.Context, id uint64) (*model.Relation, error)
    List(ctx context.Context, f RelationListFilter) ([]*model.Relation, error)
    Count(ctx context.Context, f RelationListFilter) (int64, error)
    Delete(ctx context.Context, id uint64) error
}
```

`Update` 仅接受 `propsJSON`——`(src, dst, type)` 三元组是 identity，修改即"删除重建"。

### `RelationTypeRepo`

```go
type RelationTypeRepo interface {
    Upsert(ctx context.Context, rt *model.RelationType) error
    Get(ctx context.Context, name string) (*model.RelationType, error)
    List(ctx context.Context) ([]*model.RelationType, error)
    Delete(ctx context.Context, name string) error
    CountRelationsByType(ctx context.Context, name string) (int64, error)
}
```

- `Upsert` 而非 `Create`/`Update`——operator 重新注册即更新
- `CountRelationsByType`：Delete 前的 guard，避免孤儿化 `relations.type → relation_types.name` 引用

`NodeTypeRepo` 结构与 `RelationTypeRepo` 对称，`CountNodesByType` 用途相同。

## 4. 关键函数与流程

`repo.go` 是纯接口声明，无函数实现。所有逻辑在 `usecase.go`（biz 层校验 + 编排）与 data 层实现。

## 5. 依赖关系

- **`model/topology`**：领域类型 `Node` / `Relation` / `RelationType` / `NodeType`
- **被依赖方**：
  - `Usecase`（`usecase.go`）持有四个 repo 接口字段
  - data 层（`data/topology/store`）实现这四个接口
  - `cmd/ongrid main` 在 wiring 时注入具体实现

## 6. 并发与资源管理

接口声明不涉及并发与资源管理，由实现层负责。biz 层 `Usecase` 不加锁——并发安全依赖 repo 层（DB 事务/连接池）保证。

## 7. 设计模式与亮点

### 接口在消费方定义

四个 repo 接口声明在 biz 包，data 层实现它们。这是 Go 的依赖倒置——biz 不依赖 data，data 依赖 biz 的接口。配合 `model/topology` 的共享领域类型，避免了循环依赖。

### `Update` 的字段最小化

`NodeRepo.Update` 仅接受 `name` + `propsJSON`，不接受 `Type`。`RelationRepo.Update` 仅接受 `propsJSON`，不接受 `(src, dst, type)`。这种"identity 字段不可变"的契约让 usecase 层能在不查 DB 的情况下保证 identity 稳定——下游 FK 引用（如 `device.node_id` 预设 `node.type='device'`）不会被破坏。

### `CountRelationsByType` / `CountNodesByType` 作为 Delete guard

Delete 前查询引用计数，避免孤儿化。这是"防御性删除"——宁可拒绝删除有引用的类型，也不让 `relations.type` 指向不存在的 `relation_types.name`。usecase 层据此返回 `ErrConflict`。

### `SrcOrDstID` 的语义

`RelationListFilter.SrcOrDstID` 匹配任一端点。这是"按节点查所有邻居"的专用字段——避免调用方写两次 `List(SrcID=x)` + `List(DstID=x)` 再合并。考虑到拓扑图渲染的高频场景，这种专用字段是必要的性能优化。

### `GetMany` 批量查询

`NodeRepo.GetMany(ids []uint64) map[uint64]*model.Node` 用于 `CreateRelation` 时一次性校验两个端点存在，避免两次 `Get`。返回 map 而非 slice，让调用方能 O(1) 查找。

### Filter 的 `Limit==0` 无界语义

注释明确"callers should normally cap themselves"——接口允许无界查询（用于后台批量任务），但提醒面向用户的端点应设上限。这种"接口宽容、调用方自律"避免了强制分页带来的批量任务摩擦。

## 8. 注意事项

- **本 PR 仅 CRUD**：包注释明确"reasoning / traversal helpers will land in PR-5"。当前 repo 接口未声明 `Expand` / `BlastRadius` 等遍历方法，PR-5 需扩展接口或新增独立 traversal repo
- **`Update` 不改 identity**：调用方若想"换 type"或"换端点"，必须 Delete + Create。usecase 层未暴露"重建"helper，调用方需自行组合
- **`Count` 与 `List` 分离**：分页场景需两次调用（`List` + `Count`）。若 data 层能用单次查询返回两者，可考虑 `ListWithCount` 优化，但当前接口未提供
- **无 `Upsert` for Node/Relation**：只有 `RelationType` / `NodeType` 有 `Upsert`（operator 重新注册即更新）。Node/Relation 的 identity 是 DB 自增 id，无天然 upsert 语义
- **filter 字段零值语义**：`SrcID==0` / `DstID==0` 在 `RelationListFilter` 中是"不过滤"还是"匹配 0"？data 层实现需明确——典型实现是"零值不过滤"，但 usecase 层调用时应避免传入 0
- **`Limit==0` 无界**：data 层实现必须显式处理 `Limit==0` 为"无 LIMIT"，否则会返回 0 行
