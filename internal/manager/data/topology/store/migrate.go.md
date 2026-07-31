# topology/store/migrate.go

## 1. 概述

本文件实现 topology 子域（`nodes` + `relations` + `relation_types` + `node_types` 四张表）的 schema 迁移与种子数据初始化。被 `cmd/ongrid/main.go` 启动期迁移列表调用。遵循 gospec「expand-contract / 滚动发布兼容」原则——所有步骤幂等，第二次启动后除 AutoMigrate 外都是 no-op。

设计要点：
- **delete_marker 软删迁移**：对 `relations` 表执行 dbx 标准 `NeedsDeleteMarkerMigration → DropIndexes → AutoMigrate → BackfillDeleteMarker` 四步流。
- **种子数据 upsert**：`seedBuiltinRelationTypes` / `seedBuiltinNodeTypes` 用 `OnConflict` 把代码内定义的内置类型同步到数据库，保证代码与数据库定义一致。
- **device → node 回填**：`backfillDeviceNodes` 为存量 device 行创建配对的 `Node(type='device')` 行并写回 `devices.node_id`，用裸 SQL 避免跨域 import。
- **跨域边界守卫**：`backfillDeviceNodes` 用 `db.Migrator().HasTable("devices")` 检测表是否存在，避免在 fresh DB 上崩。

## 2. 包信息

- **包名**：`store`（包注释明确「GORM-backed implementation of the topology repos」）
- **所属模块**：`internal/manager/data/topology/store`
- **依赖方向**：`cmd → data/topology/store → model/topology + pkg/dbx`，符合单服务分层。

## 3. 关键类型与接口

无导出类型，仅一个导出函数 + 三个未导出 helper。

```go
func Migrate(db *gorm.DB) error
func seedBuiltinNodeTypes(db *gorm.DB) error
func seedBuiltinRelationTypes(db *gorm.DB) error
func backfillDeviceNodes(db *gorm.DB) error
```

涉及的对象模型（在 `model/topology` 中定义）：
```go
&model.Node{}         // 拓扑节点（device / service / app 等）
&model.Relation{}     // 节点间关系（有向边）
&model.RelationType{} // 关系类型定义（内置 6 种）
&model.NodeType{}     // 节点类型定义（内置）
```

被 DropIndexes 显式删除的索引名：
- `idx_relations_src_dst_type`

## 4. 关键函数与流程

### `Migrate(db *gorm.DB) error`

- **职责**：注册四张表 + 种子 + backfill，全部幂等。
- **流程**：
  1. **delete_marker 迁移前置**：
     - `dbx.NeedsDeleteMarkerMigration(db, Relation{}.TableName())` 检测 `relations` 表是否还在用旧 `deleted_at` 模式。
     - 若需迁移，`dbx.DropIndexes(db, &model.Relation{}, "idx_relations_src_dst_type")` 删除含 `deleted_at` 的旧复合索引（新软删模式下不适用）。
  2. **AutoMigrate**：`db.AutoMigrate(&Node{}, &Relation{}, &RelationType{}, &NodeType{})`，只增不删。
  3. **delete_marker backfill**：`dbx.BackfillDeleteMarker(db, Relation{}.TableName())` 给存量 `relations` 行填默认值 `0`（未删除）。
  4. **种子 RelationType**：`seedBuiltinRelationTypes(db)`。
  5. **种子 NodeType**：`seedBuiltinNodeTypes(db)`。
  6. **device → node 回填**：`backfillDeviceNodes(db)`。
- **错误处理**：每步错误立即返回，中断迁移。

### `seedBuiltinNodeTypes(db *gorm.DB) error`

- **职责**：把代码内定义的 `model.BuiltinNodeTypes()` upsert 到数据库。
- **流程**：`db.Clauses(clause.OnConflict{Columns: [{name}], DoUpdates: [display_name, display_name_en, builtin, tier, description, updated_at]}).Create(&seeds)`。
- **设计意图**：按 `name` 主键 upsert，冲突时刷新展示名与语义字段，保证代码定义与数据库一致——未来若改内置类型定义（如改 display_name），下次启动自动同步。

### `seedBuiltinRelationTypes(db *gorm.DB) error`

- **职责**：把代码内定义的 `model.BuiltinRelationTypes()` upsert 到数据库。
- **流程**：`db.Clauses(clause.OnConflict{Columns: [{name}], DoUpdates: [display_name, display_name_en, builtin, propagates_failure, direction, semantics_tag, description, updated_at]}).Create(&seeds)`。
- **设计意图**：同 `seedBuiltinNodeTypes`，但更新列更多（含 `propagates_failure`、`direction`、`semantics_tag` 等语义字段）；注释明确「这些字段 MUST track the in-code definition」，方便未来修正内置类型定义而无需写独立 migration。

### `backfillDeviceNodes(db *gorm.DB) error`

- **职责**：为 `devices` 表中 `node_id IS NULL` 的行创建配对的 `Node(type='device')` 行并写回 `node_id`。
- **流程**：
  1. **表存在守卫**：`db.Migrator().HasTable("devices")`——fresh DB（device 迁移未跑）时直接返回 nil。
  2. `Raw("SELECT id, name FROM devices WHERE node_id IS NULL AND deleted_at IS NULL").Scan(&rows)`——跳过软删 device。
  3. 对每个 device：
     - name 为空时 fallback `fmt.Sprintf("device-%d", d.ID)`（edge 注册但未发 host_info 的情况）。
     - `db.Create(&Node{Type: "device", Name: name})` 创建节点。
     - `db.Exec("UPDATE devices SET node_id = ? WHERE id = ?", n.ID, d.ID)` 写回 device.node_id。
- **幂等性**：第二次启动时所有 device 都有 `node_id`，SELECT 返回空，循环不执行。
- **跨域边界设计**：注释明确「intentionally inspect / mutate the `devices` table directly via raw SQL rather than importing the device model package — keeps the topology data layer free of cross-domain imports」。这是 gospec monorepo 红线（`internal/<domain>` 禁止直接 import）下的妥协——用裸 SQL 操作 devices 表，依赖 schema 稳定（`id BIGINT, name VARCHAR, node_id BIGINT nullable`）。
- **迁移顺序依赖**：注释说明 `cmd/ongrid` 迁移顺序把 `device.Migrate` 放在 `topology.Migrate` 之前，正常启动 `devices` 表一定存在；`HasTable` 守卫是为 fresh DB 测试场景兜底。

## 5. 依赖关系

- **内部包**：
  - `github.com/ongridio/ongrid/internal/manager/model/topology`——`Node`、`Relation`、`RelationType`、`NodeType` 结构体 + `BuiltinNodeTypes()` / `BuiltinRelationTypes()` 种子定义 + `NodeTypeDevice` 常量。
  - `github.com/ongridio/ongrid/internal/pkg/dbx`——`NeedsDeleteMarkerMigration`、`DropIndexes`、`BackfillDeleteMarker` 三个迁移辅助函数。
- **外部库**：
  - `gorm.io/gorm`——`*gorm.DB`、`AutoMigrate`、`Migrator().HasTable`、`Raw`、`Exec`、`Create`。
  - `gorm.io/gorm/clause`——`OnConflict`、`Column`、`AssignmentColumns`。
- **标准库**：`fmt`。
- **被调用方**：`cmd/ongrid/main.go` 启动期迁移编排。

## 6. 并发与资源管理

- **无并发状态**：迁移在启动期单线程顺序执行，无锁、无 channel、无缓存。
- **无 ctx**：迁移函数签名不带 `context.Context`（启动期同步执行，无需取消）。
- **无资源释放**：不开游标、不持连接，连接池由 GORM 管理。
- **backfill 循环无事务**：`backfillDeviceNodes` 逐 device 创建 node + 更新 device.node_id，**未包在事务里**；若中途失败会留下部分 device 有 node_id、部分没有，但下次启动幂等补齐（因为 SELECT 只取 `node_id IS NULL`）。

## 7. 设计模式与亮点

- **delete_marker 四步法**：`检测 → 删旧索引 → AutoMigrate → backfill`，与 `report/store/migrate.go` 同款模式。
- **种子 upsert 同步代码定义**：`OnConflict` + `DoUpdates` 把代码内内置类型定义同步到数据库，让代码成为 single source of truth，无需写独立 migration 修正内置类型。
- **跨域裸 SQL**：`backfillDeviceNodes` 用裸 SQL 操作 `devices` 表，避免跨域 import，是 monorepo 边界下的标准妥协。
- **表存在守卫**：`HasTable("devices")` 让 fresh DB 测试场景不崩。
- **name fallback**：device name 为空时用 `device-<id>` 兜底，保证 node.name 有可用展示。
- **幂等性全方位**：AutoMigrate 幂等、种子 upsert 幂等、backfill 按 `node_id IS NULL` 过滤幂等——第二次启动后除 AutoMigrate 外都是 no-op。
- **更新列白名单**：种子 upsert 的 `DoUpdates` 明确列出更新列，不碰主键与 `created_at`。
- **注释解释设计决策**：每个关键决策都有注释（跨域裸 SQL 原因、迁移顺序依赖、字段 MUST track 代码定义），便于后续维护。

## 8. 注意事项

- **`backfillDeviceNodes` 无事务**：中途失败会留下部分状态，依赖下次启动幂等补齐；若需严格事务应包 `Transaction`，但当前设计可接受。
- **跨域裸 SQL 依赖 schema 稳定**：`devices` 表 schema 变更（如改 `node_id` 列名）需同步更新本文件的 SQL；注释已提醒「any schema change there should update this query too」。
- **迁移顺序依赖**：依赖 `cmd/ongrid` 把 `device.Migrate` 放在 `topology.Migrate` 之前；`HasTable` 守卫只是测试场景兜底，生产环境必须保证顺序。
- **种子 upsert 会覆盖数据库手动修改**：若管理员手动改了内置 `RelationType` 的 `display_name`，下次启动会被代码定义覆盖；这是「代码为 single source of truth」的代价。
- **`BackfillDeleteMarker` 用默认值 0**：与 `report/store/migrate.go` 的 `BackfillDeleteMarkerWithValue("1")` 不同——本表用 `0` 表示未删除，符合多数表约定。
- **`idx_relations_src_dst_type` 被删**：迁移后该旧索引不再存在；若业务层 SQL 显式 hint 该索引名会失败。AutoMigrate 会按新 schema 模型重建等价索引（若定义了）。
- **`backfillDeviceNodes` 只处理 `deleted_at IS NULL`**：软删 device 不创建 node；若 device 被恢复（un-delete）需另行处理 node 创建。
- **跨方言**：所有查询方言无关；`OnConflict` 自动适配 MySQL/SQLite；`Raw` / `Exec` 用标准 SQL。
- **失败即启动失败**：任何一步错误让进程启动中断，符合「服务不应带着半套 schema 跑」红线。
- **不删列**：本迁移不删除任何旧列；下线列需走独立 contract 阶段。
