# send_im_message_basetool.go

## 1. 概述

本文件实现 `send_im_message` 工具，让 assistant 主动把消息推到配置好的 IM channel（Feishu / DingTalk / Slack / Telegram / WeCom）。典型场景："把这段诊断发到运维群"。

只提供 BaseTool 形态，无闭包路径（晚于 PR-7 闭包路径清理期加入）。Class="write"——发送真实消息（side-effecting），但不是 destructive。

## 2. 包信息

- **包名**：`tools`
- **路径**：`internal/manager/biz/aiops/tools/send_im_message_basetool.go`
- **导入**：
  - `basetool`
  - `log/slog` / `strings`
- **Class**：`write`（side-effecting，非 destructive）

## 3. 关键类型与接口

### `IMChannel`

```go
type IMChannel struct {
    ID   uint64
    Name string
    Kind string  // feishu/dingtalk/slack/telegram/wecom
}
```

收窄到工具需要的字段——避免泄漏 channel config 里的 webhook URL / secret。

### `IMSender`（接口）

```go
type IMSender interface {
    ListIMChannels(ctx context.Context) ([]IMChannel, error)
    SendIM(ctx context.Context, channelID uint64, title, text string) error
}
```

channel store + notify router 的 seam。在 cmd/main.go 实现于 alert channel repo + notify.Router（同 alert notifier / flow notify node 用的 `BuildSenderFromChannel` path），让本包不依赖 data layer。

### `SendIMMessageTool`

```go
type SendIMMessageTool struct {
    sender IMSender
    log    *slog.Logger
}
```

### `sendIMMessageArgs`

```go
type sendIMMessageArgs struct {
    Channel string `json:"channel"` // 渠道名（设置→渠道）
    Text    string `json:"text"`    // 正文（纯文本，可带换行）
    Title   string `json:"title"`   // 可选标题
}
```

## 4. 关键函数与流程

### `NewSendIMMessageTool(s, log)`

`log == nil` → `slog.Default()`。

### `sendIMMessageWhenToUse`（常量）

中文 LLM-facing 文案，关键设计：

- **channel 传渠道名**（设置→渠道里配的）
- **不确定有哪些渠道时，先随便填一个调一次——报错里会列出所有可用渠道名，再据此重发**

这种"先试一次拿可用列表"的 nudge 让 LLM 不必预先知道渠道配置，第一次 miss 时 error message 自带 self-correction 信息。

### `Info(_ context.Context)`

返回 `ToolInfo`：Name=`send_im_message`，Class=`write`，含 WhenToUse。

### `InvokableRun(ctx, argsJSON, _ ...)`

1. 校验 `sender` 非 nil。
2. Unmarshal args。
3. `Channel` trim，`Channel` 或 `Text` trim 后空 → error。
4. `sender.ListIMChannels(ctx)` 拉所有渠道。
5. **case-insensitive name 匹配**：`strings.EqualFold(chans[i].Name, in.Channel)` 找 target。
6. **miss 时返回可用渠道名**：
   - 无渠道 → "no channels configured. Add one under 设置→渠道 first"
   - 有渠道但 miss → "channel %q not found. Available channels: <names>"
7. `sender.SendIM(ctx, target.ID, Title, Text)`。
8. log Info（channel + kind）。
9. Marshal `{sent: true, channel, kind}` 返回。

## 5. 依赖关系

| 方向 | 依赖 | 说明 |
|------|------|------|
| 上游 | `basetool.BaseTool` / `ToolInfo` | 实现 BaseTool 接口 |
| 下游 | `IMSender.ListIMChannels` / `SendIM` | channel store + notify router |
| 实现者 | cmd/main.go（`BuildSenderFromChannel` path） | alert channel repo + notify.Router |

## 6. 并发与资源管理

- **无 per-call timeout**：依赖外层 ctx——IM 发送可能慢（webhook 调用），但工具层不强制 timeout。
- 无共享状态，并发安全（依赖 `IMSender` 实现的线程安全）。

## 7. 设计模式与亮点

- **"先试一次拿可用列表" nudge**：WhenToUse 明示 LLM 不确定渠道时随便填一个调一次，error 里会列可用渠道名——这种 self-correction 设计让 LLM 不必预先知道配置，第一次 miss 自动学到可用列表。
- **case-insensitive name 匹配**：`strings.EqualFold` 让 LLM 写 "Ops" 或 "ops" 都能匹配 "Ops 群"——降低 LLM 出错率。
- **`IMChannel` 字段收窄**：只暴露 ID/Name/Kind，不传 webhook URL / secret——避免敏感字段泄漏到 LLM 上下文。
- **miss 时返回可用列表**：error message 自带 self-correction 信息，LLM 下一轮可以基于列表重发——比单纯 "not found" 更友好。
- **Class="write" 而非 "destructive"**：发送消息是 side-effecting 但可逆（可以追回消息或补发更正），不是不可逆操作——blast radius taxonomy 的细粒度区分。
- **`IMSender` 接口复用 alert notify path**：cmd/main.go 实现于 `BuildSenderFromChannel`（同 alert notifier / flow notify node），避免重复实现 IM 发送逻辑——single source of truth。
- **log Info 记录发送**：channel + kind 进 log，便于审计追溯（哪些渠道被 LLM 用过）。

## 8. 注意事项

- **无 per-call timeout**：IM webhook 可能慢（Feishu/DingTalk API 偶尔卡），依赖外层 ctx；若 caller 无 timeout 可能阻塞久。
- **channel 名冲突**：`EqualFold` 是 case-insensitive，若两个渠道同名（不同 case）会匹配第一个——配置时应避免同名。
- **`Text` 不裁剪**：IM 平台对消息长度有限制（Feishu ~30KB，Slack ~40KB），工具层不裁剪，依赖 IM 平台截断或报错。
- **无重试**：`SendIM` 失败直接 error，LLM 可能重试——若 IM 平台临时故障，多次重试可能撑爆对方 rate limit。
- **`Title` 可选**：部分 IM 平台（如 Slack）不支持 title，实现层可能忽略——LLM 不应假设 title 一定显示。
- **无闭包路径**：只 BaseTool 形态，意味着这个工具晚于 PR-7 闭包路径清理期加入，或从一开始就走 BaseTool。
- **Class="write" 走 ReviewGate**：作为 mutating 工具，会被 ReviewGate decorator 拦截 spawn reviewer——但发 IM 消息相对低风险，reviewer 可能直接 approve；若 reviewer 配置过严会影响 LLM 主动通知能力。
- **`Available channels` 信息泄漏**：error message 列出所有渠道名，LLM 上下文会看到——若渠道名含敏感信息（如客户名）要注意。
