# OnGrid RPC（singchia/geminio）技术实现说明文档

> 本文档深入分析 OnGrid 系统中基于 `github.com/singchia/geminio` 构建的 RPC 隧道（tunnel）子系统的全部技术实现，覆盖：架构定位、edge 侧 tunnel client、manager 侧 frontierbound wrapper、wire 协议、生命周期回调、反向 RPC handler、双向 stream（WebSSH）、重连与代际连接管理、装配流程、并发模型与架构红线。

---

## 目录

1. [概述与架构定位](#1-概述与架构定位)
2. [依赖与版本](#2-依赖与版本)
3. [拓扑：edge ↔ frontier broker ↔ manager](#3-拓扑edge--frontier-broker--manager)
4. [Wire 协议：JSON over geminio](#4-wire-协议json-over-geminio)
5. [edge 侧 tunnel.Client 实现](#5-edge-侧-tunnelclient-实现)
6. [manager 侧 frontierbound.Client 实现](#6-manager-侧-frontierboundclient-实现)
7. [生命周期回调与身份解析](#7-生命周期回调与身份解析)
8. [反向 RPC Handler（manager 侧 Install）](#8-反向-rpc-handlermanager-侧-install)
9. [双向 Stream 与 WebSSH](#9-双向-stream-与-webssh)
10. [重连机制与代际连接管理](#10-重连机制与代际连接管理)
11. [Edge Agent 使用模式](#11-edge-agent-使用模式)
12. [cmd/ongrid/main.go 装配流程](#12-cmdongridmaingo-装配流程)
13. [并发与资源管理](#13-并发与资源管理)
14. [设计模式与架构红线](#14-设计模式与架构红线)
15. [关键文件索引](#15-关键文件索引)

---

## 1. 概述与架构定位

OnGrid 是一个云边协同（cloud-edge）的可观测性与 AIOps 平台。云端 manager 需要与部署在用户机房的 edge agent 进行双向通信：

- **edge → cloud**：注册、心跳、推指标、推 K8s inventory、推 WebSSH 输出
- **cloud → edge**：取实时负载、取进程列表、describe K8s 资源、查 K8s 日志、执行 K8s 写动作、执行 skill、WebSSH 输入、agent 升级、插件配置变更通知、写 DB exporter 凭据

OnGrid 没有自研 RPC server，而是基于 `github.com/singchia/geminio`（一个支持多路复用、双向 RPC、stream 的 Go RPC 库）构建隧道子系统：

| 角色 | 实现 | 依赖 |
|------|------|------|
| **edge client** | `internal/pkg/tunnel/` | `github.com/singchia/geminio` + `geminio/client` + `geminio/delegate` |
| **manager client** | `internal/manager/service/frontierbound/` | `github.com/singchia/frontier/api/dataplane/v1/service`（封装 geminio） |
| **broker** | 外部 `github.com/singchia/frontier` 容器 | 终结 geminio 协议，不在本 repo |

**架构边界声明**（来自 `internal/pkg/tunnel/doc.go`）：

> the cloud-side listening end is no longer in this repo; the upstream github.com/singchia/frontier broker terminates geminio for us and the manager dials it via internal/manager/service/frontierbound. Only the edge keeps a NewClient here.

即：cloud 侧的 geminio listening end 由独立的 frontier broker 容器承担，manager 通过 `frontierbound` 包以 **service-end** 身份拨号到 frontier，edge 通过 `tunnel` 包以 **client-end** 身份拨号到 frontier。两者都不直接互相拨号，而是经由 frontier broker 中转。

---

## 2. 依赖与版本

来自 `go.mod`：

```
github.com/singchia/geminio v1.3.0-rc.2
```

`frontier` 的 service SDK 通过 `frontierbound/client.go` 引入：

```go
import (
    fbsvc "github.com/singchia/frontier/api/dataplane/v1/service"
    "github.com/singchia/geminio"
)
```

`frontier` 本身作为独立容器部署（见 `deploy/install/frontier.yaml`、`deploy/Dockerfile.frontier`），manager 和 edge 都不内嵌 frontier。

---

## 3. 拓扑：edge ↔ frontier broker ↔ manager

```
   ┌─────────────┐         geminio (TCP/TLS)        ┌──────────────┐
   │  edge agent │ ◄──────────────────────────────► │  frontier    │
   │  (tunnel.   │   edge 拨号到 frontier            │  broker      │
   │   Client)   │                                  │  (独立容器)   │
   └─────────────┘                                  └──────┬───────┘
                                                           │
                                          geminio service  │
                                          (manager 拨号)    │
                                                           ▼
                                                   ┌──────────────┐
                                                   │   manager    │
                                                   │ (frontier-   │
                                                   │  bound.Client)│
                                                   └──────────────┘
```

**关键点**：

- edge 与 manager **不直接**连接，都经由 frontier broker
- frontier broker 是 geminio 协议的终结点，负责将 manager 的 service-end 调用路由到对应 edge 的 client-end
- frontier 给每个 edge 分配一个 **opaque 64-bit transport ID**，与业务 edgeID 解耦（见 §6 binding 管理）
- frontier 提供 service-end SDK（`fbsvc.Service`），manager 通过它注册反向 RPC handler 和生命周期回调

---

## 4. Wire 协议：JSON over geminio

源文件：`api/tunnel/v1/tunnel.proto`、`internal/pkg/tunnel/messages.go`

### 4.1 协议形态

- **传输层**：geminio（多路复用 TCP/TLS 长连接）
- **RPC body 编码**：JSON（MVP 阶段，ADR-001）
- **proto 文件作用**：仅作为 message shape 的规范文档，**不生成 Go 代码**

### 4.2 手镜像而非代码生成

`messages.go` 顶部注释明确说明：

```go
// This file hand-mirrors api/tunnel/v1/tunnel.proto message shapes as Go
// structs with JSON tags. the tunnel body wire format is JSON
// in MVP; we deliberately avoid generating protobuf Go types for these
// payloads so internal/pkg/tunnel/ stays dependency-free (no protobuf
// import, no generated-code directory). When (if) we switch to protobuf
// binary in Phase 2, this file is the seam: swap types here, keep
// callers unchanged.
```

**设计决策**：保持 `internal/pkg/tunnel` 零外部依赖（不引 protobuf），Phase 2 切 protobuf binary 时 `messages.go` 是唯一替换 seam。

### 4.3 RPC Method 常量

所有 RPC method 名集中定义在 `messages.go`，调用方 spell-safe：

```go
const (
    // edge → cloud
    MethodRegisterEdge              = "register_edge"
    MethodHeartbeat                 = "heartbeat"
    MethodPushHostMetrics           = "push_host_metrics"
    MethodPushPromSamples           = "push_prom_samples"
    MethodPushK8sInventory          = "push_k8s_inventory"
    MethodGetPluginConfigs          = "get_plugin_configs"

    // cloud → edge
    MethodDescribeK8sResource       = "describe_k8s_resource"
    MethodQueryK8sLogs              = "query_k8s_logs"
    MethodExecuteK8sAction          = "execute_k8s_action"
    MethodGetHostLoad               = "get_host_load"
    MethodGetProcessList            = "get_process_list"
    MethodGetNetstat                = "get_netstat"
    MethodExecuteSkill              = "execute_skill"
    MethodPluginConfigsChanged      = "plugin_configs_changed"
    MethodWriteDatabaseMetricsSecret = "write_database_metrics_secret"

    // WebSSH（双向）
    MethodShellOpen   = "shell_open"   // cloud → edge
    MethodShellInput  = "shell_input"  // cloud → edge
    MethodShellResize = "shell_resize" // cloud → edge
    MethodShellClose  = "shell_close"  // cloud → edge
    MethodShellOutput = "shell_output" // edge → cloud
    MethodShellExit   = "shell_exit"   // edge → cloud

    // Agent 升级（cloud → edge）
    MethodAgentUpgrade = "agent_upgrade"
    MethodFetchPackage = "fetch_package"
    MethodApplyPackage = "apply_package"
)
```

### 4.4 关键 wire 结构（节选）

```go
// 握手 Meta（非 RPC body，序列化进 geminio Meta bytes）
type Meta struct {
    AccessKey string `json:"access_key"`
    SecretKey string `json:"secret_key"`
}

// 注册
type RegisterEdgeRequest struct {
    AccessKey    string          `json:"access_key"`
    SecretKey    string          `json:"secret_key"`
    HostInfo     HostInfo        `json:"host_info"`
    AgentVersion string          `json:"agent_version,omitempty"`
    Kubernetes   *KubernetesInfo `json:"kubernetes,omitempty"`
}

type HostInfo struct {
    Hostname      string `json:"hostname"`
    OS            string `json:"os"`
    Arch          string `json:"arch"`
    KernelVersion string `json:"kernel_version"`
    CPUCount      int    `json:"cpu_count"`
    MemTotalBytes uint64 `json:"mem_total_bytes"`
    Fingerprint        string `json:"fingerprint,omitempty"`         // /etc/machine-id
    HardwareFingerprint string `json:"hardware_fingerprint,omitempty"` // 物理 NIC MAC + CPU + disk serial（克隆抗性）
    IPAddress          string `json:"ip_address,omitempty"`
}

// 心跳（piggyback 插件健康）
type HeartbeatRequest struct {
    EdgeID      uint64             `json:"edge_id,omitempty"`
    Ts          int64              `json:"ts"`
    StatusFlags map[string]string  `json:"status_flags,omitempty"`
    Plugins     []PluginHealthWire `json:"plugins,omitempty"`
}

// Prometheus 样本
type PromSample struct {
    Name   string            `json:"name"`
    Labels map[string]string `json:"labels,omitempty"`
    Value  float64           `json:"value"`
    TsMs   int64             `json:"ts_ms"`
}

// K8s inventory（支持分块快照）
type KubernetesInventoryRequest struct {
    EdgeID            uint64
    ClusterID         uint64
    SnapshotID        string
    ChunkIndex        int
    ChunkCount        int
    Nodes             []KubernetesNodeSnapshot
    Workloads         []KubernetesWorkloadSnapshot
    Pods              []KubernetesPodSnapshot
    Events            []KubernetesEventSnapshot
    DeletedNodes      []KubernetesNodeRef
    DeletedWorkloads  []KubernetesWorkloadRef
    // ...
}

// WebSSH
type ShellOpenRequest struct {
    SessionID string `json:"session_id"`
    Cols      uint16 `json:"cols"`
    Rows      uint16 `json:"rows"`
    Term      string `json:"term"`
    SSHHost   string `json:"ssh_host"`
    SSHUser   string `json:"ssh_user"`
    SSHPass   string `json:"ssh_pass"` // wiped after Dial
}
```

### 4.5 协议设计要点

- **`HardwareFingerprint` 克隆抗性**：物理 NIC MAC + CPU model + disk serials，不被克隆 Linux VM 折叠（issue #96）；cloud 优先用它，回退到 `Fingerprint`（gopsutil HostID）
- **`PluginHealthWire` 解耦**：edge 在 plugin runtime 与 wire 间映射；`State` 为 stopped/starting/running/crashed；`LastError` 把静默失败变成 operator-visible 原因
- **`KubernetesInventoryRequest` 分块**：大集群快照分批推送，`SnapshotID` 关联同一次快照
- **`DiskUsedPct` 字段**：`GetHostLoadResponse` 加入此字段修复了 LLM 误读"disk usage = mem_pct"的真实 session 案例
- **`Meta` 与 RPC body 分离**：Meta 是握手 blob，非 RPC body；server 在 AuthFunc 前解码
- **`SSHPass` 敏感**：`ShellOpenRequest.SSHPass` 注释明示"one-shot and wiped from edge memory after Dial; never logged, never stored"，但 wire 仍明文，**必须配 TLS**

---

## 5. edge 侧 tunnel.Client 实现

源文件：`internal/pkg/tunnel/client.go`、`types.go`

### 5.1 Client 接口（types.go）

```go
type Client interface {
    Dial(ctx context.Context) error
    RegisterHandler(method string, h Handler)
    Call(ctx context.Context, method string, req, resp any) error
    AcceptStream() (StreamConn, error)
    OnReconnect(fn func())
    Close() error
}

type Handler func(ctx context.Context, s Session, method string, body []byte) ([]byte, error)

type Session struct {
    EdgeID uint64
}

type StreamConn interface {
    Read(p []byte) (int, error)
    Write(p []byte) (int, error)
    Close() error
    Meta() []byte
}
```

### 5.2 geminioClient 结构（client.go）

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
```

### 5.3 Dial —— 指数退避首次拨号

```go
func (c *geminioClient) Dial(ctx context.Context) error
```

**流程**：

1. `closed.Load()` 检查
2. `buildDialer()` 构造拨号闭包（plain TCP 或 TLS）
3. marshal `Meta{AccessKey, SecretKey}`
4. **指数退避循环**（初始 1s，max 60s，每轮 ×2）：
   - 检查 `ctx.Err()`
   - `gclient.NewEndOptions()` + `SetMeta(meta)` + `SetDelegate(&retryDelegate{...})`
   - `gclient.NewRetryEndWithDialer(dialer, opt)` —— **geminio 内部管重连**
   - 成功：`promotePendingConnection()`、`endPtr.Store(&end)`、re-apply 已注册 handler、log "connected"、返回 nil
   - 失败：Warn 日志、`select ctx.Done 或 time.After(backoff)`

**注释要点**：

> Auth / credential errors can't be distinguished from network errors at this layer (the server just closes the connection). Keep retrying with the capped backoff but log at warn; ops will see continuous failures if the key is truly wrong.

### 5.4 buildDialer —— TCP / TLS

```go
func (c *geminioClient) buildDialer() (gclient.Dialer, error)
```

- **无 CA 文件**：`net.Dialer{Timeout: 10s}`，拨号后 `trackConnection(conn)`
- **有 CA 文件**：读 PEM、`x509.NewCertPool().AppendCertsFromPEM`、`tls.Config{RootCAs, MinVersion: TLS1.2}`、`tls.Client(raw, tlsCfg)` 后 track
- `normalizeOpenAIBaseURL` 补 `/v1` 风格的路径补齐在此不适用（这是 LLM 客户端的），tunnel 直接用 cfg.addr

### 5.5 retryDelegate —— Delegate 模式接管重连通知

```go
func (d *retryDelegate) EndReOnline(_ delegate.ClientDescriber) {
    d.client.promotePendingConnection()
    // RetryEnd calls this while its reconnect lock is held. A callback may
    // issue RPCs, so run it after the reinitialisation stack has unwound.
    go d.client.fireReconnectCallbacks()
}
```

`retryDelegate` 嵌入 `delegate.UnimplementedDelegate` 并覆盖 `EndReOnline`，让 geminio 在重连完成时通知本包。本包借此完成 pending → active 连接升级 + 触发 OnReconnect 回调。

### 5.6 RegisterHandler —— Dial 前后都可调

```go
func (c *geminioClient) RegisterHandler(method string, h Handler) {
    c.handlersMu.Lock()
    c.handlers[method] = h
    c.handlersMu.Unlock()

    if end := c.loadEnd(); end != nil {
        if err := c.registerOn(end, method, h); err != nil {
            c.log.Warn("tunnel: live RegisterHandler failed", ...)
        }
    }
}
```

- 写入 map；若 end 已加载则 live 注册
- Dial 成功后会从 map re-apply 所有 handler（见 §5.3 step "re-apply 已注册 handler"）
- 后续重连由 RetryEnd 自动 re-register（geminio 内部记忆已注册 RPC）

### 5.7 registerOn —— Handler 签名适配

```go
func (c *geminioClient) registerOn(end geminio.End, method string, h Handler) error {
    wrapper := func(ctx context.Context, req geminio.Request, rsp geminio.Response) {
        // Session is always the zero value on the client side — the
        // client isn't authenticated against a specific edge ID; its
        // own identity is implicit (the end talks to one cloud).
        out, err := h(ctx, Session{}, req.Method(), req.Data())
        if err != nil {
            rsp.SetError(err)
            return
        }
        rsp.SetData(out)
    }
    return end.Register(context.Background(), method, wrapper)
}
```

**注释要点**：edge 侧 Session 始终零值，因为 edge 身份是隐式的（一个 End 对应一个 cloud）。

### 5.8 Call —— edge → cloud RPC

```go
func (c *geminioClient) Call(ctx context.Context, method string, req, resp any) error {
    end := c.loadEnd()
    if end == nil {
        return errors.New("tunnel: not dialed")
    }
    body, err := json.Marshal(req)
    if err != nil {
        return fmt.Errorf("marshal %q req: %w", method, err)
    }
    connGeneration := c.connectionGeneration()
    rsp, callErr := end.Call(ctx, method, end.NewRequest(body))
    if callErr != nil {
        c.recycleBrokenRoute(method, callErr, connGeneration)
        return fmt.Errorf("tunnel call %q: %w", method, callErr)
    }
    if rerr := rsp.Error(); rerr != nil {
        c.recycleBrokenRoute(method, rerr, connGeneration)
        return fmt.Errorf("tunnel call %q: remote: %w", method, rerr)
    }
    if resp == nil {
        return nil
    }
    if err := json.Unmarshal(rsp.Data(), resp); err != nil {
        return fmt.Errorf("unmarshal %q resp: %w", method, err)
    }
    return nil
}
```

**注释要点**：

> RetryEnd owns transport recovery; application errors must not force a second connection while the current transport is still live.

即：应用错误不强制二次连接，只有 frontier routing 错误才回收（见 §10）。

### 5.9 AcceptStream —— 接受 cloud 开的 stream

```go
func (c *geminioClient) AcceptStream() (StreamConn, error) {
    end := c.loadEnd()
    if end == nil {
        return nil, errors.New("tunnel: not dialed")
    }
    s, err := end.AcceptStream()
    if err != nil {
        return nil, err
    }
    return geminioStreamWrap{s: s}, nil
}

type geminioStreamWrap struct {
    s geminio.Stream
}

func (w geminioStreamWrap) Read(p []byte) (int, error)  { return w.s.Read(p) }
func (w geminioStreamWrap) Write(p []byte) (int, error) { return w.s.Write(p) }
func (w geminioStreamWrap) Close() error                { return w.s.Close() }
func (w geminioStreamWrap) Meta() []byte                { return w.s.Meta() }
```

**用途**：WebSSH 路径。manager open stream → edge accept → edge 根据 `Meta()` 解码目标（如 `{"target":"127.0.0.1:22"}`）→ dial 本地 sshd → `io.Copy` 双向转发字节。

### 5.10 OnReconnect / fireReconnectCallbacks

```go
func (c *geminioClient) OnReconnect(fn func()) {
    if fn == nil {
        return
    }
    c.reconnectMu.Lock()
    c.reconnectCallbacks = append(c.reconnectCallbacks, fn)
    c.reconnectMu.Unlock()
}

func (c *geminioClient) fireReconnectCallbacks() {
    c.reconnectRunMu.Lock()
    defer c.reconnectRunMu.Unlock()

    if c.closed.Load() {
        return
    }
    c.reconnectMu.Lock()
    cbs := append([]func(){}, c.reconnectCallbacks...)
    c.reconnectMu.Unlock()
    for _, fn := range cbs {
        // Each callback wrapped in defer/recover so a panicking handler
        // can't kill the reconnect goroutine — the next reconnect
        // would then deadlock on the reconnecting flag.
        func() {
            defer func() {
                if r := recover(); r != nil {
                    c.log.Warn("tunnel: OnReconnect callback panicked", slog.Any("recover", r))
                }
            }()
            fn()
        }()
    }
}
```

**注释要点**：

> Callbacks run asynchronously after the underlying reconnect lock is released.

即：回调在 `EndReOnline` 中 `go fireReconnectCallbacks()` 异步执行，不阻塞 geminio 重连锁；每个回调 defer/recover 防止 panic 死锁重连 goroutine。

### 5.11 Close —— 幂等关闭

```go
func (c *geminioClient) Close() error {
    var closeErr error
    c.closeOnce.Do(func() {
        c.closed.Store(true)
        if end := c.loadEnd(); end != nil {
            closeErr = end.Close()
        }
        c.connMu.Lock()
        pending := c.pendingConn
        c.pendingConn = nil
        c.pendingGeneration = 0
        c.activeConn = nil
        c.activeGeneration = 0
        c.connMu.Unlock()
        if pending != nil {
            if err := pending.Close(); err != nil {
                c.log.Debug("tunnel: close pending connection", slog.Any("err", err))
            }
        }
    })
    return closeErr
}
```

`closeOnce sync.Once` 保证幂等；关闭 active end + pending conn。

---

## 6. manager 侧 frontierbound.Client 实现

源文件：`internal/manager/service/frontierbound/client.go`

### 6.1 service 窄接口切片

```go
type service interface {
    NewRequest(data []byte) geminio.Request
    Call(ctx context.Context, edgeID uint64, method string, req geminio.Request) (geminio.Response, error)
    Register(ctx context.Context, method string, rpc geminio.RPC) error
    RegisterGetEdgeID(ctx context.Context, fn fbsvc.GetEdgeID) error
    RegisterEdgeOnline(ctx context.Context, fn fbsvc.EdgeOnline) error
    RegisterEdgeOffline(ctx context.Context, fn fbsvc.EdgeOffline) error
    OpenStream(ctx context.Context, edgeID uint64) (geminio.Stream, error)
    Close() error
}

var _ service = (fbsvc.Service)(nil)  // 编译期检查
```

**设计**：仅暴露本包实际用到的方法子集；编译期 `var _ service = (fbsvc.Service)(nil)` 检查上游类型满足；测试用 fake 注入（`newWithService`）。

### 6.2 Client 结构

```go
type Client struct {
    svc service
    log *slog.Logger

    mu                sync.RWMutex
    transportToEdgeID map[uint64]uint64  // frontier transport ID → 业务 edgeID
    edgeIDToTransport map[uint64]uint64  // 业务 edgeID → frontier transport ID
    transportAddrs    map[uint64]string  // transport ID → remote addr
    k8sControllers    map[uint64]bool    // edgeID → 是否 K8s controller
}
```

### 6.3 构造

- **`New(cfg, log)`**：
  1. cfg.Addr 空 → error
  2. dialer 闭包 `net.Dial("tcp", cfg.Addr)`
  3. opts 附加 `OptionServiceName`（非空时）
  4. `fbsvc.NewService(dialer, opts...)` 失败 `%w`
  5. 初始化四个 map；Info 日志记录 addr + service_name
- **`newWithService(svc, log)`**：测试 seam，注入 fake service
- **`NewDisabled(log)`**：svc=nil；出站调用返 `ErrDisabled`；Register* 全 no-op；用于 `ONGRID_FRONTIER_DISABLED=true` 路径（e2e / degraded-broker）

### 6.4 Call —— manager → edge RPC

```go
func (c *Client) Call(ctx context.Context, edgeID uint64, method string, body []byte) ([]byte, error) {
    if c.svc == nil {
        return nil, ErrDisabled
    }
    transportID := c.resolveTransportID(edgeID)
    req := c.svc.NewRequest(body)
    rsp, err := c.svc.Call(ctx, transportID, method, req)
    if err != nil {
        return nil, fmt.Errorf("frontierbound: call %q edge=%d transport=%d: %w", method, edgeID, transportID, err)
    }
    if rerr := rsp.Error(); rerr != nil {
        return nil, fmt.Errorf("frontierbound: remote %q edge=%d transport=%d: %w", method, edgeID, transportID, rerr)
    }
    return rsp.Data(), nil
}
```

**流程**：`resolveTransportID(edgeID)` 查 edgeID→transport 映射；未命中返回 edgeID 本身（直连场景 transportID==edgeID）；调 `svc.Call`；err / remote error 都 `%w` 包装含 method/edgeID/transportID 上下文。

### 6.5 Binding 管理（核心）

#### bindEdgeTransportAt

```go
func (c *Client) bindEdgeTransportAt(transportID, edgeID uint64, addr string) {
    if transportID == 0 || edgeID == 0 {
        return
    }
    c.mu.Lock()
    defer c.mu.Unlock()
    // 清除旧绑定（双向）
    if prevEdgeID, ok := c.transportToEdgeID[transportID]; ok && prevEdgeID != edgeID {
        delete(c.edgeIDToTransport, prevEdgeID)
        delete(c.transportAddrs, transportID)
    }
    if prevTransportID, ok := c.edgeIDToTransport[edgeID]; ok && prevTransportID != transportID {
        delete(c.transportToEdgeID, prevTransportID)
        delete(c.transportAddrs, prevTransportID)
    }
    // 写入双向映射
    c.transportToEdgeID[transportID] = edgeID
    c.edgeIDToTransport[edgeID] = transportID
    if addr != "" {
        c.transportAddrs[transportID] = addr
    }
}
```

#### unbindEdgeTransport —— 三重校验防 stale offline

```go
func (c *Client) unbindEdgeTransport(transportID, canonicalEdgeID uint64, addr string) bool {
    if transportID == 0 {
        return false
    }
    c.mu.Lock()
    defer c.mu.Unlock()
    mappedEdgeID, mapped := c.transportToEdgeID[transportID]
    if mapped {
        if canonicalEdgeID != 0 && canonicalEdgeID != mappedEdgeID {
            return false  // canonical 不匹配，stale event
        }
        canonicalEdgeID = mappedEdgeID
    }
    if canonicalEdgeID == 0 {
        return false
    }
    activeTransportID, active := c.edgeIDToTransport[canonicalEdgeID]
    if active && activeTransportID != transportID {
        return false  // 已被新连接替换
    }
    if activeAddr := c.transportAddrs[transportID]; activeAddr != "" && addr != "" && activeAddr != addr {
        return false  // addr 不匹配，stale
    }
    delete(c.transportToEdgeID, transportID)
    delete(c.transportAddrs, transportID)
    if active {
        delete(c.edgeIDToTransport, canonicalEdgeID)
    }
    delete(c.k8sControllers, canonicalEdgeID)
    return true
}
```

**注释要点**：

> Frontier can deliver an old connection's offline event after a replacement connection is already online; addr prevents that stale event from deleting the new binding or marking the edge offline.

#### canonicalizeEdgeID —— 防止 ghost Prom series

```go
func (c *Client) canonicalizeEdgeID(edgeID uint64) uint64 {
    if edgeID == 0 {
        return 0
    }
    c.mu.RLock()
    defer c.mu.RUnlock()
    if canonical, ok := c.transportToEdgeID[edgeID]; ok {
        return canonical
    }
    // No transport binding established yet — return 0 so callers can
    // drop the request rather than write the raw geminio transport ID
    // (an opaque 64-bit number) into a Prom label. Letting it leak as
    // edge_id="7634732871700095575" creates ghost series that pollute
    // Grafana variable dropdowns until tsdb retention purges them
    // (the test env hit this; v0.7.39 fix).
    return 0
}
```

**这是 v0.7.39 的关键修复**：未建立 binding 时返回 0，让 caller drop 请求，而不是把 raw transport ID 泄露为 `edge_id` label 产生 ghost series。

#### resolveTransportID

```go
func (c *Client) resolveTransportID(edgeID uint64) uint64 {
    if edgeID == 0 {
        return 0
    }
    c.mu.RLock()
    defer c.mu.RUnlock()
    if transportID, ok := c.edgeIDToTransport[edgeID]; ok {
        return transportID
    }
    return edgeID  // 直连场景 transportID==edgeID
}
```

### 6.6 OpenStream —— manager → edge 双向字节流

```go
func (c *Client) OpenStream(ctx context.Context, edgeID uint64) (geminio.Stream, error) {
    if c.svc == nil {
        return nil, ErrDisabled
    }
    transportID := c.resolveTransportID(edgeID)
    s, err := c.svc.OpenStream(ctx, transportID)
    if err != nil {
        return nil, fmt.Errorf("frontierbound: open stream edge=%d transport=%d: %w", edgeID, transportID, err)
    }
    return s, nil
}
```

**注释要点**：

> The stream is opaque-typed wrt routing: ongrid sets the stream's Meta blob to a small JSON descriptor (e.g. `{"target":"127.0.0.1:22"}`) that the edge decodes before dialing the local socket. This keeps the tunnel layer generic — adding future stream-based protocols (port forwarding, file copy) only touches Meta.

### 6.7 Register —— 反向 RPC handler 适配

```go
func (c *Client) Register(ctx context.Context, method string, h Handler) error {
    if h == nil {
        return fmt.Errorf("frontierbound: nil handler for %q", method)
    }
    if c.svc == nil {
        return nil  // disabled client no-op
    }
    wrap := func(rpcCtx context.Context, req geminio.Request, rsp geminio.Response) {
        edgeID := req.ClientID()  // frontier 通过 custom-byte tail 注入
        out, err := h(rpcCtx, edgeID, req.Data())
        if err != nil {
            rsp.SetError(err)
            return
        }
        rsp.SetData(out)
    }
    return c.svc.Register(ctx, method, wrap)
}
```

**关键**：从 `req.ClientID()` 取 edgeID（frontier 通过 custom-byte tail 在 `fbsvc.serviceEnd.Register` 时注入），handler 不感知 geminio.Request。

### 6.8 生命周期回调注册

```go
func (c *Client) RegisterGetEdgeID(ctx context.Context, fn func(meta []byte) (uint64, error)) error
func (c *Client) RegisterEdgeOnline(ctx context.Context, fn func(edgeID uint64, meta []byte, addr net.Addr) error) error
func (c *Client) RegisterEdgeOffline(ctx context.Context, fn func(edgeID uint64, meta []byte, addr net.Addr) error) error
```

thin 透传到 svc；svc nil 时 no-op。具体语义见 §7。

---

## 7. 生命周期回调与身份解析

源文件：`internal/manager/service/frontierbound/handlers.go`

### 7.1 GetEdgeID —— 认证 + 身份解析

```go
resolveEdgeID := func(meta []byte) (uint64, error) {
    var m tunnel.Meta
    if err := json.Unmarshal(meta, &m); err != nil {
        return 0, fmt.Errorf("bad meta: %w", err)
    }
    edgeID, err := authenticateEdge(ctx, m.AccessKey, m.SecretKey)
    if err != nil {
        return 0, err
    }
    return edgeID, nil
}

c.RegisterGetEdgeID(ctx, resolveEdgeID)
```

**流程**：解析 Meta JSON → 调 `EdgeAuthn.Authenticate(accessKey, secretKey)` → 返回 `tunnel.Session.EdgeID`。任何失败返回 0 + error，frontier 拒绝 dial —— **manager 从不分配匿名 ID**。

**注释要点**：

> AccessKeyAuthenticator already collapses all failure paths to errs.ErrUnauthorized so we don't leak enumeration here.

### 7.2 EdgeOnline —— 绑定 transport ↔ edgeID

```go
c.RegisterEdgeOnline(ctx, func(edgeID uint64, meta []byte, addr net.Addr) error {
    canonicalEdgeID, err := resolveEdgeID(meta)
    if err == nil {
        c.bindEdgeTransportAt(edgeID, canonicalEdgeID, safeAddr(addr))
    }
    log.Info("frontierbound: edge online", ...)
    return nil
})
```

**关键**：用 Meta 重新认证拿到 canonicalEdgeID，建立 transport ID ↔ canonical edgeID 双向映射。这样即便 manager 重启丢失本地 map，frontier 仍持有已认证 TCP 连接，下次 RPC 时从 `req.ClientID()` 重建 binding。

### 7.3 EdgeOffline —— 三重校验防 stale

```go
c.RegisterEdgeOffline(ctx, func(edgeID uint64, _ []byte, addr net.Addr) error {
    canonicalEdgeID := c.canonicalizeEdgeID(edgeID)
    if !c.unbindEdgeTransport(edgeID, canonicalEdgeID, safeAddr(addr)) {
        log.Debug("frontierbound: stale or unknown edge offline ignored", ...)
        return nil
    }
    log.Info("frontierbound: edge offline", ...)
    if w.EdgeUC != nil && canonicalEdgeID != 0 {
        if err := w.EdgeUC.HandleOffline(ctx, canonicalEdgeID, time.Now().UTC()); err != nil {
            log.Warn("frontierbound: handle offline failed", ...)
        }
    }
    return nil
})
```

**三重校验**（见 §6.5）：canonicalEdgeID + active transport + addr。stale event 直接忽略，不误删新 binding。

### 7.4 register_edge —— 持久化 HostInfo + 翻转 status=online

```go
c.Register(ctx, tunnel.MethodRegisterEdge, func(rpcCtx, edgeID uint64, body []byte) ([]byte, error) {
    var in tunnel.RegisterEdgeRequest
    json.Unmarshal(body, &in)
    canonicalEdgeID := edgeID  // frontier 已认证注入
    if canonicalEdgeID == 0 {
        return nil, fmt.Errorf("register_edge: authenticated edge id is missing")
    }
    c.bindEdgeTransport(edgeID, canonicalEdgeID)
    // 分支：K8s controller vs host node
    if in.Kubernetes != nil && isKubernetesControllerRole(in.Kubernetes.Role) {
        w.EdgeUC.ClearHostDeviceLink(rpcCtx, canonicalEdgeID)
        w.K8sRegistry.HandleRegister(rpcCtx, canonicalEdgeID, nil, *in.Kubernetes)
        w.EdgeUC.HandleHeartbeat(rpcCtx, canonicalEdgeID, time.Now().UTC())
        c.setKubernetesController(canonicalEdgeID, true)
    } else {
        w.EdgeUC.HandleRegister(rpcCtx, canonicalEdgeID, in.HostInfo, in.AgentVersion)
        // K8s node 元数据（可选）
        c.setKubernetesController(canonicalEdgeID, false)
    }
    out := tunnel.RegisterEdgeResponse{
        EdgeID:     canonicalEdgeID,
        ServerTime: time.Now().UTC().Unix(),
    }
    return json.Marshal(out)
})
```

**注释要点**：

> Do not trust the legacy credentials in this request body: current edges intentionally leave them empty and carry credentials only in Meta.

### 7.5 heartbeat —— bump last_seen_at + piggyback 插件健康

```go
c.Register(ctx, tunnel.MethodHeartbeat, func(rpcCtx, edgeID uint64, body []byte) ([]byte, error) {
    var in tunnel.HeartbeatRequest
    json.Unmarshal(body, &in)
    canonicalEdgeID := c.canonicalizeEdgeID(edgeID)
    if canonicalEdgeID == 0 {
        return nil, fmt.Errorf("heartbeat: edge binding not ready; re-register required")
    }
    if in.EdgeID != 0 && in.EdgeID != canonicalEdgeID {
        return nil, fmt.Errorf("heartbeat: edge id mismatch")
    }
    w.EdgeUC.HandleHeartbeat(rpcCtx, canonicalEdgeID, ts)
    refreshKubernetesControllerHeartbeat(rpcCtx, c, w.K8sRegistry, canonicalEdgeID)
    // piggyback 插件健康（best-effort，不 fail heartbeat）
    if len(in.Plugins) > 0 {
        w.EdgeUC.RecordPluginHealth(canonicalEdgeID, items)
    }
    return json.Marshal(tunnel.HeartbeatResponse{})
})
```

---

## 8. 反向 RPC Handler（manager 侧 Install）

源文件：`internal/manager/service/frontierbound/handlers.go`

### 8.1 Wiring 依赖注入

```go
type Wiring struct {
    EdgeAuthn      EdgeAuthenticator
    EdgeUC         *edgebiz.Usecase
    MetricIngester metricbiz.IngestService
    PromIngester   PromwriteIngester       // optional — nil means Prom disabled
    PluginConfigUC PluginConfigFetcher     // optional
    WebshellRouter WebshellRouter          // optional
    DeviceResolver DeviceResolver          // optional — edge_id → device_id
    K8sRegistry    KubernetesRegistry
    K8sInventory   KubernetesInventoryIngester
    Log            *slog.Logger
}
```

**设计**：所有依赖以接口形式声明在消费方（frontierbound），符合"接口在消费方定义"的架构红线。

### 8.2 Install 注册的 handler

| Method | 方向 | 职责 |
|--------|------|------|
| `register_edge` | edge→cloud | 持久化 HostInfo + 翻转 status=online + K8s controller 分支 |
| `heartbeat` | edge→cloud | bump last_seen_at + piggyback 插件健康 + K8s controller heartbeat |
| `push_host_metrics` | edge→cloud | 转发批次到 MetricIngester（resolveDeviceID 后） |
| `push_prom_samples` | edge→cloud | 转发开放集样本到 PromIngester（host / k8s 分流） |
| `push_k8s_inventory` | edge→cloud | 持久化 controller 推送的 K8s 快照 |
| `get_plugin_configs` | edge→cloud | 返回 edge 的插件配置快照 |
| `shell_output` | edge→cloud | WebSSH stdout chunk 路由到 WebSocket bridge |
| `shell_exit` | edge→cloud | WebSSH 终止帧路由 |

### 8.3 push_host_metrics 的 device_id 解析

```go
canonicalEdgeID := c.canonicalizeEdgeID(edgeID)
if canonicalEdgeID == 0 {
    // Silent drop — edge will retry once the binding is set up.
    // Letting transport ID through would create ghost edge_id labels (v0.7.39 fix).
    return json.Marshal(tunnel.PushHostMetricsResponse{Accepted: 0})
}
deviceID := resolveDeviceID(rpcCtx, w.DeviceResolver, canonicalEdgeID)
if deviceID == 0 {
    // Host junction missing — drop rather than write edge_id as bogus device_id label (issue #96).
    log.Warn("frontierbound: push_host_metrics dropped — device_id unresolved", ...)
    return json.Marshal(tunnel.PushHostMetricsResponse{Accepted: 0})
}
w.MetricIngester.Push(rpcCtx, deviceID, in.Points)
```

**注释要点**：

> It MUST NOT fall back to edge_id. After the edge/device entity split (May 2026) edge_id and device.ID are independent auto-increment sequences, so a fallback writes a WRONG device_id label into the immutable Prometheus TSDB.

### 8.4 push_prom_samples 的 host / k8s 分流

```go
if isKubernetesPromSource(in.Source) {  // source 前缀 "k8s:"
    clusterID := lookupK8sControllerCluster(rpcCtx, w.K8sRegistry, canonicalEdgeID, log)
    w.PromIngester.PushKubernetes(rpcCtx, clusterID, in.Source, in.Samples)
} else {
    deviceID := resolveDeviceID(rpcCtx, w.DeviceResolver, canonicalEdgeID)
    if deviceID == 0 {
        // 再尝试 k8s controller cluster
        clusterID := lookupK8sControllerCluster(...)
        if clusterID != 0 {
            w.PromIngester.PushKubernetes(rpcCtx, clusterID, in.Source, in.Samples)
        } else {
            // Host junction missing — drop (issue #96)
        }
    } else {
        w.PromIngester.Push(rpcCtx, deviceID, in.Source, in.Samples)
    }
}
```

### 8.5 PromIngester nil 的优雅降级

```go
if w.PromIngester == nil {
    // Prom disabled / not wired. Quiet drop, return Accepted=n so the
    // edge does not retry. We still log at DEBUG for diagnosis.
    log.Debug("frontierbound: push_prom_samples dropped (prom disabled)", ...)
    return json.Marshal(tunnel.PushPromSamplesResponse{Accepted: n})
}
```

**设计**：edge 不知道 cloud 的 Prom 状态，quiet drop + Accepted=n 让 edge 不重试。

### 8.6 NotifyPluginConfigsChanged —— fire-and-forget 推送

```go
func (c *Client) NotifyPluginConfigsChanged(ctx context.Context, edgeID uint64) error {
    _, err := c.Call(ctx, edgeID, tunnel.MethodPluginConfigsChanged, []byte("{}"))
    return err
}
```

**注释要点**：

> Failure modes are caller's responsibility to log; in particular this is fire-and-forget for the biz layer because the edge's 60s safety-net poll catches missed pushes anyway.

---

## 9. 双向 Stream 与 WebSSH

### 9.1 WebSSH 架构

```
浏览器 ──WebSocket── manager ──geminio.Stream── edge ──本地 socket── sshd
                              (OpenStream)            (AcceptStream + io.Copy)
```

**关键**：SSH client 运行在 manager 侧，edge 是"哑字节转发器"。

### 9.2 manager 侧 webshell handler

源文件：`internal/manager/server/webshell/http.go`

```go
// Open frontier stream to the edge with target meta.
streamCtx, cancelStreamOpen := context.WithTimeout(r.Context(), 10*time.Second)
stream, err := h.streamer.OpenStream(streamCtx, edge.ID)
cancelStreamOpen()

// Wrap stream with SSH client conn.
sshCfg := &ssh.ClientConfig{
    User:            openFrame.SSHUser,
    Auth:            []ssh.AuthMethod{ssh.Password(openFrame.SSHPass)},
    HostKeyCallback: ssh.InsecureIgnoreHostKey(), // localhost-only path
    Timeout:         10 * time.Second,
}
openFrame.SSHPass = "" // wipe asap

sshConn, sshChans, sshReqs, sshErr := ssh.NewClientConn(rwcAdapter{rwc: stream}, "127.0.0.1:22", sshCfg)
sshClient := ssh.NewClient(sshConn, sshChans, sshReqs)
sess, err := sshClient.NewSession()
```

### 9.3 webshellStreamerAdapter —— geminio.Stream 适配

源文件：`cmd/ongrid/main.go`

```go
type webshellStreamerAdapter struct {
    c *managersvcfb.Client
}

func (a webshellStreamerAdapter) OpenStream(ctx context.Context, edgeID uint64) (io.ReadWriteCloser, error) {
    return a.c.OpenStream(ctx, edgeID)
}
```

**注释要点**：

> The client returns a geminio.Stream which embeds Raw = net.Conn; the adapter widens it to io.ReadWriteCloser so server/webshell stays free of geminio.

### 9.4 stream Meta 的未来扩展

```go
// We can't set Meta on the manager-opened stream via the current
// frontierbound surface (geminio.OpenStreamOptions exists but we
// don't expose it yet). For now the edge always defaults to
// 127.0.0.1:22 when Meta is empty — OK for Phase 1. Phase 2
// thread Meta through when we add jumpbox support.
_ = streamMeta{Target: "127.0.0.1:22"} // documentation only
```

Phase 2 将通过 Meta 支持 jumpbox / port forwarding / file copy。

---

## 10. 重连机制与代际连接管理

### 10.1 双层重连

| 层 | 职责 | 实现 |
|----|------|------|
| geminio RetryEnd | 透明重连（首次 Dial 成功后断线自动重连） | geminio 内部 |
| OnReconnect 回调 | 业务层重连后动作（如 re-register_edge） | `tunnel.Client.OnReconnect` |

### 10.2 代际连接管理（edge 侧）

源文件：`internal/pkg/tunnel/client.go`

```go
connMu            sync.Mutex
activeConn        net.Conn
activeGeneration  uint64
pendingConn       net.Conn
pendingGeneration uint64
nextGeneration    uint64
```

**三态**：

- **pending**：新连接已建立但未升级为 active（`trackConnection` 时标记）
- **active**：当前正在用的连接（`promotePendingConnection` 时升级）
- **generation**：单调递增的代次号

**流程**：

1. `buildDialer` 返回的 dialer 每次拨号成功调 `trackConnection(conn)` → 标 pending
2. geminio RetryEnd 内部完成 End 切换后调 `retryDelegate.EndReOnline` → `promotePendingConnection()` 把 pending 升级为 active
3. `Call` 记录当前 `connectionGeneration()`；若 RPC 失败且是 frontier routing 错误，调 `recycleBrokenRoute` 关闭 active 连接让 RetryEnd 重连

### 10.3 recycleBrokenRoute —— 精确判定

```go
func (c *geminioClient) recycleBrokenRoute(method string, err error, generation uint64) {
    if !shouldRecycleBrokenRoute(method, err) || c.closed.Load() {
        return
    }
    c.connMu.Lock()
    if generation == 0 || generation != c.activeGeneration || c.activeConn == nil {
        c.connMu.Unlock()
        return
    }
    conn := c.activeConn
    c.activeConn = nil
    c.connMu.Unlock()

    c.log.Warn("tunnel: frontier route is stale; recycling transport", ...)
    conn.Close()
}

func shouldRecycleBrokenRoute(method string, err error) bool {
    if err == nil || (method != MethodRegisterEdge && method != MethodHeartbeat) {
        return false
    }
    msg := err.Error()
    if strings.Contains(msg, "no such rpc: "+method) || strings.Contains(msg, "mismatch clientID") {
        return true
    }
    if method == MethodRegisterEdge {
        return strings.Contains(msg, "register_edge: get edge: not found")
    }
    return strings.Contains(msg, "heartbeat: edge binding not ready; re-register required") ||
        strings.Contains(msg, "heartbeat: edge id mismatch")
}
```

**注释要点**：

> RetryEnd observes the close and performs the single serialized reconnect; this avoids the parallel-End race caused by manually constructing a second RetryEnd while the first transport was still alive.

即：只关闭连接让 RetryEnd 单线程重连，避免手动构造第二个 RetryEnd 与仍活的第一 transport 竞争。

**仅对 `register_edge` / `heartbeat` 的特定错误回收**：

- `"no such rpc: <method>"`
- `"mismatch clientID"`
- `register_edge`：`"register_edge: get edge: not found"`
- `heartbeat`：`"heartbeat: edge binding not ready; re-register required"` / `"heartbeat: edge id mismatch"`

### 10.4 manager 侧 stale offline 防御

见 §6.5 `unbindEdgeTransport` 三重校验：canonicalEdgeID + active transport + addr。

---

## 11. Edge Agent 使用模式

源文件：`internal/edgeagent/biz/agent.go`

### 11.1 Agent 结构

```go
type Agent struct {
    client    tunnel.Client
    collector Collector
    cfg       Config
    log       *slog.Logger
    pluginHealthFn func() []tunnel.PluginHealthWire
}
```

### 11.2 注册 cloud → edge handler

```go
a.client.RegisterHandler(tunnel.MethodGetHostLoad,
    func(ctx context.Context, _ tunnel.Session, _ string, _ []byte) ([]byte, error) {
        // 返回实时负载
    })

a.client.RegisterHandler(tunnel.MethodGetProcessList,
    func(ctx context.Context, _ tunnel.Session, _ string, body []byte) ([]byte, error) {
        var req tunnel.GetProcessListRequest
        // 返回 top-N 进程
    })

a.client.RegisterHandler(tunnel.MethodExecuteSkill,
    func(ctx context.Context, _ tunnel.Session, _ string, body []byte) ([]byte, error) {
        // skill dispatcher
    })

a.client.RegisterHandler(tunnel.MethodAgentUpgrade, ...)
a.client.RegisterHandler(tunnel.MethodFetchPackage, ...)
a.client.RegisterHandler(tunnel.MethodApplyPackage, ...)
```

### 11.3 OnReconnect —— re-register_edge

```go
a.client.OnReconnect(func() {
    // 重连后重新注册 edge，让 cloud 重新绑定 canonical edge_id
    a.registerEdge(ctx)
})
```

### 11.4 edge → cloud RPC 调用

```go
// 注册
req := tunnel.RegisterEdgeRequest{...}
var resp tunnel.RegisterEdgeResponse
a.client.Call(ctx, tunnel.MethodRegisterEdge, req, &resp)

// 心跳
a.client.Call(rctx, tunnel.MethodHeartbeat, tunnel.HeartbeatRequest{...}, &resp)

// 推主机指标
a.client.Call(rctx1, tunnel.MethodPushHostMetrics, tunnel.PushHostMetricsRequest{...}, &resp1)

// 推 Prom 样本
a.client.Call(rctx2, tunnel.MethodPushPromSamples, tunnel.PushPromSamplesRequest{...}, &resp2)
```

---

## 12. cmd/ongrid/main.go 装配流程

源文件：`cmd/ongrid/main.go`

### 12.1 装配步骤

```go
// 1. 构造 frontierbound.Client（或 NewDisabled）
var fbClient *managersvcfb.Client
if cfg.FrontierClient.Disabled {
    fbClient = managersvcfb.NewDisabled(log.With(...))
} else {
    c, err := managersvcfb.New(managersvcfb.Config{
        Addr:        cfg.FrontierClient.Addr,
        ServiceName: cfg.FrontierClient.ServiceName,
    }, log.With(...))
    if err != nil {
        os.Exit(1)
    }
    fbClient = c
}
defer fbClient.Close()

// 2. Back-fill edge service 的 EdgeCaller
edgeSvc.SetEdgeCaller(fbClient)

// 3. 构造 WebSSH router
webshellRouter := managerwebshellbiz.NewRouter()

// 4. Install 反向 RPC handler + 生命周期回调
managersvcfb.Install(rootCtx, fbClient, managersvcfb.Wiring{
    EdgeAuthn:      edgeAuthn,
    EdgeUC:         edgeUC,
    MetricIngester: metricIngestSvc,
    PromIngester:   promWiring,
    PluginConfigUC: pluginConfigUC,
    WebshellRouter: webshellRouter,
    DeviceResolver: edgeDeviceRepo,
    K8sRegistry:    k8sSvc,
    K8sInventory:   k8sSvc,
    Log:            log.With(slog.String("comp", "frontierbound")),
})

// 5. Back-fill plugin config notifier
pluginConfigUC.SetNotifier(fbClient)
pluginConfigUC.SetDatabaseMetricsSecretWriter(fbClient)

// 6. 构造 WebSSH HTTP handler
webshellHandler := managerwebshellserver.NewHandler(
    webshellStreamerAdapter{c: fbClient},
    webshellRouter,
    webshellAuditAdapter{repo: webshellAuditRepo},
    deviceRepo,
    edgeRepo,
    log.With(slog.String("comp", "webshell")),
)
```

### 12.2 ONGRID_FRONTIER_DISABLED=true 路径

```go
// ONGRID_FRONTIER_DISABLED=true bypasses the dial entirely — the
// resulting Client errors all Call/OpenStream/NotifyX with
// frontierbound.ErrDisabled and is a no-op for Register. Used by the
// e2e harness so manager can come up without a real broker. The HTTP
// surface and DB stack are unaffected; edge-tunnel-only features
// (webssh, edge reverse calls) surface ErrDisabled at the call site.
```

用于 e2e 测试 / degraded-broker 恢复测试：manager 不依赖真 frontier 即可启动；edge-tunnel 相关功能在调用点返回 `ErrDisabled`。

---

## 13. 并发与资源管理

### 13.1 锁分工

| 锁 | 文件 | 保护对象 | 类型 |
|----|------|----------|------|
| `handlersMu` | `tunnel/client.go` | handlers map | RWMutex |
| `reconnectMu` | `tunnel/client.go` | reconnectCallbacks 切片 | Mutex |
| `reconnectRunMu` | `tunnel/client.go` | 串行化回调执行 | Mutex |
| `connMu` | `tunnel/client.go` | active/pending conn 代际状态 | Mutex |
| `closeOnce` | `tunnel/client.go` | Close 幂等 | Once |
| `mu` | `frontierbound/client.go` | 四个 binding map | RWMutex |

### 13.2 atomic

| 字段 | 文件 | 用途 |
|------|------|------|
| `endPtr atomic.Pointer[geminio.End]` | `tunnel/client.go` | 无锁读取当前 end |
| `closed atomic.Bool` | `tunnel/client.go` | 无锁检查关闭状态 |

### 13.3 回调 panic 隔离

```go
func() {
    defer func() {
        if r := recover(); r != nil {
            c.log.Warn("tunnel: OnReconnect callback panicked", slog.Any("recover", r))
        }
    }()
    fn()
}()
```

每个 OnReconnect 回调 defer/recover，防 panic 杀死重连 goroutine 导致下次重连死锁。

### 13.4 geminio.Stream 生命周期

OpenStream 返回的 stream 由 caller 管理：

- WebSSH：`io.Copy` + `Close`
- manager 侧 `defer sshClient.Close()` 链式关闭

### 13.5 map 永不 shrink

`frontierbound.Client` 的四个 map delete 后容量不变；binding 数量受 edge 总数限制，可控。

### 13.6 disabled client 安全

`svc nil` 时所有调用短路，无锁竞争。

---

## 14. 设计模式与架构红线

### 14.1 设计模式

| 模式 | 应用 |
|------|------|
| Delegate | `retryDelegate` 嵌入 `UnimplementedDelegate` 覆盖 `EndReOnline` |
| Adapter | `geminioStreamWrap`（geminio.Stream → StreamConn）、`webshellStreamerAdapter`（geminio.Stream → io.ReadWriteCloser）、`Handler` wrapper（geminio 签名 → OnGrid 签名） |
| Null Object | `NewDisabled` 返回 svc=nil 的 Client，调用返回 `ErrDisabled` |
| Narrow Interface | `service` interface 仅暴露用到的方法子集；`StreamConn` 窄于 net.Conn |
| Dependency Injection | `Wiring` 结构注入所有 biz 依赖；`AuthFunc` / `DeviceResolver` 等接口在消费方定义 |
| Generation-based concurrency control | pending → active 升级 + generation 校验 |
| Three-way validation | `unbindEdgeTransport` 三重校验防 stale offline |

### 14.2 架构红线

1. **wire 协议单一来源** —— message names + JSON shapes 集中在 `internal/pkg/tunnel/messages.go`，manager 侧 frontierbound 不重新声明
2. **手镜像而非代码生成** —— `messages.go` 手镜像 `tunnel.proto`，保持 `internal/pkg/tunnel` 零外部依赖（无 protobuf import）
3. **canonicalizeEdgeID 返回 0 而非 raw ID** —— 防止 raw transport ID 泄露为 Prom `edge_id` label 产生 ghost series（v0.7.39 fix）
4. **resolveDeviceID 不回退 edge_id** —— edge/device 实体分离后 edge_id 与 device.ID 是独立自增序列，回退会写错 device_id label（issue #96）
5. **unbindEdgeTransport 三重校验** —— canonicalEdgeID + active transport + addr，防 frontier 投递 stale offline 事件误删新 binding
6. **应用错误不强制二次连接** —— 只有 frontier routing 错误才 recycleBrokenRoute，RetryEnd 拥有 transport recovery 所有权
7. **disabled client 优雅降级** —— `ONGRID_FRONTIER_DISABLED=true` 让 manager 无 frontier 可启动；Register* no-op；Call 返回 ErrDisabled
8. **接口在消费方定义** —— `service` / `EdgeAuthenticator` / `PromwriteIngester` / `DeviceResolver` / `KubernetesRegistry` 等接口在 frontierbound 包定义
9. **Meta 与 RPC body 分离** —— Meta 是握手 blob，非 RPC body；server 在 AuthFunc 前解码
10. **永不记录敏感字段** —— `SSHPass` one-shot wiped after Dial；`WriteDatabaseMetricsSecretRequest.Content` 不持久化
11. **回调 panic 隔离** —— OnReconnect 回调 defer/recover，防 panic 死锁重连 goroutine
12. **closeOnce 幂等** —— Close 多次安全
13. **生产必须配 TLS** —— Meta 含 SecretKey 明文 JSON，无 TLS 有泄漏风险

### 14.3 关键决策

| 决策 | 理由 |
|------|------|
| 经由 frontier broker 而非 edge 直连 | edge 在用户机房 NAT 后，无法被 manager 直连；frontier 作为公网 broker 中转 |
| geminio 而非 gRPC | geminio 支持双向 RPC（cloud→edge 反向调用）+ stream，gRPC 主要面向 server→client |
| JSON 而非 protobuf binary（MVP） | 保持 `internal/pkg/tunnel` 零依赖；Phase 2 切 protobuf 时 `messages.go` 是唯一 seam |
| 手镜像 proto 而非生成 | 避免 protobuf import + generated-code 目录；proto 仅作规范文档 |
| 代际连接管理 | 解决"RetryEnd 切换底层 End 时如何安全回收旧 broken route"竞态 |
| 仅 register_edge/heartbeat 触发 recycle | 这两个是 edge 启动/保活的关键 RPC，stale route 必须立即回收；其他 method 的 routing 错误 caller 自行重试 |
| SSH client 在 manager 侧 | edge 是哑字节转发器，SSH 认证/会话管理集中在 manager 便于审计 |
| OpenStream Meta 通用化 | 添加未来 stream-based 协议（jumpbox / port forwarding / file copy）只动 Meta |

---

## 15. 关键文件索引

### 15.1 edge 侧 tunnel

| 文件 | 职责 |
|------|------|
| `internal/pkg/tunnel/doc.go` | 包级文档，声明架构边界 |
| `internal/pkg/tunnel/types.go` | Client 接口、Handler/AuthFunc/StreamConn/Session/ClientConfig |
| `internal/pkg/tunnel/client.go` | geminioClient 实现（Dial/Call/RegisterHandler/AcceptStream/OnReconnect/Close + 代际管理） |
| `internal/pkg/tunnel/messages.go` | wire 协议常量与结构（手镜像 proto） |
| `internal/pkg/tunnel/client_test.go` | 测试 |

### 15.2 manager 侧 frontierbound

| 文件 | 职责 |
|------|------|
| `internal/manager/service/frontierbound/doc.go` | 包级文档 |
| `internal/manager/service/frontierbound/client.go` | Client 实现（Call/Register/OpenStream/binding 管理） |
| `internal/manager/service/frontierbound/handlers.go` | Install 注册所有反向 RPC handler + 生命周期回调 |
| `internal/manager/service/frontierbound/client_test.go` | 测试 |
| `internal/manager/service/frontierbound/handlers_test.go` | 测试 |

### 15.3 协议定义

| 文件 | 职责 |
|------|------|
| `api/tunnel/v1/tunnel.proto` | wire 协议规范文档（不生成 Go 代码） |

### 15.4 装配与使用

| 文件 | 职责 |
|------|------|
| `cmd/ongrid/main.go` | frontierbound.Client 构造 + Install + WebSSH adapter 装配 |
| `internal/edgeagent/biz/agent.go` | edge agent 使用 tunnel.Client 注册 handler + Call |
| `internal/manager/server/webshell/http.go` | WebSSH 用 OpenStream + ssh.NewClientConn |
| `internal/manager/biz/edge/usecase.go` | HandleRegister / HandleHeartbeat / HandleOffline |
| `internal/manager/biz/aiops/tools/registry.go` | aiops 工具通过 Caller 接口调用 edge |
| `internal/pkg/config/config.go` | FrontierClient 配置（Addr/ServiceName/Disabled） |
| `tests/e2e/testenv/env.go` | e2e 测试用 ONGRID_FRONTIER_DISABLED=true |

### 15.5 部署

| 文件 | 职责 |
|------|------|
| `deploy/install/frontier.yaml` | frontier broker 容器部署配置 |
| `deploy/Dockerfile.frontier` | frontier 镜像构建 |
| `deploy/install/edge/install.sh` | edge 安装脚本（含 tunnel 连接配置） |

---

> 本文档基于 OnGrid 源码中与 `singchia/geminio` 相关的全部代码编写，覆盖 tunnel client（edge 侧）、frontierbound client（manager 侧）、wire 协议、生命周期回调、反向 RPC handler、双向 stream、重连与代际管理、装配流程、并发模型与架构红线。如需深入某个模块，参考 §15 文件索引定位源文件。
