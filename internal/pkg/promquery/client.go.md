# `client.go` 技术实现文档（promquery）

> 源文件：`internal/pkg/promquery/client.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/promquery`

## 1. 概述

该文件实现 Prometheus HTTP 查询 API 的轻量客户端，封装 `/api/v1/query`（instant）与 `/api/v1/query_range`（range）。被 manager AI 工具注册表消费——响应以 raw JSON 透传回 LLM，保留 matrix / vector / scalar 形状不丢失。结构与 `logquery` / `tracequery` 镜像，支持静态 / 动态 base URL 解析。

## 2. 包信息

- **包名**：`promquery`
- **所属模块**：`internal/pkg/`（基础设施层）
- 依赖方向：被 manager AI 工具注册表（query_promql）调用；仅依赖标准库。包级文档明确跨 BC 边界——位于 `internal/pkg/` 且无 `manager/*` 导入。

## 3. 关键类型与接口

### `InstantResult`
Prom 查询响应的 `data` 字段反序列化结果。

```go
type InstantResult struct {
    ResultType string          `json:"resultType"`
    Result     json.RawMessage `json:"result"`
}
```

`Result` 保留 raw JSON，AI 工具可直接透传 LLM 不丢失形状。

### `BaseURLResolver`
动态 base URL 解析接口。

```go
type BaseURLResolver interface {
    ResolveBaseURL(ctx context.Context) (string, error)
}
```

wiring 层（cmd/ongrid + biz/setting）带 ~5s TTL 缓存，admin UI 修改无需重启但不会每 PromQL 调用都打 DB。

### `Client`
查询客户端，并发安全。

```go
type Client struct {
    base       BaseURLResolver
    httpClient *http.Client
    log        *slog.Logger
}
```

### `promResponse`（私有）
Prom 响应信封：`{status, data, errorType, error}`。

### `staticBase`
静态 base URL 实现。

## 4. 关键函数与流程

### `New` / `NewWithHTTPClient` / `NewWithResolverAndHTTPClient`
- **签名**：三个构造函数，分别支持静态 URL + 默认 client、静态 URL + 自定义 client、动态 resolver + 自定义 client。
- **流程**：均委托 `newClient`；nil hc 用默认 30s client；nil log 用 `slog.Default()`；静态 URL trim trailing `/`。

### `Query`
- **签名**：`func (c *Client) Query(ctx context.Context, expr string, ts time.Time) (*InstantResult, error)`
- **职责**：执行 instant 查询 `/api/v1/query`。
- **流程**：
  1. `url.Values`：`query=expr`；`ts` 非零时设 `time`（UnixNano / 1e9 浮点字符串）。
  2. `c.do(ctx, "/api/v1/query", q)`。

### `QueryRange`
- **签名**：`func (c *Client) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) (*InstantResult, error)`
- **职责**：执行 range 查询 `/api/v1/query_range`。
- **流程**：
  1. 校验：`step <= 0` → error；`!end.After(start)` → error。
  2. `url.Values`：query / start / end（UnixNano / 1e9 浮点）/ step（秒浮点）。
  3. `c.do(ctx, "/api/v1/query_range", q)`。

### `do`（私有）
- **签名**：`func (c *Client) do(ctx context.Context, path string, q url.Values) (*InstantResult, error)`
- **流程**：
  1. `c.base.ResolveBaseURL(ctx)` 取当前 URL；失败 → error。
  2. `full = base + path + "?" + q.Encode()`。
  3. `http.NewRequestWithContext(ctx, GET, full, nil)`。
  4. 设置 `Accept: application/json` / `User-Agent: ongrid-promquery/0.1`。
  5. `c.httpClient.Do(req)`；`defer resp.Body.Close()`。
  6. `io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))` 8 MiB 上限。
  7. `status != 200 && status != 400` → log Warn + error 含 status + body。
     - **特殊处理**：Prom 用 400 表示查询解析错误，需解码 JSON 让调用方看到 errorType；其他非 200 才视为 transport / server 失败。
  8. `json.Unmarshal(body, &env)` 解信封；`status != "success"` → error 含 status / errorType / error。
  9. `json.Unmarshal(env.Data, &ir)` 解 data；返回 `&ir`。
- **错误处理**：每步 `%w` 包装并加 `promquery:` 前缀。

## 5. 依赖关系

- **内部包**：无（明确无 `manager/*` 导入）。
- **外部库**：标准库 `context` / `encoding/json` / `errors` / `fmt` / `io` / `log/slog` / `net/http` / `net/url` / `strconv` / `strings` / `time`。
- **被调用方**：manager AI 工具注册表（query_promql 工具）。

## 6. 并发与资源管理

无显式锁。`Client` 字段构造后不变；`http.Client` 并发安全；`BaseURLResolver` 实现自身的线程安全由调用方保证（biz/setting 通常带缓存）。

## 7. 设计模式与亮点

- **raw JSON 透传**：`InstantResult.Result` 保留 raw JSON，AI 工具直接透传 LLM 不丢失 matrix / vector / scalar 形状，避免客户端模型与 Prom 演进脱节。
- **Resolver 抽象**：`BaseURLResolver` 接口让 admin UI 修改 URL 在 TTL 窗口内生效，无需重启 manager。
- **静态 / 动态双形态**：`New*` 三构造函数覆盖静态 dev 部署与动态生产部署。
- **400 特殊处理**：Prom 用 400 表示查询解析错误，本实现识别并解码 JSON 让调用方看到 errorType；其他非 200 才视为 transport 失败。
- **8 MiB body 上限**：`LimitReader` 防 OOM。
- **跨 BC 边界明确**：包注释明确位于 `internal/pkg/` 且无 `manager/*` 导入，符合 gospec monorepo BC 隔离红线。
- **对称设计**：与 `logquery` / `tracequery` 同形态，降低维护者认知成本。
- **时间戳浮点字符串**：`UnixNano / 1e9` 浮点字符串，匹配 Prom API 期望的 Unix 秒浮点格式。

## 8. 注意事项

- **30s 默认超时**：宽窗口 / 高基数 range 查询可能超时；调用方需评估调长或缩窗口。
- **8 MiB body 上限**：超限请求无显式错误，body 被截断后 JSON 反序列化失败；调用方看到 decode error 时应怀疑超限。
- **无重试**：单次失败即返回，调用方需自实现重试。
- **400 不报警**：400 是查询解析错误（业务错误），不 log Warn；调用方需自行处理 errorType。
- **无 auth 注入**：本 client 不处理 auth header；需通过 `promauth.BuildClient` 构造带 auth 的 `*http.Client` 传入。
- **`Query` 不强制 ts**：`ts` 零值时不设 `time` query param，Prom 用当前时间；调用方应显式传 ts 避免歧义。
- **错误 body 全文入 error**：非 200 错误含完整 body，可能含敏感信息；日志记录时需注意脱敏。
- **`User-Agent` 版本号 0.1**：未跟随包版本演进；若用于 Prom 端识别需更新。
