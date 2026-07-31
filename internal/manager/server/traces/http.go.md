# traces/http.go 技术实现文档

## 1. 概述

`http.go` 是 ongrid manager 链路查询代理子域（`/v1/traces/*`）的 HTTP 路由层。把内部 Tempo 查询 API 经过认证后暴露给 SPA，避免直接把 `/api/*` 透给 nginx（数据面 `/v1/traces` ingest 路由仍由 `auth_request` 卡 OTLP push）。共 3 个端点：search / getTrace / tagValues。

## 2. 包信息

- **包名**：`traces`
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/server/traces`
- **路由前缀**：`/v1/traces`（由 `cmd/ongrid/main.go` 挂载，鉴权中间件由上游注入，任何已认证用户可读）
- **文件定位**：HTTP 代理层，无业务逻辑，只做参数解析 + 调 `Querier` + 写响应

## 3. 关键类型与接口

### Querier —— 窄接口

```go
type Querier interface {
    SearchTraces(ctx context.Context, opts tracequery.SearchOptions) (*tracequery.SearchResult, error)
    GetTrace(ctx context.Context, traceID string) (*tracequery.TraceResult, error)
    TagValues(ctx context.Context, tag string) ([]string, error)
}
```

由 `*tracequery.Client` 通过结构化类型满足。

### Handler

```go
type Handler struct {
    q Querier
}
```

### 响应 DTO

```go
type searchResp struct {
    Traces  json.RawMessage `json:"traces"`
    Metrics json.RawMessage `json:"metrics,omitempty"`
    From    string          `json:"from"`
    To      string          `json:"to"`
}
```

`Traces` / `Metrics` 用 `json.RawMessage` 透传 Tempo 原始结构，避免在 handler 层重定义 Tempo 的复杂响应 schema。

## 4. 关键函数与流程

### NewHandler —— q 可为 nil

```go
func NewHandler(q Querier) *Handler
```

`q` 允许为 nil（Tempo 未启用）。所有 handler 入口 `if h.q == nil { writeErr(503, "traces backend disabled") }`，让 SPA 显示明确状态而非静默失败。

### Register —— 路由表

```go
func (h *Handler) Register(r chi.Router)
```

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/v1/traces/search` | 代理 Tempo `/api/search`（TraceQL / facet） |
| GET | `/v1/traces/tags/{tag}/values` | 代理 Tempo `/api/search/tag/<tag>/values` |
| GET | `/v1/traces/{trace_id}` | 代理 Tempo `/api/traces/<id>` |

**关键**：`{trace_id}` 路由最后注册，避免 shadow `/tags/...` 路由。

### search —— TraceQL + facet 双模式

```go
func (h *Handler) search(w http.ResponseWriter, r *http.Request)
```

流程：
1. `h.q == nil` → 503
2. `parseTime(start)` / `parseTime(end)` 必填
3. `limit`：默认 100，范围 1..1000
4. `minDuration` / `maxDuration`：可选，非负 duration
5. **facet 模式**：`q` 为空时，从 `service` / `operation` 构造 `Tags` map（`service.name` / `name`），走 Tempo legacy `tags=key=v` 形式
6. **30s context timeout**
7. 调 `h.q.SearchTraces(ctx, opts)`，返 `{traces, metrics, from, to}`

### getTrace —— OTLP JSON 透传

```go
func (h *Handler) getTrace(w http.ResponseWriter, r *http.Request)
```

`h.q == nil` → 503 → `chi.URLParam("trace_id")` 必填 → **30s context timeout** → `h.q.GetTrace(ctx, id)` → **直接 `w.Write(out.Body)` 透传 OTLP JSON**，让 SPA 直接 walk `resourceSpans` / `scopeSpans` / `spans` 无需 re-encode。

**关键**：与 search 的 `writeJSON` 不同，getTrace 直接写原始 bytes，Content-Type 手动设 `application/json`。

### tagValues

```go
func (h *Handler) tagValues(w http.ResponseWriter, r *http.Request)
```

`h.q == nil` → 503 → `chi.URLParam("tag")` 必填 → **15s context timeout**（短于 search/getTrace 的 30s）→ `h.q.TagValues(ctx, tag)` → `{values: [...]}`。

### parseTime —— 多格式时间解析

```go
func parseTime(s string) (time.Time, error)
```

与 `logs/http.go` 同模式：RFC3339 + unix 数字（自动秒/毫/纳）。

### writeJSON / writeErr

```go
func writeJSON(w http.ResponseWriter, code int, body any)
func writeErr(w http.ResponseWriter, code int, msg string)
```

`writeErr` 用 `{"error": msg}` 简化 schema（与 logs 同），不走全局 envelope。

## 5. 依赖关系

**外部**：
- `chi` —— URL 参数
- `net/http`、`encoding/json`、`strconv`、`time`、`fmt`、`errors`、`context`

**内部**：
- `internal/pkg/tracequery`（Tempo 客户端 + DTO）
- `internal/pkg/errs`

## 6. 并发与资源管理

- **无共享可变状态**：`Handler.q` 启动时装配
- **每请求独立 context**：`context.WithTimeout` 派生子 ctx，`defer cancel()` 保障释放
- **超时分级**：search/getTrace 30s，tagValues 15s——元数据查询通常更快
- **无 goroutine 启动**：所有调用同步阻塞

## 7. 设计模式与亮点

1. **`q == nil` → 503 graceful degradation**：与 logs 同模式，Tempo 未配置时返 503 + 明确文案
2. **`json.RawMessage` 透传**：`Traces` / `Metrics` 不在 handler 层重定义 Tempo schema，直接透传原始 JSON
3. **getTrace 直接写 bytes**：`w.Write(out.Body)` 透传 OTLP JSON，让 SPA 直接 walk 结构无需 re-encode
4. **facet 模式 fallback**：`q` 为空时从 service/operation 构造 Tags map，支持 Tempo legacy 非 TraceQL 搜索
5. **`{trace_id}` 路由最后注册**：避免 shadow `/tags/...` 路由——chi 路由顺序敏感
6. **多格式时间解析**：与 logs 同，RFC3339 + unix 数字（自动秒/毫/纳）
7. **limit 1..1000 硬上限**：防前端误传超大 limit 拖垮 Tempo
8. **超时分级**：search/getTrace 30s，tagValues 15s
9. **错误响应简化 schema**：`{"error": msg}` 不带 code 字段，与 logs 一致

## 8. 注意事项

1. **链路不按 org 隔离**：包注释明示"traces are not org-scoped post-pivot"，所有认证用户可查所有链路
2. **`limit > 1000` 拒绝**：硬上限，前端不应尝试翻页累积
3. **`parseTime` 必填**：search 中 start/end 都必填，与 logs 一致
4. **`getTrace` 直接写 bytes**：不套 `writeJSON`，Content-Type 手动设；如需统一应改用 `writeJSON`
5. **30s/15s timeout 硬上限**：超时后 ctx 取消，Tempo 客户端应感知 ctx 并中止
6. **`writeErr` 不带 code 字段**：与 logs 一致，但与全局 envelope 不同
7. **无审计**：链路查询是只读操作，不写审计
8. **`q == nil` 时路由仍注册**：与 logs 同模式
9. **facet 模式仅当 `q` 为空**：`q` 非空时走 TraceQL，service/operation 参数被忽略
10. **`minDuration` / `maxDuration` 非负**：允许 0（不限制），但不允许负值
11. **`tagValues` 15s timeout**：短于 search/getTrace，因为元数据查询通常更快
12. **`{trace_id}` 是字符串**：Tempo trace ID 是 hex 字符串，非数字
