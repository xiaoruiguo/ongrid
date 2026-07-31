# `http.go` 技术实现文档

> 源文件：`internal/iam/server/http.go`
> 包路径：`github.com/ongridio/ongrid/internal/iam/server`

## 1. 概述

本文件是 IAM BC 的 HTTP 入口，构建 chi 路由、登录限流器、以及认证/用户管理的 handler 集合。核心组件：基于 IP + email 双键的内存滑动窗口登录限流（防密码喷洒）、JWT 签发/刷新端点、用户 CRUD 与角色管理、统一的 JSON 编解码与错误映射。审计事件经 `auditmw.SetAuditEvent` 注入 ctx，由后续 middleware 落库。

## 2. 包信息

- **包名**：`server`
- **所属模块**：`internal/iam/server` —— IAM BC 的 HTTP 层
- **依赖方向**：被 `cmd/ongrid` 装配路由；依赖 `internal/iam/service`、`internal/iam/model`、manager 侧 audit 包、`internal/pkg/errs`、`internal/pkg/tenantctx`

## 3. 关键类型与接口

```go
type loginThrottle struct {
	mu      sync.Mutex
	byIP    map[string]*throttleSlot
	byEmail map[string]*throttleSlot
}

type throttleSlot struct {
	count    int
	windowAt time.Time
}

type Handler struct {
	svc       *service.Service
	log       *slog.Logger
	throttle  *loginThrottle
}
```

- `loginThrottle`：进程内双键（IP/email）滑动窗口限流器，sync.Mutex 保护。
- `throttleSlot`：单键的计数 + 窗口起始时间。
- `Handler`：HTTP handler 集合，持有 service、logger、throttle。

常量：
- `loginIPLimit = 20` / `loginIPWindow = 5min`（IP 维度，宽松，因 NAT）
- `loginEmailLimit = 6` / `loginEmailWindow = 15min`（email 维度，严格）

## 4. 关键函数与流程

### `NewHandler`
- **签名**：`func NewHandler(svc *service.Service, log *slog.Logger) *Handler`
- **职责**：构造 Handler 与限流器。

### `RegisterPublic` / `RegisterProtected`
- **签名**：分别注册无需 token 与需 JWT 的路由。
- **流程**：public 挂 `/v1/auth/login` + `/v1/auth/refresh`；protected 挂 `/v1/auth/register` + `/v1/self` + `/v1/me` + `/v1/users*` + `/v1/orgs*`。
- **注释要点**：public/protected 分离以便共享 URL 前缀但用不同 middleware 链。

### `login`
- **签名**：`func (h *Handler) login(w, r)`
- **职责**：登录端点，含限流 + 审计。
- **流程**：
  1. decode loginReq。
  2. `clientIP(r)` 取 IP（信任 XFF 首跳，因 nginx 覆写）。
  3. `throttle.check(ip, emailKey)` 检查窗口；超限返回 `ErrTooManyAttempts` 并记审计 failure。
  4. `svc.Login` 校验凭证；失败 `recordFailure` + 记审计 failure；成功 `recordSuccess` + 返回 token。
- **错误处理**：所有限流/认证错误经 `writeErr` 映射 HTTP 状态。
- **注释要点**：成功登录不再审计（运营反馈行数过多），仅失败审计。

### `refresh` / `register` / `self` / `listUsers` / `deleteUser` / `setRole`
- **职责**：分别处理刷新、注册、自查、用户列表、删除、改角色。
- **流程**：均先 decode/parseID → 校验权限（`requireAdmin`）→ 调 svc → 记审计 → 写响应。
- **错误处理**：统一 `writeErr`。

### `requireAdmin`
- **签名**：`func requireAdmin(w, r) bool`
- **职责**：从 `tenantctx` 取认证信息，校验 `Role == RoleAdmin`，否则 401/403。
- **错误处理**：未认证返回 `ErrUnauthorized`；非 admin 返回 `ErrForbidden`。

### `clientIP`
- **签名**：`func clientIP(r *http.Request) string`
- **职责**：取客户端 IP，信任 XFF 首跳（生产 nginx 覆写），否则取 RemoteAddr 去端口。

### 限流辅助：`check` / `recordFailure` / `recordSuccess` / `exceeded` / `bump`
- **`check`**：加锁后查 IP 与 email 两键，任一超限返回 `ErrTooManyAttempts`；不消耗 slot。
- **`recordFailure`**：登录失败后 bump 两键计数；窗口过期则重置。
- **`recordSuccess`**：仅清 email 键（IP 可能仍在攻击其他用户，保留计数）。
- **`exceeded`**：判断 slot 是否在窗口内超限。
- **`bump`**：递增计数，窗口外则新建 slot。

### `decode` / `parseID` / `writeJSON` / `writeErr`
- **`decode`**：JSON 解码 body 到 dst，失败包装 `ErrInvalid`。
- **`parseID`**：从 chi URL param 取 id 并 ParseUint。
- **`writeJSON`**：写 Content-Type + 状态码 + JSON body。
- **`writeErr`**：经 `errs.HTTPStatus(err)` 映射状态码后写错误文本。

## 5. 依赖关系

- **内部包**：`internal/iam/model`、`internal/iam/service`、`internal/manager/biz/audit`、`internal/manager/model/audit`、`internal/manager/server/middleware`（auditmw）、`internal/pkg/errs`、`internal/pkg/tenantctx`
- **外部库**：`github.com/go-chi/chi/v5`、`log/slog`、标准库 `encoding/json` / `errors` / `net/http` / `strconv` / `strings` / `sync` / `time`
- **被调用方**：`cmd/ongrid`（注册路由）

## 6. 并发与资源管理

- `loginThrottle.mu sync.Mutex` 保护 `byIP` / `byEmail` 两个 map；所有 check/record* 均加锁。
- 限流器为进程内单实例，无持久化；重启即清空（注释：feature not bug，避免过度工程）。
- 无 goroutine/channel；HTTP handler 由 chi 调度。
- `context.Context` 透传至 svc，支持取消与超时。

## 7. 设计模式与亮点

- **双键滑动窗口限流**：IP（宽松，NAT 友好）+ email（严格，防喷洒），任一超限即拒；成功登录清 email 键但保留 IP 键（IP 可能仍在攻击其他账号）。
- **限流不消耗 slot**：check 与 recordFailure 分离，成功登录不消耗预算，避免正常用户因偶发失败被锁。
- **审计事件注入 ctx**：经 `auditmw.SetAuditEvent` 写入 ctx，由后续 middleware 统一落库，handler 不直接写库。
- **fail-closed 鉴权**：`requireAdmin` 未取到 tenant 信息即拒。
- **XFF 信任策略**：仅信任首跳（nginx 覆写场景），避免客户端伪造。
- **public/protected 路由分离**：共享 URL 前缀但 middleware 链不同，便于权限分层。

## 8. 注意事项

- 限流器进程内单实例：单 manager MVP 可接受；多副本部署需迁移到 Redis（注释提及）。
- `clientIP` 信任 XFF 首跳依赖「nginx 必覆写 XFF」的部署假设；若直连暴露需重新评估。
- 成功登录不审计是运营决策，安全审计需评估是否需恢复。
- `decode` 用 `json.NewDecoder(r.Body).Decode`，未限制 body 大小，大 payload 风险需在反代层防护。
- `requireAdmin` 仅校验 `Role`，未校验 `Status`（disabled admin 仍可通过）；依赖 svc 层 Login 的 status 检查，但已签发的 JWT 在 status 变更后仍有效（无 revocation list）。
- 审计 Payload 中可能含敏感字段（如 email），需确认下游 audit 落库做了脱敏。
