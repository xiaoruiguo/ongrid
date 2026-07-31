# `router.go` 技术实现文档

> 源文件：`internal/manager/biz/alert/router.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/alert`

## 1. 概述

本文件实现 alert 子域的 `ChannelResolver`——通知渠道选择器。`DBChannelResolver` 在每次调用时枚举启用的 `notification_channels` 行，按 incident 的 severity + scope_type 过滤；无匹配时回退到构造时传入的 fallback 名单（env-seeded 默认渠道），生成 synthetic channel 行（ID=0）让 `MaybeNotify` 仍能投递。Per-rule pinning 优先：若 incident 所属 rule 的 `NotifyChannelIDsJSON` 非空，仅匹配那些 id（仍受 `Enabled` 门控），跳过全局 severity/scope 过滤——operator 显式 pin 是意图。`RuleLookup` 是可选的 func 类型，用于查询 rule 的 pinning 配置。

## 2. 包信息

- **包名**：`alert`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 `usecase.go::MaybeNotify` / `retry.go` 调用；依赖 `internal/manager/model/alert`

## 3. 关键类型与接口

```go
// ChannelResolver 为 incident 选择通知渠道。默认 DBChannelResolver 读
// notification_channels 行并应用 row-level severity/scope 过滤；测试直接 stub。
type ChannelResolver interface {
    ChannelsFor(ctx context.Context, incident *model.Incident) []*model.Channel
}

// channelLister 是 DBChannelResolver 依赖的 Repo 子集。本地重新声明
// 让 resolver 可被测试 fake 而不必满足完整 biz.Repo。
type channelLister interface {
    ListEnabledChannels(ctx context.Context) ([]*model.Channel, error)
}

// RuleLookup 加载产生 incident 的 rule。resolver 用它实现 per-rule
// notify_channel_ids 覆盖——见 model.Rule.NotifyChannelIDsJSON。
// nil-safe（resolver 在 lookup 未装配时回退全局过滤行为）。
type RuleLookup func(ctx context.Context, key string) (*model.Rule, error)

type DBChannelResolver struct {
    src      channelLister
    fallback []string
    rules    RuleLookup
}
```

`ChannelsFor` 返回 `[]*model.Channel`（非 error）——失败时回退到 synthetic fallback，不抛错让调用方处理。

## 4. 关键函数与流程

### `NewDBChannelResolver`

- **签名**：`func NewDBChannelResolver(src channelLister, fallback []string) *DBChannelResolver`
- **职责**：构造 resolver
- **流程**：
  1. 返回 `&DBChannelResolver{src, append([]string(nil), fallback...)}`
  2. fallback 拷贝防外部修改
- **错误处理**：无

### `SetRuleLookup`

- **签名**：`func (r *DBChannelResolver) SetRuleLookup(lookup RuleLookup)`
- **职责**：装配 rule lookup（可选）
- **流程**：直接赋值 `r.rules = lookup`

### `ChannelsFor`

- **签名**：`func (r *DBChannelResolver) ChannelsFor(ctx context.Context, incident *model.Incident) []*model.Channel`
- **职责**：返回匹配的 channel 行；无匹配回退 synthetic fallback
- **流程**：
  1. `r.src == nil || incident == nil` → `synthetic(r.fallback)`
  2. `rows, err := r.src.ListEnabledChannels(ctx)`；err → `synthetic(r.fallback)`
  3. **Per-rule pinning 分支**：
     - `pinned := r.pinnedIDs(ctx, incident)` 取 rule 的 `NotifyChannelIDsJSON`
     - `len(pinned) > 0`：
       - 构建 `want` set
       - 遍历 rows：`ch.Enabled` 且 `ch.ID in want` → 收集
       - 有匹配 → 返回
       - pinned 全 disabled/deleted → fall through 到全局过滤（避免通知完全丢失）
  4. **全局过滤分支**：
     - 遍历 rows：`ch.Enabled && channelMatches(ch, incident)` → 收集
     - 无匹配 → `synthetic(r.fallback)`
  5. 返回 matched
- **错误处理**：DB 失败 → synthetic fallback；pinned 全 disabled → fall through 全局过滤

### `pinnedIDs`

- **签名**：`func (r *DBChannelResolver) pinnedIDs(ctx context.Context, incident *model.Incident) []uint64`
- **职责**：取 rule 的 channel-pinning 覆盖
- **流程**：
  1. `r.rules == nil || incident == nil || incident.Rule == ""` → nil
  2. `rule, err := r.rules(ctx, incident.Rule)`；err 或 nil 或 `NotifyChannelIDsJSON` 空 → nil
  3. `json.Unmarshal(*rule.NotifyChannelIDsJSON, &ids)`；err → nil
  4. 返回 ids
- **错误处理**：任何错误返回 nil（回退全局过滤）

### `channelMatches`

- **签名**：`func channelMatches(ch *model.Channel, inc *model.Incident) bool`
- **职责**：row-level severity + scope 过滤
- **流程**：
  1. `ch.MatchSeverityMin != ""` 且 `!severityAtLeast(inc.Severity, ch.MatchSeverityMin)` → false
  2. `ch.MatchScopeTypes != ""`：splitCSV；`!contains(want, inc.ScopeType)` → false
  3. 否则 true
- **错误处理**：空字段当 wildcard

### `severityAtLeast` / `severityRank`

- **签名**：`func severityAtLeast(actual, floor string) bool` + `func severityRank(s string) int`
- **职责**：severity 阶梯比较
- **流程**：
  - `severityRank`：critical=3 / warning=2 / info=1 / default=0（unknown 当 info 最低）
  - `severityAtLeast`：`severityRank(actual) >= severityRank(floor)`
- **错误处理**：未知 severity 当 0（最低），让任何 floor 都拒绝它

### `splitCSV` / `contains` / `synthetic`

- `splitCSV(s)`：按 `,` 切分 + TrimSpace + 过滤空
- `contains(haystack, needle)`：线性扫描
- `synthetic(names)`：把 fallback 名单转成 `[]*model.Channel{Name, Enabled: true}`（ID=0）——`MaybeNotify` 处理 ID=0 时不写 delivery 行，直接走 env-config notifier by name

## 5. 依赖关系

- **内部包**：`internal/manager/model/alert`（`Channel` / `Incident` / `Rule` 类型）
- **外部库**：`context` / `encoding/json` / `strings`
- **被调用方**：`usecase.go::resolveChannels`（被 `MaybeNotify` 调用）、`retry.go`（持有 `Resolver` 但实际未用——retry 走 `channel.Name` by name）
- **依赖**：`channelLister`（`Repo` 满足）、`RuleLookup`（func 类型，典型由 `Repo.GetRuleByKey` 提供）

## 6. 并发与资源管理

- `DBChannelResolver` 无锁、无状态（`rules` 字段在 boot 时 set 一次后只读）
- 每次 `ChannelsFor` 调一次 `ListEnabledChannels` DB 读 + 可选一次 `GetRuleByKey`
- 跨 goroutine 安全：`MaybeNotify` 可能被多 goroutine 调用，`ChannelsFor` 无共享状态

## 7. 设计模式与亮点

- **接口窄化**：`ChannelResolver` 单方法 + `channelLister` 单方法——测试 fake 不必满足完整 `Repo`
- **Per-rule pinning 优先**：operator 显式 pin 是意图，跳过全局 severity/scope 过滤；pinned 全 disabled 时 fall through 全局过滤，避免通知完全丢失
- **Synthetic fallback**：无 DB 匹配时返回 ID=0 的 synthetic channel——`MaybeNotify` 对 ID=0 走 env-config notifier by name，不写 delivery 行；保留迁移窗口 + operator 逃生口
- **fail-open**：DB 失败回退 synthetic fallback，不让 resolver 故障导致告警丢失
- **`severityRank` unknown=0**：未知 severity 当最低，让任何 floor 都拒绝——保守，避免未知 severity 触发高优先级通知
- **`channelMatches` 空字段当 wildcard**：`MatchSeverityMin` 空 = 接受任何 severity；`MatchScopeTypes` 空 = 接受任何 scope
- **`pinnedIDs` 任何错误回退全局**：JSON 解析失败、rule 不存在、lookup 未装配——都回退全局过滤，不让 pinning 配置错误导致通知丢失
- **fallback 拷贝**：构造时 `append([]string(nil), fallback...)` 防外部修改

## 8. 注意事项

- **`ListEnabledChannels` 每次调用**：无缓存；高频通知场景可能 DB 压力大——典型场景每秒个位数通知，可接受
- **`pinnedIDs` 依赖 `NotifyChannelIDsJSON` 字段**：JSON 格式 `[<id>, <id>, ...]`；UI 写入时由 `usecase.go::buildRuleRow` 去重 + 校验
- **`severityRank` 仅 3 档**：critical/warning/info；error/notice/debug 等未识别值当 0——与 `usecase.go::ruleSev` / `investigator::severityRank` 不完全一致（后者 error=3）
- **Synthetic channel ID=0**：`MaybeNotify` 对 ID>0 才写 delivery 行；ID=0 走 env-config notifier by name，无审计
- **Per-rule pinning 跳过全局过滤**：operator pin 是显式意图，不受 severity/scope 限制——UI 应提示"pinning 后不受全局过滤保护"
- **`pinnedIDs` 全 disabled fall through**：避免 pinned channel 全被禁用时通知完全丢失；fall through 到全局过滤，可能命中其他 channel
- **`RuleLookup` 是 func 类型**：不是接口——`Repo.GetRuleByKey` 直接满足；测试可传闭包
- **`synthetic` 过滤空名**：fallback 名单里的空字符串被跳过
