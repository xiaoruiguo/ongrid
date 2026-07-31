# router.go 技术实现文档

## 1. 概述

`router.go` 是 `webshell` 包的核心——manager 侧 WebSSH 的会话路由器。它维护 `SessionID → live WebSocket sink` 的目录，让 edge→manager 的 Output/Exit 推送能找到正确的浏览器；同时暴露窄 `Recorder` 接口供 HTTP 层写入审计行。HTTP/WebSocket handler 位于隔壁 `internal/manager/server/webshell`，本包保持 HTTP-agnostic 以便用 fake 单元测试。

## 2. 包信息

- 包名：`webshell`
- 路径：`internal/manager/biz/webshell/router.go`
- 导入依赖：
  - 标准库：`context` / `sync` / `sync/atomic` / `time`
  - 内部包：`github.com/ongridio/ongrid/internal/manager/model/webshell`（别名 `wsmodel`）

## 3. 关键类型与接口

### `Caller`

```go
type Caller interface {
    Call(ctx context.Context, edgeID uint64, method string, body []byte) ([]byte, error)
}
```

调用 edge agent RPC 的窄 tunnel surface，与 aiops tools 使用的 shape 一致。

### `Sink`

```go
type Sink interface {
    OnOutput(data []byte) error
    OnExit(exitCode int, errMsg string)
}
```

每个 live session 一个 Sink。manager 侧 Output/Exit handler 通过它把字节推到 WebSocket，并在 Exit 时信号关闭。

### `Killer`

```go
type Killer interface {
    Kill(reason string)
}
```

`Sink` 可选实现的接口。当 bridge 支持 admin-kill 时，manager 侧 handler 在注册 sink 时一并安装 closer。

### `ActiveSession`

```go
type ActiveSession struct {
    SessionID    string
    OngridUserID uint64
    SSHUser      string
    DeviceID     uint64
    EdgeID       uint64
    StartedAt    time.Time
    LastInputAt  time.Time // 每次浏览器→edge 帧更新
}
```

live-session 元数据，供 `/v1/webshell` 列表与 admin kill 使用。镜像审计行中感兴趣的字段，无需每次请求查 DB。

### `Router`

```go
type Router struct {
    mu          sync.RWMutex
    sinks       map[string]Sink
    meta        map[string]*ActiveSession // SessionID → metadata
    stdoutBytes sync.Map                  // SessionID → *uint64
}
```

SessionID → Sink 的目录。`sinks` 与 `meta` 用 RWMutex 保护；`stdoutBytes` 用 `sync.Map` + `*uint64` + `atomic` 实现高并发计数。

### `Recorder`

```go
type Recorder interface {
    Open(ctx context.Context, s *wsmodel.Session) error
    Close(ctx context.Context, sessionID string, endedAt time.Time, bytesIn, bytesOut uint64, exitCode int, terminatedBy string) error
    List(ctx context.Context, limit int) ([]*wsmodel.Session, error)
}
```

窄审计 surface。`*data/webshell/store.Repo` 通过 `cmd/ongrid` wiring 中的小 adapter 满足此接口。

## 4. 关键函数与流程

### `NewRouter`

```go
func NewRouter() *Router {
    return &Router{
        sinks: make(map[string]Sink),
        meta:  make(map[string]*ActiveSession),
    }
}
```

### `Register(sid, s, m)`

注册 sink + 记录 active-session 元数据 + 初始化 stdoutBytes 计数器为 `*uint64(0)`。

### `Unregister(sid)`

删除 sink / meta / stdoutBytes 条目。幂等。

### `TouchInput(sid)`

标记 session 刚收到浏览器输入帧。idle-timeout watchdog 读 `LastInputAt` 决定何时驱逐。

### `Active()`

返回当前 live session 的快照（深拷贝 `ActiveSession` 值）。供列表端点与 per-user 并发限制器使用。

### `CountByUser(userID)` / `CountByDevice(deviceID)`

遍历 `meta` 计数。用于并发限制（如"每用户最多 3 个 session"）。

### `Kill(sid, reason)`

按 SessionID 终止 session：

1. RLock 查 sink
2. 类型断言为 `Killer`
3. 调 `k.Kill(reason)`
4. 返回 `false` 当 sid 未知（已关闭）或 sink 未实现 Killer

审计行由 regular pump-done 路径关闭，Kill 本身不直接写审计。

### `DispatchOutput(sid, data)`

路由一个 stdout chunk：

1. RLock 查 sink；缺失 sid 是 no-op（race：edge 在浏览器关闭后推送）
2. `atomic.AddUint64` 累加 stdoutBytes
3. `s.OnOutput(data)`

### `DispatchExit(sid, exitCode, errMsg)`

路由终止帧。同样缺失 sid 静默 no-op。

### `AddStdoutBytes(sid, n)`

直接累加 per-session stdout 计数。new HTTP path（manager 侧 SSH client）调用此方法，因为它不走 `DispatchOutput`。

### `StdoutBytes(sid)`

返回 session 累计 stdout 字节数（或 0 当未知）。

## 5. 依赖关系

- **`wsmodel.Session`**：审计行的领域类型
- **被依赖方**：
  - `internal/manager/server/webshell` 的 HTTP/WebSocket handler（Register/Unregister/Active/Kill）
  - frontierbound 注册的 tunnel-incoming handler（DispatchOutput/DispatchExit）
  - per-user / per-device 并发限制器（CountByUser/CountByDevice）
  - idle-timeout watchdog（LastInputAt）
  - `Recorder` 的实现由 `data/webshell/store.Repo` 通过 adapter 提供

## 6. 并发与资源管理

### RWMutex + sync.Map 混合策略

- `sinks` / `meta`：`sync.RWMutex` 保护。读多写少（Dispatch 频繁，Register/Unregister 罕见），RWMutex 比单 Mutex 更优
- `stdoutBytes`：`sync.Map` + `*uint64` + `atomic.AddUint64`。这是高频写入（每个 stdout chunk 都累加），用 RWMutex 会成为瓶颈。`sync.Map` 的 load-or-store + atomic 让计数无锁

### 无 goroutine

`Router` 不启动任何 goroutine。idle-timeout watchdog、pump goroutine 等由 HTTP 层管理。Router 仅是被动目录。

### 双重删除容错

`DispatchOutput` / `DispatchExit` 对缺失 sid 静默 no-op。这是为 race 设计——edge 推送可能在浏览器关闭后到达，no-op 比报错更合理。

### `Active()` 的深拷贝

```go
out = append(out, *m)
```

返回 `ActiveSession` 值的拷贝而非指针，避免调用方持锁访问。`LastInputAt` 等字段在拷贝瞬间是快照值。

## 7. 设计模式与亮点

### HTTP-agnostic 设计

`Router` 不导入 `net/http` 或 WebSocket 库，仅依赖 `Sink` 接口。这让单元测试能用 fake Sink 验证路由逻辑，无需启动真实 WebSocket。HTTP 层负责将 WebSocket 连接包装成 `Sink` 实现。

### `stdoutBytes` 的无锁计数

`sync.Map` + `*uint64` + `atomic` 的组合是高频计数器的典型模式：

- `sync.Map` 提供 SessionID → `*uint64` 的并发安全映射
- `*uint64` 让多个 goroutine 能 `atomic.AddUint64` 同一个计数器
- 纯 `sync.Map` 不行（值是不可变的），纯 `map + Mutex` 太重

### `Killer` 可选接口

`Sink` 是基础接口，`Killer` 是可选扩展。`Kill` 用类型断言检查 sink 是否实现 `Killer`，未实现则返回 `false`。这让"不支持 admin-kill 的 bridge"能安全共存——不会 panic，只是无法被远程终止。

### `DispatchOutput` 的 race 容错

注释明确"Missing sid is no-op (race: edge pushed after browser closed)"。这是分布式系统的现实——edge 推送与浏览器关闭是并发的，Router 必须容忍"推送到达时 sink 已被 Unregister"的状态。

### `ActiveSession.LastInputAt` 的 watchdog 语义

`TouchInput` 在每次浏览器→edge 帧时更新。idle-timeout watchdog 读此字段决定驱逐。这种"被动记录、主动检查"让 watchdog 逻辑与 Router 解耦——Router 不关心 idle 策略，只提供数据。

### `AddStdoutBytes` 的双路径设计

`DispatchOutput` 自动累加 stdoutBytes，但 new HTTP path（manager 侧 SSH client）不经 DispatchOutput，故提供 `AddStdoutBytes` 直接累加。这种"为特殊路径开专用方法"比"让所有路径都走 DispatchOutput"更诚实——后者会让 DispatchOutput 的语义模糊。

### `Recorder` 接口的窄化

`Recorder` 只暴露 `Open` / `Close` / `List` 三个方法，而非完整的 `Repo` 接口。这让 adapter 实现简单，且 HTTP 层无法通过 `Recorder` 做越权操作（如直接 Delete 审计行）。

## 8. 注意事项

- **`sinks` 与 `meta` 的一致性**：`Register` / `Unregister` 同时操作两个 map，已在同一 `Lock` 下保证一致。但若未来新增"只删 sink 不删 meta"的逻辑，需谨慎
- **`stdoutBytes` 的清理**：`Unregister` 时 `sync.Map.Delete(sid)`，但 `*uint64` 本身由 GC 回收。若长期运行大量 session，需确认无指针泄漏（当前实现应安全，因 Delete 后无新引用）
- **`Kill` 的非原子性**：`RLock` 查 sink → 释放锁 → `k.Kill(reason)`。中间 sink 可能被 Unregister。`Killer.Kill` 实现需容忍"已关闭"状态
- **`Active()` 的快照语义**：返回的是调用瞬间的深拷贝，调用方迭代时 `LastInputAt` 等字段可能已过期。并发限制器若需精确计数，应接受短暂不一致
- **`CountByUser` / `CountByDevice` 的 O(n)**：遍历整个 `meta`。session 数量大时（如几千个并发）可能有性能压力——当前未索引，若成为瓶颈需加 `userID → sessionIDs` 反向索引
- **`Recorder` 接口的 `terminatedBy`**：`Close` 接受 `terminatedBy` 参数，让审计行记录终止原因（admin-kill / idle-timeout / normal-exit）。HTTP 层需正确传递此值
- **`Caller` 接口未被 Router 使用**：`Caller` 声明在本文件但 Router 未引用——它供 HTTP 层或其他 biz 层（如 aiops tools）使用，本文件仅集中声明 webshell 相关的窄接口
- **无 session 数量上限**：Router 本身不限制注册 session 数。并发限制由 HTTP 层用 `CountByUser` / `CountByDevice` 实现——若 HTTP 层遗漏检查，Router 不会兜底
