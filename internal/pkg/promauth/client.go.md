# `client.go` 技术实现文档（promauth）

> 源文件：`internal/pkg/promauth/client.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/promauth`

## 1. 概述

该文件构建 promwrite / promquery 客户端用于访问 Prometheus 兼容 TSDB 的 `*http.Client`，集中处理两个关注点：TLS（dialer 层，skip-verify / 自定义 root CA，构造时解析）与 Auth（round-trip 层，Bearer / Basic，per-request 解析，带 5s TTL 缓存）。这种分离对应 Prom 自身 client_golang 的处理方式——hostname / TLS 属 dialer，headers 属 round trip。Admin UI 修改 system_settings 后无需重启 manager 即可在 TTL 窗口生效。

## 2. 包信息

- **包名**：`promauth`
- **所属模块**：`internal/pkg/`（基础设施层）
- 依赖方向：被 promwrite / promquery 客户端调用；仅依赖标准库。

## 3. 关键类型与接口

### `Config`
per-request 凭据。Bearer 优先于 Basic。

```go
type Config struct {
    BearerToken   string
    BasicUser     string
    BasicPassword string
}
```

注释说明：file-mounted bearer token（prometheus.yml `bearer_token_file` 模式）不支持——ongrid 是 docker 单体，文件挂载违背 UI 驱动配置理念。

### `TLSConfig`
静态 dialer 层 TLS 设置。CAPEM 与 CAPath 合并到同一 root pool。

```go
type TLSConfig struct {
    Insecure bool
    CAPath   string
    CAPEM    string
}
```

### `Resolver`
凭据解析接口，round-tripper 每次调用 invoke（实现方应短期 TTL 缓存）。

```go
type Resolver interface {
    Resolve(ctx context.Context) (Config, error)
}
```

### `staticResolver`
固定 Config 的 Resolver，用于测试 / system_settings 为空时回退。

### `authRoundTripper`（私有）
装饰 `http.RoundTripper`，按需注入 auth header。

```go
type authRoundTripper struct {
    base     http.RoundTripper
    resolver Resolver
    mu       sync.Mutex
    cached   Config
    cachedAt time.Time
}
```

## 4. 关键函数与流程

### `NewStaticResolver`
- **签名**：`func NewStaticResolver(cfg Config) Resolver`
- **职责**：返回固定 Config 的 Resolver。

### `BuildClient`
- **签名**：`func BuildClient(tlsCfg TLSConfig, resolver Resolver, timeout time.Duration) (*http.Client, error)`
- **职责**：构建 `*http.Client`，应用 TLS 与 auth。
- **流程**：
  1. `http.DefaultTransport.(*http.Transport).Clone()` 复制默认 transport（保留默认连接池设置）。
  2. `tlsCfg.Insecure || CAPath != "" || CAPEM != ""` 时配置 TLS：
     - `&tls.Config{MinVersion: tls.VersionTLS12}` 强制 TLS 1.2+。
     - `Insecure` → `InsecureSkipVerify = true`。
     - `CAPath != "" || CAPEM != ""` → `buildPool` 构建根 CA pool。
     - `transport.TLSClientConfig = t`。
  3. `var rt http.RoundTripper = transport`。
  4. `resolver != nil` → `rt = &authRoundTripper{base: transport, resolver: resolver}`。
  5. 返回 `&http.Client{Transport: rt, Timeout: timeout}`。
- **设计理由**：TLS 在 transport 层（dialer），auth 在 round-tripper 层，对应 Prom client_golang 的处理方式。

### `authRoundTripper.RoundTrip`
- **签名**：`func (a *authRoundTripper) RoundTrip(req *http.Request) (*http.Response, error)`
- **流程**：
  1. `a.fetch(req.Context())` 解析凭据；失败 → error `resolve creds`。
  2. `req.Clone(req.Context())` 克隆请求（避免修改调用方 Header，符合 RoundTripper 契约）。
  3. `cfg.BearerToken != ""` → `Authorization: Bearer <token>`。
  4. 否则 `cfg.BasicUser != ""` → `SetBasicAuth(user, password)`。
  5. `a.base.RoundTrip(cloned)`。
- **设计理由**：Resolver 错误"fail closed"——无 auth 请求发到真实 TSDB 看起来像网络故障，难以诊断；显式失败更明确。

### `authRoundTripper.fetch`（私有）
- **签名**：`func (a *authRoundTripper) fetch(ctx context.Context) (Config, error)`
- **流程**：
  1. `a.mu.Lock()`。
  2. `cachedAt` 非零且 `time.Since(cachedAt) < authTTL(5s)` → 返回 cached。
  3. `a.resolver.Resolve(ctx)`；失败 → 返回空 Config + error。
  4. 更新 `cached` / `cachedAt`。
  5. 返回 cfg。
- **设计理由**：5s TTL 让 admin UI 修改 system_settings 后 5s 内生效，同时避免每请求查 DB。

### `buildPool`（私有）
- **签名**：`func buildPool(tlsCfg TLSConfig) (*x509.CertPool, error)`
- **职责**：构建根 CA pool，合并 CAPEM 与 CAPath 两个来源。
- **流程**：
  1. `x509.NewCertPool()`。
  2. `CAPEM != ""` → `AppendCertsFromPEM`；失败 → error `CAPEM contained no valid certificates`。
  3. `CAPath != ""` → `os.ReadFile` + `AppendCertsFromPEM`；任一失败 → error。
  4. 返回 pool。
- **错误处理**：`%w` 包装并加 `promauth:` 前缀。

## 5. 依赖关系

- **内部包**：无。
- **外部库**：标准库 `context` / `crypto/tls` / `crypto/x509` / `errors` / `fmt` / `net/http` / `os` / `sync` / `time`。
- **被调用方**：promwrite ingester、promquery client。

## 6. 并发与资源管理

- **`sync.Mutex` 保护 cached**：`authRoundTripper.fetch` 用 mutex 保护 `cached` / `cachedAt`，并发请求共享缓存。
- **`http.Client` 并发安全**：底层 transport + round-tripper 均并发安全；多 goroutine 共享同一 client。
- **`req.Clone` 避免 Header 竞态**：RoundTripper 契约要求不修改入参 request，Clone 后设置 header 避免影响调用方。

## 7. 设计模式与亮点

- **关注点分离**：TLS（dialer 层，构造时静态）与 Auth（round-tripper 层，per-request 动态）分离，对应 Prom client_golang 处理方式。
- **Resolver 抽象**：`Resolver` 接口让凭据来源（system_settings / env / file）可插拔，调用方无需知道具体来源。
- **5s TTL 缓存**：平衡生效延迟与 DB 压力，admin UI 修改 5s 内生效。
- **fail closed**：Resolver 错误显式失败，避免无 auth 请求伪装成网络故障。
- **Clone request**：RoundTripper 严格遵守"不修改入参"契约，避免调用方 Header 被污染。
- **Bearer > Basic 优先级**：匹配 curl `-H` 优先于 `-u` 的惯例。
- **TLS 1.2+ 强制**：`MinVersion: tls.VersionTLS12`，禁用旧版本 TLS，符合安全最佳实践。
- **CAPEM + CAPath 合并**：两个来源都加入同一 pool，灵活支持内联 PEM 与文件 PEM。
- **transport.Clone**：从 `http.DefaultTransport` clone，保留默认连接池设置，仅覆盖 TLS。
- **不支持 bearer_token_file**：注释明确说明设计决策——ongrid 是 docker 单体，文件挂载违背 UI 驱动配置理念。

## 8. 注意事项

- **TLS 配置静态**：`BuildClient` 时 TLS 配置固化到 transport，修改 TLS 需重启 manager；admin UI 修改 TLS 不会热生效（auth 才热生效）。
- **5s TTL 全局**：所有用同一 client 的请求共享 5s 缓存；高频调用下 Resolver 实际每 5s 一次。
- **fail closed 可能中断监控**：Resolver 故障导致所有 Prom 请求失败；Resolver 实现需自身容错（如 system_settings 读取失败回退到上次成功值）。
- **`InsecureSkipVerify` 风险**：跳过证书校验暴露于 MITM；仅应在受控内网使用。
- **`os.ReadFile` 无缓存**：`buildPool` 每次调用读文件；但 `BuildClient` 仅在启动时调用一次，无性能问题。
- **无 mTLS 支持**：仅配置 RootCAs 用于验证服务端证书；客户端证书（mTLS）未支持，需扩展 `TLSConfig` 加 `CertFile` / `KeyFile`。
- **`http.Client.Timeout` 全局**：覆盖整个请求包括 TLS 握手与 body 读取；长查询可能超时。
- **Clone 的代价**：每请求 Clone request 有内存分配开销；高频调用下可评估优化。
