# `client.go` 技术实现文档

> 源文件：`internal/pkg/tunnel/client.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/tunnel`

## 1. 概述

本文件实现 edge 侧 tunnel 客户端，基于 `github.com/singchia/geminio` 构建到 cloud-side broker（frontier）的多路复用双向 RPC 通道。负责：建立并维持连接（含指数退避首次拨号 + geminio RetryEnd 透明重连）、注册 cloud→edge 反向 RPC handler、调用 edge→cloud RPC、接受 cloud 开启的 stream（WebSSH 路径）、reconnect 回调。所有 wire body 走 JSON 编码，日志走 slog。

## 2. 包信息

- **包名**：`tunnel`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被 edge agent 启动代码调用；依赖 `github.com/singchia/geminio` 及其子包

## 3. 关键类型与接口

```go
type geminioClient struct {
    cfg ClientConfig
    log *slog.Logger

    handlersMu sync.RWMutex
    handlers   map[string]Handler

    reconnectMu        sync.Mutex
    reconnectCallbacks []func()
    reconnectRunMu     sync.Mutex

    endPtr atomic.Pointer[geminio.End]

    connMu            sync.Mutex
    activeConn        net.Conn
    activeGeneration  uint64
    pendingConn       net.Conn
    pendingGeneration uint64
    nextGeneration    uint64

    closeOnce sync.Once
    closed    atomic.Bool
}

type retryDelegate struct {
    *delegate.UnimplementedDelegate
    client *geminioClient
}

type geminioStreamWrap struct {
    s geminio.Stream
}
```

接口 `Client` 与 `Handler` / `AuthFunc` / `StreamConn` 定义在 `types.go`。

## 4. 关键函数与流程

### `NewClient`
- **签名**：`func NewClient(cfg ClientConfig) Client`
- **职责**：构造 `geminioClient`；nil logger 用 discard text handler
- **流程**：返回 `&geminioClient{cfg, log, make(map[string]Handler)}`

### `Dial`
- **签名**：`func (c *geminioClient) Dial(ctx context.Context) error`
- **职责**：建立连接，指数退避重试直到成功或 ctx 取消
- **流程**：
  1. `closed.Load()` 检查 → 报错"client closed"
  2. `buildDialer()` 构造拨号闭包（plain TCP 或 TLS）
  3. marshal `Meta{AccessKey, SecretKey}`
  4. 退避循环（初始 1s，max 60s，每轮 ×2）：
     - 检查 ctx.Err()
     - `gclient.NewEndOptions` + SetMeta + SetDelegate(&retryDelegate)
     - `gclient.NewRetryEndWithDialer(dialer, opt)` — geminio 内部管重连
     - 成功：`promotePendingConnection()`、`endPtr.Store(&end)`、re-apply 已注册 handler、log "connected"、返回 nil
     - 失败：Warn 日志、select ctx.Done 或 time.After(backoff)
- **错误处理**：auth/credential 错误无法在网络层区分，持续重试 + Warn 让运维看到持续失败

### `buildDialer`
- **签名**：`func (c *geminioClient) buildDialer() (gclient.Dialer, error)`
- **职责**：构造 net.Dial 或 tls.Dial 闭包
- **流程**：
  - addr 空 → 报错
  - 无 CA 文件：`net.Dialer{Timeout: 10s}`，拨号后 `trackConnection(conn)`
  - 有 CA 文件：读 PEM、`x509.NewCertPool().AppendCertsFromPEM`、`tls.Config{RootCAs, MinVersion: TLS1.2}`，`tls.Client(raw, tlsCfg)` 后 track
- **错误处理**：读 CA 失败 `%w`；PEM 无效 cert 报错

### `RegisterHandler`
- **签名**：`func (c *geminioClient) RegisterHandler(method string, h Handler)`
- **职责**：注册 cloud→edge RPC handler；Dial 前后都可调
- **流程**：
  1. `handlersMu.Lock`，写入 map，Unlock
  2. 若 end 已加载：`registerOn(end, method, h)`；失败 Warn
- **错误处理**：live 注册失败仅 Warn，不阻塞调用方

### `registerOn`
- **签名**：`func (c *geminioClient) registerOn(end geminio.End, method string, h Handler) error`
- **职责**：在指定 end 上注册 handler，包装成 geminio 签名
- **流程**：wrapper 函数把 `(ctx, req, rsp)` 转成 `(ctx, Session{}, method, body)` 调用 h；h 返回 error 时 `rsp.SetError`，否则 `rsp.SetData(out)`；`end.Register(ctx, method, wrapper)`

### `Call`
- **签名**：`func (c *geminioClient) Call(ctx, method string, req, resp any) error`
- **职责**：调用 cloud 侧 RPC；不重试，应用错误不强制二次连接
- **流程**：
  1. `loadEnd()` nil → "not dialed"
  2. `json.Marshal(req)` → `end.NewRequest(body)` → `end.Call(ctx, method, req)`
  3. 记录 `connectionGeneration()` 用于 broken route 回收
  4. callErr：`recycleBrokenRoute(method, callErr, gen)` + `%w` 包装
  5. `rsp.Error()`：同样 recycle + `%w`
  6. `resp == nil` → 返回 nil
  7. `json.Unmarshal(rsp.Data(), resp)` → 失败 `%w`
- **错误处理**：marshal/unmarshal 失败 `%w`；网络/remote 错误先尝试 recycle broken route 再包装

### `trackConnection / promotePendingConnection / connectionGeneration / recycleBrokenRoute`
- **签名**：私有方法管理连接代际
- **职责**：解决"RetryEnd 切换底层 End 时如何安全回收旧 broken route"
- **流程**：
  - `trackConnection`：新连接标记为 pending（nextGeneration++）
  - `promotePendingConnection`：RetryEnd 切换后调用，把 pending 升级为 active
  - `connectionGeneration`：返回当前 active 代次
  - `recycleBrokenRoute`：仅当错误是 frontier routing 错（`shouldRecycleBrokenRoute` 判定）且 generation 匹配 active 时，关闭 active 连接让 RetryEnd 重连
- **错误处理**：`shouldRecycleBrokenRoute` 仅对 `register_edge` / `heartbeat` 的特定错误返回 true（如 "no such rpc"、"mismatch clientID"、"register_edge: get edge: not found"）

### `OnReconnect / fireReconnectCallbacks`
- **签名**：`func (c *geminioClient) OnReconnect(fn func())` + 私有 fire
- **职责**：注册重连后回调；按注册顺序串行执行
- **流程**：`reconnectMu.Lock` append；fire 在 `reconnectRunMu` 串行；每个回调 defer/recover 防止 panic 杀死重连 goroutine
- **错误处理**：回调 panic 仅 Warn，不影响后续回调

### `Close`
- **签名**：`func (c *geminioClient) Close() error`
- **职责**：终止连接，停止重连
- **流程**：`closeOnce.Do`：`closed.Store(true)`、`end.Close()`、清空 pending/active conn、关闭 pending conn

### `AcceptStream`
- **签名**：`func (c *geminioClient) AcceptStream() (StreamConn, error)`
- **职责**：阻塞直到 cloud 开 stream
- **流程**：loadEnd nil → "not dialed"；`end.AcceptStream()` → 包成 `geminioStreamWrap`

## 5. 依赖关系

- **内部包**：无
- **外部库**：
  - `github.com/singchia/geminio`
  - `gclient "github.com/singchia/geminio/client"`
  - `github.com/singchia/geminio/delegate`
- **被调用方**：edge agent 启动代码（`internal/edgeagent`）

## 6. 并发与资源管理

- **多把锁分工**：
  - `handlersMu`（RWMutex）：保护 handlers map
  - `reconnectMu`（Mutex）：保护 reconnectCallbacks 切片
  - `reconnectRunMu`（Mutex）：串行化回调执行
  - `connMu`（Mutex）：保护 active/pending conn 代际状态
  - `closeOnce`（Once）：保证 Close 幂等
- **`atomic.Pointer[geminio.End]`**：无锁读取当前 end
- **`atomic.Bool closed`**：无锁检查关闭状态
- **连接代际机制**：trackConnection 标 pending、promotePendingConnection 升级 active、recycleBrokenRoute 按 generation 安全回收。避免"手动构造第二个 RetryEnd 与仍活的第一 transport 竞争"
- **回调 panic 隔离**：每个 OnReconnect 回调 defer/recover，防 panic 死锁重连 goroutine
- **RetryEnd 内部管重连**：首次 Dial 后断线由 geminio 自动重连；`retryDelegate.EndReOnline` 在重连完成时被回调

## 7. 设计模式与亮点

- **Delegate 模式**：`retryDelegate` 嵌入 `UnimplementedDelegate` 并覆盖 `EndReOnline`，让 geminio 在重连完成时通知本包
- **代际连接管理**：pending → active 升级 + generation 校验，解决"旧 broken route 误关新候选连接"的竞态
- **`shouldRecycleBrokenRoute` 精确判定**：仅对特定 frontier routing 错误回收连接，避免应用错误触发不必要重连
- **回调隔离**：OnReconnect 回调 panic 不杀重连 goroutine，下次重连不会死锁
- **closeOnce 幂等**：多次 Close 安全
- **`geminioStreamWrap` 窄接口**：只暴露 `StreamConn` 表面，防止 caller 耦合 geminio 内部
- **Dial 前 RegisterHandler 支持**：handler 存 map，Dial 成功后 prime；后续重连由 RetryEnd 自动 re-register

## 8. 注意事项

- **auth 错误不可识别**：注释明示 server 关连接时本层无法区分 auth 与网络错误，持续重试 + Warn 让运维介入
- **代际机制复杂**：pending/active/next generation 三态管理易出错；修改时务必理解 `promotePendingConnection` 仅在 RetryEnd 切换后调用
- **`recycleBrokenRoute` 仅对 register_edge/heartbeat**：其他 method 的 routing 错误不回收，可能让 stale route 持续失败；caller 需自行重试
- **`OnReconnect` 回调异步**：注释明示"Callbacks run asynchronously after the underlying reconnect lock is released"，回调内不能假设 reconnect 完全结束
- **`AcceptStream` 阻塞**：无超时；caller 需在 goroutine 中调用并自行 ctx 取消
- **TLS CA 文件路径**：`os.ReadFile` 失败仅启动时；运行中 CA 轮换需重启
- **`Meta` 含 SecretKey**：明文 JSON 进 geminio Meta blob；若 transport 未加密（无 TLS）有泄漏风险，生产必须配 TLS
