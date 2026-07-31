# graph/react.go

## 1. 概述

本文件是 eino ReAct 图执行层的核心构造器，负责：

1. 构造内层 eino `react.Agent` 子图（ChatModel ↔ Branch ↔ ToolsNode）
2. 在外层 wrapper graph 中包夹 `MessageAssembler`（输入适配）和 `OutputProjector`（输出适配）两个 lambda 节点
3. 实现 `assembleMessages`——把 `*Input` 展平为 eino 标准消息序列
4. 实现 `buildSystemReminder`——每轮注入的防漂移 `<system-reminder>` 块构造器
5. 实现 `languageDirective`/`reminderLanguageDirective`/`normalizeLocale`——多语言指令生成

包注释明确：内层 ReAct 子图直接用 `flow/agent/react.NewAgent`，不自研——eino 的实现是 canonical maintained 的，重新实现是 churn。

## 2. 包信息

- **包名**：`graph`
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/biz/aiops/graph`
- **角色**：图构造器 + 消息装配 + system-reminder 注入
- **依赖**：
  - 标准库 `context`、`errors`、`fmt`、`strings`
  - `github.com/cloudwego/eino/components/model`（`einomodel.ToolCallingChatModel`）
  - `github.com/cloudwego/eino/components/tool`
  - `github.com/cloudwego/eino/compose`（`compose.NewGraph`/`InvokableLambda`/`AddLambdaNode`/`AddEdge`/`Compile`）
  - `github.com/cloudwego/eino/flow/agent/react`（`react.NewAgent`/`AgentConfig`）
  - `github.com/cloudwego/eino/schema`（`schema.Message`/`SystemMessage`/`UserMessage`）
  - `github.com/ongridio/ongrid/internal/manager/biz/aiops/tools/basetool`

## 3. 关键类型与接口

本文件无常驻类型定义，仅暴露常量和一个构造函数。

### 常量

```go
const (
    NodeAssembler  = "MessageAssembler"  // *Input → []*schema.Message 的 lambda
    NodeReact      = "ReActSubgraph"     // 内层 eino react.Agent 子图
    NodeProjector  = "OutputProjector"   // *schema.Message → *Output 的 lambda
)

const SystemReminderTag = "system-reminder"  // 每轮注入块的 XML 标签名
```

节点名稳定，便于 audit/SSE callback 通过 `RunInfo.Name` 过滤。内层 ReAct 子图节点名（`ChatModel`/`Tools`）由 eino 拥有，复用以便 persistence/SSE handler 看到与 canonical eino 布局一致的名字。

## 4. 关键函数与流程

### `BuildReActGraph(model, tools, cfg) (compose.Runnable[*Input, *Output], error)`

主构造函数，拓扑：

```
START
  ↓
MessageAssembler (lambda) -- *Input → []*schema.Message
  ↓
ReActSubgraph (compose.AnyGraph) -- []*schema.Message → *schema.Message
  ↓
OutputProjector (lambda) -- *schema.Message → *Output
  ↓
END
```

执行步骤：

1. **nil 防御**：`model == nil` → 返回错误
2. **应用默认 config**：`cfg = cfg.applyDefaults()`
3. **包装 budget stop model**：`model = wrapBudgetStopModel(model)`——注入预算停止装饰器
4. **构造内层 ReAct agent**：
   ```go
   reactCfg := &react.AgentConfig{
       ToolCallingModel: model,
       ToolsConfig: compose.ToolsNodeConfig{
           Tools: baseTools,
           UnknownToolsHandler: func(_ context.Context, name, _ string) (string, error) {
               return fmt.Sprintf("Tool %q is not available to you. ...", name), nil
           },
       },
       MaxStep:       cfg.MaxIterations*2 + 2,
       GraphName:     "ReActAgent",
       ModelNodeName: "ChatModel",
       ToolsNodeName: "Tools",
   }
   ```
   - `UnknownToolsHandler`：模型幻觉工具名 / 被过滤掉的工具调用时，返回可恢复的 tool-result 而不是终止整个 run（避免 flow Agent nodes 的崩溃问题）
   - `MaxStep = MaxIterations*2 + 2`：eino 的 "step" 计每个 graph 节点访问，一次 ReAct 迭代 = ChatModel + ToolsNode = 2 步；加倍 + 2 给 framing nodes 留余量
5. **构造外层 wrapper graph**：
   - `AddLambdaNode(NodeAssembler, ...)` —— 调 `assembleMessages`
   - `AddGraphNode(NodeReact, innerGraph, ...)` —— 嵌入 ReAct 子图
   - `AddLambdaNode(NodeProjector, ...)` —— 把 `*schema.Message` 转 `*Output`，提取 `ResponseMeta.Usage`
   - 4 条 `AddEdge` 串起 START → Assembler → React → Projector → END
6. **编译**：`g.Compile(ctx, WithGraphName("OngridReActAgent"), WithMaxRunSteps(cfg.MaxIterations+10))`

### `assembleMessages(in *Input) ([]*schema.Message, error)`

把 `*Input` 展平为 eino 标准消息序列：

```
[system?] + [history...] + [<system-reminder>?] + [user_text?]
```

具体顺序：

1. **system 消息**（可选）：`SystemPrompt` + `\n\n` + `languageDirective(Locale)`；空则跳过
2. **history**：原样追加（chatruntime 在上游已剥离 trailing user row，避免重复）
3. **system-reminder 块**（仅当 `UserText` 非空）：作为独立的 user-role 消息注入
4. **user 文本**（仅当 `UserText` 非空）：`MentionsRendered + "\n\n" + UserText`

→ `UserText == ""` 时跳过 user-role 消息，匹配 legacy 路径（caller 已把 user turn 追加到 History）。

### `buildSystemReminder(in *Input) string`

构造每轮防漂移 `<system-reminder>` 块：

```go
<system-reminder>
- <language directive>
- 同一工具失败两次后请换思路，不要重复调用
- device_id / alert_id 必须是数字 ID（@-mention 已经为你解析）
- 工具结果是事实，不要在没有数据时编造
- call_budget_exceeded 只限制当前用户消息；新消息可重新调用工具
- web_search 已被关闭，本轮不要调用     // 仅当 !WebSearchEnabled
- <AgentReminder>                       // 仅当非空
- <DynamicHints[i]>                     // 每条非空 hint 一行
</system-reminder>
```

**基线规则**（hardcoded，始终存在）：
- 同一工具失败两次换思路
- device_id/alert_id 必须数字 ID
- 工具结果是事实
- call_budget_exceeded 仅当前轮次

**动态部分**：
- `reminderLanguageDirective`（前置，引导块）——每轮重申响应语言（system prompt 在长会话中会滑出注意力窗口）
- `web_search` 关闭提示（条件性）
- `AgentReminder`（persona critical_reminder，条件性）
- `DynamicHints`（运行时计算，条件性）

英文模式用英文基线规则。

### `languageDirective(locale) string`

把 UI locale 转成显式 "answer in this language" 系统指令。空 locale → 空字符串（back-compat，IM bridge 无 UI locale）。

英文指令覆盖 tool-call narration（这是中文 persona 在英文模式下最先漂移回中文的部分）：
> Respond in English. Everything you write to the user — prose, explanations, headings, and the narration around every tool call — must be in English. ... Translate domain terms to their English equivalents (e.g. "0号病人" → "patient zero", "根因" → "root cause", "告警" → "alert", "巡检" → "inspection"). Leave only proper nouns, identifiers, hostnames, file paths, code, and raw command output verbatim.

### `reminderLanguageDirective(locale) string`

简短版本，注入 `<system-reminder>` 块开头，每轮重申。

### `normalizeLocale(locale) string`

```go
l := strings.ToLower(strings.TrimSpace(locale))
l = strings.ReplaceAll(l, "_", "-")
switch {
case strings.HasPrefix(l, "en"): return "en"
case strings.HasPrefix(l, "zh"): return "zh"
default: return ""
}
```

容忍 "en-US"/"en_US"/"EN"/"zh-CN" 等变体。

## 5. 依赖关系

### 上游
- chatruntime（NEXT PR）作为 `BuildReActGraph` 的调用方，装配 model + tools + cfg 后取得 `Runnable`
- `BuildReActGraph` 在 build 时调用 `wrapBudgetStopModel`（本包 `budget_stop_model.go`）、`WrapBaseTools`（本包 `tool_adapter.go`）

### 下游
- `react.NewAgent`（cloudwego/eino，内层 ReAct 子图）
- `compose.NewGraph`/`Compile`（cloudwego/eino，外层 wrapper）
- `basetool.BaseTool`（工具接口）

## 6. 并发与资源管理

### Build-time vs Invoke-time 分离

`BuildReActGraph` 是 **build-time** 操作——构造并编译 graph，返回 `Runnable`。同一 `Runnable` 可被多次 `Invoke` 复用（不同请求挂不同 handler 集合）。

handlers **不在 build 时注册**。eino graph compile API 不接受 Handler list，handlers 通过 `compose.WithCallbacks(handlers...)` 在 Invoke 调用时挂载。这一设计让"同一编译图跨请求复用 + 每请求不同 handler 集（如 per-tenant audit sink）"成为可能。

### MessageAssembler / OutputProjector 无状态

两个 lambda 都是纯函数，不持有可变状态。`assembleMessages` 只读 `*Input`，`OutputProjector` 只读 terminal `*schema.Message`。并发安全由 eino graph runtime 保证。

### `Iterations` 字段填充不在本层

`OutputProjector` 只能看到 terminal message，无法计数 ChatModel 调用。`Output.Iterations` 在 caller 端由 metrics/audit handler 填充。本层保留 0。

## 7. 设计模式与亮点

### 适配器 + 包装器模式

外层 wrapper graph 把 eino 内层 ReAct 子图包夹在两个 lambda 之间，对外暴露 `*Input`/`*Output` 契约，对内对接 eino `[]*schema.Message`/`*schema.Message`。这是经典适配器模式，让调用方无需关心 eino 内部消息格式。

### UnknownToolsHandler 防幻觉工具终止

```go
UnknownToolsHandler: func(_ context.Context, name, _ string) (string, error) {
    return fmt.Sprintf("Tool %q is not available to you. Use one of the tools provided in this conversation, or answer directly without a tool.", name), nil
}
```

注释解释：模型幻觉一个不在本节点工具集的工具名 / 调用了被 worker persona 过滤掉的工具时，eino 默认行为是 "tool X not found in toolsNode indexes" 终止整个 run。这正是 flow Agent nodes 崩溃的根因。本 handler 把它降级为可恢复的 tool-result，让模型改用真实工具或直接作答。

### MaxStep 双倍 + 余量

```go
MaxStep: cfg.MaxIterations*2 + 2,
```

eino 的 "step" 计每个 graph 节点访问，一次 ReAct 迭代 = ChatModel + ToolsNode = 2 步。若直接传 `MaxIterations=30`，LLM 实际只有 15 次 ChatModel 轮次。注释明确："Without this fix the LLM was capped at ~15 ChatModel turns when MaxIterations=30, surfacing as a graph ErrExceededMaxSteps mid-conversation (stream error)."

外层 `WithMaxRunSteps(cfg.MaxIterations+10)` 给 framing nodes（assembler + projector）留余量。

### System-reminder 周期注入（仿 claude-code）

`<system-reminder>` 块作为独立 user-role 消息每轮重新注入，确保长会话中 system prompt 滑出注意力窗口后规则仍生效。这是 claude-code 反漂移机制的 ongrid 化实现。

### 语言指令双层注入

- `languageDirective` 进 system prompt（最强信号，一次）
- `reminderLanguageDirective` 进每轮 `<system-reminder>`（重申，防漂移）

双层注入是因为 persona 是中文，英文模式下模型容易漂回中文——尤其是 tool-call narration。

### `UnknownToolsHandler` 返回 nil error

返回 `(string, nil)` 而非 `(string, error)`——保持与 eino "tool-result as data" 语义一致，让模型把它当作事实处理。

## 8. 注意事项

### PR-6 脚手架，cutover 在下一 PR

`BuildReActGraph` 当前仅被测试和内部 wiring 调用，main.go 未触及。cutover（替换 `agent.go` 660 行 for-loop）是 NEXT PR 的工作。

### `MaxStep` 公式与 eino 版本耦合

`MaxIterations*2 + 2` 假设 eino 一次 ReAct 迭代消耗 2 个 step。若未来 eino 改变 step 计数语义（例如计入 MsgAppend 节点），此公式需要调整。注释未提及版本依赖，建议跟踪 eino changelog。

### `UnknownToolsHandler` 不区分幻觉与过滤

模型幻觉工具名 / 调用了被 persona 过滤的工具，二者返回相同提示。模型无法区分"工具不存在"和"你被禁止使用"，可能反复尝试。当前提示语同时覆盖两种情况（"not available to you"）。

### `buildSystemReminder` 基线规则硬编码

中文/英文基线规则各自硬编码在 `buildSystemReminder` 内，未抽取常量。若未来需要 A/B 测试不同基线规则集，需要重构。当前好处是单文件可读性好。

### `languageDirective` 含具体翻译示例

英文指令硬编码了 "0号病人" → "patient zero"、"根因" → "root cause" 等翻译对——这些是 ongrid domain 词汇。新增 domain 词汇需要修改本函数。

### `assembleMessages` 不做 history gating

`HistoryLimit` gating 在 chatruntime 上游完成。若调用方未做 gating，长 history 直接进 prompt 可能撑爆上下文。本层按原样接收 slice。

### `OutputProjector` 的 `Usage` 字段填充

```go
if msg != nil && msg.ResponseMeta != nil && msg.ResponseMeta.Usage != nil {
    out.Usage.PromptTokens = msg.ResponseMeta.Usage.PromptTokens
    // ...
}
```

只填 terminal message 的 usage——这是最后一轮 ChatModel 的用量，**不是**整个 run 的聚合。聚合用量由 `MetricsHandler` 在 caller 端覆盖。若 caller 未挂载 MetricsHandler，`Output.Usage` 仅反映最后一轮。
