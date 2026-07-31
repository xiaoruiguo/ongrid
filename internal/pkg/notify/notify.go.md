# `notify.go` 技术实现文档

> 源文件：`internal/pkg/notify/notify.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/notify`

## 1. 概述

该文件定义 notify 包的核心抽象：`Severity` / `Message` / `Sender` 接口 / `Router` 路由器。`Router` 把归一化 `Message` 分发到一个或多个命名 channel，控制全局 enabled / timeout / defaults。支持从 `config.NotificationConfig` 构造（env 渠道）与显式 senders 构造两种方式。disabled Router 静默丢弃消息，便于 dev / 私有部署逐步启用通知。

## 2. 包信息

- **包名**：`notify`
- **所属模块**：`internal/pkg/`（基础设施层）
- **依赖方向**：被 alerting / scheduled tasks / AIOps BC 调用；依赖 `internal/pkg/config` + 标准库。

## 3. 关键类型与接口

### `Severity`
产品级通知优先级字符串类型。

```go
type Severity string
const (
    SeverityInfo     Severity = "info"
    SeverityWarning  Severity = "warning"
    SeverityCritical Severity = "critical"
)
```

### `Message`
所有 channel 接收的归一化 payload。

```go
type Message struct {
    Subject    string
    Body       string
    Severity   Severity
    Source     string
    DedupeKey  string
    Labels     map[string]string
    OccurredAt time.Time
}
```

### `Sender`
单个 outbound channel 接口。

```go
type Sender interface {
    Name() string
    Send(ctx context.Context, msg Message) error
}
```

### `Router`
channel 路由器，捆绑 enabled / timeout / defaults / channels map。

```go
type Router struct {
    enabled  bool
    timeout  time.Duration
    defaults []string
    channels map[string]Sender
}
```

## 4. 关键函数与流程

### `NewRouter`
- **签名**：`func NewRouter(enabled bool, timeout time.Duration, defaults []string, senders ...Sender) *Router`
- **流程**：
  1. `timeout <= 0` → 默认 10s。
  2. defaults 防御性 copy。
  3. 遍历 senders：跳过 nil 或 `Name()==""`；按 Name 注册到 `channels` map（**后注册覆盖先注册**——同名 channel 后者胜）。
- **设计理由**：varargs senders 让 wiring 代码简洁；Name 唯一性由 map 自然强制。

### `NewFromConfig`
- **签名**：`func NewFromConfig(cfg config.NotificationConfig, log *slog.Logger) *Router`
- **职责**：从 env 配置构造 channel adapters。
- **流程**：依次检查 Webhook / Slack / Feishu / DingTalk：`Enabled && URL != ""` 才注册（避免 enabled-but-URL-less 渠道污染 UI）。委托 `NewGenericWebhookSender` / `NewSlackSender` / 等（见 `webhook.go`）。
- **注释留痕**：明确说明 "log" channel 2026-05 移除——manager stdout 是临时存储，`alert_events` 表才是权威审计。

### `Router.Send`
- **签名**：`func (r *Router) Send(ctx context.Context, msg Message, channels ...string) error`
- **职责**：把 msg 投递到显式 channels，未传则用 router defaults。
- **流程**：
  1. `r == nil || !r.enabled` → 静默返回 nil（disabled 不报错）。
  2. `msg.Subject == ""` → error `subject required`。
  3. `msg.Severity == ""` → 默认 `SeverityInfo`。
  4. `msg.OccurredAt.IsZero()` → `time.Now().UTC()`。
  5. channels 空 → 用 `r.defaults`。
  6. channels 仍空 → error `no channels configured`。
  7. 遍历 channels：
     - 渠道未注册 → 收集 error `channel %q not configured`。
     - 已注册 → `context.WithTimeout(ctx, r.timeout)` + `sender.Send`；失败收集 error。
  8. `errors.Join(errs...)` 返回聚合错误。
- **设计理由**：单渠道失败不阻断其他渠道；`errors.Join` 让调用方看到全部失败。

### `Router.SendVia`
- **签名**：`func (r *Router) SendVia(ctx context.Context, msg Message, sender Sender) error`
- **职责**：通过显式构造的 sender 投递（绕过 name 查找）。
- **使用场景**：DB 存储的渠道，其 `Sender` 按 row 的 ChannelType + ConfigJSON 动态构造；env-config 的 `NewFromConfig` 仅按 name 注册 env 渠道。
- **流程**：同 `Send` 的 enabled / Subject / Severity / OccurredAt 校验 + 单 sender 投递。
- **设计理由**：保留 Router 的 enabled / timeout gate，让 DB 渠道与 env 渠道行为一致。

### `Router.ChannelNames`
- **签名**：`func (r *Router) ChannelNames() []string`
- **职责**：返回已配置渠道名列表，用于 readiness 检查 / 诊断。不暴露 secret。

## 5. 依赖关系

- **内部包**：`github.com/ongridio/ongrid/internal/pkg/config`。
- **外部库**：标准库 `context` / `errors` / `fmt` / `log/slog` / `time`。
- **被调用方**：alerting usecase、scheduled tasks、AIOps proactive jobs；manager wiring 代码。

## 6. 并发与资源管理

- **无显式锁**：`Router` 字段（enabled / timeout / defaults / channels）构造后不变；`channels` map 仅读不写，并发安全。
- **每渠道独立 timeout context**：`Send` 内每个渠道用 `context.WithTimeout(ctx, r.timeout)` 创建子 context + `cancel()`，互不影响。
- **`Sender.Send` 实现自身的线程安全**：各 Sender（`webhook.go`）无共享可变状态，`http.Client` 并发安全。

## 7. 设计模式与亮点

- **归一化 Message**：所有渠道接收同一结构，渠道差异由 Sender 内部消化，业务方零感知渠道细节。
- **Router 模式**：name → Sender 路由，支持多渠道 fan-out 与显式 / 默认渠道选择。
- **disabled 静默**：`!r.enabled` 不报错返回 nil，dev / 私有部署可逐步启用通知而不需改业务代码。
- **`errors.Join` 聚合**：多渠道失败聚合返回，调用方一次看到全部错误，避免单渠道失败掩盖其他。
- **SendVia 双路径**：env 渠道按 name 查找，DB 渠道按显式 sender 投递，统一 gate。
- **防御性 copy**：`NewRouter` 对 defaults slice 做 `append([]string(nil), defaults...)`，避免外部修改影响内部。
- **Name 唯一性**：map 自然强制 name 唯一，后注册覆盖前者。
- **空 Subject 拒绝**：强制业务方提供主题，避免空通知噪音。

## 8. 注意事项

- **无重试**：单次 `Send` 失败即收集 error，不重试；业务方需自行实现重试或接受丢失。
- **无至少一次投递保证**：网络抖动 / 渠道端故障会丢消息；关键告警需配合 `alert_events` 表审计。
- **`SendVia` 不验证 sender.Name`**：`SendVia` 不查 name，直接用 sender；若同一 sender 多次注册可能混乱。
- **timeout 全局**：所有渠道共享 `r.timeout`；慢渠道（如 Telegram 大消息）可能拖累整体；需评估分渠道 timeout。
- **`ChannelNames` 不暴露 URL / secret**：注释明确"不暴露 secret"，但实现上仅返回 name slice，secret 仍可能在 Sender 内部状态可见；跨进程诊断需注意。
- **defaults 顺序非确定**：`channels` map 遍历无序，`ChannelNames` 返回顺序随机；若需稳定排序需调用方自行 sort。
- **`log` channel 移除留痕**：注释明确移除时间与原因，避免运维寻找 "log" 渠道；新增渠道类型不应再引入 log。
