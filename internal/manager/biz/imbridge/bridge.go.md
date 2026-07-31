# `bridge.go` 技术实现文档

> 源文件：`internal/manager/biz/imbridge/bridge.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/imbridge`

## 1. 概述

本文件是 imbridge 包的核心——把 IM 平台 webhook 接到现有 chat agent runtime 的桥。inbound webhook 经 provider handler 验签解密后落地为 `InboundMessage`，bridge 负责：把 IM thread 映射到 ongrid chat_session（一个 chat 一个 session，无自动轮换），驱动 agent run，SSE chunk 到达时渐进式编辑 IM 消息。核心约束：session 规则——一个 chat 一个 session，Feishu reply thread 独立，**无 idle 自动轮换**，仅 `/new` / `新会话` / `新建会话` / `重新开始` 显式重置；dedup by event id 跨 reconnect 去重；webhook caller 必须在后台 goroutine 调用 `HandleInbound`（agent run 30s+，platform ack deadline 3s）。

## 2. 包信息

- **包名**：`imbridge`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 `cmd/ongrid` main.go 装配为 singleton、HTTP webhook handler 调用；依赖 `internal/manager/biz/aiops/agent`、`internal/manager/model/imbridge`、`internal/manager/biz/setting`（间接，通过 `LLMDefaultProvider` 实现）
- **包注释**：明示 inbound webhook 经 handler 验签解密后落入此层；本层负责 thread→session 映射、agent run、SSE 渐进编辑

## 3. 关键类型与接口

```go
// Repo 是 bridge 需要的窄数据层接口；实现：data/imbridge/store.Rpo
type Repo interface {
    GetAppByAppID(ctx context.Context, provider, appID string) (*model.ImApp, error)
    FindThread(ctx context.Context, imAppID uint64, imChatID, imThreadID string) (*model.ImThread, error)
    CreateThread(ctx context.Context, t *model.ImThread) error
    TouchThread(ctx context.Context, id uint64) error
    RotateThreadSession(ctx context.Context, threadID uint64, newSessionID string) error
}

// AgentSession 是 bridge 需要的 aiops runtime 接口；实现：AiopsServiceAdapter
type AgentSession interface {
    EnsureSession(ctx context.Context, ownerUserID uint64, label string) (sessionID string, err error)
    StreamMessage(ctx context.Context, sessionID string, userContent string, emit agent.Emit) error
}

// Bridge 是注入 manager main.go 的 singleton；exported 方法由 HTTP webhook handler 调用
type Bridge struct {
    repo          Repo
    agent         AgentSession
    serviceUserID uint64            // IM-originated session 的 owner_user_id（S3 前）
    feishuCache   sync.Map          // app_id -> *feishu.Client，避免每次 inbound 重建 HTTP/token
    seen          *dedupSet         // 跨 reconnect 去重，key=(provider, app_id, event_id)
    log           *slog.Logger
}

// InboundMessage 是 bridge 可处理的标准化 inbound 事件；
// provider handler 解密解码为此形，使 bridge 平台无关
type InboundMessage struct {
    Provider      string // "feishu" | "dingtalk"
    AppID         string // 平台侧 app_id（== ImApp.AppID）
    ChatID        string // 平台 chat/conversation id
    ThreadID      string // 可选 reply thread id（Feishu root_id）
    OpenID        string // 发送者平台 user id（当前仅 log；S3 binding）
    UserName      string // 发送者显示名（仅 log）
    Text          string // 标准化 message body —— text/plain
    EventID       string // 平台 event id，用于 dedup
    ReceiveIDType string // 平台 hint："chat_id" / "open_id" / "union_id"
}

type slashCommand int
const (
    cmdNone slashCommand = iota
    cmdNew
)
```

Sentinel：`dedupSet` 容量 `2048`（`newDedupSet(2048)`）；slash command 集合 `/new` / `/newsession` / `新会话` / `新建会话` / `重新开始`；canned reply key `replyNewSession` / `replyThinking`。

## 4. 关键函数与流程

### `NewBridge`
- **签名**：`func NewBridge(repo Repo, agent AgentSession, serviceUserID uint64, log *slog.Logger) *Bridge`
- **职责**：构造 singleton；log nil → `slog.Default()`；dedupSet 容量 2048
- **流程**：log nil 检查 → 返回 `&Bridge{repo, agent, serviceUserID, seen: newDedupSet(2048), log: log.With("comp", "imbridge")}`

### `HandleInbound`
- **签名**：`func (b *Bridge) HandleInbound(ctx context.Context, sender Sender, msg InboundMessage) error`
- **职责**：解析 IM thread → ongrid session，驱动 agent stream；session 规则见上
- **流程**：
  1. `msg.Text == ""` → Debug "no text — ignoring" + return nil
  2. **Dedup**：`msg.EventID != ""` → key = `provider:appID:eventID`；`b.seen.seenOrAdd(key)` 返回 true → Debug "duplicate — skipping" + return nil。**mark on entry 而非 on success**——duplicate reply 比漏 run 一条已开始的消息更糟
  3. **解析 app**：`b.repo.GetAppByAppID(ctx, provider, appID)`；失败 `%w`；`!app.Enabled` → 报错 "app disabled"
  4. **查找 thread**：`b.repo.FindThread(ctx, app.ID, ChatID, ThreadID)`；err 含 "not found" → thread=nil；其他 err `%w`
  5. `wantNew := parseSlashCommand(msg.Text) == cmdNew`
  6. **session 分支**：
     - `thread == nil`：首次消息 → `agent.EnsureSession(serviceUserID, sessionLabel(msg))` → 构造 `model.ImThread{...}` → `repo.CreateThread`
     - `wantNew`：显式重置 → `agent.EnsureSession` 新 session → `repo.RotateThreadSession(thread.ID, sid)`（**old session row 保留供审计**）→ 更新 `thread.OngridSessionID`
     - default：`repo.TouchThread(thread.ID)`（忽略 err `_ =`——touch 失败不影响主流程）
  7. **slash 短路**：`wantNew` → `sender.SendText(localizeReply(app.DefaultLocale, replyNewSession))`（忽略 err `_, _ =`）+ return nil（**当前消息不喂 agent**，避免横跨新旧 session 边界）
  8. **placeholder**：`sender.(MessageEditor)` ok → `sender.SendText(localizeReply(replyThinking))` 拿 messageID；失败 Warn "placeholder send failed; falling back to one-shot reply"（非 Editor 的 provider 如 DingTalk 用 one-shot buffer）
  9. **run agent**：`editor := newStreamEditor(ctx, sender, ChatID, ReceiveIDType, placeholder, app.DefaultLocale, log)`；`emit := func(e agent.Event) { editor.OnEvent(e) }`；`userContent := msg.Text`；`localeDirective(app.DefaultLocale)` 非空 → `userContent += "\n\n" + directive`；`agent.StreamMessage(ctx, thread.OngridSessionID, userContent, emit)`；失败 → `editor.OnFatal(err)` + `%w`
  10. `editor.Flush()` 返回
- **错误处理**：app 解析/thread 查找/EnsureSession/CreateThread/RotateThreadSession/StreamMessage 失败均 `%w`；TouchThread / placeholder send / slash reply 失败仅忽略或 Warn——不阻塞主流程
- **并发要求**：注释明示 "Webhook callers MUST invoke this on a background goroutine: agent runs are 30s+ and the platform webhook ack deadline is 3s"

### `localeDirective`
- **签名**：`func localeDirective(locale string) string`
- **职责**：渲染附加到 user content 的语言 hint；空 locale = ""（无 directive，LLM 镜像用户语言）；与 `alert/investigator` 同名 helper 对称，但此处面向 chat reply
- **流程**：`strings.ToLower(strings.TrimSpace(locale))`；`"en"` → 英文 directive "(LANGUAGE: Respond in English...)"；`"zh"` → 中文 directive "（LANGUAGE：请用简体中文回复...）"；default → ""
- **设计意图**：注释明示 `[[feedback_ai_output_locale]]`——channel 钉住 locale 时强制 agent 用该语言，覆盖 persona/model 默认

### `localizeReply`
- **签名**：`func localizeReply(locale string, key string) string`
- **职责**：返回 canned reply 的本地化字符串；`key` ∈ {`replyNewSession`, `replyThinking`}
- **流程**：locale=="en" → 英文 canned（"✦ New session started..." / "✦ Thinking…"）；default（空/zh）→ 中文 canned（"✦ 已开启新会话..." / "✦ 思考中…"）；未知 key 返回 ""
- **设计意图**：注释明示 "Plain strings rather than i18n constants so the package stays self-contained"——imbridge 包不依赖 i18n 模块，call site 选 locale

### `sessionLabel`
- **签名**：`func (b *Bridge) sessionLabel(msg InboundMessage) string`
- **职责**：构造 ongrid session 的 Title（chat-history 页面显示）；group/DM chat_id 足够消歧；sender 不在 title 中（session 跨 sender 共享）
- **流程**：`base := fmt.Sprintf("%s · %s", Provider, shortChatLabel(ChatID))`；ThreadID 非空 → `base += " (thread)"`；返回 base

### `parseSlashCommand`
- **签名**：`func parseSlashCommand(text string) slashCommand`
- **职责**：枚举 bot 保留控制动词；当前仅 `/new`（含中文别名）
- **流程**：`strings.TrimSpace(text)`；匹配 `/new` / `/newsession` / `新会话` / `新建会话` / `重新开始` → `cmdNew`；其他 → `cmdNone`
- **扩展性**：注释明示 "Adding /show or /help later is just one branch each"

### `shortChatLabel`
- **签名**：`func shortChatLabel(id string) string`
- **职责**：chat_id 太长时取首 4 + `…` + 末 4，避免 session label 过长
- **流程**：`len(id) <= 8` → 返回 id；否则 `id[:4] + "…" + id[len(id)-4:]`

## 5. 依赖关系

- **内部包**：
  - `internal/manager/biz/aiops/agent`（`Emit` 类型、`Event` 类型）
  - `internal/manager/model/imbridge`（`ImApp`、`ImThread`）
- **同包依赖**：`dedup.go`（`dedupSet`）、`sender.go`（`Sender`、`MessageEditor`、`streamEditor`、`newStreamEditor`）、`adapter.go`（`AiopsServiceAdapter` 实现 `AgentSession`）
- **被调用方**：`cmd/ongrid` main.go 装配 `NewBridge` 为 singleton；HTTP webhook handler（`server/integration/...`）调用 `HandleInbound`

## 6. 并发与资源管理

- **`feishuCache sync.Map`**：per-provider client 缓存（app_id → `*feishu.Client`），避免每次 inbound 重建 HTTP/token 状态；sync.Map 并发安全
- **`seen *dedupSet`**：内部 `sync.Mutex` 保护两代 map；`seenOrAdd` 原子检查+插入
- **`Bridge` 字段启动期只读**：`repo`、`agent`、`serviceUserID`、`log` 启动期注入、运行期不变；`feishuCache` 与 `seen` 自带并发原语
- **`HandleInbound` 无共享状态**：每次调用独立 ctx + thread + editor；bridge 不维护 per-call 状态
- **`editor` 内部 mutex**：`streamEditor` 有 `mu sync.Mutex` + 节流（800ms / 200 字符）；`OnEvent` 在 agent loop 内同步调用，节流避免 IM API 限流
- **caller 必须 background goroutine**：注释明示 agent run 30s+，platform ack deadline 3s；webhook handler 必须 `go b.HandleInbound(...)` 后立即返回 200
- **`_ = b.repo.TouchThread`**：TouchThread err 忽略——touch 仅更新 LastSeenAt，失败不影响主流程
- **`_, _ = sender.SendText`（slash reply）**：slash 确认 send 失败忽略——session 已 rotate，下条消息走新 session

## 7. 设计模式与亮点

- **session 规则三段式**（注释明示）：
  1. Map key = `(im_app_id, im_chat_id, thread_id)`，一个 chat 一个 session，群内所有人共享；Feishu reply thread 独立
  2. **无 idle 自动轮换**——同一 mapping row 永远指向同一 ongrid session
  3. **`/new` 显式重置**——allocate 新 session + rotate pointer，old session row 保留供审计；当前消息不喂 agent（避免横跨新旧 session 边界）
- **mark-on-entry dedup**：注释明示 "marked on entry (not on success)"——duplicate reply 比漏 run 一条已开始的消息更糟；event_id 空（telegram/feishu 不应出现）跳过 dedup
- **接口在消费方定义**：`Repo`、`AgentSession` 均在 bridge.go 定义（消费方），data 层与 service 层实现之——遵循 gospec 原则，避免反向依赖
- **窄接口**：`Repo` 仅 5 方法、`AgentSession` 仅 2 方法，bridge 依赖最小化；可测试性高（fake repo + fake agent）
- **placeholder 仅 Editor provider**：注释明示 DingTalk session webhook 一次性创建消息，buffer stream 后一次性发最终答案；Feishu/Telegram/Slack 实现 MessageEditor 可渐进编辑
- **`localeDirective` 附加到 user content**：而非 system prompt——因为 persona/system prompt 可能含其他语言示例，directive 作为 user message 末尾的强约束覆盖；与 `alert/investigator` 对称
- **`localizeReply` 自包含**：注释明示 "Plain strings rather than i18n constants so the package stays self-contained"——imbridge 不依赖 i18n 模块
- **`sessionLabel` 不含 sender**：session 跨 sender 共享，title 仅 provider + chat_id 缩写 + (thread) 标记
- **`shortChatLabel` 8 字符阈值**：≤8 全显，>8 取首末各 4 + `…`；平衡可读性与长度
- **`parseSlashCommand` 可扩展**：注释明示新增 `/show` / `/help` 仅加一个 case 分支
- **`RotateThreadSession` 保留 old session**：审计可追溯；新 session 干净开始
- **`TouchThread` err 忽略**：`_ =` 显式丢弃——LastSeenAt 仅用于潜在清理，失败不阻塞
- **slash reply err 忽略**：`_, _ =`——session 已 rotate，下条消息自然走新 session；slash 确认消息送达非关键
- **`feishuCache sync.Map`**：避免每次 inbound 重建 HTTP/token 状态；sync.Map 适合读多写少场景
- **`sender` 参数化**：`HandleInbound` 接收 `Sender` 而非硬编码 Feishu——支持多 provider 复用同一 bridge

## 8. 注意事项

- **`HandleInbound` 必须 background goroutine 调用**：agent run 30s+，platform webhook ack deadline 3s（Feishu 3s、Telegram getUpdates 不算 webhook 但同理）；caller 必须 `go b.HandleInbound(...)` 后立即返回 200
- **session 规则**：一个 chat 一个 session，**无 idle 自动轮换**；仅 `/new` / `新会话` / `新建会话` / `重新开始` 显式重置；Feishu reply thread（ThreadID 非空）独立 session
- **`/new` 当前消息不喂 agent**：避免横跨新旧 session 边界；用户需重发问题
- **dedup mark on entry**：event_id 非 nil 时立即 mark；duplicate 即使前次未完成也 skip——duplicate reply 比漏 run 更糟
- **dedupSet 容量 2048**：两代 map 保留 2048-4096 最近 key；进程重启后 dedup 重置（与 Telegram offset 一起），unacked backlog 可能 reprocess——比 reconnect 罕见得多
- **`serviceUserID` 当前共享**：所有 IM session 以同一 serviceUserID 创建，所有 IM 用户共享 owner；S3 per-IM-user binding 落地后需改
- **`OpenID` 当前仅 log**：S3 binding 前不用于 ownership；注释明示
- **`TouchThread` err 忽略**：`_ =` 显式；LastSeenAt 仅用于潜在清理，失败不阻塞
- **slash reply err 忽略**：`_, _ =`；session 已 rotate，下条消息自然走新 session
- **placeholder send 失败回退 one-shot**：Warn 但不 fail；Editor provider 失败时退化为 buffer-then-send
- **`localeDirective` 仅 en/zh**：其他 locale 返回 ""，LLM 镜像用户语言；新增 locale 需加 case
- **`localizeReply` 默认中文**：空 locale / 未知 locale 走中文 canned，与历史行为一致
- **`sessionLabel` 不含 sender**：session 跨 sender 共享；title 仅 provider + chat_id 缩写 + (thread)
- **`shortChatLabel` 8 字符阈值**：≤8 全显；chat_id 通常 >8（Feishu chat_id 是 `oc_` 开头长串）
- **`feishuCache` 仅缓存 Feishu**：当前 sync.Map key 是 app_id，value 是 `*feishu.Client`；DingTalk 等其他 provider 若需缓存需扩展
- **`wantNew` 短路在 session 分支之后**：先 rotate session row，再短路 return——保证下条消息走新 session；若在 session 分支之前短路，rotation 逻辑会重复
- **`editor.Flush()` 是最后一步**：StreamMessage 成功后必须 Flush 残留 buffer；失败路径调 `editor.OnFatal(err)` 显示道歉
- **canned reply 带 `✦` 前缀**：视觉标记 bot 系统消息（非 agent 回复）；用户可区分
