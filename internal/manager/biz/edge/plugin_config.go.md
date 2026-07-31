# `plugin_config.go` 技术实现文档

> 源文件：`internal/manager/biz/edge/plugin_config.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/edge`

## 1. 概述

本文件实现 `PluginConfigUC` —— per-edge 插件配置（hostmetrics/procmetrics/metrics/logs/traces/profiles/custommetrics/databasemetrics）的 use-case。两大消费者：HTTP API（UI list/set/delete）和 Tunnel RPC（edge 调 `FetchForEdge` 拿 wire snapshot）。核心特性：**默认启用策略**（新 edge 自动开 5 个观测插件）、DB 显式行覆盖默认、mutating 后 fire-and-forget 通知 edge reload。

## 2. 包信息

- **包名**：`edge`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 HTTP handler / tunnel RPC 调用；依赖 `model/edge`、`pkg/errs`、`pkg/tunnel`

## 3. 关键类型与接口

```go
type PluginConfigRepo interface {
    ListByEdge(ctx, edgeID uint64) ([]*model.PluginConfig, error)
    Get(ctx, edgeID uint64, plugin string) (*model.PluginConfig, error)
    Upsert(ctx, in *model.PluginConfig) (*model.PluginConfig, error)
    Delete(ctx, edgeID uint64, plugin string) error
    CountByPlugin(ctx) (map[string]int64, error)
}

type EdgeReloadNotifier interface {
    NotifyPluginConfigsChanged(ctx, edgeID uint64) error
}

type DatabaseMetricsSecretWriter interface {
    WriteDatabaseMetricsSecrets(ctx, edgeID uint64, reqs []tunnel.WriteDatabaseMetricsSecretRequest) error
}

type EndpointResolver interface {
    Endpoint(ctx, plugin string) string
}

type PluginConfigUC struct {
    repo         PluginConfigRepo
    notifier     EdgeReloadNotifier       // 可 nil（启动期）
    secretWriter DatabaseMetricsSecretWriter  // 可 nil
    resolver     EndpointResolver         // 必非 nil
    log          *slog.Logger
}

type PluginRow struct {
    PluginName string
    Enabled    bool
    Spec       map[string]interface{}
}

type SetInput struct {
    Enabled bool
    Spec    map[string]interface{}
}

type WireSnapshot struct {
    EdgeID  uint64
    Configs map[string]WireConfig
}

type WireConfig struct {
    Enabled  bool
    Endpoint string
    Spec     map[string]interface{}
}

var pluginDefaultEnabled = map[string]bool{
    model.PluginNameMetrics:     true,
    model.PluginNameHostMetrics: true,
    model.PluginNameProcMetrics: true,
    model.PluginNameLogs:        true,
    model.PluginNameTraces:      true,
}
```

Sentinel：`pluginDefaultEnabled` map —— 新 edge 自动开 5 个观测插件（metrics/hostmetrics/procmetrics/logs/traces）；profiles 默认关（pyroscope agent 不在默认安装包）；custommetrics/databasemetrics 默认关（操作员配置后才开）。

## 4. 关键函数与流程

### `NewPluginConfigUC`
- **签名**：`func NewPluginConfigUC(repo, notifier, resolver, log) *PluginConfigUC`
- **职责**：构造 UC；log nil → Default；notifier 可 nil（启动期）；resolver 必非 nil
- **流程**：返回 `&PluginConfigUC{repo, notifier, resolver, log}`

### `SetNotifier / SetDatabaseMetricsSecretWriter`
- **签名**：post-construction 注入
- **职责**：cmd/ongrid 先构造 UC，等 frontierbound 就绪后回填 notifier / secretWriter

### `ListForUI`
- **签名**：`func (uc *PluginConfigUC) ListForUI(ctx, edgeID uint64) ([]PluginRow, error)`
- **职责**：返回 UI 友好视图（含未配 row 的默认值占位）
- **流程**：
  1. `repo.ListByEdge`
  2. 构建 have map
  3. 遍历 knownPlugins（8 个）：默认 `Enabled = pluginDefaultEnabled[name]`
  4. 有 DB row → **DB 行覆盖默认**（保留操作员 opt-out）
  5. 返回 PluginRow 列表
- **关键设计**：UI 看到稳定的 8 个 toggle，无论是否已配 row

### `Set`
- **签名**：`func (uc *PluginConfigUC) Set(ctx, edgeID uint64, plugin string, in SetInput) (*PluginRow, error)`
- **职责**：upsert 一行 + best-effort 通知 edge reload
- **流程**：
  1. edgeID==0 → ErrInvalid
  2. `model.IsKnownPluginName(plugin)` → 否则 ErrInvalid
  3. **plugin 分支**：
     - custommetrics → `validateCustomMetricsSpec`
     - databasemetrics → `prepareDatabaseMetricsSpec`（拆分 secret 请求 + 加载 previous 用于 diff 删除）
  4. marshal spec → specJSON
  5. `repo.Upsert` 写 row
  6. 有 databaseSecretReqs → `writeDatabaseMetricsSecrets`；失败 → `rollbackPluginConfig`（恢复 previous 或 Delete）
  7. `notify(ctx, edgeID, plugin)` fire-and-forget
  8. 返回 PluginRow
- **错误处理**：unknown plugin ErrInvalid；spec 校验失败 ErrInvalid；secret 写失败回滚 row；notify 失败仅 Warn

### `FetchForEdge`
- **签名**：`func (uc *PluginConfigUC) FetchForEdge(ctx, edgeID uint64) (*WireSnapshot, error)`
- **职责**：tunnel RPC 视图 —— edge supervisor 消费的 wire snapshot
- **流程**：
  1. `repo.ListByEdge`
  2. have map
  3. 遍历 knownPlugins（8 个）：`Endpoint = resolver.Endpoint(ctx, name)`；`Enabled = pluginDefaultEnabled[name]`
  4. 有 DB row → DB 覆盖（保留 opt-out）
  5. enabled → 收集 enabledNames
  6. Info log（edge_id / rows / configs_out / enabled）
  7. 返回 WireSnapshot
- **关键设计**：disabled plugin 也 surface（supervisor 需停正在跑的进程）；Endpoint 是 single source of truth（admin 改 loki.url/tempo.url 自动重定向 edge）

### `CountByPlugin`
- **签名**：`func (uc *PluginConfigUC) CountByPlugin(ctx) (map[string]int64, error)`
- **职责**：UI Integrations 卡片统计；透传 repo

### `notify`
- **签名**：`func (uc *PluginConfigUC) notify(ctx, edgeID uint64, plugin string)`
- **职责**：fire-and-forget 推 reload 信号
- **流程**：
  1. notifier nil → Debug log "notifier not wired; skipping push"
  2. `notifier.NotifyPluginConfigsChanged(ctx, edgeID)`
  3. err → Warn（edge 60s safety net 兜底）

### `rollbackPluginConfig`
- **签名**：`func (uc *PluginConfigUC) rollbackPluginConfig(ctx, edgeID, plugin string, previous *model.PluginConfig) error`
- **职责**：secret 写失败时回滚 row
- **流程**：previous!=nil → Upsert(previous)；否则 Delete

### `decodeSpec`
- **签名**：`func decodeSpec(raw string) map[string]interface{}`
- **职责**：JSON 解码 spec；空字符串/解析失败/空 map → nil

## 5. 依赖关系

- **内部包**：`model/edge`（plugin name 常量 + IsKnownPluginName）、`pkg/errs`、`pkg/tunnel`（WriteDatabaseMetricsSecretRequest）
- **桥接接口**：`EdgeReloadNotifier`（frontierbound.Client 实现）、`DatabaseMetricsSecretWriter`（frontierbound.Client 实现）、`EndpointResolver`（cmd/ongrid 实现，组合 ONGRID_PUBLIC_URL + system_settings）
- **被调用方**：HTTP handler（UI list/set/delete）、tunnel RPC（MethodGetPluginConfigs）

## 6. 并发与资源管理

- **无共享状态**：UC 仅持有不可变 repo + notifier + secretWriter + resolver + log
- **无锁**：所有状态在 DB
- **notify fire-and-forget**：best-effort；失败仅 Warn；edge 60s safety net 兜底
- **ctx 透传**：所有 IO 第一参 context

## 7. 设计模式与亮点

- **默认启用策略**：`pluginDefaultEnabled` map 让新 edge 自动开 5 个观测插件；SPA Monitor/Logs/Traces 页面"开箱即用"
- **DB 行覆盖默认**：`pluginDefaultEnabled` 是 fallback；显式 DB row（包括 Enabled=false）always wins —— 保留操作员 opt-out
- **post-construction 注入**：SetNotifier / SetDatabaseMetricsSecretWriter 让 cmd/ongrid 先构造 UC 再回填 frontierbound 依赖
- **EndpointResolver 接口**：UI 改 system_settings.loki.url 后 edge 自动重定向；resolver 在 cmd/ongrid 实现组合 ONGRID_PUBLIC_URL + 缓存 settings
- **disabled 也 surface**：FetchForEdge 返回 disabled plugin；supervisor 需停正在跑的进程（不只是启动）
- **secret 写失败回滚 row**：保持 plugin config 与 edge-side secret 文件一致
- **notify 不阻塞**：edge 60s safety net poll 兜底；notify 失败不阻塞 UI 保存
- **EndpointResolver 必非 nil**：无 resolver FetchForEdge 无法填 endpoint；构造时强制

## 8. 注意事项

- **`pluginDefaultEnabled` 5 个开 3 个关**：metrics/hostmetrics/procmetrics/logs/traces 开；profiles/custommetrics/databasemetrics 关
- **DB row 覆盖默认**：操作员 UI 关掉 hostmetrics → 写 Enabled=false row → 默认不覆盖
- **FetchForEdge 返回 disabled plugin**：edge supervisor 需停正在跑的进程
- **notify 失败不阻塞**：edge 60s safety net 兜底；但 60s 内 edge 仍跑旧配置
- **secret 写失败回滚 row**：但 frontierbound 已可能部分写 secret 文件；回滚 row 后 edge-side secret 可能残留（幂等覆盖）
- **EndpointResolver.Endpoint(ctx, plugin)**：ctx 透传到 settings 缓存；不阻塞 UI
- **knownPlugins 顺序固定**：metrics/logs/traces/profiles/hostmetrics/procmetrics/custommetrics/databasemetrics；UI 渲染依赖此顺序
- **spec JSON marshal 失败 → ErrInvalid**：调用方应预 marshal
