# OnGrid Middleware 技术实现分析

> 分析 OnGrid 中所有 middleware 的技术实现，包括后端 Go HTTP 中间件和前端路由守卫/拦截器。
> 包含源码文件路径、关键行号、函数签名、执行顺序。

---

## 目录

1. [中间件总览](#1-中间件总览)
2. [OTel HTTP 中间件](#2-otel-http-中间件)
3. [MetricsMiddleware（自观测 HTTP 指标）](#3-metricsmiddleware自观测-http-指标)
4. [AuditMiddleware（审计日志）](#4-auditmiddleware审计日志)
5. [auth.Middleware（JWT 认证）](#5-authmiddlewarejwt-认证)
6. [authzmw.Middleware（Casbin 授权）](#6-authzmwmiddlewarecasbin-授权)
7. [edgeauth（数据平面认证）](#7-edgeauth数据平面认证)
8. [requireAdmin（遗留管理员守卫）](#8-requireadmin遗留管理员守卫)
9. [writeMW / deleteMW（路由级授权工厂）](#9-writemw--deletemw路由级授权工厂)
10. [tenantctx 双层存储机制](#10-tenantctx-双层存储机制)
11. [前端路由守卫](#11-前端路由守卫)
12. [前端请求拦截器（401 自动刷新）](#12-前端请求拦截器401-自动刷新)
13. [中间件执行顺序与挂载点](#13-中间件执行顺序与挂载点)
14. [关键设计要点](#14-关键设计要点)

---

## 1. 中间件总览

OnGrid 的中间件分为三层：

| 层级 | 中间件 | 文件 | 挂载方式 |
|------|--------|------|----------|
| **全局 mux** | OTel HTTP | [main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) L2706 | `mux.Use()` |
| **全局 mux** | MetricsMiddleware | [metrics.go](file:///d:/claude/ongrid/internal/manager/server/middleware/metrics.go) | `mux.Use()` |
| **全局 mux** | AuditMiddleware | [audit.go](file:///d:/claude/ongrid/internal/manager/server/middleware/audit.go) | `mux.Use()` |
| **protected 组** | auth.Middleware | [auth/middleware.go](file:///d:/claude/ongrid/internal/pkg/auth/middleware.go) | `protected.Use()` |
| **路由级** | authzmw.Require | [authzmw/middleware.go](file:///d:/claude/ongrid/internal/pkg/authzmw/middleware.go) | `r.With(mw.Require(obj, act))` |
| **路由级** | requireAdmin | [edge/http.go](file:///d:/claude/ongrid/internal/manager/server/edge/http.go) L315 | `r.With(h.requireAdmin)` |
| **路由级** | writeMW / deleteMW | edge/http.go, knowledge/http.go | `r.With(h.writeMW(obj))` |
| **内部端点** | edgeauth | [edgeauth/http.go](file:///d:/claude/ongrid/internal/manager/server/edgeauth/http.go) | `Register(mux)` |
| **前端** | RequireAuth | [App.tsx](file:///d:/claude/ongrid/web/src/App.tsx) L63 | React Router 包裹 |
| **前端** | PublicOnly | App.tsx L72 | React Router 包裹 |
| **前端** | 401 自动刷新 | [client.ts](file:///d:/claude/ongrid/web/src/api/client.ts) L97 | request 函数内联 |

---

## 2. OTel HTTP 中间件

**文件**: [main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) 第 2702-2715 行

### 2.1 构造

```go
// 第 2706-2715 行
otelhttpmw := func(next http.Handler) http.Handler {
    return otelhttp.NewHandler(next, "ongrid-manager",
        otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
            if route := chi.RouteContext(r.Context()).RoutePattern(); route != "" {
                return r.Method + " " + route  // e.g. "POST /v1/chat/sessions"
            }
            return r.Method + " " + r.URL.Path  // 404 时降级
        }),
    )
}
```

### 2.2 挂载

```go
// 第 2726 行
mux.Use(otelhttpmw)
```

### 2.3 技术实现

- 使用 `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` 包
- 每个请求包裹一个 OTel span，span 名为 `{METHOD} {ROUTE_PATTERN}`
- 路由模式由 chi 的 `RouteContext(r.Context()).RoutePattern()` 提供
- 404 请求（无路由模式）降级为 `{METHOD} {URL.Path}`
- Tempo 的 spanmetrics 生成器据此派生 `traces_spanmetrics_latency_bucket` 指标

### 2.4 TracerProvider 初始化

**文件**: [tracing.go](file:///d:/claude/ongrid/internal/pkg/tracing/tracing.go)

```go
// 第 60-105 行
func Init(ctx context.Context, cfg Config) (Shutdown, error) {
    // 第 68-73 行: OTLP HTTP exporter
    exporter, err := otlptracehttp.New(ctx, opts...)

    // 第 85-88 行: Resource（service.name）
    res := resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName(cfg.ServiceName))

    // 第 91-99 行: TracerProvider
    tp := sdktrace.NewTracerProvider(
        sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(2*time.Second)),
        sdktrace.WithResource(res),
        sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SamplingRatio))),
    )
    otel.SetTracerProvider(tp)
    otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
        propagation.TraceContext{}, propagation.Baggage{},
    ))
    return tp.Shutdown, nil
}
```

**关键配置**:
- `BatchTimeout: 2s` — 快速 flush，事故 span 不会滞留
- `SamplingRatio: 1.0` — 全采样（当前规模下可承受）
- `Insecure: true` — docker 网络内明文 HTTP
- Endpoint 为空时返回 no-op shutdown，调用方可无条件 defer

---

## 3. MetricsMiddleware（自观测 HTTP 指标）

**文件**: [metrics.go](file:///d:/claude/ongrid/internal/manager/server/middleware/metrics.go)

### 3.1 实现

```go
// 第 22-34 行
func MetricsMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)  // chi 的 responseWriter 包装器
        start := time.Now()
        next.ServeHTTP(ww, r)

        route := chi.RouteContext(r.Context()).RoutePattern()
        if route == "" {
            route = "unknown"  // 404 归入 "unknown"，防止基数爆炸
        }
        prom.ObserveHTTP(r.Method, route, ww.Status(), time.Since(start).Seconds())
    })
}
```

### 3.2 prom.ObserveHTTP

**文件**: [manager_metrics.go](file:///d:/claude/ongrid/internal/pkg/prom/manager_metrics.go) 第 357-363 行:

```go
func ObserveHTTP(method, route string, status int, seconds float64) {
    HTTPRequestsTotal.WithLabelValues(method, route, statusClass(status)).Inc()
    HTTPRequestDuration.WithLabelValues(method, route).Observe(seconds)
}
```

### 3.3 statusClass

```go
// 第 365-375 行
func statusClass(status int) string {
    switch {
    case status >= 500: return "5xx"
    case status >= 400: return "4xx"
    case status >= 300: return "3xx"
    default:           return "2xx"
    }
}
```

### 3.4 设计要点

- **基数控制**: route label 使用 chi 编译时的 `RoutePattern`，基数由路由表大小决定，不随 URL 参数增长
- **404 安全**: 无路由模式的请求归入 `"unknown"`，防止随机扫描 URL 创建无限 series
- **执行顺序**: 在 OTel 之后运行（`mux.Use` 顺序），此时 chi 已填充 `RouteContext`
- **ADR-026**: 自观测 HTTP 指标，用于监控 OnGrid 自身的延迟和错误率

---

## 4. AuditMiddleware（审计日志）

**文件**: [audit.go](file:///d:/claude/ongrid/internal/manager/server/middleware/audit.go)

### 4.1 核心设计

审计中间件采用**显式标注模式**：只记录 handler 主动调用 `SetAuditEvent` 标注的操作，不做通用 fallback。这避免了 `http_post_alerts` 这类无意义的审计行。

### 4.2 auditSlot 可变槽

```go
// 第 25-30 行
type auditContextKey struct{}

type auditSlot struct {
    ev  audit.Event
    set bool
}
```

**关键**: slot 是 `*auditSlot` 指针类型，即使中间件链中后续 middleware 调用 `r.WithContext()` 重新包装 request，指针仍然存活。

### 4.3 SetAuditEvent / GetAuditEvent

```go
// 第 35-45 行
func SetAuditEvent(r *http.Request, ev audit.Event) {
    slot, ok := r.Context().Value(auditContextKey{}).(*auditSlot)
    if !ok || slot == nil { return }
    slot.ev = ev
    slot.set = true
}

// 第 48-54 行
func GetAuditEvent(ctx context.Context) (audit.Event, bool) {
    slot, ok := ctx.Value(auditContextKey{}).(*auditSlot)
    if !ok || slot == nil || !slot.set { return audit.Event{}, false }
    return slot.ev, true
}
```

### 4.4 AuditMiddleware 实现

```go
// 第 72-104 行
func AuditMiddleware(uc *audit.Usecase) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)

            // L81-82: 安装 auditSlot
            slot := &auditSlot{}
            ctx := context.WithValue(r.Context(), auditContextKey{}, slot)

            // L89: 安装 tenantctx slot（让 auth.Middleware 能写入 tenant）
            ctx = tenantctx.WithSlot(ctx)

            // L90: 调用内层链
            next.ServeHTTP(ww, r.WithContext(ctx))

            // L92-94: 未标注则跳过
            if uc == nil || !slot.set { return }

            // L100: 从 request + tenantctx 补全 ev 字段
            enrichFromRequest(&slot.ev, r, ctx, ww.Status())

            // L101: 异步发射审计事件
            uc.Emit(ctx, slot.ev)
        })
    }
}
```

### 4.5 enrichFromRequest

```go
// 第 106-131 行
func enrichFromRequest(ev *audit.Event, r *http.Request, ctx context.Context, status int) {
    // 从 tenantctx 补全 UserID / Email / Role
    if t, ok := tenantctx.From(ctx); ok {
        if uid := t.UserID; uid != 0 && ev.UserID == nil { ev.UserID = &uid }
        if ev.UserEmail == "" { ev.UserEmail = t.Email }
        if ev.Role == "" { ev.Role = t.Role }
    }
    // 补全 IP / UserAgent / RequestID / Status
    if ev.IP == "" { ev.IP = clientIP(r) }
    if ev.UserAgent == "" { ev.UserAgent = truncate(r.UserAgent(), 512) }
    if ev.RequestID == "" { ev.RequestID = chimw.GetReqID(r.Context()) }
    if ev.Status == "" { ev.Status = statusBucket(status) }
}
```

### 4.6 statusBucket

```go
// 第 133-144 行
func statusBucket(status int) string {
    switch {
    case status >= 500:             return auditmodel.StatusFailure
    case status == http.StatusForbidden: return auditmodel.StatusDenied
    case status >= 400:             return auditmodel.StatusFailure
    default:                        return auditmodel.StatusSuccess
    }
}
```

### 4.7 clientIP

```go
// 第 146-161 行
func clientIP(r *http.Request) string {
    // XFF 首跳是真实客户端（nginx 代理场景）
    if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
        if comma := strings.IndexByte(xff, ','); comma >= 0 {
            return strings.TrimSpace(xff[:comma])
        }
        return strings.TrimSpace(xff)
    }
    // 降级到 RemoteAddr
    host, _, err := net.SplitHostPort(r.RemoteAddr)
    if err == nil { return host }
    return r.RemoteAddr
}
```

### 4.8 audit.Event 结构体

**文件**: [usecase.go](file:///d:/claude/ongrid/internal/manager/biz/audit/usecase.go) 第 31-56 行:

```go
type Event struct {
    UserID    *uint64    // *uint64 支持 nil（未认证路径）
    UserEmail string
    Role      string
    IP        string
    UserAgent string
    RequestID string

    Action       string   // 必须是 model.Action* 常量
    ResourceType string
    ResourceID   string
    ResourceName string

    Status       string   // success|failure|denied
    ErrorCode    string
    ErrorMessage string

    Payload any           // 自由格式，调用方负责脱敏
}
```

### 4.9 Usecase.Emit

**文件**: [usecase.go](file:///d:/claude/ongrid/internal/manager/biz/audit/usecase.go) 第 74-94 行:

```go
func (u *Usecase) Emit(ctx context.Context, ev Event) {
    if u == nil || u.repo == nil { return }
    if ev.Action == "" || ev.Status == "" {
        u.log.Warn("audit: dropped event with empty action or status", ...)
        return
    }
    row := &model.Log{
        OccurredAt:   time.Now().UTC(),
        UserID:       ev.UserID,
        UserEmail:    ev.UserEmail,
        // ... 全字段映射
    }
    u.repo.Insert(ctx, row)
}
```

---

## 5. auth.Middleware（JWT 认证）

**文件**: [auth/middleware.go](file:///d:/claude/ongrid/internal/pkg/auth/middleware.go)

### 5.1 实现

```go
// 第 21-53 行
func Middleware(signer *Signer) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            // L24: 提取 Bearer token
            tok := extractBearer(r)
            if tok == "" {
                http.Error(w, "missing bearer token", 401)
                return
            }

            // L29: 验证 JWT（不查数据库）
            claims, err := signer.Verify(tok)
            if err != nil {
                http.Error(w, "invalid token", 401)
                return
            }

            // L38: 构造 Tenant（兼容旧 token）
            isSuper := claims.IsSuperuser || claims.Role == "admin"
            t := tenantctx.Tenant{
                UserID:      claims.UserID,
                Email:       claims.Email,
                Role:        claims.Role,
                IsSuperuser: isSuper,
            }

            // L48: 写入外层可变 slot（供审计中间件读取）
            tenantctx.SetOnSlot(r.Context(), t)

            // L49: 写入 context（供下游 handler 读取）
            ctx := tenantctx.With(r.Context(), t)

            // L50: 调用下一个 handler
            next.ServeHTTP(w, r.WithContext(ctx))
        })
    }
}
```

### 5.2 extractBearer

```go
// 第 58-67 行
func extractBearer(r *http.Request) string {
    const prefix = "Bearer "
    // 优先从 Authorization 头提取
    if h := r.Header.Get("Authorization"); strings.HasPrefix(h, prefix) {
        return strings.TrimPrefix(h, prefix)
    }
    // 降级到 ?token=<jwt> query 参数（WebSocket 浏览器无法设置 header）
    if q := r.URL.Query().Get("token"); q != "" {
        return q
    }
    return ""
}
```

### 5.3 Signer.Verify

**文件**: [jwt.go](file:///d:/claude/ongrid/internal/pkg/auth/jwt.go) 第 84-99 行:

```go
func (s *Signer) Verify(token string) (*Claims, error) {
    var c Claims
    t, err := jwt.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) {
        // 防止算法混淆攻击
        if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
            return nil, errors.New("unexpected signing method")
        }
        return s.secret, nil
    })
    if err != nil { return nil, err }
    if !t.Valid { return nil, errors.New("invalid token") }
    return &c, nil
}
```

**关键**: Verify 只做签名 + 过期验证，**不查数据库**。用户身份在登录时烤进 token，token 有效期内信任。

### 5.4 Claims 结构体

```go
// jwt.go 第 24-30 行
type Claims struct {
    UserID      uint64 `json:"user_id"`
    Email       string `json:"email,omitempty"`
    Role        string `json:"role"`
    IsSuperuser bool   `json:"is_superuser,omitempty"`
    jwt.RegisteredClaims
}
```

---

## 6. authzmw.Middleware（Casbin 授权）

**文件**: [authzmw/middleware.go](file:///d:/claude/ongrid/internal/pkg/authzmw/middleware.go)

### 6.1 Authorizer 接口

```go
// 第 38-41 行
type Authorizer interface {
    Allow(ctx context.Context, userID, orgID uint64, obj, act string) bool
    AllowAnyOrg(ctx context.Context, userID uint64, obj, act string) bool
}
```

接口在消费方定义，`*iam/biz/authz.Enforcer` 满足它，避免 manager 包依赖 iam BC。

### 6.2 Middleware 结构体

```go
// 第 44-47 行
type Middleware struct {
    z   Authorizer
    log *slog.Logger
}

// 第 50-55 行
func New(z Authorizer, log *slog.Logger) *Middleware {
    if log == nil { log = slog.Default() }
    return &Middleware{z: z, log: log}
}
```

### 6.3 Require 方法 — 5 级决策

```go
// 第 70-96 行
func (m *Middleware) Require(obj, act string) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            // 1. 无 tenant → 401
            t, ok := tenantctx.From(r.Context())
            if !ok {
                http.Error(w, "unauthorized", 401)
                return
            }
            // 2. 超级用户 → 绕过（allow）
            if t.IsSuperuser {
                next.ServeHTTP(w, r)
                return
            }
            // 3. Authorizer 为 nil（遗留/测试）→ 绕过（allow）
            if m.z == nil {
                next.ServeHTTP(w, r)
                return
            }
            // 4. AllowAnyOrg → allow
            if m.z.AllowAnyOrg(r.Context(), t.UserID, obj, act) {
                next.ServeHTTP(w, r)
                return
            }
            // 5. 否则 → 403
            m.log.Info("authz: denied",
                slog.Uint64("user", t.UserID),
                slog.String("obj", obj),
                slog.String("act", act))
            http.Error(w, "forbidden", 403)
        })
    }
}
```

### 6.4 对象命名约定

```
edge:*           — edge CRUD + plugin config
knowledge:doc    — manual / repo doc mutations
knowledge:repo   — git repo registration
alert:rule       — alert rule CRUD
alert:incident   — incident ack / resolve / silence
agent:custom     — user-defined agent CRUD
monitor:panel    — monitor add-panel CRUD
org:*            — managed via /v1/orgs
user:*           — managed via /v1/users
```

### 6.5 动作词汇

`read` / `write` / `delete` / `manage`

### 6.6 挂载方式

```go
// main.go 第 474 行
authzMW := authzmw.New(authzEnf, log.With(slog.String("comp", "authzmw")))

// main.go 第 984 行
edgeHandler.SetAuthz(authzMW)

// 路由级使用
r.With(mw.Require("edge:*", "write")).Post("/v1/edges", ...)
r.With(mw.Require("edge:*", "delete")).Delete("/v1/edges/{id}", ...)
```

---

## 7. edgeauth（数据平面认证）

**文件**: [edgeauth/http.go](file:///d:/claude/ongrid/internal/manager/server/edgeauth/http.go)

### 7.1 用途

为 nginx 的 `auth_request` 模块提供内部认证端点。nginx 在代理遥测数据平面请求（Loki、Tempo）前，先调用此端点验证 Basic Auth 凭证。

### 7.2 Authenticator 接口

```go
// 第 31-33 行
type Authenticator interface {
    AuthenticateDataPlane(ctx context.Context, accessKey, secretKey string) (Identity, error)
}

type Identity struct {
    EdgeID    uint64
    ClusterID uint64
}
```

### 7.3 verify Handler

```go
// 第 67-97 行
func (h *Handler) verify(w http.ResponseWriter, r *http.Request) {
    // 解析 Basic Auth
    user, pass, ok := parseBasicAuth(r.Header.Get("Authorization"))
    if !ok {
        w.Header().Set("WWW-Authenticate", `Basic realm="ongrid-data-plane"`)
        http.Error(w, "missing or malformed Authorization header", 401)
        return
    }

    // 认证
    identity, err := h.authn.AuthenticateDataPlane(r.Context(), user, pass)
    if err != nil {
        if errors.Is(err, errs.ErrUnauthorized) {
            http.Error(w, "unauthorized", 401)
            return
        }
        http.Error(w, "auth backend error", 500)
        return
    }

    // 返回 edge_id / cluster_id 给 nginx
    if identity.EdgeID != 0 {
        w.Header().Set("X-Edge-Id", uintToA(identity.EdgeID))
    }
    if identity.ClusterID != 0 {
        w.Header().Set("X-Cluster-Id", uintToA(identity.ClusterID))
    }
    w.WriteHeader(200)
}
```

### 7.4 挂载

```go
// main.go 第 2742-2744 行
dataPlaneAuthHandler.Register(mux)                                    // /internal/auth/dataplane-verify
edgeOnlyAuthHandler.RegisterAt(mux, "/internal/auth/edge-verify")     // edge 专用
telemetryOnlyAuthHandler.RegisterAt(mux, "/internal/auth/telemetry-verify")  // K8s 遥测专用
```

**关键**: 挂载在公开 mux 上（无 JWT auth），因为 nginx 是唯一合法调用者，由 docker 网络策略保护。nginx **不得**代理外部流量到这些端点。

---

## 8. requireAdmin（遗留管理员守卫）

**文件**: [edge/http.go](file:///d:/claude/ongrid/internal/manager/server/edge/http.go) 第 313-328 行

```go
// requireAdmin is a thin middleware that 403s non-admin callers.
func (h *Handler) requireAdmin(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        t, ok := tenantctx.From(r.Context())
        if !ok {
            writeErr(w, errs.ErrUnauthorized)
            return
        }
        if t.Role != roleAdmin {
            writeErr(w, errs.ErrForbidden)
            return
        }
        next.ServeHTTP(w, r)
    })
}
```

**用途**: authzmw 未接线时的降级守卫。当 `h.authz == nil` 时，`writeMW` / `deleteMW` 返回 `h.requireAdmin`。

```go
// edge/http.go 第 37 行
const roleAdmin = "admin"  // 镜像 iam/model.RoleAdmin，不跨 BC 引用
```

---

## 9. writeMW / deleteMW（路由级授权工厂）

**文件**: [edge/http.go](file:///d:/claude/ongrid/internal/manager/server/edge/http.go) 第 115-128 行

```go
// writeMW 返回写类路由的授权中间件
func (h *Handler) writeMW(obj string) func(http.Handler) http.Handler {
    if h.authz != nil {
        return h.authz.Require(obj, "write")  // Casbin 授权
    }
    return h.requireAdmin  // 遗留降级
}

// deleteMW 返回删除类路由的授权中间件
func (h *Handler) deleteMW(obj string) func(http.Handler) http.Handler {
    if h.authz != nil {
        return h.authz.Require(obj, "delete")
    }
    return h.requireAdmin
}
```

**使用方式**:

```go
// edge/http.go Register 方法内
r.With(h.writeMW("edge:*")).Post("/v1/edges", h.create)
r.With(h.deleteMW("edge:*")).Delete("/v1/edges/{id}", h.delete)
```

**knowledge/http.go** 也有相同模式（第 93-107 行），额外有 `passthrough` 降级:

```go
func passthrough(next http.Handler) http.Handler { return next }
```

---

## 10. tenantctx 双层存储机制

**文件**: [tenantctx.go](file:///d:/claude/ongrid/internal/pkg/tenantctx/tenantctx.go)

### 10.1 问题背景

中间件执行顺序：AuditMiddleware（外层，先执行）→ auth.Middleware（内层，后执行）。外层的 `r` 不携带内层的 `WithContext`，但外层需要读取内层写入的 tenant 值。

### 10.2 解决方案

```
AuditMiddleware (外层)
  ├── 安装 tenantctx slot（*slot 指针）
  ├── 安装 auditSlot（*auditSlot 指针）
  │
  └── next.ServeHTTP(ww, r.WithContext(ctx))
        │
        auth.Middleware (内层)
          ├── 验证 JWT
          ├── SetOnSlot(ctx, t)  ← 写入外层安装的 slot
          ├── With(ctx, t)       ← 写入 context value
          │
          └── next.ServeHTTP(w, r.WithContext(ctx))
                │
                Handler
                  └── tenantctx.From(ctx)  ← 优先读 slot
```

### 10.3 核心方法

```go
// 第 33-35 行: 标准 context 存储（不可变）
func With(ctx context.Context, t Tenant) context.Context {
    return context.WithValue(ctx, ctxKey{}, t)
}

// 第 43-49 行: 读取（优先读 slot）
func From(ctx context.Context) (Tenant, bool) {
    if s, ok := ctx.Value(slotKey{}).(*slot); ok && s != nil && s.set {
        return s.t, true           // 优先读 slot
    }
    t, ok := ctx.Value(ctxKey{}).(Tenant)
    return t, ok                   // 降级到 context value
}

// 第 74-76 行: 安装可变 slot
func WithSlot(ctx context.Context) context.Context {
    return context.WithValue(ctx, slotKey{}, &slot{})
}

// 第 81-86 行: 写入 slot
func SetOnSlot(ctx context.Context, t Tenant) {
    if s, ok := ctx.Value(slotKey{}).(*slot); ok && s != nil {
        s.t = t
        s.set = true
    }
}
```

### 10.4 指针存活原理

`*slot` 是指针类型。即使中间件链中 `r.WithContext()` 创建了新的 context，只要新 context 是从携带 slot 的 context 派生的，`ctx.Value(slotKey{})` 仍然返回同一个 `*slot` 指针。指针指向的 `slot.t` 字段可以被任何持有该 context 的代码修改和读取。

---

## 11. 前端路由守卫

**文件**: [App.tsx](file:///d:/claude/ongrid/web/src/App.tsx)

### 11.1 RequireAuth

```tsx
// 第 63-70 行
function RequireAuth({ children }: { children: ReactNode }) {
  const token = useAuth((s) => s.token);
  const location = useLocation();
  if (!token) {
    return <Navigate to="/login" replace state={{ from: location.pathname }} />;
  }
  return <>{children}</>;
}
```

**机制**: 从 zustand auth store 读取 token，无 token 则重定向到 `/login`，登录后可通过 `state.from` 返回原页面。

### 11.2 PublicOnly

```tsx
// 第 72-76 行
function PublicOnly({ children }: { children: ReactNode }) {
  const token = useAuth((s) => s.token);
  if (token) return <Navigate to="/" replace />;
  return <>{children}</>;
}
```

**机制**: 已登录用户访问 `/login` 时重定向到首页。

### 11.3 路由结构

```tsx
// 第 78-206 行
<Routes>
  <Route path="/login" element={<PublicOnly><LoginPage /></PublicOnly>} />

  <Route element={<RequireAuth><Layout /></RequireAuth>}>
    <Route path="/" element={<HomePage />} />
    <Route path="/chat/:sessionId" element={<ChatThreadPage />} />
    {/* ... 其他受保护路由 ... */}
  </Route>

  <Route path="/r/:token" element={<ReportViewPage />} />  {/* 公开报告 */}
  <Route path="/p/:token" element={<PageViewPage />} />     {/* 公开页面 */}
  <Route path="*" element={<Navigate to="/" replace />} />
</Routes>
```

---

## 12. 前端请求拦截器（401 自动刷新）

**文件**: [client.ts](file:///d:/claude/ongrid/web/src/api/client.ts)

### 12.1 request 函数中的 401 处理

```typescript
// 第 97-111 行
if (res.status === 401 && !opts.noAuth) {
    const nextToken = await refreshAccessToken();
    if (nextToken && !opts._retryingAfterRefresh) {
        // 刷新成功 + 未重试过 → 递归重试一次
        return request<T>(method, path, body, { ...opts, _retryingAfterRefresh: true });
    }
    // 刷新失败 → logout
    if (!nextToken) {
        useAuth.getState().logout();
    }
}
throw new ApiError(msg, res.status, code, parsed);
```

### 12.2 refreshAccessToken — 单飞去重

```typescript
// 第 117-162 行
async function refreshAccessToken(): Promise<string | null> {
    // 单飞：并发请求只触发一次刷新
    if (refreshInFlight) return refreshInFlight;

    refreshInFlight = (async () => {
        const refreshToken = getRefreshToken();
        if (!refreshToken) return null;

        const res = await fetch(`${BASE}/auth/refresh`, {
            method: 'POST',
            headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
            body: JSON.stringify({ refresh_token: refreshToken }),
        });

        if (!res.ok) return null;

        const parsed = await res.json();
        if (!parsed?.access_token) return null;

        // 更新 auth store
        useAuth.getState().setSession({
            access_token: parsed.access_token,
            refresh_token: parsed.refresh_token ?? refreshToken,
            role: parsed.role ?? current.role ?? 'user',
            email: current.email ?? '',
        });
        return parsed.access_token;
    })()
        .catch(() => null)
        .finally(() => { refreshInFlight = null; });

    return refreshInFlight;
}
```

### 12.3 设计要点

- **单飞**: `refreshInFlight` 是模块级变量，并发 401 请求共享同一个刷新 Promise
- **一次重试**: `_retryingAfterRefresh` 标志防止无限递归
- **刷新失败才 logout**: 重试后仍 401（新 token 也被拒）不 logout，因为可能是 casbin 策略问题而非 session 过期
- **SSE 绕过**: `streamMessage`（chat.ts:252）不走 `request`，自己手动注入 token，不做 401 刷新

---

## 13. 中间件执行顺序与挂载点

### 13.1 完整中间件栈

```
HTTP 请求
    │
    ▼
┌─────────────────────────────────────────────────────────────┐
│ mux (chi.NewRouter)                          main.go L2718  │
│                                                              │
│  1. otelhttpmw                               main.go L2726  │
│     └── otelhttp.NewHandler("ongrid-manager")               │
│         span = "{METHOD} {ROUTE_PATTERN}"                    │
│                                                              │
│  2. MetricsMiddleware                        main.go L2729  │
│     └── prom.ObserveHTTP(method, route, status, duration)   │
│         ├── HTTPRequestsTotal{method,route,status}.Inc()    │
│         └── HTTPRequestDuration{method,route}.Observe(sec)  │
│                                                              │
│  3. AuditMiddleware(auditUC)                 main.go L2732  │
│     ├── 安装 auditSlot + tenantctx slot                     │
│     ├── next.ServeHTTP(ww, r)                               │
│     └── post-handler: enrichFromRequest + uc.Emit           │
│                                                              │
│  ├── /healthz, /readyz (无 auth)                            │
│  ├── /internal/auth/* (edgeauth, 无 JWT auth)               │
│  │                                                           │
│  └── /api 路由组                              main.go L2750  │
│      ├── 公开路由 (iam login/refresh, IM webhooks, pages)   │
│      │                                                       │
│      └── protected.Group                     main.go L2779  │
│          │                                                   │
│          4. auth.Middleware(signer)          main.go L2780  │
│             ├── extractBearer(r)                             │
│             ├── signer.Verify(tok)                           │
│             ├── tenantctx.SetOnSlot(ctx, t)                 │
│             ├── tenantctx.With(ctx, t)                       │
│             └── next.ServeHTTP(w, r.WithContext(ctx))       │
│          │                                                   │
│          ├── aiopsHandler.Register(protected) L2850         │
│          │   └── POST /v1/chat/sessions → createSession     │
│          │       (无路由级 authz，任何认证用户可创建)        │
│          │                                                   │
│          ├── edgeHandler.Register(protected)                │
│          │   └── r.With(writeMW("edge:*")).Post(...)        │
│          │       └── authzmw.Require("edge:*", "write")     │
│          │           ├── 1. 无 tenant → 401                  │
│          │           ├── 2. superuser → bypass               │
│          │           ├── 3. nil authorizer → bypass          │
│          │           ├── 4. AllowAnyOrg → allow             │
│          │           └── 5. else → 403                       │
│          │                                                   │
│          ├── knowledgeHandler.Register(protected)           │
│          │   └── r.With(writeMW("knowledge:doc")).Post(...) │
│          │                                                   │
│          ├── alertHandler.Register(protected)               │
│          ├── settingHandler.Register(protected)             │
│          └── ... 其他 handler                                │
└─────────────────────────────────────────────────────────────┘
```

### 13.2 中间件注册代码

```go
// main.go 第 2718-2732 行
mux := chi.NewRouter()
mux.Use(otelhttpmw)                              // 1. OTel
mux.Use(managermiddleware.MetricsMiddleware)     // 2. Metrics
mux.Use(managermiddleware.AuditMiddleware(auditUC))  // 3. Audit

// main.go 第 2779-2780 行
api.Group(func(protected chi.Router) {
    protected.Use(auth.Middleware(signer))       // 4. Auth
    // ... handler 注册 ...
})
```

### 13.3 路由级中间件使用

```go
// edge/http.go Register 方法内
r.Get("/v1/edges", h.list)                                              // 无 authz
r.With(h.writeMW("edge:*")).Post("/v1/edges", h.create)                 // write
r.With(h.deleteMW("edge:*")).Delete("/v1/edges/{id}", h.delete)         // delete
r.With(h.writeMW("edge:*")).Post("/v1/edges/{id}/rotate-secret", ...)   // write
```

---

## 14. 关键设计要点

### 14.1 显式审计模式

**文件**: [audit.go](file:///d:/claude/ongrid/internal/manager/server/middleware/audit.go) 第 56-71 行

审计中间件不做通用 fallback，只记录 handler 主动调用 `SetAuditEvent` 标注的操作。`audit_logs` 是精心策展的用户操作轨迹，不是访问日志。

### 14.2 指针存活模式

auditSlot 和 tenantctx slot 都使用 `*pointer` 类型存入 context。即使中间件链中 `r.WithContext()` 创建新 context，指针仍存活，内层 middleware 写入的值外层可见。

### 14.3 JWT 无数据库查询

**文件**: [jwt.go](file:///d:/claude/ongrid/internal/pkg/auth/jwt.go) 第 84-99 行

`Signer.Verify` 只做签名 + 过期验证，不查数据库。每个请求的认证开销仅为 HMAC 计算和 JSON 解析。

### 14.4 超级用户短路

**文件**: [authzmw/middleware.go](file:///d:/claude/ongrid/internal/pkg/authzmw/middleware.go) 第 78-81 行

`IsSuperuser` 绕过 Casbin 检查。损坏的 casbin 策略永远不会锁死系统管理员。

### 14.5 基数控制

**文件**: [metrics.go](file:///d:/claude/ongrid/internal/manager/server/middleware/metrics.go) 第 29-31 行

route label 使用 chi 编译时的 `RoutePattern`，404 归入 `"unknown"`。Prometheus series 基数由路由表大小决定，不随 URL 参数增长。

### 14.6 双层降级

authzmw 未接线时降级到 `requireAdmin`（admin-only），knowledge handler 降级到 `passthrough`（无授权）。保证部署灵活性。

### 14.7 数据平面认证分离

**文件**: [edgeauth/http.go](file:///d:/claude/ongrid/internal/manager/server/edgeauth/http.go)

数据平面认证（edge/telemetry → Loki/Tempo）使用 Basic Auth + nginx auth_request，与控制平面认证（JWT）完全分离。三个端点（dataplane/edge/telemetry）支持不同的凭证 scope。

### 14.8 前端单飞刷新

**文件**: [client.ts](file:///d:/claude/ongrid/web/src/api/client.ts) 第 118 行

`refreshInFlight` 模块级变量确保并发 401 请求只触发一次 token 刷新，所有请求共享同一个刷新 Promise。

### 14.9 WebSocket token 降级

**文件**: [auth/middleware.go](file:///d:/claude/ongrid/internal/pkg/auth/middleware.go) 第 63-65 行

浏览器原生 WebSocket 构造器无法设置请求头，auth 中间件支持 `?token=<jwt>` query 参数降级。

---

## 附录：关键文件索引

| 文件 | 中间件 | 关键行号 |
|------|--------|----------|
| [main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) | OTel + 挂载点 | 2706, 2718, 2726, 2729, 2732, 2780, 2850 |
| [tracing.go](file:///d:/claude/ongrid/internal/pkg/tracing/tracing.go) | OTel TracerProvider 初始化 | 60, 91 |
| [metrics.go](file:///d:/claude/ongrid/internal/manager/server/middleware/metrics.go) | MetricsMiddleware | 22 |
| [audit.go](file:///d:/claude/ongrid/internal/manager/server/middleware/audit.go) | AuditMiddleware | 25, 35, 72, 106, 133, 146 |
| [auth/middleware.go](file:///d:/claude/ongrid/internal/pkg/auth/middleware.go) | auth.Middleware | 21, 58 |
| [jwt.go](file:///d:/claude/ongrid/internal/pkg/auth/jwt.go) | JWT 签发/验证 | 24, 84 |
| [authzmw/middleware.go](file:///d:/claude/ongrid/internal/pkg/authzmw/middleware.go) | authzmw.Require | 38, 50, 70 |
| [tenantctx.go](file:///d:/claude/ongrid/internal/pkg/tenantctx/tenantctx.go) | tenantctx 双层存储 | 21, 33, 43, 74, 81 |
| [edgeauth/http.go](file:///d:/claude/ongrid/internal/manager/server/edgeauth/http.go) | 数据平面认证 | 31, 67 |
| [edge/http.go](file:///d:/claude/ongrid/internal/manager/server/edge/http.go) | requireAdmin + writeMW/deleteMW | 82, 115, 123, 315 |
| [knowledge/http.go](file:///d:/claude/ongrid/internal/manager/server/knowledge/http.go) | writeMW/deleteMW + passthrough | 78, 93, 100, 107 |
| [usecase.go](file:///d:/claude/ongrid/internal/manager/biz/audit/usecase.go) | audit Event + Emit | 31, 74 |
| [manager_metrics.go](file:///d:/claude/ongrid/internal/pkg/prom/manager_metrics.go) | prom.ObserveHTTP | 357, 365 |
| [App.tsx](file:///d:/claude/ongrid/web/src/App.tsx) | RequireAuth + PublicOnly | 63, 72 |
| [client.ts](file:///d:/claude/ongrid/web/src/api/client.ts) | 401 自动刷新 | 97, 117 |
| [auth.ts](file:///d:/claude/ongrid/web/src/store/auth.ts) | token 存取 | 20, 43 |
