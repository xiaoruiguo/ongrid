# `stream.go` 技术实现文档

> 源文件：`internal/manager/biz/imbridge/provider/feishu/stream.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/imbridge/provider/feishu`

## 1. 概述

本文件是飞书 IM provider 的入站长连接客户端：通过官方 `larksuite/oapi-sdk-go` 的 `ws.Client` 拨出 WebSocket，事件经 dispatcher 桥接到 agent 运行时。无需公网 webhook。设计要点：handler 快速返回（agent run 30s+ 在 detached goroutine 跑）；SDK 自带 auto-reconnect，supervisor 外层再加 backoff 兜底终端失败。

## 2. 包信息

- **包名**：`feishu`
- **所属模块**：`internal/manager/biz/imbridge/provider/feishu`
- **依赖方向**：被 `imbridge.StreamSupervisor` 通过 `NewStreamFactory` 调用；依赖 `biz/imbridge`、`model/imbridge`、`larksuite/oapi-sdk-go/v3`

## 3. 关键类型与接口

```go
type StreamClient struct {
    app    *model.ImApp
    bridge *bizbridge.Bridge
    log    *slog.Logger
}

// senderAdapter 包装 Client 以满足 bizbridge.Sender + MessageEditor
type senderAdapter struct {
    client *Client
}
```

## 4. 关键函数与流程

### `NewStreamClient`
- **签名**：`func NewStreamClient(app *model.ImApp, bridge *bizbridge.Bridge, log *slog.Logger) *StreamClient`
- **职责**：为一个 ImApp 构造 stream 客户端
- **流程**：log nil → Default；log 加 `provider=feishu` + `im_app_id`

### `StreamClient.Run`
- **签名**：`func (c *StreamClient) Run(ctx context.Context) error`
- **职责**：拨长连接并阻塞直到 ctx 取消或 SDK 耗尽重连预算
- **流程**：
  1. `dispatcher.NewEventDispatcher("", "").OnP2MessageReceiveV1(func{ c.onMessage(ev) })`
  2. `larkws.NewClient(appID, appSecret, WithEventHandler(disp), WithAutoReconnect(true))`
  3. `wsClient.Start(ctx)` 阻塞
- **错误处理**：SDK 自带重连；终端错误返回给 supervisor 外层 backoff

### `StreamClient.onMessage`
- **签名**：`func (c *StreamClient) onMessage(ev *larkim.P2MessageReceiveV1) error`
- **职责**：把飞书 P2MessageReceiveV1 信封转成 InboundMessage，异步交 bridge
- **流程**：
  1. 校验 ev/Event/Message 非空
  2. `MessageType != "text"` → 丢弃（注释：S1 阶段仅 text，贴纸/文件/卡片静默丢弃）
  3. `ChatId` 空 → 丢弃
  4. 解析 content JSON 取 text；空 → 丢弃
  5. 提取 rootID（thread）/eventID（Header）/openID（Sender.SenderId.OpenId）
  6. 构造 `InboundMessage{Provider: feishu, AppID, ChatID, ThreadID: rootID, OpenID, Text, EventID, ReceiveIDType: "chat_id"}`
  7. `senderAdapter{client: NewClient(appID, appSecret)}` —— 注释：每条消息新建 Client（token 缓存 per-instance，但飞书 SDK 回调频率低，开销可接受）
  8. `go func()` 跑 `bridge.HandleInbound(context.Background(), sender, in)`
- **错误处理**：HandleInbound 错误仅 Warn；返回 nil 让 SDK 不重试

### `senderAdapter.SendText / EditText`
- 透传到 `Client.SendText` / `Client.EditText`

### `NewStreamFactory`
- **签名**：`func NewStreamFactory(log *slog.Logger) bizbridge.StreamClientFactory`
- **职责**：返回 supervisor 注册用的工厂

## 5. 依赖关系

- **内部包**：`biz/imbridge`、`model/imbridge`、同包 `Client`
- **外部库**：`github.com/larksuite/oapi-sdk-go/v3`（dispatcher/larkim/larkws）
- **被调用方**：`main.go` 通过 `NewStreamFactory` 注册

## 6. 并发与资源管理

- **detached context**：goroutine 用 `context.Background()`——SDK 传入的 ctx 在回调返回后取消，agent run 需存活更久；supervisor 取消通过 `wsClient.Start(ctx)` 传导
- **handler 快速返回**：注释明示"returning from the callback right away keeps the SDK's read loop happy"
- **per-message Client**：每条消息 `NewClient`——token 缓存独立，凭据轮换无脏缓存
- **SDK AutoReconnect**：瞬态网络错误 SDK 自愈；终端错误冒泡到 supervisor

## 7. 设计模式与亮点

- **WebSocket 长连接**：飞书原生支持，无需公网 ingress，符合 mainland-China 友好设计
- **dispatcher 模式**：SDK 提供 `OnP2MessageReceiveV1` 注册回调，业务代码只关心事件类型
- **detached goroutine**：agent run 30s+ 必须脱离 SDK 读循环，否则阻塞后续事件
- **仅 text 消息**：S1 阶段策略，贴纸/文件/卡片静默丢弃，避免 agent 处理多模态
- **ReceiveIDType=chat_id**：飞书群消息的稳定标识

## 8. 注意事项

- **token 缓存 per-instance**：每条消息新建 Client 意味着 token 不跨消息复用——飞书回调频率低可接受，高频场景应共享 Client
- **ThreadID=rootID**：用于话题回复；非话题消息 rootID 为空
- **OpenID 可选**：Sender.SenderId.OpenId 可能为 nil（应用未授权读取 sender）
- **content 解析容错**：`_ = json.Unmarshal` 忽略错误，content.Text 空 → 丢弃
- **SDK 重连不外控**：`WithAutoReconnect(true)` 后 SDK 自己管，Run 返回即终端
