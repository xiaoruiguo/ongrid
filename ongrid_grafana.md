# OnGrid × Grafana 集成技术实现说明文档

> 本文档深入分析 OnGrid 系统与 Grafana 集成的全部代码路径：客户端 SDK、biz 编排、HTTP 路由、配置装配、SPA 反向代理、dashboard 镜像、健康探测、provisioning 文件，以及 docker-compose / nginx 部署面。
>
> 全部行号引用基于撰写时仓库快照，可能随后续提交漂移；尽量给出文件路径锚点便于跳转。

---

## 目录

1. [架构总览](#1-架构总览)
2. [分层与文件索引](#2-分层与文件索引)
3. [pkg/grafana：薄 HTTP 客户端](#3-pkggrafana薄-http-客户端)
4. [biz/grafana：业务编排层](#4-bizgrafana业务编排层)
5. [server/integration：HTTP 路由层](#5-serverintegrationhttp-路由层)
6. [biz/monitor：用户面板镜像链路](#6-bizmonitor用户面板镜像链路)
7. [systemhealth：Grafana 健康探测](#7-systemhealthgrafana-健康探测)
8. [配置与启动装配](#8-配置与启动装配)
9. [SPA 侧：dashboard 代理读取与变量替换](#9-spa-侧dashboard-代理读取与变量替换)
10. [SPA 侧：drilldown 与 Explore 深链](#10-spa-侧drilldown-与-explore-深链)
11. [部署面：docker-compose + nginx + provisioning](#11-部署面docker-compose--nginx--provisioning)
12. [并发、错误与可观测性](#12-并发错误与可观测性)
13. [架构红线与设计要点](#13-架构红线与设计要点)
14. [附录：测试覆盖](#14-附录测试覆盖)

---

## 1. 架构总览

OnGrid 把 Grafana 当作"被托管的可视化前端"——业务侧（manager）持有 Grafana 的 admin/SA 凭据，负责把 Prometheus 数据源、dashboard JSON、用户自管 PromQL 面板单向推送到 Grafana；浏览器侧不直连 Grafana，而是走两条路：

- **读路径**：浏览器 → manager `/v1/observability/dashboards/{uid}` → Grafana `/api/dashboards/uid/{uid}`，凭据留在 manager，避免 CORS / cookie 共享。
- **写路径**：admin 在浏览器 → manager `/v1/integrations/grafana/{test,sync}` → biz/grafana.Service → pkg/grafana.Client → Grafana Admin API。
- **镜像路径**：用户在 Monitor 页编辑面板 → biz/monitor.Service 持久化到 MySQL → 后台 goroutine → biz/grafana.SyncMonitorPanels → Grafana。
- **嵌入式入口**：浏览器直接访问 `/grafana/...` → nginx `auth_request` → manager 验 JWT → 票据 cookie → `proxy_pass http://grafana_backend`。

Grafana 在 OnGrid 中承担三类职责：
1. **被推送的 dashboard 容器**（ongrid 文件夹下的 cluster-overview / server-detail / ongrid-monitor）
2. **被代理的查询前端**（通过 ongrid-prometheus 数据源反查 Prometheus）
3. **被嵌入的 Explore UI**（Loki / Tempo / Prometheus 的日志/链路深链）

---

## 2. 分层与文件索引

| 层 | 文件 | 职责 |
|----|------|------|
| pkg 客户端 | [internal/pkg/grafana/client.go](file:///d:/claude/ongrid/internal/pkg/grafana/client.go) | Grafana Admin API HTTP 客户端，Bearer/BasicAuth 双模式 |
| pkg 测试 | [internal/pkg/grafana/client_test.go](file:///d:/claude/ongrid/internal/pkg/grafana/client_test.go) | httptest 桩覆盖 health / upsert / fetch / 404 |
| biz 编排 | [internal/manager/biz/grafana/service.go](file:///d:/claude/ongrid/internal/manager/biz/grafana/service.go) | Test / Sync / BootstrapEmbedded / SyncMonitorPanels / buildMonitorDashboardJSON |
| biz 测试 | [internal/manager/biz/grafana/monitor_test.go](file:///d:/claude/ongrid/internal/manager/biz/grafana/monitor_test.go) | 验证 dashboard JSON 渲染、面板类型映射 |
| 内嵌 dashboard | [internal/manager/biz/grafana/dashboards/cluster-overview.json](file:///d:/claude/ongrid/internal/manager/biz/grafana/dashboards/cluster-overview.json), [server-detail.json](file:///d:/claude/ongrid/internal/manager/biz/grafana/dashboards/server-detail.json) | 编译进二进制的预置 dashboard |
| HTTP 路由 | [internal/manager/server/integration/http.go](file:///d:/claude/ongrid/internal/manager/server/integration/http.go) | `/v1/integrations/grafana/{test,sync}` + `/v1/observability/dashboards/{uid}` |
| 镜像上层 | [internal/manager/biz/monitor/service.go](file:///d:/claude/ongrid/internal/manager/biz/monitor/service.go) | 用户面板 CRUD + 异步 kickSync → grafanaSvc.SyncMonitorPanels |
| Monitor model | [internal/manager/model/monitor/model.go](file:///d:/claude/ongrid/internal/manager/model/monitor/model.go) | Panel 实体、PanelType 常量、ValidPanelType |
| Setting model | [internal/manager/model/setting/model.go](file:///d:/claude/ongrid/internal/manager/model/setting/model.go#L146-L164) | CategoryGrafana + KeyGrafanaRootURL/SAToken/APIKey/OrgID |
| 系统健康 | [internal/manager/service/systemhealth/service.go](file:///d:/claude/ongrid/internal/manager/service/systemhealth/service.go#L190-L212) | checkGrafana 探测 |
| 配置 | [internal/pkg/config/config.go](file:///d:/claude/ongrid/internal/pkg/config/config.go#L104-L133) | GrafanaConfig（InternalRootURL / BootstrapUser / BootstrapPassword / TLSInsecure） |
| 启动装配 | [cmd/ongrid/main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go#L448-L534) | 种子 root_url、构建 grafanaSvc、bootstrap goroutine、首次 SyncNow |
| 启动装配 | [cmd/ongrid/main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1713-L1738) | systemhealth.Dependencies.Grafana = grafanaSvc |
| 前端 API | [web/src/api/grafana.ts](file:///d:/claude/ongrid/web/src/api/grafana.ts) | fetchDashboard → /v1/observability/dashboards/{uid} |
| 前端变量 | [web/src/lib/grafanaVars.ts](file:///d:/claude/ongrid/web/src/lib/grafanaVars.ts) | `$__rate_interval` / `$device_id` 替换、flattenPanels |
| 前端深链 | [web/src/lib/drilldown.ts](file:///d:/claude/ongrid/web/src/lib/drilldown.ts) | openMetricDrilldown / buildExploreUrl / fetchGrafanaRootURL |
| 部署 | [deploy/docker-compose.yml](file:///d:/claude/ongrid/deploy/docker-compose.yml#L317-L363) | grafana 服务定义 |
| 部署 | [deploy/nginx/nginx.conf](file:///d:/claude/ongrid/deploy/nginx/nginx.conf#L315-L329) | `/grafana/` 反代 + auth_request |
| Provisioning | [deploy/grafana/provisioning/dashboards/default.yml](file:///d:/claude/ongrid/deploy/grafana/provisioning/dashboards/default.yml) | 文件型 dashboard provider |
| Provisioning | [deploy/grafana/provisioning/datasources/loki.yml](file:///d:/claude/ongrid/deploy/grafana/provisioning/datasources/loki.yml), [tempo.yml](file:///d:/claude/ongrid/deploy/grafana/provisioning/datasources/tempo.yml) | 文件型数据源（editable:false） |

---

## 3. pkg/grafana：薄 HTTP 客户端

[internal/pkg/grafana/client.go](file:///d:/claude/ongrid/internal/pkg/grafana/client.go) 是一个无状态的 Grafana Admin API 封装，不依赖任何 OnGrid 业务包，可被任何 BC 复用。

### 3.1 Client 结构与构造

[client.go:28-48](file:///d:/claude/ongrid/internal/pkg/grafana/client.go#L28-L48)：

```go
type Client struct {
    baseURL  string
    token    string // Bearer; empty if basicAuth in use
    basicUsr string
    basicPwd string
    hc       *http.Client
}

func New(baseURL, token string, hc *http.Client) *Client {
    if hc == nil {
        hc = &http.Client{Timeout: 15 * time.Second}
    }
    return &Client{
        baseURL: strings.TrimRight(baseURL, "/"),
        token:   strings.TrimSpace(token),
        hc:      hc,
    }
}
```

两种构造形态对应两条路径：
- `New(baseURL, token, hc)`：业务路径（Test/Sync/Fetch），凭据来自 system_settings.{sa_token | api_key}
- `NewWithBasicAuth(baseURL, user, password, hc)`：[client.go:53-63](file:///d:/claude/ongrid/internal/pkg/grafana/client.go#L53-L63)，仅 BootstrapEmbedded 一次性使用 admin 凭据

### 3.2 do()：统一 HTTP 执行

[client.go:313-353](file:///d:/claude/ongrid/internal/pkg/grafana/client.go#L313-L353) 是所有 API 调用的底座：

```go
func (c *Client) do(ctx context.Context, method, path string, payload any) ([]byte, error) {
    // 1. baseURL 空校验
    // 2. payload != nil → json.Marshal
    // 3. http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
    // 4. 鉴权三选一：token Bearer / basicAuth / 无（匿名 Grafana）
    // 5. Accept: application/json；payload 时加 Content-Type
    // 6. hc.Do(req)
    // 7. io.LimitReader(resp.Body, 1<<20) — 1 MiB 上限，防 OOM
    // 8. 404 → notFoundErr；非 2xx → error 含状态码和原文
    // 9. 2xx → 返回 body bytes
}
```

关键设计：
- **鉴权三态**：[client.go:329-334](file:///d:/claude/ongrid/internal/pkg/grafana/client.go#L329-L334) 用 `switch` 而非 if-else，明确"要么 Bearer，要么 Basic，要么裸"。匿名分支用于外部 Grafana 启用 anonymous org 时的 dashboard fetch。
- **1 MiB cap**：[client.go:345](file:///d:/claude/ongrid/internal/pkg/grafana/client.go#L345) `io.LimitReader(resp.Body, 1<<20)`，dashboard JSON 通常 50-200KB，1 MiB 足够且防恶意/异常大响应。
- **404 转哨兵**：[client.go:346-348](file:///d:/claude/ongrid/internal/pkg/grafana/client.go#L346-L348) 把 404 转成 `notFoundErr`，让上层用 `errors.Is` 分支到"create"路径。

### 3.3 Health：连通性探测

[client.go:68-84](file:///d:/claude/ongrid/internal/pkg/grafana/client.go#L68-L84)：

```go
func (c *Client) Health(ctx context.Context) error {
    body, err := c.do(ctx, http.MethodGet, "/api/health", nil)
    // 解码 {database, version}
    if resp.Database != "ok" {
        return fmt.Errorf("grafana: unhealthy: database=%s", resp.Database)
    }
    return nil
}
```

`/api/health` 在 Grafana 中无需鉴权，但 client.go 仍带 Bearer——这样同一次调用同时验证了"URL 可达 + 凭据有效 + Grafana 数据库可用"三件事。

### 3.4 UpsertDatasource：幂等数据源

[client.go:113-147](file:///d:/claude/ongrid/internal/pkg/grafana/client.go#L113-L147) 是关键的 GET-then-PUT/POST 模式：

```go
func (c *Client) UpsertDatasource(ctx context.Context, ds Datasource) error {
    // 1. GET /api/datasources/uid/{UID}
    // 2. 若存在：
    //    a. 解码 {ID, ReadOnly}
    //    b. ReadOnly=true → return nil（provisioning editable:false 的数据源不可改，按现状信任）
    //    c. ReadOnly=false → PUT /api/datasources/{ID}
    //    d. PUT 若返回 read-only 错误 → return nil（防御性回退）
    // 3. 若 404 → POST /api/datasources（创建）
}
```

ReadOnly 处理是核心设计点（[client.go:107-112](file:///d:/claude/ongrid/internal/pkg/grafana/client.go#L107-L112) 注释）：deploy/grafana/provisioning/datasources/loki.yml 等用 `editable: false` 预置的数据源，Grafana 拒绝 PUT/DELETE，必须识别并跳过——dashboards 通过 UID 引用，数据源 row 是否可改不影响 dashboard 工作。

`isReadOnlyError`（[client.go:152-159](file:///d:/claude/ongrid/internal/pkg/grafana/client.go#L152-L159)）做字符串匹配 `"read-only data source"` / `"Cannot update read-only"`，是 readOnly 字段之外的二重保险。

### 3.5 EnsureFolder：幂等文件夹

[client.go:163-177](file:///d:/claude/ongrid/internal/pkg/grafana/client.go#L163-L177)：

```go
func (c *Client) EnsureFolder(ctx context.Context, uid, title string) error {
    body, err := c.do(ctx, http.MethodGet, "/api/folders/"+uid, nil)
    if err == nil && len(body) > 0 {
        return nil // 已存在
    }
    if !isNotFound(err) {
        return err
    }
    payload := map[string]string{"uid": uid, "title": title}
    _, cerr := c.do(ctx, http.MethodPost, "/api/folders", payload)
    return cerr
}
```

### 3.6 ServiceAccount 与 Token：bootstrap 路径

[client.go:179-247](file:///d:/claude/ongrid/internal/pkg/grafana/client.go#L179-L247) 三个方法配合：

1. `FindServiceAccountByName(ctx, name)`：调 `GET /api/serviceaccounts/search?query={name}`，Grafana 做子串匹配，**本端做精确名等值过滤**（[client.go:202-207](file:///d:/claude/ongrid/internal/pkg/grafana/client.go#L202-L207)）。找不到返回 `(nil, nil)` 而非 error，让上层走 create 分支。
2. `CreateServiceAccount(ctx, name, role)`：`POST /api/serviceaccounts` body `{name, role}`，role ∈ {Viewer, Editor, Admin}。
3. `CreateServiceAccountToken(ctx, saID, name)`：`POST /api/serviceaccounts/{saID}/tokens`，返回的 `key` 字段是明文 token——Grafana 不再返回第二次，调用方必须立即持久化（[client.go:228-229](file:///d:/claude/ongrid/internal/pkg/grafana/client.go#L228-L229) 注释强调）。

### 3.7 FetchDashboard：SPA 代理读

[client.go:258-270](file:///d:/claude/ongrid/internal/pkg/grafana/client.go#L258-L270)：

```go
func (c *Client) FetchDashboard(ctx context.Context, uid string) ([]byte, error) {
    if strings.TrimSpace(uid) == "" {
        return nil, errors.New("grafana: dashboard uid is required")
    }
    body, err := c.do(ctx, http.MethodGet, "/api/dashboards/uid/"+uid, nil)
    if err != nil {
        if isNotFound(err) {
            return nil, ErrDashboardNotFound
        }
        return nil, err
    }
    return body, nil
}
```

404 转成 `ErrDashboardNotFound`（[client.go:275](file:///d:/claude/ongrid/internal/pkg/grafana/client.go#L275)）公开哨兵，HTTP 层用 `errors.Is` 映射成 HTTP 404。返回原始 bytes 而非结构体——保留 Grafana 全字段，SPA 自己解析所需字段。

### 3.8 UpsertDashboard：dashboard 推送

[client.go:281-303](file:///d:/claude/ongrid/internal/pkg/grafana/client.go#L281-L303)：

```go
func (c *Client) UpsertDashboard(ctx context.Context, dashboard []byte, folderUID string, overwrite bool) error {
    if len(dashboard) == 0 {
        return errors.New("grafana: empty dashboard payload")
    }
    var raw map[string]any
    if err := json.Unmarshal(dashboard, &raw); err != nil {
        return fmt.Errorf("grafana: parse dashboard: %w", err)
    }
    delete(raw, "id") // ← 强制 null，让 Grafana 当新 dashboard 处理
    wrapper := map[string]any{
        "dashboard": raw,
        "overwrite": overwrite,
        "message":   "synced from ongrid",
    }
    if folderUID != "" {
        wrapper["folderUid"] = folderUID
    }
    _, err := c.do(ctx, http.MethodPost, "/api/dashboards/db", wrapper)
    return err
}
```

`delete(raw, "id")` 是关键——保留原 id 会让 Grafana 尝试按 id 查找，新装环境会失败。靠 UID + `overwrite=true` 实现幂等重推。

---

## 4. biz/grafana：业务编排层

[internal/manager/biz/grafana/service.go](file:///d:/claude/ongrid/internal/manager/biz/grafana/service.go) 桥接三件事：
1. `system_settings.{category=grafana}` 存储 root_url + sa_token / api_key
2. `system_settings.{category=prom}.query_url` 让自动创建的数据源指向 ongrid 写入的同一个 TSDB
3. embed 进二进制的 dashboard JSON（[service.go:46-47](file:///d:/claude/ongrid/internal/manager/biz/grafana/service.go#L46-L47) `//go:embed dashboards/*.json`）

### 4.1 常量与 Service 结构

[service.go:39-56](file:///d:/claude/ongrid/internal/manager/biz/grafana/service.go#L39-L56)：

```go
const (
    folderUID      = "ongrid"
    folderTitle    = "ongrid"
    datasourceUID  = "ongrid-prometheus"
    datasourceName = "ongrid-prometheus"
)

//go:embed dashboards/*.json
var dashboardsFS embed.FS

type Service struct {
    settings           *settingbiz.Service
    log                *slog.Logger
    tlsInsecure        bool
    panelDashboardUID  string // 默认 "ongrid-monitor"
    panelDashboardName string // 默认 "ongrid Monitor (managed)"
}
```

四个常量是"对外契约"——dashboard JSON 内的 `$datasource UID` 引用、文件夹归属、重推识别都依赖它们，注释明确"renaming would create duplicates instead of replacing"。

`panelDashboardUID` 可被 env `ONGRID_GRAFANA_PANEL_DASHBOARD_UID` 覆盖（[service.go:77-83](file:///d:/claude/ongrid/internal/manager/biz/grafana/service.go#L77-L83) 的 `SetPanelDashboardUID`），让运营方把 OnGrid 管理的 dashboard 隔离到自定义 uid 命名空间。

### 4.2 httpClient：TLS 单源

[service.go:88-101](file:///d:/claude/ongrid/internal/manager/biz/grafana/service.go#L88-L101)：

```go
func (s *Service) httpClient() *http.Client {
    if !s.tlsInsecure {
        return nil // pkg/grafana 用 15s 默认
    }
    return &http.Client{
        Timeout: 15 * time.Second,
        Transport: &http.Transport{
            TLSClientConfig: &tls.Config{
                MinVersion:         tls.VersionTLS12,
                InsecureSkipVerify: true,
            },
        },
    }
}
```

与 PromConfig.TLSInsecure 对称设计：运营方指向自签证书的外部 Grafana 时启用。MinVersion 强制 TLS 1.2+。

### 4.3 BootstrapEmbedded：首次启动自动建 SA

[service.go:117-168](file:///d:/claude/ongrid/internal/manager/biz/grafana/service.go#L117-L168) 是首次启动的关键：

```go
func (s *Service) BootstrapEmbedded(ctx context.Context, adminUser, adminPassword string) {
    // 1. 已有 sa_token → skip（幂等）
    if existing != "" { s.log.Debug("...skipped: token already set"); return }
    // 2. admin 凭据空 → skip（外部 Grafana 场景）
    if adminUser == "" || adminPassword == "" { s.log.Info("...admin creds not provided"); return }
    // 3. root_url 空 → skip
    if rootURL == "" { s.log.Info("...root_url empty"); return }
    // 4. 用 BasicAuth client 做一次 Health 探测
    c := pkggrafana.NewWithBasicAuth(rootURL, adminUser, adminPassword, s.httpClient())
    if err := c.Health(ctx); err != nil {
        s.log.Warn("...health check failed; skipping", ...); return // 不阻断启动
    }
    // 5. find-or-create SA "ongrid"（role=Admin）
    sa, err := c.FindServiceAccountByName(ctx, "ongrid")
    if sa == nil { sa, err = c.CreateServiceAccount(ctx, "ongrid", "Admin") }
    // 6. mint token "ongrid-bootstrap"
    token, err := c.CreateServiceAccountToken(ctx, sa.ID, "ongrid-bootstrap")
    // 7. 持久化到 system_settings.{grafana.sa_token}（sensitive=true）
    s.settings.Set(ctx, CategoryGrafana, KeyGrafanaSAToken, token, true)
}
```

设计要点：
- **三个 skip 条件**全部用 `return` + log，**绝不 panic 也绝不阻塞启动**——bootstrap 失败的运营方仍可手动在 UI 填 token。
- **幂等**：靠"sa_token 已有则 skip"和"SA find-or-create"双重保护，重启安全。
- **token 名固定** `"ongrid-bootstrap"`：若旧 token 丢失，重跑会 mint 一个新的，Grafana 同时保留——rotation 是运营方责任（注释 [service.go:115-116](file:///d:/claude/ongrid/internal/manager/biz/grafana/service.go#L115-L116)）。

### 4.4 client()：凭据优先级

[service.go:267-286](file:///d:/claude/ongrid/internal/manager/biz/grafana/service.go#L267-L286)：

```go
func (s *Service) client(ctx context.Context) (*pkggrafana.Client, error) {
    root, _, _ := s.settings.Get(ctx, CategoryGrafana, KeyGrafanaRootURL)
    if root == "" {
        return nil, errors.New("grafana: root_url is empty (configure under 设置 → 集成)")
    }
    saToken, _, _ := s.settings.Get(ctx, CategoryGrafana, KeyGrafanaSAToken)
    apiKey, _, _   := s.settings.Get(ctx, CategoryGrafana, KeyGrafanaAPIKey)
    token := strings.TrimSpace(saToken)
    if token == "" {
        token = strings.TrimSpace(apiKey) // 外部 Grafana 兜底
    }
    if token == "" {
        return nil, errors.New("grafana: sa_token / api_key empty ...")
    }
    return pkggrafana.New(root, token, s.httpClient()), nil
}
```

**优先级**：sa_token > api_key。两者落到同一个 `Authorization: Bearer` header，Grafana 内部不区分。sa_token 是嵌入式 Grafana 的"快乐路径"（bootstrap 自动 mint），api_key 是外部 Grafana 的"无 admin 权限时的妥协"。

### 4.5 Test：连通性自检

[service.go:181-187](file:///d:/claude/ongrid/internal/manager/biz/grafana/service.go#L181-L187) 极简：构造 client → `c.Health(ctx)`。错误向上透传给 HTTP 层映射状态码。

### 4.6 Sync：全量推送

[service.go:190-257](file:///d:/claude/ongrid/internal/manager/biz/grafana/service.go#L190-L257) 是 admin 在 UI 点"同步"时的主流程：

```go
func (s *Service) Sync(ctx context.Context) (*SyncResult, error) {
    c, err := s.client(ctx)
    // 1. 重读 prom.query_url（不缓存——运营方常在配置 prom + grafana 的同一秒坐下，期望刚输入的 URL 立即落到数据源）
    promURL, _, _ := s.settings.Get(ctx, CategoryProm, KeyPromQueryURL)
    if promURL == "" {
        return nil, errors.New("grafana: cannot sync — prom.query_url is empty ...")
    }
    // 2. EnsureFolder("ongrid", "ongrid")
    c.EnsureFolder(ctx, folderUID, folderTitle)
    // 3. 构造 Datasource，把 ongrid 端的 prom 凭据前转到 Grafana secureJsonData
    //    Bearer 优先；否则 BasicAuth；都没有 → 匿名
    bearer, _, _    := s.settings.Get(ctx, CategoryProm, KeyPromBearerToken)
    basicUser, _, _ := s.settings.Get(ctx, CategoryProm, KeyPromBasicUser)
    basicPass, _, _ := s.settings.Get(ctx, CategoryProm, KeyPromBasicPassword)
    ds := pkggrafana.Datasource{UID: datasourceUID, Name: datasourceName, Type: "prometheus",
        URL: promURL, Access: "proxy",
        JSONData: map[string]any{"httpMethod": "POST", "timeInterval": "30s"}}
    if bearer != "" {
        ds.JSONData["httpHeaderName1"] = "Authorization"
        ds.SecureJSONData = map[string]string{"httpHeaderValue1": "Bearer " + bearer}
    } else if basicUser != "" {
        ds.BasicAuth = true; ds.BasicAuthUser = basicUser
        ds.SecureJSONData = map[string]string{"basicAuthPassword": basicPass}
    }
    c.UpsertDatasource(ctx, ds)
    // 4. 推送所有 embed dashboard
    titles, err := s.pushDashboards(ctx, c)
    return &SyncResult{Folder: folderTitle, Datasource: datasourceName, Dashboards: titles}, nil
}
```

`SyncResult`（[service.go:172-176](file:///d:/claude/ongrid/internal/manager/biz/grafana/service.go#L172-L176)）的字段是 HTTP 响应体：

```go
type SyncResult struct {
    Folder      string   `json:"folder"`
    Datasource  string   `json:"datasource"`
    Dashboards  []string `json:"dashboards"`
}
```

### 4.7 pushDashboards：embed 推送

[service.go:316-342](file:///d:/claude/ongrid/internal/manager/biz/grafana/service.go#L316-L342)：

```go
func (s *Service) pushDashboards(ctx context.Context, c *pkggrafana.Client) ([]string, error) {
    entries, _ := dashboardsFS.ReadDir("dashboards")
    sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() }) // 字典序，日志可预测
    titles := make([]string, 0, len(entries))
    for _, e := range entries {
        if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") { continue }
        raw, _ := dashboardsFS.ReadFile("dashboards/" + e.Name())
        title := dashboardTitle(raw, e.Name()) // 从 JSON 提取 title，失败回退文件名
        c.UpsertDashboard(ctx, raw, folderUID, true) // overwrite=true
        titles = append(titles, title)
    }
    return titles, nil
}
```

字典序排序的目的：稳定测试 fixture + 给运营方可预测的日志进度序列。

### 4.8 SyncMonitorPanels：用户面板镜像

[service.go:370-397](file:///d:/claude/ongrid/internal/manager/biz/grafana/service.go#L370-L397)：

```go
func (s *Service) SyncMonitorPanels(ctx context.Context, panels []*monitormodel.Panel) error {
    c, err := s.client(ctx)
    c.EnsureFolder(ctx, folderUID, folderTitle)
    // 关键：把前端硬编码的核心面板 prepend 到用户面板前面
    // 这样"在 Grafana 中打开"看到的就是 Monitor 页相同的核心面板
    all := append(coreMonitorPanels(), panels...)
    dash := buildMonitorDashboardJSON(s.panelDashboardUID, s.panelDashboardName, all)
    raw, _ := json.Marshal(dash)
    c.UpsertDashboard(ctx, raw, folderUID, true) // overwrite=true，单向覆盖
    return nil
}
```

**单向**语义（[service.go:356-369](file:///d:/claude/ongrid/internal/manager/biz/grafana/service.go#L356-L369) 注释）：在 Grafana 内直接编辑这个 dashboard 会被下次推送覆盖。biz/monitor 是唯一真源。

### 4.9 coreMonitorPanels：硬编码核心面板

[service.go:407-431](file:///d:/claude/ongrid/internal/manager/biz/grafana/service.go#L407-L431) 重新声明 SPA `web/src/pages/Monitor.tsx buildMonitorPanels` 的 10 个核心面板：

| ID | 标题 | PromQL（节选） |
|----|------|----------------|
| 9001 | CPU 使用率 | `100 * (1 - avg by (device_id) (rate(node_cpu_seconds_total{mode="idle"}[$__rate_interval])))` |
| 9002 | 内存使用率 | `100 * (1 - ...MemAvailable / ...MemTotal)` |
| 9003 | 磁盘使用率（按物理设备） | filesystem 复杂正则 |
| 9004 | 网络吞吐 | `rate(node_network_receive_bytes_total{device!~"lo\|veth.*\|docker.*"}[$__rate_interval])` |
| 9005 | Top 8 进程 · CPU | `topk(8, avg by (groupname) (rate(namedprocess_namegroup_cpu_seconds_total{}[$__rate_interval])))` |
| 9006 | Top 8 进程 · 内存 | `topk(8, ...memory_bytes{memtype="resident"})` |
| 9007 | 负载饱和度 | `node_load1 / count by (device_id) (node_cpu_seconds_total{mode="idle"})` |
| 9008 | 磁盘 I/O | `rate(node_disk_read_bytes_total) + rate(node_disk_written_bytes_total)` |
| 9009 | conntrack 利用率 | `100 * nf_conntrack_entries / nf_conntrack_entries_limit` |
| 9010 | TCP 连接数 | `sum by (device_id) (node_netstat_Tcp_CurrEstab)` |

ID 偏移 9000+ 防止与 `monitor_panels` 自增主键冲突。注释明确"KEEP IN LOCKSTEP WITH Monitor.tsx"——前端改了核心面板这里必须同步。

### 4.10 buildMonitorDashboardJSON：渲染器

[service.go:440-489](file:///d:/claude/ongrid/internal/manager/biz/grafana/service.go#L440-L489)：

```go
func buildMonitorDashboardJSON(uid, title string, panels []*monitormodel.Panel) map[string]any {
    gPanels := make([]map[string]any, 0, len(panels))
    for i, p := range panels {
        col := (i % 2) * 12   // 2 列布局
        row := (i / 2) * 8    // 每行高 8
        gPanels = append(gPanels, map[string]any{
            "id":    p.ID,
            "type":  mapPanelType(p.Type),
            "title": p.Title,
            "datasource": {"type": "prometheus", "uid": datasourceUID},
            "gridPos": {"x": col, "y": row, "w": 12, "h": 8},
            "fieldConfig": {"defaults": {"unit": p.Unit}, "overrides": []any{}},
            "targets": [{"refId": "A", "expr": p.PromQL, "legendFormat": p.Legend,
                         "datasource": {"type": "prometheus", "uid": datasourceUID}}],
        })
    }
    return map[string]any{
        "uid": uid, "title": title,
        "description": "Auto-managed by ongrid. Edits in Grafana will be overwritten ...",
        "tags":        []string{"ongrid", "managed"},
        "editable":    true,
        "timezone":    "",
        "schemaVersion": 38,
        "panels":      gPanels,
        "time":        {"from": "now-1h", "to": "now"},
    }
}
```

布局：2 列 12-col grid（与 Monitor.tsx PanelGrid 一致），每面板 12×8。

### 4.11 mapPanelType：类型映射

[service.go:495-505](file:///d:/claude/ongrid/internal/manager/biz/grafana/service.go#L495-L505)：

```go
func mapPanelType(t string) string {
    switch t {
    case monitormodel.PanelTypeStat:  return "stat"
    case monitormodel.PanelTypeGauge: return "gauge"
    case monitormodel.PanelTypeTimeseries: return "timeseries"
    }
    return "timeseries" // 兜底
}
```

今天 1:1，但保留 indirection 给未来 ongrid 类型（如 "table"）映射到 Grafana 变种。

### 4.12 dashboardTitle：标题提取

[service.go:346-354](file:///d:/claude/ongrid/internal/manager/biz/grafana/service.go#L346-L354) 从 dashboard JSON 解析 `title` 字段，失败回退文件名（去 `.json` 后缀）。

### 4.13 FetchDashboardJSON：代理读入口

[service.go:298-311](file:///d:/claude/ongrid/internal/manager/biz/grafana/service.go#L298-L311)：

```go
func (s *Service) FetchDashboardJSON(ctx context.Context, uid string) ([]byte, error) {
    if strings.TrimSpace(uid) == "" {
        return nil, errors.New("grafana: uid is required")
    }
    c, err := s.client(ctx)
    if err != nil { return nil, err }
    body, err := c.FetchDashboard(ctx, uid)
    if err != nil { return nil, err }
    return body, nil
}
```

错误注释（[service.go:288-297](file:///d:/claude/ongrid/internal/manager/biz/grafana/service.go#L288-L297)）明确三种语义：
- `errs.ErrInvalid`（uid 空）
- `errs.ErrNotFound`（Grafana 404，包装 `pkggrafana.ErrDashboardNotFound`）
- `errs.ErrNotWiredYet`（settings 未装配）
- 其他：transport/auth 错误原样上抛

---

## 5. server/integration：HTTP 路由层

[internal/manager/server/integration/http.go](file:///d:/claude/ongrid/internal/manager/server/integration/http.go) 是薄薄的 auth + 错误映射 shim。

### 5.1 GrafanaService 接口

[http.go:29-33](file:///d:/claude/ongrid/internal/manager/server/integration/http.go#L29-L33)：

```go
type GrafanaService interface {
    Test(ctx context.Context) error
    Sync(ctx context.Context) (*bizgrafana.SyncResult, error)
    FetchDashboardJSON(ctx context.Context, uid string) ([]byte, error)
}
```

接口在消费方定义（AGENTS.md 架构红线），`*bizgrafana.Service` 结构化满足。

### 5.2 路由注册

[http.go:120-145](file:///d:/claude/ongrid/internal/manager/server/integration/http.go#L120-L145)：

```go
func (h *Handler) Register(r chi.Router) {
    r.Post("/v1/integrations/grafana/test", h.testGrafana)
    r.Post("/v1/integrations/grafana/sync", h.syncGrafana)
    // ... prom / loki / tempo / websearch / llm ...
    r.Get("/v1/observability/dashboards/{uid}", h.fetchDashboard)
}
```

注释解释了 dashboard proxy 路由放在 `/v1/observability` 而非 `/v1/integrations`：它是 Monitor 页每次加载都用的**读路径**，语义是 query 不是 admin action。

### 5.3 testGrafana / syncGrafana：admin 专用

[http.go:306-327](file:///d:/claude/ongrid/internal/manager/server/integration/http.go#L306-L327)：

```go
func (h *Handler) testGrafana(w http.ResponseWriter, r *http.Request) {
    if !h.requireAdmin(w, r) { return }
    if err := h.grafana.Test(r.Context()); err != nil {
        writeErr(w, err); return
    }
    writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) syncGrafana(w http.ResponseWriter, r *http.Request) {
    if !h.requireAdmin(w, r) { return }
    res, err := h.grafana.Sync(r.Context())
    if err != nil { writeErr(w, err); return }
    writeJSON(w, http.StatusOK, res)
}
```

`requireAdmin`（[http.go:421-432](file:///d:/claude/ongrid/internal/manager/server/integration/http.go#L421-L432)）从 tenantctx 取 role，非 admin → 403。

### 5.4 fetchDashboard：所有登录用户可读

[http.go:287-304](file:///d:/claude/ongrid/internal/manager/server/integration/http.go#L287-L304)：

```go
func (h *Handler) fetchDashboard(w http.ResponseWriter, r *http.Request) {
    if !h.requireUser(w, r) { return }
    uid := chi.URLParam(r, "uid")
    if uid == "" { writeErr(w, errs.ErrInvalid); return }
    body, err := h.grafana.FetchDashboardJSON(r.Context(), uid)
    if err != nil { writeErr(w, err); return }
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusOK)
    _, _ = w.Write(body)
}
```

`requireUser`（[http.go:438-444](file:///d:/claude/ongrid/internal/manager/server/integration/http.go#L438-L444)）只校验 tenant context 存在，admin / 普通用户都可——浏览器持有 Grafana 凭据是 manager 的责任，不暴露给前端。

响应体是 Grafana 原样 `{dashboard, meta}`，**不重塑**——[http.go:278-286](file:///d:/claude/ongrid/internal/manager/server/integration/http.go#L278-L286) 注释说明 Monitor.tsx 自己 walk `panels[]`，Grafana 全 schema 比 Go 想建模的丰富得多。

### 5.5 writeErr：错误到状态码映射

[http.go:460-478](file:///d:/claude/ongrid/internal/manager/server/integration/http.go#L460-L478)：

```go
func writeErr(w http.ResponseWriter, err error) {
    status := http.StatusInternalServerError
    slug := "internal"
    switch {
    case errors.Is(err, errs.ErrUnauthorized):  status, slug = 401, "unauthorized"
    case errors.Is(err, errs.ErrForbidden):     status, slug = 403, "forbidden"
    case errors.Is(err, errs.ErrInvalid):       status, slug = 400, "invalid"
    case errors.Is(err, errs.ErrNotFound), errors.Is(err, pkggrafana.ErrDashboardNotFound):
        status, slug = 404, "not-found"
    default:
        // 连接失败、Grafana 鉴权失败、dashboard 解析错误等 → 502 upstream
        status, slug = 502, "upstream"
    }
    writeJSON(w, status, errorBody{Error: err.Error(), Code: slug})
}
```

`pkggrafana.ErrDashboardNotFound` 显式映射到 404，让 SPA 区分"dashboard 不存在"和"Grafana 不可达"。

---

## 6. biz/monitor：用户面板镜像链路

[internal/manager/biz/monitor/service.go](file:///d:/claude/ongrid/internal/manager/biz/monitor/service.go) 是用户自管 PromQL 面板的 CRUD BC，也是 Grafana 镜像的触发源。

### 6.1 GrafanaSyncer 接口

[monitor/service.go:55-57](file:///d:/claude/ongrid/internal/manager/biz/monitor/service.go#L55-L57)：

```go
type GrafanaSyncer interface {
    SyncMonitorPanels(ctx context.Context, panels []*model.Panel) error
}
```

biz/grafana.Service 通过这个窄接口被注入。nil 时降级为"仅持久化不同步"（测试 / 禁用 Grafana 部署）。

### 6.2 kickSync：异步触发

[monitor/service.go:253-299](file:///d:/claude/ongrid/internal/manager/biz/monitor/service.go#L253-L299) 是核心异步机制：

```go
func (s *Service) kickSync(op string, panelID uint64) {
    if s.syncer == nil { return }
    go func() {
        ctx, cancel := context.WithTimeout(context.Background(), s.syncTO) // 30s
        defer cancel()
        panels, err := s.repo.List(ctx) // 重读全表，确保最新状态
        if err != nil { s.log.Warn("...list panels failed"); return }
        if err := s.syncer.SyncMonitorPanels(ctx, panels); err != nil {
            s.log.Warn("...grafana mirror failed", ...)
            if panelID != 0 {
                s.repo.SetSyncResult(ctx, panelID, truncateErr(err)) // 写 last_sync_error
            }
            return
        }
        // 成功 → 清空所有面板的 last_sync_error（"all green"信号）
        for _, p := range panels {
            if p.LastSyncError == "" { continue }
            s.repo.SetSyncResult(ctx, p.ID, "")
        }
    }()
}
```

设计要点：
- **goroutine + 30s ctx**：detached，不阻塞 API；卡住的 Grafana 不会永远占用 goroutine。
- **重读全表**：多个快速连续编辑时，最后一次 List 抓到的是最终状态——每次 kickSync 都全量重推，无需排队去重。
- **错误持久化**：失败写 `last_sync_error`（截断到 480 字符 + "…"，[monitor/service.go:302-311](file:///d:/claude/ongrid/internal/manager/biz/monitor/service.go#L302-L311)），SPA 显示"同步失败"角标。
- **成功清错**：成功的同步清空所有面板的 `last_sync_error`，给 UI 确定性"全绿"信号。

### 6.3 SyncNow：启动时首次同步

[monitor/service.go:231-240](file:///d:/claude/ongrid/internal/manager/biz/monitor/service.go#L231-L240)：

```go
func (s *Service) SyncNow(ctx context.Context) error {
    if s.syncer == nil { return nil }
    panels, err := s.repo.List(ctx)
    if err != nil { return err }
    return s.syncer.SyncMonitorPanels(ctx, panels)
}
```

被 main.go 在 bootstrap 后调用（[main.go:528](file:///d:/claude/ongrid/cmd/ongrid/main.go#L528)），让 ongrid-monitor dashboard 在新装环境也存在——至少有核心面板，否则"在 Grafana 中打开"命中空/缺失 dashboard。

### 6.4 Create / Update / Delete：API 返回即触发

[monitor/service.go:121-165, 170-211, 216-222](file:///d:/claude/ongrid/internal/manager/biz/monitor/service.go#L121-L222) 三个 CRUD 方法都是"持久化成功 → kickSync → 返回 200"。同步失败不影响 API 状态码。

---

## 7. systemhealth：Grafana 健康探测

[internal/manager/service/systemhealth/service.go](file:///d:/claude/ongrid/internal/manager/service/systemhealth/service.go#L190-L212) 把 Grafana 列为 12 个 Check 之一：

### 7.1 GrafanaTester 接口

[systemhealth/service.go:64-66](file:///d:/claude/ongrid/internal/manager/service/systemhealth/service.go#L64-L66)：

```go
type GrafanaTester interface {
    Test(ctx context.Context) error
}
```

biz/grafana.Service 满足。main.go 装配（[main.go:1730](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1730)）：`Grafana: grafanaSvc`。

### 7.2 checkGrafana：三态判定

[systemhealth/service.go:190-203](file:///d:/claude/ongrid/internal/manager/service/systemhealth/service.go#L190-L203)：

```go
func (s *Service) checkGrafana(ctx context.Context) Check {
    return s.probe(ctx, "grafana", "observability", "Grafana", func(ctx context.Context) (Status, string, map[string]any) {
        if s.deps.Grafana == nil {
            return StatusDegraded, "Grafana service is not wired", nil
        }
        if err := s.deps.Grafana.Test(ctx); err != nil {
            if isGrafanaConfigMissing(err) {
                return StatusDegraded, "Grafana integration is not configured: " + err.Error(), nil
            }
            return StatusFailed, "Grafana test failed: " + err.Error(), nil
        }
        return StatusOK, "Grafana test succeeded", nil
    })
}
```

三态：
- `StatusDegraded`（未装配 / 配置缺失）—— 不影响核心功能，UI 提示配置
- `StatusFailed`（配置完整但探测失败）—— Grafana 实际不可达
- `StatusOK`（探测通过）

### 7.3 isGrafanaConfigMissing：配置缺失识别

[systemhealth/service.go:205-212](file:///d:/claude/ongrid/internal/manager/service/systemhealth/service.go#L205-L212)：

```go
func isGrafanaConfigMissing(err error) bool {
    msg := err.Error()
    return strings.Contains(msg, "root_url is empty") ||
        strings.Contains(msg, "sa_token / api_key empty")
}
```

字符串匹配 biz/grafana.service.client() 抛的两种"配置未完成"错误，让 systemhealth 区分"还没配"vs"配错了"。

---

## 8. 配置与启动装配

### 8.1 GrafanaConfig 结构

[internal/pkg/config/config.go:104-133](file:///d:/claude/ongrid/internal/pkg/config/config.go#L104-L133)：

```go
type GrafanaConfig struct {
    InternalRootURL   string // env ONGRID_GRAFANA_INTERNAL_URL，默认 http://grafana:3000/grafana
    BootstrapUser     string // env ONGRID_GRAFANA_BOOTSTRAP_USER，默认 admin
    BootstrapPassword string // env ONGRID_GRAFANA_BOOTSTRAP_PASSWORD，空 → 跳过 bootstrap
    TLSInsecure       bool   // env ONGRID_GRAFANA_TLS_INSECURE，默认 false
}
```

[config.go:449-452](file:///d:/claude/ongrid/internal/pkg/config/config.go#L449-L452) 是 env 解析：

```go
c.Grafana.InternalRootURL   = getEnv("ONGRID_GRAFANA_INTERNAL_URL", "http://grafana:3000/grafana")
c.Grafana.BootstrapUser     = getEnv("ONGRID_GRAFANA_BOOTSTRAP_USER", "admin")
c.Grafana.BootstrapPassword = getEnv("ONGRID_GRAFANA_BOOTSTRAP_PASSWORD", "")
c.Grafana.TLSInsecure       = getEnvBool("ONGRID_GRAFANA_TLS_INSECURE", false)
```

这些只是 **first-boot seed**——运行时值在 system_settings，可在 UI 实时编辑。

### 8.2 settings model 常量

[internal/manager/model/setting/model.go:40, 146-164](file:///d:/claude/ongrid/internal/manager/model/setting/model.go#L146-L164)：

```go
CategoryGrafana = "grafana"

const (
    KeyGrafanaRootURL = "root_url"
    KeyGrafanaSAToken = "sa_token" // sensitive — Grafana service-account token
    KeyGrafanaAPIKey  = "api_key"  // sensitive — alternative bearer for external Grafana
    KeyGrafanaOrgID   = "org_id"
)
```

注释明确 sa_token vs api_key 的语义差异：sa_token 是嵌入式 Grafana 的 bootstrap 产物，api_key 是外部 Grafana 的"无 admin 权限妥协"——两者落到同一个 Bearer header，但 UI 暴露为独立字段。

### 8.3 main.go 装配链

**种子 root_url**（[main.go:448-455](file:///d:/claude/ongrid/cmd/ongrid/main.go#L448-L455)）：

```go
if err := settingSvc.SetIfAbsent(rootCtx, settingmodel.CategoryGrafana,
    settingmodel.KeyGrafanaRootURL, cfg.Grafana.InternalRootURL, false); err != nil {
    log.Warn("seed grafana root_url", ...)
}
```

`SetIfAbsent` 关键——首次启动写入默认值，后续 admin 在 UI 改的值跨重启保留。

**构建 grafanaSvc**（[main.go:482-490](file:///d:/claude/ongrid/cmd/ongrid/main.go#L482-L490)）：

```go
grafanaSvc := managerbizgrafana.New(settingSvc, cfg.Grafana.TLSInsecure,
    log.With(slog.String("comp", "grafana")))
if v := os.Getenv("ONGRID_GRAFANA_PANEL_DASHBOARD_UID"); v != "" {
    grafanaSvc.SetPanelDashboardUID(v)
}
```

**构建 monitorSvc**（[main.go:496-498](file:///d:/claude/ongrid/cmd/ongrid/main.go#L496-L498)）：

```go
monitorRepo := managermonitordata.NewRepo(db)
monitorSvc := managerbizmonitor.New(monitorRepo, grafanaSvc,
    log.With(slog.String("comp", "monitor")))
monitorHandler := managerservermonitor.NewHandler(monitorSvc)
```

grafanaSvc 同时被注入 monitorSvc（作为 GrafanaSyncer）和 integrationHandler（作为 GrafanaService 接口实现）。

**Bootstrap goroutine**（[main.go:503-534](file:///d:/claude/ongrid/cmd/ongrid/main.go#L503-L534)）：

```go
if cfg.Grafana.BootstrapPassword != "" {
    go func() {
        // 给 Grafana ~10s 启动（compose 只保证顺序不保证 ready）
        t := time.NewTimer(10 * time.Second)
        defer t.Stop()
        select {
        case <-rootCtx.Done(): return
        case <-t.C:
        }
        grafanaSvc.BootstrapEmbedded(rootCtx, cfg.Grafana.BootstrapUser, cfg.Grafana.BootstrapPassword)
        // SA token 落库后立即推一次 ongrid-monitor dashboard
        syncCtx, syncCancel := context.WithTimeout(rootCtx, 30*time.Second)
        defer syncCancel()
        if err := monitorSvc.SyncNow(syncCtx); err != nil {
            log.Warn("monitor: initial grafana mirror sync failed (retries on next panel edit)", ...)
        } else {
            log.Info("monitor: ongrid-monitor dashboard synced at boot")
        }
    }()
}
```

后台 goroutine 的三个理由（[main.go:503-509](file:///d:/claude/ongrid/cmd/ongrid/main.go#L503-L509) 注释）：
1. Grafana 容器通常未 ready
2. 不阻塞 API listener
3. bootstrap 失败非致命，admin 仍可手动配置

**systemhealth 装配**（[main.go:1713-1738](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1713-L1738)）：

```go
systemHealthSvc := managersvcsystemhealth.New(managersvcsystemhealth.Config{
    // ...
}, managersvcsystemhealth.Dependencies{
    DB:        healthDB,
    Prom:      promTester,
    Grafana:   grafanaSvc,  // ← 注入
    Loki:      lokiProbe,
    Tempo:     tempoProbe,
    // ...
})
```

**integrationHandler 装配**（[main.go:992](file:///d:/claude/ongrid/cmd/ongrid/main.go#L992)）：

```go
integrationHandler = managerserverintegration.NewHandler(grafanaSvc, promTester, lokiProbe, tempoProbe, webSearchProbe)
```

---

## 9. SPA 侧：dashboard 代理读取与变量替换

### 9.1 fetchDashboard API

[web/src/api/grafana.ts:82-87](file:///d:/claude/ongrid/web/src/api/grafana.ts#L82-L87)：

```typescript
export async function fetchDashboard(uid: string): Promise<GrafanaDashboardResp> {
  return request<GrafanaDashboardResp>(
    'GET',
    `/observability/dashboards/${encodeURIComponent(uid)}`,
  );
}
```

注释明确"走 manager 而非直连 Grafana"——外部 Grafana 场景无需 CORS / cookie 共享，manager 持有 api_key / sa_token。

### 9.2 GrafanaDashboard 类型

[grafana.ts:45-73](file:///d:/claude/ongrid/web/src/api/grafana.ts#L45-L73) 只建模渲染用到的字段：

```typescript
export type GrafanaPanel = {
  id: number;
  type: string;
  title: string;
  gridPos: { x: number; y: number; w: number; h: number };
  targets?: GrafanaTarget[];
  fieldConfig?: GrafanaFieldConfig;
  panels?: GrafanaPanel[]; // row panel 的嵌套子面板
  collapsed?: boolean;
};
```

注释明确"Grafana 全 schema 巨大且每 minor 版本变化——SPA 保持松散，未知字段忽略"。

### 9.3 grafanaVars：模板变量替换

[web/src/lib/grafanaVars.ts:29-36](file:///d:/claude/ongrid/web/src/lib/grafanaVars.ts#L29-L36) `rateIntervalForRange`：

```typescript
export function rateIntervalForRange(range: string): string {
  const ms = rangeToMs(range);
  if (ms <= 15 * 60_000) return '1m';
  if (ms <= 6 * 3600_000) return '5m';
  if (ms <= 24 * 3600_000) return '5m';
  if (ms <= 7 * 86400_000) return '1h';
  return '6h';
}
```

模拟 Grafana "max(1m, 4*step)" 规则，让 `rate(metric[$__rate_interval])` 不用模拟 intervalCalculator 也能渲染。

[grafanaVars.ts:82-104](file:///d:/claude/ongrid/web/src/lib/grafanaVars.ts#L82-L104) `substitute`：

```typescript
export function substitute(expr: string, ctx: VarContext): string {
  // 1. 特殊变量表：__rate_interval / __interval / __range
  const special: Record<string, string> = {
    __rate_interval: rateIntervalForRange(ctx.range),
    __interval:      rateIntervalForRange(ctx.range),
    __range:         ctx.range,
  };
  // 2. ${name} 形式先替换（防被 bare-$ pass 吃掉）
  let out = expr.replace(/\$\{([A-Za-z_][A-Za-z0-9_]*)(?::[^}]*)?\}/g, ...);
  // 3. $name 形式（不替换 $1 纯数字——Prom 锚点）
  out = out.replace(/\$([A-Za-z_][A-Za-z0-9_]*)/g, ...);
  return out;
}
```

[grafanaVars.ts:127-139](file:///d:/claude/ongrid/web/src/lib/grafanaVars.ts#L127-L139) `flattenPanels` 把 row panel 的嵌套 children 拍平到顶层列表，row 本身（chrome）被过滤。

---

## 10. SPA 侧：drilldown 与 Explore 深链

[web/src/lib/drilldown.ts](file:///d:/claude/ongrid/web/src/lib/drilldown.ts) 是浏览器跳 Grafana 的另一条路——不走 manager 代理，直接 `window.open` 一个 Grafana URL。

### 10.1 fetchGrafanaRootURL：docker URL 检测

[drilldown.ts:95-120](file:///d:/claude/ongrid/web/src/lib/drilldown.ts#L95-L120)：

```typescript
export async function fetchGrafanaRootURL(): Promise<string> {
  // 60s TTL 缓存
  if (cachedRoot != null && now - cachedAt < ROOT_TTL_MS) return cachedRoot;
  const sameOrigin = `${window.location.origin}/grafana`;
  try {
    const r = await listSettings('grafana');
    for (const it of r.items) {
      if (it.key === 'root_url' && (it.value ?? '').trim() !== '') {
        const stored = trimTrailingSlash(it.value);
        // 嵌入式存的是 http://grafana:3000/grafana（docker DNS）— 浏览器不可达
        // 回退到 ongrid 同源 nginx /grafana/ 代理
        cachedRoot = isBrowserReachableURL(stored) ? stored : sameOrigin;
        return cachedRoot;
      }
    }
  } catch { /* fall through */ }
  cachedRoot = sameOrigin;
  return cachedRoot;
}
```

[drilldown.ts:127-137](file:///d:/claude/ongrid/web/src/lib/drilldown.ts#L127-L137) `isBrowserReachableURL`：localhost / 127.0.0.1 / 无点无冒号的主机名（docker 内部名）→ false。

`invalidateGrafanaRootCache`（[drilldown.ts:90-93](file:///d:/claude/ongrid/web/src/lib/drilldown.ts#L90-L93)）让集成卡保存新值后立即失效缓存。

### 10.2 openObservabilityUrl：票据 cookie + 弹窗

[drilldown.ts:35-49](file:///d:/claude/ongrid/web/src/lib/drilldown.ts#L35-L49) 是跳 Grafana Explore 的关键：

```typescript
export async function openObservabilityUrl(url: string): Promise<void> {
  const popup = window.open('about:blank', '_blank'); // 同步开窗，绕过 popup blocker
  try {
    await createPrometheusLaunch({ expr: 'up' }); // mint 票据 cookie
  } catch { /* cookie 没 mint — fall through */ }
  if (popup && !popup.closed) {
    popup.location.replace(url);
    return;
  }
  window.location.href = url; // 弹窗被拦 — 当前 tab 跳
}
```

注释（[drilldown.ts:6-31](file:///d:/claude/ongrid/web/src/lib/drilldown.ts#L6-L31)）解释两个微妙点：
1. **Popup blocker**：~150ms 的 launch RPC 把 window.open 推过用户手势窗口，浏览器拦弹窗——解法是同步开 about:blank，cookie mint 后再 navigate handle。
2. **noopener 不可用**：noopener 返回 null 无法 navigate；同源 Grafana 下 noopener 的 mitigation 也不适用。

NB：cookie 只过 nginx auth_request，Explore 本身在 Grafana 内是 Editor/Admin gated——嵌入式 Grafana 必须配 `GF_AUTH_ANONYMOUS_ORG_ROLE=Editor`（docker-compose 已配，见 §11）。

### 10.3 openMetricDrilldown：dashboard 深链

[drilldown.ts:170-183](file:///d:/claude/ongrid/web/src/lib/drilldown.ts#L170-L183)：

```typescript
export async function openMetricDrilldown(input: DrilldownInput): Promise<void> {
  const launch = await createPrometheusLaunch({
    expr: input.expr,
    range_input: input.rangeInput,
    step_input: input.stepInput,
  });
  const grafanaUrl = await buildGrafanaUrl(input);
  if (grafanaUrl) {
    window.open(grafanaUrl, '_blank', 'noopener,noreferrer');
    return;
  }
  window.open(launch.url, '_blank', 'noopener,noreferrer');
}
```

buildGrafanaUrl（[drilldown.ts:152-168](file:///d:/claude/ongrid/web/src/lib/drilldown.ts#L152-L168)）拼 `${baseUrl}/d/${dashboardUid}/server-detail?from=...&to=now&lang=...&orgId=...&var-device_id=...`。

### 10.4 buildExploreUrl：Grafana 11 Explore 新协议

[drilldown.ts:207-233](file:///d:/claude/ongrid/web/src/lib/drilldown.ts#L207-L233) 适配 Grafana 11 退役的旧 `?left={...}` JSON 协议：

```typescript
export function buildExploreUrl(opts: {
  base: string;
  dsType: 'loki' | 'tempo' | 'prometheus';
  dsUid: string;
  query: Record<string, unknown>;
  fromMs: number | string;
  toMs: number | string;
  orgId?: string;
}): string {
  const ds = { type: opts.dsType, uid: opts.dsUid };
  const pane = {
    datasource: opts.dsUid,
    queries: [{ refId: 'A', datasource: ds, ...opts.query }],
    range: { from: String(opts.fromMs), to: String(opts.toMs) },
  };
  const panes = JSON.stringify({ og: pane });
  const params = new URLSearchParams();
  params.set('schemaVersion', '1');
  params.set('panes', panes);
  params.set('lang', grafanaLangFromLocale());
  if (opts.orgId && opts.orgId.trim()) params.set('orgId', opts.orgId.trim());
  return `${base}/explore?${params.toString()}`;
}
```

注释（[drilldown.ts:192-206](file:///d:/claude/ongrid/web/src/lib/drilldown.ts#L192-L206)）说明 Grafana 11 期望 `panes` 是 keyed by 任意短 id 的对象，每个 query 的 `datasource` 必须是 OBJECT `{type, uid}`——bare-string 形式在 v11 不再解析。

---

## 11. 部署面：docker-compose + nginx + provisioning

### 11.1 docker-compose grafana 服务

[deploy/docker-compose.yml:317-363](file:///d:/claude/ongrid/deploy/docker-compose.yml#L317-L363)：

```yaml
  grafana:
    image: docker.cnb.cool/ongridio/ongrid/grafana-oss:11.1.4
    container_name: ongrid-grafana
    restart: unless-stopped
    depends_on: [prometheus, loki, tempo]
    environment:
      GF_SERVER_ROOT_URL: ${ONGRID_GRAFANA_ROOT_URL:-%(protocol)s://%(domain)s/grafana/}
      GF_SERVER_SERVE_FROM_SUB_PATH: "true"
      GF_SECURITY_ADMIN_USER: ${GRAFANA_ADMIN_USER:-admin}
      GF_SECURITY_ADMIN_PASSWORD: ${GRAFANA_ADMIN_PASSWORD:-admin}
      GF_AUTH_ANONYMOUS_ENABLED: "true"
      GF_AUTH_ANONYMOUS_ORG_ROLE: Editor
      GF_AUTH_DISABLE_LOGIN_FORM: "true"
      GF_USERS_DEFAULT_THEME: system
      GF_USERS_DEFAULT_LANGUAGE: en-US
      GF_ANALYTICS_REPORTING_ENABLED: "false"
      GF_ANALYTICS_CHECK_FOR_UPDATES: "false"
      GF_SECURITY_ALLOW_EMBEDDING: "true"
      ONGRID_LOG_URL: ${ONGRID_LOG_URL:-http://loki:3100}
      ONGRID_TRACE_QUERY_URL: ${ONGRID_TRACE_QUERY_URL:-http://tempo:3200}
    ports:
      - "${GRAFANA_PORT:-3000}:3000"
    volumes:
      - grafana_data:/var/lib/grafana
      - ./grafana/provisioning:/etc/grafana/provisioning:ro
    networks: [ongrid_net]
```

关键 env：
- `GF_SERVER_SERVE_FROM_SUB_PATH=true`：让 Grafana 服务在 `/grafana` 子路径下，nginx 反代时无需重写
- `GF_AUTH_ANONYMOUS_ORG_ROLE=Editor`：浏览器无登录直进 Editor 角色——drilldown.ts 注释（[drilldown.ts:28-31](file:///d:/claude/ongrid/web/src/lib/drilldown.ts#L28-L31)）明确这是 Explore 能用的前提
- `GF_AUTH_DISABLE_LOGIN_FORM=true`：禁登录表单，纯靠 nginx auth_request 票据
- `GF_SECURITY_ALLOW_EMBEDDING=true`：允许 iframe 嵌入（/monitor 页 solo-mode panel 需要）
- `GF_USERS_DEFAULT_LANGUAGE=en-US`：兜底语言，per-tab 通过 URL `?lang=` 覆盖

### 11.2 manager 容器的 Grafana env

[docker-compose.yml:107-112](file:///d:/claude/ongrid/deploy/docker-compose.yml#L107-L112)（manager 服务 environment）：

```yaml
ONGRID_GRAFANA_INTERNAL_URL: ${ONGRID_GRAFANA_INTERNAL_URL:-http://grafana:3000/grafana}
ONGRID_GRAFANA_BOOTSTRAP_USER: ${ONGRID_GRAFANA_BOOTSTRAP_USER:-${GRAFANA_ADMIN_USER:-admin}}
ONGRID_GRAFANA_BOOTSTRAP_PASSWORD: ${ONGRID_GRAFANA_BOOTSTRAP_PASSWORD-${GRAFANA_ADMIN_PASSWORD:-admin}}
ONGRID_GRAFANA_TLS_INSECURE: ${ONGRID_GRAFANA_TLS_INSECURE:-false}
```

`ONGRID_GRAFANA_INTERNAL_URL` 是 docker 网络内 manager 访问 Grafana 的 URL（服务名 + 子路径），与浏览器视角的 URL 不同——这是 fetchGrafanaRootURL `isBrowserReachableURL` 检测的关键场景。

### 11.3 nginx `/grafana/` 反代

[deploy/nginx/nginx.conf:315-329](file:///d:/claude/ongrid/deploy/nginx/nginx.conf#L315-L329)：

```nginx
location /grafana/ {
    auth_request /__auth_prometheus;            # 票据 cookie 校验
    auth_request_set $auth_cookie $upstream_http_set_cookie;
    add_header Set-Cookie $auth_cookie;         # 滑动续期
    proxy_pass http://grafana_backend;
    proxy_http_version 1.1;
    proxy_set_header Host              $host;
    proxy_set_header X-Real-IP         $remote_addr;
    proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header Connection "";
    proxy_connect_timeout 10s;
    proxy_send_timeout    120s;
    proxy_read_timeout    120s;
}
```

与 `/prometheus/` 共用 `__auth_prometheus` 子请求（[nginx.conf:293-313](file:///d:/claude/ongrid/deploy/nginx/nginx.conf#L293-L313)），manager 验 JWT 后 mint 票据 cookie，滑动续期靠 `auth_request_set $auth_cookie` + `add_header Set-Cookie`。

`upstream grafana_backend` 定义在 [nginx.conf:39-42](file:///d:/claude/ongrid/deploy/nginx/nginx.conf#L39-L42)：

```nginx
upstream grafana_backend {
    server grafana:3000;
    keepalive 8;
}
```

### 11.4 provisioning：文件型数据源 + dashboard

[deploy/grafana/provisioning/dashboards/default.yml](file:///d:/claude/ongrid/deploy/grafana/provisioning/dashboards/default.yml)：

```yaml
apiVersion: 1
providers:
  - name: ongrid-default
    orgId: 1
    folder: ongrid
    folderUid: ongrid
    type: file
    disableDeletion: false
    updateIntervalSeconds: 30
    allowUiUpdates: false         # ← 关键：UI 不可改
    options:
      path: /etc/grafana/provisioning/dashboards/json
```

`allowUiUpdates: false` 让这些文件型 dashboard 不可通过 Grafana API 修改——但与 biz/grafana.Sync 推送的 dashboard（走 `/api/dashboards/db` API，非 provisioning）是两套体系。文件型是首次部署的 baseline，API 推送是运行时同步。

[deploy/grafana/provisioning/datasources/loki.yml](file:///d:/claude/ongrid/deploy/grafana/provisioning/datasources/loki.yml)：

```yaml
datasources:
  - name: ongrid-loki
    uid: ongrid-loki
    type: loki
    access: proxy
    url: ${ONGRID_LOG_URL}
    isDefault: false
    editable: false              # ← 关键：触发 UpsertDatasource 的 readOnly 短路
    jsonData:
      timeout: 60
      maxLines: 5000
```

`editable: false` 让 pkg/grafana.Client.UpsertDatasource 的 readOnly 检测命中——Sync 不会改它，但 dashboard 通过 UID 引用照常工作。

tempo.yml 同构（datasource uid=ongrid-tempo）。

### 11.5 内嵌 dashboard JSON 样本

[internal/manager/biz/grafana/dashboards/cluster-overview.json](file:///d:/claude/ongrid/internal/manager/biz/grafana/dashboards/cluster-overview.json) 通过 `//go:embed` 编译进二进制（[service.go:46-47](file:///d:/claude/ongrid/internal/manager/biz/grafana/service.go#L46-L47)）。每个 panel 的 datasource 引用：

```json
{
  "datasource": {
    "type": "prometheus",
    "uid": "ongrid-prometheus"
  },
  ...
}
```

`uid: "ongrid-prometheus"` 与 biz/grafana 的 `datasourceUID` 常量（[service.go:42](file:///d:/claude/ongrid/internal/manager/biz/grafana/service.go#L42)）一致——这是 dashboard 能查到数据源的契约。

---

## 12. 并发、错误与可观测性

### 12.1 并发模型

| 路径 | 并发模型 |
|------|----------|
| Test / Sync（HTTP 同步调用） | 单 HTTP 请求一个 goroutine，无共享状态 |
| BootstrapEmbedded | main.go 启动时单个 goroutine，10s 延迟后执行 |
| SyncMonitorPanels（kickSync） | 每次面板 CRUD 触发一个 detached goroutine，30s ctx 超时 |
| FetchDashboardJSON | 单 HTTP 请求，无并发 |
| pkg/grafana.Client.do | 无状态，hc 共用 |

biz/grafana.Service 字段（[service.go:50-56](file:///d:/claude/ongrid/internal/manager/biz/grafana/service.go#L50-L56)）全部在构造时确定后不可变，`panelDashboardUID` 仅通过 `SetPanelDashboardUID`（[service.go:77-83](file:///d:/claude/ongrid/internal/manager/biz/grafana/service.go#L77-L83)）在启动阶段单线程调用——无锁安全。

### 12.2 错误传播链

```
Grafana API 错误（401/403/404/500/网络）
   │
   ▼ pkg/grafana.Client.do
   ├─ 404 → notFoundErr
   ├─ 非 2xx → fmt.Errorf("grafana: %s %s returned %d: %s", ..., string(respBody))
   └─ 网络错误 → fmt.Errorf("grafana: %s %s: %w", ..., err)
   │
   ▼ biz/grafana.Service.{Test,Sync,FetchDashboardJSON}
   ├─ FetchDashboard: 404 → 包装 ErrDashboardNotFound
   └─ 其他原样上抛
   │
   ▼ server/integration/http.go writeErr
   ├─ ErrUnauthorized → 401
   ├─ ErrForbidden → 403
   ├─ ErrInvalid → 400
   ├─ ErrNotFound | ErrDashboardNotFound → 404
   └─ default → 502 upstream（含原文）
   │
   ▼ SPA
   └─ ApiError 展示
```

monitor kickSync 的错误（[monitor/service.go:268-280](file:///d:/claude/ongrid/internal/manager/biz/monitor/service.go#L268-L280)）不向上抛，而是写到 `last_sync_error` 列。

### 12.3 日志

biz/grafana.Service 用 `slog.With(slog.String("comp", "grafana"))`（[main.go:484](file:///d:/claude/ongrid/cmd/ongrid/main.go#L484)）标记组件：
- `Info` "grafana bootstrap done"
- `Info` "grafana dashboard pushed" (per dashboard)
- `Info` "grafana sync done"
- `Warn` "grafana bootstrap: ... failed; skipping"（多个分支）
- `Debug` "grafana bootstrap skipped: ..."

monitor.Service 用 `slog.With(slog.String("comp", "monitor"))`：
- `Warn` "monitor sync: list panels failed"
- `Warn` "monitor sync: grafana mirror failed"
- `Debug` "monitor sync: grafana mirror ok"

### 12.4 可观测性盲点

- **无 metrics**：biz/grafana 没暴露 Prometheus 指标（Sync 次数 / 失败次数 / 推送延迟）。运营方只能靠日志和 systemhealth 的 StatusOK/Failed 二态判定。
- **无 trace**：HTTP 调用未注入 trace_id。可通过 manager 入口的 trace middleware 间接关联。
- **dashboard 推送无去重**：每次 Sync 都重推所有 embed dashboard，Grafana 端 overwrite=true 接受。规模小（2-3 个 dashboard）可接受。

---

## 13. 架构红线与设计要点

### 13.1 红线

1. **凭据不进浏览器**：FetchDashboardJSON 必须经 manager 代理，sa_token / api_key 永远不下发到 SPA。drilldown.ts 走 nginx auth_request 票据 cookie，绕过凭据直传。
2. **单向同步**：ongrid → Grafana 是单向，Grafana 内编辑会被下次推送覆盖。biz/monitor 是唯一真源。
3. **bootstrap 非致命**：BootstrapEmbedded 任何失败都 `return + log`，绝不 panic 或阻塞启动。
4. **provisioning editable:false 不可改**：UpsertDatasource 的 readOnly 检测是契约——文件型数据源只能通过重新部署改，不能通过 API 改。
5. **dashboard.id 必须删**：UpsertDashboard 的 `delete(raw, "id")` 是关键，否则新装环境按 id 查找失败。
6. **uid 常量不可改**：folderUID / datasourceUID / panelDashboardUID 改名会创建重复而非替换。
7. **sa_token 优先于 api_key**：client() 的优先级是设计契约，sa_token 是嵌入式快乐路径，api_key 是外部 Grafana 兜底。
8. **1 MiB cap**：do() 的 `io.LimitReader(resp.Body, 1<<20)` 防恶意/异常大响应。
9. **requireAdmin for write, requireUser for read**：testGrafana / syncGrafana 是 admin-only，fetchDashboard 是 any-auth-user。
10. **配置缺失不等于失败**：systemhealth 的 isGrafanaConfigMissing 让"未配置"返回 StatusDegraded 而非 StatusFailed，UI 区分对待。
11. **docker-internal URL 不可达浏览器**：fetchGrafanaRootURL 的 isBrowserReachableURL 检测是必备——嵌入式部署 root_url 存的是 `http://grafana:3000/grafana`，浏览器无法解析 docker DNS。
12. **popup blocker 绕过**：openObservabilityUrl 同步开 about:blank 再 navigate handle，否则 ~150ms RPC 推过用户手势窗口被拦。
13. **Grafana 11 Explore 新协议**：buildExploreUrl 必须用 `panes` JSON + OBJECT datasource，旧 `?left=` JSON 在 v11 已退役。

### 13.2 设计要点

- **薄客户端 + 厚编排**：pkg/grafana 是无状态 SDK，biz/grafana 持有所有业务上下文（settings / prom 凭据 / embed dashboards）。SDK 可独立复用。
- **接口在消费方定义**：GrafanaService / GrafanaSyncer / GrafanaTester 三个接口分别定义在 server/integration / biz/monitor / service/systemhealth，biz/grafana.Service 结构化满足全部。
- **embed.FS 内嵌 dashboard**：编译进二进制，部署无需携带 JSON 文件，重启自动重推。
- **SetIfAbsent 种子**：first-boot 写默认值，admin 编辑跨重启保留。
- ** detached goroutine + 30s ctx**：kickSync 不阻塞 API，卡住的 Grafana 不永远占用 goroutine。
- **重读全表**：kickSync 不传 panel 副本，每次重新 List——快速连续编辑的最后一次状态自然胜出，无需排队去重。
- **错误持久化到列**：last_sync_error 让 UI 不轮询 Grafana 也能展示同步状态。
- **coreMonitorPanels 与前端 lockstep**：硬编码 10 个核心面板在 Go 侧重新声明，注释明确"KEEP IN LOCKSTEP WITH Monitor.tsx"。
- **schemaVersion: 38**：buildMonitorDashboardJSON 写死 Grafana schema 38（对应 Grafana 11.x），让 Grafana 不触发 schema 迁移提示。

---

## 14. 附录：测试覆盖

### 14.1 pkg/grafana/client_test.go

[internal/pkg/grafana/client_test.go](file:///d:/claude/ongrid/internal/pkg/grafana/client_test.go) 用 httptest 桩覆盖：

| 测试 | 行 | 验证点 |
|------|----|--------|
| `TestHealthSendsBearerAndDecodesOK` | [14-31](file:///d:/claude/ongrid/internal/pkg/grafana/client_test.go#L14-L31) | `/api/health` 路径 + Bearer header + database=ok 解码 |
| `TestHealthRejectsNonOK` | [33-43](file:///d:/claude/ongrid/internal/pkg/grafana/client_test.go#L33-L43) | database != ok 返回 error |
| `TestUpsertDatasourceCreatesWhenAbsent` | [45-76](file:///d:/claude/ongrid/internal/pkg/grafana/client_test.go#L45-L76) | 404 → POST /api/datasources |
| `TestUpsertDatasourceSkipsReadOnly` | [78-103](file:///d:/claude/ongrid/internal/pkg/grafana/client_test.go#L78-L103) | readOnly=true 短路，PUT 不调用 |
| `TestUpsertDatasourceUpdatesWhenPresent` | [105-130](file:///d:/claude/ongrid/internal/pkg/grafana/client_test.go#L105-L130) | 存在 → PUT /api/datasources/{ID} |
| `TestUpsertDashboardSendsWrappedPayload` | [132-162](file:///d:/claude/ongrid/internal/pkg/grafana/client_test.go#L132-L162) | wrapper 含 overwrite / folderUid，dashboard.id 被删 |
| `TestFetchDashboardReturnsRawJSONOn2xx` | [164-185](file:///d:/claude/ongrid/internal/pkg/grafana/client_test.go#L164-L185) | 原样 bytes 返回 + Bearer 头 |
| `TestFetchDashboardMaps404ToErrDashboardNotFound` | [187-198](file:///d:/claude/ongrid/internal/pkg/grafana/client_test.go#L187-L198) | 404 → ErrDashboardNotFound 哨兵 |
| `TestFetchDashboardRejectsEmptyUID` | [200-206](file:///d:/claude/ongrid/internal/pkg/grafana/client_test.go#L200-L206) | 空 uid 报错 |
| `TestFetchDashboardWithoutAuthOmitsHeader` | [208-226](file:///d:/claude/ongrid/internal/pkg/grafana/client_test.go#L208-L226) | 无 token 不合成 Bearer 头 |
| `TestNon2xxBubblesUp` | [228-247](file:///d:/claude/ongrid/internal/pkg/grafana/client_test.go#L228-L247) | 401 错误含状态码；404 → notFoundErr |

### 14.2 biz/grafana/monitor_test.go

[internal/manager/biz/grafana/monitor_test.go](file:///d:/claude/ongrid/internal/manager/biz/grafana/monitor_test.go) 验证渲染逻辑（无需真实 Grafana）：

| 测试 | 行 | 验证点 |
|------|----|--------|
| `TestBuildMonitorDashboardJSON` | [14-68](file:///d:/claude/ongrid/internal/manager/biz/grafana/monitor_test.go#L14-L68) | 2×8 grid 布局、type 1:1 映射、PromQL/legend/refId/datasource uid 透传 |
| `TestMapPanelType` | [73-87](file:///d:/claude/ongrid/internal/manager/biz/grafana/monitor_test.go#L73-L87) | timeseries/stat/gauge 1:1，未知 → timeseries 兜底 |

### 14.3 未覆盖点（盲区）

- **BootstrapEmbedded**：靠集成测试 / 人工 smoke，无单测
- **Sync 全流程**：同上
- **SyncMonitorPanels**：同上
- **FetchDashboardJSON**：靠 e2e
- **systemhealth.checkGrafana**：靠 systemhealth 服务层测试间接覆盖
- **nginx auth_request 票据**：靠 e2e
- **drilldown.ts / grafanaVars.ts**：靠前端单测（如果有）

---

## 文档版本

- 撰写日期：2026-07-31
- 仓库快照：撰写时 working tree
- 覆盖代码：pkg/grafana + biz/grafana + biz/monitor + server/integration + service/systemhealth + cmd/ongrid + web/src/{api,lib} + deploy/{docker-compose,nginx,grafana}
