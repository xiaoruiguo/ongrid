# `jwt.go` 技术实现文档

> 源文件：`internal/pkg/auth/jwt.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/auth`

## 1. 概述

该文件实现了 ongrid 项目 JWT（JSON Web Token）的签发与验证核心逻辑，是身份认证（authentication）的根。`Signer` 类型封装了 HMAC-SHA256 签名密钥与 access/refresh 两套 TTL，为登录、续期、内部反向代理等场景提供 token 生命周期管理。验证流程仅做签名与 claims 校验，不查数据库——用户身份在登录时被打包进 token，并在 token 生命周期内被信任。

## 2. 包信息

- **包名**：`auth`
- **所属模块**：`internal/pkg/`（基础设施层 / 共享工具包）
- **依赖方向**：被 `internal/pkg/auth/middleware.go`（中间件）、`cmd/ongrid`、`cmd/ongrid-edge`、iam/manager 业务层调用；调用 `github.com/golang-jwt/jwt/v5`。

## 3. 关键类型与接口

### `Claims`
嵌入 `jwt.RegisteredClaims`，扩展 ongrid 业务字段。

```go
type Claims struct {
    UserID      uint64 `json:"user_id"`
    Email       string `json:"email,omitempty"`
    Role        string `json:"role"`
    IsSuperuser bool   `json:"is_superuser,omitempty"`
    jwt.RegisteredClaims
}
```

`IsSuperuser` 是独立于 Role 与 org 成员的系统管理员标志。旧 token 无该字段时解码为 `false`，由 middleware 通过 `Role=="admin"` 兼容回退。

### `Signer`
持有 secret 与两种 TTL 的不可变签名器。

```go
type Signer struct {
    secret     []byte
    accessTTL  time.Duration
    refreshTTL time.Duration
}
```

## 4. 关键函数与流程

### `NewSigner`
- **签名**：`func NewSigner(secret string, accessTTL, refreshTTL time.Duration) *Signer`
- **职责**：构造签名器；将 secret 转为 `[]byte` 持有。
- **流程**：纯赋值，无校验（secret 为空在运行时由调用方自行处理）。

### `AccessTTL / RefreshTTL`
- **签名**：`func (s *Signer) AccessTTL() time.Duration` / `RefreshTTL()`
- **职责**：暴露 TTL 配置，供 handler 向客户端返回 `expires_in`。

### `SignAccess / SignRefresh / SignWithTTL`
- **签名**：`func (s *Signer) SignAccess(c Claims) (string, error)` 等
- **职责**：分别用 access TTL、refresh TTL、自定义 TTL 签发 token。
- **流程**：统一委托给私有 `sign` 方法。

### `sign`（私有）
- **签名**：`func (s *Signer) sign(c Claims, ttl time.Duration) (string, error)`
- **流程**：
  1. 取当前时间 `now`。
  2. 覆盖 `IssuedAt = now`、`ExpiresAt = now + ttl`（caller 提供的 IAT/EXP 会被覆盖）。
  3. 用 `jwt.NewWithClaims(jwt.SigningMethodHS256, c)` 构造 token。
  4. `tok.SignedString(s.secret)` 输出字符串。
- **错误处理**：仅传播 `SignedString` 的底层错误。

### `Verify`
- **签名**：`func (s *Signer) Verify(token string) (*Claims, error)`
- **职责**：解析并验证 token。
- **流程**：
  1. `jwt.ParseWithClaims` 注入 keyFunc：检查 `t.Method` 必须是 `*jwt.SigningMethodHMAC`，否则返回 `unexpected signing method`（防 `alg=none` 攻击）。
  2. 解析失败 / `!t.Valid` → 返回错误。
  3. 成功返回填充好的 `*Claims`。
- **错误处理**：签名不匹配、过期、格式错误统一以 error 返回；由 middleware 翻译为 HTTP 401。

## 5. 依赖关系

- **内部包**：无（独立工具包）。
- **外部库**：`github.com/golang-jwt/jwt/v5`。
- **被调用方**：`internal/pkg/auth/middleware.go`、iam/manager 登录与续期 handler、edge 反向代理 hop。

## 6. 并发与资源管理

`Signer` 字段在构造后不再变更，`sign` / `Verify` 均为只读访问，天然并发安全，无需加锁。

## 7. 设计模式与亮点

- **信任边界明确**：`Verify` 只做签名/过期校验，不查 DB——红线注释明确禁止在 Verify 中执行数据库 lookup，将用户身份与 token 生命周期绑定。
- **算法固定校验**：keyFunc 强制校验 `SigningMethodHMAC`，避免算法混淆攻击。
- **TTL 解耦**：access / refresh / 自定义 TTL 共用同一 `sign`，仅参数差异；`SignWithTTL` 用于反向代理等内部短票。
- **向后兼容**：`IsSuperuser` 字段 `omitempty` + middleware 兼容回退，保证升级路径平滑。

## 8. 注意事项

- **secret 默认值风险**：`config.JWTConfig.Secret` 默认 `"dev-insecure-secret-change-me"`，生产部署必须显式覆盖。
- **时间覆盖**：caller 传入的 `IssuedAt/ExpiresAt` 会被 `sign` 覆盖，调用方不能依赖传入值。
- **不做黑名单**：token 在 TTL 内全程可信，登出 / 撤销需要更高层（短期 access TTL + refresh 轮换）覆盖。
- **HS256 对称密钥**：任何持有 secret 的服务都能签发 token，secret 泄露后果严重；多服务部署需考虑非对称（RS256）演进路径。
