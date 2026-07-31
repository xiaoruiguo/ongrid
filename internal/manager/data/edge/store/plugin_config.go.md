# `plugin_config.go` 技术实现文档

> 源文件：`internal/manager/data/edge/store/plugin_config.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/edge/store`

## 1. 概述

本文件实现 `edge_plugin_configs` 表的持久化层，管理每个 edge 的 plugin 配置（enabled / spec_json）。核心设计：`Upsert` 用 `ON CONFLICT (edge_id, plugin_name, delete_marker) DO UPDATE` 原子 upsert；`CountByPlugin` 聚合各 plugin 在多少 edge 上启用，供 Integrations UI 卡片显示 "active on N/M edges"。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/edge`
- **依赖方向**：被 `internal/manager/biz/edge/plugin_config` 装配；依赖 `internal/manager/model/edge`、`internal/pkg/errs`、`gorm.io/gorm`、`gorm.io/gorm/clause`。

## 3. 关键类型与接口

```go
// PluginConfigRepo 持久化 edge_plugin_configs。
// biz 通过 internal/manager/biz/edge/plugin_config.go 的窄接口消费。
type PluginConfigRepo struct {
    db *gorm.DB
}
```

## 4. 关键函数与流程

### `NewPluginConfigRepo`
- **签名**：`func NewPluginConfigRepo(db *gorm.DB) *PluginConfigRepo`

### `ListByEdge`
- **签名**：`func (r *PluginConfigRepo) ListByEdge(ctx, edgeID uint64) ([]*model.PluginConfig, error)`
- **职责**：返回该 edge 的全部 plugin config，按 `plugin_name ASC` 排序（稳定 UI 渲染）。

### `Get`
- **签名**：`func (r *PluginConfigRepo) Get(ctx, edgeID uint64, plugin string) (*model.PluginConfig, error)`
- **职责**：按 (edge_id, plugin_name) 取单条；`gorm.ErrRecordNotFound` → `ErrNotFound`。

### `Upsert`
- **签名**：`func (r *PluginConfigRepo) Upsert(ctx, in *model.PluginConfig) (*model.PluginConfig, error)`
- **职责**：插入或更新 (edge_id, plugin_name) 行，返回持久化行（含 ID + timestamps）。
- **流程**：
  1. `in == nil` → `ErrInvalid`
  2. `CreatedAt.IsZero()` → `time.Now().UTC()`
  3. `UpdatedAt = now`
  4. `ON CONFLICT (edge_id, plugin_name, delete_marker) DO UPDATE` 更新 enabled / spec_json / updated_at
- **关键约束**：ON CONFLICT 列含 delete_marker，让软删行复活时正确 upsert。

### `Delete`
- **签名**：`func (r *PluginConfigRepo) Delete(ctx, edgeID uint64, plugin string) error`
- **职责**：删 (edge_id, plugin_name) 行。幂等——不存在时无错。

### `CountByPlugin`
- **签名**：`func (r *PluginConfigRepo) CountByPlugin(ctx) (map[string]int64, error)`
- **职责**：返回各 plugin 名 → 启用 edge 数的 map。供 Integrations UI 卡片显示 "active on N/M edges"。
- **实现**：`SELECT plugin_name, count(*) WHERE enabled = true GROUP BY plugin_name`，Scan 到匿名 struct 后转 map。

## 5. 依赖关系

- **内部包**：`internal/manager/model/edge`、`internal/pkg/errs`
- **外部库**：`gorm.io/gorm`、`gorm.io/gorm/clause`、`context`、`errors`、`time`
- **被调用方**：`internal/manager/biz/edge/plugin_config` usecase；Integrations UI

## 6. 并发与资源管理

- **无显式锁**：依赖 ON CONFLICT 原子 upsert。
- **ctx 透传**：所有方法首参 ctx。

## 7. 设计模式与亮点

- **ON CONFLICT DO UPDATE 原子 upsert**：避免 read-modify-write 竞态；含 delete_marker 列让软删行复活正确。
- **CountByPlugin 聚合**：单查询 + GROUP BY，避免 N 次 per-plugin count。
- **ListByEdge 稳定排序**：`plugin_name ASC` 保证 UI 渲染顺序稳定。
- **Delete 幂等**：不存在时无错，简化 caller。

## 8. 注意事项

- **Upsert ON CONFLICT 列含 delete_marker**：软删行复活时 upsert 正确；如移除 delete_marker 列需同步更新 ON CONFLICT 列。
- **CountByPlugin 仅算 enabled=true**：disabled plugin 不计入；UI 显示 "active on N/M edges" 时分母需另查总 edge 数。
- **Delete 软删**：使用 gorm 默认软删；硬删需 Unscoped。
- **Upsert CreatedAt 兜底**：caller 未填时 repo 补 UTC now；但 ON CONFLICT DO UPDATE 不会更新 CreatedAt（仅 updated_at），保证创建时间不可变。
