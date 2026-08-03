# OnGrid 中 eino 框架使用场景与技术实现分析

> 本文档分析 OnGrid 项目中对 [cloudwego/eino](https://github.com/cloudwego/eino) 框架的全部使用场景，标注相关代码文件名、行号，并给出 API 与参数说明。

---

## 目录

1. [eino 框架概述](#1-eino-框架概述)
2. [eino import 路径清单](#2-eino-import-路径清单)
3. [ChatModel 接口实现](#3-chatmodel-接口实现)
   - 3.1 [RoutingChatModel — 多 Provider 路由](#31-routingchatmodel--多-provider-路由)
   - 3.2 [clientChatModel — 适配器](#32-clientchatmodel--适配器)
   - 3.3 [derivedChatModel — WithTools 派生包装](#33-derivedchatmodel--withtools-派生包装)
   - 3.4 [budgetStopModel — 预算停机装饰器](#34-budgetstopmodel--预算停机装饰器)
4. [ReAct Agent 构建与执行](#4-react-agent-构建与执行)
5. [Callback 体系与 Handler 实现](#5-callback-体系与-handler-实现)
   - 5.1 [Callback 链装配](#51-callback-链装配)
   - 5.2 [SSEHandler](#52-ssehandler)
   - 5.3 [PersistenceHandler](#53-persistencehandler)
   - 5.4 [AuditHandler](#54-audithandler)
   - 5.5 [MetricsHandler](#55-metricshandler)
   - 5.6 [AlertDraftGuardHandler](#56-alertdraftguardhandler)
   - 5.7 [BudgetCallbackHandler](#57-budgetcallbackhandler)
6. [Tool 接口与适配层](#6-tool-接口与适配层)
7. [Graph/Compose API 使用](#7-graphcompose-api-使用)
8. [Schema 类型使用](#8-schema-类型使用)
9. [完整调用链路](#9-完整调用链路)
10. [架构图示](#10-架构图示)

---

## 1. eino 框架概述

eino 是字节跳动开源的 Go AI 应用编排框架（`github.com/cloudwego/eino`），提供：

| 能力 | eino 包路径 | OnGrid 使用位置 |
|------|------------|----------------|
| ChatModel 接口 | `components/model` | LLM 调用层 |
| Tool 接口 | `components/tool` | 工具调用层 |
| ReAct Agent | `flow/agent/react` | 智能体构建 |
| Graph 编排 | `compose` | 图构建与执行 |
| Callback 回调 | `callbacks` | SSE/持久化/审计/指标 |
| Schema 类型 | `schema` | 消息/工具/流 |

OnGrid 没有直接使用 `eino-ext/components/model/openai`（官方 OpenAI 适配），而是自建薄适配器 `clientChatModel` 包装已有的 `sashabaranov/go-openai` 客户端，保持依赖最小化。

---

## 2. eino import 路径清单

以下列出项目中所有 eino 相关 import 及其用途：

| import 路径 | 用途 | 代表文件 |
|------------|------|---------|
| `github.com/cloudwego/eino/components/model` | ChatModel 接口 + Option | `internal/pkg/llm/eino_routing.go` |
| `github.com/cloudwego/eino/components/tool` | Tool 接口 + Option | `internal/manager/biz/aiops/graph/tool_adapter.go` |
| `github.com/cloudwego/eino/components` | Component 枚举 | `internal/manager/biz/aiops/graph/callbacks/sse.go` |
| `github.com/cloudwego/eino/compose` | Graph 构建 + 编译 | `internal/manager/biz/aiops/graph/react.go` |
| `github.com/cloudwego/eino/flow/agent/react` | ReAct Agent | `internal/manager/biz/aiops/graph/react.go` |
| `github.com/cloudwego/eino/callbacks` | Handler 接口 + Timing | `internal/manager/biz/aiops/graph/callbacks/chain.go` |
| `github.com/cloudwego/eino/schema` | Message/ToolInfo/StreamReader | 多处 |
| `github.com/eino-contrib/jsonschema` | JSON Schema 类型 | `internal/manager/biz/aiops/graph/tool_adapter.go` |

---

## 3. ChatModel 接口实现

eino 的 `model.ChatModel` 接口定义（来自 eino SDK）：

```go
type ChatModel interface {
    Generate(ctx context.Context, input []*schema.Message, opts ...Option) (*schema.Message, error)
    Stream(ctx context.Context, input []*schema.Message, opts ...Option) (*schema.StreamReader[*schema.Message], error)
    BindTools(tools []*schema.ToolInfo) error
}

// 扩展接口（eino 推荐）
type ToolCallingChatModel interface {
    ChatModel
    WithTools(tools []*schema.ToolInfo) (ToolCallingChatModel, error)
}
```

OnGrid 实现了 **4 个** ChatModel 接口实现：

```
RoutingChatModel  ←  model.ChatModel + model.ToolCallingChatModel
  └── inner map[string]model.ChatModel
        ├── clientChatModel (openai)     ←  model.ChatModel + model.ToolCallingChatModel
        ├── clientChatModel (anthropic)
        ├── clientChatModel (zhipu)
        └── clientChatModel (gemini / deepseek / kimi / custom)
              └── 包装 llm.Client (sashabaranov/go-openai)

derivedChatModel  ←  WithTools 派生产物，包装 ToolCallingChatModel
budgetStopModel   ←  装饰器，拦截工具预算超限
```

### 3.1 RoutingChatModel — 多 Provider 路由

**文件**: [internal/pkg/llm/eino_routing.go](file:///d:/claude/ongrid/internal/pkg/llm/eino_routing.go)

**结构定义** (L89-101):

```go
type RoutingChatModel struct {
    inner           map[string]model.ChatModel
    defaultProvider string
    defaultResolver func(context.Context) (provider, mdl string)
}
```

**参数说明**:
- `inner`: provider id → ChatModel 映射表
- `defaultProvider`: 默认 provider（当 WithProvider 缺省时使用）
- `defaultResolver`: 运行时解析最新默认 provider + model（支持热更新，无需重启）

**支持的 Provider 常量** (L40-51):

```go
const (
    ProviderOpenAI    = "openai"
    ProviderAnthropic = "anthropic"
    ProviderZhipu     = "zhipu"
    ProviderGemini    = "gemini"
    ProviderDeepSeek  = "deepseek"
    ProviderKimi      = "kimi"
    ProviderCustom    = "custom"
)
```

**构造函数** `NewRoutingChatModel` (L119-142):

```go
func NewRoutingChatModel(cfg RoutingChatModelConfig) (*RoutingChatModel, error)
```

`RoutingChatModelConfig` 参数 (L108-115):
- `Inner`: provider → ChatModel map（必须包含 DefaultProvider）
- `DefaultProvider`: 缺省 provider
- `DefaultResolver`: 可选，运行时默认 provider 解析器

**路由选项** — `WithProvider` (L72-76):

```go
func WithProvider(provider string) model.Option {
    return model.WrapImplSpecificOptFn(func(o *providerOpts) {
        o.provider = provider
    })
}
```

使用 `model.WrapImplSpecificOptFn` 注册实现特定选项，通过 `model.GetImplSpecificOptions` 读取。

**动态默认** — `withDynamicDefault` (L151-170):

```go
func (r *RoutingChatModel) withDynamicDefault(ctx context.Context, opts []model.Option) []model.Option
```

逻辑：
1. 若 `defaultResolver` 为 nil → 直接返回原 opts
2. 若已通过 `WithProvider` 指定 provider → 不覆盖
3. 调用 `defaultResolver(ctx)` 获取最新 provider + model
4. 若 provider 存在于 inner map → 注入 `WithProvider(prov)`
5. 若 model 非空且未通过 `model.WithModel` 指定 → 注入 `model.WithModel(mdl)`

**Generate 方法** (L189-196):

```go
func (r *RoutingChatModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
    opts = r.withDynamicDefault(ctx, opts)
    inner, _, err := r.pick(opts...)
    if err != nil {
        return nil, err
    }
    return inner.Generate(ctx, input, opts...)
}
```

**Stream 方法** (L200-207): 结构同 Generate，转发到 `inner.Stream`。

**WithTools 方法** (L232-259):

```go
func (r *RoutingChatModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error)
```

关键行为：
- 遍历每个 inner ChatModel
- 若 inner 实现 `model.ToolCallingChatModel` → 调用其 `WithTools` 派生新实例，用 `derivedChatModel` 包装
- 若 inner 仅实现 `model.ChatModel` → 回退到 `BindTools`（非原子操作，有文档警告）
- 返回新的 RoutingChatModel 副本（不修改 receiver）

**编译时接口检查** (L298-301):

```go
var (
    _ model.ChatModel            = (*RoutingChatModel)(nil)
    _ model.ToolCallingChatModel = (*RoutingChatModel)(nil)
)
```

### 3.2 clientChatModel — 适配器

**文件**: [internal/pkg/llm/eino_routing.go](file:///d:/claude/ongrid/internal/pkg/llm/eino_routing.go) L318-542

**结构定义** (L318-331):

```go
type clientChatModel struct {
    inner      Client               // llm.Client (sashabaranov/go-openai)
    model      string               // 默认模型名
    userID     uint64               // 预算 gate 用
    boundTools []*schema.ToolInfo   // BindTools 绑定的工具
}
```

**构造函数** `NewClientChatModel` (L344-353):

```go
func NewClientChatModel(cfg ClientChatModelConfig) (model.ChatModel, error)
```

`ClientChatModelConfig` 参数 (L334-338):
- `Client`: llm.Client（必须非 nil）
- `Model`: 默认模型名
- `UserID`: 预算检查用 user id

**Generate 方法** (L357-368):

```go
func (c *clientChatModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
    common := model.GetCommonOptions(&model.Options{}, opts...)
    req, err := c.buildChatReq(input, common)
    // ...
    resp, err := c.inner.Chat(ctx, req)
    // ...
    return einoMessageFromChatResp(resp), nil
}
```

**Stream 方法 — 伪流式** (L372-378):

```go
func (c *clientChatModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
    msg, err := c.Generate(ctx, input, opts...)
    if err != nil {
        return nil, err
    }
    return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}
```

> **重要**: 当前实现是**伪流式** — 先完整 Generate 再包装成单 chunk StreamReader。真正的 token-by-token 流式尚未实现（文件头注释 L22-25 明确标注）。

**buildChatReq — eino → llm 转换** (L396-428):

```go
func (c *clientChatModel) buildChatReq(input []*schema.Message, common *model.Options) (ChatReq, error)
```

转换逻辑：
1. 遍历 `input` 调用 `einoMessageToLLM(m)` 转换消息
2. 合并 `boundTools` 与 `common.Tools`（后者优先）
3. 调用 `einoToolsToLLM` 转换工具
4. 从 `common.Temperature` / `common.Model` 读取覆盖值

**einoMessageToLLM** (L433-455): 转换 `*schema.Message` → `llm.Message`，保留 Role/Content/ToolCallID/ToolName/ToolCalls。

**einoMessageFromChatResp** (L460-492): 转换 `*llm.ChatResp` → `*schema.Message`，填充 `ResponseMeta.Usage`（`schema.TokenUsage`），供 BudgetCallbackHandler 在 OnEnd 读取。

**einoToolsToLLM** (L498-518): 转换 `[]*schema.ToolInfo` → `[]ToolSchema`，通过 `paramsToJSONSchema` 渲染 `ParamsOneOf` 为 JSON Schema。

**paramsToJSONSchema** (L524-542): 调用 `p.ToJSONSchema()` 渲染；nil 时返回 `{"type":"object","properties":{}}` 兜底。

### 3.3 derivedChatModel — WithTools 派生包装

**文件**: [internal/pkg/llm/eino_routing.go](file:///d:/claude/ongrid/internal/pkg/llm/eino_routing.go) L266-280

```go
type derivedChatModel struct {
    tcm model.ToolCallingChatModel
}

func (d *derivedChatModel) Generate(...)  // 转发到 tcm.Generate
func (d *derivedChatModel) Stream(...)    // 转发到 tcm.Stream
func (d *derivedChatModel) BindTools(_ []*schema.ToolInfo) error { return nil } // no-op
```

作用：`RoutingChatModel.WithTools` 调用 inner 的 `WithTools` 后，返回的 `ToolCallingChatModel` 不带 `BindTools` 方法，但 inner map 存储的是 `model.ChatModel`（带 BindTools）。此包装器补一个 no-op `BindTools` 使类型匹配。

### 3.4 budgetStopModel — 预算停机装饰器

**文件**: [internal/manager/biz/aiops/graph/budget_stop_model.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/budget_stop_model.go)

```go
type budgetStopModel struct {
    inner einomodel.ToolCallingChatModel
}

func wrapBudgetStopModel(inner einomodel.ToolCallingChatModel) einomodel.ToolCallingChatModel
```

**Generate/Stream 拦截** (L23-35):

```go
func (m *budgetStopModel) Generate(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.Message, error) {
    if msg, ok := finalAnswerAfterToolBudget(input); ok {
        return msg, nil  // 短路，不调用 inner
    }
    return m.inner.Generate(ctx, input, opts...)
}
```

`finalAnswerAfterToolBudget` (L45-59): 扫描消息历史，若最近的 tool 消息包含 `{"status":"call_budget_exceeded","final_answer_required":true}`，则返回一条"本轮查询已达上限"的合成 assistant 消息，跳过 LLM 调用。

**调用位置**: [react.go L87](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/react.go#L87)

```go
model = wrapBudgetStopModel(model)
```

在 BuildReActGraph 入口处装饰，确保所有 LLM 调用都经过预算检查。

---

## 4. ReAct Agent 构建与执行

**文件**: [internal/manager/biz/aiops/graph/react.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/react.go)

### 4.1 BuildReActGraph 函数签名 (L78-82)

```go
func BuildReActGraph(
    model einomodel.ToolCallingChatModel,
    tools []basetool.BaseTool,
    cfg Config,
) (compose.Runnable[*Input, *Output], error)
```

**参数说明**:
- `model`: 已装饰的 ChatModel（含 budgetStopModel）
- `tools`: ongrid basetool.BaseTool 列表
- `cfg`: 图配置（MaxIterations 等）

**返回**: `compose.Runnable[*Input, *Output]` — eino 的可执行图

### 4.2 图拓扑 (L50-60)

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

节点名常量 (L23-32):
- `NodeAssembler = "MessageAssembler"`
- `NodeReact = "ReActSubgraph"`
- `NodeProjector = "OutputProjector"`

### 4.3 内部 ReAct Agent 构建 (L97-130)

```go
reactCfg := &react.AgentConfig{
    ToolCallingModel: model,
    ToolsConfig: compose.ToolsNodeConfig{
        Tools: baseTools,
        UnknownToolsHandler: func(_ context.Context, name, _ string) (string, error) {
            return fmt.Sprintf("Tool %q is not available...", name), nil
        },
    },
    MaxStep:       cfg.MaxIterations*2 + 2,
    GraphName:     "ReActAgent",
    ModelNodeName: "ChatModel",
    ToolsNodeName: "Tools",
}
reactAgent, err := react.NewAgent(context.Background(), reactCfg)
```

**react.AgentConfig 参数说明**:
| 参数 | 值 | 说明 |
|------|-----|------|
| `ToolCallingModel` | `model` | 已装饰的 ChatModel |
| `ToolsConfig.Tools` | `baseTools` | eino tool.BaseTool 列表 |
| `ToolsConfig.UnknownToolsHandler` | 兜底函数 | 模型幻觉工具名时返回可恢复的 tool result，而非终止整个运行 |
| `MaxStep` | `cfg.MaxIterations*2 + 2` | eino 的 step = 每个节点访问；一次 ReAct 迭代 = ChatModel + ToolsNode = 2 step，所以 ×2，+2 给外层 framing 节点留余量 |
| `GraphName` | `"ReActAgent"` | 内部子图名 |
| `ModelNodeName` | `"ChatModel"` | 模型节点名（SSE/persistence 回调按此过滤） |
| `ToolsNodeName` | `"Tools"` | 工具节点名 |

**UnknownToolsHandler 的重要性** (L102-109): 没有它，模型调用一个不存在的工具名会直接 abort 整个运行（"tool X not found in toolsNode indexes"）。OnGrid 返回一个可恢复的 tool result，让模型自己换工具或直接回答。

### 4.4 导出内部图 (L132)

```go
innerGraph, innerNodeOpts := reactAgent.ExportGraph()
```

`react.Agent.ExportGraph()` 返回 eino 内部的 `compose.AnyGraph` + 节点选项，供外层包装图嵌入。

### 4.5 外层包装图构建 (L135-187)

**创建图** (L135):
```go
g := compose.NewGraph[*Input, *Output]()
```

**MessageAssembler Lambda** (L137-142):
```go
assembler := compose.InvokableLambda(func(ctx context.Context, in *Input) ([]*schema.Message, error) {
    return assembleMessages(in)
})
g.AddLambdaNode(NodeAssembler, assembler)
```

`compose.InvokableLambda` 创建一个同步 Lambda 节点；`g.AddLambdaNode` 添加到图。

**ReAct 子图节点** (L144-146):
```go
g.AddGraphNode(NodeReact, innerGraph, innerNodeOpts...)
```

`AddGraphNode` 将 eino 内部图作为子图节点嵌入。

**OutputProjector Lambda** (L148-163):
```go
projector := compose.InvokableLambda(func(ctx context.Context, msg *schema.Message) (*Output, error) {
    out := &Output{AssistantMessage: msg}
    if msg.ResponseMeta != nil && msg.ResponseMeta.Usage != nil {
        out.Usage.PromptTokens = msg.ResponseMeta.Usage.PromptTokens
        // ...
    }
    return out, nil
})
g.AddLambdaNode(NodeProjector, projector)
```

**边连接** (L165-176):
```go
g.AddEdge(compose.START, NodeAssembler)
g.AddEdge(NodeAssembler, NodeReact)
g.AddEdge(NodeReact, NodeProjector)
g.AddEdge(NodeProjector, compose.END)
```

`compose.START` / `compose.END` 是 eino 的图入口/出口哨兵节点。

**编译** (L178-186):
```go
runnable, err := g.Compile(context.Background(),
    compose.WithGraphName("OngridReActAgent"),
    compose.WithMaxRunSteps(cfg.MaxIterations+10),
)
```

**Compile 参数**:
- `compose.WithGraphName("OngridReActAgent")`: 外层图名
- `compose.WithMaxRunSteps(cfg.MaxIterations+10)`: 外层步数限制（assembler + react-subgraph + projector）

### 4.6 assembleMessages — 消息组装 (L212-248)

```go
func assembleMessages(in *Input) ([]*schema.Message, error)
```

消息顺序：
1. **System message**（若 SystemPrompt 非空）— 含 languageDirective
2. **History** — 持久化的历史消息（chatruntime 已剥离尾部 user 行）
3. **System-Reminder**（user role）— 每轮注入的防漂移块
4. **User text**（含 @-mention preamble）

使用 `schema.SystemMessage(sp)` / `schema.UserMessage(body)` 构造 eino 消息。

### 4.7 buildSystemReminder — 防漂移注入 (L304-349)

每轮注入 `<system-reminder>` 块（user role），包含：
- 基线规则（工具失败重试限制、ID 格式、工具结果是事实等）
- 语言指令
- web_search 开关状态
- Persona critical_reminder
- 动态提示（DynamicHints）

---

## 5. Callback 体系与 Handler 实现

eino 的 `callbacks.Handler` 接口：

```go
type Handler interface {
    OnStart(ctx, *RunInfo, CallbackInput) context.Context
    OnEnd(ctx, *RunInfo, CallbackOutput) context.Context
    OnError(ctx, *RunInfo, error) context.Context
    OnStartWithStreamInput(ctx, *RunInfo, *StreamReader[CallbackInput]) context.Context
    OnEndWithStreamOutput(ctx, *RunInfo, *StreamReader[CallbackOutput]) context.Context
}

type TimingChecker interface {
    Needed(ctx, *RunInfo, CallbackTiming) bool
}
```

`RunInfo` 包含 `Component`（ComponentOfChatModel / ComponentOfTool / ComponentOfGraph）和 `Name`。

### 5.1 Callback 链装配

**文件**: [internal/manager/biz/aiops/graph/callbacks/chain.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/chain.go)

**Deps 结构** (L52-73):

```go
type Deps struct {
    AlertDraftGuard AlertDraftGuardDeps
    Persistence     PersistenceDeps
    SSE             SSEEmitter
    Audit           AuditDeps
    Metrics         MetricsDeps
    BudgetChecker   llm.BudgetChecker
    BudgetUserID    uint64
}
```

**NewDefaultHandlers** (L88-118):

```go
func NewDefaultHandlers(deps Deps) []callbacks.Handler
```

链顺序（注册顺序 = eino OnEnd 执行顺序）：

| 序号 | Handler | 条件 | 作用 |
|------|---------|------|------|
| 1 | AlertDraftGuardHandler | 用户意图像"创建告警规则" | 拦截模型-only 文字草案 |
| 2 | PersistenceHandler | PersistenceDeps.Repo 非 nil | 写 chat_messages + chat_tool_calls |
| 3 | SSEHandler | SSE 非 nil | 推送 SSE 帧 |
| 4 | AuditHandler | Logger 非 nil | slog INFO 审计日志 |
| 5 | MetricsHandler | Registerer 非 nil | Prometheus 计数器 |
| 6 | BudgetCallbackHandler | BudgetChecker 非 nil | token 预算 gate |

**assistantIDRelay** (L26-45): 跨 Handler 通信槽。

```go
type assistantIDRelay struct {
    id atomic.Pointer[string]
}
```

PersistenceHandler.OnEnd 写入持久化的 chat_messages.id；SSEHandler.OnEnd 读取该 id 附加到 `assistant_end` 帧。两者在同一 goroutine 内按注册顺序执行，`atomic.Pointer` 足够安全。

**FinalizeBatches** (L127-133): 请求结束后刷新未完成的持久化批次。

### 5.2 SSEHandler

**文件**: [internal/manager/biz/aiops/graph/callbacks/sse.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/sse.go)

**SSEEventType 常量** (L22-50):

```go
const (
    SSEEventAssistantStart    SSEEventType = "assistant_start"
    SSEEventAssistantDelta    SSEEventType = "assistant_delta"
    SSEEventAssistantEnd      SSEEventType = "assistant_end"
    SSEEventToolStart         SSEEventType = "tool_start"
    SSEEventToolEnd           SSEEventType = "tool_end"
    SSEEventDone              SSEEventType = "done"
    SSEEventError             SSEEventType = "error"
    SSEEventTaskNotification  SSEEventType = "task_notification"
)
```

**SSEEvent 结构** (L57-66):

```go
type SSEEvent struct {
    Type         SSEEventType
    Iteration    int
    Assistant    *AssistantPayload
    Delta        *AssistantDelta
    Tool         *ToolPayload
    Done         *DonePayload
    Error        *ErrorPayload
    Notification *TaskNotificationPayload
}
```

**SSEHandler 结构** (L172-188):

```go
type SSEHandler struct {
    emit             SSEEmitter
    iterations       atomic.Int64
    toolStartsMu     sync.Mutex
    toolStarts       map[string]toolStart
    assistantIDRelay *assistantIDRelay
}
```

**Needed — Timing 过滤** (L209-233):

```go
func (h *SSEHandler) Needed(_ context.Context, info *callbacks.RunInfo, timing callbacks.CallbackTiming) bool
```

| Component | 监听的 Timing |
|-----------|--------------|
| ComponentOfChatModel | OnStart, OnEnd, OnError, OnEndWithStreamOutput |
| ComponentOfTool | OnStart, OnEnd, OnError |
| 其他（Graph/Workflow/Chain） | OnEnd, OnError |

**OnStart** (L236-271):
- `ComponentOfChatModel` → `iterations.Add(1)` + emit `assistant_start`
- `ComponentOfTool` → 记录 toolStart（时间 + args） + emit `tool_start`

**OnEnd** (L274-343):
- `ComponentOfChatModel` → 读取 `assistantIDRelay.load()` + emit `assistant_end`（含 content + pending tool count）
- `ComponentOfTool` → 计算 duration + emit `tool_end`（含 result + status=success）
- 其他 → emit `done`

**OnError** (L347-387):
- `ComponentOfTool` → emit `tool_end`（status=error/timeout）
- 其他 → emit `error`

**OnEndWithStreamOutput — Token 流式** (L404-417):

```go
func (h *SSEHandler) OnEndWithStreamOutput(ctx context.Context, info *callbacks.RunInfo, out *schema.StreamReader[callbacks.CallbackOutput]) context.Context {
    if info.Component != components.ComponentOfChatModel {
        out.Close()
        return ctx
    }
    go h.drainStream(out)
    return ctx
}
```

**drainStream** (L419-440): goroutine 中循环 `out.Recv()`，对每个 chunk 调用 `einomodel.ConvCallbackOutput` 转换，提取 `mo.Message.Content`，emit `assistant_delta` 帧。

> **注意**: 当前 `toCallbackEmitter` ([runtime.go L880-883](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go#L880)) **丢弃了 assistant_start 和 assistant_delta 帧**，因为前端 SPA 尚未支持。仅 assistant_end/tool_start/tool_end/done/error 被转发。

### 5.3 PersistenceHandler

**文件**: [internal/manager/biz/aiops/graph/callbacks/persistence.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/persistence.go)

**PersistenceDeps** (L44-53):

```go
type PersistenceDeps struct {
    SessionID  string
    Repo       biz.SessionRepo
    Logger     *slog.Logger
    Registerer prometheus.Registerer
    Model      string  // LLM model id，写入 chat_messages.model 列
}
```

**事件映射** (L68-78 文档注释):
- `OnChatModelStart` → `flushIncompleteBatch`（自动愈合上一轮未完成的 tool_call stubs）
- `OnChatModelEnd` → INSERT chat_messages (role=assistant)
- `OnToolStart` → INSERT chat_tool_calls (status=pending)
- `OnToolEnd` → UPDATE chat_tool_calls + INSERT chat_messages (role=tool)

**使用 compose.GetToolCallID**: 通过 eino 的 `compose.GetToolCallID` 从 ctx 中提取工具调用 ID（L307 附近）。

**自动愈合** `flushIncompleteBatch` (L560-622): 若上一轮 assistant 有 tool_calls 但对应的 OnEnd 从未触发（并行 ToolsNode 丢了一个），在下一轮 OnStart 时补写 stub tool result，避免 strict provider（DeepSeek v4+）拒绝孤儿 tool 消息。

### 5.4 AuditHandler

**文件**: [internal/manager/biz/aiops/graph/callbacks/audit.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/audit.go)

**AuditDeps** (L22-26):

```go
type AuditDeps struct {
    Logger    *slog.Logger
    SessionID string
    UserID    uint64
}
```

**AuditHandler 结构** (L37-44):

```go
type AuditHandler struct {
    deps     AuditDeps
    chatTurn atomic.Int64
    startsMu sync.Mutex
    starts   map[string]auditStart
}
```

**行为**:
- 每个 ChatModel turn → slog INFO（prompt token 估算 + reply tokens）
- 每个 tool call → slog INFO（name + duration + status）
- **红线**: 用户原始 prompt 内容**永不**进入日志，仅记录计数和标识符

### 5.5 MetricsHandler

**文件**: [internal/manager/biz/aiops/graph/callbacks/metrics.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/metrics.go)

**MetricsDeps** (L20-22):

```go
type MetricsDeps struct {
    Registerer prometheus.Registerer
}
```

**Prometheus 指标** (L32-37):

| 指标名 | 类型 | Labels |
|--------|------|--------|
| `ongrid_tool_invocations_total` | Counter | name, result |
| `ongrid_tool_duration_seconds` | Histogram | name |
| `ongrid_graph_iterations_total` | Counter | result |
| `ongrid_chat_turns_total` | Counter | result |

**基数红线** (L30-31 注释): labels 限定为 {name, result} / {provider, result} / {result}，**永不**包含 user/tenant/session。

### 5.6 AlertDraftGuardHandler

**文件**: [internal/manager/biz/aiops/graph/callbacks/alert_draft_guard.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/callbacks/alert_draft_guard.go)

**作用**: 当用户意图是"创建告警规则"时，拦截模型-only 的文字草案（没有通过 `draft_config_change` 工具生成 `config_draft`/`draft_hash` 的草案），避免把自由文本误当成可确认的告警规则。

**AlertDraftGuardDeps** (L24-26):

```go
type AlertDraftGuardDeps struct {
    UserText string
}
```

**构造** `NewAlertDraftGuardHandler` (L38-43): 若 `looksLikeAlertRuleCreationRequest(UserText)` 为 false → 返回 nil（不安装 handler）。

**Needed** (L45-55): 仅监听 `ComponentOfChatModel` 和 `ComponentOfTool` 的 `TimingOnEnd`。

### 5.7 BudgetCallbackHandler

**文件**: [internal/pkg/llm/budget_callback.go](file:///d:/claude/ongrid/internal/pkg/llm/budget_callback.go)

**BudgetCallbackHandler 结构** (L64-76):

```go
type BudgetCallbackHandler struct {
    checker   BudgetChecker
    userID    uint64
    checks    atomic.Uint64
    rejects   atomic.Uint64
    records   atomic.Uint64
    tokensIn  atomic.Uint64
}
```

**构造** `NewBudgetCallbackHandler` (L81-83):

```go
func NewBudgetCallbackHandler(checker BudgetChecker, userID uint64) *BudgetCallbackHandler
```

**Needed** (L87-100): 仅 `ComponentOfChatModel` 的 `TimingOnStart` + `TimingOnEnd`。

**OnStart** (L105-123):
1. `einomodel.ConvCallbackInput(input)` 转换输入
2. `estimateEinoPromptTokens(mi.Messages)` 估算 prompt token（内容长度 / 4 + perMsgOverhead=4）
3. `checker.Check(ctx, userID, est)` — 若拒绝，存入 ctx（`budgetRejectKey`），**不返回 error**（eino 回调不支持短路）

**OnEnd** (L128-147):
1. `einomodel.ConvCallbackOutput(output)` 转换输出
2. `extractUsage(mo)` 提取 TokenUsage（优先 `mo.TokenUsage`，回退 `mo.Message.ResponseMeta.Usage`）
3. `checker.Record(ctx, userID, *usage)` — 记录失败被吞掉（不阻断用户请求）

**estimateEinoPromptTokens** (L199-214):

```go
func estimateEinoPromptTokens(msgs []*schema.Message) int {
    const perMsgOverhead = 4
    total := 0
    for _, m := range msgs {
        total += perMsgOverhead
        total += len(m.Content) / 4
        for _, tc := range m.ToolCalls {
            total += len(tc.Function.Name) / 4
            total += len(tc.Function.Arguments) / 4
        }
    }
    return total
}
```

---

## 6. Tool 接口与适配层

eino 的 Tool 接口：

```go
// components/tool
type BaseTool interface {
    Info(ctx context.Context) (*schema.ToolInfo, error)
}

type InvokableTool interface {
    BaseTool
    InvokableRun(ctx context.Context, argumentsInJSON string, opts ...Option) (string, error)
}
```

### 6.1 einoToolAdapter

**文件**: [internal/manager/biz/aiops/graph/tool_adapter.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/tool_adapter.go)

**结构定义** (L276-286):

```go
type einoToolAdapter struct {
    inner      basetool.BaseTool
    memo       *toolMemo      // 每 run 相同调用缓存
    infoOnce   sync.Once
    cacheName  string
    cacheable  bool           // Class == "read"
}
```

### 6.2 WrapBaseTool / WrapBaseTools

**WrapBaseTool** (L238-243): 单工具包装。

```go
func WrapBaseTool(t basetool.BaseTool) einotool.InvokableTool {
    return &einoToolAdapter{inner: t}  // memo = nil（测试用）
}
```

**WrapBaseTools** (L438-450): 批量包装，共享一个 `toolMemo`。

```go
func WrapBaseTools(tools []basetool.BaseTool) []einotool.BaseTool {
    memo := newToolMemo()
    out := make([]einotool.BaseTool, 0, len(tools))
    for _, t := range tools {
        if t == nil { continue }
        out = append(out, &einoToolAdapter{inner: t, memo: memo})
    }
    return out
}
```

### 6.3 Info 方法 — 工具元信息 (L305-342)

```go
func (a *einoToolAdapter) Info(ctx context.Context) (*schema.ToolInfo, error)
```

转换逻辑：
1. 调用 `a.inner.Info(ctx)` 获取 ongrid `basetool.ToolInfo`
2. 拼接 `Description` + `WhenToUse`（`"When to use: " + info.WhenToUse`）
3. 解析 `info.Parameters`（JSON Schema raw bytes）→ `jsonschema.Schema` → `schema.NewParamsOneOfByJSONSchema(js)`

**返回的 eino schema.ToolInfo**:

```go
out := &schema.ToolInfo{
    Name: info.Name,
    Desc: desc,
    ParamsOneOf: schema.NewParamsOneOfByJSONSchema(js),
}
```

### 6.4 InvokableRun — 工具执行 (L360-432)

```go
func (a *einoToolAdapter) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...einotool.Option) (string, error)
```

**执行流程**:
1. **相同调用缓存**（仅 read 工具）: `memoKey = cacheName + "\x00" + argumentsInJSON`，命中则直接返回
2. **每工具执行上限**: 若 `memo.count(name) >= maxCallsForTool(name)` → 返回 `toolBudgetExceeded` 合成结果
3. **draft_config_change 预检**: `draftMetricCatalogPreflight` 检查是否需要先调用 `list_metric_catalog`
4. **解析 eino Option**: `einotool.GetImplSpecificOptions(&einoInvokeOptKey{}, opts...)` 提取 `basetool.InvokeOption`
5. **调用 inner**: `a.inner.InvokableRun(ctx, argumentsInJSON, resolved.opts...)`
6. **错误包装** (L389-414): **Tool error 永远不作为 Go error 返回**，而是包装成 `{"error":"...","status":"failed"}` JSON。原因：eino 的 ToolsNode 把 Go error 视为 graph-fatal（终止整个 invoke + SSE 流），而 OnGrid 的不变式是"工具失败是 LLM 可恢复的事实"。
7. **成功计数 + 缓存**: `memo.bump(name)` + `memo.putLast(name, out)` + `memo.put(memoKey, out)`

### 6.5 WithInvokeOpts — eino Option 桥接 (L248-269)

```go
func WithInvokeOpts(opts ...basetool.InvokeOption) einotool.Option {
    return einotool.WrapImplSpecificOptFn(func(k *einoInvokeOptKey) {
        k.opts = append(k.opts, opts...)
    })
}
```

用途：将 ongrid 的 `basetool.InvokeOption`（`WithUserID` / `WithTenant` 等）通过 eino 的 `tool.Option` 系统传递到 ToolsNode 调用。

**使用示例**（注释 L256-264）:

```go
runnable.Invoke(ctx, in, compose.WithToolsNodeOption(
    compose.WithToolOption(graph.WithInvokeOpts(
        basetool.WithUserID(uid),
        basetool.WithTenant(tenantID),
    )),
))
```

### 6.6 toolMemo — 调用缓存与预算

**结构** (L27-32):

```go
type toolMemo struct {
    mu     sync.Mutex
    m      map[string]string  // (tool\x00args) -> result
    counts map[string]int     // tool name -> 执行次数
    last   map[string]string  // tool name -> 最近成功结果
}
```

**maxToolCallsPerRun** (L92): `30` — 每个工具在每个 agent run 中最多执行 30 次。

**maxCallsForTool** (L94-104): `draft_config_change` 特殊处理为 1 次（仅可确认的 config_draft 计数，validation_failed 可重试）。

---

## 7. Graph/Compose API 使用

### 7.1 使用的 compose API 清单

| API | 位置 | 用途 |
|-----|------|------|
| `compose.NewGraph[*Input, *Output]()` | react.go L135 | 创建泛型图 |
| `compose.InvokableLambda(fn)` | react.go L137, L148 | 创建同步 Lambda 节点 |
| `g.AddLambdaNode(name, lambda)` | react.go L140, L161 | 添加 Lambda 节点 |
| `g.AddGraphNode(name, subGraph, opts...)` | react.go L144 | 添加子图节点 |
| `g.AddEdge(from, to)` | react.go L165-174 | 添加边 |
| `compose.START` / `compose.END` | react.go L165, L174 | 图入口/出口哨兵 |
| `g.Compile(ctx, opts...)` | react.go L178 | 编译图为 Runnable |
| `compose.WithGraphName(name)` | react.go L179 | 编译选项：图名 |
| `compose.WithMaxRunSteps(n)` | react.go L182 | 编译选项：最大步数 |
| `compose.ToolsNodeConfig{Tools: ...}` | react.go L99-110 | ReAct 工具节点配置 |
| `compose.WithCallbacks(handlers...)` | Invoke 调用时 | 注册回调链 |
| `compose.WithToolsNodeOption(opt)` | 注释示例 | 工具节点选项 |
| `compose.WithToolOption(opt)` | 注释示例 | 工具选项 |
| `compose.GetToolCallID(ctx)` | persistence.go L307 | 从 ctx 提取 tool_call_id |

### 7.2 Runnable 调用

**文件**: [internal/manager/biz/aiops/chatruntime/runtime.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go)

chatruntime.Runtime.Handle 在构建好 graph + callback chain 后调用：

```go
// 构建 handler 链
handlers := callbacks.NewDefaultHandlers(deps)

// 调用 graph（伪代码，实际在 runGraph 中）
reply, err := runnable.Invoke(ctx, input, compose.WithCallbacks(handlers...))
```

**Callback 注册时机**: eino 的 graph compile API **不接受** handler 列表（react.go L70-77 注释），handler 通过 `compose.WithCallbacks` 在 **Invoke 调用时**传入，使得同一编译图可跨请求复用不同 handler 集。

### 7.3 chatModelOpts — 每请求模型选择 (runtime.go L1126-1138)

```go
func chatModelOpts(req *Request) []model.Option {
    var opts []model.Option
    if p := strings.TrimSpace(req.Provider); p != "" {
        opts = append(opts, llm.WithProvider(p))
    }
    if m := strings.TrimSpace(req.Model); m != "" {
        opts = append(opts, model.WithModel(m))
    }
    return opts
}
```

将前端 ModelDropdown 的选择转换为 eino `model.Option`：
- `llm.WithProvider(p)` → RoutingChatModel.pick 选择 inner
- `model.WithModel(m)` → clientChatModel.buildChatReq 覆盖模型名

---

## 8. Schema 类型使用

### 8.1 schema.Message

**构造点**:

| 构造方式 | 位置 | 用途 |
|---------|------|------|
| `schema.SystemMessage(sp)` | react.go L228 | 系统提示 |
| `schema.UserMessage(body)` | react.go L237, L245 | system-reminder + 用户文本 |
| `&schema.Message{Role: schema.RoleType(...), ...}` | runtime.go L986-991, L1001-1004 | 历史消息重建 |
| `&schema.Message{Role: schema.Assistant, Content: ...}` | budget_stop_model.go L58 | 预算停机合成消息 |
| `einoMessageFromChatResp(resp)` | eino_routing.go L460-492 | LLM 响应转换 |

**schema.Message 字段使用**:

```go
type Message struct {
    Role          RoleType           // schema.System/User/Assistant/Tool
    Content       string
    Name          string
    ToolCalls     []ToolCall         // assistant 的工具调用请求
    ToolCallID    string             // tool 消息的关联 ID
    ToolName      string
    ResponseMeta  *ResponseMeta      // 含 Usage
}
```

**schema.ToolCall** (eino_routing.go L474-481):

```go
schema.ToolCall{
    ID:   tc.ID,
    Type: "function",
    Function: schema.FunctionCall{
        Name:      tc.Name,
        Arguments: string(tc.Args),
    },
}
```

**schema.ResponseMeta.Usage** (eino_routing.go L484-490):

```go
m.ResponseMeta = &schema.ResponseMeta{
    Usage: &schema.TokenUsage{
        PromptTokens:     resp.Usage.PromptTokens,
        CompletionTokens: resp.Usage.CompletionTokens,
        TotalTokens:      resp.Usage.TotalTokens,
    },
}
```

### 8.2 schema.StreamReader

**使用点**:

| 位置 | 用途 |
|------|------|
| eino_routing.go L377 | `schema.StreamReaderFromArray([]*schema.Message{msg})` — 伪流式单 chunk |
| budget_stop_model.go L32 | 同上，预算停机时返回单 chunk 流 |
| sse.go L390, L404 | `*schema.StreamReader[callbacks.CallbackInput]` / `callbacks.CallbackOutput` — 回调流 |
| budget_callback.go L155-167 | drain & close 回调流（no-op） |

`schema.StreamReader[T]` 是 eino 的泛型流读取器，必须调用 `Close()` 释放资源。

### 8.3 schema.ToolInfo

**einoToolAdapter.Info 返回** (tool_adapter.go L324-340):

```go
out := &schema.ToolInfo{
    Name: info.Name,
    Desc: desc,
    ParamsOneOf: schema.NewParamsOneOfByJSONSchema(js),
}
```

**RoutingChatModel.WithTools 接收** `[]*schema.ToolInfo` (eino_routing.go L232)。

**clientChatModel.boundTools** 存储为 `[]*schema.ToolInfo` (eino_routing.go L330)。

### 8.4 schema.TokenUsage

在以下位置读取：
- `budget_callback.go L223-228`: 从 `einomodel.CallbackOutput.TokenUsage` 提取
- `budget_callback.go L232-238`: 从 `Message.ResponseMeta.Usage` 回退提取
- `react.go L150-153`: OutputProjector 从 `msg.ResponseMeta.Usage` 提取到 Output.Usage

### 8.5 ConvCallbackInput / ConvCallbackOutput

eino 提供的类型转换辅助函数：

| 函数 | 包 | 用途 |
|------|-----|------|
| `einomodel.ConvCallbackInput(input)` | components/model | `callbacks.CallbackInput` → `*model.CallbackInput` |
| `einomodel.ConvCallbackOutput(output)` | components/model | `callbacks.CallbackOutput` → `*model.CallbackOutput` |
| `einotool.ConvCallbackInput(input)` | components/tool | → `*tool.CallbackInput` |
| `einotool.ConvCallbackOutput(output)` | components/tool | → `*tool.CallbackOutput` |

使用位置：
- sse.go L249: `einotool.ConvCallbackInput(input)` — OnStart 提取 tool args
- sse.go L280: `einomodel.ConvCallbackOutput(output)` — OnEnd 提取 model message
- sse.go L308: `einotool.ConvCallbackOutput(output)` — OnEnd 提取 tool response
- sse.go L426: `einomodel.ConvCallbackOutput(chunk)` — drainStream 提取 delta
- budget_callback.go L112: `einomodel.ConvCallbackInput(input)` — OnStart 估算 token
- budget_callback.go L135: `einomodel.ConvCallbackOutput(output)` — OnEnd 提取 usage

---

## 9. 完整调用链路

```
前端 ChatThread.send()
  ↓ fetch POST /v1/chat/sessions/{id}/messages/stream
  ↓
http.go: postMessageStream (L414-477)
  ↓ SSE handshake (text/event-stream, X-Accel-Buffering: no)
  ↓
service.go: runWithKernel (L334-376)
  ↓ context.WithoutCancel (L353) — 脱离 HTTP 生命周期
  ↓ context.WithCancel (L358) — Esc 可中断
  ↓
service.go: runGraph (L434-467)
  ↓ translateRuntimeEvent — chatruntime.Event → agent.Event
  ↓
runtime.go: Runtime.Handle (L468-851)
  ├── 持久化 user message (L513-521)
  ├── 加载历史 buildEinoHistory (L939-1088)
  ├── 解析 skills + system prompt
  ├── 构建 Deps → NewDefaultHandlers (chain.go L88-118)
  │     [AlertDraftGuard → Persistence → SSE → Audit → Metrics → Budget]
  ├── BuildReActGraph(react.go L78-188)
  │     ├── wrapBudgetStopModel(model)
  │     ├── WrapBaseTools(tools) — einoToolAdapter 包装
  │     ├── react.NewAgent(reactCfg)
  │     ├── reactAgent.ExportGraph()
  │     ├── compose.NewGraph + AddLambdaNode + AddGraphNode + AddEdge
  │     └── g.Compile(WithGraphName, WithMaxRunSteps)
  └── runnable.Invoke(ctx, input, compose.WithCallbacks(handlers...))
        ↓
        eino ReAct 循环:
          ChatModel.Generate (clientChatModel → RoutingChatModel → llm.Client → OpenAI API)
            ↓ callback: OnStart → [SSE: assistant_start] [Audit] [Budget.Check]
            ↓ callback: OnEnd → [Persistence: INSERT assistant] [SSE: assistant_end] [Audit] [Metrics] [Budget.Record]
          若有 ToolCalls:
            ToolsNode 并行执行 einoToolAdapter.InvokableRun
              ↓ callback: OnStart → [Persistence: INSERT pending] [SSE: tool_start] [Audit]
              ↓ inner.InvokableRun (basetool 装饰器链)
              ↓ callback: OnEnd → [Persistence: UPDATE + INSERT tool msg] [SSE: tool_end] [Audit] [Metrics]
            → 回到 ChatModel（下一轮 ReAct 迭代）
          最终 assistant message（无 ToolCalls）:
            ↓ OutputProjector 提取 Usage
            ↓ callback: Graph OnEnd → [SSE: done]
  ↓
service.go: runtimeReplyToAgentReply (L466)
  ↓
http.go: writeSSE("done", ...) (L601-612)
  ↓ 关闭 HTTP response writer
```

---

## 10. 架构图示

```
┌─────────────────────────────────────────────────────────────────┐
│                        前端 ChatThread                           │
│  fetch + ReadableStream → dispatchFrame → onAssistant/onTool... │
└──────────────────────────────┬──────────────────────────────────┘
                               │ POST /messages/stream
┌──────────────────────────────▼──────────────────────────────────┐
│                    http.go: postMessageStream                    │
│            SSE handshake + writeSSE + eventName                  │
└──────────────────────────────┬──────────────────────────────────┘
                               │
┌──────────────────────────────▼──────────────────────────────────┐
│                  service.go: runWithKernel                       │
│      context.WithoutCancel + context.WithCancel                  │
│         (Esc 可停 / HTTP 断开不影响)                              │
└──────────────────────────────┬──────────────────────────────────┘
                               │
┌──────────────────────────────▼──────────────────────────────────┐
│              chatruntime/runtime.go: Runtime.Handle              │
│  持久化 user msg → 加载 history → 构建 Deps → Invoke graph       │
└──────────────────────────────┬──────────────────────────────────┘
                               │
          ┌────────────────────▼────────────────────┐
          │     graph/react.go: BuildReActGraph     │
          │                                          │
          │  START → MessageAssembler → ReActSubgraph → OutputProjector → END │
          │                                          │
          │  ReActSubgraph (eino react.Agent):       │
          │    ChatModel ↔ Branch(tool_calls?) ↔ ToolsNode │
          └────────────────────┬────────────────────┘
                               │
          ┌────────────────────▼────────────────────┐
          │         Callback 链 (Invoke 时注入)       │
          │                                          │
          │  [AlertDraftGuard] → [Persistence] → [SSE] → [Audit] → [Metrics] → [Budget] │
          │                                          │
          │  assistantIDRelay: Persistence → SSE      │
          └────────────────────┬────────────────────┘
                               │
          ┌────────────────────▼────────────────────┐
          │       ChatModel 层 (eino_routing.go)     │
          │                                          │
          │  RoutingChatModel (多 provider 路由)      │
          │    ├── clientChatModel (openai)          │
          │    ├── clientChatModel (anthropic)       │
          │    ├── clientChatModel (zhipu)           │
          │    └── clientChatModel (gemini/...)      │
          │          ↓                               │
          │    budgetStopModel (装饰器)               │
          │          ↓                               │
          │    llm.Client (sashabaranov/go-openai)   │
          └────────────────────┬────────────────────┘
                               │
          ┌────────────────────▼────────────────────┐
          │       Tool 层 (tool_adapter.go)          │
          │                                          │
          │  einoToolAdapter (eino InvokableTool)    │
          │    ├── Info() → schema.ToolInfo          │
          │    └── InvokableRun() → basetool         │
          │         ├── toolMemo (相同调用缓存)       │
          │         ├── maxToolCallsPerRun=30 (预算)  │
          │         └── error → JSON envelope        │
          └─────────────────────────────────────────┘
```

---

## 附录：eino API 速查表

### model 包

| API | 签名 | 用途 |
|-----|------|------|
| `model.Option` | `func(*Options)` | 选项函数类型 |
| `model.Options` | struct | 通用选项（Temperature, Model, Tools, MaxTokens, Stop, TopP） |
| `model.GetCommonOptions` | `(*Options, ...Option) *Options` | 读取通用选项 |
| `model.GetImplSpecificOptions` | `(*T, ...Option) *T` | 读取实现特定选项 |
| `model.WrapImplSpecificOptFn` | `func(*T) Option` | 包装实现特定选项 |
| `model.WithModel(m)` | `Option` | 设置模型名 |
| `model.WithTemperature(t)` | `Option` | 设置温度 |
| `model.WithTools(tools)` | `Option` | 设置工具列表 |

### compose 包

| API | 用途 |
|-----|------|
| `compose.NewGraph[I,O]()` | 创建泛型有向图 |
| `compose.InvokableLambda(fn)` | 创建同步 Lambda 节点 |
| `g.AddLambdaNode(name, lambda)` | 添加 Lambda 节点 |
| `g.AddGraphNode(name, subGraph, opts...)` | 添加子图节点 |
| `g.AddEdge(from, to)` | 添加边 |
| `g.Compile(ctx, opts...)` | 编译为 Runnable |
| `compose.START` / `compose.END` | 图入口/出口 |
| `compose.WithGraphName(name)` | 编译选项 |
| `compose.WithMaxRunSteps(n)` | 编译选项 |
| `compose.WithCallbacks(handlers...)` | Invoke 时回调 |
| `compose.ToolsNodeConfig{Tools, UnknownToolsHandler}` | 工具节点配置 |
| `compose.GetToolCallID(ctx)` | 从 ctx 提取 tool_call_id |

### react 包

| API | 用途 |
|-----|------|
| `react.NewAgent(ctx, *AgentConfig)` | 创建 ReAct Agent |
| `react.AgentConfig` | 配置（ToolCallingModel, ToolsConfig, MaxStep, GraphName, ModelNodeName, ToolsNodeName） |
| `agent.ExportGraph()` | 导出内部 compose.AnyGraph |

### callbacks 包

| API | 用途 |
|-----|------|
| `callbacks.Handler` | 回调接口（OnStart/OnEnd/OnError/Stream 变体） |
| `callbacks.TimingChecker` | Timing 过滤接口 |
| `callbacks.RunInfo` | 运行信息（Component, Name） |
| `callbacks.CallbackInput` / `CallbackOutput` | 回调输入/输出（`any`） |
| `callbacks.CallbackTiming` | Timing 枚举（OnStart/OnEnd/OnError/OnEndWithStreamOutput） |

### schema 包

| API | 用途 |
|-----|------|
| `schema.Message` | 消息结构 |
| `schema.SystemMessage(s)` / `schema.UserMessage(s)` | 消息构造快捷函数 |
| `schema.RoleType` | 角色枚举 |
| `schema.ToolInfo` | 工具元信息 |
| `schema.ToolCall` / `schema.FunctionCall` | 工具调用 |
| `schema.ParamsOneOf` | 参数 Schema（OneOf） |
| `schema.NewParamsOneOfByJSONSchema(*jsonschema.Schema)` | 从 JSON Schema 构造 |
| `schema.StreamReader[T]` | 泛型流读取器 |
| `schema.StreamReaderFromArray([]T)` | 从数组构造单/多 chunk 流 |
| `schema.ResponseMeta` | 响应元数据 |
| `schema.TokenUsage` | Token 用量 |

### components 包

| 常量 | 值 | 用途 |
|------|-----|------|
| `components.ComponentOfChatModel` | "ChatModel" | 回调过滤 |
| `components.ComponentOfTool` | "Tool" | 回调过滤 |
| `components.ComponentOfGraph` | "Graph" | 回调过滤 |

---

*文档生成时间: 2026-08-03*
*分析基于 OnGrid 代码库当前状态*
