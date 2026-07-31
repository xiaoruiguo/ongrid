# `stream.go` 技术实现文档

> 源文件：`internal/manager/biz/imbridge/provider/slack/stream.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/imbridge/provider/slack`

## 1. 概述

本文件是 Slack IM provider 的入站 Socket Mode 客户端：通过 `apps.connections.open` 拿短-lived WebSocket URL，拨号后处理 events_api 事件。设计要点：ack 先于 handle（Slack 3s ack 窗口）；20s ping 保活（Slack 30s 空闲关闭）；sender allowlist 强制（Slack workspace 默认任何成员可发消息，必须显式 allowlist）；bot 自己发的消息和编辑通知丢弃防回环。

## 2. 包信息

- **包名**：`slack`
- **所属模块**：`internal/manager/biz/imbridge/provider/slack`
- **依赖方向**：被 `imbridge.StreamSupervisor` 通过 `NewStreamFactory` 调用；依赖 `biz/imbridge`、`model/imbridge`、同包 `Client`、`gorilla/websocket`

## 3. 关键类型与接口

```go
const pingInterval = 20 * time.Second // Slack 30s 空闲关闭，20s ping 保活

type envelope struct {
    EnvelopeID             string          `json:"envelope_id"`
    Type                   string          `json:"type"`      // hello/disconnect/events_api/...
    Payload                json.RawMessage `json:"payload"`
    AcceptsResponsePayload bool            `json:"accepts_response_payload"`
    RetryAttempt           int             `json:"retry_attempt"`
    RetryReason            string          `json:"retry_reason"`
}

type eventsAPIPayload struct {
    TeamID string `json:"team_id"`
    Event  struct {
        Type, Subtype, Channel, User, Text, TS, BotID, ThreadTS string
    } `json:"event"`
}

type StreamClient struct {
    app     *model.ImApp
    bridge  *bizbridge.Bridge
    client  *Client
    allowed map[string]struct{} // Slack user_id allowlist
    log     *slog.Logger
}

type senderAdapter struct {
    client  *Client
    channel string // per-inbound 绑定
}
```

## 4. 关键函数与流程

### `NewStreamClient`
- **签名**：`func NewStreamClient(app *model.ImApp, bridge *bizbridge.Bridge, log *slog.Logger) (*StreamClient, error)`
- **职责**：构造 stream 客户端，解析 secret + allowlist
- **流程**：
  1. `NewClientFromSecret(app.AppSecret)` 解析双 token
  2. `bizbridge.ParseAllowFrom(app.AllowFrom)` 解析 allowlist 到 map
  3. log 加 `provider=slack` + `im_app_id`
- **错误处理**：secret 解析失败返回 error（supervisor 会 log 并跳过）

### `StreamClient.Run`
- **签名**：`func (c *StreamClient) Run(ctx context.Context) error`
- **职责**：拨 Socket Mode WebSocket，处理事件直到 ctx 取消或连接断开
- **流程**：
  1. `client.OpenConnection(ctx)` 拿 wsURL
  2. `url.Parse` 取 host 用于 log
  3. `websocket.DefaultDialer` + `HandshakeTimeout = DialTimeout`
  4. `dialer.DialContext(ctx, wsURL, nil)`；defer `conn.Close()`
  5. 启 `pingLoop` goroutine（20s ticker，发 PingMessage）
  6. 读循环：
     - `conn.ReadMessage()`；ctx done 返回 ctx.Err()
     - probe `type` 字段分派：hello（Info log continue）/ disconnect（Info log + return nil 让 supervisor 立即重连，无 backoff）/ 其他进 envelope 解码
     - **ack 先于 handle**：`conn.WriteMessage` 发 `{"envelope_id":...}`；ack 失败 → error
     - `env.Type == "events_api"` → `handleEvent(env.Payload)`
- **错误处理**：任何错误返回 supervisor；disconnect 返回 nil（无 backoff 重连）

### `StreamClient.pingLoop`
- **签名**：`func (c *StreamClient) pingLoop(ctx, conn, done)`
- **职责**：20s ticker 发 PingMessage，5s 写超时
- **流程**：select ctx.Done / done / ticker.C；ping 失败仅 Debug log（下次 ReadMessage 会暴露真实错误）

### `StreamClient.handleEvent`
- **签名**：`func (c *StreamClient) handleEvent(payload json.RawMessage)`
- **职责**：解码 events_api payload，路由用户文本到 bridge
- **流程**：
  1. `json.Unmarshal` 到 eventsAPIPayload；失败 Warn
  2. Info log 每个事件（event_type/subtype/channel/user/has_bot_id/text_len）
  3. `Type != "message" && Type != "app_mention"` → 丢弃
  4. `BotID != "" || Subtype != ""` → 丢弃（自己发的 + 编辑/删除通知，防回环）
  5. `Text == "" || Channel == ""` → 丢弃
  6. **allowlist 校验**：`c.allowed[user]` 不存在 → Warn "non-allowlisted sender — ignored"
  7. 构造 `InboundMessage{Provider: slack, AppID, ChatID: channel, ThreadID: ThreadTS, OpenID: user, UserName: user, Text: stripMentions(text), EventID: ts, ReceiveIDType: "channel"}`
  8. `senderAdapter{client, channel}` + `go func()` 跑 `bridge.HandleInbound(context.Background(), sender, in)`
- **错误处理**：HandleInbound 错误仅 Warn

### `stripMentions`
- **签名**：`func stripMentions(s string) string`
- **职责**：把 Slack `<@UABCD>` / `<#C1234|general>` / `<https://x|x>` mention 标记转成可读文本
- **流程**：扫描 `<` `>` 段；`@`/`#` 前缀保留 id 部分（去掉 `|label`）；其他保留 `|` 后部分
- **注释**：保留 user-id 字母让 model 看到稳定引用；完整 display-name 解析需 users.info round-trip，MVP 跳过

### `senderAdapter.SendText / EditText`
- SendText：channel 优先 receiveID，回退 s.channel；`client.PostMessage`
- EditText：`client.UpdateMessage(ctx, s.channel, messageID, text)`

### `NewStreamFactory`
- 返回 `bizbridge.StreamClientFactory`

## 5. 依赖关系

- **内部包**：`biz/imbridge`（Bridge/InboundMessage/ParseAllowFrom/StreamClient/StreamClientFactory）、`model/imbridge`、同包 `Client`
- **外部库**：`github.com/gorilla/websocket`
- **被调用方**：`main.go` 通过 `NewStreamFactory` 注册

## 6. 并发与资源管理

- **ping goroutine**：`pingLoop` 独立 goroutine，`done` channel 关闭时退出；defer `close(pingDone)`
- **detached context**：`bridge.HandleInbound` 用 `context.Background()`，注释明示"agent runs take 30s+; the read loop must keep moving (and must keep acking — Slack reuses one socket for all events)"
- **conn.Close**：defer 保证
- **allowlist map 只读**：构造时定型，无锁

## 7. 设计模式与亮点

- **ack 先于 handle**：注释明示 Slack 3s ack 窗口，先 ack 再 handle 避免 Slack 重试
- **disconnect 返回 nil**：让 supervisor 立即重连无 backoff（Slack 主动要求的重连）
- **20s ping 保活**：Slack 30s 空闲关闭，ping 独立 goroutine 不阻塞读循环
- **allowlist 强制**：注释明示 Slack workspace 默认任何成员可发消息，必须显式 allowlist 防止意外用户命令 agent
- **bot 回环防护**：`BotID != "" || Subtype != ""` 丢弃，防止 agent 回复触发自己
- **stripMentions 保留 id**：让 model 看到稳定引用，避免 display-name 解析的 round-trip
- **per-inbound channel 绑定**：senderAdapter 绑定 channel，bridge 拿 ts 后可直接 EditText

## 8. 注意事项

- **pingInterval=20s**：Slack 30s 空闲关闭，留 10s buffer
- **DialTimeout=10s**：定义在 client.go，测试可读
- **allowlist 解析**：`bizbridge.ParseAllowFrom` 共享，Slack user_id 是 U/W 前缀字母
- **ThreadTS 用于话题回复**：非话题消息 ThreadTS 为空
- **EventID=ts**：Slack ts 是高精度浮点字符串，原样用作 dedup key
- **disconnect 不算错误**：返回 nil，supervisor 立即重连
- **ack 用 envelope_id**：Slack 期望 ack body 包含 envelope_id
