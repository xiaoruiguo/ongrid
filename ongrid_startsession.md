# OnGrid `startSession` 完整调用链分析

> 从 `Home.tsx` 的 `startSession` 函数出发，追踪到后端 GORM 持久化的端到端全链路。
> 包含源码文件路径、关键行号、函数签名、数据流向。

---

## 目录

1. [调用链总览](#1-调用链总览)
2. [前端：Home.tsx startSession](#2-前端homettsx-startsession)
3. [前端：chat.ts createSession](#3-前端chatts-createsession)
4. [前端：client.ts request 封装](#4-前端clientts-request-封装)
5. [前端：auth.ts token 注入](#5-前端authts-token-注入)
6. [后端：中间件链](#6-后端中间件链)
7. [后端：auth.Middleware JWT 验证](#7-后端authmiddleware-jwt-验证)
8. [后端：tenantctx 身份传递](#8-后端tenantctx-身份传递)
9. [后端：http.go createSession Handler](#9-后端httpgo-createsession-handler)
10. [后端：service.go CreateSession](#10-后端servicego-createsession)
11. [后端：repo.go SessionRepo 接口](#11-后端repogo-sessionrepo-接口)
12. [后端：session.go GORM 持久化](#12-后端sessiongo-gorm-持久化)
13. [后端：model.go 数据模型](#13-后端modelgo-数据模型)
14. [后端：HTTP 响应构造](#14-后端http-响应构造)
15. [前端：导航到 ChatThread](#15-前端导航到-chatthread)
16. [端到端时序图](#16-端到端时序图)
17. [关键设计要点](#17-关键设计要点)

---

## 1. 调用链总览

```
Home.tsx startSession (L226)
  ↓ createSession({title, agent_id}) 
chat.ts createSession (L80)
  ↓ request('POST', '/chat/sessions', input)
client.ts request (L27)
  ↓ getToken() → 注入 Bearer token
  ↓ fetch POST /api/v1/chat/sessions
  ↓
HTTP 请求
  ↓
main.go 中间件链 (L2718)
  ├── otelhttpmw (OTel span)           (L2726)
  ├── MetricsMiddleware                (L2729)
  └── AuditMiddleware                  (L2732)
  ↓
auth.Middleware (L2780)
  ↓ extractBearer → signer.Verify → tenantctx.With
  ↓
aiopsHandler.Register (L2850)
  ↓ POST /v1/chat/sessions → h.createSession
http.go createSession (L307)
  ↓ callerFromCtx → svc.Caller{UserID, Role}
  ↓ h.svc.CreateSession(ctx, caller, input)
service.go CreateSession (L204)
  ↓ trim title, 构造 model.Session
  ↓ s.sessions.CreateSession(ctx, sess)
repo.go SessionRepo.CreateSession (L28)
  ↓
session.go CreateSession (L31)
  ↓ r.db.WithContext(ctx).Create(s)
  ↓ GORM → BeforeCreate 钩子生成 UUID
  ↓ INSERT INTO chat_sessions
  ← *model.Session
  ←
http.go toSessionDTO (L860)
  ↓ writeJSON(201, sessionDTO)
  ← HTTP 201 + JSON
  ←
client.ts request (L114)
  ↓ parsed as T
  ← ChatSession
  ←
Home.tsx startSession (L240)
  ↓ navigate(`/chat/${session.id}`, {state: {initialPrompt: content}})
  ↓
ChatThread.tsx (L179-190)
  ↓ useEffect 检测 initialPrompt
  ↓ send(initialPrompt, [])  ← 进入 SSE 流式链路
```

---

## 2. 前端：Home.tsx startSession

**文件**: [Home.tsx](file:///d:/claude/ongrid/web/src/pages/Home.tsx)

### 2.1 函数签名

```tsx
// 第 226-245 行
async function startSession(content: string) {
  if (!content.trim() || submitting) return;
  setError(null);
  setSubmitting(true);
  try {
    const title = content.trim().slice(0, 30);                    // L231: 截取前 30 字符做标题
    const session = await createSession({ title, agent_id: 'default' });  // L235: 创建会话
    navigate(`/chat/${session.id}`, { state: { initialPrompt: content } });  // L240: 导航 + 透传初始提问
  } catch (err) {
    setError((err as Error).message || tr('创建会话失败', 'Failed to create session'));
    setSubmitting(false);
  }
}
```

### 2.2 关键设计

- **标题截取**: `content.trim().slice(0, 30)` — 用户输入前 30 字符作为会话标题
- **agent_id: 'default'**: 绑定到虚拟 "default" persona，后端使用 unrestricted coordinator-equivalent toolBag
- **不在 Home 发消息**: 只创建 session，首条消息由 ChatThread 的 SSE 流式路径发送（用户能看到 tool cards 增量更新）
- **initialPrompt 透传**: 通过 React Router 的 `location.state` 传递，ChatThread 检测后自动触发 `send()`
- **模型选择继承**: 模型选择存储在 `useModelSelection`（[modelSelection.ts](file:///d:/claude/ongrid/web/src/store/modelSelection.ts)），跨页面共享，ChatThread 继承 Home 的选择

### 2.3 调用入口

- **PromptCard 点击** (L313): `onClick={() => void startSession(tr(p.promptZh, p.promptEn))}`
- **ChatInput 提交** (L262-264): `onSubmit={(p) => { setDraft(''); void startSession(p.text); }}`

---

## 3. 前端：chat.ts createSession

**文件**: [chat.ts](file:///d:/claude/ongrid/web/src/api/chat.ts)

```typescript
// 第 80-87 行
export function createSession(input: {
  title: string;
  scope?: string[];
  related_incident_id?: number;
  agent_id?: string;
}) {
  return request<ChatSession>('POST', '/chat/sessions', input);
}
```

**入参**: `{title, scope?, related_incident_id?, agent_id?}`

**返回类型** `ChatSession` (第 42-51 行):
```typescript
export type ChatSession = {
  id: string;              // UUID
  user_id: number;
  title: string;
  related_incident_id?: number | null;
  agent_id?: string | null;
  created_at?: string;
  updated_at?: string;
  closed_at?: string | null;
};
```

调用 `request<ChatSession>('POST', '/chat/sessions', input)`，由 `client.ts` 封装 fetch。

---

## 4. 前端：client.ts request 封装

**文件**: [client.ts](file:///d:/claude/ongrid/web/src/api/client.ts)

### 4.1 request 函数

**第 27-115 行**:

```typescript
export async function request<T = unknown>(
  method: 'GET' | 'POST' | 'PUT' | 'DELETE' | 'PATCH',
  path: string,
  body?: unknown,
  opts: RequestOpts = {}
): Promise<T> {
  // L33-41: 默认 headers
  const headers: Record<string, string> = {
    Accept: 'application/json',
    'Accept-Language': getLocale(),  // 让后端 LLM 输出跟随 UI 语言
  };

  // L43-46: 注入 Authorization
  if (!opts.noAuth) {
    const token = getToken();
    if (token) headers['Authorization'] = `Bearer ${token}`;
  }

  // L48-57: body 序列化
  if (body !== undefined && body !== null) {
    headers['Content-Type'] = 'application/json';
    payload = JSON.stringify(body);
  }

  // L59: URL 拼接
  const url = `${BASE}${path}`;  // BASE = '/api/v1' (L24)

  // L63: fetch
  res = await fetch(url, { method, headers, body: payload });

  // L69-80: 解析响应（JSON 或 text）

  // L82-111: 错误处理
  if (!res.ok) {
    // L97-110: 401 自动刷新
    if (res.status === 401 && !opts.noAuth) {
      const nextToken = await refreshAccessToken();
      if (nextToken && !opts._retryingAfterRefresh) {
        return request<T>(method, path, body, { ...opts, _retryingAfterRefresh: true });
      }
      if (!nextToken) useAuth.getState().logout();
    }
    throw new ApiError(msg, res.status, code, parsed);
  }

  // L114: 返回 parsed as T
  return parsed as T;
}
```

### 4.2 createSession 的完整 HTTP 请求

```
POST /api/v1/chat/sessions
Headers:
  Accept: application/json
  Accept-Language: zh-CN  (或 en-US)
  Content-Type: application/json
  Authorization: Bearer <JWT access token>
Body:
  {"title":"找出资源最紧张的 3 台设备","agent_id":"default"}
```

### 4.3 401 自动刷新

**第 117-162 行** `refreshAccessToken`:
- 单飞（`refreshInFlight` 去重），避免并发请求同时刷新
- 用 `getRefreshToken()` 调 `POST /api/v1/auth/refresh`
- 成功则 `useAuth.getState().setSession(...)` 更新 token
- 失败返回 null → 触发 logout

---

## 5. 前端：auth.ts token 注入

**文件**: [auth.ts](file:///d:/claude/ongrid/web/src/store/auth.ts)

```typescript
// 第 20-41 行: zustand + persist，持久化到 localStorage key "ongrid.auth"
export const useAuth = create<AuthState>()(persist(...));

// 第 43-45 行: 非 hook，供 client.ts 同步读取
export function getToken(): string | null {
  return useAuth.getState().token;
}

// 第 47-49 行
export function getRefreshToken(): string | null {
  return useAuth.getState().refreshToken;
}
```

**调用链**: `client.ts:44` → `getToken()` → `useAuth.getState().token` → 返回 localStorage 中的 JWT access token。

---

## 6. 后端：中间件链

**文件**: [main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go)

### 6.1 顶层 mux

**第 2718-2732 行**:

```go
mux := chi.NewRouter()

// L2726: OTel HTTP 中间件 — 每个请求包裹 span
mux.Use(otelhttpmw)

// L2729: ADR-026 自观测 HTTP 指标
mux.Use(managermiddleware.MetricsMiddleware)

// L2732: HLD-010 审计中间件 — 捕获变更请求 + 认证失败
mux.Use(managermiddleware.AuditMiddleware(auditUC))
```

### 6.2 /api 路由组

**第 2750 行**: `mux.Route("/api", ...)` — 所有 BC HTTP 挂载在 `/api` 下

### 6.3 protected 路由组

**第 2779-2780 行**:

```go
api.Group(func(protected chi.Router) {
    protected.Use(auth.Middleware(signer))  // JWT 验证
    // ... 所有需要认证的路由 ...
})
```

### 6.4 aiopsHandler 注册

**第 2850 行**:

```go
aiopsHandler.Register(protected)
```

这会执行 [http.go](file:///d:/claude/ongrid/internal/manager/server/aiops/http.go) 第 131-156 行的 `Register` 方法，将 `POST /v1/chat/sessions` 映射到 `h.createSession`。

### 6.5 完整中间件执行序

```
HTTP POST /api/v1/chat/sessions
  │
  ├── 1. otelhttpmw                    — 创建 OTel span
  ├── 2. MetricsMiddleware             — 记录 HTTP 指标
  ├── 3. AuditMiddleware               — 安装 tenantctx slot + 后置审计
  ├── 4. auth.Middleware(signer)       — JWT 验证 + 写入 tenantctx
  │
  └── 5. h.createSession               — 业务 handler
```

---

## 7. 后端：auth.Middleware JWT 验证

**文件**: [middleware.go](file:///d:/claude/ongrid/internal/pkg/auth/middleware.go)

### 7.1 Middleware 函数

**第 21-53 行**:

```go
func Middleware(signer *Signer) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            // L24: 提取 Bearer token
            tok := extractBearer(r)
            if tok == "" {
                http.Error(w, "missing bearer token", 401)
                return
            }

            // L29: 验证 JWT
            claims, err := signer.Verify(tok)
            if err != nil {
                http.Error(w, "invalid token", 401)
                return
            }

            // L38: 构造 Tenant
            isSuper := claims.IsSuperuser || claims.Role == "admin"
            t := tenantctx.Tenant{
                UserID:      claims.UserID,
                Email:       claims.Email,
                Role:        claims.Role,
                IsSuperuser: isSuper,
            }

            // L48: 写入可变 slot（供审计中间件读取）
            tenantctx.SetOnSlot(r.Context(), t)

            // L49: 写入 context（供下游 handler 读取）
            ctx := tenantctx.With(r.Context(), t)

            // L50: 调用下一个 handler
            next.ServeHTTP(w, r.WithContext(ctx))
        })
    }
}
```

### 7.2 extractBearer

**第 58-67 行**: 从 `Authorization: Bearer <tok>` 头提取，或 `?token=<jwt>` query 参数（WebSocket 降级）。

### 7.3 Signer.Verify

**文件**: [jwt.go](file:///d:/claude/ongrid/internal/pkg/auth/jwt.go) 第 84-99 行:

```go
func (s *Signer) Verify(token string) (*Claims, error) {
    var c Claims
    t, err := jwt.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) {
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

**关键**: Verify 只做签名/过期验证，**不查数据库**。用户身份在登录时烤进 token，token 有效期内信任。

### 7.4 Claims 结构体

**第 24-30 行**:

```go
type Claims struct {
    UserID      uint64 `json:"user_id"`
    Email       string `json:"email,omitempty"`
    Role        string `json:"role"`
    IsSuperuser bool   `json:"is_superuser,omitempty"`
    jwt.RegisteredClaims
}
```

---

## 8. 后端：tenantctx 身份传递

**文件**: [tenantctx.go](file:///d:/claude/ongrid/internal/pkg/tenantctx/tenantctx.go)

### 8.1 Tenant 结构体

**第 21-26 行**:

```go
type Tenant struct {
    UserID      uint64
    Email       string
    Role        string
    IsSuperuser bool
}
```

### 8.2 双层存储机制

| 方法 | 行号 | 说明 |
|------|------|------|
| `With(ctx, t)` | 33-35 | 标准 context.WithValue（不可变） |
| `From(ctx)` | 43-49 | 优先读 slot，再读 context value |
| `WithSlot(ctx)` | 74-76 | 安装可变 slot（由 AuditMiddleware 调用） |
| `SetOnSlot(ctx, t)` | 81-86 | 写入 slot（由 auth.Middleware 调用） |

**设计原因**: 审计中间件在外层（先执行），auth 中间件在内层（后执行）。外层的 `r` 不携带内层的 `WithContext`，通过 slot 指针让外层能看到内层写入的值。

---

## 9. 后端：http.go createSession Handler

**文件**: [http.go](file:///d:/claude/ongrid/internal/manager/server/aiops/http.go)

### 9.1 路由映射

**第 132 行**:

```go
r.Post("/v1/chat/sessions", h.createSession)
```

### 9.2 createSessionReq DTO

**第 160-171 行**:

```go
type createSessionReq struct {
    Title             string   `json:"title"`
    Scope             []string `json:"scope,omitempty"`
    RelatedIncidentID *uint64  `json:"related_incident_id,omitempty"`
    AgentID           string   `json:"agent_id,omitempty"`
}
```

### 9.3 createSession Handler

**第 307-329 行**:

```go
func (h *Handler) createSession(w http.ResponseWriter, r *http.Request) {
    // L308: 从 context 提取调用者身份
    caller, ok := callerFromCtx(r.Context())
    if !ok {
        writeErr(w, errs.ErrUnauthorized)  // 401
        return
    }

    // L313-317: 解析 JSON 请求体
    var req createSessionReq
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        writeErr(w, errors.Join(errs.ErrInvalid, err))  // 400
        return
    }

    // L318-323: 调用 service 层
    s, err := h.svc.CreateSession(r.Context(), caller, svc.CreateSessionInput{
        Title:             req.Title,
        Scope:             req.Scope,
        RelatedIncidentID: req.RelatedIncidentID,
        AgentID:           req.AgentID,
    })
    if err != nil {
        writeErr(w, err)
        return
    }

    // L328: 返回 201 + sessionDTO
    writeJSON(w, http.StatusCreated, toSessionDTO(s))
}
```

### 9.4 callerFromCtx

**第 967-973 行**:

```go
func callerFromCtx(ctx context.Context) (svc.Caller, bool) {
    t, ok := tenantctx.From(ctx)
    if !ok { return svc.Caller{}, false }
    return svc.Caller{UserID: t.UserID, Role: t.Role}, true
}
```

从 context 中的 `tenantctx.Tenant` 提取 `Caller{UserID, Role}`。

### 9.5 writeJSON / writeErr

- **writeJSON** (第 994-1001 行): `Content-Type: application/json` + `WriteHeader(code)` + `json.Encode(body)`
- **writeErr** (第 1008-1013 行): `errs.HTTPStatus(err)` 映射错误码 + `json.Encode(errorBody{Error, Code})`

---

## 10. 后端：service.go CreateSession

**文件**: [service.go](file:///d:/claude/ongrid/internal/manager/service/aiops/service.go)

### 10.1 CreateSessionInput

**第 190-198 行**:

```go
type CreateSessionInput struct {
    Title             string
    Scope             []string
    RelatedIncidentID *uint64
    AgentID           string
}
```

### 10.2 CreateSession 方法

**第 204-232 行**:

```go
func (s *Service) CreateSession(ctx context.Context, caller Caller, in CreateSessionInput) (*model.Session, error) {
    // L205-208: 标题处理
    title := strings.TrimSpace(in.Title)
    if title == "" {
        title = "Untitled"
    }

    // L209-215: 构造 Session 模型
    sess := &model.Session{
        UserID:            caller.UserID,      // 从 JWT claims 透传
        Title:             title,
        RelatedIncidentID: in.RelatedIncidentID,
        CreatedAt:         time.Now().UTC(),
        UpdatedAt:         time.Now().UTC(),
    }

    // L216-219: 绑定 persona
    if in.AgentID != "" {
        ag := in.AgentID
        sess.AgentID = &ag
    }

    // L220-227: Scope 序列化
    if len(in.Scope) > 0 {
        b, err := json.Marshal(in.Scope)
        if err != nil {
            return nil, fmt.Errorf("%w: scope marshal: %v", errs.ErrInvalid, err)
        }
        scopeStr := string(b)
        sess.ScopeJSON = &scopeStr
    }

    // L228-230: 持久化
    if err := s.sessions.CreateSession(ctx, sess); err != nil {
        return nil, fmt.Errorf("aiops service: create session: %w", err)
    }

    // L231: 返回（sess.ID 已由 GORM BeforeCreate 钩子填充 UUID）
    return sess, nil
}
```

### 10.3 Service 结构体

**第 73-89 行**:

```go
type Service struct {
    legacyAgent *agent.Agent
    runtime     RuntimeHandler
    kernel      Kernel
    sessions    biz.SessionRepo       // ← CreateSession 调用此字段
    proposals   biz.MutatingProposalRepo
    usage       *biz.UsageUsecase
    log         *slog.Logger
    cancelMu    sync.Mutex
    cancels     map[string]context.CancelFunc
}
```

### 10.4 Caller 类型

**第 136-147 行**:

```go
type Caller struct {
    UserID uint64
    Role   string
}

func (c Caller) IsAdmin() bool  { return c.Role == RoleAdmin }
func (c Caller) IsViewer() bool { return c.Role == RoleViewer }
```

### 10.5 依赖注入

**文件**: [main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) 第 1780-1782 行:

```go
aiopsSvc := managersvcaiops.NewWithKernel(aiopsAgent, aiopsRuntime, kernel, aiopsRepo, aiopsUsage, log)
aiopsSvc.SetMutatingProposalRepo(mutatingProposalRepo)
aiopsHandler := managerserveraiops.NewHandler(aiopsSvc)
```

`aiopsRepo` 是 `biz.SessionRepo` 接口实例（由 `data/aiops/store.NewBizRepo(db)` 构造）。

---

## 11. 后端：repo.go SessionRepo 接口

**文件**: [repo.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/repo.go)

**第 26-28 行**:

```go
type SessionRepo interface {
    CreateSession(ctx context.Context, s *model.Session) error
    GetSession(ctx context.Context, id string) (*model.Session, error)
    // ... 其他方法
}
```

**接口在消费方定义**（biz 层），实现在 data 层，符合依赖倒置原则。

---

## 12. 后端：session.go GORM 持久化

**文件**: [session.go](file:///d:/claude/ongrid/internal/manager/data/aiops/store/session.go)

### 12.1 SessionRepo 结构体

**第 16-18 行**:

```go
type SessionRepo struct {
    db *gorm.DB
}
```

### 12.2 编译期接口保证

**第 28 行**:

```go
var _ biz.SessionRepo = (*SessionRepo)(nil)
```

### 12.3 CreateSession 方法

**第 30-36 行**:

```go
// CreateSession inserts s.
func (r *SessionRepo) CreateSession(ctx context.Context, s *model.Session) error {
    if s == nil {
        return errs.ErrInvalid
    }
    return r.db.WithContext(ctx).Create(s).Error
}
```

GORM `Create(s)` 执行:
1. 调用 `s.BeforeCreate()` 钩子生成 UUID
2. 执行 `INSERT INTO chat_sessions (id, user_id, title, ...) VALUES (?, ?, ?, ...)`

---

## 13. 后端：model.go 数据模型

**文件**: [model.go](file:///d:/claude/ongrid/internal/manager/model/aiops/model.go)

### 13.1 Session 模型

**第 49-73 行** — 表名 `chat_sessions`:

```go
type Session struct {
    ID              string  `gorm:"primaryKey;type:char(36);column:id"`
    UserID          uint64  `gorm:"index;not null;column:user_id"`
    Title           string  `gorm:"size:256;not null"`
    ScopeJSON       *string `gorm:"type:text;column:scope_json"`
    AgentID         *string `gorm:"size:128;index;column:agent_id"`
    ParentSessionID *string `gorm:"size:36;index;column:parent_session_id"`
    Background      bool    `gorm:"not null;default:false;column:background"`
    RelatedIncidentID *uint64 `gorm:"index;column:related_incident_id"`
    Kind              string     `gorm:"size:16;not null;default:'user';column:kind"`
    CreatedAt         time.Time
    UpdatedAt         time.Time
    ClosedAt          *time.Time `gorm:"column:closed_at"`
}
```

### 13.2 TableName

**第 81 行**:

```go
func (Session) TableName() string { return "chat_sessions" }
```

### 13.3 BeforeCreate 钩子

**第 86-91 行**:

```go
func (s *Session) BeforeCreate(*gorm.DB) error {
    if s.ID == "" {
        s.ID = uuid.NewString()  // 自动生成 UUIDv4
    }
    return nil
}
```

**关键**: ID 在 GORM 插入前由 `BeforeCreate` 钩子自动填充 UUIDv4，所以 `service.go` 构造 `sess` 时不需要预设 ID，`CreateSession` 返回后 `sess.ID` 已有值。

---

## 14. 后端：HTTP 响应构造

### 14.1 toSessionDTO

**文件**: [http.go](file:///d:/claude/ongrid/internal/manager/server/aiops/http.go) 第 860-878 行:

```go
func toSessionDTO(s *model.Session) sessionDTO {
    out := sessionDTO{
        ID:                s.ID,
        UserID:            s.UserID,
        Title:             s.Title,
        RelatedIncidentID: s.RelatedIncidentID,
        AgentID:           s.AgentID,
        CreatedAt:         s.CreatedAt,
        UpdatedAt:         s.UpdatedAt,
        ClosedAt:          s.ClosedAt,
    }
    if s.ScopeJSON != nil && *s.ScopeJSON != "" {
        var scope []string
        if err := json.Unmarshal([]byte(*s.ScopeJSON), &scope); err == nil {
            out.Scope = scope
        }
    }
    return out
}
```

### 14.2 sessionDTO

**第 173-183 行**:

```go
type sessionDTO struct {
    ID                string     `json:"id"`
    UserID            uint64     `json:"user_id"`
    Title             string     `json:"title"`
    Scope             []string   `json:"scope,omitempty"`
    RelatedIncidentID *uint64    `json:"related_incident_id,omitempty"`
    AgentID           *string    `json:"agent_id,omitempty"`
    CreatedAt         time.Time  `json:"created_at"`
    UpdatedAt         time.Time  `json:"updated_at"`
    ClosedAt          *time.Time `json:"closed_at,omitempty"`
}
```

### 14.3 HTTP 响应

```
HTTP/1.1 201 Created
Content-Type: application/json

{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "user_id": 1,
  "title": "找出资源最紧张的 3 台设备",
  "agent_id": "default",
  "created_at": "2026-08-03T10:00:00Z",
  "updated_at": "2026-08-03T10:00:00Z"
}
```

---

## 15. 前端：导航到 ChatThread

### 15.1 navigate 调用

**文件**: [Home.tsx](file:///d:/claude/ongrid/web/src/pages/Home.tsx) 第 240 行:

```tsx
navigate(`/chat/${session.id}`, { state: { initialPrompt: content } });
```

React Router 导航到 `/chat/:sessionId`，通过 `location.state` 透传 `initialPrompt`。

### 15.2 ChatThread 接收 initialPrompt

**文件**: [ChatThread.tsx](file:///d:/claude/ongrid/web/src/pages/ChatThread.tsx)

**第 29-30 行**: 从 location.state 读取

```tsx
const location = useLocation();
const initialPrompt = (location.state as LocationState)?.initialPrompt;
```

**第 179-190 行**: useEffect 检测并自动发送

```tsx
useEffect(() => {
    if (loading) return;
    if (!initialPrompt || !sessionId || sentInitialRef.current) return;
    if (messages.length > 0) {
        sentInitialRef.current = true;  // 已有消息，跳过
        return;
    }
    sentInitialRef.current = true;
    void send(initialPrompt, []);  // ← 进入 SSE 流式链路
}, [loading, initialPrompt, sessionId, messages.length]);
```

**`send` 函数**（第 217 行）调用 `streamMessage`（[chat.ts:252](file:///d:/claude/ongrid/web/src/api/chat.ts#L252)），进入 SSE 流式消息发送链路（详见 [ongrid_route_chat.md](file:///d:/claude/ongrid/ongrid_route_chat.md) 第 17.2 节）。

### 15.3 模型选择继承

**文件**: [modelSelection.ts](file:///d:/claude/ongrid/web/src/store/modelSelection.ts)

```typescript
// 第 20-31 行: zustand + persist，持久化到 localStorage key "ongrid.model-selection"
export const useModelSelection = create<ModelSelectionState>()(persist(...));
```

Home 页的模型选择通过 `useModelSelection` 共享，ChatThread mount 时读取同一 store，继承 Home 的选择。

Home 页 `handleModelChange`（第 212-224 行）还会将选择持久化到服务端:

```tsx
await setSetting('llm', 'default_provider', sel.provider, false);
await setSetting('llm', `${sel.provider}_default_model`, sel.model, false);
await invalidateLLMRouter();  // 刷新后端 60s TTL 缓存
```

---

## 16. 端到端时序图

```
用户                    前端                     后端中间件              Handler/Service          Data/Model
 │                       │                         │                        │                       │
 │  输入文本+Enter       │                         │                        │                       │
 │──────────────────────>│                         │                        │                       │
 │                       │ startSession(content)   │                        │                       │
 │                       │  L226: trim+slice(0,30) │                        │                       │
 │                       │  L235: createSession()  │                        │                       │
 │                       │                         │                        │                       │
 │                       │ request('POST',         │                        │                       │
 │                       │  '/chat/sessions',      │                        │                       │
 │                       │  {title, agent_id})     │                        │                       │
 │                       │  L44: getToken()        │                        │                       │
 │                       │  L63: fetch POST        │                        │                       │
 │                       │────────────────────────>│                        │                       │
 │                       │  /api/v1/chat/sessions  │                        │                       │
 │                       │  Authorization: Bearer  │                        │                       │
 │                       │                         │                        │                       │
 │                       │                         │ 1. otelhttpmw          │                       │
 │                       │                         │    创建 OTel span       │                       │
 │                       │                         │                        │                       │
 │                       │                         │ 2. MetricsMiddleware    │                       │
 │                       │                         │    记录 HTTP 指标       │                       │
 │                       │                         │                        │                       │
 │                       │                         │ 3. AuditMiddleware      │                       │
 │                       │                         │    WithSlot(ctx)        │                       │
 │                       │                         │                        │                       │
 │                       │                         │ 4. auth.Middleware      │                       │
 │                       │                         │    extractBearer(r)     │                       │
 │                       │                         │    signer.Verify(tok)   │                       │
 │                       │                         │    tenantctx.SetOnSlot  │                       │
 │                       │                         │    tenantctx.With       │                       │
 │                       │                         │───────────────────────>│                       │
 │                       │                         │                        │                       │
 │                       │                         │            h.createSession (L307)               │
 │                       │                         │            callerFromCtx → Caller{UserID,Role} │
 │                       │                         │            json.Decode → createSessionReq      │
 │                       │                         │                        │                       │
 │                       │                         │            svc.CreateSession (L204)            │
 │                       │                         │             trim title                         │
 │                       │                         │             构造 model.Session                │
 │                       │                         │             AgentID = &"default"              │
 │                       │                         │──────────────────────────────────────────────>│
 │                       │                         │                        │                       │
 │                       │                         │                        │     sessions.CreateSession
 │                       │                         │                        │     (session.go L31)  │
 │                       │                         │                        │     db.Create(s)      │
 │                       │                         │                        │     BeforeCreate:     │
 │                       │                         │                        │       s.ID = uuid.New()│
 │                       │                         │                        │     INSERT INTO       │
 │                       │                         │                        │       chat_sessions   │
 │                       │                         │                        │<──────────────────────│
 │                       │                         │                        │     *model.Session    │
 │                       │                         │                        │     (ID 已填充)        │
 │                       │                         │            toSessionDTO (L860)                │
 │                       │                         │            writeJSON(201, dto)                │
 │                       │                         │<───────────────────────│                       │
 │                       │                         │                        │                       │
 │                       │  HTTP 201 Created       │                        │                       │
 │                       │  {"id":"uuid",...}      │                        │                       │
 │                       │<────────────────────────│                        │                       │
 │                       │                         │                        │                       │
 │                       │ parsed as ChatSession   │                        │                       │
 │                       │ navigate(`/chat/${id}`, │                        │                       │
 │                       │   {state:{initialPrompt}})│                       │                       │
 │                       │                         │                        │                       │
 │  URL 变化到 /chat/uuid│                         │                        │                       │
 │<──────────────────────│                         │                        │                       │
 │                       │                         │                        │                       │
 │  ChatThread mount     │                         │                        │                       │
 │  useEffect 检测       │                         │                        │                       │
 │  initialPrompt        │                         │                        │                       │
 │  send(initialPrompt)  │                         │                        │                       │
 │  → 进入 SSE 流式链路   │                         │                        │                       │
 │                       │                         │                        │                       │
```

---

## 17. 关键设计要点

### 17.1 Home 只创建不发消息

**文件**: [Home.tsx](file:///d:/claude/ongrid/web/src/pages/Home.tsx) 第 236-239 行

```tsx
// Don't post here — ChatThread takes the initialPrompt and runs it
// through the SSE streamMessage path so the user sees tool cards and
// the assistant reply incrementally.
```

Home 页只创建 session（REST POST），不发首条消息。首条消息由 ChatThread 的 SSE 流式路径发送，用户能看到 tool cards 和 assistant reply 增量更新。

### 17.2 agent_id: 'default' 绑定

**文件**: [Home.tsx](file:///d:/claude/ongrid/web/src/pages/Home.tsx) 第 234-235 行

Home 启动的会话绑定到虚拟 "default" persona，后端使用 unrestricted coordinator-equivalent toolBag。

### 17.3 JWT 无数据库查询

**文件**: [jwt.go](file:///d:/claude/ongrid/internal/pkg/auth/jwt.go) 第 84-99 行

`Signer.Verify` 只做签名/过期验证，不查数据库。用户身份在登录时烤进 token，token 有效期内信任。这避免了每个请求的数据库查询。

### 17.4 tenantctx 双层存储

**文件**: [tenantctx.go](file:///d:/claude/ongrid/internal/pkg/tenantctx/tenantctx.go)

`With`/`From` 是标准 context 存取；`WithSlot`/`SetOnSlot` 是可变 slot 机制。外层审计中间件（先执行）安装 slot，内层 auth 中间件（后执行）写入 slot，外层通过 `From` 优先读 slot 就能看到内层写入的值。

### 17.5 UUID 自动生成

**文件**: [model.go](file:///d:/claude/ongrid/internal/manager/model/aiops/model.go) 第 86-91 行

`Session.BeforeCreate` 钩子在 GORM 插入前自动填充 UUIDv4。Service 层构造 `sess` 时不需要预设 ID，`CreateSession` 返回后 `sess.ID` 已有值。

### 17.6 接口在消费方定义

**文件**: [repo.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/repo.go) 第 26 行

`SessionRepo` 接口在 biz 层定义（消费方），实现在 data 层（[session.go](file:///d:/claude/ongrid/internal/manager/data/aiops/store/session.go) 第 16 行）。`var _ biz.SessionRepo = (*SessionRepo)(nil)` 编译期保证实现。

### 17.7 模型选择跨页面共享

**文件**: [modelSelection.ts](file:///d:/claude/ongrid/web/src/store/modelSelection.ts)

`useModelSelection` 使用 zustand + persist 持久化到 localStorage，跨 Home/ChatThread 共享。Home 页的选择会：
1. 写入 localStorage（ChatThread mount 时读取）
2. 持久化到服务端 `system_settings`（`default_provider` + `<provider>_default_model`）
3. 调用 `invalidateLLMRouter` 刷新后端 60s TTL 缓存

### 17.8 401 自动刷新

**文件**: [client.ts](file:///d:/claude/ongrid/web/src/api/client.ts) 第 97-110 行

`request` 函数遇到 401 时自动调 `refreshAccessToken`，成功则重试一次（`_retryingAfterRefresh` 防止无限递归）。刷新失败才触发 logout。单飞（`refreshInFlight`）避免并发请求同时刷新。

---

## 附录：关键文件索引

| 文件 | 作用 | 关键行号 |
|------|------|----------|
| [Home.tsx](file:///d:/claude/ongrid/web/src/pages/Home.tsx) | startSession 入口 | 226, 235, 240 |
| [chat.ts](file:///d:/claude/ongrid/web/src/api/chat.ts) | createSession API | 80-87 |
| [client.ts](file:///d:/claude/ongrid/web/src/api/client.ts) | request 封装 + 401 刷新 | 27, 44, 63, 97, 117 |
| [auth.ts](file:///d:/claude/ongrid/web/src/store/auth.ts) | token 存取 | 20, 43 |
| [modelSelection.ts](file:///d:/claude/ongrid/web/src/store/modelSelection.ts) | 模型选择共享 store | 20 |
| [ChatThread.tsx](file:///d:/claude/ongrid/web/src/pages/ChatThread.tsx) | initialPrompt 接收 + send | 29, 179, 188 |
| [main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) | 中间件链 + 路由挂载 | 2718, 2726, 2780, 2850 |
| [middleware.go](file:///d:/claude/ongrid/internal/pkg/auth/middleware.go) | JWT auth 中间件 | 21, 24, 29, 48 |
| [jwt.go](file:///d:/claude/ongrid/internal/pkg/auth/jwt.go) | JWT 签发/验证 | 24, 84 |
| [tenantctx.go](file:///d:/claude/ongrid/internal/pkg/tenantctx/tenantctx.go) | 身份 context 传递 | 21, 33, 43, 74, 81 |
| [http.go](file:///d:/claude/ongrid/internal/manager/server/aiops/http.go) | HTTP handler + DTO | 132, 160, 307, 860, 994, 1008 |
| [service.go](file:///d:/claude/ongrid/internal/manager/service/aiops/service.go) | Service 层 | 73, 136, 190, 204 |
| [repo.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/repo.go) | SessionRepo 接口 | 26 |
| [session.go](file:///d:/claude/ongrid/internal/manager/data/aiops/store/session.go) | GORM 持久化 | 16, 28, 31 |
| [model.go](file:///d:/claude/ongrid/internal/manager/model/aiops/model.go) | Session 数据模型 | 49, 81, 86 |
