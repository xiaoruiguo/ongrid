# prometheus/http.go 技术实现文档

## 1. 概述

`http.go` 是 ongrid manager Prometheus 集成子域的 HTTP 路由层。提供 3 个端点：
1. `POST /v1/prometheus/launch` —— 生成带 ticket 的 Grafana 跳转 URL，把 ticket 写入 HttpOnly cookie
2. `GET /v1/prometheus/auth` —— nginx `auth_request` 子请求鉴权（验证 + 滑动刷新 ticket cookie）
3. `POST /v1/prometheus/query_range` —— 通用 PromQL passthrough，让 SPA PromQLPanel 直接查 Prom

设计目标：让 SPA 通过短期 ticket cookie 访问内嵌 Grafana，无需把 Grafana 直接暴露给浏览器。

## 2. 包信息

- **包名**：`prometheus`
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/server/prometheus`
- **路由**：
  - `RegisterProtected` —— `/v1/prometheus/launch` + `/v1/prometheus/query_range`（auth 中间件保护）
  - `RegisterPublic` —— `/v1/prometheus/auth`（nginx auth_request 调用，无 auth 中间件）
- **文件定位**：HTTP 适配层 + cookie 管理 + PromQL passthrough

## 3. 关键类型与接口

### Service —— ticket 业务接口

```go
type Service interface {
    BuildLaunch(caller svc.Caller, in svc.LaunchInput) (url string, ticket string, ttl time.Duration, err error)
    RefreshTicket(token string) (newToken string, ttl time.Duration, ok bool)
    VerifyTicket(token string) error
}
```

### PromQuerier —— 窄 PromQL 接口

```go
type PromQuerier interface {
    QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) (*promquery.InstantResult, error)
}
```

由 `*promquery.Client` 满足；可为 nil（`ONGRID_PROM_ENABLED=false` 时路由仍注册但返 503）。

### Handler

```go
type Handler struct {
    svc  Service
    prom PromQuerier // may be nil
}
```

两个构造器：`NewHandler(s)` 不带 Prom；`NewHandlerWithProm(s, prom)` 带 Prom 暴露 query_range。

### 常量

```go
const promTicketCookie = "ongrid_prom_ticket"
const maxExprBytes = 4 * 1024  // 4 KiB PromQL 上限
```

### DTO

```go
type launchReq struct {
    Expr       string `json:"expr"`
    RangeInput string `json:"range_input,omitempty"`
    EndInput   string `json:"end_input,omitempty"`
    StepInput  string `json:"step_input,omitempty"`
}

type launchResp struct {
    URL string `json:"url"`
}

type queryRangeReq struct {
    Expr  string `json:"expr"`
    Start string `json:"start"`
    End   string `json:"end"`
    Step  string `json:"step"`
}

type queryRangeResp struct {
    ResultType string          `json:"result_type"`
    Result     json.RawMessage `json:"result"`
    From       string          `json:"from"`
    To         string          `json:"to"`
}
```

## 4. 关键函数与流程

### launch —— 生成跳转 URL + ticket cookie

```go
func (h *Handler) launch(w http.ResponseWriter, r *http.Request)
```

1. `callerFromCtx` 取 caller
2. `json.Decode(&launchReq)`
3. `h.svc.BuildLaunch(caller, LaunchInput{...})` 返 (url, ticket, ttl, err)
4. **写 cookie**：`ongrid_prom_ticket` = ticket，HttpOnly + Secure + SameSite=Lax，MaxAge = ttl 秒
5. 200 + `{url}` —— SPA 用 url 跳转到 Grafana，浏览器带 cookie 访问

### auth —— nginx auth_request 鉴权 + 滑动刷新

```go
func (h *Handler) auth(w http.ResponseWriter, r *http.Request)
```

1. `r.Cookie(ongrid_prom_ticket)` 取 cookie，缺失 → 401
2. `h.svc.VerifyTicket(c.Value)` 验证，无效 → 401
3. **滑动刷新**：`h.svc.RefreshTicket(c.Value)` 返 (fresh, ttl, ok)，成功则 `Set-Cookie` 重写 cookie
4. 204 No Content（auth_request 协议：2xx 表示允许）

**关键设计**：每次成功 auth 都重 mint cookie，活跃会话永不过期；空闲 > TTL → 下次 401 → SPA 弹新 launch。

### queryRange —— PromQL passthrough

```go
func (h *Handler) queryRange(w http.ResponseWriter, r *http.Request)
```

1. `h.prom == nil` → `errs.ErrNotWiredYet`（503）
2. `tenantctx.From` 鉴权（任何已登录用户）
3. `json.Decode(&queryRangeReq)`
4. 校验 `expr`（非空 + ≤4 KB）、`start`/`end`（RFC3339 + end > start）、`step`（duration + > 0）
5. **30s context timeout**
6. `h.prom.QueryRange(ctx, expr, start, end, step)`
7. matrix 透传：`res.ResultType == "matrix"` 用 `res.Result`，否则 `[]`
8. 200 + `{result_type: "matrix", result, from, to}`

### helpers

- `callerFromCtx(ctx)` —— 从 `tenantctx` 构造 `svc.Caller{UserID, Role}`
- `writeJSON` / `writeErr` / `errCode` —— 标准 errs 映射，含 `not-wired-yet` slug

## 5. 依赖关系

**外部**：
- `chi` —— 路由
- `net/http`、`encoding/json`、`time`、`strings`、`fmt`、`errors`、`context`

**内部**：
- `internal/manager/service/prometheus`（`svc.Caller` / `svc.LaunchInput`）
- `internal/pkg/promquery`（InstantResult）
- `internal/pkg/errs`
- `internal/pkg/tenantctx`

## 6. 并发与资源管理

- **无共享可变状态**：`Handler.svc` / `Handler.prom` 启动时装配
- **30s context timeout**：queryRange 防 Prom 慢查询拖死请求
- **cookie 由浏览器管理**：本层只 Set-Cookie，无服务端 session 存储
- **`writeJSON` swallow encode 错误**：响应已开始无法回传

## 7. 设计模式与亮点

1. **ticket cookie 模式**：短期 ticket 写 HttpOnly cookie，浏览器访问 Grafana 时自动携带，无需把 Grafana 直接暴露给 SPA 代码
2. **滑动刷新 cookie**：每次 auth_request 成功都重 mint cookie，活跃会话永不过期；空闲超时自动 401 弹新 launch
3. **`prom == nil` 仍注册路由**：返 503 + 明确 slug `not-wired-yet`，让 SPA 优雅降级而非 404
4. **`maxExprBytes = 4 KiB`**：与 `/v1/metrics/query_range` 一致，防 authenticated client 用大 PromQL pin Prom
5. **matrix 透传 `json.RawMessage`**：不在 handler 层重定义 Prom schema，让 SPA 自行 reshape
6. **`Set-Cookie` 显式 String()**：auth 端点用 `(&http.Cookie{...}).String()` 而非 `http.SetCookie`，便于在已开始写响应后追加 header
7. **Secure + HttpOnly + SameSite=Lax**：cookie 安全三件套，防 XSS 偷 ticket + 防 CSRF
8. **两个 Register 方法**：`RegisterProtected`（auth 中间件内）+ `RegisterPublic`（auth_request 子请求无 auth 中间件）
9. **30s timeout 与 promquery.Client 内部一致**：显式设置避免继承父 ctx 更长 deadline

## 8. 注意事项

1. **cookie Secure=true**：仅 HTTPS 工作；HTTP 部署需调整或确保 nginx 终止 TLS
2. **`auth_request` 协议**：2xx 允许，401/403 拒绝；本端点返 204（无 body）
3. **滑动刷新有竞态**：并发 auth_request 可能 mint 多个新 cookie，最后一个生效；通常无害
4. **`RefreshTicket` 失败静默**：`if ok` 才 Set-Cookie，失败时返旧 cookie + 204，下次再刷新
5. **`queryRange` Prom 解析错误返 400**：`promquery` 返 plain error 无法区分用户输入错 vs upstream down，统一 400 让 UI 简单
6. **`prom == nil` 路由仍注册**：但 queryRange 返 503；launch 不依赖 prom 仍可工作（生成 URL）
7. **`launchReq` 含 `RangeInput/EndInput/StepInput`**：biz 层负责拼接到 Grafana URL；本层不解析
8. **无审计**：launch/queryRange 未调 `SetAuditEvent`；如需追溯 Prom 查询历史需另埋点
9. **`callerFromCtx` 与 marketplace 的 `callerFromRequest` 类似**：从 tenantctx 构造 biz caller，避免重复
10. **cookie Path="/"**：所有路径都可发送；如需限制可改 Path="/prometheus"
