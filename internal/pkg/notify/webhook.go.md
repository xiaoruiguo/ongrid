# `webhook.go` 技术实现文档

> 源文件：`internal/pkg/notify/webhook.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/notify`

## 1. 概述

该文件实现 notify 包的具体 `Sender` 适配器，覆盖六种渠道：通用 Webhook、Slack、Feishu、DingTalk、WeCom（企业微信）、Telegram。所有渠道基于统一的 `webhookSender` 私有结构，通过 `buildBody`（payload 形状）与 `signTarget`（签名机制）两个回调差异化。签名支持：通用 webhook 的 HMAC-SHA256 header、Feishu 的 HMAC-SHA256 base64、DingTalk 的 HMAC-SHA256 URL query。

## 2. 包信息

- **包名**：`notify`
- **所属模块**：`internal/pkg/`（基础设施层）
- **依赖方向**：被 `notify.NewFromConfig` 与 DB 渠道动态构造调用；仅依赖标准库。

## 3. 关键类型与接口

### `webhookSender`（私有）
所有渠道 Sender 的统一实现。

```go
type webhookSender struct {
    name       string
    endpoint   string
    secret     string
    client     *http.Client
    buildBody  func(Message) (any, error)
    signTarget func(endpoint, secret string, body []byte) (string, map[string]string, error)
}
```

`buildBody` 决定 payload 形状（Slack attachments / Feishu text / DingTalk text 等）；`signTarget` 决定签名机制（nil 表示不签名）。

## 4. 关键函数与流程

### 渠道构造函数

| 函数 | buildBody | signTarget |
|---|---|---|
| `NewGenericWebhookSender` | 直接返回 Message | `signGenericWebhook`（HMAC-SHA256 header） |
| `NewSlackSender` | `formatSlack`（attachments） | nil（Slack incoming webhook 无 secret） |
| `NewFeishuSender` | Feishu text + timestamp + sign | nil（签名在 buildBody 内） |
| `NewDingTalkSender` | DingTalk text | `signDingTalkURL`（HMAC URL query） |
| `NewWeComSender` | WeCom text（同 DingTalk 形状） | nil（URL 即凭据） |
| `NewTelegramSender` | Telegram sendMessage | nil（token 在 URL） |

### `newWebhookSender`（私有）
统一构造，name 空 → 默认 `"webhook"`；client nil → `http.DefaultClient`。

### `webhookSender.Send`
- **签名**：`func (s *webhookSender) Send(ctx context.Context, msg Message) error`
- **流程**：
  1. `endpoint == ""` → error。
  2. `s.buildBody(msg)` 构造 payload。
  3. `json.Marshal(payload)`。
  4. `s.signTarget != nil` → 调用获取最终 endpoint + headers（可能修改 URL 加 query，如 DingTalk）。
  5. `http.NewRequestWithContext(ctx, POST, endpoint, body)`。
  6. 设置 `Content-Type: application/json` / `User-Agent: ongrid-notify/1.0` + signTarget 返回的 headers。
  7. `s.client.Do(req)`；`defer resp.Body.Close()`。
  8. 非 2xx → error `unexpected status: <status>`。
- **错误处理**：每步 `%w` 包装并加前缀（build payload / marshal / sign / new request / post）。

### `formatSlack`
- **签名**：`func formatSlack(msg Message) map[string]any`
- **职责**：把 Message 渲染为 Slack incoming-webhook attachments payload。
- **流程**：
  1. summary = `[SEVERITY] Subject`（severity 大写）。
  2. attachment：color（按 severity） / fallback / title / text（含 body） / mrkdwn_in。
  3. fields：Severity / Source / Rule / Incident (#id) / Device (#id) / Dedupe key（按 short/long 布局）。
  4. footer = "ongrid" / ts = OccurredAt.Unix()。
  5. 返回 `{text: summary, attachments: [att]}`。
- **设计理由**：attachments 格式带彩色边栏（operators 一眼判严重程度），通用兼容（Block Kit 需新 app），JSON-flat 易测试；顶层 `text` 让 Slack 通知 preview 即使剥 attachments 也有内容。

### `slackColor`
按 severity 返回 hex 颜色：critical=#d92f2f / warning=#f2c037 / info=#36a64f / 默认 #6f7a87。

### `formatText`
- **签名**：`func formatText(msg Message) string`
- **职责**：Feishu / DingTalk / WeCom / Telegram 共用的纯文本渲染。
- **流程**：`[SEVERITY] Subject` + body + `source: X` + `dedupe: X`，换行连接。

### `signGenericWebhook`
- **签名**：`func signGenericWebhook(endpoint, secret string, body []byte) (string, map[string]string, error)`
- **流程**：secret 空 → 不签名；否则 HMAC-SHA256(body) → hex → header `X-Ongrid-Signature: sha256=<hex>`。

### `signFeishu`
- **签名**：`func signFeishu(timestamp, secret string) string`
- **流程**：`stringToSign = timestamp + "\n" + secret` → HMAC-SHA256(stringToSign) → base64。
- **设计理由**：Feishu 自定义机器人签名规范要求 timestamp + 换行 + secret 作为 HMAC key。

### `signDingTalkURL`
- **签名**：`func signDingTalkURL(endpoint, secret string, _ []byte) (string, map[string]string, error)`
- **流程**：
  1. secret 空 → 不签名。
  2. ts = `time.Now().UnixMilli()`。
  3. `stringToSign = ts + "\n" + secret` → HMAC-SHA256(secret, stringToSign) → base64。
  4. `url.Parse(endpoint)` → query 加 `timestamp` + `sign` → 重组 URL。
- **设计理由**：DingTalk 自定义机器人签名把 timestamp + sign 加在 URL query 而非 header。

## 5. 依赖关系

- **内部包**：无。
- **外部库**：标准库 `bytes` / `context` / `crypto/hmac` / `crypto/sha256` / `encoding/base64` / `encoding/hex` / `encoding/json` / `fmt` / `net/http` / `net/url` / `strings` / `time`。
- **被调用方**：`notify.NewFromConfig`（env 渠道）+ manager DB 渠道动态构造。

## 6. 并发与资源管理

无显式锁。`webhookSender` 字段构造后不变；`http.Client`（或 `http.DefaultClient`）并发安全；多 goroutine 并发 Send 由连接池承载。

## 7. 设计模式与亮点

- **统一私有结构 + 双回调**：`webhookSender` 用 `buildBody` + `signTarget` 两个回调把六种渠道差异封装在一个结构内，新增渠道只需实现两个回调。
- **签名策略可插拔**：`signTarget` 是 `func(endpoint, secret, body) (endpoint, headers, error)`，签名可修改 URL（DingTalk）或加 header（generic），统一返回值形状。
- **Slack attachments 优化**：彩色边栏 + 结构化 fields + 顶层 text preview，兼顾视觉与兼容性。
- **severity → color 映射**：hex 颜色 pin 跨 Slack 客户端版本一致，避免 sentinel 颜色解析差异。
- **`formatText` 复用**：Feishu / DingTalk / WeCom / Telegram 共用纯文本渲染，减少重复。
- **nil signTarget 支持**：Slack / WeCom / Telegram 无签名，`signTarget=nil` 跳过签名步骤。
- **secret 在 URL 即凭据**：Slack / WeCom / Telegram 的 secret 在 URL / path，不在 body，注释明确说明。
- **`User-Agent: ongrid-notify/1.0`**：标识客户端便于服务端识别与统计。

## 8. 注意事项

- **无重试**：单次 POST 失败即返回 error，调用方需自行实现重试。
- **无响应体读取**：`Send` 仅检查 status code，不读 body；错误诊断信息有限。
- **`http.DefaultClient` 无 timeout**：`newWebhookSender` 默认 `http.DefaultClient`，无 timeout 可能挂死；生产应显式传 `*http.Client` 带 timeout。
- **DingTalk timestamp 毫秒**：DingTalk 要求毫秒 timestamp，与其他渠道秒级不同；签名错误时需检查时间单位。
- **Feishu timestamp 在 buildBody 内**：Feishu 把 timestamp + sign 放在 payload 而非 header，与其他渠道签名位置不一致——这是 Feishu 规范要求，非设计选择。
- **Telegram token 在 URL path**：`NewTelegramSender` endpoint 含 bot token，泄露风险高；日志 / 错误信息需注意脱敏。
- **Slack secret 静默丢弃**：`NewSlackSender` 不接收 secret 参数，注释说明 Slack incoming webhook 无 secret；若用 Slack Block Kit 或其他需要 OAuth 的 API 需另实现。
- **WeCom v1 无签名**：`NewWeComSender` 注释明确 v1 wiring 无额外签名，URL query 即凭据；若 WeCom API 升级需补签名。
- **`formatSlack` fields 顺序依赖 map 迭代**：`msg.Labels` 是 map，遍历顺序非确定；`Rule` / `Incident` / `Device` 顺序可能变化（但实际只用三个固定 key，顺序由代码决定）。
