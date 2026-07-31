# `telemetry_auth.go` 技术实现文档

> 源文件：`internal/manager/biz/k8s/telemetry_auth.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/k8s`

## 1. 概述

本文件是 K8s telemetry 凭据的认证器：验证 cluster-scoped write-only identity（access_key + secret_key）。设计要点：30s TTL 缓存减少 DB round-trip；cache key 用 SHA-256(access_key + secret_key) 避免明文存储；LRU 淘汰策略（1024 上限 + 过期清理）。红线：tunnel authenticator 不用此类型，这是数据面凭据不能成为控制面凭据的强制边界。

## 2. 包信息

- **包名**：`k8s`
- **所属模块**：`internal/manager/biz/k8s`
- **依赖方向**：被 telemetry 写入路径（Prometheus remote_write / Loki / Tempo）调用；依赖 `pkg/errs`、`pkg/passwd`

## 3. 关键类型与接口

```go
const (
    telemetryAuthCacheTTL        = 30 * time.Second
    maxTelemetryAuthCacheEntries = 1024
)

// TelemetryAuthenticator 校验 cluster-scoped write-only identity
type TelemetryAuthenticator struct {
    repo  Repository
    cache *telemetryAuthCache
}

type telemetryAuthCacheEntry struct {
    clusterID uint64
    expiresAt time.Time
}

type telemetryAuthCache struct {
    mu      sync.Mutex
    entries map[[32]byte]telemetryAuthCacheEntry
}
```

## 4. 关键函数与流程

### `NewTelemetryAuthenticator`
- **签名**：`func NewTelemetryAuthenticator(repo Repository) *TelemetryAuthenticator`
- **职责**：构造认证器 + 空 cache

### `TelemetryAuthenticator.Authenticate`
- **签名**：`func (a *TelemetryAuthenticator) Authenticate(ctx, accessKey, secretKey string) (uint64, error)`
- **职责**：校验凭据，返回 clusterID
- **流程**：
  1. a/repo nil → `errs.ErrNotWiredYet`
  2. **cache lookup**：命中且未过期 → 返回 clusterID
  3. `repo.GetTelemetryCredentialByAccessKey(ctx, accessKey)`
  4. err / credential nil / `!passwd.Verify(secretKey, credential.SecretKeyHash)` → `errs.ErrUnauthorized`
  5. **cache store**：(accessKey, secretKey, clusterID)
  6. 返回 clusterID
- **错误处理**：cache miss 不算错误；DB 查询失败/凭据不匹配/密码错均返回 ErrUnauthorized（不区分，防探测）

### `telemetryAuthCache.lookup`
- **签名**：`func (c *telemetryAuthCache) lookup(accessKey, secretKey string, now time.Time) (uint64, bool)`
- **职责**：查缓存
- **流程**：
  1. c nil → (0, false)
  2. key = `sha256(accessKey + "\x00" + secretKey)`
  3. `mu.Lock`；查 entry
  4. 不存在 → (0, false)
  5. 过期 → delete + (0, false)
  6. 返回 (clusterID, clusterID != 0)

### `telemetryAuthCache.store`
- **签名**：`func (c *telemetryAuthCache) store(accessKey, secretKey string, clusterID uint64, now time.Time)`
- **职责**：写缓存 + LRU 淘汰
- **流程**：
  1. c nil / clusterID == 0 → return
  2. `mu.Lock`
  3. **过期清理**：遍历 entries，过期则 delete
  4. **LRU 淘汰**：len >= 1024 → 找最旧 expiry 的 key 删除
  5. 写入新 entry，expiresAt = now + 30s
- **错误处理**：无 error

### `telemetryAuthCacheKey`
- **签名**：`func telemetryAuthCacheKey(accessKey, secretKey string) [32]byte`
- **职责**：生成 cache key
- **流程**：`sha256.Sum256([]byte(accessKey + "\x00" + secretKey))`
- **注释**：`\x00` 分隔防 accessKey 末尾 + secretKey 开头拼接歧义

## 5. 依赖关系

- **内部包**：`pkg/errs`（ErrUnauthorized/ErrNotWiredYet）、`pkg/passwd`（Verify）
- **外部库**：仅标准库
- **被调用方**：telemetry 写入路径的 auth middleware

## 6. 并发与资源管理

- **`mu`（Mutex）**：保护 entries map
- **cache key 哈希**：SHA-256，避免明文 secret 存内存
- **30s TTL**：cache 条目 30s 过期
- **1024 上限**：LRU 淘汰最旧条目
- **过期清理**：store 时顺便清理过期条目

## 7. 设计模式与亮点

- **数据面/控制面隔离**：注释明示 tunnel authenticator 不用此类型，强制边界
- **cache key 哈希**：SHA-256(access_key + secret_key)，不明文存 secret
- **`\x00` 分隔符**：防拼接歧义
- **30s TTL**：平衡性能与凭据轮换生效时间
- **LRU + 过期双淘汰**：1024 上限防内存膨胀 + 过期自动清理
- **统一 ErrUnauthorized**：不区分 err/nil/密码错，防凭据探测

## 8. 注意事项

- **telemetryAuthCacheTTL=30s**：凭据轮换后最长 30s 生效
- **maxTelemetryAuthCacheEntries=1024**：cluster 数上限，超出 LRU 淘汰
- **cache key 含 secret**：哈希后存内存，进程重启清空
- **ErrUnauthorized 不区分**：防探测，统一错误
- **passwd.Verify**：用 bcrypt/argon2id（遵循 AGENTS.md 红线）
- **store 时清理过期**：惰性清理，无后台 goroutine
