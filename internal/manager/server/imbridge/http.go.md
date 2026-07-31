# `http.go` 技术实现文档

> 源文件：`internal/manager/server/imbridge/http.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/server/imbridge`

## 1. 概述

本文件是 IM（即时通讯）bridge 的 HTTP 层：暴露两类路由——public webhook（`/v1/im/{provider}/events`，平台签名鉴权）+ protected admin CRUD（`/v1/im/apps`，bearer + admin）。设计要点：webhook 端点**在 bearer-auth group 之外**，因为 IM 平台无法携带 manager JWT，鉴权靠平台签名方案；Feishu webhook 有 3 秒 ack deadline，agent run 在后台 goroutine 跑。关键红线：app_secret 在 list 只返 `has_secret=true`，`reveal` 端点才返明文（避免每次 page render 记录 secret）；webhook 收到非 message 事件静默 ack。

## 2. 包信息

- **包名**：`imbridge`
- **所属模块**：`internal/manager/server/`（HTTP handler 层）
- **依赖方向**：被上层 router 装配（public + protected 两组）；依赖 `biz/imbridge`、`biz/imbridge/provider/feishu`、`iam/model`（RoleAdmin）、`model/imbridge`、`pkg/errs`、`pkg/tenantctx`

## 3. 关键类型与接口

```go
type AppRepo interface {
    GetAppByAppID(ctx, provider, appID string) (*model.ImApp, error)
}

type Handler struct {
    bridge *bizbridge.Bridge
    apps   AppRepo
    uc     *bizbridge.UC
    log    *slog.Logger
}

// Feishu event envelope (v2)
type feishuEnvelope struct {
    Encrypt   string                 `json:"encrypt"`   // 加密载荷
    Schema    string                 `json:"schema"`
    Type      string                 `json:"type"`      // url_verification only
    Challenge string                 `json:"challenge"` // url_verification only
    Token     string                 `json:"token"`
    Header    map[string]interface{} `json:"header"`    // 解密后填充
    Event     map[string]interface{} `json:"event"`
}

type feishuChallenge struct {
    Challenge string `json:"challenge"`
}

// admin CRUD DTOs
type appDTO struct {
    ID uint64
    Provider, Mode, Name, AppID string
    HasSecret bool // list 不返明文 secret
    VerifyToken, EncryptKey, AllowFrom, DefaultLocale string
    Enabled bool
    IdleTimeoutSeconds int
    CreatedAt, UpdatedAt string
}

type appPayload struct { /* 创建/更新入参 */ }
```

## 4. 关键函数与流程

### `NewHandler`
- **签名**：`func NewHandler(bridge, apps, uc, log) *Handler`
- **职责**：构造；log nil 回退 Default，附 `comp=imbridge.http`

### `RegisterPublic` / `RegisterProtected`
- **职责**：分别挂 public webhook + protected admin 路由
- **流程**：
  - public：`POST /v1/im/feishu/events`（dingtalk 在 S2）
  - protected：`GET/POST/PUT/DELETE /v1/im/apps`、`GET /v1/im/apps/{id}`、`POST /v1/im/apps/{id}/reveal`

### `requireAdmin`
- **签名**：`func requireAdmin(w, r) bool`
- **职责**：admin gating（IM app config 暴露 app_secret/encryption key，连读都 admin-only）

### `handleFeishuEvent`
- **签名**：`func (h *Handler) handleFeishuEvent(w, r)`
- **职责**：处理 Feishu webhook 事件
- **流程**：
  1. `io.ReadAll(r.Body)` → close body
  2. unmarshal `feishuEnvelope`；失败 → 400「bad json」
  3. **URL verification handshake**：`env.Type == "url_verification"` → 200 echo challenge
  4. **解析 app_id**：优先 `X-Lark-Source-App` header；fallback `env.Header["app_id"]`（plaintext 载荷）；都空 → 400「missing app_id」
  5. `h.apps.GetAppByAppID(ctx, ProviderFeishu, appID)`；失败 → 401「unknown app」
  6. **签名校验**（仅 `app.EncryptKey != ""`）：取 `X-Lark-Request-Timestamp`/`Nonce`/`Signature` header → `feishu.VerifyEventSignature`；失败 → 401「bad signature」
  7. **解密**（仅 `env.Encrypt != ""`）：`feishu.DecryptEvent` → 再 unmarshal；解密后可能再次是 `url_verification`，二次 echo challenge
  8. `extractFeishuMessage(env)` 提取 (chat_id, message_id, text)；非 message 事件 → 200 ack「status: ok」
  9. **后台 goroutine 跑 agent**：`go func() { h.bridge.HandleInbound(ctx, feishuSender, in) }()`——避免超过 Feishu 3 秒 ack deadline 导致重试
  10. 立即 200 ack「status: ok」
- **关键**：3 秒 ack deadline 是硬约束，必须先 ack 再处理

### `extractFeishuMessage`
- **签名**：`func extractFeishuMessage(env feishuEnvelope) (bizbridge.InboundMessage, bool)`
- **职责**：从 envelope 提取 (chat_id, message_id, text)
- **流程**：
  1. `env.Header["event_type"] == "im.message.receive_v1"` 否则返 false
  2. 提取 `event_id`、`chat_id`、`root_id`（thread）、`message_type`
  3. **仅支持 text**：`message_type != "text"` → false（cards/files/audio 静默丢弃）
  4. 解析 content JSON string `{text: "..."}`
  5. 提取 `sender.sender_id.open_id`
  6. `ReceiveIDType = "chat_id"`（Feishu 模型中 DM 也是 1:1 chat）
- **错误处理**：浅 type-assertion，平台可能加字段

### `feishuSender`
- **职责**：适配 `feishu.Client` 到 `bridge.Sender` 接口
- **方法**：`SendText` / `EditText`

### admin CRUD（`listApps` / `getApp` / `createApp` / `updateApp` / `deleteApp` / `revealAppSecret`）
- **职责**：IM app 配置管理
- **流程**：每个都 `requireAdmin` gating；`listApps` 仅返 `has_secret=true` 不返明文；`revealAppSecret` 返明文 `app_secret`（镜像 SystemSetting reveal 流）
- **错误处理**：用 `http.Error` 而非 `writeErr`（slug 表更简单）

### `parseIDFromURL`
- **签名**：`func parseIDFromURL(r) (uint64, bool)`
- **职责**：手写 digit 解析（避免 import strconv）；非数字字符 → false；0 → false

### `writeJSON`
- 标准 helper

## 5. 依赖关系

- **内部包**：
  - `biz/imbridge`（`Bridge`、`UC`、`InboundMessage`、`AppInput`）、`biz/imbridge/provider/feishu`（`Client`、`VerifyEventSignature`、`DecryptEvent`）
  - `iam/model`（`RoleAdmin`）
  - `model/imbridge`（`ImApp`、`ProviderFeishu`）
  - `pkg/errs`、`pkg/tenantctx`
- **外部库**：`github.com/go-chi/chi/v5`、`log/slog`
- **被调用方**：上层 router 装配（public + protected 两组）

## 6. 并发与资源管理

- **后台 goroutine 跑 agent**：`go func() { h.bridge.HandleInbound(...) }()`——避免超过 Feishu 3 秒 ack deadline
- **`context.Background()` 用于后台**：webhook 请求 ctx 在 200 ack 后取消，agent run 需独立 ctx
- **无共享状态**：Handler 字段只读
- **无锁**：只读字段

## 7. 设计模式与亮点

- **Webhook 在 bearer-auth group 之外**：IM 平台无法携带 manager JWT，鉴权靠平台签名方案
- **3 秒 ack deadline 应对**：agent run 在后台 goroutine 跑，立即 200 ack；避免 Feishu 重试导致重复处理
- **app_id 解析双路径**：优先 `X-Lark-Source-App` header（加密载荷也带），fallback `env.Header["app_id"]`（plaintext 载荷）——因为需先验签才能解密，但验签需 secret，secret 需 app_id
- **解密后二次 challenge 检查**：注释明示「Feishu has done this in the past after rotating formats — be defensive」
- **`has_secret` 不返明文**：list 仅返布尔，`reveal` 端点才返明文——避免每次 page render 记录 secret
- **仅支持 text 消息**：S1 范围，cards/files/audio 静默丢弃
- **浅 type-assertion**：注释明示「platform reserves the right to add fields」——不严格 unmarshal
- **`parseIDFromURL` 手写**：避免 import strconv 仅为此一处

## 8. 注意事项

- **`requireAdmin` 用 `http.Error`**：与其它 handler 的 `writeErr` 不一致，slug 信息丢失；admin CRUD 错误响应格式较简单
- **后台 goroutine 用 `context.Background()`**：webhook 请求 ctx 在 ack 后取消，agent run 需独立 ctx；若 manager 关闭时仍有 in-flight agent run 可能丢失
- **`extractFeishuMessage` 仅 text**：S1 限制，cards/files/audio 静默丢弃；S2+ 需扩展
- **签名校验仅当 `EncryptKey != ""`**：plaintext 模式（无 encrypt_key）不验签，安全性弱——注释明示「weaker security, but matches Feishu docs "non-encrypted mode"」
- **`dingtalk` handler 在 S2**：当前仅 Feishu
- **`revealAppSecret` 无审计**：未调 `auditmw.SetAuditEvent`，明文 secret 查询不留审计痕迹（潜在合规风险）
- **`parseIDFromURL` 手写 digit 解析**：非数字字符返 false，0 返 false；与其它 handler 的 `parseID` 不一致
- **`var _ = errors.New`**：防 unused import 的 guard，若未来重构掉 Feishu 保留 errors import
