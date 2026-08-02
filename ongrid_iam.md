# OnGrid × IAM 技术实现文档

> 本文档深入分析 OnGrid 系统与 IAM（身份与访问管理）的完整代码实现，覆盖身份认证（JWT 双 token + argon2id 密码哈希）、授权（Casbin RBAC with domains）、用户/组织/成员管理、登录限流、审计、启动引导等全部代码路径。所有引用均锚定到具体文件路径与行号。

---

## 目录

1. [IAM 总览](#1-iam-总览)
2. [分层文件索引](#2-分层文件索引)
3. [架构与启动装配](#3-架构与启动装配)
4. [身份认证：JWT 双 token](#4-身份认证jwt-双-token)
5. [密码哈希：argon2id](#5-密码哈希argon2id)
6. [用户 usecase](#6-用户-usecase)
7. [组织 usecase](#7-组织-usecase)
8. [成员 usecase](#8-成员-usecase)
9. [Casbin 授权 Enforcer](#9-casbin-授权-enforcer)
10. [授权中间件 authzmw](#10-授权中间件-authzmw)
11. [认证中间件 auth.Middleware](#11-认证中间件-authmiddleware)
12. [tenantctx 上下文](#12-tenantctx-上下文)
13. [HTTP Handler](#13-http-handler)
14. [登录限流 loginThrottle](#14-登录限流-loginthrottle)
15. [审计集成](#15-审计集成)
16. [数据层](#16-数据层)
17. [启动引导 5 步](#17-启动引导-5-步)
18. [配置层](#18-配置层)
19. [并发与资源管理](#19-并发与资源管理)
20. [架构红线与设计要点](#20-架构红线与设计要点)
21. [附录：完整调用链](#21-附录完整调用链)

---

## 1. IAM 总览

### 1.1 IAM 在 OnGrid 中的角色

OnGrid 的 IAM 是一个独立的 BC（Bounded Context），位于 `internal/iam`，与 `internal/manager` 同级。它负责：

```
┌─────────────────────────────────────────────────────────────────┐
│                      OnGrid × IAM 架构                            │
├─────────────────────────────────────────────────────────────────┤
│                                                                  │
│  HTTP 层（internal/iam/server）                                   │
│  ┌────────────────────────────────────────────┐                  │
│  │ Handler                                     │                  │
│  │ /v1/auth/login      (public)                │                  │
│  │ /v1/auth/refresh    (public)                │                  │
│  │ /v1/auth/register   (admin)                 │                  │
│  │ /v1/self /v1/me     (authed)                │                  │
│  │ /v1/users/*         (admin)                 │                  │
│  │ /v1/orgs/*          (admin write/authed read)│                 │
│  │ loginThrottle (IP+email 限流)                │                 │
│  └────────────────────────────────────────────┘                  │
│                                                                  │
│  Service 层（internal/iam/service）                               │
│  ┌────────────────────────────────────────────┐                  │
│  │ Service 聚合 user/org/membership/authz      │                  │
│  │ 延迟装配：SetOrgs/SetMemberships/SetAuthz   │                  │
│  └────────────────────────────────────────────┘                  │
│                                                                  │
│  Biz 层（internal/iam/biz）                                       │
│  ┌────────────┐ ┌────────────┐ ┌────────────┐ ┌────────────┐     │
│  │ user       │ │ org        │ │ membership │ │ authz      │     │
│  │ Usecase    │ │ Service    │ │ Service    │ │ Enforcer   │     │
│  │ argon2id   │ │ CRUD+seed  │ │ N:M+casbin │ │ Casbin     │     │
│  │ JWT issue  │ │ cycle check│ │ sync       │ │ g/p rules  │     │
│  └────────────┘ └────────────┘ └────────────┘ └────────────┘     │
│                                                                  │
│  Data 层（internal/iam/data）                                     │
│  ┌────────────┐ ┌────────────┐ ┌────────────┐                    │
│  │ user/sqlite│ │ org/store  │ │ membership │                    │
│  │ GORM Repo  │ │ GORM Repo  │ │ /store     │                    │
│  └────────────┘ └────────────┘ └────────────┘                    │
│                                                                  │
│  跨 BC 共享（internal/pkg）                                       │
│  ┌────────────┐ ┌────────────┐ ┌────────────┐ ┌────────────┐     │
│  │ auth       │ │ passwd     │ │ tenantctx  │ │ authzmw    │     │
│  │ JWT Signer │ │ argon2id   │ │ ctx slot   │ │ manager MW │     │
│  │ Middleware │ │ Hash/Verify│ │ outer/inner│ │ Require()  │     │
│  └────────────┘ └────────────┘ └────────────┘ └────────────┘     │
└─────────────────────────────────────────────────────────────────┘
```

### 1.2 三个核心实体

| 实体 | 表 | 说明 |
|---|---|---|
| `User` | `users` | 登录身份，argon2id 密码哈希 |
| `Org` | `orgs` | 组织单元（扁平，1 层），可嵌套 ParentID |
| `OrgMembership` | `org_memberships` | 用户↔组织 N:M 关联，含角色 |

源码：[model.go#L87-L101](file:///d:/claude/ongrid/internal/iam/model/model.go#L87-L101)（User）、[L111-L121](file:///d:/claude/ongrid/internal/iam/model/model.go#L111-L121)（Org）、[L129-L136](file:///d:/claude/ongrid/internal/iam/model/model.go#L129-L136)（OrgMembership）。

### 1.3 三个角色层

| 层 | 常量 | 来源 | 用途 |
|---|---|---|---|
| 系统角色 | `admin` / `user` / `viewer` | `User.Role` 列 | JWT claim + 平台级权限 |
| 超级管理员 | `User.IsSuperuser` | `users.is_superuser` 列 | casbin 短路（防锁死） |
| 组织角色 | `org_admin` / `member` / `viewer` | `OrgMembership.Role` | casbin g 规则 |

源码：[model.go#L31-L67](file:///d:/claude/ongrid/internal/iam/model/model.go#L31-L67)。

### 1.4 端口分层

IAM 不独立监听端口，HTTP 路由挂在 Manager 的 `:9090` 上（路径 `/api/v1/*`）。源码：[main.go#L2236-L2356](file:///d:/claude/ongrid/cmd/ongrid/main.go#L2236-L2356)。

---

## 2. 分层文件索引

### 2.1 HTTP 层

| 文件 | 行号 | 作用 |
|---|---|---|
| [internal/iam/server/http.go](file:///d:/claude/ongrid/internal/iam/server/http.go) | L1-L437 | 认证/用户 HTTP Handler + loginThrottle |
| [internal/iam/server/orgs.go](file:///d:/claude/ongrid/internal/iam/server/orgs.go) | L1-L541 | 组织/成员/扩展用户 HTTP Handler |

### 2.2 Service 层

| 文件 | 行号 | 作用 |
|---|---|---|
| [internal/iam/service/service.go](file:///d:/claude/ongrid/internal/iam/service/service.go) | L1-L101 | Service 聚合 + 延迟装配 |

### 2.3 Biz 层

| 文件 | 行号 | 作用 |
|---|---|---|
| [internal/iam/biz/user/usecase.go](file:///d:/claude/ongrid/internal/iam/biz/user/usecase.go) | L1-L387 | 用户业务（登录/注册/JWT/CRUD） |
| [internal/iam/biz/user/hash.go](file:///d:/claude/ongrid/internal/iam/biz/user/hash.go) | L1-L16 | argon2id 薄包装 |
| [internal/iam/biz/user/repo.go](file:///d:/claude/ongrid/internal/iam/biz/user/repo.go) | L1-L29 | Repo 接口 |
| [internal/iam/biz/org/usecase.go](file:///d:/claude/ongrid/internal/iam/biz/org/usecase.go) | L1-L225 | 组织业务（CRUD+seed+cycle check） |
| [internal/iam/biz/membership/usecase.go](file:///d:/claude/ongrid/internal/iam/biz/membership/usecase.go) | L1-L88 | 成员业务（N:M + casbin sync） |
| [internal/iam/biz/authz/authz.go](file:///d:/claude/ongrid/internal/iam/biz/authz/authz.go) | L1-L315 | Casbin Enforcer |
| [internal/iam/biz/authz/model.conf](file:///d:/claude/ongrid/internal/iam/biz/authz/model.conf) | L1-L15 | Casbin RBAC with domains 模型 |

### 2.4 Data 层

| 文件 | 行号 | 作用 |
|---|---|---|
| [internal/iam/data/user/sqlite/user.go](file:///d:/claude/ongrid/internal/iam/data/user/sqlite/user.go) | L1-L150 | 用户 GORM Repo |
| [internal/iam/data/user/sqlite/migrate.go](file:///d:/claude/ongrid/internal/iam/data/user/sqlite/migrate.go) | L1-L30 | User 表迁移 |
| [internal/iam/data/org/store/repo.go](file:///d:/claude/ongrid/internal/iam/data/org/store/repo.go) | L1-L112 | 组织 GORM Repo |
| [internal/iam/data/org/store/migrate.go](file:///d:/claude/ongrid/internal/iam/data/org/store/migrate.go) | L1-L13 | Org 表迁移 |
| [internal/iam/data/membership/store/repo.go](file:///d:/claude/ongrid/internal/iam/data/membership/store/repo.go) | L1-L163 | 成员 GORM Repo |
| [internal/iam/data/membership/store/migrate.go](file:///d:/claude/ongrid/internal/iam/data/membership/store/migrate.go) | L1-L13 | OrgMembership 表迁移 |

### 2.5 Model 层

| 文件 | 行号 | 作用 |
|---|---|---|
| [internal/iam/model/model.go](file:///d:/claude/ongrid/internal/iam/model/model.go) | L1-L139 | 三个实体 + 角色常量 |

### 2.6 跨 BC 共享

| 文件 | 行号 | 作用 |
|---|---|---|
| [internal/pkg/auth/jwt.go](file:///d:/claude/ongrid/internal/pkg/auth/jwt.go) | L1-L99 | JWT Signer（HS256） |
| [internal/pkg/auth/middleware.go](file:///d:/claude/ongrid/internal/pkg/auth/middleware.go) | L1-L67 | auth.Middleware |
| [internal/pkg/passwd/argon2.go](file:///d:/claude/ongrid/internal/pkg/passwd/argon2.go) | L1-L78 | argon2id Hash/Verify |
| [internal/pkg/tenantctx/tenantctx.go](file:///d:/claude/ongrid/internal/pkg/tenantctx/tenantctx.go) | L1-L86 | Tenant ctx + mutable slot |
| [internal/pkg/authzmw/middleware.go](file:///d:/claude/ongrid/internal/pkg/authzmw/middleware.go) | L1-L97 | manager 侧 Casbin 中间件 |
| [internal/manager/server/middleware/audit.go](file:///d:/claude/ongrid/internal/manager/server/middleware/audit.go) | L1-L168 | 审计中间件（HLD-010） |

### 2.7 装配

| 文件 | 行号 | 作用 |
|---|---|---|
| [cmd/ongrid/main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) | L281-L390 | IAM 装配（5 步启动引导） |
| [cmd/ongrid/main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) | L2236-L2356 | HTTP 路由注册 |

---

## 3. 架构与启动装配

### 3.1 gospec 分层

源码：[service.go#L1-L4](file:///d:/claude/ongrid/internal/iam/service/service.go#L1-L4)

```go
// Package service is the iam BC's HTTP/gRPC handler layer. It validates
// requests, maps errors and delegates to biz/ usecases. It must never
// import internal/iam/data/** (gospec red line, enforced by go-arch-lint).
```

**红线**：Service 层禁止 import `internal/iam/data/**`，由 go-arch-lint 强制。

### 3.2 Service 延迟装配

源码：[service.go#L32-L52](file:///d:/claude/ongrid/internal/iam/service/service.go#L32-L52)

```go
func New(user *biz.Usecase, log *slog.Logger) *Service {
    return &Service{user: user, log: log}
}

func (s *Service) SetOrgs(o *org.Service) { s.orgs = o }
func (s *Service) SetMemberships(m *membership.Service) { s.memberships = m }
func (s *Service) SetAuthz(a *authz.Enforcer) { s.authz = a }
```

**关键**：Phase-1 之前的部署只有 user biz，post-Phase-1 服务（orgs/memberships/authz）可选注入。未装配时 HTTP 路由返回 503 NotWiredYet。

### 3.3 main.go 装配顺序

源码：[cmd/ongrid/main.go#L281-L390](file:///d:/claude/ongrid/cmd/ongrid/main.go#L281-L390)

```go
// 1. JWT secret 检测
const insecureJWTSecret = "dev-insecure-secret-change-me"
if cfg.JWT.Secret == insecureJWTSecret {
    log.Error("FATAL: ONGRID_JWT_SECRET is still the built-in default — refusing to start.")
    os.Exit(1)
}

// 2. user biz 装配
userRepo := iamdatauser.NewRepo(db)
signer := auth.NewSigner(cfg.JWT.Secret, cfg.JWT.AccessTTL, cfg.JWT.RefreshTTL)
userUC := iambizuser.NewUsecase(userRepo, signer, log)

// 3. BootstrapAdmin（首次启动）
userUC.BootstrapAdmin(rootCtx, cfg.Admin.Email, cfg.Admin.Password)

// 4. iam Service + Handler
iamSvc := iamservice.New(userUC, log)
iamHandler := iamserver.NewHandler(iamSvc, log)

// 5. Phase-1 启动引导（5 步，见下文）
userUC.EnsureSuperuser(rootCtx)
authzEnf, _ := iambizauthz.New(db, log)
authzEnf.SeedRolePolicies(rootCtx)
orgSvc := iambizorg.New(orgRepo, membershipRepo, authzEnf)
membershipSvc := iambizmembership.New(membershipRepo, authzEnf)
authzEnf.HydrateMemberships(rootCtx, rows)
orgSvc.EnsureSeed(rootCtx, "默认组织", "...")
iamSvc.SetOrgs(orgSvc)
iamSvc.SetMemberships(membershipSvc)
iamSvc.SetAuthz(authzEnf)

// 6. manager 侧 casbin 中间件
authzMW := authzmw.New(authzEnf, log)
```

---

## 4. 身份认证：JWT 双 token

### 4.1 Claims 结构

源码：[internal/pkg/auth/jwt.go#L24-L30](file:///d:/claude/ongrid/internal/pkg/auth/jwt.go#L24-L30)

```go
type Claims struct {
    UserID      uint64 `json:"user_id"`
    Email       string `json:"email,omitempty"`
    Role        string `json:"role"`
    IsSuperuser bool   `json:"is_superuser,omitempty"`
    jwt.RegisteredClaims
}
```

**关键**：`IsSuperuser` 独立于 `Role`，是系统管理员标志。旧 token 无此字段，解码为 false，通过 `Role=="admin"` fallback 兜底（[middleware.go#L38](file:///d:/claude/ongrid/internal/pkg/auth/middleware.go#L38)）。

### 4.2 Signer 签发

源码：[jwt.go#L33-L79](file:///d:/claude/ongrid/internal/pkg/auth/jwt.go#L33-L79)

```go
type Signer struct {
    secret     []byte
    accessTTL  time.Duration
    refreshTTL time.Duration
}

func (s *Signer) SignAccess(c Claims) (string, error) {
    return s.sign(c, s.accessTTL)
}

func (s *Signer) SignRefresh(c Claims) (string, error) {
    return s.sign(c, s.refreshTTL)
}

func (s *Signer) sign(c Claims, ttl time.Duration) (string, error) {
    now := time.Now()
    c.RegisteredClaims.IssuedAt = jwt.NewNumericDate(now)
    c.RegisteredClaims.ExpiresAt = jwt.NewNumericDate(now.Add(ttl))
    tok := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
    return tok.SignedString(s.secret)
}
```

**关键**：HS256 对称签名，secret 从 `ONGRID_JWT_SECRET` 读取。

### 4.3 Verify 验证

源码：[jwt.go#L84-L99](file:///d:/claude/ongrid/internal/pkg/auth/jwt.go#L84-L99)

```go
func (s *Signer) Verify(token string) (*Claims, error) {
    var c Claims
    t, err := jwt.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) {
        if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
            return nil, errors.New("unexpected signing method")
        }
        return s.secret, nil
    })
    if err != nil {
        return nil, err
    }
    if !t.Valid {
        return nil, errors.New("invalid token")
    }
    return &c, nil
}
```

**红线**（[jwt.go#L4-L6](file:///d:/claude/ongrid/internal/pkg/auth/jwt.go#L4-L6)）：

```go
// Red line: Verify does signature/claims validation ONLY; it does NOT look up
// the user in the iam database. User identity is baked into the access token
// at login time and trusted for the token's lifetime.
```

Verify 不查 DB，身份在登录时烤入 token，token 生命周期内信任。

### 4.4 双 token 流程

源码：[usecase.go#L362-L387](file:///d:/claude/ongrid/internal/iam/biz/user/usecase.go#L362-L387)

```go
func (u *Usecase) issuePair(user *model.User) (*TokenPair, error) {
    base := auth.Claims{
        UserID:      user.ID,
        Email:       user.Email,
        Role:        user.Role,
        IsSuperuser: user.IsSuperuser,
        RegisteredClaims: jwt.RegisteredClaims{
            Subject: fmt.Sprintf("%d", user.ID),
        },
    }
    access, err := u.signer.SignAccess(base)
    refresh, err := u.signer.SignRefresh(base)
    return &TokenPair{
        AccessToken:  access,
        RefreshToken: refresh,
        ExpiresIn:    int64(u.signer.AccessTTL() / time.Second),
        Role:         user.Role,
        UserID:       user.ID,
    }, nil
}
```

**关键**：access 和 refresh 用相同 Claims，仅 TTL 不同（access 15m / refresh 168h 或生产 720h）。

### 4.5 Refresh 不轮换

源码：[usecase.go#L102-L123](file:///d:/claude/ongrid/internal/iam/biz/user/usecase.go#L102-L123)

```go
// Refresh verifies a refresh token and issues a new access/refresh pair.
// MVP: no rotation / revocation list; signature validity is enough.
func (u *Usecase) Refresh(ctx context.Context, refreshToken string) (*TokenPair, error) {
    claims, err := u.signer.Verify(refreshToken)
    // ...
    user, err := u.repo.GetByID(ctx, claims.UserID)
    if user.Status != model.StatusActive {
        return nil, errs.ErrUnauthorized
    }
    return u.issuePair(user)
}
```

**关键**：MVP 无 token 轮换/撤销列表，签名有效即可。disabled 用户 refresh 会被拒。

---

## 5. 密码哈希：argon2id

### 5.1 参数

源码：[internal/pkg/passwd/argon2.go#L18-L24](file:///d:/claude/ongrid/internal/pkg/passwd/argon2.go#L18-L24)

```go
const (
    argonTime    uint32 = 1
    argonMemory  uint32 = 64 * 1024 // 64 MiB
    argonThreads uint8  = 4
    argonSaltLen uint32 = 16
    argonKeyLen  uint32 = 32
)
```

**关键**：64 MiB memory / 4 threads / 1 iteration，约 60ms/ hash（2023 M-series Mac）。

### 5.2 PHC 编码格式

源码：[argon2.go#L36-L45](file:///d:/claude/ongrid/internal/pkg/passwd/argon2.go#L36-L45)

```go
encoded := fmt.Sprintf(
    "$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
    argon2.Version,
    argonMemory, argonTime, argonThreads,
    base64.RawStdEncoding.EncodeToString(salt),
    base64.RawStdEncoding.EncodeToString(sum),
)
```

**格式**：`$argon2id$v=19$m=65536,t=1,p=4$<salt-b64>$<hash-b64>`

参数编码在 hash 中，改参数不失效旧 hash。

### 5.3 常数时间比较

源码：[argon2.go#L76-L77](file:///d:/claude/ongrid/internal/pkg/passwd/argon2.go#L76-L77)

```go
got := argon2.IDKey([]byte(plain), salt, t, mem, p, uint32(len(want)))
return subtle.ConstantTimeCompare(got, want) == 1
```

**关键**：`subtle.ConstantTimeCompare` 防止时序侧信道。Verify 任何解码错误都返回 false，不区分哪一步失败。

### 5.4 跨 BC 共享

源码：[hash.go#L1-L16](file:///d:/claude/ongrid/internal/iam/biz/user/hash.go#L1-L16)

```go
// hashPassword is a thin wrapper around passwd.Hash. The argon2id helpers
// were promoted to internal/pkg/passwd so manager/biz/edge can reuse the
// same scheme for its SecretKeyHash without crossing the iam BC boundary
// (arch-lint forbids manager -> iam imports).
func hashPassword(password string) (string, error) {
    return passwd.Hash(password)
}
```

**关键**：argon2id 提升到 `internal/pkg/passwd`，避免 manager → iam 跨 BC import。

---

## 6. 用户 usecase

### 6.1 Register 注册

源码：[usecase.go#L43-L78](file:///d:/claude/ongrid/internal/iam/biz/user/usecase.go#L43-L78)

```go
func (u *Usecase) Register(ctx context.Context, email, password, role string) (*model.User, error) {
    email = strings.TrimSpace(strings.ToLower(email))
    if email == "" || password == "" {
        return nil, fmt.Errorf("%w: email and password required", errs.ErrInvalid)
    }
    if role == "" {
        role = model.RoleUser
    }
    if !model.IsValidRole(role) {
        return nil, fmt.Errorf("%w: unknown role %q", errs.ErrInvalid, role)
    }
    if existing, err := u.repo.GetByEmail(ctx, email); err == nil && existing != nil {
        return nil, fmt.Errorf("%w: email already registered", errs.ErrConflict)
    }
    ph, err := hashPassword(password)
    user := &model.User{
        Email:    email,
        PassHash: ph,
        Role:     role,
        Status:   model.StatusActive,
    }
    if err := u.repo.Create(ctx, user); err != nil {
        return nil, fmt.Errorf("create user: %w", err)
    }
    user.PassHash = "" // Never echo the hash back.
    return user, nil
}
```

### 6.2 Login 登录

源码：[usecase.go#L80-L100](file:///d:/claude/ongrid/internal/iam/biz/user/usecase.go#L80-L100)

```go
func (u *Usecase) Login(ctx context.Context, email, password string) (*TokenPair, error) {
    email = strings.TrimSpace(strings.ToLower(email))
    user, err := u.repo.GetByEmail(ctx, email)
    if err != nil {
        if errors.Is(err, errs.ErrNotFound) {
            return nil, errs.ErrUnauthorized
        }
        return nil, err
    }
    if user.Status != model.StatusActive {
        return nil, errs.ErrUnauthorized
    }
    if !verifyPassword(password, user.PassHash) {
        return nil, errs.ErrUnauthorized
    }
    return u.issuePair(user)
}
```

**关键**：用户不存在 / disabled / 密码错都返回 `ErrUnauthorized`，不泄露具体原因。

### 6.3 BootstrapAdmin 首次管理员

源码：[usecase.go#L127-L164](file:///d:/claude/ongrid/internal/iam/biz/user/usecase.go#L127-L164)

```go
func (u *Usecase) BootstrapAdmin(ctx context.Context, email, password string) error {
    email = strings.TrimSpace(strings.ToLower(email))
    if email == "" || password == "" {
        return nil
    }
    n, err := u.repo.Count(ctx)
    if n > 0 {
        u.log.Info("bootstrap admin skipped; users table non-empty", "existing", n)
        return nil
    }
    ph, err := hashPassword(password)
    user := &model.User{
        Email:        email,
        DisplayName:  "admin",
        PassHash:     ph,
        Role:         model.RoleAdmin,
        IsSuperuser:  true,
        Status:       model.StatusActive,
    }
    return u.repo.Create(ctx, user)
}
```

**关键**：users 表非空时跳过（幂等）。DisplayName 默认 "admin"。

### 6.4 SetRole 角色+超级管理员同步

源码：[usecase.go#L196-L208](file:///d:/claude/ongrid/internal/iam/biz/user/usecase.go#L196-L208)

```go
func (u *Usecase) SetRole(ctx context.Context, id uint64, role string) error {
    if !model.IsValidRole(role) {
        return fmt.Errorf("%w: unknown role %q", errs.ErrInvalid, role)
    }
    if err := u.repo.UpdateRole(ctx, id, role); err != nil {
        return err
    }
    // Best-effort: keep is_superuser in sync so casbin short-circuit stays correct.
    _ = u.repo.UpdateSuperuser(ctx, id, role == model.RoleAdmin)
    return nil
}
```

**关键**：admin ⇔ superuser 同步，casbin 中间件仍读 `is_superuser` 做短路。

### 6.5 EnsureSuperuser 迁移

源码：[usecase.go#L323-L360](file:///d:/claude/ongrid/internal/iam/biz/user/usecase.go#L323-L360)

```go
func (u *Usecase) EnsureSuperuser(ctx context.Context) error {
    users, err := u.repo.List(ctx)
    for _, user := range users {
        if user.Role == model.RoleAdmin && !user.IsSuperuser {
            u.repo.UpdateSuperuser(ctx, user.ID, true)
        }
        // Display-name backfill
        if strings.TrimSpace(user.DisplayName) == "" {
            derived := user.Email
            if at := strings.Index(user.Email, "@"); at > 0 {
                derived = user.Email[:at]
            }
            u.repo.UpdateProfile(ctx, user.ID, derived, user.Phone)
        }
    }
    return nil
}
```

**关键**：启动时把 legacy admin 迁移到 `is_superuser=true`，并 backfill 空 display_name 为 email local-part。

### 6.6 PassHash 永不回传

源码：[usecase.go#L75-L76](file:///d:/claude/ongrid/internal/iam/biz/user/usecase.go#L75-L76), [L172](file:///d:/claude/ongrid/internal/iam/biz/user/usecase.go#L172), [L183](file:///d:/claude/ongrid/internal/iam/biz/user/usecase.go#L183)

```go
user.PassHash = "" // Never echo the hash back.
```

GetByID / List / Register / Create 都清空 PassHash 后返回。

---

## 7. 组织 usecase

### 7.1 单根节点不变量

源码：[org/usecase.go#L69-L104](file:///d:/claude/ongrid/internal/iam/biz/org/usecase.go#L69-L104)

```go
// Top-level uniqueness invariant: the seed org ("默认组织") is the ONE
// AND ONLY top-level org. Any new org without an explicit parent is
// auto-reparented under the seed so the org tree always has a single
// root.
func (s *Service) Create(ctx context.Context, in CreateInput) (*model.Org, error) {
    name := strings.TrimSpace(in.Name)
    parent := in.ParentID
    if parent != nil {
        if _, err := s.repo.GetByID(ctx, *parent); err != nil {
            return nil, fmt.Errorf("%w: parent org not found", errs.ErrInvalid)
        }
    } else {
        // No parent supplied → pin under the seed org
        seed, serr := s.repo.GetByName(ctx, defaultSeedName)
        if serr == nil && seed != nil {
            seedID := seed.ID
            parent = &seedID
        }
    }
    // ...
}
```

**关键**：无显式 parent 的 Create 自动挂到 "默认组织" 下，保证单根。

### 7.2 cycle check

源码：[org/usecase.go#L174-L188](file:///d:/claude/ongrid/internal/iam/biz/org/usecase.go#L174-L188)

```go
// Cycle check: walk the candidate parent's ancestors back to root;
// if id appears anywhere, the move would create a loop.
cur := *in.ParentID
for hop := 0; hop < 1024; hop++ {
    ancestor, err := s.repo.GetByID(ctx, cur)
    if err != nil {
        break
    }
    if ancestor.ID == id {
        return nil, fmt.Errorf("%w: cycle detected; cannot reparent under a descendant", errs.ErrInvalid)
    }
    if ancestor.ParentID == nil {
        break
    }
    cur = *ancestor.ParentID
}
```

**关键**：reparent 时向上走 1024 跳，防止环。

### 7.3 EnsureSeed 幂等

源码：[org/usecase.go#L118-L131](file:///d:/claude/ongrid/internal/iam/biz/org/usecase.go#L118-L131)

```go
func (s *Service) EnsureSeed(ctx context.Context, name, description string) (*model.Org, error) {
    if existing, err := s.repo.GetByName(ctx, name); err == nil {
        return existing, nil
    }
    o := &model.Org{Name: name, Description: description}
    if err := s.repo.Create(ctx, o); err != nil {
        // race-safe re-fetch
        if again, err2 := s.repo.GetByName(ctx, name); err2 == nil {
            return again, nil
        }
        return nil, err
    }
    return o, nil
}
```

**关键**：竞态安全——Create 失败后 re-fetch，处理并发首次创建。

### 7.4 Delete 拒绝有子节点

源码：[org/usecase.go#L203-L220](file:///d:/claude/ongrid/internal/iam/biz/org/usecase.go#L203-L220)

```go
func (s *Service) Delete(ctx context.Context, id uint64) error {
    if n, err := s.repo.CountChildren(ctx, id); err != nil {
        return fmt.Errorf("count children: %w", err)
    } else if n > 0 {
        return fmt.Errorf("%w: org has %d sub-org(s); move them first", errs.ErrInvalid, n)
    }
    if s.memberships != nil {
        s.memberships.DeleteByOrg(ctx, id)
    }
    if s.authz != nil {
        s.authz.RevokeAllForOrg(ctx, id)
    }
    return s.repo.Delete(ctx, id)
}
```

**关键**：有子组织时拒绝删除（保守策略），先清 memberships + casbin g 规则。

---

## 8. 成员 usecase

### 8.1 AddOrUpdate + casbin sync

源码：[membership/usecase.go#L46-L60](file:///d:/claude/ongrid/internal/iam/biz/membership/usecase.go#L46-L60)

```go
func (s *Service) AddOrUpdate(ctx context.Context, userID, orgID uint64, role string) (*model.OrgMembership, error) {
    if !model.IsValidMembershipRole(role) {
        return nil, fmt.Errorf("%w: invalid role %q", errs.ErrInvalid, role)
    }
    row, err := s.repo.Upsert(ctx, userID, orgID, role)
    if err != nil {
        return nil, err
    }
    if s.authz != nil {
        if err := s.authz.SyncMembership(ctx, userID, orgID, role); err != nil {
            return nil, fmt.Errorf("sync casbin: %w", err)
        }
    }
    return row, nil
}
```

**关键**：每次 mutation 都 sync casbin g 规则，truth table 与 `casbin_rule` 不漂移。

### 8.2 Remove + casbin revoke

源码：[membership/usecase.go#L63-L73](file:///d:/claude/ongrid/internal/iam/biz/membership/usecase.go#L63-L73)

```go
func (s *Service) Remove(ctx context.Context, userID, orgID uint64) error {
    if err := s.repo.Delete(ctx, userID, orgID); err != nil {
        return err
    }
    if s.authz != nil {
        if err := s.authz.RevokeMembership(ctx, userID, orgID); err != nil {
            return fmt.Errorf("revoke casbin: %w", err)
        }
    }
    return nil
}
```

---

## 9. Casbin 授权 Enforcer

### 9.1 RBAC with domains 模型

源码：[model.conf](file:///d:/claude/ongrid/internal/iam/biz/authz/model.conf)

```ini
[request_definition]
r = sub, dom, obj, act

[policy_definition]
p = sub, dom, obj, act

[role_definition]
g = _, _, _

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = g(r.sub, p.sub, r.dom) && (p.dom == "*" || r.dom == p.dom) && (p.obj == "*" || keyMatch(r.obj, p.obj)) && (p.act == "*" || r.act == p.act)
```

**关键**：
- `r = sub, dom, obj, act`：四元组（用户、组织、对象、动作）
- `g = _, _, _`：三元组分组（用户、角色、组织）
- `keyMatch`：对象支持通配符（`edge:*` 匹配 `edge:create`）

### 9.2 硬编码角色策略

源码：[authz.go#L90-L110](file:///d:/claude/ongrid/internal/iam/biz/authz/authz.go#L90-L110)

```go
var rolePolicies = [][]string{
    // org_admin: full inside their domain, plus member management.
    {iammodel.MembershipRoleAdmin, "*", "org:*", "*"},
    {iammodel.MembershipRoleAdmin, "*", "member:*", "*"},
    {iammodel.MembershipRoleAdmin, "*", "*", "*"},

    // member: read + write + exec on resources, no member management.
    {iammodel.MembershipRoleMember, "*", "*", "read"},
    {iammodel.MembershipRoleMember, "*", "*", "write"},
    {iammodel.MembershipRoleMember, "*", "device:shell", "exec"},

    // viewer: read only. No device:shell access.
    {iammodel.MembershipRoleViewer, "*", "*", "read"},

    // superuser fallback (defense in depth)
    {"superuser", "*", "*", "*"},
}
```

**关键**：
- `org_admin` 全权限（含成员管理）
- `member` 读写 + shell exec，但不能管成员
- `viewer` 只读，无 shell
- `superuser` 兜底策略（正常走中间件短路，防 future 路径绕过）

### 9.3 SeedRolePolicies 幂等注入

源码：[authz.go#L114-L125](file:///d:/claude/ongrid/internal/iam/biz/authz/authz.go#L114-L125)

```go
func (a *Enforcer) SeedRolePolicies(ctx context.Context) error {
    a.mu.Lock()
    defer a.mu.Unlock()
    for _, p := range rolePolicies {
        ok, err := a.e.AddPolicy(p[0], p[1], p[2], p[3])
        if err != nil {
            return fmt.Errorf("authz: add policy %v: %w", p, err)
        }
        _ = ok // false = already present
    }
    return nil
}
```

**关键**：`AddPolicy` 对重复项返回 false（no-op），启动每次都安全调用。

### 9.4 HydrateMemberships 启动回填

源码：[authz.go#L134-L143](file:///d:/claude/ongrid/internal/iam/biz/authz/authz.go#L134-L143)

```go
func (a *Enforcer) HydrateMemberships(ctx context.Context, ms []iammodel.OrgMembership) error {
    a.mu.Lock()
    defer a.mu.Unlock()
    for _, m := range ms {
        if _, err := a.e.AddGroupingPolicy(uidStr(m.UserID), m.Role, oidStr(m.OrgID)); err != nil {
            return fmt.Errorf("authz: hydrate membership %d: %w", m.ID, err)
        }
    }
    return nil
}
```

**关键**：启动时把所有 OrgMembership 行镜像为 casbin g 规则。重复 Add 是 no-op。

### 9.5 SyncMembership 增量同步

源码：[authz.go#L149-L169](file:///d:/claude/ongrid/internal/iam/biz/authz/authz.go#L149-L169)

```go
func (a *Enforcer) SyncMembership(ctx context.Context, userID, orgID uint64, role string) error {
    a.mu.Lock()
    defer a.mu.Unlock()
    uid, oid := uidStr(userID), oidStr(orgID)
    // Strip any existing g (user, *, org) row first.
    groupings, err := a.e.GetFilteredGroupingPolicy(0, uid, "", oid)
    for _, g := range groupings {
        if len(g) >= 3 && g[2] == oid {
            a.e.RemoveGroupingPolicy(g[0], g[1], g[2])
        }
    }
    if _, err := a.e.AddGroupingPolicy(uid, role, oid); err != nil {
        return fmt.Errorf("authz: add grouping: %w", err)
    }
    return nil
}
```

**关键**：casbin 不隐式替换，需先 Remove 旧 g 规则再 Add 新的。

### 9.6 Allow / AllowAnyOrg

源码：[authz.go#L235-L271](file:///d:/claude/ongrid/internal/iam/biz/authz/authz.go#L235-L271)

```go
func (a *Enforcer) Allow(ctx context.Context, userID, orgID uint64, obj, act string) bool {
    ok, err := a.e.Enforce(uidStr(userID), oidStr(orgID), obj, act)
    if err != nil {
        a.l.Warn("authz: enforce error", ...)
        return false
    }
    return ok
}

func (a *Enforcer) AllowAnyOrg(ctx context.Context, userID uint64, obj, act string) bool {
    orgs, err := a.userDomains(userID)
    for _, oidStr := range orgs {
        ok, err := a.e.Enforce(uidStr(userID), oidStr, obj, act)
        if ok {
            return true
        }
    }
    return false
}
```

**关键**：
- `Allow`：指定 org 的精确判定
- `AllowAnyOrg`：遍历用户所有 org，任一允许即通过（Phase-1 默认，无 X-Active-Org header）
- 错误一律 deny + log

### 9.7 错误一律 deny

源码：[authz.go#L237-L246](file:///d:/claude/ongrid/internal/iam/biz/authz/authz.go#L237-L246)

```go
if err != nil {
    a.l.Warn("authz: enforce error", ...)
    return false
}
```

**红线**：Enforce 出错时 deny，不 fail-open。

---

## 10. 授权中间件 authzmw

### 10.1 Require 五步解析

源码：[internal/pkg/authzmw/middleware.go#L70-L96](file:///d:/claude/ongrid/internal/pkg/authzmw/middleware.go#L70-L96)

```go
func (m *Middleware) Require(obj, act string) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            // 1. No tenant → 401
            t, ok := tenantctx.From(r.Context())
            if !ok {
                http.Error(w, errs.ErrUnauthorized.Error(), http.StatusUnauthorized)
                return
            }
            // 2. Superuser → bypass
            if t.IsSuperuser {
                next.ServeHTTP(w, r)
                return
            }
            // 3. Authorizer nil → bypass (legacy/test)
            if m.z == nil {
                next.ServeHTTP(w, r)
                return
            }
            // 4. AllowAnyOrg → allow
            if m.z.AllowAnyOrg(r.Context(), t.UserID, obj, act) {
                next.ServeHTTP(w, r)
                return
            }
            // 5. Otherwise → 403
            m.log.Info("authz: denied", ...)
            http.Error(w, errs.ErrForbidden.Error(), http.StatusForbidden)
        })
    }
}
```

### 10.2 超级管理员短路

源码：[middleware.go#L1-L5](file:///d:/claude/ongrid/internal/pkg/authzmw/middleware.go#L1-L5)

```go
// Package authzmw is the manager-side casbin authorization middleware.
// It wraps the iam authz.Enforcer in a chi-friendly handler decorator
// while keeping a hard short-circuit for superusers — corrupt casbin
// policies can never lock the system administrator out of the box.
```

**红线**：超级管理员短路 casbin，防 corrupt policy 锁死系统管理员。

### 10.3 对象命名约定

源码：[middleware.go#L11-L22](file:///d:/claude/ongrid/internal/pkg/authzmw/middleware.go#L11-L22)

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

动作词汇：`read` / `write` / `delete` / `manage`。

### 10.4 Authorizer 接口解耦

源码：[middleware.go#L38-L41](file:///d:/claude/ongrid/internal/pkg/authzmw/middleware.go#L38-L41)

```go
type Authorizer interface {
    Allow(ctx context.Context, userID, orgID uint64, obj, act string) bool
    AllowAnyOrg(ctx context.Context, userID uint64, obj, act string) bool
}
```

**关键**：接口定义在 authzmw，避免 import iam BC。

---

## 11. 认证中间件 auth.Middleware

### 11.1 三步流程

源码：[internal/pkg/auth/middleware.go#L21-L53](file:///d:/claude/ongrid/internal/pkg/auth/middleware.go#L21-L53)

```go
func Middleware(signer *Signer) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            // 1. 提取 token
            tok := extractBearer(r)
            if tok == "" {
                http.Error(w, "missing bearer token", http.StatusUnauthorized)
                return
            }
            // 2. 验证 JWT
            claims, err := signer.Verify(tok)
            if err != nil {
                http.Error(w, "invalid token", http.StatusUnauthorized)
                return
            }
            // 3. 写 tenantctx
            isSuper := claims.IsSuperuser || claims.Role == "admin"
            t := tenantctx.Tenant{
                UserID:      claims.UserID,
                Email:       claims.Email,
                Role:        claims.Role,
                IsSuperuser: isSuper,
            }
            tenantctx.SetOnSlot(r.Context(), t)
            ctx := tenantctx.With(r.Context(), t)
            next.ServeHTTP(w, r.WithContext(ctx))
        })
    }
}
```

### 11.2 WebSocket token fallback

源码：[middleware.go#L58-L67](file:///d:/claude/ongrid/internal/pkg/auth/middleware.go#L58-L67)

```go
func extractBearer(r *http.Request) string {
    const prefix = "Bearer "
    if h := r.Header.Get("Authorization"); strings.HasPrefix(h, prefix) {
        return strings.TrimPrefix(h, prefix)
    }
    if q := r.URL.Query().Get("token"); q != "" {
        return q
    }
    return ""
}
```

**关键**：浏览器原生 WebSocket 不能设 header，fallback 到 `?token=<jwt>` query string。

### 11.3 旧 token 兼容

源码：[middleware.go#L34-L44](file:///d:/claude/ongrid/internal/pkg/auth/middleware.go#L34-L44)

```go
// IsSuperuser comes from the JWT claim when present; old
// tokens (pre-claim) fall back to Role=="admin"
isSuper := claims.IsSuperuser || claims.Role == "admin"
```

**关键**：升级前的 token（无 `is_superuser` 字段）通过 `Role=="admin"` 兜底，保留全权限。

### 11.4 不查 DB

源码：[middleware.go#L18-L20](file:///d:/claude/ongrid/internal/pkg/auth/middleware.go#L18-L20)

```go
// No DB lookup is performed. Per-route role checks live in the
// iam / manager HTTP handlers which have access to tenantctx.From.
```

**红线**：中间件不查 DB，身份信任 JWT claim。

---

## 12. tenantctx 上下文

### 12.1 Tenant 结构

源码：[internal/pkg/tenantctx/tenantctx.go#L21-L26](file:///d:/claude/ongrid/internal/pkg/tenantctx/tenantctx.go#L21-L26)

```go
type Tenant struct {
    UserID      uint64
    Email       string
    Role        string
    IsSuperuser bool
}
```

### 12.2 mutable slot 模式

源码：[tenantctx.go#L51-L86](file:///d:/claude/ongrid/internal/pkg/tenantctx/tenantctx.go#L51-L86)

```go
// Mirrors the auditSlot pattern in the audit middleware: a *slot
// pointer in the OUTER request context lets outer code see what an
// inner middleware wrote, even though the inner's r.WithContext()
// produced a new ctx that the outer didn't capture.
type slotKey struct{}
type slot struct {
    t   Tenant
    set bool
}

func WithSlot(ctx context.Context) context.Context {
    return context.WithValue(ctx, slotKey{}, &slot{})
}

func SetOnSlot(ctx context.Context, t Tenant) {
    if s, ok := ctx.Value(slotKey{}).(*slot); ok && s != nil {
        s.t = t
        s.set = true
    }
}

func From(ctx context.Context) (Tenant, bool) {
    if s, ok := ctx.Value(slotKey{}).(*slot); ok && s != nil && s.set {
        return s.t, true
    }
    t, ok := ctx.Value(ctxKey{}).(Tenant)
    return t, ok
}
```

**关键**：
- audit 中间件（outer）`WithSlot` 装 slot 指针
- auth 中间件（inner）`SetOnSlot` 写入
- audit post-handler `From` 优先读 slot
- 解决 inner `r.WithContext()` 后 outer 看不到 tenant 的问题

### 12.3 装配链

```
AuditMiddleware（outer）
  ↓ WithSlot(ctx) 装 slot
  ↓ next.ServeHTTP
auth.Middleware（inner）
  ↓ verify JWT
  ↓ SetOnSlot(ctx, t)
  ↓ ctx = With(ctx, t)
  ↓ next.ServeHTTP(r.WithContext(ctx))
handler
  ↓ tenantctx.From(ctx) 读 Tenant
AuditMiddleware post-handler
  ↓ enrichFromRequest 读 slot（看到 auth 写的值）
```

---

## 13. HTTP Handler

### 13.1 路由注册

源码：[http.go#L152-L183](file:///d:/claude/ongrid/internal/iam/server/http.go#L152-L183)

```go
// RegisterPublic attaches routes that DO NOT require an auth token.
func (h *Handler) RegisterPublic(r chi.Router) {
    r.Post("/v1/auth/login", h.login)
    r.Post("/v1/auth/refresh", h.refresh)
}

// RegisterProtected attaches routes that require a valid JWT.
func (h *Handler) RegisterProtected(r chi.Router) {
    r.Post("/v1/auth/register", h.register)
    r.Get("/v1/self", h.self)
    r.Get("/v1/me", h.me) // Phase-1: enriched self with memberships.
    r.Get("/v1/users", h.listUsers)
    r.Post("/v1/users", h.createUser)
    r.Patch("/v1/users/{id}", h.updateUser)
    r.Patch("/v1/users/{id}/role", h.setRole)
    r.Patch("/v1/users/{id}/password", h.resetPassword)
    r.Delete("/v1/users/{id}", h.deleteUser)

    r.Get("/v1/orgs", h.listOrgs)
    r.Post("/v1/orgs", h.createOrg)
    r.Patch("/v1/orgs/{id}", h.updateOrg)
    r.Delete("/v1/orgs/{id}", h.deleteOrg)
    r.Get("/v1/orgs/{id}/members", h.listOrgMembers)
    r.Post("/v1/orgs/{id}/members", h.addOrgMember)
    r.Patch("/v1/orgs/{id}/members/{user_id}", h.updateOrgMember)
    r.Delete("/v1/orgs/{id}/members/{user_id}", h.removeOrgMember)
}
```

### 13.2 main.go 路由挂载

源码：[main.go#L2236-L2356](file:///d:/claude/ongrid/cmd/ongrid/main.go#L2236-L2356)

```go
mux.Route("/api", func(api chi.Router) {
    iamHandler.RegisterPublic(api)  // L2237

    api.Group(func(protected chi.Router) {
        protected.Use(auth.Middleware(signer))  // L2266
        // ...
        iamHandler.RegisterProtected(protected)  // L2326
        // ...其他 BC handlers
    })
})
```

### 13.3 requireAdmin helper

源码：[http.go#L397-L408](file:///d:/claude/ongrid/internal/iam/server/http.go#L397-L408)

```go
func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
    t, ok := tenantctx.From(r.Context())
    if !ok {
        writeErr(w, errs.ErrUnauthorized)
        return false
    }
    if t.Role != model.RoleAdmin {
        writeErr(w, errs.ErrForbidden)
        return false
    }
    return true
}
```

**关键**：iam 自己的 admin 检查走 `t.Role == "admin"`，不经过 casbin（iam 路由是平台级管理）。

### 13.4 /v1/me 富化

源码：[orgs.go#L176-L206](file:///d:/claude/ongrid/internal/iam/server/orgs.go#L176-L206)

```go
func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
    t, ok := tenantctx.From(r.Context())
    u, err := h.svc.GetByID(r.Context(), t.UserID)
    out := meDTO{
        ID:          u.ID,
        Email:       u.Email,
        DisplayName: u.DisplayName,
        Phone:       u.Phone,
        Role:        u.Role,
        Status:      u.Status,
        Memberships: []orgMembershipForUserDTO{},
    }
    if rows, err := h.svc.MembershipsByUser(r.Context(), t.UserID); err == nil {
        for _, m := range rows {
            out.Memberships = append(out.Memberships, orgMembershipForUserDTO{
                OrgID:   m.OrgID,
                OrgName: m.Org.Name,
                Role:    m.Role,
            })
        }
    }
    writeJSON(w, http.StatusOK, out)
}
```

**关键**：`/v1/me` 比 `/v1/self` 多返回 memberships，供 SPA 渲染组织切换器。

### 13.5 createUser 自动加入默认组织

源码：[orgs.go#L431-L469](file:///d:/claude/ongrid/internal/iam/server/orgs.go#L431-L469)

```go
func (h *Handler) createUser(w http.ResponseWriter, r *http.Request) {
    // ...
    u, err := h.svc.User().Create(r.Context(), user.CreateInput{...})
    // Auto-join "默认组织" as member
    if !in.SkipDefaultOrg {
        if orgs := h.svc.Orgs(); orgs != nil {
            ms := h.svc.Memberships()
            if seed, err := orgs.EnsureSeed(r.Context(), "默认组织", ""); err == nil && seed != nil && ms != nil {
                if _, mErr := ms.AddOrUpdate(r.Context(), u.ID, seed.ID, "member"); mErr != nil {
                    h.log.Warn("iam: auto-join default org", ...)
                }
            }
        }
    }
    writeJSON(w, http.StatusCreated, toFullUserDTO(u))
}
```

**关键**：新建用户默认加入 "默认组织" 为 member，否则 casbin 拒绝所有动作。

---

## 14. 登录限流 loginThrottle

### 14.1 双维度限流

源码：[http.go#L43-L52](file:///d:/claude/ongrid/internal/iam/server/http.go#L43-L52)

```go
const (
    // IP-level limit: looser, since one IP may legitimately host multiple users (NAT).
    loginIPLimit       = 20
    loginIPWindow      = 5 * time.Minute
    // Email-level limit: tighter — anyone trying 6+ passwords against admin@x in 15min is hostile.
    loginEmailLimit  = 6
    loginEmailWindow = 15 * time.Minute
)
```

**关键**：
- IP 维度：20 次/5 分钟（NAT 友好）
- Email 维度：6 次/15 分钟（紧）
- 任一维度触发即拒绝

### 14.2 check 不消费 slot

源码：[http.go#L65-L76](file:///d:/claude/ongrid/internal/iam/server/http.go#L65-L76)

```go
// check does NOT consume a slot — callers invoke recordFailure after
// the auth check, so successful logins don't burn budget.
func (t *loginThrottle) check(ip, email string) error {
    now := time.Now()
    t.mu.Lock()
    defer t.mu.Unlock()
    if exceeded(t.byIP[ip], now, loginIPLimit, loginIPWindow) {
        return errs.ErrTooManyAttempts
    }
    if exceeded(t.byEmail[email], now, loginEmailLimit, loginEmailWindow) {
        return errs.ErrTooManyAttempts
    }
    return nil
}
```

### 14.3 recordSuccess 只清 email

源码：[http.go#L92-L96](file:///d:/claude/ongrid/internal/iam/server/http.go#L92-L96)

```go
// recordSuccess clears the email-keyed slot — a real user who finally
// gets their password right shouldn't stay throttled. IP keeps its
// counter (the IP might still be hosting an attack against other users).
func (t *loginThrottle) recordSuccess(email string) {
    t.mu.Lock()
    delete(t.byEmail, email)
    t.mu.Unlock()
}
```

**关键**：成功登录清 email slot，但保留 IP slot（IP 可能还在攻击其他用户）。

### 14.4 in-process 不用 Redis

源码：[http.go#L26-L31](file:///d:/claude/ongrid/internal/iam/server/http.go#L26-L31)

```go
// Tracked entirely in-process: a single-manager MVP doesn't need Redis.
// Restart drains the counter — that's a feature, not a bug, because
// operator-grade attackers will reuse the same key after restart anyway
// and we'd rather not over-engineer.
```

**关键**：单 manager MVP 用进程内 map，重启清零（特性而非 bug）。

### 14.5 login handler 集成

源码：[http.go#L221-L269](file:///d:/claude/ongrid/internal/iam/server/http.go#L221-L269)

```go
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
    var in loginReq
    decode(r, &in)
    ip := clientIP(r)
    emailKey := strings.ToLower(strings.TrimSpace(in.Email))
    if err := h.throttle.check(ip, emailKey); err != nil {
        auditmw.SetAuditEvent(r, bizaudit.Event{
            Action:       auditmodel.ActionAuthLoginFailed,
            ResourceID:   emailKey,
            Status:       auditmodel.StatusFailure,
            ErrorMessage: "rate limited",
        })
        writeErr(w, err)
        return
    }
    pair, err := h.svc.Login(r.Context(), in.Email, in.Password)
    if err != nil {
        h.throttle.recordFailure(ip, emailKey)
        auditmw.SetAuditEvent(r, bizaudit.Event{
            Action:       auditmodel.ActionAuthLoginFailed,
            ResourceID:   emailKey,
            ErrorMessage: err.Error(),
        })
        writeErr(w, err)
        return
    }
    h.throttle.recordSuccess(emailKey)
    // Successful login no longer audited (operator flagged row volume)
    writeJSON(w, http.StatusOK, loginResp{...})
}
```

**关键**：成功登录不再审计（操作员反馈行数过多），仅失败登录审计。

---

## 15. 审计集成

### 15.1 HLD-010 显式审计

源码：[internal/manager/server/middleware/audit.go#L56-L71](file:///d:/claude/ongrid/internal/manager/server/middleware/audit.go#L56-L71)

```go
// AuditMiddleware records HLD-010 audit_logs rows for **explicitly-
// annotated user actions only**. The middleware no longer derives a
// generic "http_<method>_<resource>" fallback — that produced ugly,
// non-actionable rows like `http_post_alerts`.
//
// To audit a new operation, the handler must call SetAuditEvent with
// a canonical Action constant.
```

**关键**：只审计显式 `SetAuditEvent` 标注的动作，非访问日志。

### 15.2 iam 审计点

| 动作 | 触发位置 | Action 常量 |
|---|---|---|
| 登录失败（限流） | [http.go#L230-L237](file:///d:/claude/ongrid/internal/iam/server/http.go#L230-L237) | `ActionAuthLoginFailed` |
| 登录失败（凭证错） | [http.go#L246-L253](file:///d:/claude/ongrid/internal/iam/server/http.go#L246-L253) | `ActionAuthLoginFailed` |
| 注册用户 | [http.go#L304-L311](file:///d:/claude/ongrid/internal/iam/server/http.go#L304-L311) | `ActionUserCreate` |
| 删除用户 | [http.go#L358-L363](file:///d:/claude/ongrid/internal/iam/server/http.go#L358-L363) | `ActionUserDelete` |
| 改角色 | [http.go#L385-L391](file:///d:/claude/ongrid/internal/iam/server/http.go#L385-L391) | `ActionUserUpdate` |

### 15.3 enrichFromRequest 自动填充

源码：[audit.go#L106-L131](file:///d:/claude/ongrid/internal/manager/server/middleware/audit.go#L106-L131)

```go
func enrichFromRequest(ev *audit.Event, r *http.Request, ctx context.Context, status int) {
    if t, ok := tenantctx.From(ctx); ok {
        uid := t.UserID
        if uid != 0 && ev.UserID == nil {
            ev.UserID = &uid
        }
        if ev.UserEmail == "" {
            ev.UserEmail = t.Email
        }
        if ev.Role == "" {
            ev.Role = t.Role
        }
    }
    if ev.IP == "" {
        ev.IP = clientIP(r)
    }
    // ...
}
```

**关键**：tenantctx slot 让 audit 中间件（outer）看到 auth 中间件（inner）写的 tenant。

---

## 16. 数据层

### 16.1 User 表迁移

源码：[internal/iam/data/user/sqlite/migrate.go#L16-L29](file:///d:/claude/ongrid/internal/iam/data/user/sqlite/migrate.go#L16-L29)

```go
func Migrate(db *gorm.DB) error {
    if err := db.AutoMigrate(&model.User{}); err != nil {
        return err
    }
    // existing deploys had `chk_users_role` baked in with the old 2-value set.
    // AutoMigrate doesn't ALTER existing CHECK constraints, so drop + recreate.
    _ = db.Exec("ALTER TABLE users DROP CONSTRAINT chk_users_role").Error
    _ = db.Exec("ALTER TABLE users ADD CONSTRAINT chk_users_role CHECK (role IN ('admin','user','viewer'))").Error
    return nil
}
```

**关键**：旧部署的 CHECK 约束只有 `admin/user`，AutoMigrate 不 ALTER，需手动 drop + recreate 加 `viewer`。

### 16.2 User Repo 方法

源码：[internal/iam/data/user/sqlite/user.go](file:///d:/claude/ongrid/internal/iam/data/user/sqlite/user.go)

| 方法 | 行号 | 说明 |
|---|---|---|
| `Create` | L27-L32 | 插入用户 |
| `GetByEmail` | L35-L44 | 邮箱查询 |
| `GetByID` | L47-L56 | ID 查询 |
| `List` | L59-L65 | 全量（id asc） |
| `Count` | L68-L74 | 计数 |
| `Delete` | L77-L86 | 硬删除 |
| `UpdateRole` | L89-L98 | 更新角色 |
| `UpdateProfile` | L101-L113 | 更新 display_name + phone |
| `UpdateStatus` | L116-L125 | active/disabled |
| `UpdateSuperuser` | L128-L137 | is_superuser |
| `UpdatePassHash` | L141-L150 | 密码哈希 |

**关键**：所有 Update 方法在 `RowsAffected == 0` 时返回 `errs.ErrNotFound`。

### 16.3 Membership Repo 双 join

源码：[internal/iam/data/membership/store/repo.go#L81-L153](file:///d:/claude/ongrid/internal/iam/data/membership/store/repo.go#L81-L153)

```go
type MembershipWithUser struct {
    model.OrgMembership
    User model.User `gorm:"-"`
}

type MembershipWithOrg struct {
    model.OrgMembership
    Org model.Org `gorm:"-"`
}
```

**关键**：用 `gorm:"-"` 标记嵌入字段，手动两步查询（先 memberships，再 IN 查 users/orgs），避免 GORM 跨表 join 的 dialect 漂移。

---

## 17. 启动引导 5 步

源码：[cmd/ongrid/main.go#L304-L381](file:///d:/claude/ongrid/cmd/ongrid/main.go#L304-L381)

### 17.1 步骤 1：EnsureSuperuser 迁移

```go
if err := userUC.EnsureSuperuser(rootCtx); err != nil {
    log.Error("iam: ensure superuser migration", slog.Any("err", err))
}
```

把 legacy admin（`Role==admin`）迁移到 `IsSuperuser=true`，并 backfill 空 display_name。

### 17.2 步骤 2：Casbin Enforcer + SeedRolePolicies

```go
authzEnf, err := iambizauthz.New(db, log)
if err := authzEnf.SeedRolePolicies(rootCtx); err != nil {
    os.Exit(1)
}
```

构建 Enforcer（gorm-adapter 自动建 `casbin_rule` 表），注入硬编码角色策略。

### 17.3 步骤 3：org/membership Service 装配

```go
orgRepo := iamdataorg.NewRepo(db)
membershipRepo := iamdatamembership.NewRepo(db)
orgSvc := iambizorg.New(orgRepo, membershipRepo, authzEnf)
membershipSvc := iambizmembership.New(membershipRepo, authzEnf)
```

org 和 membership 都注入 authzEnf 作为 CasbinHook。

### 17.4 步骤 4：HydrateMemberships

```go
if rows, err := membershipRepo.All(rootCtx); err == nil {
    if err := authzEnf.HydrateMemberships(rootCtx, rows); err != nil {
        log.Warn("iam: hydrate casbin failed", slog.Any("err", err))
    }
}
```

把所有 OrgMembership 行镜像为 casbin g 规则。

### 17.5 步骤 5：Seed 默认组织 + 回填成员 + 重 parenting

```go
if seedOrg, err := orgSvc.EnsureSeed(rootCtx, "默认组织", "首次部署的默认组织..."); err == nil {
    // 5a. 回填现有用户为 member（admin → org_admin）
    for _, u := range existing {
        role := iammodel.MembershipRoleMember
        if u.Role == iammodel.RoleAdmin {
            role = iammodel.MembershipRoleAdmin
        }
        membershipSvc.AddOrUpdate(rootCtx, u.ID, seedOrg.ID, role)
    }
    // 5b. 重 parenting 散落的顶级 org 到 seed 下
    for _, o := range allOrgs {
        if o.ParentID == nil {
            orgSvc.Update(rootCtx, o.ID, UpdateInput{SetParent: true, ParentID: &seedID})
        }
    }
}
```

**关键**：2026-05 前 "ongridio" vendor org 作为 "默认组织" 的 sibling，造成 UX 混乱，现在所有非 seed 顶级 org 自动挂到 seed 下。

---

## 18. 配置层

### 18.1 JWT 配置

源码：[internal/pkg/config/config.go#L280-L284](file:///d:/claude/ongrid/internal/pkg/config/config.go#L280-L284)

```go
type JWTConfig struct {
    Secret     string
    AccessTTL  time.Duration
    RefreshTTL time.Duration
}

JWT: JWTConfig{
    Secret:     getEnv("ONGRID_JWT_SECRET", "dev-insecure-secret-change-me"),
    AccessTTL:  getEnvDuration("ONGRID_JWT_ACCESS_TTL", 15*time.Minute),
    RefreshTTL: getEnvDuration("ONGRID_JWT_REFRESH_TTL", 168*time.Hour), // 生产 720h
},
```

### 18.2 不安全 secret fatal

源码：[main.go#L282-L286](file:///d:/claude/ongrid/cmd/ongrid/main.go#L282-L286)

```go
const insecureJWTSecret = "dev-insecure-secret-change-me"
if cfg.JWT.Secret == insecureJWTSecret {
    log.Error("FATAL: ONGRID_JWT_SECRET is still the built-in default — refusing to start.")
    os.Exit(1)
}
```

**红线**：默认 secret 拒绝启动，强制操作员设置强 secret。

### 18.3 Admin 配置

源码：[config.go#L330-L333](file:///d:/claude/ongrid/internal/pkg/config/config.go#L330-L333)

```go
type AdminConfig struct {
    Email    string
    Password string
}

Admin: AdminConfig{
    Email:    getEnv("ONGRID_ADMIN_EMAIL", "admin@ongrid.local"),
    Password: getEnv("ONGRID_ADMIN_PASSWORD", ""),
},
```

**关键**：首次启动用此凭证 BootstrapAdmin，密码空则跳过。

---

## 19. 并发与资源管理

### 19.1 Casbin SyncedEnforcer

源码：[authz.go#L47-L51](file:///d:/claude/ongrid/internal/iam/biz/authz/authz.go#L47-L51)

```go
type Enforcer struct {
    mu sync.Mutex
    e  *casbin.SyncedEnforcer
    l  *slog.Logger
}
```

**关键**：`SyncedEnforcer` 自带读写锁，额外 `mu` 保护批量操作（Seed/Hydrate/Sync）。

### 19.2 loginThrottle mutex

源码：[http.go#L32-L36](file:///d:/claude/ongrid/internal/iam/server/http.go#L32-L36)

```go
type loginThrottle struct {
    mu      sync.Mutex
    byIP    map[string]*throttleSlot
    byEmail map[string]*throttleSlot
}
```

**关键**：单 mutex 保护两个 map，check/recordFailure/recordSuccess 都加锁。

### 19.3 tenantctx slot 指针

源码：[tenantctx.go#L74-L76](file:///d:/claude/ongrid/internal/pkg/tenantctx/tenantctx.go#L74-L76)

```go
func WithSlot(ctx context.Context) context.Context {
    return context.WithValue(ctx, slotKey{}, &slot{})
}
```

**关键**：slot 是 `*slot` 指针，inner `r.WithContext()` 产生新 ctx 但指针指向同一 slot，outer 可见 mutation。

### 19.4 argon2id 64 MiB memory

源码：[argon2.go#L20](file:///d:/claude/ongrid/internal/pkg/passwd/argon2.go#L20)

```go
argonMemory  uint32 = 64 * 1024 // 64 MiB
```

**关键**：每次 hash 占 64 MiB 内存，防 GPU 暴力破解。并发 login 会瞬时占大量内存，由 loginThrottle 限流间接控制。

---

## 20. 架构红线与设计要点

### 20.1 红线

1. **Service 禁 import data**：gospec 分层，go-arch-lint 强制（[service.go#L1-L4](file:///d:/claude/ongrid/internal/iam/service/service.go#L1-L4)）
2. **JWT secret 默认值 fatal**：`dev-insecure-secret-change-me` 拒绝启动（[main.go#L282-L286](file:///d:/claude/ongrid/cmd/ongrid/main.go#L282-L286)）
3. **Verify 不查 DB**：身份信任 JWT claim，token 生命周期内不查 DB（[jwt.go#L4-L6](file:///d:/claude/ongrid/internal/pkg/auth/jwt.go#L4-L6)）
4. **PassHash 永不回传**：Register/Create/GetByID/List 都清空 PassHash（[usecase.go#L75-L76](file:///d:/claude/ongrid/internal/iam/biz/user/usecase.go#L75-L76)）
5. **超级管理员短路 casbin**：防 corrupt policy 锁死系统管理员（[authzmw#L1-L5](file:///d:/claude/ongrid/internal/pkg/authzmw/middleware.go#L1-L5)）
6. **Casbin 错误一律 deny**：Enforce 出错 deny + log，不 fail-open（[authz.go#L237-L246](file:///d:/claude/ongrid/internal/iam/biz/authz/authz.go#L237-L246)）
7. **argon2id 常数时间比较**：`subtle.ConstantTimeCompare` 防时序侧信道（[argon2.go#L77](file:///d:/claude/ongrid/internal/pkg/passwd/argon2.go#L77)）
8. **Login 不区分失败原因**：用户不存在 / disabled / 密码错都返回 `ErrUnauthorized`（[usecase.go#L87-L98](file:///d:/claude/ongrid/internal/iam/biz/user/usecase.go#L87-L98)）
9. **IAM 路由走 Role 检查不走 casbin**：iam 是平台级管理，`requireAdmin` 读 `t.Role == "admin"`（[http.go#L397-L408](file:///d:/claude/ongrid/internal/iam/server/http.go#L397-L408)）
10. **单根组织不变量**：无显式 parent 的 Create 自动挂到 "默认组织" 下（[org/usecase.go#L69-L104](file:///d:/claude/ongrid/internal/iam/biz/org/usecase.go#L69-L104)）

### 20.2 设计要点

1. **延迟装配**：Phase-1 服务（orgs/memberships/authz）可选注入，旧部署兼容（[service.go#L32-L52](file:///d:/claude/ongrid/internal/iam/service/service.go#L32-L52)）
2. **mutable slot 模式**：解决 inner middleware `r.WithContext()` 后 outer 看不到 tenant 的问题（[tenantctx.go#L51-L86](file:///d:/claude/ongrid/internal/pkg/tenantctx/tenantctx.go#L51-L86)）
3. **双 token 同 Claims**：access 和 refresh 用相同 Claims，仅 TTL 不同（[usecase.go#L362-L387](file:///d:/claude/ongrid/internal/iam/biz/user/usecase.go#L362-L387)）
4. **旧 token 兼容**：`is_superuser` 缺失时 `Role=="admin"` fallback（[middleware.go#L38](file:///d:/claude/ongrid/internal/pkg/auth/middleware.go#L38)）
5. **PHC 编码自描述**：argon2 参数编码在 hash 中，改参数不失效旧 hash（[argon2.go#L36-L45](file:///d:/claude/ongrid/internal/pkg/passwd/argon2.go#L36-L45)）
6. **Casbin RBAC with domains**：四元组（sub, dom, obj, act），g 三元组分组（[model.conf](file:///d:/claude/ongrid/internal/iam/biz/authz/model.conf)）
7. **硬编码角色策略**：3 角色 + superuser fallback，启动时幂等注入（[authz.go#L90-L125](file:///d:/claude/ongrid/internal/iam/biz/authz/authz.go#L90-L125)）
8. **truth table 与 casbin 不漂移**：每次 membership mutation 都 sync casbin（[membership/usecase.go#L46-L73](file:///d:/claude/ongrid/internal/iam/biz/membership/usecase.go#L46-L73)）
9. **双维度登录限流**：IP（20/5min）+ email（6/15min），成功清 email 保留 IP（[http.go#L43-L96](file:///d:/claude/ongrid/internal/iam/server/http.go#L43-L96)）
10. **审计显式标注**：只审计 `SetAuditEvent` 标注的动作，非访问日志（[audit.go#L56-L71](file:///d:/claude/ongrid/internal/manager/server/middleware/audit.go#L56-L71)）
11. **createUser 自动加入默认组织**：否则 casbin 拒绝所有动作（[orgs.go#L456-L467](file:///d:/claude/ongrid/internal/iam/server/orgs.go#L456-L467)）
12. **cycle check 1024 跳**：reparent 时向上走 1024 跳防环（[org/usecase.go#L174-L188](file:///d:/claude/ongrid/internal/iam/biz/org/usecase.go#L174-L188)）
13. **EnsureSeed 竞态安全**：Create 失败后 re-fetch（[org/usecase.go#L118-L131](file:///d:/claude/ongrid/internal/iam/biz/org/usecase.go#L118-L131)）
14. **WebSocket token fallback**：`?token=<jwt>` query string（[middleware.go#L58-L67](file:///d:/claude/ongrid/internal/pkg/auth/middleware.go#L58-L67)）

---

## 21. 附录：完整调用链

### 21.1 登录链

```
POST /api/v1/auth/login
    ↓ auth.Middleware（跳过，public 路由）
    ↓ iamHandler.login（[http.go#L221](file:///d:/claude/ongrid/internal/iam/server/http.go#L221)）
    ↓ loginThrottle.check（ip, email）
    ↓ Service.Login（[service.go#L65](file:///d:/claude/ongrid/internal/iam/service/service.go#L65)）
    ↓ user.Usecase.Login（[usecase.go#L80](file:///d:/claude/ongrid/internal/iam/biz/user/usecase.go#L80)）
    ↓ repo.GetByEmail
    ↓ verifyPassword（argon2id）
    ↓ issuePair（JWT SignAccess + SignRefresh）
    ↓ loginThrottle.recordSuccess(email)
    ↓ 200 + {access_token, refresh_token, expires_in, role}
```

### 21.2 受保护路由链

```
GET /api/v1/edges
    ↓ AuditMiddleware（outer，WithSlot）
    ↓ auth.Middleware（inner）
        ↓ extractBearer
        ↓ signer.Verify（JWT HS256）
        ↓ tenantctx.SetOnSlot（写 slot）
        ↓ tenantctx.With（写 ctx）
    ↓ authzmw.Require("edge:*", "write")
        ↓ tenantctx.From
        ↓ IsSuperuser? → bypass
        ↓ AllowAnyOrg(userID, "edge:*", "write")
            ↓ userDomains(userID)
            ↓ casbin.Enforce(uid, oid, "edge:*", "write")
        ↓ allow → next
    ↓ edgeHandler
    ↓ SetAuditEvent（可选）
    ↓ AuditMiddleware post-handler
        ↓ enrichFromRequest（读 slot 填 user_id/email）
        ↓ uc.Emit（写 audit_logs）
```

### 21.3 创建组织链

```
POST /api/v1/orgs
    ↓ auth.Middleware
    ↓ requireAdmin（t.Role == "admin"）
    ↓ org.Service.Create（[org/usecase.go#L77](file:///d:/claude/ongrid/internal/iam/biz/org/usecase.go#L77)）
    ↓ 无 parent → GetByName("默认组织") → auto reparent
    ↓ repo.Create
    ↓ 201 + orgDTO
```

### 21.4 添加成员链

```
POST /api/v1/orgs/{id}/members
    ↓ auth.Middleware
    ↓ requireAdmin
    ↓ membership.Service.AddOrUpdate（[membership/usecase.go#L46](file:///d:/claude/ongrid/internal/iam/biz/membership/usecase.go#L46)）
    ↓ IsValidMembershipRole
    ↓ repo.Upsert
    ↓ authz.SyncMembership（[authz.go#L149](file:///d:/claude/ongrid/internal/iam/biz/authz/authz.go#L149)）
        ↓ GetFilteredGroupingPolicy（strip old g）
        ↓ RemoveGroupingPolicy
        ↓ AddGroupingPolicy（new g）
    ↓ 201 + {user_id, org_id, role}
```

### 21.5 启动引导链

```
main.go
    ↓ Migrate（users, orgs, org_memberships, casbin_rule）
    ↓ userUC.BootstrapAdmin（首次）
    ↓ userUC.EnsureSuperuser（legacy admin → is_superuser）
    ↓ authzEnf = New(db)
    ↓ authzEnf.SeedRolePolicies（硬编码 3 角色 + superuser）
    ↓ orgSvc / membershipSvc 装配（注入 authzEnf）
    ↓ authzEnf.HydrateMemberships（所有 membership → casbin g）
    ↓ orgSvc.EnsureSeed("默认组织")
    ↓ 回填现有用户为 member/admin
    ↓ 重 parenting 散落顶级 org
    ↓ iamSvc.SetOrgs/SetMemberships/SetAuthz
    ↓ authzMW = authzmw.New(authzEnf)
```

---

## 22. 交叉引用

- [ongrid_configs.md](file:///d:/claude/ongrid/ongrid_configs.md)：完整配置说明（`ONGRID_JWT_SECRET` / `ONGRID_ADMIN_EMAIL` / `ONGRID_ADMIN_PASSWORD`）
- [ongrid_api.md](file:///d:/claude/ongrid/ongrid_api.md)：26 个业务域 API（含 IAM 域 + 认证授权章节）
- [ongrid_architecture.md](file:///d:/claude/ongrid/ongrid_architecture.md)：架构总览
- [ongrid_LLM.md](file:///d:/claude/ongrid/ongrid_LLM.md)：AIOps 编排（agent 工具权限受 authzmw 控制）
- [ongrid_frontier.md](file:///d:/claude/ongrid/ongrid_frontier.md)：Frontier 集成（edge agent 凭证使用 IAM 体系）

---

**文档版本**：v1.0
**生成时间**：2026-07-31
**覆盖源码版本**：v0.7.113
**Casbin 版本**：v2
**JWT 库**：github.com/golang-jwt/jwt/v5
**密码哈希**：argon2id（PHC 编码）
