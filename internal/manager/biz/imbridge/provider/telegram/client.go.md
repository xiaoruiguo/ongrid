# `client.go` 技术实现文档

> 源文件：`internal/manager/biz/imbridge/provider/telegram/client.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/imbridge/provider/telegram`

## 1. 概述

本文件是 Telegram Bot API 的薄客户端：提供 `GetUpdates`（long-poll）/ `SendMessage` / `EditMessageText` 三个方法。设计要点：零值 `http.Client` 遵循 `HTTP(S)_PROXY`（mainland-China 出口代理友好，因 setWebhook 需 Telegram 入站不可靠）；429 + 5xx 重试（honoring `retry_after`），4xx 不重试；`editMessageText` 的 "message is not modified" 静默吞掉（流式更新时 no-op 常见）。红线：网络级错误不在此重试，冒泡到 supervisor 的 backoff。

## 2. 包信息

- **包名**：`telegram`
- **所属模块**：`internal/manager/biz/imbridge/provider/telegram`
- **依赖方向**：被同包 `stream.go` 调用；依赖 `imformat`（TelegramHTML）

## 3. 关键类型与接口

```go
const (
    maxCallRetries = 3               // 429 + 5xx 重试上限
    maxRetryWait   = 60 * time.Second // retry_after 上限，防多分钟 429 阻塞 poll loop
)

type Client struct {
    token string
    hc    *http.Client
    base  string // API root；测试可覆盖，默认 https://api.telegram.org
}

type apiResp struct {
    OK          bool            `json:"ok"`
    Result      json.RawMessage `json:"result"`
    Description string          `json:"description"`
    ErrorCode   int             `json:"error_code"`
    Parameters  *struct {
        RetryAfter int `json:"retry_after"`
    } `json:"parameters"`
}

type Update struct {
    UpdateID int `json:"update_id"`
    Message  *struct {
        MessageID int    `json:"message_id"`
        From      *struct {
            ID        int64  `json:"id"`
            FirstName string `json:"first_name"`
            Username  string `json:"username"`
        } `json:"from"`
        Chat *struct {
            ID   int64  `json:"id"`
            Type string `json:"type"`
        } `json:"chat"`
        Text string `json:"text"`
    } `json:"message"`
}
```

## 4. 关键函数与流程

### `NewClient`
- **签名**：`func NewClient(token string) *Client`
- **职责**：构造一个 bot token 对应的客户端
- **流程**：零值 `http.Client`（遵循代理环境变量），base 默认 `https://api.telegram.org`

### `Client.call`
- **签名**：`func (c *Client) call(ctx, method string, body any) (json.RawMessage, error)`
- **职责**：POST JSON 到 `/bot<token>/<method>`，带 429/5xx 重试
- **流程**：
  1. Marshal body
  2. `http.NewRequestWithContext` + `Content-Type: application/json`
  3. `hc.Do`；读 raw body
  4. **重试判定**（attempt < maxCallRetries）：`StatusCode == 429 || StatusCode >= 500`
     - 429：解析 `parameters.retry_after`，转 duration；超过 maxRetryWait 截断
     - 5xx：`backoffDelay(attempt)` = 1s/2s/4s
     - `select { ctx.Done | time.After(wait) }` 后 continue
  5. 解析 `apiResp` 信封
  6. `!env.OK` → error（带 method + ErrorCode + Description）
  7. 返回 `env.Result`
- **错误处理**：网络错误直接返回（不重试，让 supervisor backoff）；decode 失败带截断 body

### `backoffDelay`
- **签名**：`func backoffDelay(attempt int) time.Duration`
- **职责**：5xx 重试退避 = `1 << attempt` 秒（1s/2s/4s）

### `Client.GetUpdates`
- **签名**：`func (c *Client) GetUpdates(ctx, offset, timeoutSec int) ([]Update, error)`
- **职责**：long-poll 拉消息
- **流程**：call `getUpdates` with `{offset, timeout, allowed_updates: ["message"]}` → 解析 `[]Update`

### `Client.SendMessage`
- **签名**：`func (c *Client) SendMessage(ctx, chatID, text string) (int, error)`
- **职责**：发消息，返回 message_id
- **流程**：`telegramMessageBody` → call `sendMessage` → 解析 message_id

### `Client.EditMessageText`
- **签名**：`func (c *Client) EditMessageText(ctx, chatID string, messageID int, text string) error`
- **职责**：编辑消息文本
- **流程**：`telegramMessageBody` + `message_id` → call `editMessageText`
- **错误处理**：错误消息含 "message is not modified" → 返回 nil（注释：流式更新时 no-op 常见，Telegram 400 拒绝，吞掉；其他 400/error 仍传播）

### `telegramMessageBody`
- **签名**：`func telegramMessageBody(chatID, markdown string) map[string]any`
- **职责**：构造请求体
- **流程**：`{chat_id, text: imformat.TelegramHTML(markdown), parse_mode: "HTML", link_preview_options: {is_disabled: true}}`
- **注释**：禁用 link preview 避免 IM 内嵌预览干扰

### `truncate`
- 截断 byte 到 n 字符 + "…"

## 5. 依赖关系

- **内部包**：`biz/imbridge/imformat`（TelegramHTML）
- **外部库**：仅标准库
- **被调用方**：同包 `stream.go`

## 6. 并发与资源管理

- **无锁**：Client 无共享状态，token 不变
- **零值 http.Client**：注释明示遵循 `HTTP(S)_PROXY`/`NO_PROXY`，mainland-China 经代理走 Telegram
- **ctx 透传**：所有 `http.NewRequestWithContext`
- **重试 ctx 感知**：`select { ctx.Done | time.After(wait) }`，supervisor 取消时立即返回

## 7. 设计模式与亮点

- **long-poll 而非 webhook**：注释明示 setWebhook 需 Telegram 入站，mainland-China 不可靠；getUpdates 是出向调用，走代理
- **429/5xx 重试，4xx 不重试**：注释明示 4xx 是硬错误重试无益
- **retry_after 截断**：maxRetryWait=60s 防多分钟 429 阻塞 poll loop
- **"message is not modified" 吞掉**：流式更新时 no-op 常见，Telegram 严格拒绝，吞掉避免噪音
- **link_preview_options 禁用**：避免 IM 内嵌预览干扰
- **allowed_updates=["message"]**：只拉 message 更新，减少无关事件
- **网络错误不重试**：冒泡到 supervisor backoff，per-poll ctx deadline 是有意为之的 stall 检测

## 8. 注意事项

- **maxCallRetries=3**：仅 429 + 5xx；网络错误由 supervisor 兜底
- **maxRetryWait=60s**：截断 Telegram 的 retry_after，防阻塞
- **backoffDelay 指数**：1s/2s/4s，无 jitter（supervisor 层有 backoff）
- **parse_mode=HTML**：用 imformat.TelegramHTML 转换，HTML 转义面比 MarkdownV2 小
- **token 在 URL path**：`/bot<token>/<method>` 是 Telegram 规范；HTTPS 下可接受
- **base 默认 https://api.telegram.org**：测试通过 base 覆盖
