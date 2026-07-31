# `authn.go` 技术实现文档

> 源文件：`internal/manager/biz/edge/authn.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/edge`

## 1. 概述

本文件实现 `AccessKeyAuthenticator` —— edge tunnel 握手时的认证器。`Authenticate` 按 AccessKeyID 查 edge row，用 `passwd.Verify`（argon2id）常量时间校验 SecretKeyHash，成功返回 `tunnel.Session{EdgeID}`。所有失败路径折叠为 `errs.ErrUnauthorized`（**不泄露枚举信号**）。成功验证缓存 60s（authCache）避免 64 MiB argon2id 单次成本。成功后异步 goroutine 翻 status=online + last_seen_at。

## 2. 包信息

- **包名**：`edge`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 `cmd/ongrid` 注入 `internal/pkg/tunnel`；依赖 `model/edge`、`pkg/errs`、`pkg/passwd`、`pkg/tunnel`

## 3. 关键类型与接口

```go
type AccessKeyAuthenticator struct {
    repo  Repo
    log   *slog.Logger
    cache *authCache
}
```

实现 `tunnel.AuthFunc` 适配（通过 `AsAuthFunc` 方法）。

## 4. 关键函数与流程

### `NewAccessKeyAuthenticator`
- **签名**：`func NewAccessKeyAuthenticator(repo Repo, log *slog.Logger) *AccessKeyAuthenticator`
- **职责**：构造 authenticator；log 可 nil
- **流程**：返回 `&AccessKeyAuthenticator{repo, log, newAuthCache()}`

### `Authenticate`
- **签名**：`func (a *AccessKeyAuthenticator) Authenticate(ctx, accessKey, secretKey string) (tunnel.Session, error)`
- **职责**：edge 握手认证；成功返回 Session{EdgeID}
- **流程**：
  1. repo nil → `ErrNotWiredYet`
  2. **Fast path**：`cache.lookup(accessKey, secretKey)` 命中 → 返回 `Session{EdgeID}`（跳过 argon2id）
  3. `repo.GetByAccessKey(ctx, accessKey)`；err 或 e==nil → `ErrUnauthorized`
  4. `passwd.Verify(secretKey, e.SecretKeyHash)`；false → `ErrUnauthorized`
  5. `cache.store(accessKey, secretKey, edgeID)` 缓存 60s
  6. **异步 goroutine**：`context.WithTimeout(context.Background(), 5*time.Second)` → `repo.UpdateStatus(bgCtx, edgeID, StatusOnline, time.Now().UTC())`；err 且 log!=nil → Warn
  7. 返回 `Session{EdgeID: edgeID}`
- **错误处理**：所有失败折叠 `ErrUnauthorized`（不区分 key 不存在 vs 密码错，防枚举）；UpdateStatus 失败仅 Warn（不阻塞握手）

### `AsAuthFunc`
- **签名**：`func (a *AccessKeyAuthenticator) AsAuthFunc() tunnel.AuthFunc`
- **职责**：适配 `Authenticate` 到 `tunnel.AuthFunc` 类型
- **流程**：返回 `a.Authenticate`（方法值）

## 5. 依赖关系

- **内部包**：`model/edge`（`StatusOnline`）、`pkg/errs`（`ErrUnauthorized` / `ErrNotWiredYet`）、`pkg/passwd`（`Verify` argon2id 常量时间）、`pkg/tunnel`（`Session`、`AuthFunc`）
- **被调用方**：`cmd/ongrid` 注入 tunnel server
- **协作**：`authCache`（同包）

## 6. 并发与资源管理

- **cache 并发安全**：authCache 内部 Mutex
- **异步 goroutine**：成功握手后 fire-and-forget 翻 status；5s 超时防 DB stall 时 goroutine 泄漏
- **`context.Background()` 而非传入 ctx**：握手 ctx 在握手返回后可能取消；DB 写需独立 ctx
- **无共享状态**：authenticator 仅持有不可变 repo + log + cache

## 7. 设计模式与亮点

- **所有失败折叠 ErrUnauthorized**：注释明示"tunnel layer never leaks enumeration signals"；不区分 key 不存在 vs 密码错
- **60s 缓存避 argon2id**：64 MiB 单次成本 × 高频 push 会拖垮 manager；cache 命中跳过计算
- **SHA-256 cache key**：明文凭据不入 map（见 authcache.go）
- **异步翻 status=online**：DB 写不阻塞握手；5s 超时防泄漏；失败仅 Warn
- **`context.Background()` for goroutine**：握手 ctx 生命周期短于 DB 写；独立 ctx 避免被取消
- **`AsAuthFunc` 适配器**：让 `*AccessKeyAuthenticator` 满足 `tunnel.AuthFunc` 类型；注入时显式调用

## 8. 注意事项

- **5s 超时 goroutine**：DB stall 时 goroutine 5s 后退出；但 DB 写失败仅 Warn，edge 仍 online（cache 已记录）
- **60s 吊销延迟**：吊销 access key 后最长 60s 内 push 仍可用；操作员应 kick edge 强制重连
- **不区分失败原因**：所有失败 ErrUnauthorized；日志不记录具体原因（防侧信道）
- **UpdateStatus 失败不阻塞**：edge 仍握手成功；下次 push 重新尝试翻 online
- **passwd.Verify 是 argon2id 常量时间**：防 timing attack 区分不存在 key vs 错密码（虽然 GetByAccessKey 失败时直接返回，理论上可探测 key 存在性，但 argon2id 计算成本高，攻击成本仍可控）
- **cache 仅成功入**：失败每次走 argon2id；攻击者无法用 cache 命中区分 key 存在性
