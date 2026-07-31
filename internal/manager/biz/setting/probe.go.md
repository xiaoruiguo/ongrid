# probe.go 技术实现文档

## 1. 概述

`probe.go` 实现了设置项下的"测试连接"探针集合。它针对 Loki / Tempo / WebSearch 三类外部集成提供有限的连通性检查与样本查询，供管理后台 Integrations 卡片的"测试连接"按钮直接调用。探针刻意重新发起上游 HTTP 调用而非通过 skill registry —— 因为 registry 路径会附加 agent-loop audit pipeline 与 JSON envelope，而这些在管理员探针场景下是不需要的。provider URL / key 读取自对应的 Resolver，与 skill 实际使用的相同，因此探针通过即可保证 skill 本身可用。

## 2. 包信息

- 包名：`setting`
- 路径：`internal/manager/biz/setting/probe.go`
- 导入依赖：
  - 标准库：`context` / `crypto/tls` / `encoding/json` / `fmt` / `io` / `net/http` / `net/url` / `strings` / `time`
  - 内部包：`github.com/ongridio/ongrid/internal/manager/model/setting`

## 3. 关键类型与接口

### `LokiURLProbe`

```go
type LokiURLProbe struct {
    resolver *LokiResolver
    timeout  time.Duration
}
```

Loki 的 `/ready` 探针。5 秒硬超时——管理员期望快速成功或清晰的"超时"提示，慢网络下没必要等待，因为真正的 ingest 路径有自己的重试预算。

### `TempoURLProbe`

Tempo 侧的对应实现。Tempo 同样在 compactor/ingester 重放完成后返回 200 的 `/ready`。管理员填入的 URL 可能是 OTLP push URL（以 `/v1/traces` 结尾），构造时通过 `strings.TrimSuffix(u, "/v1/traces")` 将其剥离，使 `/ready` 落在 API root。

### `WebSearchProbe`

```go
type WebSearchProbe struct {
    resolver *WebSearchResolver
    timeout  time.Duration
}
```

manager-scoped `web_search` skill 的集成侧探针。运行一个仅返回 1 条结果的查询，返回 `(provider, sample-title, error)`，让 SPA 测试连接按钮能展示可感知的确认信息。

## 4. 关键函数与流程

### `LokiURLProbe.Probe` / `TempoURLProbe.Probe`

两者均委托给共享 helper `probeReadyEndpoint`：

1. 从 resolver 取 `URL` / `Auth` / `TLSInsecure`
2. 拼接 `/ready`
3. `GET` 检查 HTTP 2xx
4. 失败时读取最多 200 字节 body，生成 `status %d: <body>` 错误

### `WebSearchProbe.Probe`

根据 `resolver.Provider(ctx)` 分发：

- `ProviderSearxng` → `probeSearxng`
- `ProviderTavily` → `probeTavily`
- `ProviderBrave` → `probeBrave`

未知 provider 防御性回落到 searxng（resolver 已规范化此值）。

### `probeSearxng`

构造 `q=ongrid web search probe&format=json&safesearch=1&pageno=1`，向 `<base>/search` 发 GET。响应体最多读 1MB（`io.LimitReader`），解析 `results[].title`。

### `probeTavily`

需 API key，否则返回 `"tavily api key not configured"`。POST `https://api.tavily.com/search`，body 含 `api_key` / `query` / `max_results=1`。

### `probeBrave`

需 API key。GET `https://api.search.brave.com/res/v1/web/search?q=...&count=1`，鉴权头 `X-Subscription-Token: <key>`。

### `probeReadyEndpoint`（共享 helper）

```go
func probeReadyEndpoint(ctx context.Context, fullURL, user, pass string, insecure bool, timeout time.Duration) error
```

- `WithTimeout` 包裹 context
- 当 `user != ""` 时 `req.SetBasicAuth(user, pass)`
- `InsecureSkipVerify` 由 operator opt-in（`//nolint:gosec` 标注）
- 非 2xx 时 body 最多读 200 字节——Tempo dump 可能数 MB，绝不应推到管理员界面

### `newHTTPClient`

`WebSearchProbe` 三条 provider 路径共用的 HTTP client 构造器。保持 local 以免扰动 `probeReadyEndpoint` 更紧凑的 shape。

## 5. 依赖关系

- **Resolver 依赖**：每个 Probe 都持有对应 Resolver（`LokiResolver` / `TempoResolver` / `WebSearchResolver`），resolver 本身再依赖 `*Service` 与 `system_settings` 行
- **model 依赖**：`model.ProviderSearxng` / `ProviderTavily` / `ProviderBrave` / `DefaultSearxngURL`
- **无外部 SDK 依赖**：全部用标准库 `net/http`

## 6. 并发与资源管理

- 所有探针均无共享可变状态，构造后只读
- `context.WithTimeout` 显式 cancel（`defer cancel()`）
- HTTP 响应 body 均通过 `defer resp.Body.Close()` 关闭
- Body 读取统一通过 `io.LimitReader` 限定上限（200 字节错误体 / 1MB 成功体），防止恶意或异常上游 OOM

## 7. 设计模式与亮点

### 重新发起而非走 registry

`WebSearchProbe` 的实现注释明确：刻意重新发起 HTTP 调用而不是通过 skill registry，因为后者附加 agent-loop audit pipeline + JSON envelope，对管理员探针是噪音。provider URL / key 读取自同一 `WebSearchResolver`，因此探针通过即保证 skill 可用——这种"同源验证"避免了双重实现的漂移风险。

### 紧凑超时

5 秒（Loki/Tempo）/ 8 秒（WebSearch）。注释指出"管理员期望快速成功或清晰超时"——慢网络下真正的 ingest 路径有自己的 retry 预算，探针不应阻塞 UI。

### 错误体裁剪

`probeReadyEndpoint` 强制 body ≤ 200 字节，让 401/403 的配置错误能 verbatim 展示给管理员，同时阻止 Tempo 多 MB dump 涌入界面。

### OTLP URL 适配

`TempoURLProbe` 主动剥离 `/v1/traces` 后缀，兼容管理员将 OTLP push URL 误填入"Tempo URL"字段的情况——这是真实部署中容易出现的配置混淆。

### InsecureSkipVerify 显式标注

`//nolint:gosec // operator opt-in` 表明这是管理员显式选择，而非疏忽。

## 8. 注意事项

- **超时选择**：5s/8s 是经验值；若上游确实慢，探针失败但 skill 实际可用——管理员需理解探针是"快速 smoke test"而非"完整可用性证明"
- **SearXNG 默认 URL**：`model.DefaultSearxngURL` 是 docker-internal `http://searxng:8080`，外部部署需管理员显式覆盖
- **Tavily/Brave 无 key 时**：返回的错误是普通 `error`，由上层 handler 转 UI 提示；不要将其与"上游不可达"混淆
- **`probeReadyEndpoint` 与 `newHTTPClient` 的 client 重复**：当前是刻意的——前者用于 ready 探针，后者用于 WebSearch；保持分离以免互相扰动
- **TLS insecure**：仅当 resolver 返回 `true` 时启用，resolver 读取的 `loki.tls_insecure` / `tempo.tls_insecure` 行由 HTTP handler 默认标记为 non-sensitive
