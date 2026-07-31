# OnGrid 外部系统集成技术实现说明文档

> 本文档深入分析 OnGrid 系统与 MySQL、Frontier、Prometheus、Loki、Qdrant、SearXNG、Tempo、Grafana 八个外部系统的集成实现，覆盖：客户端封装、认证机制、配置热更新、健康探测、数据流向、装配流程、并发模型与架构红线。

---

## 目录

1. [集成总览与拓扑](#1-集成总览与拓扑)
2. [MySQL 集成](#2-mysql-集成)
3. [Frontier 集成（singchia/frontier）](#3-frontier-集成singchiafrontier)
4. [Prometheus 集成（读 + 写）](#4-prometheus-集成读--写)
5. [Loki 集成](#5-loki-集成)
6. [Tempo 集成](#6-tempo-集成)
7. [Qdrant 集成（向量库）](#7-qdrant-集成向量库)
8. [SearXNG 集成（Web 搜索）](#8-searxng-集成web-搜索)
9. [Grafana 集成（看板同步）](#9-grafana-集成看板同步)
10. [Prometheus 代理票据（nginx auth_request）](#10-prometheus-代理票据nginx-auth_request)
11. [systemhealth 聚合探测](#11-systemhealth-聚合探测)
12. [配置热更新与 Resolver 模式](#12-配置热更新与-resolver-模式)
13. [cmd/ongrid/main.go 装配流程](#13-cmdongridmaingo-装配流程)
14. [并发与资源管理](#14-并发与资源管理)
15. [设计模式与架构红线](#15-设计模式与架构红线)
16. [关键文件索引](#16-关键文件索引)

---

## 1. 集成总览与拓扑

OnGrid 是云边协同的可观测性 + AIOps 平台，与以下八个外部系统深度集成：

| 系统 | 角色 | 集成方向 | 客户端包 |
|------|------|----------|----------|
| **MySQL** | 主关系数据库 | 读写 | `internal/pkg/dbx`（GORM） |
| **Frontier** | geminio RPC broker | edge ↔ manager 双向 | `internal/pkg/tunnel`（edge）+ `internal/manager/service/frontierbound`（manager） |
| **Prometheus** | 指标 TSDB | remote_write + PromQL 查询 | `internal/pkg/promwrite` + `internal/pkg/promquery` + `internal/pkg/promauth` |
| **Loki** | 日志后端 | LogQL 查询 + edge push | `internal/pkg/logquery` |
| **Tempo** | 链路追踪后端 | TraceQL 查询 + OTLP push | `internal/pkg/tracequery` |
| **Qdrant** | 向量库（知识库 RAG） | upsert + search | `internal/pkg/qdrantx` |
| **SearXNG** | 自托管 Web 搜索 | LLM 联网搜索 skill | `internal/skill/builtin/web_search.go` |
| **Grafana** | 可视化看板 | 自动同步 datasource + dashboard | `internal/pkg/grafana` + `internal/manager/biz/grafana` |

### 拓扑

```
                         ┌─────────────────────────────────────────────┐
                         │                  manager                     │
                         │  ┌──────┐  ┌──────────┐  ┌──────────────┐  │
                         │  │ dbx  │  │frontier- │  │  promwrite   │  │
                         │  │(MySQL)│  │ bound    │  │  promquery   │  │
                         │  └──┬───┘  └────┬─────┘  └──────┬───────┘  │
                         │     │          │               │           │
                         │  ┌──┴──┐  ┌────┴────┐  ┌──────┴───────┐  │
                         │  │log- │  │ trace-  │  │  qdrantx     │  │
                         │  │query│  │ query   │  │  (RAG)       │  │
                         │  └──┬──┘  └────┬────┘  └──────┬───────┘  │
                         │     │          │               │           │
                         │  ┌──┴──────────┴───────────────┴──────┐  │
                         │  │       biz/grafana + pkg/grafana    │  │
                         │  │       biz/setting (Resolvers)      │  │
                         │  │       service/systemhealth         │  │
                         │  └────────────────────────────────────┘  │
                         └───┬─────────┬──────────┬──────────┬──────┘
                             │         │          │          │
                    ┌────────▼───┐ ┌───▼──┐ ┌─────▼────┐ ┌───▼─────┐
                    │  MySQL     │ │Frontier│ │Prometheus│ │  Loki   │
                    │  8.0       │ │broker  │ │  + Mimir │ │         │
                    └────────────┘ └───┬───┘ └──────────┘ └─────────┘
                                       │
                                       ▼
                                  edge agent
                                       │
                          ┌────────────┴────────────┐
                          ▼                         ▼
                    ┌──────────┐              ┌──────────┐
                    │  Tempo   │              │  Qdrant  │
                    │          │              │ (vector) │
                    └──────────┘              └──────────┘
                                       │
                                       ▼
                                  SearXNG（自托管）
                                  + Tavily / Brave（外部 API）
```

---

## 2. MySQL 集成

源文件：`internal/pkg/dbx/dbx.go`

### 2.1 双后端支持

OnGrid 默认 MySQL，SQLite 作为单用户本地调试 opt-in。数据模型是 dialect-agnostic GORM。

```go
func Open(cfg config.DBConfig, log *slog.Logger) (*gorm.DB, error) {
    switch cfg.Dialect {
    case "", "mysql":
        return openMySQL(cfg.DSN, log)
    case "sqlite":
        return openSQLite(cfg.Path, log)
    }
}
```

### 2.2 openMySQL —— fail-fast

```go
func openMySQL(dsn string, log *slog.Logger) (*gorm.DB, error) {
    gdb, err := gorm.Open(gormmysql.Open(dsn), &gorm.Config{
        Logger: logger.Default.LogMode(logger.Warn),
    })
    sqlDB, _ := gdb.DB()
    if err := sqlDB.Ping(); err != nil {
        return nil, fmt.Errorf("dbx: mysql ping failed: %w", err)
    }
    log.Info("mysql opened", "endpoint", redactDSN(dsn))
    return gdb, nil
}
```

**关键设计**：

- **Ping at Open time**：配置错误 fail-fast，而非惰性首次查询失败
- **Warn-level logger**：正常查询流不污染应用日志
- **`redactDSN`**：日志中脱敏密码，保留 `user@host:port/db?params` 形态

### 2.3 openSQLite —— WAL + busy_timeout + foreign_keys

```go
func buildSQLiteDSN(path string) string {
    q := url.Values{}
    q.Add("_pragma", "journal_mode(WAL)")
    q.Add("_pragma", "busy_timeout(5000)")
    q.Add("_pragma", "foreign_keys(on)")
    return path + "?" + q.Encode()
}
```

- **WAL**：并发读 + 单写
- **busy_timeout 5000ms**：避免 SQLITE_BUSY
- **foreign_keys ON**：SQLite 默认关闭 FK，显式开启

### 2.4 配置

```env
ONGRID_DB_DIALECT=mysql
ONGRID_DB_DSN=ongrid:${MYSQL_PASSWORD}@tcp(mysql:3306)/ongrid?parseTime=true&charset=utf8mb4&loc=Local
```

### 2.5 生产 schema 变更

- 走 migration 文件（GORM AutoMigrate + 手写 migrate.go）
- 大表用在线 DDL 工具
- 变更兼容滚动发布（expand-contract）

---

## 3. Frontier 集成（singchia/frontier）

> 详细的 geminio RPC 实现见 `ongrid_rpc_singchia_geminio.md`。本节仅从集成角度概述。

### 3.1 Frontier 角色

Frontier 是独立的 `github.com/singchia/frontier` broker 容器，终结 geminio 协议，中转 edge ↔ manager 的双向 RPC + stream。

### 3.2 manager 侧集成（frontierbound）

源文件：`internal/manager/service/frontierbound/client.go`

```go
import (
    fbsvc "github.com/singchia/frontier/api/dataplane/v1/service"
    "github.com/singchia/geminio"
)

type service interface {
    NewRequest(data []byte) geminio.Request
    Call(ctx, edgeID uint64, method string, req) (Response, error)
    Register(ctx, method string, rpc) error
    RegisterGetEdgeID(ctx, fn) error
    RegisterEdgeOnline(ctx, fn) error
    RegisterEdgeOffline(ctx, fn) error
    OpenStream(ctx, edgeID uint64) (Stream, error)
    Close() error
}
```

**关键集成点**：

- **`New(addr, service_name)`**：构造 `fbsvc.NewService` 拨号到 frontier
- **`NewDisabled(log)`**：svc=nil，所有 Call 返回 `ErrDisabled`，用于 e2e / degraded-broker
- **binding map**：transport ID ↔ canonical edgeID 双向映射，三重校验防 stale offline
- **Install(wiring)**：注册所有反向 RPC handler + 生命周期回调

### 3.3 edge 侧集成（tunnel）

源文件：`internal/pkg/tunnel/client.go`

- **`gclient.NewRetryEndWithDialer`**：geminio 内管重连
- **`retryDelegate.EndReOnline`**：重连完成通知
- **代际连接管理**：pending → active 升级 + generation 校验

### 3.4 配置

```env
ONGRID_FRONTIER_ADDR=frontier:40011
ONGRID_FRONTIER_SERVICE_NAME=ongrid-manager
# 或 ONGRID_FRONTIER_DISABLED=true（e2e / degraded）
```

### 3.5 部署

`deploy/install/frontier.yaml` + `deploy/Dockerfile.frontier`，作为独立容器，不暴露 host 端口，仅 docker network 内可达。

---

## 4. Prometheus 集成（读 + 写）

OnGrid 与 Prometheus 的集成是双向的：**remote_write**（edge → manager → Prom）+ **PromQL 查询**（manager → Prom，供 AI 工具 + 告警 + UI）。

### 4.1 promwrite —— remote_write 客户端

源文件：`internal/pkg/promwrite/client.go`

```go
type Client struct {
    endpoint   EndpointResolver
    httpClient *http.Client
    log        *slog.Logger
}

func (c *Client) Write(ctx context.Context, samples []Sample) error {
    // 1. resolve endpoint URL
    // 2. 每个 Sample 编码为独立 TimeSeries（一个 series 一个 sample）
    // 3. snappy 压缩 protobuf
    // 4. POST /api/v1/write
    //    Content-Type: application/x-protobuf
    //    Content-Encoding: snappy
    //    X-Prometheus-Remote-Write-Version: 0.1.0
    // 5. 200/204 = success
}
```

**关键设计**：

- **EndpointResolver 接口**：动态解析 URL，admin 编辑 system_settings 后 ~5s 生效
- **无内部重试**：匹配 prometheus/common 行为，caller 负责 retry
- **手写 protobuf 编码**：`encodeTimeSeries` + `encodeWriteRequest`，不引 prometheus/proto 依赖
- **defaultTimeout 10s**：Prom 推荐 30s，但 ongrid 批次小（1 sample/series），用更紧的 10s
- **empty samples no-op**：caller 不需 branch

### 4.2 promquery —— PromQL 查询客户端

源文件：`internal/pkg/promquery/client.go`

```go
type Client struct {
    base       BaseURLResolver
    httpClient *http.Client
    log        *slog.Logger
}

func (c *Client) Query(ctx, expr, ts) (*InstantResult, error)        // /api/v1/query
func (c *Client) QueryRange(ctx, expr, start, end, step) (*InstantResult, error)  // /api/v1/query_range
```

**关键设计**：

- **BaseURLResolver**：每次调用解析，~5s TTL 缓存
- **8 MiB body cap**：防 OOM
- **400 也解码 JSON**：Prom 用 400 返回 query parse error，要解码看 errorType
- **InstantResult.Result 是 raw JSON**：matrix/vector/scalar 形态保留，AI tool 直接传给 LLM
- **defaultTimeout 30s**：range query 可能很慢

### 4.3 promauth —— 认证 + TLS 客户端构建

源文件：`internal/pkg/promauth/client.go`

```go
type Config struct {
    BearerToken   string  // 优先
    BasicUser     string
    BasicPassword string
}

type TLSConfig struct {
    Insecure bool
    CAPath   string
    CAPEM    string
}

func BuildClient(tlsCfg TLSConfig, resolver Resolver, timeout) (*http.Client, error)
```

**关键设计**：

- **TLS 静态**：dialer 层，构造时确定，改 TLS 需重启 manager
- **Auth 动态**：`authRoundTripper` 每请求解析，5s TTL 缓存
- **Bearer > Basic 优先级**：匹配 curl `-H` over `-u`
- **fail closed**：resolver 错误返回 error，不静默发无 auth 请求（"silent auth-less request 看起来像网络 glitch，难诊断"）
- **不支持 `bearer_token_file`**：ongrid 是 docker-managed monolith，文件挂载违背 UI-driven 配置理念

### 4.4 数据流

**写路径**：

```
edge agent（hostmetrics / k8s metrics plugin）
   ↓ push_prom_samples（tunnel RPC）
manager frontierbound.handlers.go
   ↓ host / k8s 分流
   ↓ resolveDeviceID / lookupK8sControllerCluster
biz/promwrite.ingester
   ↓ batch
pkg/promwrite.Client.Write
   ↓ snappy + protobuf
Prometheus /api/v1/write
```

**读路径**：

```
AI tool query_promql / 告警 evaluator / UI
   ↓
biz/setting.PromResolver.ResolveBaseURL（5s TTL）
   ↓
pkg/promauth.BuildClient（TLS + authRoundTripper）
   ↓
pkg/promquery.Client.Query / QueryRange
   ↓
Prometheus /api/v1/query[_range]
```

### 4.5 配置

```env
ONGRID_PROM_WRITE_URL=http://prometheus:9090/api/v1/write  # 或省略，默认 baseURL + /api/v1/write
ONGRID_PROM_QUERY_URL=http://prometheus:9090
ONGRID_PROM_BEARER_TOKEN=...
ONGRID_PROM_BASIC_USER=...
ONGRID_PROM_BASIC_PASSWORD=...
ONGRID_PROM_TLS_INSECURE=false
ONGRID_PROM_CA_PEM=...
```

system_settings `category=prom` 提供 UI 编辑。

### 4.6 ADR-009

Prometheus 是核心服务：接收 manager remote_write，驱动 AI agent 的 query_promql 工具。不暴露 host 端口；manager 通过 `http://prometheus:9090/prometheus` 访问。

---

## 5. Loki 集成

源文件：`internal/pkg/logquery/client.go`

### 5.1 客户端

```go
type Client struct {
    base       BaseURLResolver
    httpClient *http.Client
    log        *slog.Logger
}

func (c *Client) QueryRange(ctx, opts) (*QueryRangeResult, error)  // /loki/api/v1/query_range
func (c *Client) LabelNames(ctx, start, end) ([]string, error)     // /loki/api/v1/labels
func (c *Client) LabelValues(ctx, name, start, end) ([]string, error)  // /loki/api/v1/label/<name>/values
```

### 5.2 关键设计

- **包名 `logquery` 而非 `lokiquery`**：backend-decoupled，未来换 VictoriaLogs 时包名 + import path 不变
- **`X-Scope-OrgID: ongrid`**：单租户硬编码，匹配 nginx ingest 侧注入
- **纳秒时间戳**：Loki 要求 `start`/`end` 是纳秒 unix 字符串
- **direction 默认 backward**：最新优先，匹配 operator 直觉
- **limit 默认 1000**：Loki 默认 100，ongrid 提到 1000 让 UI 表格有滚动材料
- **8 MiB body cap**：防 OOM
- **defaultTimeout 30s**：宽窗口 / 大 limit 的 query_range 可能慢
- **QueryRangeResult.Result 是 raw JSON**：streams / matrix 形态保留，SPA 自己 switch

### 5.3 edge 侧 push

edge 的 logs plugin 通过 `plugin_config` 拿到 Loki URL（`LokiResolver.URL`），直接 push 到 Loki（或经 nginx auth_request 转发到内部 Loki）。

### 5.4 配置

```env
ONGRID_LOKI_URL=http://loki:3100
ONGRID_LOKI_BASIC_USER=...
ONGRID_LOKI_BASIC_PASSWORD=...
ONGRID_LOKI_TLS_INSECURE=false
```

system_settings `category=loki`。

### 5.5 LokiURLProbe

源文件：`internal/manager/biz/setting/probe.go`

```go
func (p *LokiURLProbe) Probe(ctx) error {
    return probeReadyEndpoint(ctx, u+"/ready", user, pass, tlsInsecure, 5*time.Second)
}
```

GET `<url>/ready`，5s 超时，200/2xx = ok。用于 Integrations UI 的"测试连接"按钮。

---

## 6. Tempo 集成

源文件：`internal/pkg/tracequery/client.go`

### 6.1 客户端

```go
type Client struct {
    base       BaseURLResolver
    httpClient *http.Client
    log        *slog.Logger
}

func (c *Client) SearchTraces(ctx, opts) (*SearchResult, error)  // /api/search
func (c *Client) TagValues(ctx, tag) ([]string, error)            // /api/search/tag/<tag>/values
func (c *Client) GetTrace(ctx, traceID) (*TraceResult, error)     // /api/traces/<id>
```

### 6.2 关键设计

- **包名 `tracequery`**：同样 backend-decoupled
- **TraceQL 优先**：`opts.Query`（TraceQL 表达式）→ `q=`；fallback 到 `tags=` legacy 形式
- **limit 默认 100**：Tempo 默认 20，ongrid 提到 100 给 AI 更多推理材料
- **start/end 是 unix 秒**：与 Loki（纳秒）不同，与 Prom（浮点秒）也不同
- **16 MiB body cap**：单 trace 可能很大
- **defaultTimeout 30s**：filesystem backend 的冷 block search 慢
- **404 单独处理**：trace 不存在返回明确错误
- **SearchResult.Traces 是 raw JSON**：Tempo 跨版本 schema 变化，passthrough 最稳

### 6.3 edge 侧 push

edge 的 traces plugin 通过 `plugin_config` 拿到 Tempo OTLP HTTP push URL（`TempoResolver.URL`），otelcol exporter 直接 push。

### 6.4 TempoURLProbe

```go
func (p *TempoURLProbe) Probe(ctx) error {
    base := strings.TrimSuffix(u, "/v1/traces")  // 剥离 OTLP 后缀
    return probeReadyEndpoint(ctx, base+"/ready", user, pass, tlsInsecure, 5*time.Second)
}
```

**关键**：admin 填的 URL 可能是 OTLP push URL（`/v1/traces` 结尾），probe 剥离后拼 `/ready`。

### 6.5 配置

```env
ONGRID_TEMPO_URL=http://tempo:3200  # 或 https://otelcol:4318/v1/traces
ONGRID_TEMPO_BASIC_USER=...
ONGRID_TEMPO_BASIC_PASSWORD=...
ONGRID_TEMPO_TLS_INSECURE=false
```

system_settings `category=tempo`。

---

## 7. Qdrant 集成（向量库）

源文件：`internal/pkg/qdrantx/client.go`

### 7.1 设计哲学

```go
// Package qdrantx is a thin HTTP wrapper around qdrant's REST API.
// We don't pull in the full upstream go-client because it's gRPC-first
// and we only need 4 ops: ensure-collection, upsert, delete-by-filter,
// and search.
```

**手写 HTTP client，不引上游 gRPC SDK**：仅需 4 个操作，HTTP REST 足够。

### 7.2 客户端

```go
type Client struct {
    base string
    hc   *http.Client  // 30s timeout
    log  *slog.Logger
}

func (c *Client) EnsureCollection(ctx, name, dim) error
func (c *Client) EnsurePayloadIndex(ctx, collection, field, schema) error
func (c *Client) Upsert(ctx, collection, points) error
func (c *Client) DeleteByFilter(ctx, collection, mustMatch) error
func (c *Client) DeleteByID(ctx, collection, id) error
func (c *Client) GetPoints(ctx, collection, ids) ([]SearchHit, error)
func (c *Client) Search(ctx, collection, vector, opts) ([]SearchHit, error)
func (c *Client) Scroll(ctx, collection, opts) (*ScrollResult, error)
```

### 7.3 关键设计

- **一个 collection per deployment**：默认名 `knowledge`
- **float32 + Cosine distance**
- **Point ID = `md5(payload.url) >> 64` lo bits**：stable across sync runs，re-import 覆盖而非复制
- **dim mismatch 处理**：
  - collection 存在且 dim 匹配 → no-op
  - dim 不匹配 + 有 points → **refuse + clear error**（"refusing to drop, backup first"）
  - dim 不匹配 + 无 points → drop + recreate
- **PayloadIndex**：`keyword`（默认）/ `text`（前缀匹配）/ `integer`/`float`/`bool`/`geo`；无 index 时 qdrant 全表扫描
- **`PrefixMatch` sentinel**：`match.text` 前缀/全文匹配（path 前缀过滤）
- **`[]string` → `match.any`**：any-of 匹配
- **409 on existing collection = success**：幂等
- **DeleteByFilter 拒绝空 filter**：防误删全表

### 7.4 数据流

```
用户上传文档 / builtin vault / 代码浏览
   ↓
biz/knowledge.usecase
   ↓ chunk + embed（OpenAI 兼容 或 本地 ONNX fastembed）
   ↓
qdrantx.Client.Upsert（payload: source_type/title/content/url/repo_id）
   ↓
Qdrant /collections/knowledge/points
```

**查询**：

```
AI tool query_knowledge / UI 搜索
   ↓
embed query → vector
   ↓
qdrantx.Client.Search（vector + MustMatch filter + Limit）
   ↓
返回 top-k SearchHit（score + payload）
```

### 7.5 配置

```env
ONGRID_QDRANT_URL=http://qdrant:6333
ONGRID_QDRANT_COLLECTION=knowledge
ONGRID_EMBEDDING_DIM=512  # 必须与 embedding model 一致
```

systemhealth 检查 `QdrantURL` + `QdrantCollection`。

### 7.6 dim 同步

`EnsureCollection` 的 dim 由 caller（knowledge usecase）传入，必须与 embedding model 一致：

- OpenAI text-embedding-3-small → 1536
- 本地 ONNX BGE-small-zh-v1.5 → 512

dim 不一致时 `EnsureCollection` 报错，operator 手动 drop。

---

## 8. SearXNG 集成（Web 搜索）

源文件：`internal/skill/builtin/web_search.go`

### 8.1 多 provider 架构

```go
const (
    providerSearxng = "searxng"
    providerTavily  = "tavily"
    providerBrave   = "brave"
)

const defaultSearxngURL = "http://searxng:8080"
```

**三 provider**：

| Provider | 类型 | 认证 | 特性 |
|----------|------|------|------|
| SearXNG | 自托管 | 无 key | 默认，零配置 |
| Tavily | 外部 API | api_key | 支持 `answer` 字段 + include/exclude_domains |
| Brave | 外部 API | X-Subscription-Token | 纯结果链接 |

### 8.2 WebSearchConfigResolver

```go
type WebSearchConfigResolver interface {
    Provider(ctx) string          // "searxng" | "tavily" | "brave"
    SearxngURL(ctx) string
    TavilyAPIKey(ctx) string
    BraveAPIKey(ctx) string
}
```

**layering rule**：skill 包不依赖 manager model 包，provider 名常量本地复制。

### 8.3 SearXNG 调用

```go
func (s *webSearchSkill) searchSearxng(ctx, client, resolver, p) (json.RawMessage, error) {
    base := defaultSearxngURL  // http://searxng:8080
    if u := resolver.SearxngURL(ctx); u != "" {
        base = u
    }
    // GET /search?q=...&format=json&safesearch=1,pageno=1
    // Accept: application/json
    // User-Agent: ongrid-web-search/1.0
}
```

**关键设计**：

- **GET 而非 POST**：上游 docker image 默认 `settings.yml` 只开 GET
- **Accept + User-Agent**：SearXNG 拒绝 bot-like UA
- **4 MiB body cap**
- **reachability failure → skipped_reason（非 error）**：operator 没起 searxng 时，LLM 拿到 `skipped_reason` 引导用户改设置，而非 crash agent loop

### 8.4 Tavily / Brave

- **Tavily**：POST `/search`，body 含 `api_key`、`include_answer: true`，返回 `answer` + `results[]`
- **Brave**：GET `/res/v1/web/search?q=...&count=...`，header `X-Subscription-Token`
- **未配置 key → skipped_reason**：不 error

### 8.5 统一响应

```go
type webSearchResponse struct {
    Provider      string            `json:"provider"`
    Results       []WebSearchResult `json:"results"`
    Answer        string            `json:"answer,omitempty"`         // 仅 Tavily
    SkippedReason string            `json:"skipped_reason,omitempty"` // 配置缺失/不可达
}
```

### 8.6 WebSearchProbe

源文件：`internal/manager/biz/setting/probe.go`

```go
func (p *WebSearchProbe) Probe(ctx) (provider, sample, error) {
    // 1-result query "ongrid web search probe"
    // 返回 provider + 首个 result title
}
```

8s 超时，比 Loki/Tempo probe 宽松（外部 API 可能慢）。

### 8.7 配置

```env
ONGRID_WEB_SEARCH_PROVIDER=searxng  # 默认
ONGRID_SEARXNG_URL=http://searxng:8080
ONGRID_TAVILY_API_KEY=...
ONGRID_BRAVE_API_KEY=...
```

system_settings `category=websearch`。

---

## 9. Grafana 集成（看板同步）

### 9.1 pkg/grafana —— HTTP 客户端

源文件：`internal/pkg/grafana/client.go`

```go
type Client struct {
    baseURL, token           string  // Bearer
    basicUsr, basicPwd       string  // bootstrap
    hc                       *http.Client
}

func New(baseURL, token, hc) *Client                           // SA token / API key
func NewWithBasicAuth(baseURL, user, password, hc) *Client     // bootstrap

func (c *Client) Health(ctx) error                              // /api/health
func (c *Client) UpsertDatasource(ctx, ds) error                // GET-then-PUT/POST by UID
func (c *Client) EnsureFolder(ctx, uid, title) error            // 412 = exists
func (c *Client) FindServiceAccountByName(ctx, name) (*SA, error)
func (c *Client) CreateServiceAccount(ctx, name, role) (*SA, error)
func (c *Client) CreateServiceAccountToken(ctx, saID, name) (string, error)
func (c *Client) FetchDashboard(ctx, uid) ([]byte, error)       // ErrDashboardNotFound
func (c *Client) UpsertDashboard(ctx, dashboard, folderUID, overwrite) error
```

**关键设计**：

- **Bearer 认证**：SA token 或 API key 都走 `Authorization: Bearer`
- **readOnly 检测**：file-provisioned `editable:false` datasource，GET 返回 `readOnly:true` → no-op；fallback 检测 403 "read-only data source"
- **dashboard.id = null**：UpsertDashboard 删除 id 字段，让 Grafana 当新 dashboard 处理
- **1 MiB body cap**：防 OOM
- **notFoundErr sentinel**：UpsertDatasource / EnsureFolder 用它 branch 到 create

### 9.2 biz/grafana —— 业务编排

源文件：`internal/manager/biz/grafana/service.go`

```go
const (
    folderUID      = "ongrid"
    datasourceUID  = "ongrid-prometheus"
)

//go:embed dashboards/*.json
var dashboardsFS embed.FS

type Service struct {
    settings           *settingbiz.Service
    tlsInsecure        bool
    panelDashboardUID  string  // "ongrid-monitor"
}
```

**四大功能**：

#### BootstrapEmbedded —— 首次启动自动配 SA token

```go
func (s *Service) BootstrapEmbedded(ctx, adminUser, adminPassword) {
    // Skip if: sa_token 已存在 / admin creds 空 / root_url 空
    c := pkggrafana.NewWithBasicAuth(rootURL, adminUser, adminPassword, ...)
    c.Health(ctx)
    sa := c.FindServiceAccountByName("ongrid") || c.CreateServiceAccount("ongrid", "Admin")
    token := c.CreateServiceAccountToken(sa.ID, "ongrid-bootstrap")
    s.settings.Set(CategoryGrafana, KeyGrafanaSAToken, token, true)
}
```

**fail-soft**：任何失败都 Warn + skip，不 crash 启动。

#### Sync —— 全量同步 datasource + dashboards

```go
func (s *Service) Sync(ctx) (*SyncResult, error) {
    promURL := s.settings.Get(CategoryProm, KeyPromQueryURL)
    c.EnsureFolder(folderUID, folderTitle)
    
    // 把 ongrid 的 prom 凭据透传到 Grafana datasource 的 secureJsonData
    ds := Datasource{UID: "ongrid-prometheus", URL: promURL, ...}
    if bearer != "" {
        ds.JSONData["httpHeaderName1"] = "Authorization"
        ds.SecureJSONData["httpHeaderValue1"] = "Bearer " + bearer
    } else if basicUser != "" {
        ds.BasicAuth = true
        ds.SecureJSONData["basicAuthPassword"] = basicPass
    }
    c.UpsertDatasource(ds)
    
    s.pushDashboards(ctx, c)  // embed.FS dashboards/*.json
}
```

#### SyncMonitorPanels —— 镜像 Monitor 页面到 Grafana

```go
func (s *Service) SyncMonitorPanels(ctx, panels) error {
    all := append(coreMonitorPanels(), panels...)  // 硬编码核心 panel + 用户 panel
    dash := buildMonitorDashboardJSON("ongrid-monitor", "ongrid Monitor (managed)", all)
    c.UpsertDashboard(dash, folderUID, true)
}
```

**coreMonitorPanels**：硬编码 10 个核心 panel（CPU/内存/磁盘/网络/Top8进程CPU/Top8进程内存/负载/磁盘IO/conntrack/TCP），ID 9001-9010 避免与 DB auto-increment 冲突，**KEEP IN LOCKSTEP WITH Monitor.tsx**。

#### FetchDashboardJSON —— SPA 代理查询

```go
func (s *Service) FetchDashboardJSON(ctx, uid) ([]byte, error) {
    // SPA 的 PromQLPanel 渲染器通过 manager 代理查 Grafana
    // 因为只有 manager 有 Grafana 凭据
}
```

### 9.3 配置

```env
ONGRID_GRAFANA_ROOT_URL=http://grafana:3000
ONGRID_GRAFANA_SA_TOKEN=...  # Bootstrap 自动填
ONGRID_GRAFANA_API_KEY=...   # 外部 Grafana fallback
ONGRID_GRAFANA_TLS_INSECURE=false
ONGRID_GRAFANA_PANEL_DASHBOARD_UID=ongrid-monitor
ONGRID_GRAFANA_ADMIN_USER=admin  # bootstrap 用
ONGRID_GRAFANA_ADMIN_PASSWORD=...
```

system_settings `category=grafana`。

### 9.4 embedded dashboards

`internal/manager/biz/grafana/dashboards/cluster-overview.json` + `server-detail.json`，`//go:embed` 进二进制，随版本发布。

---

## 10. Prometheus 代理票据（nginx auth_request）

源文件：`internal/manager/service/prometheus/service.go`

### 10.1 背景

OnGrid 把 Prometheus 暴露给用户浏览器（`/prometheus/*`），但要用 ongrid 的 JWT 鉴权而非 Prom 自带的基础认证。用 nginx `auth_request` 模块实现。

### 10.2 票据机制

```go
const promTicketTTL = 30 * time.Minute

func (s *Service) BuildLaunch(caller, in) (path, ticket, ttl, error) {
    ticket := s.signer.SignWithTTL(Claims{
        UserID: caller.UserID,
        Role:   caller.Role,
        Subject: "prometheus-proxy",
    }, promTicketTTL)
    return "/prometheus/graph?" + q.Encode(), ticket, promTicketTTL, nil
}

func (s *Service) RefreshTicket(token) (newToken, ttl, ok) {
    // 滑动续期：每次成功 auth_request 重签 cookie
    claims := s.signer.Verify(token)
    fresh := s.signer.SignWithTTL(claims, promTicketTTL)
    return fresh, promTicketTTL, true
}

func (s *Service) VerifyTicket(token) error {
    claims := s.signer.Verify(token)
    if claims.Subject != "prometheus-proxy" {
        return ErrUnauthorized
    }
}
```

### 10.3 流程

```
浏览器 → manager BuildLaunch → 拿 ticket cookie + /prometheus/graph?g0.expr=...
浏览器 → nginx /prometheus/*
   ↓
nginx auth_request → manager VerifyTicket
   ↓ 200 OK
nginx proxy_pass → Prometheus
   ↓
同时 manager RefreshTicket → Set-Cookie 新 ticket（滑动续期）
```

**30min TTL**：覆盖读 dashboard / drill trace；之前 2min 用户读一半被 401。

---

## 11. systemhealth 聚合探测

源文件：`internal/manager/service/systemhealth/service.go`

### 11.1 设计

聚合所有外部系统的健康检查，供 UI Health 页面展示。

```go
type Dependencies struct {
    DB        DBPinger           // MySQL
    Prom      PromQuerier        // Prometheus
    Grafana   GrafanaTester      // Grafana
    Loki      URLProbe           // Loki
    Tempo     URLProbe           // Tempo
    Rules     RuleLister         // 告警规则
    Incidents IncidentCounter    // 事件
    Edges     EdgeLister         // edge 列表
    LLM       LLMProviderResolver // LLM provider
    HTTP      *http.Client
}

type Config struct {
    Version, FrontierAddr, QdrantURL, QdrantCollection string
    PromEnabled, LogsEnabled, TracesEnabled, AlertEnabled bool
    FrontierDisabled, LLMConfigured, EmbeddingConfigured bool
    ProbeTimeout, EvaluatorInterval, NotifyCooldown time.Duration
}
```

### 11.2 Check 项

| Check ID | Group | 检查内容 |
|----------|-------|----------|
| db | storage | MySQL PingContext |
| prom | observability | PromQuerier up |
| loki | observability | LokiURLProbe /ready |
| tempo | observability | TempoURLProbe /ready |
| grafana | visualization | GrafanaTester.Test |
| frontier | rpc | FrontierAddr 可达 / Disabled |
| qdrant | knowledge | QdrantURL + Collection 可达 |
| llm | ai | LLMConfigured + provider resolve |
| embedding | ai | EmbeddingConfigured |
| edges | fleet | EdgeLister count |
| rules | alert | RuleLister count |
| incidents | alert | IncidentCounter recent |

### 11.3 状态

```go
StatusOK       // 正常
StatusDegraded // 部分功能受影响
StatusFailed   // 完全不可用
StatusUnknown  // 未配置 / 未启用
```

---

## 12. 配置热更新与 Resolver 模式

### 12.1 Resolver 模式

所有外部系统客户端都采用 **Resolver 接口 + TTL 缓存**模式，admin UI 编辑 system_settings 后无需重启：

```go
type XxxResolver interface {
    Resolve(ctx) (cfg, error)
}
```

| Resolver | TTL | 配置 category | 用途 |
|----------|-----|---------------|------|
| `promauth.Resolver` | 5s | prom | Prom 写/读 auth header |
| `promquery.BaseURLResolver` | 5s | prom.query_url | Prom 查询 URL |
| `promwrite.EndpointResolver` | 5s | prom.write_url | Prom remote_write URL |
| `logquery.BaseURLResolver` | 5s | loki.url | Loki 查询 URL |
| `tracequery.BaseURLResolver` | 5s | tempo.url | Tempo 查询 URL |
| `LokiResolver` | 实时 | loki | Loki 全配置（URL + auth + TLS） |
| `TempoResolver` | 实时 | tempo | Tempo 全配置 |
| `WebSearchConfigResolver` | 实时 | websearch | Web 搜索 provider + keys |
| `LLM Resolver`（client.go） | 60s | llm | LLM 凭据 |
| `LLM Resolver`（MultiClient） | 60s | llm | LLM provider catalog |

### 12.2 biz/setting.Service 缓存

`biz/setting/service.go` 提供 `Get(category, key)` 接口，内部带 ~5s TTL 缓存，所有 Resolver 依赖它。

### 12.3 失败回退

- **Resolver 失败 → fallback**：`LokiResolver.URL` DB row 空 → fallback 到 env-seeded `fallbackURL`
- **promauth fail closed**：Resolver 错误返回 error，不静默无 auth 请求
- **promauth fail closed 假设**："no auth = fail closed because silent auth-less request looks identical to network glitch"

---

## 13. cmd/ongrid/main.go 装配流程

### 13.1 装配顺序

```go
// 1. DB
gdb := dbx.Open(cfg.DB, log)
defer gdb.Close()

// 2. LLM（见 ongrid_LLM.md）
llmClient := llm.NewWithResolver(...)
multiClient := llm.NewMultiClient(...)

// 3. Frontier
var fbClient *frontierbound.Client
if cfg.FrontierClient.Disabled {
    fbClient = frontierbound.NewDisabled(log)
} else {
    fbClient = frontierbound.New(cfg.FrontierClient, log)
}
defer fbClient.Close()

// 4. Prom auth + clients
promAuthClient := promauth.BuildClient(promTLS, promResolver, 30s)
promQueryClient := promquery.NewWithResolverAndHTTPClient(promURLResolver, promAuthClient, log)
promWriteClient := promwrite.NewWithResolverAndHTTPClient(promWriteResolver, promAuthClient, log)

// 5. Loki / Tempo
lokiClient := logquery.NewWithResolverAndHTTPClient(lokiResolver, lokiAuthClient, log)
tempoClient := tracequery.NewWithResolverAndHTTPClient(tempoResolver, tempoAuthClient, log)

// 6. Qdrant + embedding
qdrantClient := qdrantx.New(cfg.Qdrant.URL, log)
embedder := embedding.NewWithResolver(...)
knowledgeUC := knowledge.New(qdrantClient, embedder, gdb, log)

// 7. Grafana
grafanaSvc := grafana.New(settingSvc, cfg.Grafana.TLSInsecure, log)
grafanaSvc.BootstrapEmbedded(ctx, cfg.Grafana.AdminUser, cfg.Grafana.AdminPassword)

// 8. Web search
builtin.SetWebSearchConfigResolver(webSearchResolver)

// 9. systemhealth
systemhealthSvc := systemhealth.New(systemhealth.Config{
    FrontierAddr: cfg.FrontierClient.Addr,
    FrontierDisabled: cfg.FrontierClient.Disabled,
    QdrantURL: cfg.Qdrant.URL,
    QdrantCollection: cfg.Qdrant.Collection,
    LLMConfigured: llmConfigured,
    EmbeddingConfigured: embeddingConfigured,
    ...
}, systemhealth.Dependencies{
    DB: gdb.DB(),
    Prom: promQueryClient,
    Grafana: grafanaSvc,
    Loki: lokiURLProbe,
    Tempo: tempoURLProbe,
    LLM: llmResolver,
    ...
})

// 10. frontierbound.Install 反向 RPC handler
frontierbound.Install(ctx, fbClient, frontierbound.Wiring{
    EdgeAuthn: edgeAuthn,
    EdgeUC: edgeUC,
    MetricIngester: metricIngestSvc,
    PromIngester: promIngester,
    ...
})

// 11. Prometheus proxy ticket signer
promSvc := prometheus.New(authSigner)
```

### 13.2 ONGRID_FRONTIER_DISABLED 路径

```go
// ONGRID_FRONTIER_DISABLED=true bypasses the dial entirely — the
// resulting Client errors all Call/OpenStream/NotifyX with
// frontierbound.ErrDisabled and is a no-op for Register. Used by the
// e2e harness so manager can come up without a real broker.
```

manager 无 frontier 可启动；edge-tunnel 相关功能在调用点返回 `ErrDisabled`。

---

## 14. 并发与资源管理

### 14.1 HTTP 客户端复用

所有外部系统客户端 **safe for concurrent use**：

| 包 | 并发安全机制 |
|----|--------------|
| dbx | GORM 内部连接池 |
| promwrite | 无状态，httpClient 共享 |
| promquery | 无状态，httpClient 共享 |
| logquery | 无状态，httpClient 共享 |
| tracequery | 无状态，httpClient 共享 |
| qdrantx | 无状态，httpClient 共享 |
| grafana | 无状态，httpClient 共享 |
| promauth | `authRoundTripper` mu.Lock 保护 cached/cachedAt |
| frontierbound | RWMutex 保护 binding map |
| tunnel | 多个锁（handlersMu / reconnectMu / connMu） |

### 14.2 超时

| 客户端 | defaultTimeout | 理由 |
|--------|----------------|------|
| promwrite | 10s | 批次小 |
| promquery | 30s | range query 慢 |
| logquery | 30s | 宽窗口慢 |
| tracequery | 30s | 冷 block search 慢 |
| qdrantx | 30s | 搜索可能慢 |
| grafana | 15s | API 一般快 |
| probe（Loki/Tempo） | 5s | 紧凑，operator 期望快速反馈 |
| probe（WebSearch） | 8s | 外部 API 可能慢 |

### 14.3 body cap

| 客户端 | cap | 理由 |
|--------|-----|------|
| promquery | 8 MiB | range 结果 |
| logquery | 8 MiB | query_range 结果 |
| tracequery | 16 MiB | 单 trace 可能大 |
| qdrantx | 无显式 cap（http.Client 默认） | |
| grafana | 1 MiB | API 响应 |
| web_search | 4 MiB | 搜索结果 |

### 14.4 TLS

- **promauth.BuildClient**：统一 TLS 构造（Insecure / CAPath / CAPEM）
- **LokiResolver.TLSInsecure / TempoResolver.TLSInsecure**：per-system TLS skip
- **grafana.Service.httpClient()**：tlsInsecure 时返 custom http.Client
- **MinVersion TLS1.2**：所有手写 tls.Config 强制

### 14.5 ctx 透传

所有外部调用第一个参数是 `context.Context`，遵守架构红线。

---

## 15. 设计模式与架构红线

### 15.1 设计模式

| 模式 | 应用 |
|------|------|
| Resolver + TTL 缓存 | 所有外部系统配置热更新（promauth/promquery/promwrite/logquery/tracequery/LLM） |
| Backend-decoupled 命名 | `logquery`/`tracequery` 而非 `lokiquery`/`tempoquery`，换后端时包名不变 |
| Thin HTTP wrapper | qdrantx（不引 gRPC SDK）、grafana（手写 HTTP） |
| Fail-fast | dbx openMySQL Ping at open time |
| Fail-soft | grafana.BootstrapEmbedded 任何失败 Warn + skip |
| Fail-closed | promauth resolver 错误返回 error 不静默 |
| Null Object | frontierbound.NewDisabled（svc=nil） |
| Bootstrap | grafana.BootstrapEmbedded 自动创 SA + token |
| Embed | grafana dashboards / knowledge builtin_vault |
| Skipped-reason envelope | web_search 配置缺失/不可达返回 `skipped_reason` 而非 error |
| Proxy + auth_request | Prometheus 票据代理 |
| Three-way validation | frontierbound unbindEdgeTransport 防 stale offline |
| Sliding session | Prom ticket 30min + RefreshTicket 续期 |

### 15.2 架构红线

1. **接口在消费方定义** —— `service`/`URLProbe`/`GrafanaTester`/`PromQuerier`/`DBPinger`/`LLMProviderResolver` 等接口在消费方包定义
2. **ctx 透传** —— 所有外部 IO 函数第一参数 `context.Context`
3. **错误 `%w` 包装** —— 所有外部错误包装含上下文（method/edgeID/transportID/status）
4. **DSN 密码脱敏** —— `redactDSN` 日志中脱敏
5. **永不记录敏感字段** —— Prom label 禁 user_id/org_id/session_id；LLM 永不记录 content；SSHPass one-shot wiped
6. **body cap** —— 所有 HTTP 响应 `io.LimitReader` 防 OOM
7. **MinVersion TLS1.2** —— 所有手写 tls.Config 强制
8. **fail-closed auth** —— promauth resolver 错误不静默无 auth 请求
9. **backend-decoupled 包名** —— `logquery`/`tracequery` 换后端时不变
10. **dim 同步** —— Qdrant collection dim 必须与 embedding model 一致，不一致 refuse
11. **readOnly 尊重** —— Grafana file-provisioned datasource readOnly 时不强改
12. **dim mismatch refuse** —— Qdrant 有 points 时 dim 不匹配 refuse，不自动 drop
13. **DeleteByFilter 拒绝空 filter** —— 防误删全表
14. **coreMonitorPanels LOCKSTEP** —— biz/grafana 与 web/Monitor.tsx 必须同步
15. **Prom ticket Subject 校验** —— `prometheus-proxy` subject 防止其他 JWT 滥用

### 15.3 关键决策

| 决策 | 理由 |
|------|------|
| MySQL 默认 + SQLite opt-in | 生产用 MySQL，本地调试用 SQLite（WAL + FK） |
| Frontier 独立容器 | edge 在 NAT 后，frontier 作公网 broker 中转 geminio |
| Prom 双向集成 | remote_write 收 edge 指标，PromQL 供 AI/告警/UI |
| 手写 promwrite protobuf | 不引 prometheus/proto 依赖 |
| qdrantx 手写 HTTP | 仅需 4 op，gRPC SDK 过重 |
| SearXNG 默认 | 自托管零配置，Tavily/Brave 作 opt-in |
| Grafana BootstrapEmbedded | 首次启动自动配 SA token，operator 无需登录 Grafana |
| nginx auth_request 代理 Prom | 用 ongrid JWT 鉴权而非 Prom 基础认证 |
| 30min Prom ticket | 覆盖读 dashboard，2min 太短 |
| WebSearch skipped_reason 非 error | LLM 拿到提示引导用户改设置，而非 crash agent |
| dim mismatch refuse | 防自动 drop 丢数据 |
| logquery/tracequery 命名 | 换 VictoriaLogs / Jaeger 时包名不变 |

---

## 16. 关键文件索引

### 16.1 MySQL

| 文件 | 职责 |
|------|------|
| `internal/pkg/dbx/dbx.go` | Open（MySQL/SQLite）+ Ping + redactDSN |

### 16.2 Frontier

| 文件 | 职责 |
|------|------|
| `internal/pkg/tunnel/client.go` | edge 侧 geminio client |
| `internal/pkg/tunnel/types.go` | Client/Handler/StreamConn 接口 |
| `internal/pkg/tunnel/messages.go` | wire 协议常量与结构 |
| `internal/manager/service/frontierbound/client.go` | manager 侧 service-end + binding map |
| `internal/manager/service/frontierbound/handlers.go` | Install 反向 RPC handler |
| `api/tunnel/v1/tunnel.proto` | wire 协议规范文档 |
| `deploy/install/frontier.yaml` | frontier broker 部署配置 |

### 16.3 Prometheus

| 文件 | 职责 |
|------|------|
| `internal/pkg/promwrite/client.go` | remote_write 客户端 |
| `internal/pkg/promwrite/proto.go` | 手写 protobuf 编码 |
| `internal/pkg/promquery/client.go` | PromQL 查询客户端 |
| `internal/pkg/promauth/client.go` | TLS + auth http.Client 构建 |
| `internal/manager/biz/promwrite/ingester.go` | edge push 转发到 Prom |
| `internal/manager/biz/setting/promauth.go` | Prom auth resolver |
| `internal/manager/service/prometheus/service.go` | Prom 代理票据 signer |
| `internal/manager/server/prometheus/http.go` | Prom 代理 HTTP handler |

### 16.4 Loki

| 文件 | 职责 |
|------|------|
| `internal/pkg/logquery/client.go` | LogQL 查询客户端 |
| `internal/manager/biz/setting/telemetry.go` | LokiResolver + TempoResolver |
| `internal/manager/biz/setting/probe.go` | LokiURLProbe |
| `internal/manager/server/logs/http.go` | Logs UI 代理 |

### 16.5 Tempo

| 文件 | 职责 |
|------|------|
| `internal/pkg/tracequery/client.go` | TraceQL 查询客户端 |
| `internal/manager/biz/setting/probe.go` | TempoURLProbe |
| `internal/manager/server/traces/http.go` | Traces UI 代理 |

### 16.6 Qdrant

| 文件 | 职责 |
|------|------|
| `internal/pkg/qdrantx/client.go` | HTTP REST 客户端 |
| `internal/manager/biz/knowledge/usecase.go` | 知识库 RAG 主逻辑 |
| `internal/manager/data/knowledge/store/repo.go` | MySQL metadata 存储 |

### 16.7 SearXNG / Web Search

| 文件 | 职责 |
|------|------|
| `internal/skill/builtin/web_search.go` | 三 provider 调度（SearXNG/Tavily/Brave） |
| `internal/manager/biz/setting/websearch.go` | WebSearchResolver |
| `internal/manager/biz/setting/probe.go` | WebSearchProbe |
| `deploy/install/searxng/settings.yml` | SearXNG 配置 |

### 16.8 Grafana

| 文件 | 职责 |
|------|------|
| `internal/pkg/grafana/client.go` | HTTP admin API 客户端 |
| `internal/manager/biz/grafana/service.go` | 业务编排（Bootstrap/Sync/SyncMonitorPanels） |
| `internal/manager/biz/grafana/dashboards/*.json` | embedded dashboards |
| `deploy/install/grafana/provisioning/` | file-provisioned datasource/dashboard |

### 16.9 systemhealth

| 文件 | 职责 |
|------|------|
| `internal/manager/service/systemhealth/service.go` | 聚合健康检查 |
| `internal/manager/server/systemhealth/http.go` | Health UI API |

### 16.10 配置与装配

| 文件 | 职责 |
|------|------|
| `internal/pkg/config/config.go` | 全配置结构（FrontierClient/Prom/Loki/Tempo/Qdrant/Grafana/WebSearch） |
| `cmd/ongrid/main.go` | 装配所有外部系统客户端 |
| `deploy/install/docker-compose.yml` | 生产 compose（MySQL/Frontier/Prom/Loki/Tempo/Qdrant/SearXNG/Grafana） |
| `deploy/install/.env.example` | 环境变量模板 |
| `deploy/install/install.sh` | 安装脚本 |

---

> 本文档基于 OnGrid 源码中与 MySQL、Frontier、Prometheus、Loki、Qdrant、SearXNG、Tempo、Grafana 八个外部系统集成相关的全部代码编写。如需深入某个集成，参考 §16 文件索引定位源文件。Frontier/geminio 的详细 RPC 实现见 `ongrid_rpc_singchia_geminio.md`，LLM 集成见 `ongrid_LLM.md`。
