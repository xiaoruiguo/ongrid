# graph/types.go

## 1. 概述

本文件定义了 eino ReAct 图执行层（`internal/manager/biz/aiops/graph`）的对外契约类型——`Input`/`Output`/`Config`。这些类型是 `BuildReActGraph` 构造出的 `compose.Runnable[*Input, *Output]` 的输入输出形状，承担"调用方与图执行层之间结构化数据传递"的职责。

包注释明确：本包采用 `flow/agent/react.NewAgent` 作为内层 ReAct 子图（非自研），自家 wrapper graph 仅在外层增加 `MessageAssembler`（输入适配）和 `OutputProjector`（输出适配）两个 lambda 节点。当前 PR-6 仅做脚手架，cutover（替换 `agent.go` 的 660 行 for-loop）属于下一个 PR。

## 2. 包信息

- **包名**：`graph`
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/biz/aiops/graph`
- **角色**：执行层契约类型定义；不含业务逻辑
- **依赖**：
  - 标准库 `time`
  - `github.com/cloudwego/eino/schema`（消息类型）
  - `github.com/ongridio/ongrid/internal/pkg/llm`（`llm.Usage` 用于 token 用量聚合）

## 3. 关键类型与接口

### `Input` 结构体

一次 ReAct-loop 运行的结构化请求，镜像 legacy `agent.go` for-loop 在分派给 LLM 前从 chat session 拼装的信息。由 `react.go` 的 `MessageAssembler` lambda 展平为 `[]*schema.Message` 喂给 eino 图。

| 字段 | 类型 | 含义 |
|------|------|------|
| `SystemPrompt` | `string` | 基础 agent 指令；空=不发 system 消息。由 chatruntime 拼接 base prompt + skill prompts + agent persona |
| `History` | `[]*schema.Message` | 历史对话（user/assistant/tool），按时间序。HistoryLimit gating 在上游完成 |
| `UserText` | `string` | 新用户轮次原文（post-mention-inlining） |
| `WebSearchEnabled` | `bool` | 每调用一次的 web_search 闸门（SPA globe toggle）。强制在 toolBag 层执行；此处供 assembler 写入 system-reminder 防止 hijacked tool_call 漏过 |
| `MentionsRendered` | `string` | 预渲染的 @-mention markdown 列表；空=本轮无 mention。格式与 `agent.go` 一致 |
| `AgentReminder` | `string` | persona 级 critical_reminder；非空时作为 `<system-reminder>` 块内的一行 bullet 重新注入每轮 |
| `DynamicHints` | `[]string` | 运行时计算的提示行（"tool X 失败 N 次"、"已运行 Y 次迭代"等）。由 chatruntime 计算，图不是真相源 |
| `Locale` | `string` | UI 语言（"en-US"/"zh-CN"）；assembler 转成显式 "respond in <language>" 指令 |

### `Output` 结构体

图运行的终态结果，镜像 `agent.Reply` 中本层可填充的部分（不触及 session repo，持久化是 persistence callback 的工作）。

| 字段 | 类型 | 含义 |
|------|------|------|
| `AssistantMessage` | `*schema.Message` | 最终的 assistant 消息（无 tool_calls）；成功时非 nil |
| `Iterations` | `int` | 本轮 ChatModel 调用次数；通过 metrics/audit handler 链计数；未挂载时为 0 |
| `Usage` | `llm.Usage` | 本次运行所有 ChatModel 调用聚合的 token 用量；由 `MetricsHandler` 填充 |

### `Config` 结构体

调优图行为。默认值与 `agent.go` 既有 for-loop 对齐，确保 cutover PR 行为零漂移。

| 字段 | 类型 | 默认 | 含义 |
|------|------|------|------|
| `Model` | `string` | "" | LLM 模型 id（"gpt-4o"、"claude-sonnet-4-6"）；空=用底层 ChatModel 默认 |
| `Provider` | `string` | "" | `RoutingChatModel` 识别的路由键；空=用默认 provider |
| `Temperature` | `float32` | 0 | 采样随机性；0 保留 legacy 默认 0.1 |
| `MaxIterations` | `int` | 30 | 外层 ReAct 循环上限；0→30 |
| `ToolTimeout` | `time.Duration` | 15s | 每个 tool 的墙上时间上限；由 BaseTool decorator 链（PR-3）执行 |

## 4. 关键函数与流程

### `func (c Config) applyDefaults() Config`

填充零值字段为 `agent.go` 的默认值，按值返回（避免调用方看到 mutation）。

```go
func (c Config) applyDefaults() Config {
    if c.MaxIterations <= 0 {
        c.MaxIterations = 30
    }
    if c.ToolTimeout <= 0 {
        c.ToolTimeout = 15 * time.Second
    }
    return c
}
```

调用方：`BuildReActGraph` 在构造内层 ReAct agent 前调用一次，确保 `MaxStep`、`WithMaxRunSteps` 拿到非零值。

## 5. 依赖关系

### 上游（被谁调用）
- `graph/react.go::BuildReActGraph` 消费 `Config`，构造 `compose.Runnable[*Input, *Output]`
- chatruntime 层（NEXT PR）作为 `Input`/`Output`/`Config` 的生产者/消费者

### 下游（依赖谁）
- `eino/schema.Message`：`History`、`AssistantMessage` 字段类型
- `internal/pkg/llm.Usage`：`Output.Usage` 字段类型，token 聚合契约

## 6. 并发与资源管理

本文件仅声明类型和一个无副作用的 `applyDefaults` 方法，**不持有可变状态、不开 goroutine、不分配需要释放的资源**。并发安全由调用方在 graph 运行时保证。

## 7. 设计模式与亮点

### 适配器模式（Adapter）

`Input`/`Output` 是"调用方语义层"与"eino 内部 `[]*schema.Message` 层"之间的适配层。eino 原生 API 接受 `[]*schema.Message`，对调用方不友好；本层让调用方传 `*Input`，由 `MessageAssembler` 展平。

### 配置默认值与零漂移策略

`applyDefaults` 按值返回而非指针返回，且零值即"使用默认"——这是 cutover PR 的关键设计：调用方可以传 `Config{}` 而无需逐字段填写，零行为漂移迁移 `agent.go`。

### 字段职责单一拆分

`WebSearchEnabled` 和 `MentionsRendered` 没有塞进 `UserText`，而是独立字段——让 assembler 决定如何 inline（mentions 在 user text 上方；web_search gating 由 toolBag 执行，assembler 仅镜像写入 system-reminder 做 belt-and-braces）。

## 8. 注意事项

### PR-6 脚手架阶段

包注释明确："PR-6 of scaffolding only. The cutover (replacing agent.go's 660-line for-loop with this graph) is the NEXT PR — main.go is not touched here. Callers in this PR are tests + internal wiring only."

→ 当前阶段这些类型仅被测试与内部 wiring 消费，**不要在 main.go / 业务路径直接使用**。

### `Iterations` 字段填充时机

`OutputProjector` lambda 只能看到 terminal message，无法计数 ChatModel 调用次数。`Iterations` 由 metrics/audit handler 在 caller 端填充——若未挂载计数 handler，此字段保持 0。

### `History` 不在本层做 gating

`HistoryLimit` gating 在上游 chatruntime 完成，本层按原样接收 slice。若调用方未做 gating，长 history 会直接进入 prompt，可能撑爆上下文。

### `Locale` 空值行为

空 `Locale` → 不附加语言指令（back-compat；IM bridge 不带 UI locale）。`assembleMessages` 中 `languageDirective("")` 返回空字符串。

### `WebSearchEnabled` 不在本层强制

字段本身只是 bool flag；强制（移除 web_search 工具）在 chatruntime 的 toolBag 装配层完成。本层只把它写入 system-reminder 作为防御性双保险。
