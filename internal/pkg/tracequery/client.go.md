# `client.go` 技术实现文档

> 源文件：`internal/pkg/tracequery/client.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/tracequery`

## 1. 概述

本文件实现 Tempo HTTP 查询 API 的轻量客户端：`/api/search`（TraceQL 搜索）与 `/api/traces/<id>`（单 trace 获取），外加 `/api/search/tag/<tag>/values`（标签值列表，供 SPA 下拉）。响应体保留为 `json.RawMessage` 透传给 LLM，避免 Tempo 跨版本 schema 变化导致字段丢失。包名 `tracequery`（非 `tempoquery`）刻意解耦后端，便于未来换 VictoriaTraces 等只改 import 站点。

## 2. 包信息

- **包名**：`tracequery`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被 manager AI 工具注册表调用；仅依赖标准库 `net/http`、`encoding/json`、`net/url`、`log/slog`

## 3. 关键类型与接口

```go
type SearchResult struct {
    Traces  json.RawMessage
    Metrics json.RawMessage
}

type TraceResult struct {
    Body json.RawMessage
}

type BaseURLResolver interface {
    ResolveBaseURL(ctx context.Context) (string, error)
}

type Client struct {
    base       BaseURLResolver
    httpClient *http.Client
    log        *slog.Logger
}

type SearchOptions struct {
    Query       string            // TraceQL 表达式
    Tags        map[string]string // legacy key=value（Query 为空时使用）
    Limit       int               // 默认 100
    Start       time.Time
    End         time.Time
    MinDuration time.Duration
    MaxDuration time.Duration
}
```

`staticBase` 是内置的 `BaseURLResolver`，适配固定 URL。

## 4. 关键函数与流程

### `New / NewWithHTTPClient / NewWithResolverAndHTTPClient`
- **签名**：多种构造函数，汇入私有 `newClient`
- **职责**：根据静态 URL 或动态 resolver 构造 Client
- **流程**：`newClient` 对 nil logger / nil httpClient 给默认值（`slog.Default()` + 30s 超时 client）
- **错误处理**：构造期不返回错误

### `Client.SearchTraces`
- **签名**：`func (c *Client) SearchTraces(ctx, opts SearchOptions) (*SearchResult, error)`
- **职责**：执行 Tempo trace summary 搜索
- **流程**：
  1. 构造 `url.Values`：
     - `Query` 非空 → `q=<Query>`（TraceQL）
     - 否则 `Tags` 非空 → `tags=k1=v1 k2=v2`（legacy 形式）
  2. limit 默认 100（Tempo 默认 20，调高给 AI 更多素材）
  3. Start/End/MinDuration/MaxDuration 非零则设置
  4. `c.do(ctx, "/api/search", q)` 获取 body
  5. JSON unmarshal 到 `SearchResult`
- **错误处理**：unmarshal 失败时把原始 body 塞入 `sr.Traces`，让 caller 仍能检查（兼容 Tempo 版本返回 bare array 的情况）

### `Client.TagValues`
- **签名**：`func (c *Client) TagValues(ctx, tag string) ([]string, error)`
- **职责**：列某标签的已知值（如 service.name），供 SPA 下拉
- **流程**：
  1. tag 空 → 报错
  2. path = `/api/search/tag/<url.PathEscape(tag)>/values`
  3. `c.do` 获取 body
  4. unmarshal `{"tagValues":[...]}` → 返回切片
- **错误处理**：tag 空报错；decode 失败 `%w` 包装

### `Client.GetTrace`
- **签名**：`func (c *Client) GetTrace(ctx, traceID string) (*TraceResult, error)`
- **职责**：按 ID 获取单 trace（OTLP-shaped JSON）
- **流程**：
  1. traceID 空 → 报错
  2. path = `/api/traces/<url.PathEscape(traceID)>`（接受 hex 或 0x 前缀，不校验格式让 Tempo 4xx 原样返回）
  3. `c.do` 获取 body → 包成 `TraceResult{Body: body}`
- **错误处理**：traceID 空报错；其他交给 `do`

### `Client.do`
- **签名**：`func (c *Client) do(ctx, path string, q url.Values) ([]byte, error)`
- **职责**：私有 HTTP 执行层
- **流程**：
  1. `c.base.ResolveBaseURL(ctx)` 解析 base URL；失败 `%w` 包装
  2. 拼 `full = base + path + "?" + q.Encode()`（q 非空时）
  3. `http.NewRequestWithContext` GET，设置 `Accept: application/json`、`User-Agent: ongrid-tracequery/0.1`
  4. `c.httpClient.Do`；defer close body
  5. `io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024))` — 16 MiB 上限（单 trace 可能很大）
  6. 404 → `"tracequery: %s: not found"`
  7. 非 200 → Warn 日志 + `%w` 包装（body 截断到 512 字节）
- **错误处理**：resolve 失败、build request 失败、HTTP 失败、非 2xx 均返回 error

### `truncate`
- **签名**：`func truncate(s string, n int) string`
- **职责**：错误体截断到 n 字节，加 `...` 后缀

## 5. 依赖关系

- **内部包**：无
- **外部库**：无
- **被调用方**：manager AI 工具注册表（trace 查询工具）

## 6. 并发与资源管理

- **`Client` 字段只读**，安全用于并发调用
- **`http.Client` 共享连接池**，可被多 goroutine 使用
- **`defaultTimeout = 30s`**：单次 round-trip 上限，Tempo 冷块搜索可能慢
- **body 16 MiB 上限**：防止单 trace 响应 OOM
- **`context.Context` 透传**到 `http.NewRequestWithContext`

## 7. 设计模式与亮点

- **包名解耦后端**：`tracequery` 而非 `tempoquery`，换后端只改 import 站点（注释提到 logquery 同款约定）
- **`BaseURLResolver` 动态配置**：每次调用询问 base URL，让 admin UI 修改无需重启（与 promquery 同款 wiring layer）
- **Raw JSON 透传**：`SearchResult.Traces` / `TraceResult.Body` 用 `json.RawMessage`，避免 Tempo schema 跨版本字段丢失，LLM 直接拿到原始 JSON
- **unmarshal 容错**：SearchTraces 解码失败时把 body 塞入 `Traces`，让 caller 仍能检查（兼容 Tempo 返回 bare array 的版本）
- **错误体有界**：`do` 中非 2xx 截断到 512 字节，防止多 MB 错误页膨胀 chat context 与日志
- **不校验 traceID 格式**：让 Tempo 4xx 原样返回，caller 拿到准确错误信息

## 8. 注意事项

- **16 MiB body 上限**：极大 trace 可能被截断；若 LLM 需要完整 trace 需评估调高
- **`SearchTraces` 优先 Query**：Query 与 Tags 同时设时 Query 胜出，Tags 被忽略；调用方需明确意图
- **`TagValues` 仅 v1 端点**：注释明示 v2 端点 payload 更丰富但 v1 足够 UI 需求；若未来需要 v2 的额外字段需扩展
- **30s 超时**：Tempo 冷块搜索可能超过 30s；caller 可注入更长超时的 `http.Client`
- **`tags` 形式拼接**：`parts` 未排序，Tempo API 不要求但测试需注意
- **无重试**：失败留给 caller 处理
