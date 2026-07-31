# `client.go` 技术实现文档（logquery）

> 源文件：`internal/pkg/logquery/client.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/logquery`

## 1. 概述

该文件实现 manager 侧 Loki 查询客户端，封装 `/loki/api/v1/query_range` 与 `/loki/api/v1/label/<name>/values`。结构与 `promquery` / `tracequery` 镜像——同形态、独立包，便于三种信号独立可替换。包名 `logquery` 而非 `lokiquery` 是有意为之——当 Loki 被换为 VictoriaLogs 时包名与 import path 仍有效。`X-Scope-OrgID: ongrid` header 硬编码，匹配 nginx 在数据面 ingest 路径的注入，单租户部署无需 admin 调整。

## 2. 包信息

- **包名**：`logquery`
- **所属模块**：`internal/pkg/`（基础设施层）
- **依赖方向**：被 alert evaluator（log_match / log_volume）、Logs UI 页代理、AI 工具 query_logs 调用；仅依赖标准库。

## 3. 关键类型与接口

### `QueryRangeResult`
`/query_range` 的 `data` 字段反序列化结果。

```go
type QueryRangeResult struct {
    ResultType string          `json:"resultType"`
    Result     json.RawMessage `json:"result"`
}
```

`Result` 保留 raw JSON，SPA 根据 `ResultType`（"streams" / "matrix"）自行切换解析。

### `LabelValuesResult`
`/label/<name>/values` 的 data 字段，直接暴露 slice。

### `BaseURLResolver`
动态 base URL 解析接口，每次调用 invoke 一次，让 admin 端 URL 修改无需重启 manager 即可生效。

```go
type BaseURLResolver interface {
    ResolveBaseURL(ctx context.Context) (string, error)
}
```

### `Client`
Loki 查询客户端，并发安全。

```go
type Client struct {
    base       BaseURLResolver
    httpClient *http.Client
    log        *slog.Logger
}
```

### `QueryRangeOptions`
`/query_range` 调用参数：`Query` / `Start` / `End` / `Limit`（默认 1000）/ `Step` / `Direction`（默认 backward）。

### `staticBase`
静态 base URL 实现，用于 URL 不变的场景。

## 4. 关键函数与流程

### `New` / `NewWithHTTPClient` / `NewWithResolverAndHTTPClient`
- **签名**：三个构造函数，分别支持静态 URL + 默认 client、静态 URL + 自定义 client、动态 resolver + 自定义 client。
- **流程**：均委托 `newClient`；nil hc 用默认 30s client；nil log 用 `slog.Default()`；静态 URL trim trailing `/`。

### `QueryRange`
- **签名**：`func (c *Client) QueryRange(ctx context.Context, opts QueryRangeOptions) (*QueryRangeResult, error)`
- **职责**：执行 `/loki/api/v1/query_range`。
- **流程**：
  1. 参数校验：query 空 / start / end 零 / end <= start → error。
  2. `url.Values` 拼 query：query / start（UnixNano 字符串）/ end / limit（默认 1000）/ step（>0 才设）/ direction（默认 backward）。
  3. `c.do(ctx, "/loki/api/v1/query_range", q)`。
  4. 反序列化 `{status, data, error}`；status != "success" → error。
- **设计理由**：Loki 要求 nanosecond unix 时间戳为字符串；limit 默认 1000（Loki 默认 100）让 UI 表格有足够滚动素材。

### `LabelNames`
- **签名**：`func (c *Client) LabelNames(ctx context.Context, start, end time.Time) ([]string, error)`
- **职责**：GET `/loki/api/v1/labels`，列出窗口内所有 label key。
- **使用场景**：SPA Logs 页 label selector 自动补全。

### `LabelValues`
- **签名**：`func (c *Client) LabelValues(ctx context.Context, name string, start, end time.Time) ([]string, error)`
- **职责**：GET `/loki/api/v1/label/<name>/values`，列出某 label 的已知值。
- **流程**：`name` 空 → error；`url.PathEscape(name)` 拼路径。

### `do`（私有）
- **签名**：`func (c *Client) do(ctx context.Context, path string, q url.Values) ([]byte, error)`
- **流程**：
  1. `c.base.ResolveBaseURL(ctx)` 取当前 URL；失败 → error。
  2. 拼 `full = base + path + "?" + q.Encode()`（q 非空时）。
  3. `http.NewRequestWithContext(ctx, GET, full, nil)`。
  4. 设置 `Accept: application/json` / `User-Agent: ongrid-logquery/0.1` / **`X-Scope-OrgID: ongrid`**（单租户硬编码，匹配 nginx ingest 注入）。
  5. `c.httpClient.Do(req)`；`defer resp.Body.Close()`。
  6. `io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))` 8 MiB 上限。
  7. 非 200 → log Warn + error 含 status + 截断 512 字符 body。
- **错误处理**：每步 `%w` 包装并加 `logquery:` 前缀。

### `truncate`（私有）
截断字符串，超长加 `...`。

## 5. 依赖关系

- **内部包**：无。
- **外部库**：标准库 `context` / `encoding/json` / `errors` / `fmt` / `io` / `log/slog` / `net/http` / `net/url` / `strconv` / `strings` / `time`。
- **被调用方**：alert evaluator、Logs UI 代理 endpoint、AI 工具 query_logs。

## 6. 并发与资源管理

无显式锁。`Client` 字段构造后不变；`http.Client` 并发安全，多 goroutine 并发查询由连接池承载。`BaseURLResolver` 实现自身的线程安全由调用方保证（biz/setting 通常带缓存）。

## 7. 设计模式与亮点

- **backend-decoupled 命名**：包名 `logquery` 而非 `lokiquery`，让后端切换（如 VictoriaLogs）无需改 import path。
- **Resolver 抽象**：`BaseURLResolver` 接口让 admin UI 修改 URL 在 TTL 窗口内生效，无需重启 manager。
- **静态 / 动态双形态**：`New*` 三构造函数覆盖静态 dev 部署与动态生产部署。
- **raw JSON 透传**：`QueryRangeResult.Result` 保留 raw JSON，SPA 自行按 `resultType` 解析，避免客户端模型与 Loki 演进脱节。
- **8 MiB body 上限**：`LimitReader` 防 OOM，注释提示超限应缩窗口或加 limit。
- **X-Scope-OrgID 硬编码**：单租户部署简化，匹配 nginx ingest 路径。
- **对称设计**：与 `promquery` / `tracequery` 同形态，降低维护者认知成本。

## 8. 注意事项

- **30s 默认超时**：宽窗口 / 大 limit 的 query_range 可能超时；调用方需评估调长或缩窗口。
- **8 MiB body 上限**：超限请求无显式错误，body 被截断后 JSON 反序列化失败；调用方看到 decode error 时应怀疑超限。
- **X-Scope-OrgID 硬编码**：多租户场景需改造为动态注入；当前单租户部署假设与 nginx 一致。
- **无重试**：单次失败即返回，调用方需自实现重试。
- **LabelNames / LabelValues 不校验窗口**：start / end 零值时不设 query param，由 Loki 用默认窗口；调用方应显式传窗口避免歧义。
- **`Direction` 默认 backward**：与操作员对日志搜索"最新优先"的预期一致；若需 forward 必须显式传。
- **error body 截断 512**：诊断信息可能被截断；复杂问题需直接查 Loki 日志。
