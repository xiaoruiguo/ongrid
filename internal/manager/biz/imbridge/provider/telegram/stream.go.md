# `stream.go` 技术实现文档

> 源文件：`internal/manager/biz/imbridge/provider/telegram/stream.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/imbridge/provider/telegram`

## 1. 概述

本文件是 Telegram IM provider 的入站 long-poll 客户端：因 Telegram 无 WebSocket stream（不像飞书），用 `getUpdates` long-poll（出向调用，代理友好）。设计要点：每 poll ctx 加 10s buffer 在 server-side timeout 之上（stall 检测）；agent run 在 detached goroutine 跑（30s+ 不能阻塞 poll loop）；sender allowlist 强制（bot 公开可发现，空 allowlist 会让任何人命令 agent，ADR-031）。红线：非 allowlist 用户静默丢弃，不回复不确认（防存在性泄露）。

## 2. 包信息

- **包名**：`telegram`
- **所属模块**：`internal/manager/biz/imbridge/provider/telegram`
- **依赖方向**：被 `imbridge.StreamSupervisor` 通过 `NewStreamFactory` 调用；依赖 `biz/imbridge`、`model/imbridge`、同包 `Client`

## 3. 关键类型与接口

```go
const pollTimeoutSec = 25 // server-side long-poll 等待；per-poll ctx 加 buffer

type StreamClient struct {
    app     *model.ImApp
    bridge  *bizbridge.Bridge
    client  *Client
    allowed map[string]struct{} // sender user-id allowlist（ADR-031）
    log     *slog.Logger
}

// senderAdapter 满足 bizbridge.Sender + MessageEditor
// chatID per-inbound 绑定；message_id 作为十进制字符串 round-trip
type senderAdapter struct {
    client *Client
    chatID string
}
```

## 4. 关键函数与流程

### `NewStreamClient`
- **签名**：`func NewStreamClient(app *model.ImApp, bridge *bizbridge.Bridge, log *slog.Logger) *StreamClient`
- **职责**：构造一个 ImApp 对应的 poll 客户端，解析 allowlist
- **流程**：
  1. log nil → Default；log 加 `provider=telegram` + `im_app_id`
  2. `bizbridge.ParseAllowFrom(app.AllowFrom)` 解析 allowlist 到 map
  3. `NewClient(app.AppSecret)` 用 bot token 构造 Client
- **注释**：validate() 保证 Telegram 的 AllowFrom 非空，空 set 只能是畸形 legacy row，正确拒绝所有人

### `StreamClient.Run`
- **签名**：`func (c *StreamClient) Run(ctx context.Context) error`
- **职责**：long-poll getUpdates 直到 ctx 取消
- **流程**：
  1. Info log "starting telegram getUpdates poll"
  2. 循环：
     - `ctx.Err() != nil` → 返回
     - `pollCtx, cancel := context.WithTimeout(ctx, (pollTimeoutSec+10)*time.Second)` —— 35s 上限，server 25s + 10s buffer
     - `client.GetUpdates(pollCtx, offset, pollTimeoutSec)`
     - `cancel()`
     - err：ctx done 返回 ctx.Err()；否则 `fmt.Errorf("getUpdates: %w", err)` 返回 supervisor
     - 遍历 updates：`UpdateID >= offset` → `offset = UpdateID + 1`（ack 不再重投）；`c.handle(u)`
- **注释**：单 bot 只能一个 poller（Telegram 拒绝并发 getUpdates），supervisor 保证一 ImApp 一客户端

### `StreamClient.handle`
- **签名**：`func (c *StreamClient) handle(u Update)`
- **职责**：处理单条 update，校验 + allowlist + 异步交 bridge
- **流程**：
  1. `m == nil || m.Chat == nil || m.Text == ""` → 丢弃（S1：仅 text DM/group，忽略非 text + 编辑）
  2. `chatID = strconv.FormatInt(m.Chat.ID, 10)`
  3. 提取 openID（From.ID）+ userName（From.FirstName）
  4. **allowlist 校验**：`c.allowed[openID]` 不存在或 `m.From == nil` → Warn "non-allowlisted sender — ignored"，**静默丢弃**（不回复不确认，防存在性泄露）
  5. 构造 `InboundMessage{Provider: telegram, AppID, ChatID: chatID, OpenID, UserName, Text, EventID: strconv.Itoa(UpdateID), ReceiveIDType: "chat_id"}`
  6. `senderAdapter{client, chatID}` + `go func()` 跑 `bridge.HandleInbound(context.Background(), sender, in)`
- **错误处理**：HandleInbound 错误仅 Warn
- **注释**：allowlist 静默丢弃镜像 OpenClaw allowFrom，回复会确认 bot 存在 + 泄露是 agent

### `senderAdapter.SendText`
- **签名**：`func (s senderAdapter) SendText(ctx, receiveID, _, text string) (string, error)`
- **职责**：发消息，返回 message_id 字符串
- **流程**：chat 优先 receiveID，回退 s.chatID；`client.SendMessage` → `strconv.Itoa(id)`

### `senderAdapter.EditText`
- **签名**：`func (s senderAdapter) EditText(ctx, messageID, text string) error`
- **职责**：编辑消息
- **流程**：`strconv.Atoi(messageID)` → `client.EditMessageText(ctx, s.chatID, mid, text)`
- **错误处理**：Atoi 失败 `fmt.Errorf("telegram message id %q: %w", messageID, err)`

### `NewStreamFactory`
- **签名**：`func NewStreamFactory(log *slog.Logger) bizbridge.StreamClientFactory`
- **职责**：返回 supervisor 注册用的工厂；校验 AppSecret 非空

## 5. 依赖关系

- **内部包**：`biz/imbridge`（Bridge/InboundMessage/ParseAllowFrom/StreamClient/StreamClientFactory）、`model/imbridge`、同包 `Client`
- **外部库**：仅标准库
- **被调用方**：`main.go` 通过 `NewStreamFactory` 注册

## 6. 并发与资源管理

- **detached context**：goroutine 用 `context.Background()`，注释明示"agent runs take 30s+; the poll loop must keep moving"
- **per-poll ctx timeout**：35s 上限，stall 检测；poll 返回后立即 cancel
- **offset 单调递增**：`UpdateID >= offset` 才更新 offset，防回退
- **allowlist map 只读**：构造时定型，无锁

## 7. 设计模式与亮点

- **long-poll 出向**：注释明示 setWebhook 需入站不可靠，getUpdates 出向走代理
- **allowlist 静默丢弃**：ADR-031 + OpenClaw 兼容，回复会泄露 bot 存在 + 是 agent
- **per-poll ctx buffer**：server 25s + ctx 35s，stall 时 ctx 先超时返回 error
- **offset ack 机制**：`offset = UpdateID + 1` 让 Telegram 不再重投该 update
- **message_id 字符串 round-trip**：Telegram 是 int，bridge 是 string，`strconv` 转换
- **单 poller 保证**：supervisor 保证一 ImApp 一客户端，Telegram 拒绝并发 getUpdates
- **chatID per-inbound 绑定**：senderAdapter 绑定 chatID，bridge 拿 message_id 后可直接 EditText

## 8. 注意事项

- **pollTimeoutSec=25**：server-side long-poll 等待；ctx 加 10s buffer = 35s
- **allowlist 强制**：usecase.go validate 保证 Telegram 的 AllowFrom 非空 numeric ID
- **静默丢弃非 allowlist**：不回复不确认，Warn log 带 user_id 供 admin 加白
- **仅 text 消息**：非 text + 编辑忽略
- **EventID=UpdateID**：用作 dedup key
- **chat_id 字符串化**：Telegram int64 → string，bridge 统一用 string
