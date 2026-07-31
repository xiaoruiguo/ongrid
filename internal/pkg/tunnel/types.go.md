# `types.go` 技术实现文档

> 源文件：`internal/pkg/tunnel/types.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/tunnel`

## 1. 概述

本文件定义 `tunnel` 包的核心接口与配置类型：`Session`（已认证 edge 身份）、`Handler`（cloud→edge RPC handler 签名）、`AuthFunc`（access/secret key 校验签名）、`ClientConfig`（edge 侧配置）、`Client` 接口（Dial/RegisterHandler/Call/AcceptStream/OnReconnect/Close）、`StreamConn`（stream 窄接口）。是 `client.go` 实现的契约层。

## 2. 包信息

- **包名**：`tunnel`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被 `client.go`（`geminioClient`）实现、被 edge agent 启动代码调用；仅依赖标准库 `context`、`log/slog`

## 3. 关键类型与接口

```go
type Session struct {
    EdgeID uint64
    // MVP 单租户；public 多租户时加 OrgID
}

type Handler func(ctx context.Context, s Session, method string, body []byte) ([]byte, error)

type AuthFunc func(ctx context.Context, accessKey, secretKey string) (Session, error)

type ClientConfig struct {
    ServerAddr string  // cloud tunnel endpoint，如 "cloud.example.com:11000"
    CloudAddr  string  // legacy 别名；ServerAddr 优先
    AccessKey  string
    SecretKey  string
    TLSCAFile  string  // 可选 CA PEM
    TLSCA      string  // legacy 别名
    Log        *slog.Logger
}

type Client interface {
    Dial(ctx context.Context) error
    RegisterHandler(method string, h Handler)
    Call(ctx context.Context, method string, req, resp any) error
    AcceptStream() (StreamConn, error)
    OnReconnect(fn func())
    Close() error
}

type StreamConn interface {
    Read(p []byte) (int, error)
    Write(p []byte) (int, error)
    Close() error
    Meta() []byte
}
```

## 4. 关键函数与流程

### `ClientConfig.resolvedServerAddr`
- **签名**：`func (c ClientConfig) resolvedServerAddr() string`
- **职责**：返回 ServerAddr，否则 CloudAddr（legacy 兼容）
- **流程**：ServerAddr 非空返回它；否则返回 CloudAddr

### `ClientConfig.resolvedTLSCA`
- **签名**：`func (c ClientConfig) resolvedTLSCA() string`
- **职责**：返回 TLSCAFile，否则 TLSCA（legacy 兼容）

## 5. 依赖关系

- **内部包**：无
- **外部库**：无
- **被调用方**：
  - `client.go`：`geminioClient` 实现 `Client` 接口，使用 `ClientConfig` / `Handler` / `Session`
  - edge agent 启动代码：构造 `ClientConfig`，调用 `NewClient`，`RegisterHandler` 注册各 method handler
  - manager 侧 frontierbound：实现 `AuthFunc`（`biz/edge.AccessKeyAuthenticator`）

## 6. 并发与资源管理

接口层无并发控制。`ClientConfig` 是值类型可安全并发传递。`Session` 是值类型，`EdgeID` 只读。具体实现（`geminioClient`）在 `client.go` 中用多把锁管理并发。

## 7. 设计模式与亮点

- **接口与实现分离**：`Client` 是接口，`geminioClient` 是实现；`NewClient` 返回接口让测试可 mock
- **`Handler` 签名统一**：`(ctx, Session, method, body) ([]byte, error)` — method 名传入让一个 handler 服务多个 RPC（如 dispatcher 模式）
- **`AuthFunc` 注入**：cloud 侧 authenticator 实现，frontierbound service-end wrapper 在每次 edge dial 时调用。让本包不依赖 manager biz
- **legacy 别名兼容**：`CloudAddr` / `TLSCA` 是 Phase 1 命名，`ServerAddr` / `TLSCAFile` 是新名；`resolvedXxx` 方法让两套共存
- **`StreamConn` 窄接口**：仅 `Read/Write/Close/Meta`，比 `net.Conn` 窄（无 LocalAddr/RemoteAddr/SetDeadline 等），防止 caller 耦合不必要的 API。注释明示"keeps the tunnel layer generic"
- **`Session` MVP 单字段**：注释明示"MVP is single-tenant private deployment, so no OrgID here yet"；多租户时扩展
- **`AcceptStream` 注释详尽**：解释 WebSSH 路径用法（manager open stream, edge accept + io.Copy to/from sshd），Meta 携带目标描述符

## 8. 注意事项

- **`Session` 是 edge 身份**：注释明示"edge isn't authenticated against a specific edge ID; its identity is implicit (one End per edge)"，edge 侧 Session 始终零值
- **legacy 别名优先级**：ServerAddr 优先于 CloudAddr；同时设时 CloudAddr 被忽略
- **`AuthFunc` 在 cloud 侧实现**：本包不提供；manager 侧 `biz/edge.AccessKeyAuthenticator` 实现
- **`StreamConn` 无 deadline**：窄接口不含 SetDeadline；若需超时由 caller ctx 控制
- **`OnReconnect` 回调串行**：注释明示"Multiple callbacks may be registered; they run sequentially"
- **`AcceptStream` 阻塞**：无超时；caller 需在 goroutine 中调用
- **`ClientConfig.Log` 可选**：nil 时 `NewClient` 用 discard text handler
- **多租户扩展**：`Session` 当前仅 `EdgeID`；public 多租户时需加 `OrgID` 并更新 `AuthFunc` 契约
