# `plugin_config.go` 技术实现文档

> 源文件：`internal/manager/model/edge/plugin_config.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/model/edge`

## 1. 概述

本文件定义 `PluginConfig` 实体：per-edge per-plugin 的启用标志 + 插件特定 spec。schema 故意窄：(edge_id, plugin_name) 唯一；enabled 控制是否启动；spec_json 承载 plugin 特定配置（如 logs 插件的 journald_units / file_paths）。设计要点：endpoint URL 不在此行（派生自 manager-wide ONGRID_PUBLIC_URL）；auth 凭据不在此行（edge 用自身 access_key/secret_key）；edge_id 由 tunnel session 注入。红线：`SpecJSON` 无 DEFAULT（MySQL Error 1101）；biz 层 Set 总写至少 "{}"。

## 2. 包信息

- **包名**：`edge`（与 model.go 同包）
- **所属模块**：`internal/manager/model/`
- **依赖方向**：被 `manager/biz/edge` 的 PluginConfigUC 调用；依赖 `gorm.io/plugin/soft_delete`、`time`

## 3. 关键类型与接口

```go
type PluginConfig struct {
    ID         uint64 `gorm:"primaryKey;autoIncrement"`
    EdgeID     uint64 `gorm:"not null;column:edge_id;uniqueIndex:uk_edge_plugin,priority:1;index:idx_edge_plugin_edge"`
    PluginName string `gorm:"size:32;not null;column:plugin_name;uniqueIndex:uk_edge_plugin,priority:2"`
    Enabled    bool   `gorm:"not null;default:false;column:enabled"`
    // SpecJSON 无 DEFAULT — MySQL 拒绝 TEXT DEFAULT（Error 1101）
    SpecJSON     string                `gorm:"type:text;not null;column:spec_json"`
    CreatedAt    time.Time             `gorm:"column:created_at"`
    UpdatedAt    time.Time             `gorm:"column:updated_at"`
    DeletedAt    *time.Time            `gorm:"index;column:deleted_at"`
    DeleteMarker soft_delete.DeletedAt `gorm:"column:delete_marker;not null;default:0;softDelete:milli,DeletedAtField:DeletedAt;uniqueIndex:uk_edge_plugin,priority:3"`
}

// Plugin name 常量
const (
    PluginNameMetrics        = "metrics"
    PluginNameLogs           = "logs"
    PluginNameTraces         = "traces"
    PluginNameProfiles       = "profiles"
    PluginNameHostMetrics    = "hostmetrics"
    PluginNameProcMetrics    = "procmetrics"
    PluginNameCustomMetrics  = "custommetrics"
    PluginNameDatabaseMetrics = "databasemetrics"
)
```

## 4. 关键函数与流程

### `PluginConfig.TableName`
- **签名**：`func (PluginConfig) TableName() string`
- **职责**：固定表名 `edge_plugin_configs`

### `IsKnownPluginName`
- **签名**：`func IsKnownPluginName(n string) bool`
- **职责**：判断是否为 manager 当前已知的 plugin 名
- **流程**：switch n 是否在 8 个已知常量中
- **覆盖**：metrics/logs/traces/profiles（原 OTel 信号名）+ hostmetrics/procmetrics/custommetrics/databasemetrics（Prom 生态 exporter 包装）

## 5. 依赖关系

- **内部包**：`internal/edgeagent/plugins/<name>` 包（plugin 实现端，与本表 lock-step）
- **外部库**：`gorm.io/plugin/soft_delete`、`time`
- **被调用方**：`manager/biz/edge` 的 PluginConfigUC；supervisor 启动时按 Enabled 决定是否起 plugin

## 6. 并发与资源管理

- 本文件仅定义 schema，无锁 / channel / 缓存
- `soft_delete.DeletedAt` 实现 milli 精度软删除；DeleteMarker 加入 unique index 让软删后可重建同 (edge, plugin)

## 7. 设计模式与亮点

- **schema 故意窄**：仅 enabled + spec_json；endpoint / auth / edge_id 都不在行内
- **endpoint URL 派生自 ONGRID_PUBLIC_URL**：避免 per-row drift；所有 edge 永远指向 canonical manager URL
- **auth 用 edge 自身 access_key/secret_key**：edge 已 enrollment；不需额外凭据
- **edge_id 由 tunnel session 注入**：wire payload 不带 edge_id；manager 在外发时填
- **SpecJSON free-form**：manager 按 plugin_name 在 biz 层校验 shape；存储层不感知，schema 演进无需迁移
- **8 个已知 plugin 名常量**：4 个 OTel 信号名 + 4 个 Prom 生态 exporter 包装
- **IsKnownPluginName 校验**：service 层在写库前调，拒绝未知 plugin 名
- **MySQL TEXT DEFAULT 兼容**：SpecJSON 无 default；biz 层 Set 总写至少 "{}"
- **(edge_id, plugin_name) 唯一**：同一 edge 同一 plugin 仅一行；DeleteMarker 加入 unique 让软删后可重建

## 8. 注意事项

- **SpecJSON 必填**：写入前 biz 层应校验 plugin 特定 shape；空 spec 应至少写 "{}"
- **Enabled 默认 false**：新建配置需显式启用
- **PluginName size:32**：与常量集合匹配
- **与 internal/edgeagent/plugins/<name> 包 lock-step**：新增 plugin 需同时更新本常量集与 plugin 实现
- **hostmetrics / procmetrics 是子进程 plugin**：edge 把 node_exporter / process-exporter 当 bundled subprocess 起
- **custommetrics / databasemetrics**：高级用户自定义 / 数据库特定采集
- **endpoint URL 不在行内**：若 per-edge 需不同 endpoint，需扩 schema
- **软删 vs Enabled**：删除走 DeletedAt；Enabled 仅控制启动
