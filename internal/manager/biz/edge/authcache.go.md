# `authcache.go` 技术实现文档

> 源文件：`internal/manager/biz/edge/authcache.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/edge`

## 1. 概述

本文件实现 edge 数据面认证的 60 秒凭据缓存。`authCache` 缓存成功的 argon2id 验证结果，避免每个高频 push（metric/log/trace）都付出 64 MiB 单次 argon2id 计算成本。Key 是 accessKey:secretKey 的 SHA-256（**绝不存明文凭据**），value 是 edgeID + 过期时间。

## 2. 包信息

- **包名**：`edge`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：仅被 `AccessKeyAuthenticator`（authn.go）使用；仅依赖标准库

## 3. 关键类型与接口

```go
const authCacheTTL = 60 * time.Second

type authCache struct {
    mu      sync.Mutex
    entries map[[32]byte]authCacheEntry
}

type authCacheEntry struct {
    edgeID    uint64
    expiresAt time.Time
}
```

Sentinel：`authCacheTTL = 60 * time.Second`（覆盖典型高频 push 突发；保证吊销的 key 60s 内失效）。

## 4. 关键函数与流程

### `newAuthCache`
- **签名**：`func newAuthCache() *authCache`
- **职责**：构造空 cache
- **流程**：返回 `&authCache{entries: make(map[[32]byte]authCacheEntry)}`

### `key`
- **签名**：`func (c *authCache) key(accessKey, secretKey string) [32]byte`
- **职责**：生成 cache key —— `sha256.Sum256(accessKey + ":" + secretKey)`
- **关键设计**：明文凭据绝不入 map；只存 SHA-256 摘要

### `lookup`
- **签名**：`func (c *authCache) lookup(accessKey, secretKey string) (uint64, bool)`
- **职责**：命中返回 (edgeID, true)；未命中或过期返回 (0, false)
- **流程**：
  1. 算 key
  2. mu.Lock
  3. 查 entry；不存在 → false
  4. 存在但 `time.Now().After(expiresAt)` → `delete(entries, k)` + false（惰性清理过期项）
  5. 有效 → 返回 (edgeID, true)
- **错误处理**：无 error；过期项惰性删除

### `store`
- **签名**：`func (c *authCache) store(accessKey, secretKey string, edgeID uint64)`
- **职责**：记录一次成功验证；TTL 60s
- **流程**：
  1. 算 key
  2. mu.Lock
  3. `entries[key] = authCacheEntry{edgeID, time.Now().Add(authCacheTTL)}`
  4. mu.Unlock

## 5. 依赖关系

- **外部库**：`crypto/sha256`、`sync`、`time`
- **被调用方**：`AccessKeyAuthenticator.Authenticate`（authn.go）—— 每次 push 都先 lookup 命中跳过 argon2id
- **无内部包依赖**

## 6. 并发与资源管理

- **`mu sync.Mutex`**：保护 entries map；lookup/store 都加锁
- **`map[[32]byte]authCacheEntry`**：key 是 32 字节 SHA-256 数组（可直接做 map key）
- **无上限**：cache 不设容量上限；edge 数 × 凭据数有限，60s TTL 自动过期
- **惰性清理**：lookup 命中过期项时 delete；不主动扫表

## 7. 设计模式与亮点

- **SHA-256 key 避免明文存储**：注释明示"plaintext credentials are never stored"；即使内存 dump 也无法还原凭据
- **60s TTL 平衡**：覆盖典型高频 push 突发（同一 edge 几秒内多次 push）；保证吊销 key 在 60s 内失效
- **惰性过期清理**：lookup 命中过期项时 delete，无需后台扫表 goroutine
- **Mutex 而非 RWMutex**：lookup 也用 Lock（需要 delete 过期项）；写多读多场景 Mutex 简单够用
- **32 字节数组做 map key**：`[32]byte` 可比较，直接做 map key 无需 stringify

## 8. 注意事项

- **60s TTL 是吊销延迟**：吊销 access key 后最长 60s 内仍可用；操作员应配合 kick edge 强制重连
- **不主动清理**：依赖 lookup 惰性删除；若某些 key 再不被 lookup 会留内存（但 edge 数有限，可忽略）
- **无容量上限**：部署大量 edge 时内存占用 = edge 数 × 32 字节 key + entry，可控
- **lookup 用 Lock 不是 RLock**：因需 delete 过期项；RWMutex 收益不大
- **cache 仅缓存成功验证**：失败不入 cache；每次失败都走 argon2id（防 timing attack 区分不存在 key vs 错密码）
- **manager 重启清空**：cache 仅内存；重启后所有 push 首次走 argon2id
