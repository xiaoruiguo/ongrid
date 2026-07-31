# `http.go` 技术实现文档

> 源文件：`internal/manager/server/edgeauth/http.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/server/edgeauth`

## 1. 概述

本文件是 nginx `auth_request` 模块的内部鉴权端点：nginx 在代理遥测数据面请求（到 Loki / Tempo 等下游）前，先调用本端点验证 Basic auth。设计要点：**挂在 public mux（无 JWT auth）**，因为 nginx 是唯一合法调用者，且位于本地 docker 网络内——nginx 绝不能 proxy_pass 外部流量到此处。关键红线：验证通过后通过 `X-Edge-Id` / `X-Cluster-Id` 响应头把身份回传给 nginx（用于注入 Loki 强制 label 等）；认证失败返 401 + `WWW-Authenticate` 头。

## 2. 包信息

- **包名**：`edgeauth`
- **所属模块**：`internal/manager/server/`（HTTP handler 层）
- **依赖方向**：被 nginx `auth_request` 调用；依赖注入的 `Authenticator`（实现在 `cmd/ongrid` wiring）+ `pkg/errs`

## 3. 关键类型与接口

```go
// 窄契约，具体实现 living in cmd/ongrid wiring
type Authenticator interface {
    AuthenticateDataPlane(ctx, accessKey, secretKey string) (Identity, error)
}

type Identity struct {
    EdgeID    uint64
    ClusterID uint64
}

type Handler struct {
    authn Authenticator
    log   *slog.Logger
}
```

## 4. 关键函数与流程

### `NewHandler`
- **签名**：`func NewHandler(authn Authenticator, log *slog.Logger) *Handler`
- **职责**：构造 handler；log nil 回退 `slog.Default()`；附 `comp=edgeauth` 字段
- **流程**：直接赋值

### `Register` / `RegisterAt`
- **签名**：`func (h *Handler) Register(r chi.Router)` + `func (h *Handler) RegisterAt(r chi.Router, path string)`
- **职责**：挂载 GET 端点；`Register` 默认路径 `/internal/auth/dataplane-verify`；`RegisterAt` 允许 wire 时挂到更窄路径（edge-only / telemetry-only nginx auth_request 端点）
- **流程**：`r.Get(path, h.verify)`

### `verify`
- **签名**：`func (h *Handler) verify(w http.ResponseWriter, r *http.Request)`
- **职责**：验证 Basic auth，回传身份头
- **流程**：
  1. `parseBasicAuth(Authorization header)` 解析 `Basic <base64>` → user/pass；失败 → 401 + `WWW-Authenticate: Basic realm="ongrid-data-plane"`
  2. `h.authn.AuthenticateDataPlane(ctx, user, pass)` → Identity
  3. err 是 `ErrUnauthorized` → Debug log + 401「unauthorized」
  4. 其他 err → Warn log + 500「auth backend error」
  5. `identity.EdgeID != 0` → 设 `X-Edge-Id` 头
  6. `identity.ClusterID != 0` → 设 `X-Cluster-Id` 头
  7. 200 OK
- **关键**：身份头供 nginx 通过 `auth_request_set $edge_id $upstream_http_x_edge_id;` 读取并注入下游

### `parseBasicAuth`
- **签名**：`func parseBasicAuth(header string) (user, pass string, ok bool)`
- **职责**：拆 `Basic <base64>` 为 user/pass
- **流程**：HasPrefix `Basic ` → base64 decode → 按 `:` 拆分；任一步失败返 ok=false
- **错误处理**：无错误返回，ok=false 表示任意形态不匹配

### `uintToA`
- **签名**：`func uintToA(v uint64) string`
- **职责**：无依赖 uint → string 转换（避免 import strconv 仅为此）
- **流程**：手写 digit-by-digit 填充 `[20]byte` 缓冲；0 特判返 `"0"`

## 5. 依赖关系

- **内部包**：`pkg/errs`（`ErrUnauthorized`）
- **外部库**：`github.com/go-chi/chi/v5`、标准库 `encoding/base64`、`log/slog`
- **被调用方**：nginx `auth_request` 模块（通过 `/internal/auth/dataplane-verify` 等路径）

## 6. 并发与资源管理

- **无共享状态**：Handler 仅持有 `authn` + `log`，均只读
- **ctx 透传**：`r.Context()` 透传给 Authenticator
- **无锁**：只读字段

## 7. 设计模式与亮点

- **Narrow Authenticator 契约**：handler 不依赖 edge identity 或 k8s telemetry credential domain；具体实现在 wiring site，避免 BC 跨界
- **`RegisterAt` 支持多窄路径**：同一 verifier 可挂到 edge-only / telemetry-only / compat 多个 nginx auth_request 端点
- **身份头回传**：`X-Edge-Id` 让 nginx 注入 Loki 强制 label，`X-Cluster-Id` 类似——身份在数据面下游可用
- **`WWW-Authenticate` 头**：401 时附 `Basic realm="ongrid-data-plane"`，符合 HTTP 规范，浏览器/curl 会提示输入凭据
- **`uintToA` 手写**：避免 import strconv 仅为此一处，减小依赖面
- **nil log 回退 Default**：`NewHandler` 容错 nil log，附 `comp=edgeauth` 字段方便过滤

## 8. 注意事项

- **挂在 public mux**：无 JWT auth，依赖 nginx 网络隔离——**nginx 绝不能 proxy_pass 外部流量到此处**
- **`Authenticator` 实现在 wiring site**：本包不依赖 edge identity 或 k8s telemetry credential domain，新增数据面身份源需扩展 wiring
- **Basic auth 明文**：user/pass 通过 base64 传（非加密），安全性依赖 nginx 本地 docker 网络 + HTTPS 终止在 nginx
- **`uintToA` 不处理负数**：uint64 无负数，无需处理
- **`parseBasicAuth` 严格前缀**：仅认 `Basic ` 前缀，Bearer 等其他 scheme 直接失败
- **认证失败区分 401 vs 500**：`ErrUnauthorized` → 401（凭据错），其他 err → 500（后端故障），便于 nginx 决策重试
- **`RegisterAt` 路径由 wire 决定**：不同部署可挂不同路径，handler 不关心具体路径
