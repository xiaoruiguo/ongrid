# graph/callbacks/alert_draft_guard.go

## 1. 概述

本文件实现 `AlertDraftGuardHandler`——一个 eino callback handler，专门用于**防止模型把"自由文字草案"误当作可确认的告警规则草案持久化**。

设计动机：ongrid 告警规则的合法草案必须来自 `draft_config_change` 工具，由后端代码生成 payload + preview + draft_hash。模型有时会在没有调用该工具的情况下，直接在 assistant 消息中输出"规则键：xxx，草案哈希：xxx，确认应用..."这类**看起来可确认但实际无 backend 支撑**的文字草案。本 handler 拦截这类消息，替换为预设的拦截提示。

附带职责：当 `draft_config_change` 成功产出 `metric_raw` 类草案且 `source_explicit` 为否时，隐藏 assistant 后续消息中泄露的 `db:`/`custom:` 采集源标识（替换为"某个数据库/自定义采集源"）。

## 2. 包信息

- **包名**：`callbacks`
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/biz/aiops/graph/callbacks`
- **角色**：eino callback handler；挂在 `compose.WithCallbacks` 链上
- **依赖**：
  - 标准库 `context`、`encoding/json`、`regexp`、`strings`、`sync/atomic`
  - `github.com/cloudwego/eino/callbacks`（Handler 接口）
  - `github.com/cloudwego/eino/components`（Component 常量）
  - `github.com/cloudwego/eino/components/model`（`einomodel.ConvCallbackOutput`）
  - `github.com/cloudwego/eino/components/tool`（`einotool.ConvCallbackOutput`）
  - `github.com/cloudwego/eino/schema`

## 3. 关键类型与接口

### `AlertDraftGuardHandler` 结构体

```go
type AlertDraftGuardHandler struct {
    userText          string
    draftSucceeded    atomic.Bool
    hideSampleSources atomic.Bool
}
```

实现 `einocallbacks.Handler` + `einocallbacks.TimingChecker` 接口。两个 atomic.Bool 标记状态。

### `AlertDraftGuardDeps` 结构体

```go
type AlertDraftGuardDeps struct {
    UserText string
}
```

构造参数，仅携带用户原始输入文本。

## 4. 关键函数与流程

### `NewAlertDraftGuardHandler(deps) *AlertDraftGuardHandler`

```go
func NewAlertDraftGuardHandler(deps AlertDraftGuardDeps) *AlertDraftGuardHandler {
    if !looksLikeAlertRuleCreationRequest(deps.UserText) {
        return nil  // 非告警规则创建请求 → 不挂 handler
    }
    return &AlertDraftGuardHandler{userText: deps.UserText}
}
```

返回 nil 是关键设计——非告警创建请求不挂本 handler，避免无谓开销。

### `Needed(_ ctx, info, timing) bool`

仅响应 `ComponentOfChatModel` 和 `ComponentOfTool` 的 `TimingOnEnd`。其他阶段一律返回 false。

### `OnEnd(ctx, info, output) context.Context`

核心逻辑分两路：

```go
switch info.Component {
case components.ComponentOfTool:
    // draft_config_change 成功 → 设 draftSucceeded=true
    // 若 draftResultShouldHideSampleSources → 设 hideSampleSources=true
case components.ComponentOfChatModel:
    // 若消息调用了 draft_config_change 工具 → 不处理（让工具先跑）
    // 否则 → mo.Message.Content = SanitizeAssistantContent(content)
}
```

### `SanitizeAssistantContent(content) string`

公开方法，三态决策：

```go
func (h *AlertDraftGuardHandler) SanitizeAssistantContent(content string) string {
    if h.draftSucceeded.Load() {
        if h.hideSampleSources.Load() {
            return sanitizeCollectedSampleSourceMentions(content)
        }
        return content  // 草案成功，无需拦截
    }
    if !looksLikeConfirmableAlertDraft(content) {
        return content  // 不像可确认草案，放行
    }
    return alertDraftGuardBlockedMessage  // 拦截！
}
```

### `looksLikeAlertRuleCreationRequest(text) bool`

判断用户输入是否是告警规则创建请求（中英文关键词双重识别）：

第一组（动作词，必命中其一）：
- 中文：创建/新增/添加/配置/生成
- 英文：create/add/configure/new/generate

第二组（领域词，必命中其一）：
- 中文：告警/告警规则/监控规则/规则/链路/日志
- 英文：alert/rule/monitoring rule/slo/burn rate/metric/log/trace

两组都命中才返回 true。

### `looksLikeConfirmableAlertDraft(content) bool`

判断 assistant 消息内容是否"看起来像可确认的告警草案"。三层判定：

1. **强信号**：含 `config_draft` 或 `apply_config_change` → 直接 true
2. **领域 + 确认词**：含告警/规则键/规则设计/规则 key/alert/rule_key/draft_hash 之一 + 含"草案哈希/确认应用/告警规则草案/draft_hash/sha256:/config_draft/apply_config_change"之一 → true
3. **规则设计 + 确认请求**：含"规则设计/规则 key/promql/触发条件/生效范围"之一 + 含"需要确认/确认创建/要确认/确认吗/confirm/approve"之一 → true

### `draftResultShouldHideSampleSources(result) bool`

判断 `draft_config_change` 工具结果是否应该触发"采集源隐藏"行为：

1. JSON 解析 `payload.rule.kind` 必须是 `metric_raw`
2. `spec.source_explicit` 不为真（`boolish` 判断 true/yes/y/1/explicit/user/specified/requested）
3. `spec` 中无任何 `expr`/`promql`/`query`/`selector`/`label_selector` 字段含显式 source matcher（`ongrid_source="`/`device_id="`/`instance="`/`service="`）

三者都满足才返回 true。

### `sanitizeCollectedSampleSourceMentions(content) string`

```go
return collectedSourceMentionRE.ReplaceAllStringFunc(content, func(match string) string {
    if strings.HasPrefix(match, "custom:") {
        return "某个自定义采集源"
    }
    return "某个数据库采集源"
})
```

正则 `\b(?:db|custom):[A-Za-z0-9_.:-]+` 匹配 `db:xxx` 或 `custom:xxx` 形式的采集源标识。

### 辅助函数

- `messageCallsTool(msg, toolName)`：检查消息是否调用了指定工具
- `LooksLikeAlertDraftGuardBlockedMessage(content)`：识别本 handler 自己产出的拦截消息（含 `draft_config_change` + `config_draft` + "拦截文字草案"/"没有通过 draft_config_change"/"我还没有通过 draft_config_change"）
- `AlertDraftGuardFromHandlers(handlers)`：从 handler 列表中找出本类型实例
- `LooksLikeAlertRuleCreationText(text)`：导出包装，供外部使用
- `containsAnyNeedle`、`boolish`、`containsExplicitSourceMatcher`、`toolResultLooksLikeConfigDraft`：辅助判定

## 5. 依赖关系

### 上游
- `callbacks/chain.go::NewDefaultHandlers` 装配本 handler 到默认链
- 外部代码通过 `AlertDraftGuardFromHandlers` 从 handler 列表取出本实例，调用 `SanitizeAssistantContent`

### 下游
- `einocallbacks.Handler`/`TimingChecker` 接口
- `einomodel.ConvCallbackOutput`/`einotool.ConvCallbackOutput`：从 eino callback output 反序列化

## 6. 并发与资源管理

### `atomic.Bool` 状态保护

`draftSucceeded` 和 `hideSampleSources` 使用 `atomic.Bool`，可在多个 goroutine 并发调用 `OnEnd` 时安全读写（tool fan-out 可能并发）。

### 无锁读取 userText

`userText` 在构造时确定后只读，无需加锁。

### 单次 graph run 一个实例

注释明确："One handler instance per graph run." 每个 graph run 创建一个新实例，状态天然隔离。

## 7. 设计模式与亮点

### 状态机：draftSucceeded + hideSampleSources

handler 维护两个原子布尔状态：

| draftSucceeded | hideSampleSources | SanitizeAssistantContent 行为 |
|---|---|---|
| false | - | 内容像可确认草案 → 拦截 |
| true | false | 放行 |
| true | true | 隐藏 db:/custom: 采集源 |

三态决策在 `SanitizeAssistantContent` 中明确表达。

### 短路设计：非创建请求返回 nil

```go
if !looksLikeAlertRuleCreationRequest(deps.UserText) {
    return nil
}
```

返回 nil 让 `NewDefaultHandlers` 跳过本 handler，避免非告警请求承担无谓的 OnEnd 调用开销。这是性能优化的关键设计。

### 双语关键词识别

`looksLikeAlertRuleCreationRequest` 和 `looksLikeConfirmableAlertDraft` 都同时识别中英文关键词。`containsAnyNeedle` 接受 `(text, lowerText, needles)` 双形式参数，对每个 needle 同时检查原文和 lower-case 形式——容忍大小写混合。

### `boolish` 宽松布尔解析

```go
case string:
    switch strings.ToLower(strings.TrimSpace(v)) {
    case "true", "yes", "y", "1", "explicit", "user", "specified", "requested":
        return true
    }
```

容忍 LLM 在 `source_explicit` 字段填各种"是"的变体（包括自然语言"explicit"/"user"/"specified"/"requested"）。这是与 LLM 输出打交道的现实工程实践。

### 工具结果 JSON 解析的容错

`toolResultLooksLikeConfigDraft` 有两条路径：
1. 严格 JSON 解析 `kind` + `draft_hash`
2. 失败则 fallback 到字符串包含检查（lower-case 含 `config_draft` + `draft_hash`）

→ 容忍工具结果格式不规范的情况。

### 采集源隐藏的语义

`metric_raw` 类草案且 `source_explicit` 为否时，意味着采集源是模型从样本数据推断的，不应在面向用户的 assistant 消息中明文暴露（可能泄露内部拓扑）。隐藏为"某个数据库/自定义采集源"是脱敏处理。

## 8. 注意事项

### 拦截消息是硬编码常量

```go
const AlertDraftGuardBlockedMessage = "这次没有通过 draft_config_change 生成可应用的 config_draft/draft_hash，我已拦截文字草案..."
const alertDraftGuardBlockedMessage = AlertDraftGuardBlockedMessage
```

同时有导出和未导出两个常量指向同一字符串。`alertDraftGuardBlockedMessage`（小写）用于内部使用，`AlertDraftGuardBlockedMessage`（大写）供外部识别。`LooksLikeAlertDraftGuardBlockedMessage` 用于检测这一拦截消息。

### `looksLikeConfirmableAlertDraft` 的误判风险

判定逻辑依赖关键词匹配，可能误判：
- 模型解释"什么是 config_draft"可能命中强信号
- 模型讨论"规则设计"和"是否需要确认"可能命中第三层

→ 当前通过三层叠加降低误判，但仍非完美。注释未提及误判率统计。

### `OnStart` / `OnError` 是 no-op

本 handler 只在 `OnEnd` 工作，其他回调方法返回 ctx 不做任何事。这是 `Needed` 限定 `TimingOnEnd` 的体现。

### `OnEnd` 改写 `mo.Message.Content` 是 mutation

```go
mo.Message.Content = h.SanitizeAssistantContent(mo.Message.Content)
```

直接修改 callback output 中的 message content。eino callback 系统允许这一行为——后续 handler 会看到改写后的 content。但若上游 handler 已经基于原 content 做了持久化，会出现不一致。装配顺序在 `chain.go` 中需要谨慎。

### Stream 路径未实现

`OnStartWithStreamInput`/`OnEndWithStreamOutput` 是 no-op（仅返回 ctx）。streaming 模式下本 handler 不工作——这是一个待补完的缺口。

### `draftConfigChangeToolName` 常量耦合

`const draftConfigChangeToolName = "draft_config_change"` 与 `tool_adapter.go::toolNameDraftConfigChange` 是同一字符串。两处独立定义，未共享常量——存在改名漂移风险。
