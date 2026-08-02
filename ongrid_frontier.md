# OnGrid × Frontier 集成技术实现说明文档

> 本文档深入分析 OnGrid 系统与 upstream `github.com/singchia/frontier` broker 集成的全部代码路径：edge 侧 tunnel.Client、manager 侧 frontierbound.Client、wire 协议（messages.go）、生命周期回调、反向 RPC handler、WebSSH Stream、配置装配、systemhealth 探测、docker-compose 部署面。
>
> 全部行号引用基于撰写时仓库快照，可能随后续提交漂移；尽量给出文件路径锚点便于跳转。

---

## 目录

1. [架构总览](#1-架构总览)
2. [分层与文件索引](#2-分层与文件索引)
3. [Wire 协议：tunnel/messages.go](#3-wire-协议tunnelmessagesgo)
4. [Edge 侧：tunnel.Client（geminio SDK 封装）](#4-edge-侧tunnelclientgeminio-sdk-封装)
5. [Edge 侧：edgeagent/biz/Agent 运行时](#5-edge-侧edgeagentbizagent-运行时)
6. [Manager 侧：frontierbound.Client（service-end SDK 封装）](#6-manager-侧frontierboundclientservice-end-sdk-封装)
7. [Manager 侧：frontierbound.Install 反向 RPC 与生命周期回调](#7-manager-侧frontierboundinstall-反向-rpc-与生命周期回调)
8. [Transport ↔ EdgeID 双向映射](#8-transport--edgeid-双向映射)
9. [WebSSH：OpenStream + 双向 push](#9-websshopenstream--双向-push)
10. [配置与启动装配](#10-配置与启动装配)
11. [systemhealth：Frontier 探测](#11-systemhealthfrontier-探测)
12. [aiops tools：Caller 接口](#12-aiops-toolscaller-接口)
13. [部署面：docker-compose + frontier.yaml + Dockerfile](#13-部署面docker-compose--frontieryaml--dockerfile)
14. [并发、错误与可观测性](#14-并发错误与可观测性)
15. [架构红线与设计要点](#15-架构红线与设计要点)
16. [附录：测试覆盖](#16-附录测试覆盖)

---

## 1. 架构总览

OnGrid 的 edge ↔ cloud 隧道基于 upstream `github.com/singchia/frontier` broker——一个独立的 geminio 多路复用代理。三方拓扑：

```
edge agent ──(geminio + Meta JSON)──▶ frontier:40012 (edgebound)
                                       │
                                       │ (frontier 内部路由 + service-end 转发)
                                       ▼
manager ──(fbsvc.Service SDK)──▶ frontier:40011 (servicebound)
```

- **edge 侧**：`internal/pkg/tunnel.Client` 是 geminio `RetryEnd` 的薄封装，长连到 frontier edgebound，发 `register_edge` / `heartbeat` / `push_*`，注册反向 RPC handler（`get_host_load` / `execute_skill` / `shell_*` 等）。
- **manager 侧**：`internal/manager/service/frontierbound.Client` 是 `fbsvc.Service` 的薄封装，长连到 frontier servicebound，注册三个生命周期回调（`GetEdgeID` / `EdgeOnline` / `EdgeOffline`）+ 多个反向 RPC handler（`register_edge` / `heartbeat` / `push_host_metrics` / `push_prom_samples` / `shell_output` / `shell_exit` 等），并通过 `Call` / `OpenStream` 向 edge 发起 RPC 和双向流。
- **broker**：frontier 是无业务逻辑的路由器——只做 edge ID 分配（通过 manager 的 `GetEdgeID` 回调）、连接代际管理、service-end 转发。它不解析 RPC body，body 是 JSON over geminio wire。

这种三方解耦的关键好处：**manager 重启不切断 edge 连接**。frontier 保持 edge TCP/geminio 会话，manager 重连后通过 `register_edge` / `GetEdgeID` 重建 in-memory binding（`transportToEdgeID` map）。

---

## 2. 分层与文件索引

| 层 | 文件 | 职责 |
|----|------|------|
| Wire 协议 | [internal/pkg/tunnel/messages.go](file:///d:/claude/ongrid/internal/pkg/tunnel/messages.go) | 方法名常量 + JSON 请求/响应结构体（手镜像 tunnel.proto） |
| Wire 类型 | [internal/pkg/tunnel/types.go](file:///d:/claude/ongrid/internal/pkg/tunnel/types.go) | `Session` / `Handler` / `AuthFunc` / `ClientConfig` / `Client` 接口 / `StreamConn` |
| Wire 包文档 | [internal/pkg/tunnel/doc.go](file:///d:/claude/ongrid/internal/pkg/tunnel/doc.go) | 包注释：edge 侧 cloud channel 抽象 |
| Edge 客户端 | [internal/pkg/tunnel/client.go](file:///d:/claude/ongrid/internal/pkg/tunnel/client.go) | `geminioClient` 实现 `Client` 接口；Dial / RegisterHandler / Call / AcceptStream / OnReconnect / Close |
| Edge 客户端测试 | [internal/pkg/tunnel/client_test.go](file:///d:/claude/ongrid/internal/pkg/tunnel/client_test.go) | `retryDelegate.EndReOnline` 回调顺序测试 |
| Edge 运行时 | [internal/edgeagent/biz/agent.go](file:///d:/claude/ongrid/internal/edgeagent/biz/agent.go) | `Agent.Run`：registerHandlers → Dial → registerEdge → heartbeatLoop + metricsLoop |
| Manager 客户端 | [internal/manager/service/frontierbound/client.go](file:///d:/claude/ongrid/internal/manager/service/frontierbound/client.go) | `Client` 包 `fbsvc.Service`：`New` / `NewDisabled` / `Call` / `Register` / `OpenStream` / 三大生命周期注册 / transport↔edgeID 映射 |
| Manager 客户端测试 | [internal/manager/service/frontierbound/client_test.go](file:///d:/claude/ongrid/internal/manager/service/frontierbound/client_test.go) | mapping / unbind / canonicalize / resolveTransportID 单测 |
| Manager 反向 RPC | [internal/manager/service/frontierbound/handlers.go](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go) | `Install(ctx, c, Wiring)`：注册所有反向 RPC + 生命周期回调 |
| Manager 包文档 | [internal/manager/service/frontierbound/doc.go](file:///d:/claude/ongrid/internal/manager/service/frontierbound/doc.go) | 包注释：service-end SDK 封装 |
| Manager 测试 | [internal/manager/service/frontierbound/handlers_test.go](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers_test.go) | handlers 行为测试 |
| WebSSH 路由 | [internal/manager/biz/webshell/router.go](file:///d:/claude/ongrid/internal/manager/biz/webshell/router.go) | `Router` Register/Unregister/DispatchOutput/DispatchExit |
| WebSSH HTTP | [internal/manager/server/webshell/http.go](file:///d:/claude/ongrid/internal/manager/server/webshell/http.go) | `Streamer` 接口 + `OpenStream` + SSH over tunnel |
| Prom 写入 | [internal/manager/biz/promwrite/ingester.go](file:///d:/claude/ongrid/internal/manager/biz/promwrite/ingester.go) | `Push` / `PushKubernetes` 实现 |
| AIOps 工具 | [internal/manager/biz/aiops/tools/registry.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/registry.go) | `Caller` 接口（被 `frontierbound.Client` 满足） |
| Edge biz | [internal/manager/biz/edge/usecase.go](file:///d:/claude/ongrid/internal/manager/biz/edge/usecase.go) | `HandleRegister` / `HandleHeartbeat` / `HandleOffline` |
| 配置 | [internal/pkg/config/config.go](file:///d:/claude/ongrid/internal/pkg/config/config.go#L246-L262) | `FrontierClientConfig`（Addr / ServiceName / Disabled） |
| 启动装配 | [cmd/ongrid/main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1036-L1108) | `NewDisabled` 或 `New` → `Install(Wiring)` → `SetEdgeCaller` → `SetNotifier` |
| Edge 启动装配 | [cmd/ongrid-edge/main.go](file:///d:/claude/ongrid/cmd/ongrid-edge/main.go#L130-L135) | `tunnel.NewClient(ClientConfig{CloudAddr, AccessKey, SecretKey})` |
| 系统健康 | [internal/manager/service/systemhealth/service.go](file:///d:/claude/ongrid/internal/manager/service/systemhealth/service.go#L265-L273) | `checkFrontier` 探测 |
| 部署 | [deploy/docker-compose.yml:210-222](file:///d:/claude/ongrid/deploy/docker-compose.yml#L210-L222) | frontier 服务定义 |
| 部署 | [deploy/install/frontier.yaml](file:///d:/claude/ongrid/deploy/install/frontier.yaml) | frontier broker 配置（edgebound / servicebound / `edgeid_alloc_when_no_idservice_on: false`） |
| 部署 | [deploy/Dockerfile.frontier](file:///d:/claude/ongrid/deploy/Dockerfile.frontier) | 镜像构建（非 root 65532） |

---

## 3. Wire 协议：tunnel/messages.go

[internal/pkg/tunnel/messages.go](file:///d:/claude/ongrid/internal/pkg/tunnel/messages.go) 是 wire 协议的单一真源——**手镜像** `api/tunnel/v1/tunnel.proto` 的 message 为 Go struct + JSON tag，**不生成** protobuf 代码。

### 3.1 设计原则

[messages.go:5-13](file:///d:/claude/ongrid/internal/pkg/tunnel/messages.go#L5-L13) 注释明确：

```go
// This file hand-mirrors api/tunnel/v1/tunnel.proto message shapes as Go
// structs with JSON tags. the tunnel body wire format is JSON
// in MVP; we deliberately avoid generating protobuf Go types for these
// payloads so internal/pkg/tunnel/ stays dependency-free (no protobuf
// import, no generated-code directory). When (if) we switch to protobuf
// binary in Phase 2, this file is the seam: swap types here, keep
// callers unchanged.
```

关键：`internal/pkg/tunnel` 零依赖——edge 和 manager 都能直接 import，无 protobuf runtime 拖累。Phase 2 切 protobuf 时只改这一个文件。

### 3.2 方法名常量

[messages.go:17-84](file:///d:/claude/ongrid/internal/pkg/tunnel/messages.go#L17-L84) 定义全部 wire 方法名：

| 方法 | 方向 | 用途 |
|------|------|------|
| `register_edge` | edge → cloud | 注册 + 拿 canonical edge_id |
| `heartbeat` | edge → cloud | 心跳 + piggyback plugin health |
| `push_host_metrics` | edge → cloud | 8 字段快速路径 |
| `push_prom_samples` | edge → cloud | open-set Prometheus 样本 |
| `push_k8s_inventory` | edge → cloud | K8s 资源快照 |
| `describe_k8s_resource` / `query_k8s_logs` / `execute_k8s_action` | cloud → edge | K8s 探查 |
| `get_host_load` / `get_process_list` / `get_netstat` | cloud → edge | 主机探查 |
| `execute_skill` | cloud → edge | skill 框架统一分发器 |
| `get_plugin_configs` | edge → cloud | 拉 plugin 配置快照 |
| `plugin_configs_changed` | cloud → edge | 推通知（edge 再去拉） |
| `write_database_metrics_secret` | cloud → edge | 写数据库监控凭据文件 |
| `shell_open` / `shell_input` / `shell_resize` / `shell_close` | cloud → edge | WebSSH 控制 |
| `shell_output` / `shell_exit` | edge → cloud | WebSSH 输出/退出 |
| `agent_upgrade` | cloud → edge | 单 binary 替换 |
| `fetch_package` / `apply_package` | cloud → edge | 完整 bundle 暂存 + 应用 |

### 3.3 关键 wire 结构体

[messages.go:225-252](file:///d:/claude/ongrid/internal/pkg/tunnel/messages.go#L225-L252) `HostInfo` 是 register_edge 的静态描述：

```go
type HostInfo struct {
    Hostname      string `json:"hostname"`
    OS            string `json:"os"`
    Arch          string `json:"arch"`
    KernelVersion string `json:"kernel_version"`
    CPUCount      int    `json:"cpu_count"`
    MemTotalBytes uint64 `json:"mem_total_bytes"`
    Fingerprint   string `json:"fingerprint,omitempty"`         // /etc/machine-id 等
    HardwareFingerprint string `json:"hardware_fingerprint,omitempty"` // 物理 NIC MAC + CPU + disk serial，抗 clone
    IPAddress     string `json:"ip_address,omitempty"`
}
```

`HardwareFingerprint` 是 issue #96 的修复——克隆 Linux VM 共享 SMBIOS product_uuid，gopsutil HostID 会撞，但物理 NIC MAC 由 hypervisor 重生成，作为更强指纹。

[messages.go:257-267](file:///d:/claude/ongrid/internal/pkg/tunnel/messages.go#L257-L267) `KubernetesInfo` 区分 controller / node 模式。

[messages.go:269-282](file:///d:/claude/ongrid/internal/pkg/tunnel/messages.go#L269-L282) `RegisterEdgeRequest` / `RegisterEdgeResponse`——注意 Request 的 `AccessKey` / `SecretKey` 字段保留但**当前 edge 留空**（[edgeagent/biz/agent.go:362-363](file:///d:/claude/ongrid/internal/edgeagent/biz/agent.go#L362-L363)），凭据走 geminio Meta blob。

[messages.go:292-332](file:///d:/claude/ongrid/internal/pkg/tunnel/messages.go#L292-L332) `HeartbeatRequest` 携带 `Ts` + 可选 `Plugins` 健康（piggyback）。

[messages.go:339-349](file:///d:/claude/ongrid/internal/pkg/tunnel/messages.go#L339-L349) `HostMetricPoint` 是 8 字段快速路径：`CPUPct` / `MemPct` / `Load1/5/15` / `NetRxBps` / `NetTxBps` / `DiskUsedPct`。

### 3.4 Meta blob

[tunnel/types.go:8-14](file:///d:/claude/ongrid/internal/pkg/tunnel/types.go#L8-L14)：

```go
type Session struct {
    EdgeID uint64
}
```

[tunnel/client.go:97-103](file:///d:/claude/ongrid/internal/pkg/tunnel/client.go#L97-L103) Dial 时把 `{access_key, secret_key}` 序列化成 Meta blob 发给 frontier，frontier 转给 manager 的 `GetEdgeID` 回调鉴权。**body 里的凭据字段是 legacy**，鉴权走 Meta。

---

## 4. Edge 侧：tunnel.Client（geminio SDK 封装）

[internal/pkg/tunnel/client.go](file:///d:/claude/ongrid/internal/pkg/tunnel/client.go) 是 `github.com/singchia/geminio` 的 `RetryEnd` 薄封装。

### 4.1 Client 接口与实现

[tunnel/types.go:68-105](file:///d:/claude/ongrid/internal/pkg/tunnel/types.go#L68-L105) 定义接口：

```go
type Client interface {
    Dial(ctx context.Context) error
    RegisterHandler(method string, h Handler)
    Call(ctx context.Context, method string, req, resp any) error
    AcceptStream() (StreamConn, error)
    OnReconnect(fn func())
    Close() error
}
```

[client.go:41-67](file:///d:/claude/ongrid/internal/pkg/tunnel/client.go#L41-L67) `geminioClient` 实现，关键字段：

```go
type geminioClient struct {
    cfg ClientConfig
    log *slog.Logger

    handlersMu sync.RWMutex
    handlers   map[string]Handler      // Dial 前后都能注册

    reconnectMu        sync.Mutex
    reconnectCallbacks []func()         // OnReconnect 注册的回调
    reconnectRunMu     sync.Mutex       // 串行执行回调

    endPtr atomic.Pointer[geminio.End]  // 当前 End，原子读写

    connMu            sync.Mutex
    activeConn        net.Conn          // 当前活跃 TCP 连接
    activeGeneration  uint64            // 活跃连接代际
    pendingConn       net.Conn          // 重连中的新连接
    pendingGeneration uint64
    nextGeneration    uint64

    closeOnce sync.Once
    closed    atomic.Bool
}
```

**代际机制**是关键——`trackConnection` 在 `net.Dial` 成功时把 conn 暂存为 `pendingConn` 并分配新代际；`promotePendingConnection` 在 geminio `RetryEnd` 完成原子切换后把 pending 提升为 active。这避免了"重连中"的 RPC 误把新连接当旧连接关闭。

### 4.2 Dial：指数退避 + 回调注册

[client.go:85-165](file:///d:/claude/ongrid/internal/pkg/tunnel/client.go#L85-L165)：

```go
func (c *geminioClient) Dial(ctx context.Context) error {
    if c.closed.Load() { return errors.New("tunnel: client closed") }

    dialer, err := c.buildDialer()          // TCP 或 TLS 拨号器
    meta, _ := json.Marshal(Meta{AccessKey, SecretKey})

    backoff := time.Second
    const maxBackoff = 60 * time.Second

    for {
        if err := ctx.Err(); err != nil { return err }

        opt := gclient.NewEndOptions()
        opt.SetMeta(meta)
        opt.SetDelegate(&retryDelegate{...})  // EndReOnline 回调

        end, derr := gclient.NewRetryEndWithDialer(dialer, opt)
        if derr == nil {
            c.promotePendingConnection()
            c.endPtr.Store(&end)
            // 重新注册所有 handler（首次 Dial 后无需，RetryEnd 内部记忆）
            c.handlersMu.RLock()
            methods := ... // 复制
            c.handlersMu.RUnlock()
            for method, h := range methods {
                c.registerOn(end, method, h)
            }
            return nil
        }

        c.log.Warn("tunnel: dial failed; will retry", ...)
        select {
        case <-ctx.Done(): return ctx.Err()
        case <-time.After(backoff):
        }
        backoff *= 2
        if backoff > maxBackoff { backoff = maxBackoff }
    }
}
```

设计要点：
- **`NewRetryEndWithDialer`** 是 geminio 的"自愈 End"——首次拨号成功后，连接丢失时它自动用同一个 dialer 重连。
- **首次失败重试在我们这里**：`for` 循环 + 指数退避（1s → 60s 封顶），让 edge 启动时 frontier 未就绪也能等。
- **后续断线重连在 geminio 内部**：`RetryEnd` 自带重连逻辑，我们的 `retryDelegate.EndReOnline` 是它重连成功后的回调。
- **handler 重注册只在首次 Dial 后做一次**：`RetryEnd` 内部记忆已注册的 RPC，后续重连不需要我们重新注册。

### 4.3 buildDialer：TCP / TLS 双模

[client.go:168-206](file:///d:/claude/ongrid/internal/pkg/tunnel/client.go#L168-L206)：

```go
func (c *geminioClient) buildDialer() (gclient.Dialer, error) {
    addr := c.cfg.resolvedServerAddr()
    caFile := c.cfg.resolvedTLSCA()
    if addr == "" { return nil, errors.New("tunnel: ServerAddr ...") }
    if caFile == "" {
        d := &net.Dialer{Timeout: 10 * time.Second}
        return func() (net.Conn, error) {
            conn, err := d.Dial("tcp", addr)
            if err == nil { c.trackConnection(conn) }
            return conn, err
        }, nil
    }
    // TLS 模式：读 CA PEM，tls.Config{MinVersion: TLS 1.2}
    pem, err := os.ReadFile(caFile)
    pool := x509.NewCertPool()
    pool.AppendCertsFromPEM(pem)
    tlsCfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
    d := &net.Dialer{Timeout: 10 * time.Second}
    return func() (net.Conn, error) {
        raw, err := d.Dial("tcp", addr)
        if err != nil { return nil, err }
        conn := tls.Client(raw, tlsCfg)
        c.trackConnection(conn)
        return conn, nil
    }, nil
}
```

`resolvedServerAddr` / `resolvedTLSCA`（[types.go:53-66](file:///d:/claude/ongrid/internal/pkg/tunnel/types.go#L53-L66)）是 Phase 1 命名兼容层：`ServerAddr` 优先于 `CloudAddr`，`TLSCAFile` 优先于 `TLSCA`。

### 4.4 RegisterHandler：Dial 前后都能注册

[client.go:211-224](file:///d:/claude/ongrid/internal/pkg/tunnel/client.go#L211-L224)：

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

`agent.registerHandlers()`（[edgeagent/biz/agent.go:272-349](file:///d:/claude/ongrid/internal/edgeagent/biz/agent.go#L272-L349)）在 Dial **之前**调用，handler 进 map 等 Dial 时统一注册；后续重连由 geminio 内部自动重注册。这避免了"先 Dial 再 Register 时错过首批 RPC"的竞态。

### 4.5 Call：主动 RPC + broken route 回收

[client.go:244-270](file:///d:/claude/ongrid/internal/pkg/tunnel/client.go#L244-L270)：

```go
func (c *geminioClient) Call(ctx context.Context, method string, req, resp any) error {
    end := c.loadEnd()
    if end == nil { return errors.New("tunnel: not dialed") }
    body, err := json.Marshal(req)
    if err != nil { return fmt.Errorf("marshal %q req: %w", method, err) }
    connGeneration := c.connectionGeneration()  // 记录调用时的活跃代际
    rsp, callErr := end.Call(ctx, method, end.NewRequest(body))
    if callErr != nil {
        c.recycleBrokenRoute(method, callErr, connGeneration)
        return fmt.Errorf("tunnel call %q: %w", method, callErr)
    }
    if rerr := rsp.Error(); rerr != nil {
        c.recycleBrokenRoute(method, rerr, connGeneration)
        return fmt.Errorf("tunnel call %q: remote: %w", method, rerr)
    }
    if resp == nil { return nil }
    if err := json.Unmarshal(rsp.Data(), resp); err != nil {
        return fmt.Errorf("unmarshal %q resp: %w", method, err)
    }
    return nil
}
```

### 4.6 recycleBrokenRoute：精准回收 stale 连接

[client.go:326-361](file:///d:/claude/ongrid/internal/pkg/tunnel/client.go#L326-L361) 是关键的恢复机制：

```go
func (c *geminioClient) recycleBrokenRoute(method string, err error, generation uint64) {
    if !shouldRecycleBrokenRoute(method, err) || c.closed.Load() { return }
    c.connMu.Lock()
    // 只关闭"调用时的代际 == 当前活跃代际"的连接
    // 防止误关重连后的新连接
    if generation == 0 || generation != c.activeGeneration || c.activeConn == nil {
        c.connMu.Unlock()
        return
    }
    conn := c.activeConn
    c.activeConn = nil
    c.connMu.Unlock()

    c.log.Warn("tunnel: frontier route is stale; recycling transport", ...)
    if closeErr := conn.Close(); closeErr != nil { ... }
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

**触发条件**：仅 `register_edge` 或 `heartbeat` 失败，且错误消息含 `"no such rpc"` / `"mismatch clientID"` / `"edge binding not ready"` / `"edge id mismatch"` 等"frontier 路由失效"信号。

**回收策略**：关闭当前活跃 TCP 连接，让 geminio `RetryEnd` 重新拨号。代际校验防止"重连已开始但回调未触发"时误关新连接。

**为什么不直接重建 End**：注释 [client.go:322-325](file:///d:/claude/ongrid/internal/pkg/tunnel/client.go#L322-L325) 说明——手动构造第二个 RetryEnd 会与第一个 transport 产生并发竞态。关闭连接让 RetryEnd 单线程串行重连是更安全的恢复路径。

### 4.7 retryDelegate.EndReOnline：重连完成回调

[client.go:69-79](file:///d:/claude/ongrid/internal/pkg/tunnel/client.go#L69-L79)：

```go
type retryDelegate struct {
    *delegate.UnimplementedDelegate
    client *geminioClient
}

func (d *retryDelegate) EndReOnline(_ delegate.ClientDescriber) {
    d.client.promotePendingConnection()
    // RetryEnd 持锁状态下不能直接调 RPC，所以异步触发回调
    go d.client.fireReconnectCallbacks()
}
```

`EndReOnline` 是 geminio `RetryEnd` 重连成功后的钩子。先提升 pending 连接为 active（让后续 Call 用新代际），再异步触发 `OnReconnect` 注册的回调（让 edge agent 重新发 `register_edge`）。

### 4.8 OnReconnect / fireReconnectCallbacks

[client.go:366-399](file:///d:/claude/ongrid/internal/pkg/tunnel/client.go#L366-L399)：

```go
func (c *geminioClient) OnReconnect(fn func()) {
    if fn == nil { return }
    c.reconnectMu.Lock()
    c.reconnectCallbacks = append(c.reconnectCallbacks, fn)
    c.reconnectMu.Unlock()
}
func (c *geminioClient) fireReconnectCallbacks() {
    c.reconnectRunMu.Lock()
    defer c.reconnectRunMu.Unlock()
    if c.closed.Load() { return }
    c.reconnectMu.Lock()
    cbs := append([]func(){}, c.reconnectCallbacks...)
    c.reconnectMu.Unlock()
    for _, fn := range cbs {
        // panic recover，防止单个回调卡死整个重连流程
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

`reconnectRunMu` 串行化多次重连的回调执行——避免"上一次重连回调还在跑，下一次重连又触发"的并发。

### 4.9 AcceptStream：被动接受流

[client.go:428-449](file:///d:/claude/ongrid/internal/pkg/tunnel/client.go#L428-L449)：

```go
func (c *geminioClient) AcceptStream() (StreamConn, error) {
    end := c.loadEnd()
    if end == nil { return nil, errors.New("tunnel: not dialed") }
    s, err := end.AcceptStream()
    if err != nil { return nil, err }
    return geminioStreamWrap{s: s}, nil
}

type geminioStreamWrap struct{ s geminio.Stream }

func (w geminioStreamWrap) Read(p []byte) (int, error)  { return w.s.Read(p) }
func (w geminioStreamWrap) Write(p []byte) (int, error) { return w.s.Write(p) }
func (w geminioStreamWrap) Close() error                { return w.s.Close() }
func (w geminioStreamWrap) Meta() []byte                { return w.s.Meta() }
```

`geminioStreamWrap` 只暴露 `StreamConn` 接口（[types.go:110-118](file:///d:/claude/ongrid/internal/pkg/tunnel/types.go#L110-L118)），防止调用方耦合 geminio 内部类型。WebSSH 路径用这个——manager `OpenStream` 推过来，edge `AcceptStream` 接住后 `io.Copy` 到本地 sshd。

---

## 5. Edge 侧：edgeagent/biz/Agent 运行时

[internal/edgeagent/biz/agent.go](file:///d:/claude/ongrid/internal/edgeagent/biz/agent.go) 是 edge agent 的核心 run-loop。

### 5.1 Agent 结构

[agent.go:77-100](file:///d:/claude/ongrid/internal/edgeagent/biz/agent.go#L77-L100)：

```go
type Agent struct {
    client    tunnel.Client
    collector Collector
    cfg       Config
    log       *slog.Logger

    edgeID     uint64            // register_edge 后由 cloud 分配
    mu         sync.RWMutex
    registerMu sync.Mutex       // 串行化 registerEdge 调用

    upgradeRequested chan struct{} // agent_upgrade handler 触发，Run 返回让 systemd 重启

    pluginHealthFn func() []tunnel.PluginHealthWire  // 心跳 piggyback
}
```

### 5.2 Run：8 步启动序列

[agent.go:152-230](file:///d:/claude/ongrid/internal/edgeagent/biz/agent.go#L152-L230)：

```go
func (a *Agent) Run(ctx context.Context) error {
    // 1. 注册 cloud→edge 反向 handler（get_host_load / execute_skill / shell_* / agent_upgrade）
    a.registerHandlers()

    // 2. 注册 OnReconnect 回调：重连后重新 register_edge
    a.client.OnReconnect(func() {
        rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()
        if err := a.registerEdge(rctx); err != nil {
            a.log.Warn("agent: re-register after tunnel reconnect failed", ...)
            return
        }
        a.log.Info("agent: re-registered after tunnel reconnect", ...)
    })

    // 3. Dial（阻塞直到成功或 ctx 取消）
    if err := a.client.Dial(ctx); err != nil {
        if errors.Is(err, context.Canceled) { return nil }
        return fmt.Errorf("agent dial: %w", err)
    }

    // 4. register_edge（首次注册）
    if err := a.registerEdge(ctx); err != nil {
        a.log.Warn("agent: register_edge failed; will keep running", ...)
    } else {
        a.writeHealthMarker()  // 健康标记，供 apply-pending-upgrade.sh 判断是否回滚
    }

    // 5. errgroup 启动三个 goroutine
    eg, egCtx := errgroup.WithContext(ctx)
    eg.Go(func() error { return a.heartbeatLoop(egCtx) })
    eg.Go(func() error { return a.metricsLoop(egCtx) })
    eg.Go(func() error {
        select {
        case <-egCtx.Done(): return nil
        case <-a.upgradeRequested:
            a.log.Info("agent: exiting cleanly for upgrade swap")
            return errUpgradeRequested  // 哨兵，让 errgroup 取消其他 goroutine
        }
    })

    err := eg.Wait()
    _ = a.client.Close()
    if errors.Is(err, errUpgradeRequested) { return nil }  // 升级退出转 nil，让 systemd 视为干净退出
    if err != nil && !errors.Is(err, context.Canceled) { return err }
    return nil
}
```

### 5.3 registerEdge：首次握手

[agent.go:352-380](file:///d:/claude/ongrid/internal/edgeagent/biz/agent.go#L352-L380)：

```go
func (a *Agent) registerEdge(ctx context.Context) error {
    a.registerMu.Lock()
    defer a.registerMu.Unlock()

    info, err := a.collector.HostInfo(ctx)
    if err != nil { a.log.Warn("agent: HostInfo collection failed", ...) }
    applyKubernetesHostIdentity(a.cfg.Kubernetes, &info)
    req := tunnel.RegisterEdgeRequest{
        AccessKey:    "", // 凭据走 Meta，body 留空
        SecretKey:    "",
        HostInfo:     info,
        AgentVersion: a.cfg.AgentVersion,
        Kubernetes:   a.cfg.Kubernetes,
    }
    var resp tunnel.RegisterEdgeResponse
    if err := a.client.Call(ctx, tunnel.MethodRegisterEdge, req, &resp); err != nil {
        return err
    }
    a.mu.Lock()
    a.edgeID = resp.EdgeID
    a.mu.Unlock()
    a.log.Info("agent: registered with cloud", ...)
    return nil
}
```

`applyKubernetesHostIdentity`（[agent.go:382-398](file:///d:/claude/ongrid/internal/edgeagent/biz/agent.go#L382-L398)）在 K8s node 模式下用 NodeName 覆盖 Hostname、用 `k8s-node:<cluster>:<nodeUID>` 作为指纹——让 cloud 能识别同一 K8s node 的多次注册。

### 5.4 heartbeatLoop：30s 心跳 + 自愈

[agent.go:406-472](file:///d:/claude/ongrid/internal/edgeagent/biz/agent.go#L406-L472)：

```go
func (a *Agent) heartbeatLoop(ctx context.Context) error {
    t := time.NewTicker(a.cfg.HeartbeatInterval)  // 默认 30s
    defer t.Stop()
    var consecutiveFail int
    for {
        select {
        case <-ctx.Done(): return nil
        case <-t.C:
            if a.EdgeID() == 0 {
                // 首次注册失败的重试路径
                rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
                err := a.registerEdge(rctx)
                cancel()
                if err != nil {
                    consecutiveFail++
                    a.log.Warn("agent: register_edge retry failed", ...)
                    continue
                }
                a.writeHealthMarker()
                consecutiveFail = 0
            }

            // piggyback plugin health
            a.mu.RLock()
            healthFn := a.pluginHealthFn
            a.mu.RUnlock()
            var plugins []tunnel.PluginHealthWire
            if healthFn != nil { plugins = healthFn() }

            rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
            err := a.client.Call(rctx, tunnel.MethodHeartbeat,
                tunnel.HeartbeatRequest{
                    EdgeID:  a.EdgeID(),
                    Ts:      time.Now().Unix(),
                    Plugins: plugins,
                }, nil)
            cancel()
            if err != nil {
                consecutiveFail++
                a.log.Warn("agent: heartbeat failed", ...)
                // manager 重启可能丢了 in-memory binding 但 frontier 连接还活着
                // 主动 re-register 修复状态
                rctx, rcancel := context.WithTimeout(ctx, 10*time.Second)
                rerr := a.registerEdge(rctx)
                rcancel()
                if rerr != nil {
                    if consecutiveFail >= tunnelStuckThreshold {  // 5 次
                        return fmt.Errorf("%w after %d heartbeat failures: %v",
                            errTunnelStuck, consecutiveFail, rerr)
                    }
                } else {
                    a.writeHealthMarker()
                    consecutiveFail = 0
                }
                continue
            }
            consecutiveFail = 0
        }
    }
}
```

`tunnelStuckThreshold = 5`（[agent.go:474](file:///d:/claude/ongrid/internal/edgeagent/biz/agent.go#L474)）——连续 5 次心跳失败 + 5 次 re-register 失败才返回 `errTunnelStuck` 让 Run 退出（systemd 会重启）。

### 5.5 metricsLoop：双路径推送

[agent.go:487-563](file:///d:/claude/ongrid/internal/edgeagent/biz/agent.go#L487-L563) 每个 tick（默认 10s）调用 `collector.CollectAll`，对每个 `CollectorOutput` 走两条路径：

1. **legacy 快速路径**：`push_host_metrics`，8 字段 HostMetricPoint，dashboard / alert 用
2. **open-set 路径**：`push_prom_samples`，Prometheus 远程写入用

两条路径独立失败独立重试，下一个 tick 全新数据。

### 5.6 registerHandlers：反向 RPC 注册

[agent.go:272-349](file:///d:/claude/ongrid/internal/edgeagent/biz/agent.go#L272-L349) 注册：

- `get_host_load` → `collector.GetHostLoad`
- `get_process_list` → `collector.GetProcessList`（带 TopN / SortBy 参数）
- `execute_skill` → `skilldispatch.Dispatch`（skill 框架统一入口）
- `agent_upgrade` / `fetch_package` / `apply_package`：仅当 `UpgradeStageDir != ""` 注册（dev / 无 systemd 时缺失，manager 看到"method not found"）

### 5.7 Edge 启动装配

[cmd/ongrid-edge/main.go:130-135](file:///d:/claude/ongrid/cmd/ongrid-edge/main.go#L130-L135)：

```go
client := tunnel.NewClient(tunnel.ClientConfig{
    CloudAddr:  cfg.Edge.CloudAddr,
    AccessKey:  cfg.Edge.AccessKey,
    SecretKey:  cfg.Edge.SecretKey,
    Log:        log,
})
```

`CloudAddr` 即 frontier edgebound 地址，生产环境通常 `frontier:40012` 或公网域名。

---

## 6. Manager 侧：frontierbound.Client（service-end SDK 封装）

[internal/manager/service/frontierbound/client.go](file:///d:/claude/ongrid/internal/manager/service/frontierbound/client.go) 是 `fbsvc.Service` 的薄封装。

### 6.1 service 接口与 Client 结构

[client.go:35-47](file:///d:/claude/ongrid/internal/manager/service/frontierbound/client.go#L35-L47)：

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

[client.go:51-60](file:///d:/claude/ongrid/internal/manager/service/frontierbound/client.go#L51-L60) `Client`：

```go
type Client struct {
    svc service
    log *slog.Logger

    mu                sync.RWMutex
    transportToEdgeID map[uint64]uint64  // frontier 分配的 transport ID → canonical edge_id
    edgeIDToTransport map[uint64]uint64  // 反向
    transportAddrs    map[uint64]string  // transport → 远端 addr（防 stale offline）
    k8sControllers    map[uint64]bool    // edge_id → 是否 K8s controller
}
```

两个 map 是双向映射——frontier 用 transport ID 路由（每次重连变），manager 用 canonical edge_id（DB 主键，不变）。`transportAddrs` 防 stale offline 事件删错新连接（§8 详述）。

### 6.2 New / NewDisabled

[client.go:63-93](file:///d:/claude/ongrid/internal/manager/service/frontierbound/client.go#L63-L93) `New`：

```go
func New(cfg Config, log *slog.Logger) (*Client, error) {
    if cfg.Addr == "" { return nil, errors.New("frontierbound: cfg.Addr is required") }
    dialer := func() (net.Conn, error) { return net.Dial("tcp", cfg.Addr) }
    opts := []fbsvc.ServiceOption{}
    if cfg.ServiceName != "" {
        opts = append(opts, fbsvc.OptionServiceName(cfg.ServiceName))
    }
    svc, err := fbsvc.NewService(dialer, opts...)
    if err != nil { return nil, fmt.Errorf("frontierbound: NewService: %w", err) }
    log.Info("frontierbound: connected", ...)
    return &Client{svc: svc, ..., maps: ...}, nil
}
```

[client.go:115-134](file:///d:/claude/ongrid/internal/manager/service/frontierbound/client.go#L115-L134) `NewDisabled`：

```go
var ErrDisabled = errors.New("frontierbound: disabled")

func NewDisabled(log *slog.Logger) *Client {
    return &Client{svc: nil, ..., maps: ...}
}
```

`NewDisabled` 让 `svc=nil`——所有 `Call` / `OpenStream` 返回 `ErrDisabled`，所有 `Register*` 是 no-op。用于 e2e 测试和降级 broker 启动场景（[main.go:1047-1049](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1047-L1049)）。

### 6.3 Call：edge→cloud 的反向

[client.go:139-153](file:///d:/claude/ongrid/internal/manager/service/frontierbound/client.go#L139-L153)：

```go
func (c *Client) Call(ctx context.Context, edgeID uint64, method string, body []byte) ([]byte, error) {
    if c.svc == nil { return nil, ErrDisabled }
    transportID := c.resolveTransportID(edgeID)  // canonical → transport
    req := c.svc.NewRequest(body)
    rsp, err := c.svc.Call(ctx, transportID, method, req)
    if err != nil {
        return nil, fmt.Errorf("frontierbound: call %q edge=%d transport=%d: %w",
            method, edgeID, transportID, err)
    }
    if rerr := rsp.Error(); rerr != nil {
        return nil, fmt.Errorf("frontierbound: remote %q edge=%d transport=%d: %w",
            method, edgeID, transportID, rerr)
    }
    return rsp.Data(), nil
}
```

错误信息同时打印 `edge_id`（业务可读）和 `transport_id`（debug 用）。

### 6.4 Register：cloud→edge 反向 RPC 的 adapter

[client.go:181-198](file:///d:/claude/ongrid/internal/manager/service/frontierbound/client.go#L181-L198)：

```go
func (c *Client) Register(ctx context.Context, method string, h Handler) error {
    if h == nil { return fmt.Errorf("frontierbound: nil handler for %q", method) }
    if c.svc == nil { return nil }
    wrap := func(rpcCtx context.Context, req geminio.Request, rsp geminio.Response) {
        edgeID := req.ClientID()  // frontier 在转发时填的 transport ID
        out, err := h(rpcCtx, edgeID, req.Data())
        if err != nil { rsp.SetError(err); return }
        rsp.SetData(out)
    }
    return c.svc.Register(ctx, method, wrap)
}
```

`Handler` 签名（[client.go:29](file:///d:/claude/ongrid/internal/manager/service/frontierbound/client.go#L29)）：

```go
type Handler func(ctx context.Context, edgeID uint64, body []byte) ([]byte, error)
```

`req.ClientID()` 是 frontier 转发时填的 transport ID——handler 内部用 `canonicalizeEdgeID` 映射到 canonical edge_id 才能用。

### 6.5 三个生命周期注册

[client.go:203-224](file:///d:/claude/ongrid/internal/manager/service/frontierbound/client.go#L203-L224)：

```go
func (c *Client) RegisterGetEdgeID(ctx, fn func(meta []byte) (uint64, error)) error
func (c *Client) RegisterEdgeOnline(ctx, fn func(edgeID uint64, meta []byte, addr net.Addr) error) error
func (c *Client) RegisterEdgeOffline(ctx, fn func(edgeID uint64, meta []byte, addr net.Addr) error) error
```

这三个是 frontier service-end SDK 的回调契约：

- **GetEdgeID**：edge dial 时 frontier 调，传入 edge 的 Meta blob，返回 canonical edge_id（鉴权失败返 error 让 frontier 拒绝 dial）
- **EdgeOnline**：edge TCP/geminio 会话建立后 frontier 调
- **EdgeOffline**：会话关闭时 frontier 调

### 6.6 OpenStream：双向流

[client.go:240-250](file:///d:/claude/ongrid/internal/manager/service/frontierbound/client.go#L240-L250)：

```go
func (c *Client) OpenStream(ctx context.Context, edgeID uint64) (geminio.Stream, error) {
    if c.svc == nil { return nil, ErrDisabled }
    transportID := c.resolveTransportID(edgeID)
    s, err := c.svc.OpenStream(ctx, transportID)
    if err != nil {
        return nil, fmt.Errorf("frontierbound: open stream edge=%d transport=%d: %w",
            edgeID, transportID, err)
    }
    return s, nil
}
```

`geminio.Stream` 满足 `io.ReadWriteCloser`（embed `Raw = net.Conn`），WebSSH 直接在上面跑 SSH 协议（[webshell/http.go:285](file:///d:/claude/ongrid/internal/manager/server/webshell/http.go#L285)）。

---

## 7. Manager 侧：frontierbound.Install 反向 RPC 与生命周期回调

[internal/manager/service/frontierbound/handlers.go](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go) 是 manager 侧的核心——一次性注册全部反向 RPC + 生命周期回调。

### 7.1 Wiring：依赖注入包

[handlers.go:61-89](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L61-L89)：

```go
type Wiring struct {
    EdgeAuthn      EdgeAuthenticator       // 凭据鉴权（必需）
    EdgeUC         *edgebiz.Usecase        // edge 业务用例（必需）
    MetricIngester metricbiz.IngestService // 主机指标写入（必需）
    PromIngester   PromwriteIngester       // Prom 远程写入（可选，nil = Prom 禁用）
    PluginConfigUC PluginConfigFetcher     // 可选
    WebshellRouter WebshellRouter          // 可选
    DeviceResolver DeviceResolver          // 可选，edge_id → device_id
    K8sRegistry    KubernetesRegistry      // 可选
    K8sInventory   KubernetesInventoryIngester  // 可选
    Log            *slog.Logger
}
```

可选字段 nil 时对应 handler 不注册或静默 drop——这是"渐进降级"设计。

### 7.2 Install 主流程

[handlers.go:110-614](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L110-L614) 一次性注册：

1. **GetEdgeID**：[handlers.go:166-168](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L166-L168)
2. **EdgeOnline**：[handlers.go:170-189](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L170-L189)
3. **EdgeOffline**：[handlers.go:191-225](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L191-L225)
4. **register_edge**：[handlers.go:228-292](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L228-L292)
5. **heartbeat**：[handlers.go:295-360](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L295-L360)
6. **push_k8s_inventory**：[handlers.go:362-409](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L362-L409)
7. **push_host_metrics**：[handlers.go:412-453](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L412-L453)
8. **push_prom_samples**：[handlers.go:458-550](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L458-L550)
9. **get_plugin_configs**：[handlers.go:556-581](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L556-L581)（条件注册）
10. **shell_output / shell_exit**：[handlers.go:586-610](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L586-L610)（条件注册）

### 7.3 GetEdgeID：edge dial 鉴权

[handlers.go:146-168](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L146-L168)：

```go
resolveEdgeID := func(meta []byte) (uint64, error) {
    var m tunnel.Meta
    if err := json.Unmarshal(meta, &m); err != nil {
        return 0, fmt.Errorf("bad meta: %w", err)
    }
    edgeID, err := authenticateEdge(ctx, m.AccessKey, m.SecretKey)
    if err != nil {
        log.Warn("frontierbound: edge authn failed", ...)
        return 0, err
    }
    log.Info("frontierbound: GetEdgeID: authn ok", slog.Uint64("edge_id", edgeID))
    return edgeID, nil
}

if err := c.RegisterGetEdgeID(ctx, resolveEdgeID); err != nil { ... }
```

`AccessKeyAuthenticator.Authenticate`（edge biz 实现）把所有失败路径 collapse 到 `errs.ErrUnauthorized`，不泄露"key 不存在" vs "key 错"的枚举信号。

### 7.4 EdgeOnline：建立 binding

[handlers.go:170-189](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L170-L189)：

```go
c.RegisterEdgeOnline(ctx, func(edgeID uint64, meta []byte, addr net.Addr) error {
    canonicalEdgeID, err := resolveEdgeID(meta)
    if err == nil {
        c.bindEdgeTransportAt(edgeID, canonicalEdgeID, safeAddr(addr))
    }
    log.Info("frontierbound: edge online",
        slog.Uint64("edge_id", canonicalEdgeID),
        slog.Uint64("transport_edge_id", edgeID),
        slog.String("addr", safeAddr(addr)),
    )
    if err != nil { return err }
    return nil
})
```

注释 [handlers.go:182-186](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L182-L186) 说明：实时 edge_offline 告警已移除，靠 `PipelineEvaluator` 下一 tick（30s）刷新 `edge_last_seen_seconds_ago` gauge 自动触发 / 自动恢复。

### 7.5 EdgeOffline：stale 事件过滤

[handlers.go:191-225](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L191-L225)：

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

`unbindEdgeTransport`（[client.go:293-323](file:///d:/claude/ongrid/internal/manager/service/frontierbound/client.go#L293-L323)）三重校验防 stale 事件删错新连接：
1. `canonicalEdgeID` 必须匹配 mapped value
2. `activeTransportID` 必须等于要删的 transport
3. `activeAddr` 必须等于事件中的 addr

任一不符 → 返回 false，忽略 stale 事件。这是 frontier "旧连接的 offline 事件在新连接 online 之后到达"的常见竞态修复。

### 7.6 register_edge：核心注册 handler

[handlers.go:228-292](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L228-L292) 是最关键的反向 RPC：

```go
c.Register(ctx, tunnel.MethodRegisterEdge, func(rpcCtx, edgeID, body) ([]byte, error) {
    var in tunnel.RegisterEdgeRequest
    if err := json.Unmarshal(body, &in); err != nil { ... }

    // frontier 已在 GetEdgeID 鉴权，canonical edge_id 通过 req.ClientID() 传来
    canonicalEdgeID := edgeID
    if canonicalEdgeID == 0 {
        return nil, fmt.Errorf("register_edge: authenticated edge id is missing")
    }
    c.bindEdgeTransport(edgeID, canonicalEdgeID)

    if in.Kubernetes != nil && isKubernetesControllerRole(in.Kubernetes.Role) {
        // K8s controller 路径：清 host link + 注册 controller + heartbeat
        w.EdgeUC.ClearHostDeviceLink(rpcCtx, canonicalEdgeID)
        w.K8sRegistry.HandleRegister(rpcCtx, canonicalEdgeID, nil, *in.Kubernetes)
        w.EdgeUC.HandleHeartbeat(rpcCtx, canonicalEdgeID, time.Now().UTC())
        c.setKubernetesController(canonicalEdgeID, true)
    } else {
        // 主机 / K8s node 路径：HandleRegister 持久化 HostInfo + status=online
        w.EdgeUC.HandleRegister(rpcCtx, canonicalEdgeID, in.HostInfo, in.AgentVersion)
        if in.Kubernetes != nil && w.K8sRegistry != nil {
            // K8s node：额外注册到 K8s registry
            var deviceID *uint64
            if w.DeviceResolver != nil {
                if resolved, err := w.DeviceResolver.LookupHostDevice(rpcCtx, canonicalEdgeID); err == nil {
                    deviceID = &resolved
                }
            }
            w.K8sRegistry.HandleRegister(rpcCtx, canonicalEdgeID, deviceID, *in.Kubernetes)
        }
        c.setKubernetesController(canonicalEdgeID, false)
    }
    c.bindEdgeTransport(edgeID, canonicalEdgeID)
    out := tunnel.RegisterEdgeResponse{
        EdgeID:     canonicalEdgeID,
        ServerTime: time.Now().UTC().Unix(),
    }
    return json.Marshal(out)
})
```

注释 [handlers.go:233-238](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L233-L238) 强调：**不要信任 body 里的凭据字段**——当前 edge 留空，凭据只走 Meta。manager 重启后 in-memory binding 丢失，但 frontier 保持 edge 连接，所以 `register_edge` 重建 binding 是恢复路径。

### 7.7 heartbeat：心跳 + plugin health

[handlers.go:295-360](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L295-L360)：

```go
c.Register(ctx, tunnel.MethodHeartbeat, func(rpcCtx, edgeID, body) ([]byte, error) {
    var in tunnel.HeartbeatRequest
    if err := json.Unmarshal(body, &in); err != nil { ... }

    canonicalEdgeID := c.canonicalizeEdgeID(edgeID)
    if canonicalEdgeID == 0 {
        return nil, fmt.Errorf("heartbeat: edge binding not ready; re-register required")
    }
    if in.EdgeID != 0 && in.EdgeID != canonicalEdgeID {
        return nil, fmt.Errorf("heartbeat: edge id mismatch")
    }
    ts := time.Unix(in.Ts, 0).UTC()
    if in.Ts == 0 { ts = time.Now().UTC() }
    w.EdgeUC.HandleHeartbeat(rpcCtx, canonicalEdgeID, ts)

    // K8s controller heartbeat 刷新（best-effort）
    refreshKubernetesControllerHeartbeat(rpcCtx, c, w.K8sRegistry, canonicalEdgeID)

    // piggyback plugin health
    if len(in.Plugins) > 0 {
        items := ... // 转换 wire → biz 类型
        w.EdgeUC.RecordPluginHealth(canonicalEdgeID, items)
    }
    return json.Marshal(tunnel.HeartbeatResponse{})
})
```

`heartbeat: edge binding not ready; re-register required` 这个错误信号会被 edge 侧 `recycleBrokenRoute` 捕获（§4.6），触发 frontier 连接回收 + 重连 + 重新 `register_edge`。

### 7.8 push_host_metrics：device_id 解析 + drop

[handlers.go:412-453](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L412-L453) 关键设计：

```go
canonicalEdgeID := c.canonicalizeEdgeID(edgeID)
if in.EdgeID != 0 && canonicalEdgeID != 0 && in.EdgeID != canonicalEdgeID {
    return nil, fmt.Errorf("push_host_metrics: edge id mismatch")
}
if canonicalEdgeID == 0 {
    // register_edge 未完成，silent drop
    return json.Marshal(tunnel.PushHostMetricsResponse{Accepted: 0})
}
deviceID := resolveDeviceID(rpcCtx, w.DeviceResolver, canonicalEdgeID)
if deviceID == 0 {
    // host junction 缺失——drop 而不是把 edge_id 当 device_id 写入
    log.Warn("frontierbound: push_host_metrics dropped — device_id unresolved ...", ...)
    return json.Marshal(tunnel.PushHostMetricsResponse{Accepted: 0})
}
w.MetricIngester.Push(rpcCtx, deviceID, in.Points)
```

`resolveDeviceID`（[handlers.go:657-669](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L657-L669)）**绝不 fallback 到 edge_id**——issue #96 的核心：edge_id 和 device_id 是独立自增序列，fallback 会把不存在的 device_id 写进 Prometheus TSDB，污染 Grafana 变量下拉框。

### 7.9 push_prom_samples：双路径 + K8s 探测

[handlers.go:458-550](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L458-L550) 比 host_metrics 复杂——多了一个 K8s controller 路径：

```go
if w.PromIngester == nil {
    // Prom 禁用：silent drop + Accepted=n，edge 不重试
    return json.Marshal(tunnel.PushPromSamplesResponse{Accepted: n})
}
if isKubernetesPromSource(in.Source) {  // source 前缀 "k8s:"
    clusterID := lookupK8sControllerCluster(rpcCtx, w.K8sRegistry, canonicalEdgeID, log)
    if clusterID == 0 {
        // controller cluster 未解析——drop
        return json.Marshal(tunnel.PushPromSamplesResponse{Accepted: n})
    }
    w.PromIngester.PushKubernetes(rpcCtx, clusterID, in.Source, in.Samples)
    return ...
}
// 主机路径：先解 device_id，device_id=0 时回退查 K8s controller cluster
deviceID := resolveDeviceID(rpcCtx, w.DeviceResolver, canonicalEdgeID)
if deviceID == 0 {
    clusterID := lookupK8sControllerCluster(...)
    if clusterID != 0 {
        w.PromIngester.PushKubernetes(...)
        return ...
    }
    // 都没——drop
    return json.Marshal(tunnel.PushPromSamplesResponse{Accepted: n})
}
w.PromIngester.Push(rpcCtx, deviceID, in.Source, in.Samples)
```

### 7.10 shell_output / shell_exit：WebSSH 回流

[handlers.go:586-610](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L586-L610)：

```go
if w.WebshellRouter != nil {
    c.Register(ctx, tunnel.MethodShellOutput, func(rpcCtx, _, body) ([]byte, error) {
        var in tunnel.ShellOutputRequest
        if err := json.Unmarshal(body, &in); err != nil { ... }
        if err := w.WebshellRouter.DispatchOutput(in.SessionID, in.Data); err != nil {
            log.Warn("frontierbound: shell_output dispatch", ...)
        }
        return json.Marshal(tunnel.ShellOutputResponse{})
    })
    c.Register(ctx, tunnel.MethodShellExit, func(rpcCtx, _, body) ([]byte, error) {
        var in tunnel.ShellExitRequest
        if err := json.Unmarshal(body, &in); err != nil { ... }
        w.WebshellRouter.DispatchExit(in.SessionID, in.ExitCode, in.Err)
        return json.Marshal(tunnel.ShellExitResponse{})
    })
}
```

`WebshellRouter`（[biz/webshell/router.go](file:///d:/claude/ongrid/internal/manager/biz/webshell/router.go)）按 `SessionID` 路由到活跃 WebSocket bridge——浏览器侧每个 WebSSH 会话有唯一 uuid SessionID。

### 7.11 NotifyPluginConfigsChanged：cloud → edge 主动推

[handlers.go:634-637](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L634-L637)：

```go
func (c *Client) NotifyPluginConfigsChanged(ctx context.Context, edgeID uint64) error {
    _, err := c.Call(ctx, edgeID, tunnel.MethodPluginConfigsChanged, []byte("{}"))
    return err
}
```

body 空设计——edge 收到后自己去 `get_plugin_configs` 拉，避免 push payload 和 pull response 的 wire 格式耦合。fire-and-forget，edge 60s 安全网轮询兜底。

---

## 8. Transport ↔ EdgeID 双向映射

[client.go:260-362](file:///d:/claude/ongrid/internal/manager/service/frontierbound/client.go#L260-L362) 是 manager 侧的核心数据结构。

### 8.1 bindEdgeTransportAt：双向绑定

[client.go:264-283](file:///d:/claude/ongrid/internal/manager/service/frontierbound/client.go#L264-L283)：

```go
func (c *Client) bindEdgeTransportAt(transportID, edgeID uint64, addr string) {
    if transportID == 0 || edgeID == 0 { return }
    c.mu.Lock()
    defer c.mu.Unlock()
    // 若 transport 之前绑过别的 edge，清掉反向映射
    if prevEdgeID, ok := c.transportToEdgeID[transportID]; ok && prevEdgeID != edgeID {
        delete(c.edgeIDToTransport, prevEdgeID)
        delete(c.transportAddrs, transportID)
    }
    // 若 edge 之前绑过别的 transport，清掉反向映射
    if prevTransportID, ok := c.edgeIDToTransport[edgeID]; ok && prevTransportID != transportID {
        delete(c.transportToEdgeID, prevTransportID)
        delete(c.transportAddrs, prevTransportID)
    }
    c.transportToEdgeID[transportID] = edgeID
    c.edgeIDToTransport[edgeID] = transportID
    if addr != "" { c.transportAddrs[transportID] = addr }
}
```

双向覆盖：换 transport 时清旧 transport 的反向；换 edge 时清旧 edge 的反向。这处理"edge 重连后 frontier 分配新 transport ID"的场景。

### 8.2 unbindEdgeTransport：三重校验

[client.go:293-323](file:///d:/claude/ongrid/internal/manager/service/frontierbound/client.go#L293-L323)：

```go
func (c *Client) unbindEdgeTransport(transportID, canonicalEdgeID uint64, addr string) bool {
    if transportID == 0 { return false }
    c.mu.Lock()
    defer c.mu.Unlock()
    mappedEdgeID, mapped := c.transportToEdgeID[transportID]
    if mapped {
        if canonicalEdgeID != 0 && canonicalEdgeID != mappedEdgeID {
            return false  // 校验 1：canonical 必须匹配
        }
        canonicalEdgeID = mappedEdgeID
    }
    if canonicalEdgeID == 0 { return false }
    activeTransportID, active := c.edgeIDToTransport[canonicalEdgeID]
    if active && activeTransportID != transportID {
        return false  // 校验 2：当前活跃 transport 必须等于要删的
    }
    if activeAddr := c.transportAddrs[transportID]; activeAddr != "" && addr != "" && activeAddr != addr {
        return false  // 校验 3：addr 必须匹配
    }
    delete(c.transportToEdgeID, transportID)
    delete(c.transportAddrs, transportID)
    if active { delete(c.edgeIDToTransport, canonicalEdgeID) }
    delete(c.k8sControllers, canonicalEdgeID)
    return true
}
```

三重校验全部通过才真正 unbind——任意一项不符说明这是 stale 事件，新连接已上线，必须忽略。

### 8.3 canonicalizeEdgeID / resolveTransportID

[client.go:346-374](file:///d:/claude/ongrid/internal/manager/service/frontierbound/client.go#L346-L374)：

```go
func (c *Client) canonicalizeEdgeID(edgeID uint64) uint64 {
    if edgeID == 0 { return 0 }
    c.mu.RLock()
    defer c.mu.RUnlock()
    if canonical, ok := c.transportToEdgeID[edgeID]; ok {
        return canonical
    }
    return 0  // binding 未建立，返回 0 让 caller drop
}

func (c *Client) resolveTransportID(edgeID uint64) uint64 {
    if edgeID == 0 { return 0 }
    c.mu.RLock()
    defer c.mu.RUnlock()
    if transportID, ok := c.edgeIDToTransport[edgeID]; ok {
        return transportID
    }
    return edgeID  // fallback：未绑定时返回原值（call 会失败但不卡死）
}
```

注释 [client.go:356-361](file:///d:/claude/ongrid/internal/manager/service/frontierbound/client.go#L356-L361) 强调 `canonicalizeEdgeID` 不 fallback——返回 0 让 caller drop 请求，避免把 transport ID 当 edge_id 写进 Prom label 制造 ghost series。

---

## 9. WebSSH：OpenStream + 双向 push

WebSSH 是 frontier 集成中最复杂的路径，分三个阶段：

### 9.1 阶段 1：浏览器 → manager → edge（OpenStream + shell_open）

[webshell/http.go:261-298](file:///d:/claude/ongrid/internal/manager/server/webshell/http.go#L261-L298)：

```go
streamCtx, cancelStreamOpen := context.WithTimeout(r.Context(), 10*time.Second)
stream, err := h.streamer.OpenStream(streamCtx, edge.ID)
cancelStreamOpen()
if err != nil {
    h.closeAudit(sid, br, 0, wsmodel.TerminatedByDisconnect)
    br.sendText(map[string]any{"type": "auth_error", "message": "edge unreachable: " + err.Error()})
    br.closeWith(websocket.CloseInternalServerErr, "open stream")
    return
}
// streamMeta 暂未透传，edge 默认 127.0.0.1:22
_ = streamMeta{Target: "127.0.0.1:22"}

// SSH 客户端 over tunnel stream
sshCfg := &ssh.ClientConfig{
    User:            openFrame.SSHUser,
    Auth:            []ssh.AuthMethod{ssh.Password(openFrame.SSHPass)},
    HostKeyCallback: ssh.InsecureIgnoreHostKey(), // localhost-only
    Timeout:         10 * time.Second,
}
openFrame.SSHPass = "" // wipe

sshConn, sshChans, sshReqs, sshErr := ssh.NewClientConn(rwcAdapter{rwc: stream}, "127.0.0.1:22", sshCfg)
```

`Streamer` 接口（[webshell/http.go:47-51](file:///d:/claude/ongrid/internal/manager/server/webshell/http.go#L47-L51)）：

```go
type Streamer interface {
    OpenStream(ctx context.Context, edgeID uint64) (io.ReadWriteCloser, error)
}
```

`*frontierbound.Client` 满足这个接口。`rwcAdapter` 把 `io.ReadWriteCloser` 适配成 `io.ReadWriteCloser`（geminio.Stream 已满足）。

注释 [webshell/http.go:269-274](file:///d:/claude/ongrid/internal/manager/server/webshell/http.go#L269-L274) 说明 Meta 透传暂未实现——Phase 2 加 jumpbox 时会通过 `geminio.OpenStreamOptions` 把 target meta 传过去，让 edge 能连非 127.0.0.1:22 的目标。

### 9.2 阶段 2：edge → 本地 sshd

edge 侧 AcceptStream 后 `io.Copy` 到本地 sshd socket（[tunnel/types.go:82-95](file:///d:/claude/ongrid/internal/pkg/tunnel/types.go#L82-L95) 注释）：

> the manager opens a stream, edge accepts and io.Copy's bytes to/from the sshd socket.

edge 是"哑字节转发器"——SSH 协议在 manager 侧的 `ssh.NewClientConn` 终结，edge 只是把 tunnel 字节流桥接到 `127.0.0.1:22`。

### 9.3 阶段 3：edge → manager → 浏览器（shell_output / shell_exit）

edge 把 sshd 的 stdout 通过 `shell_output` RPC 推给 manager（每 chunk 一帧），manager 通过 `WebshellRouter.DispatchOutput` 按 SessionID 路由到对应 WebSocket bridge（[handlers.go:587-599](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L587-L599)）。会话结束时 edge 推 `shell_exit`（带 exit code），manager 调 `DispatchExit` 通知浏览器。

### 9.4 WebshellRouter：会话路由

[biz/webshell/router.go](file:///d:/claude/ongrid/internal/manager/biz/webshell/router.go) 是 in-memory session 表：

- `Register(sid, s, m)` ([router.go:74](file:///d:/claude/ongrid/internal/manager/biz/webshell/router.go#L74))：WebSocket 连接建立时注册
- `Unregister(sid)` ([router.go:84](file:///d:/claude/ongrid/internal/manager/biz/webshell/router.go#L84))：连接关闭时注销
- `DispatchOutput(sid, data)` ([router.go:162](file:///d:/claude/ongrid/internal/manager/biz/webshell/router.go#L162))：按 sid 找到 sink 推送
- `DispatchExit(sid, exitCode, errMsg)` ([router.go:176](file:///d:/claude/ongrid/internal/manager/biz/webshell/router.go#L176))：终止会话

注释 [router.go:55-56](file:///d:/claude/ongrid/internal/manager/biz/webshell/router.go#L55-L56) 总结：HTTP handler 在 open 时 Register、close 时 Unregister；frontierbound 注册的 tunnel handler 调 Dispatch。

---

## 10. 配置与启动装配

### 10.1 FrontierClientConfig

[internal/pkg/config/config.go:246-262](file:///d:/claude/ongrid/internal/pkg/config/config.go#L246-L262)：

```go
type FrontierClientConfig struct {
    Addr        string  // ONGRID_FRONTIER_ADDR，默认 frontier:40011
    ServiceName string  // ONGRID_FRONTIER_SERVICE_NAME，默认 ongrid-manager
    Disabled    bool    // ONGRID_FRONTIER_DISABLED，默认 false
}
```

[config.go:439-441](file:///d:/claude/ongrid/internal/pkg/config/config.go#L439-L441) env 解析：

```go
c.FrontierClient.Addr = getEnv("ONGRID_FRONTIER_ADDR", "frontier:40011")
c.FrontierClient.ServiceName = getEnv("ONGRID_FRONTIER_SERVICE_NAME", "ongrid-manager")
c.FrontierClient.Disabled = getEnvBool("ONGRID_FRONTIER_DISABLED", false)
```

### 10.2 main.go 装配链

[cmd/ongrid/main.go:1046-1108](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1046-L1108)：

```go
var fbClient *managersvcfb.Client
if cfg.FrontierClient.Disabled {
    log.Warn("frontierbound: disabled (ONGRID_FRONTIER_DISABLED=true) ...")
    fbClient = managersvcfb.NewDisabled(log.With(slog.String("comp", "frontierbound")))
} else {
    c, err := managersvcfb.New(managersvcfb.Config{
        Addr:        cfg.FrontierClient.Addr,
        ServiceName: cfg.FrontierClient.ServiceName,
    }, log.With(slog.String("comp", "frontierbound")))
    if err != nil {
        log.Error("frontierbound: new client", ...)
        os.Exit(1)
    }
    fbClient = c
}
defer func() {
    if err := fbClient.Close(); err != nil { ... }
}()

// Back-fill edge service's tunnel dispatcher
edgeSvc.SetEdgeCaller(fbClient)

// Prom 写入器：typed-nil 陷阱——必须显式 nil
var promWiring managersvcfb.PromwriteIngester
if promwriteIngester != nil { promWiring = promwriteIngester }

// WebSSH router 构造（在 Install 之前，让 shell_* handler 能注册）
webshellRouter := managerwebshellbiz.NewRouter()
webshellAuditRepo := managerwebshelldata.NewRepo(db)

if err := managersvcfb.Install(rootCtx, fbClient, managersvcfb.Wiring{
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
}); err != nil {
    log.Error("frontierbound: install handlers", ...)
    os.Exit(1)
}

// Back-fill plugin config UC 的 notifier 和 secret writer
pluginConfigUC.SetNotifier(fbClient)
pluginConfigUC.SetDatabaseMetricsSecretWriter(fbClient)

// WebSSH HTTP handler
webshellHandler := managerwebshellserver.NewHandler(
    webshellStreamerAdapter{c: fbClient},  // Streamer 接口适配
    webshellRouter,
    webshellAuditAdapter{repo: webshellAuditRepo},
    deviceRepo, edgeRepo,
    log.With(slog.String("comp", "webshell")),
)
```

**装配顺序**关键：
1. `fbClient` 先建（disabled 或 real）
2. `edgeSvc.SetEdgeCaller(fbClient)`：让 edge 业务能反向调 edge
3. `webshellRouter` 在 Install 前建，让 shell_* handler 能注册
4. `Install(Wiring)` 注册全部反向 RPC + 生命周期回调
5. `pluginConfigUC.SetNotifier(fbClient)`：让 plugin config 变更能推到 edge
6. `webshellHandler` 在 Install 后建，用 `webshellStreamerAdapter{c: fbClient}` 包装 fbClient 作为 Streamer

### 10.3 typed-nil 陷阱

[main.go:1071-1077](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1071-L1077) 注释：

```go
// promIngester for the Wiring is typed as the interface; passing a
// typed-nil *Ingester would be a non-nil interface, so explicitly hand
// the handler a true nil when Prom is disabled.
var promWiring managersvcfb.PromwriteIngester
if promwriteIngester != nil {
    promWiring = promwriteIngester
}
```

Go 接口 typed-nil 陷阱：`var p *Ingester = nil; var i PromwriteIngester = p; i != nil` 为 true。必须显式 nil 接口，否则 `w.PromIngester == nil` 检查不成立。

---

## 11. systemhealth：Frontier 探测

[internal/manager/service/systemhealth/service.go:265-273](file:///d:/claude/ongrid/internal/manager/service/systemhealth/service.go#L265-L273)：

```go
func (s *Service) checkFrontier(ctx context.Context) Check {
    return s.probe(ctx, "frontier", "core", "Frontier", func(context.Context) (Status, string, map[string]any) {
        details := map[string]any{"addr": s.cfg.FrontierAddr}
        if s.cfg.FrontierDisabled {
            return StatusDegraded, "frontier client is disabled", details
        }
        return StatusOK, "frontier client is enabled; edge online state is checked separately", details
    })
}
```

注意这个 check 的局限——它只检查"是否 disabled"，不实际探测 frontier 连通性。原因：frontier 连接是 manager 内部状态，外部健康检查无法直接观测。edge online 状态由独立的 `checkEdges` 检查（基于 DB 中 `last_seen_at`）。

[main.go:1713-1738](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1713-L1738) systemhealth 装配：

```go
systemHealthSvc := managersvcsystemhealth.New(managersvcsystemhealth.Config{
    FrontierAddr:     cfg.FrontierClient.Addr,
    FrontierDisabled: cfg.FrontierClient.Disabled,
    // ...
}, ...)
```

---

## 12. aiops tools：Caller 接口

[internal/manager/biz/aiops/tools/registry.go:30-36](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/registry.go#L30-L36)：

```go
// Caller is the narrow seam this package needs from the frontierbound SDK
// wrapper. Declaring it locally lets tests inject a fake without standing
// up a real Client. Any frontierbound.Client value satisfies it via its
// own Call method.
type Caller interface {
    Call(ctx context.Context, edgeID uint64, method string, body []byte) ([]byte, error)
}
```

aiops 工具（`get_host_load` / `get_process_list` / `execute_skill` / `restart_service` 等）通过这个窄接口反向调 edge——`*frontierbound.Client` 结构化满足。注释 [registry.go:10-14](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/registry.go#L10-L14) 强调：

> Reverse calls travel through a Caller — concretely the frontierbound.Client wrapping github.com/singchia/frontier — so this package stays free of any geminio / SDK-level types.

aiops 包零 geminio 依赖，测试可注入 fake Caller。

---

## 13. 部署面：docker-compose + frontier.yaml + Dockerfile

### 13.1 docker-compose frontier 服务

[deploy/docker-compose.yml:210-222](file:///d:/claude/ongrid/deploy/docker-compose.yml#L210-L222)：

```yaml
  frontier:
    image: docker.cnb.cool/ongridio/ongrid/frontier:v1.2.4
    container_name: ongrid-frontier
    restart: unless-stopped
    volumes:
      - ./install/frontier.yaml:/usr/conf/frontier.yaml:ro
    ports:
      - "40012:40012"   # edgebound 暴露给 host，让外部 edge 能拨入
    networks:
      - ongrid_net
```

注意：**只暴露 40012（edgebound）**，40011（servicebound）只在 docker 网络内可达——manager 通过 `frontier:40011` 拨入，外部无法直连 servicebound（安全设计）。

manager 容器的 env（[docker-compose.yml:58-60](file:///d:/claude/ongrid/deploy/docker-compose.yml#L58-L60)）：

```yaml
ONGRID_FRONTIER_ADDR: ${ONGRID_FRONTIER_ADDR:-frontier:40011}
```

### 13.2 frontier.yaml：broker 配置

[deploy/install/frontier.yaml](file:///d:/claude/ongrid/deploy/install/frontier.yaml)：

```yaml
edgebound:
  listen:
    network: tcp
    addr: 0.0.0.0:40012
  # Ongrid's manager registers the GetEdgeID authentication RPC. Fail closed
  # while that RPC is unavailable (for example during a restart), otherwise
  # Frontier assigns an unauthenticated temporary ID that remains attached to
  # the live edge connection after the manager recovers.
  edgeid_alloc_when_no_idservice_on: false

servicebound:
  listen:
    network: tcp
    addr: 0.0.0.0:40011
```

`edgeid_alloc_when_no_idservice_on: false` 是关键安全开关——manager 重启期间 GetEdgeID 不可用时，frontier **拒绝** edge dial（而不是分配临时 ID）。注释解释：临时 ID 会附着在 live edge 连接上，manager 恢复后也无法把它们重新映射到 canonical edge_id。

### 13.3 Dockerfile.frontier：非 root 镜像

[deploy/Dockerfile.frontier](file:///d:/claude/ongrid/deploy/Dockerfile.frontier)：

```dockerfile
FROM golang:1.24-alpine AS builder
RUN apk add --no-cache build-base make
ENV GO111MODULE=on \
    GOPROXY=https://goproxy.cn,https://goproxy.io,https://proxy.golang.org,direct \
    CGO_CFLAGS="-D_LARGEFILE64_SOURCE"
WORKDIR /go/src/github.com/singchia/frontier
COPY . .
RUN make DESTDIR=/tmp/install all install-frontier

FROM alpine:3.20
RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 65532 frontier \
    && adduser -S -D -H -u 65532 -G frontier frontier
COPY --from=builder /tmp/install/ /
USER 65532:65532
EXPOSE 40011
EXPOSE 40012
ENTRYPOINT ["/usr/bin/frontier"]
CMD ["--config", "/usr/conf/frontier.yaml"]
```

符合 AGENTS.md 安全红线"容器以非 root 用户运行"——UID/GID 65532。

---

## 14. 并发、错误与可观测性

### 14.1 并发模型

| 路径 | 并发模型 |
|------|----------|
| edge tunnel.Client | 单 goroutine + RetryEnd 内部重连；Call 是同步阻塞 |
| edge Agent.Run | errgroup 启动 heartbeatLoop / metricsLoop / upgrade-watch 三 goroutine |
| manager frontierbound.Client | 单 service-end 连接，多 RPC 并发调用（frontier 内部多路复用） |
| manager Install | 启动时单线程注册，运行时由 frontier 触发回调 |
| mapping 操作 | `sync.RWMutex` 保护双向 map |
| reconnectCallbacks | `reconnectRunMu` 串行化多次重连的回调 |

### 14.2 错误传播

```
edge → cloud RPC 失败
   │
   ▼ tunnel.Client.Call
   ├─ 网络错误 → fmt.Errorf("tunnel call %q: %w", method, err)
   ├─ remote error → fmt.Errorf("tunnel call %q: remote: %w", method, rerr)
   └─ shouldRecycleBrokenRoute → recycleBrokenRoute 关闭连接 → RetryEnd 重连

cloud → edge RPC 失败
   │
   ▼ frontierbound.Client.Call
   ├─ svc.Call 错误 → fmt.Errorf("frontierbound: call %q edge=%d transport=%d: %w", ...)
   ├─ rsp.Error → fmt.Errorf("frontierbound: remote %q edge=%d transport=%d: %w", ...)
   └─ ErrDisabled（svc=nil）

edge 反向 RPC（register_edge / heartbeat / push_*）
   │
   ▼ frontierbound handler
   ├─ JSON 解码错误 → fmt.Errorf("...: decode: %w", err)
   ├─ 业务错误 → fmt.Errorf("...: %w", err) 返给 edge
   └─ silent drop（push_* 时 canonical=0 或 device_id=0）→ 返 Accepted=0/ Accepted=n
```

### 14.3 日志

所有 frontier 路径用 `slog.With(slog.String("comp", "frontierbound"))` 或 `"comp": "tunnel"` 标记：

- `Info` "frontierbound: connected"（New 成功）
- `Info` "frontierbound: GetEdgeID: authn ok"
- `Info` "frontierbound: edge online" / "edge offline"
- `Info` "frontierbound: handlers installed"
- `Warn` "frontierbound: edge authn failed"
- `Warn` "frontierbound: push_host_metrics dropped — device_id unresolved"
- `Warn` "frontierbound: stale or unknown edge offline ignored"（Debug 级）
- `Warn` "tunnel: dial failed; will retry"
- `Warn` "tunnel: frontier route is stale; recycling transport"

### 14.4 可观测性盲点

- **无 metrics**：frontierbound 未暴露 Prometheus 指标（RPC 次数 / 失败次数 / 重连次数）。运营方靠日志和 systemhealth 二态判定。
- **无 trace_id 透传**：geminio wire 不携带 trace_id，edge → manager 的 RPC 无法与 manager HTTP 入口 trace 关联。
- **edge online 状态靠 DB**：`checkFrontier` 只看 disabled flag，真实 edge online 数靠 `checkEdges`（基于 `edges.last_seen_at`）。

---

## 15. 架构红线与设计要点

### 15.1 红线

1. **凭据走 Meta，不走 body**：[handlers.go:236-238](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L236-L238) 注释明确"Do not trust the legacy credentials in this request body"——鉴权只走 `GetEdgeID` 回调里的 Meta blob。
2. **edgeid_alloc_when_no_idservice_on: false**：[frontier.yaml:23](file:///d:/claude/ongrid/deploy/install/frontier.yaml#L23) 是安全开关——manager 不可用时 frontier 拒绝 edge dial，绝不分配匿名 ID。
3. **canonicalizeEdgeID 不 fallback**：[client.go:356-361](file:///d:/claude/ongrid/internal/manager/service/frontierbound/client.go#L356-L361) 返回 0 让 caller drop，绝不把 transport ID 当 edge_id 写进 Prom label。
4. **resolveDeviceID 不 fallback 到 edge_id**：[handlers.go:651-669](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L651-L669) issue #96 修复——edge_id 和 device_id 是独立自增序列，fallback 会污染 TSDB。
5. **unbindEdgeTransport 三重校验**：[client.go:293-323](file:///d:/claude/ongrid/internal/manager/service/frontierbound/client.go#L293-L323) 防 stale offline 事件删错新连接。
6. **recycleBrokenRoute 代际校验**：[client.go:326-346](file:///d:/claude/ongrid/internal/pkg/tunnel/client.go#L326-L346) 只关闭"调用时活跃代际"的连接，防误关新连接。
7. **RegisterHandler 在 Dial 前调用**：[edgeagent/biz/agent.go:155](file:///d:/claude/ongrid/internal/edgeagent/biz/agent.go#L155) 避免首批 RPC 错过。
8. **OnReconnect 异步触发**：[client.go:74-79](file:///d:/claude/ongrid/internal/pkg/tunnel/client.go#L74-L79) RetryEnd 持锁状态下不能直接调 RPC，必须异步。
9. **OnReconnect panic recover**：[client.go:388-397](file:///d:/claude/ongrid/internal/pkg/tunnel/client.go#L388-L397) 单个回调 panic 不能卡死整个重连流程。
10. **typed-nil 显式处理**：[main.go:1071-1077](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1071-L1077) Prom ingester 必须显式 nil 接口。
11. **shell_open SSHPass 立即 wipe**：[webshell/http.go:283](file:///d:/claude/ongrid/internal/manager/server/webshell/http.go#L283) `openFrame.SSHPass = ""` 用完即抹。
12. **非 root 容器**：[Dockerfile.frontier](file:///d:/claude/ongrid/deploy/Dockerfile.frontier) UID/GID 65532。
13. **servicebound 不暴露 host**：[docker-compose.yml:210-222](file:///d:/claude/ongrid/deploy/docker-compose.yml#L210-L222) 只 ports 40012，40011 仅网络内可达。
14. **recycleBrokenRoute 仅 register_edge/heartbeat 触发**：[client.go:348-361](file:///d:/claude/ongrid/internal/pkg/tunnel/client.go#L348-L361) 避免业务 RPC 失败误触发连接回收。
15. **push_* silent drop 而非 error**：canonical=0 / device_id=0 / Prom 禁用时返 Accepted=0/n，让 edge 不重试，避免 churn。

### 15.2 设计要点

- **三方解耦**：edge ↔ frontier ↔ manager。manager 重启不切断 edge 连接，靠 frontier 保持会话 + register_edge 重建 binding。
- **wire 协议手镜像 proto**：[messages.go:5-13](file:///d:/claude/ongrid/internal/pkg/tunnel/messages.go#L5-L13) `internal/pkg/tunnel` 零依赖，Phase 2 切 protobuf 只改这一个文件。
- **接口在消费方定义**：`Caller`（aiops tools）、`Streamer`（webshell）、`GrafanaTester`（systemhealth）等窄接口在消费方定义，`*frontierbound.Client` 结构化满足。
- **代际机制**：[client.go:56-63](file:///d:/claude/ongrid/internal/pkg/tunnel/client.go#L56-L63) active/pending 连接分离 + `promotePendingConnection` 原子切换，避免重连中误关新连接。
- **RetryEnd 自愈**：首次 Dial 成功后，断线重连由 geminio 内部处理；我们的 `retryDelegate.EndReOnline` 只是回调钩子。
- **register_edge 双重作用**：首次注册 + 重连后重建 binding。manager 重启 / frontier 路由失效后都靠它恢复。
- **stale 事件三重校验**：transport ID / canonical edge_id / addr 三个维度校验 offline 事件，处理 frontier 异步事件竞态。
- **fire-and-forget 推送**：`NotifyPluginConfigsChanged` 不带 body，edge 60s 安全网轮询兜底——避免 push/pull wire 格式耦合。
- **WebSSH 三阶段**：OpenStream（manager 主动）+ SSH over tunnel（manager 终结 SSH 协议）+ shell_output/exit push（edge 推回）。
- **edge 是哑字节转发器**：SSH 协议在 manager 侧终结，edge 只 io.Copy 到本地 sshd——tunnel 层保持通用。
- **piggyback plugin health**：[agent.go:430-436](file:///d:/claude/ongrid/internal/edgeagent/biz/agent.go#L430-L436) 心跳携带 plugin 健康，避免单独 RPC。
- **k8s controller / node 双路径**：[handlers.go:244-283](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers.go#L244-L283) controller 不创建 host device，node 仍然创建。
- **errgroup + 哨兵 error**：[agent.go:211-219](file:///d:/claude/ongrid/internal/edgeagent/biz/agent.go#L211-L219) upgradeRequested 触发哨兵 error 让 errgroup 取消其他 goroutine，Run 返回前过滤回 nil 让 systemd 干净重启。

---

## 16. 附录：测试覆盖

### 16.1 internal/pkg/tunnel/client_test.go

[client_test.go:17-50](file:///d:/claude/ongrid/internal/pkg/tunnel/client_test.go#L17-L50) `TestRetryDelegateFiresCallbacksAfterReconnect`：验证 `EndReOnline` 触发 OnReconnect 回调的顺序。

### 16.2 internal/manager/service/frontierbound/client_test.go

[client_test.go](file:///d:/claude/ongrid/internal/manager/service/frontierbound/client_test.go) 覆盖：
- `bindEdgeTransport` / `unbindEdgeTransport` 双向映射
- 三重校验 stale 事件过滤
- `canonicalizeEdgeID` / `resolveTransportID` 转换
- K8s controller state 标记

### 16.3 internal/manager/service/frontierbound/handlers_test.go

[handlers_test.go](file:///d:/claude/ongrid/internal/manager/service/frontierbound/handlers_test.go) 覆盖：
- register_edge 主机 / K8s controller / K8s node 三路径
- heartbeat 校验（mismatch / not ready）
- push_host_metrics / push_prom_samples 的 device_id 解析 + drop 路径
- shell_output / shell_exit 路由

### 16.4 未覆盖点（盲区）

- **Dial 重试循环**：靠 e2e
- **RetryEnd 实际重连**：靠 upstream geminio 测试
- **recycleBrokenRoute 代际校验**：靠 e2e
- **OpenStream + SSH**：靠 e2e
- **frontier broker 本身**：upstream 项目测试

---

## 文档版本

- 撰写日期：2026-07-31
- 仓库快照：撰写时 working tree
- 覆盖代码：internal/pkg/tunnel + internal/manager/service/frontierbound + internal/edgeagent/biz + internal/manager/server/webshell + internal/manager/biz/{webshell,promwrite,edge,aiops/tools} + internal/manager/service/systemhealth + cmd/{ongrid,ongrid-edge} + deploy/{docker-compose,install/frontier.yaml,Dockerfile.frontier}
- 关联文档：[ongrid_rpc_singchia_geminio.md](file:///d:/claude/ongrid/ongrid_rpc_singchia_geminio.md)（更深入的 RPC 协议层分析）、[ongrid_integration.md](file:///d:/claude/ongrid/ongrid_integration.md)（外部系统集成总览）
