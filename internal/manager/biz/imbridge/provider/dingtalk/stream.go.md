# `stream.go` 技术实现文档

> 源文件：`internal/manager/biz/imbridge/provider/dingtalk/stream.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/imbridge/provider/dingtalk`

## 1. 概述

本文件是钉钉 IM provider 的入站流式客户端：通过钉钉官方 `dingtalk-stream-sdk-go` 建立出向 Stream 连接，接收 chatbot 回调并桥接到 agent 运行时。无需公网 webhook。设计要点：每条 inbound 在独立 goroutine 中处理（agent run 30s+，不能阻塞 SDK 读循环）；session webhook 只能创建消息不能编辑，所以 `senderAdapter` 故意只实现 `Sender` 不实现 `MessageEditor`。

## 2. 包信息

- **包名**：`dingtalk`
- **所属模块**：`internal/manager/biz/imbridge/provider/dingtalk`
- **依赖方向**：被 `imbridge.StreamSupervisor` 通过 `NewStreamFactory` 调用；依赖 `biz/imbridge`（Bridge/InboundMessage）、`model/imbridge`（ImApp/Provider 常量）、钉钉官方 SDK

## 3. 关键类型与接口

```go
type StreamClient struct {
    app    *model.ImApp
    bridge *bizbridge.Bridge
    log    *slog.Logger
}

type markdownReplier interface {
    SimpleReplyMarkdown(ctx context.Context, sessionWebhook string, title, content []byte) error
}

// senderAdapter 故意只实现 bizbridge.Sender。钉钉的 per-message session webhook
// 无法编辑先前消息，所以 bridge 缓冲流式输出并在最终答案时调用一次原生 Markdown 回复。
type senderAdapter struct {
    webhook string
    replier markdownReplier
}
```

## 4. 关键函数与流程

### `NewStreamClient`
- **签名**：`func NewStreamClient(app *model.ImApp, bridge *bizbridge.Bridge, log *slog.Logger) *StreamClient`
- **职责**：构造一个 ImApp 对应的 Stream 客户端
- **流程**：log nil → `slog.Default()`；log 加 `provider=dingtalk` + `im_app_id` 字段

### `StreamClient.Run`
- **签名**：`func (c *StreamClient) Run(ctx context.Context) error`
- **职责**：启动 SDK 连接并阻塞直到 supervisor 取消
- **流程**：
  1. `client.NewStreamClient(WithAppCredential(NewAppCredentialConfig(appID, appSecret)))`
  2. `RegisterChatBotCallbackRouter(c.onMessage)`
  3. `stream.Start(ctx)`；失败 `fmt.Errorf("start dingtalk stream: %w", err)`
  4. `<-ctx.Done()` 阻塞
  5. 关闭前 `AutoReconnect = false` + `Close()`，防止凭据轮换后旧客户端后台存活
  6. 返回 `ctx.Err()`
- **错误处理**：SDK 自己负责瞬态重连；Close 前关自愈避免幽灵客户端

### `StreamClient.onMessage`
- **签名**：`func (c *StreamClient) onMessage(_ context.Context, data *chatbot.BotCallbackDataModel) ([]byte, error)`
- **职责**：SDK 回调入口，转成 `InboundMessage` 后异步交给 bridge
- **流程**：
  1. `inboundFromCallback` 解析；失败返回 `(nil, nil)`（不向 SDK 报错）
  2. 构造 `senderAdapter{webhook: SessionWebhook, replier: NewChatbotReplier()}`
  3. `go func()` 中 `defer recover()` + `bridge.HandleInbound(context.Background(), sender, in)`
  4. 返回 `(nil, nil)` 让 SDK 立即释放
- **错误处理**：goroutine 内 recover 兜底 panic 并 log stack；HandleInbound 错误仅 Warn

### `inboundFromCallback`
- **签名**：`func inboundFromCallback(appID string, data *chatbot.BotCallbackDataModel) (bizbridge.InboundMessage, bool)`
- **职责**：把钉钉回调转成平台无关 InboundMessage
- **校验**：data nil / msgtype != "text" / content 空 / ConversationId 空 / SessionWebhook 空 → 返回 false
- **OpenID 取值**：优先 `SenderStaffId`，回退 `SenderId`
- **ReceiveIDType**：固定 `"conversation_id"`

### `senderAdapter.SendText`
- **签名**：`func (s senderAdapter) SendText(ctx, _, _ string, text string) (string, error)`
- **职责**：通过 session webhook 回复一条 Markdown
- **流程**：webhook 空 → error；`replier.SimpleReplyMarkdown(ctx, webhook, []byte("Ongrid"), []byte(text))`；成功返回 `"sent"`（钉钉无 message_id 概念）
- **错误处理**：`fmt.Errorf("dingtalk reply markdown: %w", err)`

### `NewStreamFactory`
- **签名**：`func NewStreamFactory(log *slog.Logger) bizbridge.StreamClientFactory`
- **职责**：返回 supervisor 注册用的工厂；校验 AppID/AppSecret 非空

## 5. 依赖关系

- **内部包**：`biz/imbridge`（Bridge/InboundMessage/Sender/StreamClient/StreamClientFactory）、`model/imbridge`（ImApp/ProviderDingTalk）
- **外部库**：`github.com/open-dingtalk/dingtalk-stream-sdk-go`（client/chatbot）
- **被调用方**：`main.go` 通过 `NewStreamFactory` 注册到 `StreamSupervisor`

## 6. 并发与资源管理

- **goroutine 分离**：每条 inbound 启独立 goroutine 跑 `bridge.HandleInbound`，避免阻塞 SDK 读循环
- **detached context**：goroutine 用 `context.Background()`——SDK 传入的 ctx 在回调返回后取消，agent run 需要存活更久；supervisor 取消通过 `stream.Start(ctx)` 间接传导
- **recover 兜底**：goroutine 内 `defer recover()` + `debug.Stack()` log，防止单条消息处理 panic 杀死整个连接
- **Close 顺序**：先 `AutoReconnect=false` 再 `Close()`，凭据轮换时旧客户端不会后台重连

## 7. 设计模式与亮点

- **故意只实现 Sender**：注释明示钉钉 session webhook 不能编辑消息，bridge 据此缓冲流式输出并单次回复最终答案
- **返回 (nil, nil) 而非错误**：SDK 不需要知道 bridge 是否成功；错误已 log，向 SDK 报错只会触发不必要的重试
- **OpenID 回退**：`SenderStaffId` 优先，`SenderId` 兜底，适应不同钉钉机器人配置
- **factory 校验前置**：AppID/AppSecret 空在 factory 阶段就拒，避免运行时才发现配置错误

## 8. 注意事项

- **ReceiveIDType 固定 conversation_id**：钉钉的 session webhook 已隐含目标会话
- **message_id 返回 "sent"**：钉钉不返回可编辑用的 message_id，bridge 拿到 "sent" 后不会尝试 EditText
- **仅支持 text 消息**：`msgtype != "text"` 直接丢弃
- **凭据轮换**：supervisor 比较 AppID+AppSecret tail，变化时 cancel+重建；Run 中 `AutoReconnect=false` 保证旧客户端干净退出
