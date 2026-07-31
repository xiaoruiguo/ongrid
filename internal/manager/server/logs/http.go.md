# logs/http.go 技术实现文档

## 1. 概述

`http.go` 是 ongrid manager 日志查询代理子域（`/v1/logs/*`）的 HTTP 路由层。它把内部 Loki 的查询 API 经过认证后暴露给 SPA，避免直接把 `/loki/*` 透给 nginx（数据面 `/loki/api/v1/push` 仍由 `auth_request` 卡 ingest）。共 3 个端点：query_range / labels / label values。

## 2. 包信息

- **包名**：`logs`
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/server/logs`
- **路由前缀**：`/v1/logs`（由 `cmd/ongrid/main.go` 在 chi router 上挂载，鉴权中间件由上游统一注入）
- **文件定位**：HTTP 代理层，无业务逻辑，只做参数解析 + 调 `Querier` + 写响应

## 3. 关键类型与接口

### Querier —— 窄接口

```go
type Querier interface {
    QueryRange(ctx context.Context, opts logquery.QueryRangeOptions) (*logquery.QueryRangeResult, error)
    LabelNames(ctx context.Context, start, end time.Time) ([]string, error)
    LabelValues(ctx context.Context, name string, start, end time.Time) ([]string, error)
}
```

由 `*logquery.Client` 通过结构化类型满足；handler 不依赖 Loki 客户端具体类型，便于测试替身。

### Handler

```go
type Handler struct {
    q Querier
}
```

### 响应 DTO

```go
type queryRangeResp struct {
    ResultType string          `json:"resultType"`
    Result     json.RawMessage `json:"result"`
    From       string          `json:"from"`
    To         string          `json:"to"`
}
```

`Result` 用 `json.RawMessage` 透传 Loki 原始结构（matrix/vector），避免在 handler 层重定义 Loki 的复杂响应 schema。

## 4. 关键函数与流程

### NewHandler —— q 可为 nil

```go
func NewHandler(q Querier) *Handler
```

`q` 允许为 nil，表示 Loki 未启用。所有 handler 在入口处 `if h.q == nil { writeErr(503, "logs backend disabled") }`，让 SPA 显示明确的"日志后端未启用"状态而非静默失败。

### Register —— 路由表

```go
func (h *Handler) Register(r chi.Router)
```

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/v1/logs/query_range` | 代理 `/loki/api/v1/query_range` |
| GET | `/v1/logs/labels` | 代理 `/loki/api/v1/labels` |
| GET | `/v1/logs/labels/{name}/values` | 代理 `/loki/api/v1/label/<name>/values` |

### queryRange

```go
func (h *Handler) queryRange(w http.ResponseWriter, r *http.Request)
```

流程：
1. `h.q == nil` → 503
2. `parseTime(start)` / `parseTime(end)` 解析时间（支持 RFC3339 + unix-seconds/millis/nanos）
3. `limit`：默认 1000，范围 1..5000（超出 400）
4. `step`：可选，`time.ParseDuration`，必须 > 0
5. `direction`：可选，必须为 `forward`/`backward`
6. **30s context timeout**：`context.WithTimeout(r.Context(), 30*time.Second)` 防 Loki 慢查询拖死请求
7. 调 `h.q.QueryRange`，返 `{resultType, result, from, to}`（from/to 转 RFC3339 UTC 字符串）

### labels / labelValues

```go
func (h *Handler) labels(w http.ResponseWriter, r *http.Request)
func (h *Handler) labelValues(w http.ResponseWriter, r *http.Request)
```

时间参数可选（`parseTime` 返 zero time 不阻断），**15s context timeout**（短于 query_range 的 30s，因为元数据查询通常更快）。`labelValues` 必须带 `{name}` URL 参数。

### parseTime —— 多格式时间解析

```go
func parseTime(s string) (time.Time, error)
```

按以下顺序尝试：
1. RFC3339（`2006-01-02T15:04:05Z07:00`）
2. Unix 数字（按数量级自动判秒/毫/纳）：
   - `> 1e15` → 纳秒（`time.Unix(0, n)`）
   - `> 1e12` → 毫秒（`time.UnixMilli(n)`）
   - 其它 → 秒（`time.Unix(n, 0)`）
3. 都失败 → `errs.ErrInvalid`

设计目的：方便 curl 测试（`?start=$(date +%s)`）和 SPA 同时使用。

### writeJSON / writeErr

```go
func writeJSON(w http.ResponseWriter, code int, body any)
func writeErr(w http.ResponseWriter, code int, msg string)
```

`writeErr` 用 `{"error": msg}` 而非全局 `{code,message,data}` envelope——本子域错误响应走简化 schema，前端按 `error` 字段直接展示。

## 5. 依赖关系

**外部**：
- `chi` —— URL 参数
- `net/http`、`encoding/json`、`strconv`、`time`

**内部**：
- `internal/pkg/logquery`（Loki 客户端 + DTO）
- `internal/pkg/errs`（错误哨兵）

## 6. 并发与资源管理

- **无共享可变状态**：`Handler.q` 在构造时确定，handler 内不修改；chi handler 并发安全。
- **每请求独立 context**：`context.WithTimeout` 派生子 ctx，`defer cancel()` 保障释放，避免 Loki 慢查询长时间占用连接。
- **超时分级**：query_range 30s，labels/labelValues 15s——按预期查询时长分级，避免元数据查询被慢 query_range 拖累。
- **无 goroutine 启动**：所有调用同步阻塞。

## 7. 设计模式与亮点

1. **`q == nil` → 503 graceful degradation**：Loki 可选部署，未配置时返 503 + 明确文案"logs backend disabled"，让 SPA 显示明确状态而非空列表/超时。
2. **`json.RawMessage` 透传**：`Result` 不在 handler 层重定义 Loki 复杂 schema，直接透传原始 JSON——既减少耦合，又避免 schema 漂移。
3. **多格式时间解析**：RFC3339 + unix 数字（自动秒/毫/纳），同时支持 curl 简单测试和 SPA 标准时间格式。
4. **limit 1..5000 硬上限**：防前端误传超大 limit 拖垮 Loki；默认 1000 是日志页典型一屏大小。
5. **错误响应简化 schema**：`{"error": msg}` 不带 code 字段，前端按 HTTP status + error 文案直接展示，简化前后端契约。
6. **结构化类型满足接口**：`*logquery.Client` 不需显式 `var _ Querier = ...`，鸭子类型让测试替身易写。

## 8. 注意事项

1. **日志不按 org 隔离**：包注释明示"logs are not org-scoped post-pivot"，所有认证用户可查所有日志。多租户隔离需求需在 Loki 层用 tenant label + 查询时强制注入实现。
2. **`limit > 5000` 拒绝**：硬上限，前端不应尝试翻页累积；如需深度查询应缩小时间窗口。
3. **`parseTime` 静默 fallback**：labels/labelValues 中 `parseTime` 失败时返 zero time，**不阻断**——这是有意的"宽容解码"，让元数据查询对时间参数缺失容错。
4. **30s/15s timeout 是硬上限**：超时后 ctx 取消，Loki 客户端应感知 ctx 并中止；前端 loading 超过这个时间会收到 504/502。
5. **`writeErr` 不带 code 字段**：与全局 envelope 不一致；如需统一需评估前端兼容。
6. **无审计**：日志查询是只读操作，不写审计；如需追溯"谁查过哪些日志"需另埋点。
7. **`q == nil` 时路由仍注册**：`Register` 不依赖 `h.q`，nil 时各 handler 各自返 503，路由表本身完整。
