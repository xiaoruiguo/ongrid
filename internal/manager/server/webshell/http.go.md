# webshell/http.go 技术实现文档

## 1. 概述

`http.go` 是 ongrid manager WebSSH 子域的 HTTP/WebSocket 路由层。Manager 通过 frontier stream 连接到 edge（Meta = `{"target":"127.0.0.1:22"}`），用 `golang.org/x/crypto/ssh.NewClientConn` 包装 stream，运行 PTY + Shell，通过 WebSocket 与浏览器 pump stdin/stdout。提供 3 个端点：openShell（WS 升级 + 会话建立）/ listSessions（活跃 + 历史）/ killSession（admin 强制终止）。

**关键架构**：Edge agent 是 dumb byte forwarder（见 `internal/edgeagent/webshell`）；SSH 协议、PTY、session lifecycle 全在 manager 端实现。

## 2. 包信息

- **包名**：`webshell`
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/server/webshell`
- **路由**：
  - `GET /v1/devices/{device_id}/shell` —— WebSocket 升级 + 会话（casbin `device:shell:exec`）
  - `GET /v1/webshell/sessions` —— 列活跃 + 最近 50 历史（casbin `device:shell:read`）
  - `DELETE /v1/webshell/sessions/{id}` —— admin 强制终止（casbin `device:shell:manage`）
- **文件定位**：HTTP/WS handler + SSH 客户端 + bridge（WS ↔ SSH pump）

## 3. 关键类型与接口

### 窄接口

```go
type AuthzMW interface {
    Require(obj, act string) func(http.Handler) http.Handler
}

type Streamer interface {
    OpenStream(ctx context.Context, edgeID uint64) (io.ReadWriteCloser, error)
}

type DeviceRepo = devicebiz.Repo  // 类型别名
```

`Streamer` 由 `*managersvcfb.Client` 满足；`OpenStream` 建立 frontier 流到 edge。

### Handler

```go
type Handler struct {
    streamer Streamer               // frontier 流客户端
    router   *bizwebshell.Router    // 活跃会话路由
    audit    bizwebshell.Recorder   // 审计记录器
    devices  DeviceRepo             // 设备仓库
    edges    edgebiz.Repo           // edge 仓库
    authz    AuthzMW                // 可选 casbin
    log      *slog.Logger
    upgrader websocket.Upgrader
}
```

### 常量

```go
const MaxSessionsPerUser = 5        // 每用户最大并发 shell
const MaxSessionsPerDevice = 5      // 每设备最大并发 shell
const IdleTimeout = 15 * time.Minute // 空闲超时自动关闭
```

### 消息 DTO

```go
type openMsg struct {
    Type    string `json:"type"`              // "open"
    Cols    uint16 `json:"cols"`
    Rows    uint16 `json:"rows"`
    Term    string `json:"term,omitempty"`
    SSHHost string `json:"ssh_host,omitempty"` // 预留 jumpbox，当前忽略
    SSHUser string `json:"ssh_user"`
    SSHPass string `json:"ssh_pass"`
}

type ctlMsg struct {
    Type string `json:"type"` // "resize" | "close"
    Cols uint16 `json:"cols,omitempty"`
    Rows uint16 `json:"rows,omitempty"`
}
```

### bridge —— WS ↔ SSH 桥接

```go
type bridge struct {
    conn *websocket.Conn
    log  *slog.Logger
    wmu  sync.Mutex           // gorilla 要求单并发 writer
    stdin    uint64            // browser → edge 字节计数
    exitOnce sync.Once
    exit     chan struct{}
    exitC    int32             // ssh exit code
    killHook func(reason string)
}
```

### rwcAdapter —— io.ReadWriteCloser → net.Conn

```go
type rwcAdapter struct {
    rwc io.ReadWriteCloser
}
```

把 frontier stream 适配为 `net.Conn` 供 SSH 客户端使用；addr/deadline 方法 stub（frontier stream 不暴露）。

## 4. 关键函数与流程

### Register —— 路由表（含 casbin）

```go
func (h *Handler) Register(r chi.Router)
```

- `/v1/devices/{device_id}/shell` —— `h.authz.Require("device:shell", "exec")`（nil 时 passthrough）
- `/v1/webshell/sessions` —— `h.authz.Require("device:shell", "read")`
- `/v1/webshell/sessions/{id}` DELETE —— `h.authz.Require("device:shell", "manage")`

### openShell —— WS 升级 + SSH 会话建立

```go
func (h *Handler) openShell(w http.ResponseWriter, r *http.Request)
```

1. `tenantctx.From` 取 tenant
2. `ParseUint(device_id)`
3. **找在线 edge**：`h.edges.List(Limit:1000)` 遍历找 `DeviceID == deviceID && Status == Online`，否则 503"device offline"
4. **并发限制**：
   - `h.router.CountByUser(tenant.UserID) >= MaxSessionsPerUser` → 429
   - `h.router.CountByDevice(deviceID) >= MaxSessionsPerDevice` → 429
5. **WS 升级**：`h.upgrader.Upgrade(w, r, nil)`
6. **读 open frame**：10s 读 deadline，必须 TextMessage + `Type == "open"` + `SSHUser/SSHPass` 非空
7. 默认 `cols=80, rows=24, term=xterm-256color`
8. **审计 Open**：`h.audit.Open(ctx, Session{ID: uuid, ...})`
9. **router.Register**：`h.router.Register(sid, br, ActiveSession{...})` + `defer h.router.Unregister(sid)`
10. **OpenStream**：10s timeout 调 `h.streamer.OpenStream(ctx, edge.ID)`，失败 close audit + 关 WS
11. **SSH 客户端**：
    - `ssh.ClientConfig{User, Auth: Password, HostKeyCallback: InsecureIgnoreHostKey, Timeout: 10s}`
    - **`openFrame.SSHPass = ""` wipe asap**
    - `ssh.NewClientConn(rwcAdapter{stream}, "127.0.0.1:22", sshCfg)`
    - 失败时映射"unable to authenticate" → "用户名或密码错误"
12. **SSH Session + PTY**：`sshClient.NewSession()` + `RequestPty(term, rows, cols, ...)` + `Shell()`
13. **`br.sendText({type: "ready"})`** 通知浏览器
14. **killHook**：`br.killHook = func(reason) { pumpDone <- terminationCause(reason) }`
15. **启动 4 个 goroutine**：
    - `pumpReaderToBridge(br, stdout, router, sid)` —— stdout → WS binary
    - `pumpReaderToBridge(br, stderr, router, sid)` —— stderr → WS binary
    - `h.pumpBrowserToSSH(ctx, sid, br, stdin, sess, pumpDone)` —— WS → stdin + resize/close
    - `waitSSH(sess, pumpDone)` —— sess.Wait → exit code
    - `h.idleWatchdog(ctx, sid, br, pumpDone)` —— 15min 空超时
16. **`cause := <-pumpDone`** 等待终止
17. **清理**：`sess.Close()` + `sshClient.Close()` + `stream.Close()`
18. **closeAudit**：`h.audit.Close(ctx, sid, endedAt, stdinBytes, stdoutBytes, exitCode, terminatedBy)`
19. **`br.closeWith(NormalClosure, "")`**

### pumpReaderToBridge —— stdout/stderr → WS

```go
func pumpReaderToBridge(br *bridge, r io.Reader, router *bizwebshell.Router, sid string)
```

8KB buffer 循环 Read → `router.AddStdoutBytes(sid, n)` + `br.writeBinary(buf[:n])`。

### pumpBrowserToSSH —— WS → stdin + control

```go
func (h *Handler) pumpBrowserToSSH(parent context.Context, sid string, br *bridge, stdin io.WriteCloser, sess *ssh.Session, done chan<- terminationCause)
```

按 WS 消息类型分发：
- **BinaryMessage** → `stdin.Write(data)` + `br.addStdin(n)` + `router.TouchInput(sid)`
- **TextMessage** → 解析 `ctlMsg`：
  - `resize` → `sess.WindowChange(rows, cols)` + `router.TouchInput(sid)`
  - `close` → `done <- TerminatedByUser`
- **CloseMessage** → `done <- TerminatedByUser`

### idleWatchdog —— 15min 空超时

```go
func (h *Handler) idleWatchdog(parent context.Context, sid string, br *bridge, done chan<- terminationCause)
```

60s tick 检查 `router.Active()` 中该 session 的 `LastInputAt`，超 15min 则 `done <- TerminatedByIdle`。

### waitSSH —— SSH 退出码

```go
func waitSSH(sess *ssh.Session, done chan<- terminationCause)
```

`sess.Wait()` 阻塞，退出时 `done <- TerminatedBySSHExit`（exit code 由 router 通过 Audit 捕获）。

### listSessions —— 活跃 + 历史

```go
func (h *Handler) listSessions(w http.ResponseWriter, r *http.Request)
```

`h.audit.List(ctx, 50)` 取最近 50 历史 + `h.router.Active()` 取活跃，合并去重（活跃 ID 不在历史中重复），返 `{items, total}`。

### killSession —— admin 强制终止

```go
func (h *Handler) killSession(w http.ResponseWriter, r *http.Request)
```

`h.router.Kill(id, TerminatedByAdminKill)` —— 调 bridge 的 killHook 触发 pumpDone，返 204；session 不存在返 404。

### bridge 方法

- `read()` —— `conn.ReadMessage()`
- `addStdin(n)` / `stdinBytes()` —— atomic 计数
- `sendText(payload)` —— JSON marshal + WriteMessage（wmu 加锁）
- `writeBinary(data)` —— WriteMessage BinaryMessage（wmu 加锁）
- `OnOutput(data)` / `OnExit(exitCode, errMsg)` —— Sink 接口实现（router 兼容）
- `Kill(reason)` —— 调 killHook
- `closeWith(code, reason)` —— WriteControl CloseMessage + Close

## 5. 依赖关系

**外部**：
- `chi` —— URL 参数
- `gorilla/websocket` —— WS 协议
- `golang.org/x/crypto/ssh` —— SSH 客户端
- `google/uuid` —— session ID
- `net/http`、`net`、`io`、`sync`、`sync/atomic`、`log/slog`、`time`、`strconv`、`strings`、`encoding/json`、`fmt`、`errors`、`context`

**内部**：
- `internal/manager/biz/webshell`（Router + Recorder + ActiveSession）
- `internal/manager/biz/device`（Repo 别名）
- `internal/manager/biz/edge`（Repo + ListFilter）
- `internal/manager/model/edge`（Edge + StatusOnline）
- `internal/manager/model/webshell`（Session + TerminatedBy* 常量）
- `internal/pkg/errs`
- `internal/pkg/tenantctx`

## 6. 并发与资源管理

- **多 goroutine pump 模型**：4-5 个 goroutine 协作（stdout/stderr/browser/waitSSH/idleWatchdog），通过 `pumpDone chan` 同步终止
- **`pumpDone` buffered channel cap=4**：避免发送方阻塞
- **`br.wmu sync.Mutex`**：gorilla websocket 要求单并发 writer，所有 WriteMessage 走 wmu
- **`br.exitOnce sync.Once`**：OnExit 只触发一次
- **`atomic.AddUint64` / `atomic.LoadUint64`**：stdin 字节计数无锁
- **`defer h.router.Unregister(sid)`**：保证会话注销
- **`defer sshClient.Close()` / `defer sess.Close()`**：保证 SSH 资源释放
- **`openFrame.SSHPass = ""` wipe**：密码用完立即清空，减少内存驻留
- **`IdleTimeout = 15min`**：空超时自动关闭，防泄漏 session
- **`MaxSessionsPerUser/Device = 5`**：并发上限，防资源耗尽
- **10s open frame deadline**：防恶意客户端升级后不发 open frame 占连接

## 7. 设计模式与亮点

1. **Manager 端 SSH 客户端**：edge 是 dumb byte forwarder，SSH 协议/PTY/session 全在 manager——简化 edge agent，复杂逻辑集中
2. **`rwcAdapter` 适配 frontier stream → net.Conn**：让 `ssh.NewClientConn` 能直接用 frontier stream，无需本地 TCP
3. **`InsecureIgnoreHostKey`**：localhost-only 路径，SSH 连接的是 edge 内部 127.0.0.1:22，无需 host key 验证
4. **多 goroutine pump + channel 同步**：stdout/stderr/browser/waitSSH/idleWatchdog 各一 goroutine，`pumpDone` 集中终止信号
5. **`killHook` 桥接 router Kill → pumpDone**：admin Kill 信号通过 killHook 注入 pumpDone，触发统一清理流程
6. **`SSHPass` wipe asap**：密码用完立即清空，减少内存驻留时间
7. **错误消息中文化**：`unable to authenticate` → `用户名或密码错误`，提升用户体验
8. **并发限制双维度**：per-user + per-device，防单用户耗尽 + 防单设备被压垮
10. **`IdleTimeout` 15min**：空超时自动关闭，防泄漏
9. **10s open frame deadline**：防恶意客户端升级后不发 open frame 占连接
10. **审计覆盖全生命周期**：Open + Close（含 stdin/stdout 字节、exit code、terminatedBy）
11. **`listSessions` 活跃+历史合并去重**：活跃 session 不在历史中重复出现
12. **`casbin` 三级权限**：exec（开 shell）/ read（列表）/ manage（kill），细粒度 RBAC
13. **`passthrough` 兜底**：casbin 未注入时降级为 passthrough（已登录即可），便于灰度
14. **WS Subprotocol `ongrid.shell.v1`**：协议版本协商，便于未来升级
15. **`CheckOrigin: true`**：开发环境方便；生产应由 nginx 校验 Origin

## 8. 注意事项

1. **`InsecureIgnoreHostKey`**：仅因 localhost-only 路径安全；如改为 jumpbox 模式需启用 host key 验证
2. **`CheckOrigin: true`**：接受任意 Origin，生产环境应由 nginx 校验或改 `CheckOrigin` 实现
3. **`SSHPass` 明文传 WS**：浏览器→manager WS 帧，依赖 WSS 加密；manager 内存中 wipe asap
4. **`MaxSessionsPerUser/Device = 5`**：硬编码，如需调整需改常量
5. **`IdleTimeout = 15min`**：硬编码；运维长时间 idle 会被踢，需重新连接
6. **`edges.List(Limit:1000)` 找在线 edge**：设备多时可能漏（超过 1000）；应改按 device_id 索引查询
7. **`streamMeta{Target: "127.0.0.1:22"}` 仅文档**：当前 frontier 不支持设 Meta，edge 默认 127.0.0.1:22；Phase 2 jumpbox 支持需 thread Meta
8. **`pumpDone` cap=4**：4 个 goroutine 可能同时发送，cap=4 避免阻塞；但 select+default 仍兜底
9. **`waitSSH` exit code 未直接用**：`_ = code`，exit code 由 router 通过 Audit 捕获；本函数仅触发终止信号
10. **`closeAudit` 用 `context.Background()`**：不继承请求 ctx（请求可能已取消），确保审计写入完成
11. **无审计中间件**：本文件未用 `auditmw.SetAuditEvent`，而是直接调 `h.audit.Open/Close`——webshell 有自己的审计记录器
12. **`killSession` 返 204**：kill 信号异步触发，不等待 session 实际关闭
13. **`listSessions` 限制 50 历史**：硬编码，如需分页需扩接口
14. **`bridge.OnOutput/OnExit` Sink 接口**：新路径不调用，但保留以满足 `Router.Register` 的 Sink 契约
15. **`noopAddr` Network/String = "tunnel"**：rwcAdapter 的 addr 是 stub，仅满足 net.Conn 接口
16. **`clientIP` 取 XFF 第一跳**：与 audit 中间件同模式，信任 nginx 剥离
