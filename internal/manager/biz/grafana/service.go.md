# `service.go` 技术实现文档

> 源文件：`internal/manager/biz/grafana/service.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/grafana`

## 1. 概述

本文件是 manager 侧 Grafana 自动配置流程（PR-2）的 biz 层编排器。它桥接三件事：`system_settings.{category=grafana}` 中的 root URL + service-account token、`system_settings.{category=prom}.query_url` 让自动创建的 datasource 指向 manager 写入的同一个 TSDB、以及嵌入 manager 二进制的 dashboard JSON。对外暴露 `Test`（健康探活）、`Sync`（folder + datasource + dashboards 全量推送）、`BootstrapEmbedded`（embedded Grafana SA + token 一次性自举）、`FetchDashboardJSON`（SPA PromQLPanel 代理取数）、`SyncMonitorPanels`（监控页镜像 dashboard 单向推送）五个操作。红线：标识符常量稳定不可改（rename 等于 duplicate 而非 replace）、Monitor 镜像单向覆盖（Grafana 端手改会被下次推送覆盖）、bootstrap 失败绝不 panic 启动。

## 2. 包信息

- **包名**：`grafana`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 `server/integration/http.go`（Test/Sync/FetchDashboardJSON API）、`biz/monitor.Service.kickSync`（SyncMonitorPanels）、`cmd/ongrid` 启动钩子（BootstrapEmbedded）调用；依赖 `internal/manager/biz/setting`、`internal/pkg/grafana`、`internal/manager/model/{monitor,setting}`

## 3. 关键类型与接口

```go
type Service struct {
    settings           *settingbiz.Service
    log                *slog.Logger
    tlsInsecure        bool   // 调用 Grafana 时跳过证书校验
    panelDashboardUID  string // Monitor 页面镜像 dashboard uid（HLD-monitor-panels）
    panelDashboardName string // Grafana 中显示的标题
}

type SyncResult struct {
    Folder     string   `json:"folder"`
    Datasource string   `json:"datasource"`
    Dashboards []string `json:"dashboards"` // 已同步的 dashboard 标题列表
}
```

Sentinel 常量（**稳定不可改**，rename 会产生 duplicate 而非 replace）：

```go
const (
    folderUID      = "ongrid"
    folderTitle    = "ongrid"
    datasourceUID  = "ongrid-prometheus"
    datasourceName = "ongrid-prometheus"
)
```

`//go:embed dashboards/*.json` 把 `dashboards/` 目录下所有 JSON 嵌入二进制，通过 `dashboardsFS embed.FS` 暴露。

`panelDashboardUID` 默认 `"ongrid-monitor"`，`panelDashboardName` 默认 `"ongrid Monitor (managed)"`；可通过 `SetPanelDashboardUID` 由 `ONGRID_GRAFANA_PANEL_DASHBOARD_UID` 覆盖。

## 4. 关键函数与流程

### `New`
- **签名**：`func New(settings *settingbiz.Service, tlsInsecure bool, log *slog.Logger) *Service`
- **职责**：构造 Service；log nil → `slog.Default()`；填入默认 panelDashboardUID/Name
- **流程**：log nil 检查 → 返回 `&Service{...}`，panelDashboardUID=`"ongrid-monitor"`，panelDashboardName=`"ongrid Monitor (managed)"`

### `SetPanelDashboardUID`
- **签名**：`func (s *Service) SetPanelDashboardUID(uid string)`
- **职责**：从 `ONGRID_GRAFANA_PANEL_DASHBOARD_UID` 启动覆盖默认 uid；空输入保留默认
- **流程**：`strings.TrimSpace(uid)`；空则 return；否则赋值 `s.panelDashboardUID`

### `httpClient`
- **签名**：`func (s *Service) httpClient() *http.Client`
- **职责**：Grafana 调用的 TLS 处理单一真相源；`tlsInsecure=false` 返回 nil 让 pkg/grafana 用 15s 默认；true 返回跳过校验的 client
- **流程**：!tlsInsecure → return nil；否则构造 `http.Client{Timeout: 15s, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: TLS12, InsecureSkipVerify: true}}}`

### `BootstrapEmbedded`
- **签名**：`func (s *Service) BootstrapEmbedded(ctx context.Context, adminUser, adminPassword string)`
- **职责**：一次性启动钩子，给 embedded Grafana 自动创建 SA + token，避免首次运行时操作员登录 Grafana 手动配置
- **流程**：
  1. `settings == nil` → 直接 return
  2. 读 `KeyGrafanaSAToken`，非空 → Debug "already set" + return（已自举过）
  3. `adminUser` 或 `adminPassword` 空 → Info "external Grafana?" + return
  4. 读 `KeyGrafanaRootURL`，TrimSpace 后空 → Info "root_url empty" + return
  5. `pkggrafana.NewWithBasicAuth(rootURL, adminUser, adminPassword, httpClient())`
  6. `c.Health(ctx)` 失败 → Warn "health check failed" + return（不 panic 启动）
  7. `FindServiceAccountByName(ctx, "ongrid")`；nil → `CreateServiceAccount(ctx, "ongrid", "Admin")`；任一失败 Warn + return
  8. `CreateServiceAccountToken(ctx, sa.ID, "ongrid-bootstrap")`；失败 Warn + return
  9. `settings.Set(ctx, CategoryGrafana, KeyGrafanaSAToken, token, true)` 持久化（敏感）；失败 Error + return
  10. Info "grafana bootstrap done"（sa, sa_id）
- **错误处理**：所有失败仅 Warn/Error log 后 return，绝不 panic；操作员可后续手动通过 UI 配置
- **幂等性**：SA 名 + token 名 well-known，re-run 不重复创建；token 丢失时 mint 新的，rotation 由操作员负责

### `Test`
- **签名**：`func (s *Service) Test(ctx context.Context) error`
- **职责**：从 settings 构造 client，调 `/api/health`；返回人类可读错误
- **流程**：`s.client(ctx)` → `c.Health(ctx)`

### `Sync`
- **签名**：`func (s *Service) Sync(ctx context.Context) (*SyncResult, error)`
- **职责**：全量 bootstrap 流——folder + datasource + dashboards
- **流程**：
  1. `s.client(ctx)` 构造 client
  2. **重读** `KeyPromQueryURL`（非缓存——操作员常在同一会话中配 prom+grafana，几秒前输入的 query_url 期望落到 datasource 里）；空 → 报错 "configure Prometheus first"
  3. `c.EnsureFolder(ctx, folderUID, folderTitle)`；失败 `%w`
  4. 透传 ongrid 侧 prom 凭据到 Grafana `secureJsonData`：读 `KeyPromBearerToken` / `KeyPromBasicUser` / `KeyPromBasicPassword`；**Bearer 优先于 Basic**；都空则 datasource 匿名
  5. 构造 `pkggrafana.Datasource{UID, Name, Type: "prometheus", URL: promURL, Access: "proxy", JSONData: {httpMethod: POST, timeInterval: 30s}}`；bearer 非空 → `JSONData["httpHeaderName1"]="Authorization"` + `SecureJSONData["httpHeaderValue1"]="Bearer "+bearer`；否则 basicUser 非空 → `BasicAuth=true, BasicAuthUser=basicUser, SecureJSONData["basicAuthPassword"]=basicPass`
  6. `c.UpsertDatasource(ctx, ds)`；失败 `%w`
  7. `s.pushDashboards(ctx, c)` 推送所有 embedded JSON
  8. 构造 `SyncResult{Folder: folderTitle, Datasource: datasourceName, Dashboards: titles}`；Info "grafana sync done"
  9. 返回 res
- **错误处理**：任何步骤失败 `%w` 包装返回，不影响已成功的步骤（folder 已建则下次 EnsureFolder 是 no-op）

### `client`
- **签名**：`func (s *Service) client(ctx context.Context) (*pkggrafana.Client, error)`
- **职责**：从 settings 构造 pkg/grafana.Client；配置不全返回友好错误
- **流程**：
  1. settings nil → 报错 "settings service not wired"
  2. 读 `KeyGrafanaRootURL`；TrimSpace 空报错 "root_url is empty (configure under 设置 → 集成)"
  3. 读 `KeyGrafanaSAToken` + `KeyGrafanaAPIKey`；**sa_token 优先于 api_key**（embedded Grafana 的 happy path 是 sa_token；api_key 是外部 Grafana 无 admin 时的 fallback）；二者都进 `Authorization: Bearer` 头，Grafana 不区分
  4. token 都空报错 "sa_token / api_key empty"
  5. 返回 `pkggrafana.New(root, token, s.httpClient())`

### `FetchDashboardJSON`
- **签名**：`func (s *Service) FetchDashboardJSON(ctx context.Context, uid string) ([]byte, error)`
- **职责**：SPA PromQLPanel 代理取数——浏览器无 Grafana 凭据，必须经 manager 转发
- **流程**：uid TrimSpace 空报错 "uid is required" → `s.client(ctx)` → `c.FetchDashboard(ctx, uid)` → 返回原始 `{dashboard, meta}` envelope
- **错误处理**：注释明示 errs.ErrInvalid（uid 空）、errs.ErrNotFound（Grafana 404，wrap `pkggrafana.ErrDashboardNotFound`）、errs.ErrNotWiredYet（settings/config 缺失）、其他错误为 transport/auth 失败原样透传

### `pushDashboards`
- **签名**：`func (s *Service) pushDashboards(ctx context.Context, c *pkggrafana.Client) ([]string, error)`
- **职责**：读 embed 的 `dashboards/*.json` 全部推送到 ongrid folder
- **流程**：
  1. `dashboardsFS.ReadDir("dashboards")`；失败 `%w`
  2. `sort.Slice` 按文件名升序——使测试 fixture 稳定、log 进度可预测
  3. 遍历 entries：跳过子目录与非 `.json`；`ReadFile("dashboards/"+name)` 失败 `%w`；`dashboardTitle(raw, name)` 取标题；`c.UpsertDashboard(ctx, raw, folderUID, true)`（overwrite=true）失败 `%w`；append title；Info "dashboard pushed"
  4. 返回 titles
- **错误处理**：单文件失败即返回当前已成功 titles + 包装错误；caller 决定是否继续

### `dashboardTitle`
- **签名**：`func dashboardTitle(raw []byte, fallback string) string`
- **职责**：从 dashboard JSON 抽 title；失败回退 filename 去 `.json` 后缀
- **流程**：`json.Unmarshal` 到 `map[string]any`；`m["title"].(string)` ok 且非空返回；否则 `strings.TrimSuffix(fallback, ".json")`

### `SyncMonitorPanels`
- **签名**：`func (s *Service) SyncMonitorPanels(ctx context.Context, panels []*monitormodel.Panel) error`
- **职责**：把用户管理的 Monitor 页面 panel 列表镜像到单一 ongrid-managed Grafana dashboard；uid 固定 `panelDashboardUID`，re-push 总是覆盖同一行
- **流程**：
  1. `s.client(ctx)`
  2. `c.EnsureFolder(ctx, folderUID, folderTitle)`（best-effort；已存在是 no-op）
  3. **prepend 硬编码 core panels**：`all := append(coreMonitorPanels(), panels...)`——前端 Monitor.tsx 总是渲染 core panels，"在 Grafana 中打开" 期望看到与平台一致的面板；core 在前，user 在后，index-based layout 按顺序排
  4. `buildMonitorDashboardJSON(s.panelDashboardUID, s.panelDashboardName, all)`
  5. `json.Marshal` → `c.UpsertDashboard(ctx, raw, folderUID, true)` 覆盖
- **错误处理**：失败 bubble up 给 caller `biz/monitor.Service.kickSync` 记录到 `last_sync_error`；**不重试**——下次用户编辑自然 re-trigger，未来可加 "重新同步" 按钮
- **单向覆盖语义**：Grafana 端手改会被下次 push 覆盖；biz/monitor 是 source of truth

### `coreMonitorPanels`
- **签名**：`func coreMonitorPanels() []*monitormodel.Panel`
- **职责**：重新声明 SPA Monitor.tsx `buildMonitorPanels` 的硬编码默认面板（CPU/内存/磁盘/网络/进程 CPU/进程内存/负载/IO/conntrack/TCP 共 10 个）
- **流程**：返回 10 个 `*monitormodel.Panel`，`ID=9001..9010`（**9000+ offset 避免与 monitor_panels 表 row id 自增冲突**）；全部 `Type: PanelTypeTimeseries`；PromQL 使用 `$__rate_interval`（Grafana 原生解析）
- **红线**：注释明示 "KEEP IN LOCKSTEP WITH Monitor.tsx"——前端改 panel 这里必须同步

### `buildMonitorDashboardJSON`
- **签名**：`func buildMonitorDashboardJSON(uid, title string, panels []*monitormodel.Panel) map[string]any`
- **职责**：把 panel 列表渲染成最小 Grafana dashboard JSON（仅必填 + 影响渲染的字段，Grafana import 时补默认）
- **流程**：遍历 panels；`col := (i%2)*12`、`row := (i/2)*8`——**2 列 12 宽 grid**（与 Monitor.tsx PanelGrid 对齐），每 panel `w=12, h=8`；构造 panel map（id/type/title/datasource{type:prometheus, uid:datasourceUID}/gridPos/fieldConfig.defaults.unit/targets[{refId:A, expr:PromQL, legendFormat:Legend}]）；返回 dashboard map（uid/title/description "Auto-managed by ongrid..." /tags["ongrid","managed"]/editable:true/timezone:""/schemaVersion:38/panels/time{from:now-1h, to:now}）

### `mapPanelType`
- **签名**：`func mapPanelType(t string) string`
- **职责**：ongrid panel type → Grafana type；当前 1:1 但保留 indirection 供未来扩展
- **流程**：`PanelTypeStat`→`"stat"`；`PanelTypeGauge`→`"gauge"`；`PanelTypeTimeseries`→`"timeseries"`；default→`"timeseries"`

## 5. 依赖关系

- **内部包**：
  - `internal/manager/biz/setting`（settings 读写）
  - `internal/pkg/grafana`（Grafana HTTP client：New/NewWithBasicAuth/Client/EnsureFolder/UpsertDatasource/UpsertDashboard/FetchDashboard/Health/FindServiceAccountByName/CreateServiceAccount/CreateServiceAccountToken/Datasource/ErrDashboardNotFound）
  - `internal/manager/model/monitor`（Panel、PanelType*）
  - `internal/manager/model/setting`（CategoryGrafana/CategoryProm/KeyGrafana*/KeyProm*）
- **标准库**：`embed`、`crypto/tls`、`encoding/json`、`net/http`、`sort`、`strings`、`time`
- **被调用方**：`server/integration/http.go`（Test/Sync/FetchDashboardJSON handler）、`biz/monitor.Service.kickSync`（SyncMonitorPanels）、`cmd/ongrid`（BootstrapEmbedded 启动钩子 + SetPanelDashboardUID 从 env 覆盖）

## 6. 并发与资源管理

- **无共享可变状态**：Service 字段除 `panelDashboardUID/Name` 在启动时由 `SetPanelDashboardUID` 设置外，运行期只读；所有调用无锁安全
- **`embed.FS` 只读**：`dashboardsFS` 编译期生成，`ReadDir`/`ReadFile` 并发安全
- **`http.Client` 每次新建**：`httpClient()` 调用即构造，不复用；底层 `http.Transport` 由 pkg/grafana 管理
- **`tls.Config`**：MinVersion 强制 TLS 1.2；`InsecureSkipVerify` 仅在 tlsInsecure=true 时启用
- **ctx 透传**：所有外部调用带 ctx，支持取消与超时
- **无 goroutine**：本文件内不 spawn goroutine，全部同步

## 7. 设计模式与亮点

- **稳定标识符常量**：folderUID/datasourceUID 等用 const，注释明示 "rename 会产生 duplicate 而非 replace"——re-sync 用它们做 "is this already there?" 的 key
- **Auth precedence sa_token > api_key**：注释明示 sa_token 是 embedded Grafana 的 happy path，api_key 是外部 Grafana 无 admin 时的 fallback；二者都进 `Authorization: Bearer` 头，Grafana 不区分
- **Bearer 优先于 Basic**：Sync 中透传 prom 凭据时 bearer 非空则用 header 注入，否则 basic auth，都空则匿名
- **重读 prom.query_url 而非缓存**：注释明示操作员常在同一会话配 prom+grafana，几秒前输入的值期望立即生效
- **embedded dashboard JSON**：`//go:embed dashboards/*.json` 把 dashboard 随二进制分发，无需独立部署文件
- **sort.Slice 确定性**：pushDashboards 按文件名升序，使测试 fixture 稳定、log 进度可预测
- **BootstrapEmbedded 绝不 panic**：所有失败仅 Warn + return，操作员可后续手动通过 UI 配置；token 丢失时 mint 新的，rotation 留给操作员
- **Monitor 镜像单向覆盖**：注释明示 "any edits made directly in Grafana to this dashboard get overwritten on the next push"；biz/monitor 是 source of truth
- **core panels 9000+ ID offset**：避免与 monitor_panels 表自增 row id 冲突；注释 "KEEP IN LOCKSTEP WITH Monitor.tsx" 提示前后端同步
- **2 列 12 宽 grid 布局**：与 Monitor.tsx PanelGrid 一致，"在 Grafana 中打开" 体验一致
- **mapPanelType indirection**：当前 1:1 但保留映射函数，未来 ongrid type（如 "table"）可扩展
- **FetchDashboardJSON 代理**：注释明示浏览器无 Grafana 凭据，必须经 manager 转发——SPA PromQLPanel 的唯一取数路径
- **SyncMonitorPanels 不重试**：注释明示下次用户编辑自然 re-trigger，未来可加 "重新同步" 按钮；失败 bubble up 由 caller 记 `last_sync_error`
- **schemaVersion 38**：固定版本，避免 Grafana 升级时 dashboard schema 漂移

## 8. 注意事项

- **稳定标识符不可 rename**：folderUID/folderTitle/datasourceUID/datasourceName 改名会在 Grafana 中产生 duplicate（旧 UID 仍在，新 UID 被创建），re-sync 失去幂等性
- **panelDashboardUID 默认 "ongrid-monitor"**：可通过 `ONGRID_GRAFANA_PANEL_DASHBOARD_UID` 覆盖；空输入保留默认
- **BootstrapEmbedded 三个 skip 条件**：sa_token 已非空 / admin 凭据空 / root_url 空——任一满足即 Info/Debug log + return，不报错
- **SA 名固定 "ongrid"、token 名 "ongrid-bootstrap"**：re-run find-or-create 不重复；token 丢失 mint 新的，rotation 留给操作员（UI 提供 sa_token 字段）
- **tlsInsecure**：仅当操作员指向自签名外部 Grafana 时开启，匹配 `PromConfig.TLSInsecure` 模式
- **15s HTTP timeout**：httpClient 在 tlsInsecure=true 时设 15s timeout；tlsInsecure=false 时返回 nil 让 pkg/grafana 用其默认 15s
- **prom.query_url 必须先配置**：Sync 在 promURL 空时直接报错 "configure Prometheus first"，不创建空 datasource
- **Bearer 优先于 Basic**：bearer 非空时 basic 被忽略；二者都空 datasource 匿名（适用于 prom 自身无认证的场景）
- **dashboard JSON 仅必填字段**：Grafana import 时补默认；未列出的字段（如 refresh interval、templating）走 Grafana 默认
- **Monitor core panels 必须与 Monitor.tsx 同步**：注释 "KEEP IN LOCKSTEP WITH Monitor.tsx"；前端改 panel 这里不同步会导致 "在 Grafana 中打开" 与平台不一致
- **core panel ID 9001-9010**：9000+ offset 避免与 monitor_panels 表自增 row id 冲突；新增 core panel 必须继续 9011、9012...
- **Monitor 镜像单向**：Grafana 端手改会被下次 push 覆盖；用户应在 ongrid Monitor 页面改 panel，而非在 Grafana 改
- **SyncMonitorPanels 不重试**：失败由 caller 记 `last_sync_error`；下次用户编辑自然 re-trigger
- **FetchDashboardJSON 错误分类**：注释明示 ErrInvalid/ErrNotFound/ErrNotWiredYet/transport 四类，caller 可按类型返回不同 HTTP 状态
- **`$__rate_interval` Grafana 原生解析**：coreMonitorPanels 的 PromQL 使用此变量，Grafana 自动按时间范围计算；ongrid 自身 PromQL 查询不使用此变量
- **schemaVersion 38**：固定，未来 Grafana 升级可能需要同步提升
