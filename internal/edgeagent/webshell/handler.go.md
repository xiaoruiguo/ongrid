# `handler.go` 技术实现文档

> 源文件：`internal/edgeagent/webshell/handler.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/webshell`

## 1. 概述

本文件把 edge agent 变成通用 TCP 流转发器：manager 通过 frontier 开一个流到 edge，Meta blob 描述目标（`{"target":"127.0.0.1:22"}`），edge 拨号该本地 TCP socket 并 `io.Copy` 双向字节。SSH 完全在 manager 侧：manager 包 ssh.NewClientConn、跑 PTY + Shell、pump 浏览器 WebSocket。edge 无 SSH 客户端 / pty 管理 / session map——保持轻量。

## 2. 包信息

- **包名**：`webshell`
- **所属模块**：edgeagent 流转发能力层
- **依赖方向**：被 `cmd/ongrid-edge` 调用 `Register`；调用 `tunnel`

## 3. 关键类型与接口

```go
// Acceptor 接受入站流；*tunnel.Client 满足
type Acceptor interface {
    AcceptStream() (tunnel.StreamConn, error)
}

// streamMeta 是 manager 放在 stream Meta blob 的 JSON 形状
type streamMeta struct {
    Target string `json:"target"`
}

// allowedTargets 限制 edge 可拨号的目标地址
var allowedTargets = map[string]bool{
    "127.0.0.1:22": true,
    "localhost:22": true,
}
```

## 4. 关键函数与流程

### `Register`
- **签名**：`func Register(client Acceptor, log *slog.Logger)`
- **职责**：启动 AcceptStream 循环 goroutine
- **流程**：log nil → default；`go acceptLoop(client, log)`；log Info「stream forwarder running」
- **错误处理**：无错误返回；立即返回

### `acceptLoop`
- **签名**：`func acceptLoop(client Acceptor, log *slog.Logger)`
- **职责**：永远泵 `AcceptStream` 调用
- **流程**：
  1. `client.AcceptStream()` 阻塞等流
  2. 错误时判断：
     - `io.EOF` / "not dialed" / "closed" → sleep 500ms 重试（隧道重建中）
     - 其他 → Warn + sleep 1s 重试
  3. 成功 → `go handleStream(stream, log)`（每流独立 goroutine，并发 shell 不互阻塞）
- **错误处理**：所有错误都重试，永不退出（除非进程终止）

### `handleStream`
- **签名**：`func handleStream(stream tunnel.StreamConn, log *slog.Logger)`
- **职责**：处理单个流转发
- **流程**：
  1. `defer stream.Close()`
  2. 解码 Meta blob 到 `streamMeta`；失败写 stream 错误返回
  3. target 空 → 默认 `127.0.0.1:22`
  4. `allowedTargets[target]` 校验；不在 allowlist → 写 stream 错误 + Warn 返回
  5. `net.DialTimeout("tcp", target, 5*time.Second)`；失败写 stream 错误返回
  6. `defer conn.Close()`
  7. log Info「forwarding」
  8. 起两个 goroutine：`io.Copy(conn, stream)` 和 `io.Copy(stream, conn)`
  9. errs channel buffer 2；首个 error 触发关闭两端
  10. `<-errs` 等首个；关闭两端；再 `<-errs` 等第二个 goroutine 退出
- **错误处理**：每步失败都通过 `writeStreamError` 把错误写入 stream 让 manager 看到

### `writeStreamError`
- **签名**：`func writeStreamError(s io.Writer, msg string)`
- **职责**：把简短纯文本错误写入流
- **流程**：`io.WriteString(s, "ongrid-edge webshell forwarder: "+msg+"\n")`；忽略写入错误
- **错误处理**：写入错误被忽略（manager 侧 ssh.NewClientConn 会因 EOF 失败）

## 5. 依赖关系

- **内部包**：`internal/pkg/tunnel`
- **外部库**：标准库 `encoding/json`、`errors`、`fmt`、`io`、`log/slog`、`net`、`strings`、`time`
- **被调用方**：`cmd/ongrid-edge` 主程序调 `Register`

## 6. 并发与资源管理

- **`acceptLoop` 永不退出**：所有错误都 sleep + 重试；隧道重建时透明恢复
- **每流独立 goroutine**：`handleStream` 在新 goroutine 跑；并发 shell 不互阻塞
- **双向 io.Copy + errs channel**：两个 goroutine 分别复制一个方向；首个 error 关闭两端；再等第二个 goroutine 退出避免泄漏
- **defer 关闭**：stream 和 conn 都 `defer Close`；handleStream 返回时自动清理
- **5s 拨号超时**：防止目标不可达时卡住

## 7. 设计模式与亮点

- **edge 极简转发器**：edge 无 SSH 客户端 / pty / session map——所有策略 / 审计 / 并发 / 踢出逻辑在 manager 侧；edge 只做 TCP 字节转发
- **allowlist 是安全边界**：`allowedTargets` 硬编码 localhost:22——防止被攻破的 manager 把 edge pivot 到任意内网 IP；Phase 2 可通过 `/etc/ongrid-edge/webshell.yaml` 扩展
- **`writeStreamError` 友好失败**：把错误写入 stream 让 manager 的 `ssh.NewClientConn` 因有用消息失败，而非「EOF on protocol read」
- **重试而非退出**：`acceptLoop` 对 transient 错误（EOF / not dialed / closed）sleep 500ms 重试，对其他错误 sleep 1s 重试——让隧道重建时 edge 自动恢复
- **首个 error 关闭两端**：双向 copy 中任一方断开都关闭另一端——避免半开连接泄漏
- **等待两个 goroutine 退出**：`<-errs` 两次确保两个 io.Copy 都返回后再 handleStream 返回——防止 goroutine 泄漏

## 8. 注意事项

- **`allowedTargets` 硬编码 localhost:22**：未来要支持 jumpbox / sidecar SSH（`10.0.0.5:22`）需扩展 allowlist；当前拒绝所有非 localhost 目标
- **无审计日志**：edge 不记录「谁连了哪个 shell」——所有审计在 manager 侧；edge 只记录「forwarding to target」Info
- **无并发上限**：每个流独立 goroutine，无并发限制；manager 侧应控制并发（踢出旧 session）
- **无超时**：转发连接无空闲超时；shell 会话可能长时间空闲；manager 侧应控制
- **`net.DialTimeout` 5s**：如果 sshd 启动慢或负载高，5s 可能不够；可配置化
- **`writeStreamError` 忽略写入错误**：若 stream 已断开，错误写入失败被忽略；manager 会因 EOF 失败
- **Meta blob 解码容忍未知字段**：`json.Unmarshal` 默认允许 unknown fields；未来 manager 加 `ttl` / `audit_id` 等字段不破坏老 edge
- **Phase 2 配置化**：注释提到未来通过 `/etc/ongrid-edge/webshell.yaml` 扩展 allowlist；当前硬编码是临时方案
- **SSH 私钥不进 edge**：edge 不持有任何 SSH 凭据；认证完全在 manager 侧；edge 只信任 tunnel 层的 AuthFunc
- **`acceptLoop` 永不退出意味着进程退出依赖外部**：ctx 取消时 AcceptStream 应返回错误，但当前实现不传 ctx——`Register` 应接受 ctx 并在 cancel 时退出 acceptLoop（当前未实现，依赖进程终止）
