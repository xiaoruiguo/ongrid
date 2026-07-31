# `client.go` 技术实现文档

> 源文件：`internal/manager/biz/imbridge/provider/feishu/client.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/imbridge/provider/feishu`

## 1. 概述

本文件是飞书 OpenAPI 的薄客户端：管理 `tenant_access_token` 缓存（200s 预刷新窗口），提供 `SendText`/`EditText` 两个出站方法。设计要点：一个实例对应一组 (app_id, app_secret)，token 缓存 per-instance，轮换凭据必须重建 client。红线：token 在过期前 200s 主动刷新，避免用户发送路径上吃 401 round-trip。

## 2. 包信息

- **包名**：`feishu`
- **所属模块**：`internal/manager/biz/imbridge/provider/feishu`
- **依赖方向**：被同包 `stream.go` 的 `senderAdapter` 调用；仅依赖标准库 `net/http` + `encoding/json`

## 3. 关键类型与接口

```go
const DefaultBaseURL = "https://open.feishu.cn"

type Client struct {
    baseURL    string
    appID      string
    appSecret  string
    http       *http.Client
    tokMu      sync.Mutex
    tokValue   string
    tokExpires time.Time
}

type Option func(*Client)

func WithBaseURL(u string) Option          // 国际版 Lark 切换 baseURL
func WithHTTPClient(h *http.Client) Option // 注入测试 http.Client
```

## 4. 关键函数与流程

### `NewClient`
- **签名**：`func NewClient(appID, appSecret string, opts ...Option) *Client`
- **职责**：构造客户端，默认 15s http 超时，默认 `DefaultBaseURL`
- **流程**：应用 Option 覆盖默认值

### `Client.tenantAccessToken`
- **签名**：`func (c *Client) tenantAccessToken(ctx context.Context) (string, error)`
- **职责**：返回缓存的 tenant_access_token，过期前 200s 内刷新
- **流程**：
  1. `tokMu.Lock`；缓存命中且 `time.Until(expires) > 200s` → 返回
  2. POST `/open-apis/auth/v3/tenant_access_token/internal` with `{app_id, app_secret}`
  3. 解析 `{code, msg, tenant_access_token, expire}`
  4. `code != 0 || token == ""` → error
  5. 缓存 `tokValue` + `tokExpires = now + expire秒`
- **错误处理**：decode 失败带 body；非 0 code 返回 code+msg

### `Client.SendText`
- **签名**：`func (c *Client) SendText(ctx, receiveID, receiveIDType, text string) (string, error)`
- **职责**：向目标会话发原生富文本消息，返回 message_id 供后续 EditText
- **流程**：
  1. `tenantAccessToken`
  2. `messageContent(text)` 决定 msg_type（post/text）
  3. POST `/open-apis/im/v1/messages?receive_id_type=<type>` with `{receive_id, msg_type, content}`
  4. 解析 `{code, msg, data.message_id}`
  5. `code != 0` → error；返回 `data.message_id`
- **错误处理**：decode 失败带 body；非 0 code 返回 code+msg

### `Client.EditText`
- **签名**：`func (c *Client) EditText(ctx, messageID, text string) error`
- **职责**：PATCH 已有消息（流式更新用）
- **流程**：
  1. `tenantAccessToken`
  2. `messageID == ""` → error
  3. `messageContent(text)`
  4. PUT `/open-apis/im/v1/messages/<messageID>` with `{msg_type, content}`（注释：msg_type 在 PUT 上不能省，否则 99992402 校验失败）
  5. `code != 0` → error
- **错误处理**：decode 失败带 body；非 0 code 返回 code+msg

### `messageContent`
- **签名**：`func messageContent(markdown string) (string, []byte, error)`
- **职责**：构造消息内容，优先 post+md 节点，超 28KiB 降级 text
- **流程**：
  1. 构造 `[[{"tag":"md","text":markdown}]]`，同时填 `zh_cn` 和 `en_us`（注释：双 locale 防止飞书/Lark 客户端因 UI locale 不同而隐藏消息）
  2. `len(rich) <= 28*1024` → 返回 `("post", rich, nil)`
  3. 否则降级 `{"text": markdown}` → 返回 `("text", plain, nil)`
- **注释要点**：飞书 post 上限 30KiB，text 上限 150KiB；JSON 转义 + 双 locale 条目增加开销，所以 28KiB 阈值留 buffer

## 5. 依赖关系

- **内部包**：无
- **外部库**：仅标准库
- **被调用方**：`stream.go` 的 `senderAdapter`

## 6. 并发与资源管理

- **`tokMu`（Mutex）**：保护 `tokValue`/`tokExpires`；多 goroutine 并发 SendText 时只有一个会刷新 token
- **http.Client 共享**：per-Client 实例，复用连接池
- **ctx 透传**：所有 `http.NewRequestWithContext(ctx, ...)`
- **defer resp.Body.Close**：每个请求体关闭

## 7. 设计模式与亮点

- **200s 预刷新窗口**：注释明示飞书 token 7200s，提前 200s 刷新避免用户发送路径吃 401
- **双 locale 兜底**：`zh_cn` + `en_us` 同时填相同内容，防止飞书/Lark 客户端 UI locale 差异导致消息隐藏
- **post/text 自动降级**：超 28KiB 降级 text，避免 post 30KiB 上限导致发送失败
- **Option 模式**：`WithBaseURL`/`WithHTTPClient` 支持国际版 Lark 和测试注入
- **per-instance token 缓存**：凭据轮换重建 client 即清缓存，无脏 token 风险

## 8. 注意事项

- **15s http 超时**：`http.Client{Timeout: 15 * time.Second}`；caller 可用 ctx deadline 覆盖
- **EditText 必须带 msg_type**：注释明示 PUT 上省 msg_type 会触发 99992402，文档不明显
- **28KiB 降级阈值**：post 上限 30KiB，留 2KiB 给 JSON 转义 + 双 locale 开销
- **token 缓存不持久化**：进程重启重新获取；飞书 token 无状态可重发
- **仅 tenant_access_token**：不用 user_access_token，因为 IM bot 代表应用而非用户
