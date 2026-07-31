# graph/budget_stop_model.go

## 1. 概述

本文件实现 `budgetStopModel`——一个透明包装器，套在真实 `einomodel.ToolCallingChatModel` 之外，用于在工具调用预算耗尽时**短路 LLM 调用**，直接返回一条预先合成的"基于已收集证据作答"的 final-answer 消息。

设计目的：防止 ReAct loop 在 `query_promql`、`query_alert_rules` 等工具被 `maxToolCallsPerRun` 截断后，模型仍然继续 ChatModel 循环、试图调用其他替代工具走同一条查询路径。一旦工具预算耗尽并发出 `call_budget_exceeded`+`final_answer_required` 的合成结果，`budgetStopModel` 检测到这一信号后立即终止循环。

## 2. 包信息

- **包名**：`graph`
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/biz/aiops/graph`
- **角色**：LLM 模型包装器；`BuildReActGraph` 内部使用
- **依赖**：
  - 标准库 `context`、`encoding/json`、`strings`
  - `github.com/cloudwego/eino/components/model`（`einomodel.ToolCallingChatModel` 接口）
  - `github.com/cloudwego/eino/schema`（`schema.Message`、`schema.StreamReaderFromArray`）

## 3. 关键类型与接口

### `budgetStopModel` 结构体（未导出）

```go
type budgetStopModel struct {
    inner einomodel.ToolCallingChatModel
}
```

仅持有 inner model 引用，无其他状态。实现 `einomodel.ToolCallingChatModel` 接口（`Generate`/`Stream`/`WithTools` 三个方法）。

### `toolBudgetEnvelope` 结构体（未导出）

```go
type toolBudgetEnvelope struct {
    Status      string `json:"status"`
    Tool        string `json:"tool"`
    FinalAnswer bool   `json:"final_answer_required"`
}
```

解析工具返回的 `call_budget_exceeded` 合成 JSON。`FinalAnswer=true` 才触发短路。

## 4. 关键函数与流程

### `wrapBudgetStopModel(inner) einomodel.ToolCallingChatModel`

构造包装器。`inner=nil` 时返回 nil（防御性）。

### `Generate` / `Stream` / `WithTools`

```go
func (m *budgetStopModel) Generate(ctx, input, opts...) (*schema.Message, error) {
    if msg, ok := finalAnswerAfterToolBudget(input); ok {
        return msg, nil  // 短路，不调用 inner
    }
    return m.inner.Generate(ctx, input, opts...)
}
```

`Stream` 用 `schema.StreamReaderFromArray([]*schema.Message{msg})` 把合成消息包成单元素 stream。`WithTools` 递归包装 next model，确保预算停止行为在 `react.NewAgent` 调用 `WithTools` 后仍生效。

### `finalAnswerAfterToolBudget(messages) (*schema.Message, bool)`

合成 final-answer 消息：

1. 调用 `latestTerminalToolBudget(messages)` 取最近的终止信号
2. 未取到 → 返回 `(nil, false)`，正常继续
3. 取到 → 根据消息历史是否含 "respond in english" 选择中文或英文模板
4. 返回 `(assistantMessage, true)`，模型层直接吐回这条消息结束循环

中文模板：
> 本轮 `<tool>` 查询已经达到安全上限。我会停止继续调用工具，基于已经拿到的结果回答：当前证据不足以继续细分这条查询路径；如果前面的结果为空或报错，请检查查询标签/语法/数据源配置，或在下一条消息给出更具体的时间窗、service、device_id 后再查。

### `latestTerminalToolBudget(messages) (toolBudgetEnvelope, bool)`

**从后往前**扫描消息序列，识别最近的 `call_budget_exceeded` + `final_answer_required` 信号：

```go
for i := len(messages) - 1; i >= 0; i-- {
    msg := messages[i]
    if msg.Role == schema.User && !isSystemReminderMessage(msg.Content) {
        return zero, false  // 遇到真实用户消息，停止扫描
    }
    if msg.Role != schema.Tool {
        continue
    }
    var env toolBudgetEnvelope
    if err := json.Unmarshal([]byte(msg.Content), &env); err != nil {
        continue
    }
    if env.Status == "call_budget_exceeded" && env.FinalAnswer {
        return env, true
    }
}
return zero, false
```

**关键边界**：
- 遇到真实 User 消息即停止扫描——预算停止只对"当前用户轮次"生效，新用户消息重置预算
- `<system-reminder>` 块也是 User role 但用 `isSystemReminderMessage` 排除，不视作真实用户输入
- 仅 Tool role 消息才可能携带 budget envelope

### `wantsEnglishResponse(messages) bool`

从后往前扫，找 System/User 消息中是否含 "respond in english" 字符串。决定合成消息用中文还是英文模板。

### `isSystemReminderMessage(content) bool`

```go
trimmed := strings.TrimSpace(strings.ToLower(content))
return strings.HasPrefix(trimmed, "<system-reminder>")
```

## 5. 依赖关系

### 上游
- `graph/react.go::BuildReActGraph` 在构造内层 ReAct agent 前调用 `wrapBudgetStopModel(model)` 包装
- `graph/tool_adapter.go::toolBudgetExceeded` 产出触发本包装器的合成 JSON

### 下游
- `einomodel.ToolCallingChatModel` 接口（cloudwego/eino）
- `schema.Message`/`schema.StreamReader`（cloudwego/eino）

## 6. 并发与资源管理

- **无状态、无锁**：`budgetStopModel` 仅持有 inner 引用，每次 `Generate`/`Stream` 都是无状态读取
- **无 goroutine**：合成消息是同步构造，stream 用 `StreamReaderFromArray` 同步包装
- **无资源释放**：不持有需要 Close 的资源

## 7. 设计模式与亮点

### 透明装饰器（Transparent Decorator）

`budgetStopModel` 满足 `einomodel.ToolCallingChatModel` 接口，对调用方完全透明。`react.NewAgent` 不知道也不需要知道它实际拿到的是被装饰过的 model——这是经典的装饰器模式，让 ReAct agent 在不修改源码的前提下获得"预算停止"行为。

### `WithTools` 递归保留装饰

```go
func (m *budgetStopModel) WithTools(tools) (einomodel.ToolCallingChatModel, error) {
    next, err := m.inner.WithTools(tools)
    if err != nil {
        return nil, err
    }
    return &budgetStopModel{inner: next}, nil
}
```

`react.NewAgent` 会调用 `WithTools` 把工具声明注入 model；如果 `WithTools` 不递归包装返回的 next model，预算停止行为就会丢失。这是装饰器链的关键细节。

### 预算作用域：current_user_turn

`latestTerminalToolBudget` 遇到真实 User 消息立即返回 false——明确语义是"预算只在当前用户轮次内生效，新用户消息重置预算"。这一语义与 `tool_adapter.go` 合成 `call_budget_exceeded` 时填的 `"scope":"current_user_turn"` 字段一致。

### 双语模板

合成消息支持中英文，依据消息历史中的语言指令选择。`wantsEnglishResponse` 扫描 System/User 消息里的 "respond in english" 字符串——这是 `react.go::languageDirective` 注入的指令文本。

## 8. 注意事项

### 短路时机：在 ChatModel 调用前

`Generate`/`Stream` 在调用 `m.inner` 前先做 budget 检测，命中则直接返回合成消息。意味着模型不会被实际调用、不消耗 token——这是 budget stop 的核心成本节约点。

### 仅响应 `final_answer_required=true`

`tool_adapter.go::toolBudgetExceeded` 总是把 `FinalAnswer` 填为 true，所以正常路径必然触发短路。但 envelope schema 保留 `final_answer_required` 字段是为了未来扩展（例如非终止型预算提示）——本文件只响应终止型。

### System-reminder 识别的脆弱性

`isSystemReminderMessage` 仅检查 `<system-reminder>` 前缀，**不验证后缀或完整结构**。如果未来 system-reminder 格式变化（例如换 tag 名），此判断会失效，导致 `latestTerminalToolBudget` 误把 reminder 当作真实 user 输入而提前停止扫描。

### 与 `tool_adapter.go` 的协议契约

本文件依赖 `tool_adapter.go::toolBudgetExceeded` 产出的 JSON 字段：
- `status` == `"call_budget_exceeded"`
- `final_answer_required` == `true`

任一字段名变更都会破坏短路逻辑。建议未来抽取为常量或共享类型。

### `Stream` 路径的单元素 stream

`schema.StreamReaderFromArray([]*schema.Message{msg})` 把合成消息包成"一次性"stream——客户端读取一次后 stream 结束。这与真实 LLM 的多 chunk stream 形态不同，但 SSE callback 应该已经处理了这种边界（PR-6 未在本文件体现）。
