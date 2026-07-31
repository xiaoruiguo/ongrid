# `agent.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/agent/agent.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/agent`

## 1. 概述

本文件是 legacy AIOps agent loop 实现：自管 OpenAI tool-calling 循环，按 turn 推进 ChatModel ↔ Tools，带 web_search 过滤、legacyKernelMutatingTools 拒绝清单、toolreplay hoisting、SSE 流、persistence 同步写。该实现正被新 eino graph + chatruntime 替换，PR-6 后保留作为回退路径。

## 2. 包信息

- **包名**：`agent`
- **所属模块**：`internal/manager/biz/aiops/agent`
- **依赖方向**：被 `controlplane/aiops`、`chatruntime`（部分复用 buildMessages 逻辑）调用；依赖 `llm`、`tools`、`toolreplay`、`aiops` repo 接口

## 3. 关键类型与接口

```go
type Config struct {
    MaxIterations int
    ToolTimeout   time.Duration
    HistoryLimit  int
    WebSearchEnabled bool
    SystemPrompt  string
    // ...
}

type Reply struct {
    Content    string
    Iterations int
    Usage      llm.Usage
}

type Event interface{ isEvent() }

type AssistantEvent struct { /* ... */ }
type ToolEvent struct { /* ... */ }

type Emit func(Event)

type Agent struct {
    cfg    Config
    llm    llm.Client
    tools  *tools.Registry
    repo   aiops.SessionRepo
    log    *slog.Logger
    // ...
}
```

`legacyKernelMutatingTools` 是 map[string]bool 拒绝清单，列出 legacy loop 不允许 LLM 直接调用的变更类工具（必须走草案流程）。

## 4. 关键函数与流程

### `Run / RunStream`
- **签名**：`func (a *Agent) Run(ctx, sessionID, userText string) (*Reply, error)` + `RunStream(ctx, sessionID, userText, Emit) (*Reply, error)`
- **职责**：执行 agent loop 直到 LLM 返回无 tool_calls 或达 MaxIterations
- **流程**：调 `runInternal`，按事件类型 emit AssistantEvent/ToolEvent；同步写 repo；返回 Reply

### `runInternal`
- **流程**（每 turn）：
  1. `buildMessages(ctx, sessionID, userText)` 组装 system + history + 用户消息
  2. toolreplay hoisting：`toolreplay.Resolve` + `MarkDependentToolsForSkip` 修复 strict provider 的 tool_call 配对
  3. 过滤 tools：按 `WebSearchEnabled` 移除 web_search；按 legacyKernelMutatingTools 拒绝清单移除变更类
  4. `llm.Chat(ctx, ChatReq{Messages, Tools, ...})`
  5. 若 `Choices[0].Message.ToolCalls` 非空 → 并发执行 tools（每 tool 一个 goroutine + ToolTimeout）→ 写 repo → 进入下一 turn
  6. 否则 → 终态，返回 content
- **错误处理**：tool 错误包装为 JSON `{"error":...}` 回喂 LLM；LLM 错误直接返回

### `buildMessages`
- **职责**：组装 LLM 请求消息序列
- **流程**：system → history（带 toolreplay hoisting 修复）→ 用户消息（含 @-mention 渲染）
- **关键约束**：strict provider（DeepSeek v4+）要求每个 tool_calls 后紧跟 role=tool 消息；hoisting 把孤儿 tool 行上移到父 assistant 之后

## 5. 依赖关系

- **内部包**：`internal/pkg/llm`、`internal/manager/biz/aiops`（repo）、`internal/manager/biz/aiops/tools`、`internal/manager/biz/aiops/toolreplay`、`model/aiops`
- **外部库**：`github.com/sashabaranov/go-openai`、`github.com/prometheus/client_golang`

## 6. 并发与资源管理

- **Tool 并发执行**：单 turn 内多个 tool_call 并行 goroutine；每个带 ToolTimeout ctx
- **Repo 写入串行**：assistant row 先写，拿到 id 后再写 tool_call rows（避免 NOT NULL 约束失败）
- **SSE emit 串行**：单 Emit 函数由调用方序列化（http response writer 单 goroutine）
- **无共享可变状态**：Agent 实例字段只读；per-request 状态在栈上

## 7. 设计模式与亮点

- **Legacy loop 兜底**：PR-6 cutover 后保留，便于回滚；新 chatruntime 通过 graph 实现等价行为
- **toolreplay hoisting**：解决 strict provider 对 tool_call/tool 配对的严格顺序要求
- **拒绝清单模式**：legacyKernelMutatingTools 显式列出禁用工具，强制走 alertdraft 草案流程，避免 LLM 直接执行破坏性操作
- **web_search 动态过滤**：按 per-call 配置决定是否暴露 web_search 工具
- **tool 错误包装为 JSON**：注释明示"tool failures are facts"，LLM 看到错误后可重试 / 切换 / 询问用户，而非让整个会话崩溃

## 8. 注意事项

- **Legacy 状态**：新功能应在 chatruntime + graph 实现；本文件仅维护回退路径
- **MaxIterations 默认 30**：与 graph Config 默认值对齐
- **ToolTimeout 默认 15s**：与 graph Config 默认值对齐
- **buildMessages 逻辑被 chatruntime 复用**：修改时需考虑两个 caller
- **strict provider 兼容性**：DeepSeek v4+ 对 tool_call 配对严格，toolreplay 是必须的兜底
- **SSE 帧格式**：与 SPA 约定，修改需前端协同
