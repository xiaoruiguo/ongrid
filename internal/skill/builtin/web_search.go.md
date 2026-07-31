# `web_search.go` 技术实现文档

> 源文件：`internal/skill/builtin/web_search.go`
> 包路径：`github.com/ongridio/ongrid/internal/skill/builtin`

## 1. 概述

`web_search.go` 实现 `web_search` skill：manager 侧的联网搜索能力，支持 SearXNG / Tavily / Brave 三个 provider。这是 ongrid AI agent 获取实时外部信息的主要入口，Scope=Manager（跑在 manager 进程内，不经 edge），Class=Safe（只读 HTTP）。通过 `WebSearchConfigResolver` 接口注入运行时配置，避免 skill 包依赖 manager model 层。

## 2. 包信息

- **包名**：`builtin`
- **所属模块**：`internal/skill/builtin`（内置 skill 实现层）
- **依赖方向**：被 `builtin` 包 `init()` 自注册；被 `cmd/ongrid` 启动时注入 resolver/httpClient；依赖 `internal/skill` 框架类型

## 3. 关键类型与接口

```go
// 包级单例，cmd/main.go 通过 SetWebSearch* 注入配置
var WebSearch = &webSearchSkill{
    tavilyEndpoint: "https://api.tavily.com/search",
    braveEndpoint:  "https://api.search.brave.com/res/v1/web/search",
}

// 配置解析接口（依赖倒置，避免 skill 依赖 manager model）
type WebSearchConfigResolver interface {
    Provider(ctx context.Context) string
    SearxngURL(ctx context.Context) string
    TavilyAPIKey(ctx context.Context) string
    BraveAPIKey(ctx context.Context) string
}

// 旧版兼容接口（仅 Tavily）
type TavilyKeyResolver interface {
    TavilyAPIKey(ctx context.Context) string
}

// skill 实现
type webSearchSkill struct {
    mu             sync.RWMutex
    tavilyEndpoint string
    braveEndpoint  string
    cfgResolver    WebSearchConfigResolver
    httpClient     *http.Client
}

// 输入参数
type webSearchParams struct {
    Query          string
    MaxResults     int
    IncludeDomains string
    ExcludeDomains string
    Provider       string
}

// 单条搜索结果（snake_case 稳定 key）
type WebSearchResult struct {
    Title         string
    URL           string
    Snippet       string
    PublishedDate string
}

// 标准化响应
type webSearchResponse struct {
    Provider      string
    Results       []WebSearchResult
    Answer        string
    SkippedReason string
}
```

Provider 常量：`providerSearxng="searxng"`、`providerTavily="tavily"`、`providerBrave="brave"`；`defaultSearxngURL="http://searxng:8080"`（compose 内部地址）。

## 4. 关键函数与流程

### `init()`
- **签名**：`func init() { skill.Register(WebSearch) }`
- **职责**：注册单例 `WebSearch`（指针，便于外部注入配置）。

### 配置注入函数
- `SetWebSearchConfigResolver(r)`：注入完整多 provider resolver；nil 禁用 resolver 路径（降级为 SearXNG 默认 URL 无 key）。
- `SetWebSearchKeyResolver(r)`（deprecated）：旧版 Tavily-only 兼容 shim，包装为 `legacyTavilyResolver`（provider 固定 tavily）。
- `SetWebSearchHTTPClient(c)`：测试缝，注入 httptest client；nil 重置为默认 30s client。
- `SetWebSearchEndpoint(u)` / `SetWebSearchTavilyEndpoint(u)` / `SetWebSearchBraveEndpoint(u)`：覆盖 endpoint URL，测试指向 httptest server。
- **并发**：所有 Set* 函数加 `WebSearch.mu.Lock()` 写锁。

### `webSearchSkill.Metadata`
- **签名**：`func (s *webSearchSkill) Metadata() skill.Metadata`
- **职责**：返回元数据。Key=`web_search`，Class=`ClassSafe`，Scope=`ScopeManager`，Category=`web`。
- **参数**：`query`（必填）、`max_results`（int 默认 5，1~10）、`include_domains`/`exclude_domains`（string，仅 Tavily）、`provider`（enum，留空走系统设置）。

### `webSearchSkill.Execute`
- **签名**：`func (s *webSearchSkill) Execute(ctx context.Context, params json.RawMessage) (json.RawMessage, error)`
- **职责**：解析参数、解析 provider、分派到具体搜索函数。
- **流程**：
  1. 读锁快照 `tavilyEndpoint`/`braveEndpoint`/`resolver`/`httpClient`；
  2. endpoint 空 → 默认值；client nil → 默认 30s client；
  3. 解码 params（空 params 跳过）；
  4. `query` 非空校验；
  5. `max_results <= 0` → 5；`> 10` → 10；
  6. **Provider 解析**：参数显式 > resolver 默认 > `"searxng"`；
  7. switch 分派：tavily → `searchTavily`；brave → `searchBrave`；searxng → `searchSearxng`；未知 → Go error。
- **错误处理**：参数缺失/未知 provider 返回 Go error；provider 未配置/不可达返回 `{skipped_reason:...}` JSON（让 LLM 引导用户修复）。

### `searchSearxng`
- **签名**：`func (s *webSearchSkill) searchSearxng(ctx, client, resolver, p) (json.RawMessage, error)`
- **职责**：调用 SearXNG `/search?q=...&format=json`。
- **流程**：
  1. base URL：resolver 提供 > `defaultSearxngURL`；
  2. 构造 query string：`q`/`format=json`/`safesearch=1`/`pageno=1`；
  3. GET 请求，设 `Accept: application/json` + 自定义 UA（SearXNG 拒 bot-like UA）；
  4. 请求失败 → `{skipped_reason: "SearXNG 不可达..."}`（提示检查 docker-compose）；
  5. 非 2xx → `{skipped_reason: "SearXNG 返回 %d..."}`；
  6. 解码 `searxngResponse`，按 `max_results` 截断，映射为 `WebSearchResult`。
- **亮点**：不可达时返回 skipped_reason 而非 error，让 LLM 引导用户启动 searxng 服务。

### `searchTavily`
- **签名**：`func (s *webSearchSkill) searchTavily(ctx, client, endpoint, resolver, p) (json.RawMessage, error)`
- **职责**：调用 Tavily `/search` POST API。
- **流程**：
  1. `apiKey` 空 → `{skipped_reason: "Tavily API key 未配置..."}`；
  2. 构造 `tavilyRequest`（含 `include_answer:true`、`search_depth:"basic"`、domain 过滤）；
  3. POST JSON，设 `Content-Type`/`Accept`；
  4. 请求失败 → Go error；
  5. 非 2xx → Go error 带 truncated body；
  6. 解码 `tavilyResponse`，映射为 `WebSearchResult`，保留 `Answer` 字段。
- **亮点**：Tavily 是唯一支持 `answer` 字段的 provider。

### `searchBrave`
- **签名**：`func (s *webSearchSkill) searchBrave(ctx, client, endpoint, resolver, p) (json.RawMessage, error)`
- **职责**：调用 Brave Search `/web/search` GET API。
- **流程**：
  1. `apiKey` 空 → `{skipped_reason: "Brave Search API key 未配置..."}`；
  2. 构造 query string：`q`/`count`；
  3. GET 请求，设 `Accept: application/json` + `X-Subscription-Token: apiKey`（Brave 的 key 在 header，非 Bearer）；
  4. 请求失败/非 2xx → Go error；
  5. 解码 `braveResponse`，`web.results[]` 映射为 `WebSearchResult`（`age` → `published_date`）。
- **亮点**：Brave 无 `answer` 字段，纯结果链接。

### 辅助函数
- `splitCSV(s)`：手工 CSV split（避免引入 `encoding/csv`），trim 空格，跳过空段。
- `truncate(s, n)`：截断字符串超长加 `...`，用于 error body 预览。
- `legacyTavilyResolver`：把旧 `TavilyKeyResolver` 适配为 `WebSearchConfigResolver`，provider 固定 tavily。

## 5. 依赖关系

- **内部包**：`github.com/ongridio/ongrid/internal/skill`
- **外部库**：`bytes`、`context`、`encoding/json`、`errors`、`fmt`、`io`、`net/http`、`net/url`、`strings`、`sync`、`time`
- **被调用方**：
  - `internal/skill/builtin` 包 `init()` 注册；
  - `cmd/ongrid` 启动时调用 `SetWebSearchConfigResolver` 注入 manager 侧 `biz/setting` 实现的 resolver；
  - `internal/manager/biz/aiops` 通过 `skill.Registry` 调度 Execute（ScopeManager，跑在 manager 进程内）。

## 6. 并发与资源管理

- **`sync.RWMutex`** 保护 `webSearchSkill` 的可变字段（endpoints/resolver/httpClient）：
  - `Set*` 函数加写锁；
  - `Execute` 开头加读锁快照配置，后续搜索逻辑无锁，避免长时间持锁。
- **`http.Client` 可注入**：默认 30s 超时；测试可注入 httptest client。
- **`io.LimitReader(resp.Body, 4*1024*1024)`**：所有 provider 响应体限制 4MiB，防恶意/异常响应 OOM。
- **`defer resp.Body.Close()`**：所有 provider 路径确保连接归还。
- `WebSearch` 是包级单例指针，多 goroutine 并发调用 `Execute` 安全（读锁快照 + 无状态搜索逻辑）。

## 7. 设计模式与亮点

- **依赖倒置（接口注入）**：`WebSearchConfigResolver` 接口让 skill 包不依赖 manager model 层，符合 monorepo 分层规则；manager 侧 `biz/setting` 实现该接口并注入。
- **Strategy 模式（多 provider）**：`Execute` 按 provider 分派到 `searchSearxng`/`searchTavily`/`searchBrave`，新增 provider 只需加一个分支。
- **skipped_reason vs error 语义分离**：
  - 未配置/不可达 → `skipped_reason` JSON（LLM 引导用户修复，agent 循环不中断）；
  - 网络错误/非 2xx → Go error（审计 + 重试逻辑触发）。
- **结果标准化**：三个 provider 的异构响应统一映射为 `WebSearchResult`，下游消费者无需感知 provider 差异。
- **Legacy 适配器**：`legacyTavilyResolver` + `SetWebSearchKeyResolver` deprecated 但保留，让旧集成平滑迁移。
- **SearXNG 默认零配置**：compose 内部 `http://searxng:8080` 默认可达，无需 API key，降低首次使用门槛。
- **测试缝完备**：`SetWebSearchHTTPClient` + `SetWebSearch*Endpoint` 让单测可用 httptest server 注入 canned 响应，无需真实网络。

## 8. 注意事项

- **`Scope=ScopeManager`**：搜索跑在 manager 进程，不经 edge；调用方不需传 `edge_id`。
- **`InsecureSkipVerify` 不在此处**：本 skill 走标准 HTTPS，`ProbeHTTP` 的跳过 TLS 验证策略不适用；Tavily/Brave 是公网 API，需有效证书。
- **`max_results` 上限 10**：受 Tavily/Brave API 限制；SearXNG 无硬上限但建议遵循统一约束。
- **`include_domains`/`exclude_domains` 仅 Tavily 支持**：SearXNG/Brave 会忽略这两个参数；UI 应根据 provider 动态显示。
- **SearXNG 不可达是常见场景**：operator 未 `docker compose up -d searxng` 时返回 skipped_reason，LLM 会引导用户启动或切换 provider；这是设计意图。
- **`WebSearch` 是包级单例指针**：`Set*` 函数修改其字段影响全局；测试间需注意状态污染（建议测试末尾 reset）。
- **`legacyTavilyResolver` 固定 provider=tavily**：旧集成迁移到 `SetWebSearchConfigResolver` 后，provider 才能动态切换。
- **`splitCSV` 手工实现**：未用 `encoding/csv`，因 CSV 解析对引号/转义的处理与简单逗号分隔场景不匹配；当前实现不支持引号包裹的含逗号域名。
- **`truncate` 按字节截断**：多字节 UTF-8 字符可能被截断在中间，产生乱码；用于 error body 预览可接受。
- **响应体 4MiB 限制**：超大会被截断，JSON 解码可能失败；当前 provider 响应均远小于此。
- **无重试**：单次 HTTP 请求，瞬时网络抖动可能导致失败；上层可自行重试。
- **`Answer` 仅 Tavily 填充**：SearXNG/Brave 无此字段，调用方应 `omitempty` 处理。
