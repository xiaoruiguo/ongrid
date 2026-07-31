# `service.go` 技术实现文档

> 源文件：`internal/manager/service/prometheus/service.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/service/prometheus`

## 1. 概述

本文件是 Prometheus 代理访问的应用服务，负责为 UI 签发短时 JWT ticket（30min TTL），让 nginx `auth_request` 子请求校验后透传到内网 Prometheus / Grafana。核心红线：(1) ticket TTL 30min（从早期 2min 提升 —— 用户读 dashboard 中途 401 体验差），nginx 端 sliding refresh（每次成功 auth 重签 cookie）；(2) ticket subject 固定 `prometheus-proxy`，VerifyTicket 校验 subject 防止其他用途的 JWT 滥用；(3) RefreshTicket 任何错误都返回 `("", 0, false)` 而非 error，让 nginx 端简单判定。

## 2. 包信息

- **包名**：`prometheus`
- **所属模块**：`internal/manager/service/prometheus`
- **依赖方向**：被 HTTP handler 调用（BuildLaunch / VerifyTicket）；被 nginx auth_request handler 调用（RefreshTicket）；依赖 `internal/pkg/auth`、`internal/pkg/errs`

## 3. 关键类型与接口

```go
const (
    promTicketSubject = "prometheus-proxy"
    promTicketTTL     = 30 * time.Minute
)

type Caller struct { UserID uint64; Role string }

type LaunchInput struct {
    Expr       string
    RangeInput string
    EndInput   string
    StepInput  string
}

type Service struct { signer *auth.Signer }
```

`auth.Signer` 是 JWT 签发/校验器；`auth.Claims` 含 UserID + Role + jwt.RegisteredClaims。

## 4. 关键函数与流程

### `New(signer)`

- 唯一构造器；signer 由 cmd 注入（含 JWT secret）。

### `BuildLaunch(caller, in) (path, ticket string, ttl time.Duration, err error)`

- **职责**：为 UI 构造 Prometheus graph URL + 一次性 ticket。
- **流程**：
  1. signer nil → `ErrNotWiredYet`。
  2. TrimSpace expr；空 → `ErrInvalid`；>2048 字符 → `ErrInvalid`（防爆 PromQL）。
  3. `signer.SignWithTTL(auth.Claims{UserID, Role, Subject: promTicketSubject}, promTicketTTL)` 签发 ticket。
  4. 构造 `url.Values`：`g0.expr` / `g0.tab=0` / 可选 `g0.range_input` / `g0.end_input` / `g0.step_input`。
  5. 返回 `"/prometheus/graph?" + q.Encode()`, ticket, ttl。
- **错误处理**：sign 失败直接返回 err。

### `RefreshTicket(token) (newToken string, ttl time.Duration, ok bool)`

- **职责**：nginx auth_request 滑动续签 —— 每次成功 /grafana auth 子请求给浏览器新 cookie。
- **流程**：
  1. signer nil → `("", 0, false)`。
  2. `signer.Verify(TrimSpace(token))`；err 或 `claims.Subject != promTicketSubject` → `("", 0, false)`。
  3. 用原 claims 的 UserID + Role 重新签 30min TTL ticket。
  4. 签发失败 → `("", 0, false)`；成功返回 `(fresh, ttl, true)`。
- **错误处理**：所有失败路径统一返回 false，不暴露原因（防探测）。

### `VerifyTicket(token) error`

- **职责**：nginx auth_request 子请求校验。
- **流程**：
  1. signer nil → `ErrNotWiredYet`。
  2. TrimSpace；空 → `ErrUnauthorized`。
  3. `signer.Verify(token)`；err → `ErrUnauthorized`。
  4. `claims.Subject != promTicketSubject` → `ErrUnauthorized`。
- **错误处理**：所有失败统一 `ErrUnauthorized`（不区分 expired / invalid / wrong subject，防探测）。

## 5. 依赖关系

- **内部包**：`internal/pkg/auth`（Signer / Claims）、`internal/pkg/errs`
- **外部库**：`github.com/golang-jwt/jwt/v5`、`net/url`、`strings`、`time`、`fmt`
- **被调用方**：HTTP handler（BuildLaunch 给 UI 启动 Prometheus）；nginx auth_request handler（VerifyTicket + RefreshTicket）

## 6. 并发与资源管理

- **无共享可变状态**：Service 仅持 signer 引用（线程安全）；无锁。
- **signer 内部状态**：JWT secret 在构造时注入；Verify 无状态。
- **ctx 缺失**：本服务方法无 ctx 参数 —— JWT 签发/校验是纯 CPU 操作，无 IO。

## 7. 设计模式与亮点

- **subject-scoped ticket**：固定 `prometheus-proxy` subject，VerifyTicket 校验 subject 防止其他业务 JWT 被复用访问 Prometheus。
- **sliding refresh**：nginx 每次成功 auth 重签 cookie，30min 是 idle timeout（用户离开 30min 后需重新登录）；注释提及早期 2min TTL 导致读 dashboard 中途 401。
- **错误统一化**：VerifyTicket 所有失败 → `ErrUnauthorized`；RefreshTicket 所有失败 → `false`；不暴露内部原因防探测。
- **URL 构造**：`g0.expr` 等 Grafana query string 参数；UI 直接跳转，无需前端拼装。
- **expr 长度限制 2048**：防爆 PromQL；注释未提及具体上限理由，遵循常见 URL 长度约束。

## 8. 注意事项

- **TTL 30min 是 idle timeout**：用户活跃使用时 nginx sliding refresh 持续续签；30min 不活动才需重新登录。
- **signer nil 时 BuildLaunch 返回 ErrNotWiredYet**：部署未配 JWT secret 时的降级。
- **RefreshTicket 不区分错误类型**：nginx 端只需 ok/not-ok 二值判定；具体原因不暴露。
- **VerifyTicket 无 ctx**：JWT 校验纯 CPU；若 signer 未来加 IO（如 KMS）需补 ctx。
- **expr 2048 字符上限**：超过返回 `ErrInvalid`；UI 应在前端限制。
- **ticket 跨用户不可复用**：claims 含 UserID + Role；VerifyTicket 不校验 UserID（仅 subject + 签名），实际访问控制由 nginx 后续 Prometheus RBAC 完成。
- **URL path 固定 `/prometheus/graph`**：仅支持 graph 页面；其他 Prometheus 页面（如 /prometheus/alerts）需扩展。
