# websearch.go 技术实现文档

## 1. 概述

`websearch.go` 定义 `WebSearchResolver`，从 `system_settings` 读取 web search provider 选择与各 provider 配置（Tavily/Brave key、SearXNG URL）。它实现 `builtin.WebSearchConfigResolver` 接口（声明于 `internal/skill/builtin/web_search.go`），让 `cmd/main.go` 能在启动时装配 skill 而无需 `internal/skill` 反向导入 `biz/setting`。结构与 `LokiResolver` / `TempoResolver` 一致——单 `Service` 依赖、nil-safe Get 路径。

## 2. 包信息

- 包名：`setting`
- 路径：`internal/manager/biz/setting/websearch.go`
- 导入依赖：
  - 标准库：`context` / `strings`
  - 内部包：`github.com/ongridio/ongrid/internal/manager/model/setting`

## 3. 关键类型与接口

### `WebSearchResolver`

```go
type WebSearchResolver struct {
    svc *Service
}
```

无 `fallbackURL` 字段——SearXNG 的默认值由 `model.DefaultSearxngURL` 常量提供（docker-internal `http://searxng:8080`），无需 env 引导。

## 4. 关键函数与流程

### `NewWebSearchResolver`

```go
func NewWebSearchResolver(svc *Service) *WebSearchResolver {
    return &WebSearchResolver{svc: svc}
}
```

仅注入 `Service`。

### `get`（私有 helper）

```go
func (r *WebSearchResolver) get(ctx context.Context, key string) string {
    if r == nil || r.svc == nil { return "" }
    v, _, err := r.svc.Get(ctx, model.CategoryWebSearch, key)
    if err != nil { return "" }
    return strings.TrimSpace(v)
}
```

与 `LokiResolver.get` 完全对称的容错策略。

### `Provider(ctx)` —— provider 选择

```go
func (r *WebSearchResolver) Provider(ctx context.Context) string
```

选择规则（优先级）：

1. **显式 `websearch.provider` 行** → 校验后使用（未知值 fall through 到第 4 步）
2. （注释提及的"Tavily key 且无其他配置 → tavily"与"Brave key 且无其他配置 → brave"在当前实现中**未编码**）
3. 当前实际逻辑：显式行匹配 `searxng` / `tavily` / `brave` 则返回；否则 fall through
4. **默认 → `searxng`**

注释说明：当 SearXNG 与 Tavily 均配置且 provider 未显式选择时，SearXNG 胜出——因为它是免费/无限的。但当前代码实现简化为"未显式选择即 searxng"，未做"根据已配置 key 推断"的逻辑分支。

### `SearxngURL(ctx)`

```go
if v := r.get(ctx, model.KeySearxngURL); v != "" {
    return strings.TrimRight(v, "/")
}
return model.DefaultSearxngURL
```

DB 行优先，回落到 `DefaultSearxngURL`。再次 `TrimRight` 防御管理员输入尾斜杠。

### `TavilyAPIKey(ctx)` / `BraveAPIKey(ctx)`

```go
return r.get(ctx, model.KeyTavilyAPIKey)
return r.get(ctx, model.KeyBraveAPIKey)
```

直接返回 trimmed key，未配置即空串。

## 5. 依赖关系

- **`*Service`**：唯一运行时依赖
- **`model`**：常量 `CategoryWebSearch` / `KeyWebSearchProvider` / `KeySearxngURL` / `KeyTavilyAPIKey` / `KeyBraveAPIKey` / `ProviderSearxng` / `ProviderTavily` / `ProviderBrave` / `DefaultSearxngURL`
- **被依赖方**：
  - `internal/skill/builtin/web_search.go` 通过 `WebSearchConfigResolver` 接口消费
  - `probe.go` 的 `WebSearchProbe` 直接持有 resolver

## 6. 并发与资源管理

- resolver 构造后完全只读
- `Service.Get` 内部 RWMutex 保护 cache
- 无 goroutine、无 IO 持有
- 所有方法 nil-safe：`if r == nil { return model.ProviderSearxng }` 或 `return ""`

## 7. 设计模式与亮点

### 接口在消费方定义

`WebSearchConfigResolver` 接口声明于 `internal/skill/builtin/web_search.go`，本包不导入该接口——只实现其方法。这是 Go 的隐式接口 + 依赖倒置：`biz/setting` 不依赖 `internal/skill`，避免循环依赖。`cmd/main.go` 在 wiring 时将 `*WebSearchResolver` 传入 skill，编译期接口匹配由 Go 自动完成。

### SearXNG 作为零配置基线

`Provider()` 默认返回 `searxng`，`SearxngURL()` 默认返回 `DefaultSearxngURL`。这让内嵌 compose stack 部署开箱即用——无需任何 UI 配置，web_search skill 即可工作。注释明确："it's the zero-config baseline that always works in the embedded compose stack."

### Provider 选择的 fail-safe

显式 `websearch.provider` 行若填入未知值（如拼错），不报错而是 fall through 到默认 `searxng`。这是"宁愿回落到免费默认也不让 skill 不可用"的设计——管理员 typo 不会导致 web_search 完全失效。

### 与 LokiResolver/TempoResolver 的结构对称

无 fallback 字段是因为 SearXNG 默认值是常量而非 env 派生。这种"按需字段"的取舍避免了"为对称而对称"的过度抽象。

## 8. 注意事项

- **注释与实现的不一致**：`Provider` 的文档注释提及"Tavily key set + nothing else → tavily"与"Brave key set + nothing else → brave"作为第 2/3 优先级，但当前实现并未编码此推断逻辑——未知显式 provider 直接 fall through 到 searxng。若依赖此 back-compat 行为的旧部署，需注意此差异
- **`get` 丢弃 `found`**：与所有 `*Resolver.get` 一致，无法区分"行存在但空"与"行不存在"。对 API key 来说无差异，但若有"显式空 key = 禁用 provider"的需求会失效
- **SearXNG 默认 URL 假设内嵌部署**：`http://searxng:8080` 是 docker-internal 地址，外部部署必须由管理员显式覆盖，否则 skill 会连不上
- **多租户**：cache key 当前是 `(category, key)`，多租户后需加 `org_id` 前缀
- **provider 字符串大小写**：`Provider()` 用 `strings.ToLower` 归一化显式行，但 `case` 分支匹配的是 `model.ProviderSearxng` 等常量——需确认这些常量本身是小写（从命名看应是）
