# `model.go` 技术实现文档

> 源文件：`internal/manager/model/imbridge/model.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/model/imbridge`

## 1. 概述

本文件是 multi-turn IM bridge 的 storage schema：`ImApp`（IM bot 应用注册）+ `ImThread`（IM 会话 → ongrid chat_session 映射）。ImApp 持有凭据 + flags（支持 Feishu / DingTalk / Telegram / Slack 四 provider，Stream / Webhook 两种模式）；ImThread 把一个 IM 会话（group/DM/reply thread）映射到一个 ongrid chat_session，使同会话消息流入同一 agent conversation。设计要点：会话映射 key 是 (im_app_id, im_chat_id, im_thread_id)，按 chat 共享一个 session（O(active chats) 而非 O(users × chats × time)）；仅显式 `/new` 才 rotate。红线：Telegram 公开 bot 必须有 AllowFrom 白名单（ADR-031，防"任何人可达平台"风险）；AppSecret 等敏感字段由 SystemSetting reveal/store 流加密落盘。

## 2. 包信息

- **包名**：`imbridge`
- **所属模块**：`internal/manager/model/`
- **依赖方向**：被 `manager/biz/imbridge` 的 StreamSupervisor / webhook handler 调用；依赖 `gorm.io/plugin/soft_delete`、`time`

## 3. 关键类型与接口

```go
// Provider 常量
const (
    ProviderFeishu   = "feishu"
    ProviderDingTalk = "dingtalk"
    ProviderTelegram = "telegram" // stream-only: getUpdates long-poll
    ProviderSlack    = "slack"    // stream-only: Socket Mode WebSocket
)

// Mode 常量
const (
    ModeStream  = "stream"  // 默认，manager 拨出 WebSocket
    ModeWebhook = "webhook" // 经典 HTTP callback + 签名
)

type ImApp struct {
    ID       uint64 `gorm:"primaryKey;autoIncrement"`
    Provider string `gorm:"size:16;not null;uniqueIndex:uk_provider_app_id,priority:1"`
    Mode        string `gorm:"size:16;not null;default:stream"`
    Name        string `gorm:"size:128;not null"`
    AppID       string `gorm:"column:app_id;size:128;not null;uniqueIndex:uk_provider_app_id,priority:2"`
    AppSecret   string `gorm:"column:app_secret;type:text;not null"`
    VerifyToken string `gorm:"column:verify_token;size:128"`
    EncryptKey  string `gorm:"column:encrypt_key;size:128"`
    AllowFrom string `gorm:"column:allow_from;type:text"` // Telegram 必填
    IdleTimeoutSeconds int `gorm:"column:idle_timeout_seconds;not null;default:0"` // legacy 未用
    DefaultLocale string `gorm:"column:default_locale;size:8;not null;default:''"` // "en"/"zh"
    Enabled       bool   `gorm:"not null;default:true"`
    CreatedAt     time.Time
    UpdatedAt     time.Time
    DeletedAt     *time.Time            `gorm:"index;column:deleted_at"`
    DeleteMarker  soft_delete.DeletedAt `gorm:"column:delete_marker;not null;default:0;softDelete:milli,DeletedAtField:DeletedAt;uniqueIndex:uk_provider_app_id,priority:3"`
}

type ImThread struct {
    ID         uint64 `gorm:"primaryKey;autoIncrement"`
    ImAppID    uint64 `gorm:"column:im_app_id;not null;uniqueIndex:uk_app_chat,priority:1;index:idx_app_chat"`
    Provider   string `gorm:"size:16;not null"`
    ImChatID   string `gorm:"column:im_chat_id;size:128;not null;uniqueIndex:uk_app_chat,priority:2"`
    ImThreadID string `gorm:"column:im_thread_id;size:128;uniqueIndex:uk_app_chat,priority:3"`
    ImSenderID      string `gorm:"column:im_sender_id;size:128"` // 审计，非 unique key
    OngridSessionID string `gorm:"column:ongrid_session_id;size:128;not null;index:idx_session"`
    LastSeenAt      time.Time
    CreatedAt       time.Time
    UpdatedAt       time.Time
}
```

## 4. 关键函数与流程

### `ImApp.TableName`
- **签名**：`func (ImApp) TableName() string`
- **职责**：固定表名 `im_apps`

### `ImThread.TableName`
- **签名**：`func (ImThread) TableName() string`
- **职责**：固定表名 `im_threads`

## 5. 依赖关系

- **内部包**：`aiops` 包（通过 OngridSessionID 反查 chat_sessions）
- **外部库**：`gorm.io/plugin/soft_delete`、`time`
- **被调用方**：`manager/biz/imbridge` 的 StreamSupervisor（stream 模式）与 webhook handler（webhook 模式）

## 6. 并发与资源管理

- 本文件仅定义 schema，无锁 / channel / 缓存
- `ImApp.DeleteMarker` 加入 unique index 让软删后可重建同 (provider, app_id)
- `ImThread` 无软删（永久审计 / 历史会话映射）

## 7. 设计模式与亮点

- **Stream vs Webhook 模式**：Stream 是私有云首选（manager 拨出 WebSocket-over-TLS，proxy-friendly）；Webhook 是经典 HTTP callback + 签名（出站受限防火墙场景）
- **Provider-specific transport**：
  - Telegram：getUpdates long-poll（无 webhook 路径）
  - Slack：Socket Mode WebSocket（需 app_token + bot_token 两个 token，存为 JSON）
  - Feishu/DingTalk：双向支持
- **AllowFrom Telegram 强制白名单**：公开 bot 无白名单 = "任何人可达平台"风险；空 = deny-all；Feishu/DingTalk 由企业租户成员鉴权
- **ImThread 按 chat 共享 session**：bot 是房间共享助手，非 per-user agent；行增长 O(active chats)
- **ImSenderID 仅审计**：不参与 unique key；同一 chat 多人发言都进同一 ongrid session
- **不自动 rotate**：idle timeout 不再触发；仅显式 `/new` 才新建 session（防 chatty channel 行爆炸增长）
- **DefaultLocale 语言指令**：每条用户消息前 bridge 追加；Slack workspace 可设 en 让 LLM 用英文回（即便 persona 是中文）
- **IdleTimeoutSeconds legacy 保留**：当前未用；未来"长对话上下文窗口"可能作为 soft cap 重启用
- **AppSecret 加密落盘**：由 SystemSetting reveal/store 流加密；从不明文返回

## 8. 注意事项

- **(provider, app_id) 唯一**：跨未软删行 + DeleteMarker 联合唯一
- **AppSecret 必填**：AppID/AppSecret/VerifyToken/EncryptKey 视 provider 不同必填性
- **Slack AppSecret 是 JSON**：`{"app_token":"xapp-...","bot_token":"xoxb-..."}`
- **Telegram AllowFrom**：逗号/空格/换行分隔的数字 Telegram user id；空 = deny-all（必须显式配置）
- **DefaultLocale 取值**：仅 "en"/"zh"；空 = 无指令（LLM 跟随用户）
- **ImThread.ImThreadID 可空**：DM / 群聊顶层无 thread；回复 thread 才填
- **OngridSessionID 必填**：建映射时由 biz 层预生成 UUID
- **ImThread 无软删**：永久保留历史映射；如需清理另起 retention
- **IdleTimeoutSeconds 默认 0**：无行为；保留仅作 legacy 兼容
