# `client.go` 技术实现文档

> 源文件：`internal/manager/biz/imbridge/provider/slack/client.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/imbridge/provider/slack`

## 1. 概述

本文件是 Slack Web API + Socket Mode 的薄客户端：管理双 token（app_token for Socket Mode 握手，bot_token for Web API 调用），提供 `PostMessage`/`UpdateMessage`/`OpenConnection` 三个出站方法。设计要点：零值 `http.Client` 以遵循 `HTTPS_PROXY`/`NO_PROXY` 环境变量（mainland-China 出口代理友好）；Slack 错误通过响应体 `ok=false` 表达（HTTP 200），所以总是读 body 再判 ok。红线：app_token 仅用于 `apps.connections.open`，绝不外泄。

## 2. 包信息

- **包名**：`slack`
- **所属模块**：`internal/manager/biz/imbridge/provider/slack`
- **依赖方向**：被同包 `stream.go` 调用；依赖 `imformat`（SlackSections/PlainExcerpt）

## 3. 关键类型与接口

```go
// SecretFields 是 ImApp.AppSecret 解析后的形状（JSON 存储，便于扩展）
type SecretFields struct {
    AppToken string `json:"app_token"` // xapp-... app-level token
    BotToken string `json:"bot_token"` // xoxb-... bot user token
}

type Client struct {
    appToken string
    botToken string
    hc       *http.Client
    base     string // API root；测试可覆盖，默认 https://slack.com/api
}

type apiResp struct {
    OK               bool            `json:"ok"`
    Error            string          `json:"error"`
    Warning          string          `json:"warning"`
    ResponseMetadata json.RawMessage `json:"response_metadata,omitempty"`
}

const DialTimeout = 10 * time.Second // WebSocket TLS 握手超时
```

## 4. 关键函数与流程

### `ParseSecret`
- **签名**：`func ParseSecret(raw string) (SecretFields, error)`
- **职责**：解析 ImApp.AppSecret JSON 为双 token；校验前缀
- **流程**：
  1. TrimSpace；空 → error
  2. `json.Unmarshal` 到 SecretFields
  3. TrimSpace 各 token
  4. AppToken 空 → error；BotToken 空 → error
  5. AppToken 必须以 `xapp-` 开头；BotToken 必须以 `xoxb-` 开头（redact 后报错）
- **错误处理**：非 JSON / 缺 token / 前缀错误均明确报错，便于 UI 提前发现配置问题

### `redact`
- **签名**：`func redact(s string) string`
- **职责**：保留前 6 字符 + "…"，避免错误日志泄露完整 token

### `NewClient / NewClientFromSecret`
- **签名**：`func NewClient(appToken, botToken string) *Client` / `func NewClientFromSecret(rawSecret string) (*Client, error)`
- **职责**：构造客户端；`NewClientFromSecret` 是 stream factory 的便捷封装
- **流程**：零值 `http.Client`（遵循代理环境变量），base 默认 `https://slack.com/api`

### `Client.SetBaseURL`
- 测试 seam，指向 httptest server

### `Client.call`
- **签名**：`func (c *Client) call(ctx, method, token string, body any, dst any) error`
- **职责**：POST JSON 到 `/api/<method>`，解码信封
- **流程**：
  1. Marshal body
  2. `http.NewRequestWithContext` + `Authorization: Bearer <token>`
  3. `hc.Do`；读 raw body
  4. `StatusCode != 200` → error（带 status + 截断 body）
  5. 解析 `apiResp` 信封
  6. `!env.OK` → error（带 method + env.Error，暴露真实 Slack 原因如 "channel_not_found"）
  7. dst 非 nil → 二次 unmarshal 到 dst
- **错误处理**：decode 失败带截断 body；ok=false 带 env.Error

### `Client.PostMessage`
- **签名**：`func (c *Client) PostMessage(ctx, channel, text string) (string, error)`
- **职责**：`chat.postMessage`，返回 ts（Slack 的高精度浮点字符串，不要 parse）
- **流程**：`nativeMessageBody` → call → 校验 ts 非空
- **错误处理**：ts 空 → error

### `Client.UpdateMessage`
- **签名**：`func (c *Client) UpdateMessage(ctx, channel, ts, text string) error`
- **职责**：`chat.update` 替换消息文本
- **流程**：`nativeMessageBody` + `ts` → call
- **注释**：Slack 静默接受 no-op（相同文本）返回 ok=true，无需像 Telegram 那样吞 "message is not modified"

### `Client.OpenConnection`
- **签名**：`func (c *Client) OpenConnection(ctx) (string, error)`
- **职责**：`apps.connections.open` 获取短-lived Socket Mode WebSocket URL（唯一用 app_token 的地方）
- **流程**：call with appToken → 返回 url；空 → error

### `nativeMessageBody`
- **签名**：`func nativeMessageBody(channel, markdown string) map[string]any`
- **职责**：构造 Block Kit 消息体
- **流程**：
  1. `imformat.SlackSections(markdown)` 切 section
  2. 每个 section 包装成 `{"type":"section","text":{"type":"mrkdwn","text":section}}`
  3. 顶层 `text` 用 `imformat.PlainExcerpt(markdown, 4000)` 作为通知/降级回退

### `truncate`
- 截断 byte 到 n 字符 + "…"

## 5. 依赖关系

- **内部包**：`biz/imbridge/imformat`（SlackSections/PlainExcerpt）
- **外部库**：仅标准库
- **被调用方**：同包 `stream.go`

## 6. 并发与资源管理

- **无锁**：Client 无共享状态，token 是构造时传入的不变值
- **零值 http.Client**：遵循 `HTTPS_PROXY`/`NO_PROXY`，mainland-China 出口代理友好
- **ctx 透传**：所有 `http.NewRequestWithContext`
- **defer resp.Body.Close**：每个请求体关闭

## 7. 设计模式与亮点

- **双 token 分离**：app_token 仅 Socket Mode 握手用，bot_token 仅 Web API 用，职责清晰
- **零值 http.Client 代理友好**：注释明示 mainland-China 经代理走 Slack，同 Telegram
- **ok=false 而非 HTTP 错误**：Slack 用 200 + body.ok 表达业务错误，call 函数统一处理
- **ts 不 parse**：注释明示 "DON'T parse it, just round-trip the string verbatim"——高精度浮点 parse 风险
- **nativeMessageBody 双层**：Block Kit sections 给富客户端，顶层 text 给通知/降级客户端
- **ParseSecret 前缀校验**：`xapp-`/`xoxb-` 前缀检查，UI 阶段就发现配置错误
- **redact 日志安全**：错误日志只保留前 6 字符 + "…"

## 8. 注意事项

- **app_token 仅 apps.connections.open**：注释明示这是唯一使用 app_token 的地方
- **ts 是字符串**：高精度浮点，parse 风险大，原样 round-trip
- **SlackSections 上限**：imformat 内部 cap 45 个 section + 每 section 2900 rune
- **DialTimeout=10s**：WebSocket TLS 握手上限，注释明示 Slack wss 端点通常 <5s
- **base 默认 https://slack.com/api**：测试通过 SetBaseURL 指向 httptest
- **无重试**：call 不重试，由 stream.go 的 supervisor 外层 backoff 兜底
