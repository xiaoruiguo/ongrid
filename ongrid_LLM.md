# OnGrid LLM 技术实现说明文档

> 本文档深入分析 OnGrid 系统中与 LLM（大语言模型）相关的全部技术实现，覆盖：LLM 客户端基础架构、嵌入层、MCP 客户端、AIOps chatruntime 编排、ReAct 图构造、callbacks 链、工具注册表与 decorators、LLM 设置与探测、双 kernel 派发、Investigator 与 structured RCA、知识库 RAG、Report 反幻觉生成、IM bridge、装配流程、并发模型与架构红线。

---

## 目录

1. [概述与架构红线](#1-概述与架构红线)
2. [LLM 客户端基础架构](#2-llm-客户端基础架构)
3. [多 Provider 路由（MultiClient）](#3-多-provider-路由multiclient)
4. [预算门控（Budget）](#4-预算门控budget)
5. [eino 路由适配与监控](#5-eino-路由适配与监控)
6. [嵌入层与 MCP 客户端](#6-嵌入层与-mcp-客户端)
7. [智谱 JWT 认证](#7-智谱-jwt-认证)
8. [AIOps chatruntime 编排](#8-aiops-chatruntime-编排)
9. [graph ReAct 图构造](#9-graph-react-图构造)
10. [callbacks 链](#10-callbacks-链)
11. [工具注册表与 ToolBag](#11-工具注册表与-toolbag)
12. [BaseTool 基类与 ctx 传播](#12-basetool-基类与-ctx-传播)
13. [decorators 装饰器链](#13-decorators-装饰器链)
14. [关键工具实现](#14-关键工具实现)
15. [LLM 设置与探测](#15-llm-设置与探测)
16. [service/aiops 双 kernel 派发](#16-serviceaiops-双-kernel-派发)
17. [Investigator 与 structured RCA](#17-investigator-与-structured-rca)
18. [告警规则 AI 辅助编辑](#18-告警规则-ai-辅助编辑)
19. [知识库 RAG](#19-知识库-rag)
20. [Report 反幻觉生成](#20-report-反幻觉生成)
21. [Flow 生成与 IM Bridge](#21-flow-生成与-im-bridge)
22. [cmd/ongrid/main.go 装配流程](#22-cmdongridmaingo-装配流程)
23. [并发与资源管理](#23-并发与资源管理)
24. [设计模式与架构红线](#24-设计模式与架构红线)

---

## 1. 概述与架构红线

OnGrid 的 LLM 子系统是一个围绕 **OpenAI-compatible Chat Completion + tool calling** 形状构建的、多 provider 可路由、带预算门控、可观测、可热更新、可降级的智能体运行时。它由以下几大模块组成：

| 层次 | 模块 | 职责 |
|------|------|------|
| 客户端基础 | `internal/pkg/llm/` | OpenAI SDK 封装、Resolver 热更新、预算门控、Prometheus 监控、Zhipu JWT 适配、reasoning model 响应式自愈 |
| 嵌入与 MCP | `internal/pkg/embedding/`、`internal/pkg/mcpclient/` | 向量嵌入（OpenAI 兼容 + 本地 ONNX fastembed）、MCP Streamable HTTP 客户端 |
| AIOps 编排 | `internal/manager/biz/aiops/chatruntime/`、`internal/manager/biz/aiops/graph/` | 10 步主流程、Worker 状态机、Skill/Agent 注册、eino ReAct 图构造、budget stop model |
| 回调链 | `internal/manager/biz/aiops/graph/callbacks/` | AlertDraftGuard → Persistence → SSE → Audit → Metrics → Budget 6 个 handler |
| 工具系统 | `internal/manager/biz/aiops/tools/` | 双实现注册表、ToolBag two-tier deferral、tool_search 元工具、BaseTool 基类、7 个 ctx 传播子包、7 个 decorators |
| LLM 设置 | `internal/manager/biz/setting/` | provider 解析器、健康探针、AgentWriteEnabled fail-safe |
| 服务派发 | `internal/manager/service/aiops/` | 双 kernel 派发（Legacy / Graph）、HLD-021 turn 解耦、自定义 persona |
| 业务消费 | `investigator`、`alertconfig`、`alertdraft`、`knowledge`、`report`、`flow`、`imbridge` | legacy AI initial diagnosis、AI 辅助告警编辑、知识库 RAG、反幻觉报告、Flow 生成、IM 桥接 |

### 三条架构红线（来自 `internal/pkg/llm/doc.go`）

1. **永不记录用户消息内容** —— Prom label 仅 `model/kind/result`，日志仅记 `model/user_id/tokens/duration/tool_calls`，content 永不出现在 log/metrics/audit 任何地方。
2. **预算门控前置** —— 网络调用前 `budget.Check`，超限直接返回 `ErrBudgetExceeded`，不消耗 API 配额；`Record` 失败仅 Warn，不 fail 请求（用户优先）。
3. **无 provider 抽象** —— 接口形状跟随 OpenAI；所有 provider 走 OpenAI-compatible 端点（OpenAI/Anthropic/Zhipu/Gemini/DeepSeek/Kimi/Custom），通过 `MultiClient` 路由。

---

## 2. LLM 客户端基础架构

源文件：`internal/pkg/llm/client.go`

### 2.1 核心类型

```go
type Config struct {
    APIKey, Model, BaseURL string
    Timeout time.Duration
}

type Message struct {
    Role, Content string
    ToolCalls     []ToolCall
    ToolCallID, ToolName string
}

type ToolCall struct {
    ID, Name string
    Args     json.RawMessage
}

type ToolSchema struct {
    Name, Description string
    Parameters        json.RawMessage
}

type Usage struct {
    PromptTokens, CompletionTokens, TotalTokens int
}

type ChatReq struct {
    Model, Provider string
    Messages        []Message
    Tools           []ToolSchema
    Temperature     float32
    UserID          uint64
}

type ChatResp struct {
    Assistant Message
    Usage     Usage
}

type BudgetChecker interface {
    Check(ctx, userID uint64, estPromptTokens int) error
    Record(ctx, userID uint64, usage Usage) error
}

type Client interface {
    Chat(ctx context.Context, req ChatReq) (*ChatResp, error)
}

type Resolver interface {
    Resolve(ctx) (apiKey, model, baseURL string, err error)
}
```

Sentinel：`ErrBudgetExceeded`、`ErrNoAPIKey`。`defaultTimeout = 120 * time.Second`（从 30s 提到 120s，因 DeepSeek v4 reasoning 等慢模型）。

### 2.2 openaiClient.Chat 17 步流程

1. `effectiveCreds(ctx)` 解析凭据（resolver 覆盖 cfg；空回退 cfg；TTL 60s 缓存）
2. apiKey 空 → `ErrNoAPIKey`
3. model 空 → 用 defaultModel
4. **预算门控**：`budget.Check(ctx, req.UserID, estimatePromptTokens(req.Messages))`；失败 metrics `budget_exceeded` + Warn + 返回 `ErrBudgetExceeded`
5. `toOpenAIReq(req, model)` 翻译；失败 `%w`
6. ctx 无 deadline → `context.WithTimeout(ctx, cfg.Timeout)`
7. `sdkFor(apiKey, baseURL)` 取缓存 SDK 实例
8. `sdk.CreateChatCompletion(callCtx, sdkReq)`，记录 start
9. **响应式自愈**：err 是 sampling param error 且 req 带自定义采样 → `rememberNoSampling(model)` + `stripSamplingParams(&sdkReq)` + 重试一次
10. metrics `requestSeconds` 观察
11. err 非 nil → metrics `error` + Error log + `%w`
12. `len(Choices)==0` → metrics `error` + 报错
13. `fromOpenAIMessage` 翻译响应
14. metrics tokensTotal (prompt/completion) + requestsTotal `success`
15. `budget.Record(ctx, req.UserID, usage)`；失败 Warn（不 fail 请求）
16. Info log（model, user_id, tokens, tool_calls, duration — **永不记录 content**）
17. 返回 `&ChatResp{Assistant, Usage}`

### 2.3 effectiveCreds —— Resolver 热更新

- resolver nil → 直接返回 cfg
- 否则检查 `resolvedAt` + TTL（60s）；过期调 `resolver.Resolve(ctx)`
- 失败 Warn + 回退 cfg；空字段回退 cfg
- 缓存 `resolved`

**含义**：admin 编辑 system_settings 后 60s 内生效；可通过 `MultiClient.Invalidate` 立即刷新。

### 2.4 sdkFor —— SDK 实例缓存

- 按 (apiKey, baseURL) key 缓存，跨调用复用连接池
- `normalizeOpenAIBaseURL`：path 为空时补 `/v1`，适配 Ollama / vLLM 裸地址
- 若 `LooksLikeZhipuURL(baseURL) && LooksLikeZhipuKey(apiKey)` → 装 `zhipuJWTTransport`

### 2.5 reasoning model 采样响应式自愈

```go
func isReasoningModel(model string) bool {
    // 名字启发式：o1/o3/o4/gpt-5/kimi-k2/kimi-k3/reasoner/reasoning 前缀
}

func modelRejectsSampling(model string) bool {
    // 查 noSampling map（reactive 学习）
}

func rememberNoSampling(model string) {
    // 写入 map
}

func isSamplingParamError(err error) bool {
    // err msg 含 "temperature"/"top_p"/"sampling" 且含
    // "fixed at 1"/"only 1 is allowed"/"beta-limitations"/... → true
}

func stripSamplingParams(req *openai.ChatCompletionRequest) {
    // 全部清零
}
```

**机制**：名字启发式 + reactive 学习。首次 400 后记 map + 重试，后续主动省采样参数。这是唯一允许的重试路径（注释明示 "no auto-retry (tools are not idempotent)"）。

### 2.6 estimatePromptTokens —— 粗估

~4 字符/token + per-msg overhead 4；含 ToolCall.Name/Arguments。仅用于预算门控，真实计费是返回的 Usage。

### 2.7 noopClient —— Null Object 降级

无 APIKey 时返回 noopClient，caller 拿 `ErrNoAPIKey` 而非 401。保证系统在未配置 LLM 的情况下仍可启动并优雅降级。

---

## 3. 多 Provider 路由（MultiClient）

源文件：`internal/pkg/llm/router.go`

### 3.1 设计

```go
type MultiClient struct {
    clients map[string]Client  // provider name → Client
    defaultClient Client
    catalog map[string]string  // provider → model
    mu sync.RWMutex
    catalogTTL time.Duration
    lastCatalogLoad time.Time
}
```

### 3.2 关键特性

- **按 provider 路由**：`ChatReq.Provider` 非空时路由到对应子 Client；否则用 defaultClient
- **动态 catalog**：从 admin 设置加载 provider → model 映射，60s TTL 缓存
- **空 catalog authoritative**：catalog 为空时视为"未配置"，不报错，由 caller 决定降级行为
- **`Invalidate()`**：admin 改设置后立即刷新缓存（与 client.go 的 60s TTL 协同）

### 3.3 noopClient 降级

当 `MultiClient` 无任何子 Client 时，返回 noopClient；caller 拿 `ErrNoAPIKey` 而非 panic。

---

## 4. 预算门控（Budget）

源文件：`internal/pkg/llm/budget.go`、`internal/pkg/llm/budget_callback.go`

### 4.1 InMemoryBudget

```go
type InMemoryBudget struct {
    mu sync.Mutex
    dailyLimit int
    used map[time.Time]int  // UTC day → used tokens
}
```

- **per-UTC-day 上限**：每个 UTC 日独立计数
- **`Check(ctx, userID, estPromptTokens)`**：估算 + 已用 vs dailyLimit
- **`Record(ctx, userID, usage)`**：扣减实际 Usage
- **reset**：UTC 日切换自动归零（map key 是 UTC date）

### 4.2 BudgetCallbackHandler —— eino-side 预算回调

```go
type BudgetCallbackHandler struct {
    budget BudgetChecker
    used   atomic.Uint64
    limit  uint64
}
```

- **atomic.Uint64 计数**：lock-free 累加
- **OnEnd**：累加 prompt + completion tokens
- **超过 limit**：设置 stop flag，下一个 turn 短路（见 §9 budget_stop_model）

### 4.3 双层预算门控

| 层 | 位置 | 时机 | 失败行为 |
|---|---|---|---|
| 预算前置 | `openaiClient.Chat` step 4 | 网络调用前 | 返回 `ErrBudgetExceeded`，不消耗 API 配额 |
| eino 回调 | `BudgetCallbackHandler.OnEnd` | 每个 turn 结束 | 设置 stop flag，下一 turn 短路 |

---

## 5. eino 路由适配与监控

源文件：`internal/pkg/llm/eino_routing.go`、`internal/pkg/llm/metrics.go`、`internal/pkg/llm/probe.go`、`internal/pkg/llm/noop.go`

### 5.1 RoutingChatModel + clientChatModel

```go
type RoutingChatModel struct {
    router *MultiClient
}

type clientChatModel struct {
    client Client
    model  string
}
```

- **RoutingChatModel** 实现 eino `model.ChatModel` 接口，内部按 provider 路由
- **clientChatModel** 适配器：把 `Client.Chat` 翻译成 eino `Generate` 形态
- 这是 eino ReAct 图与 OnGrid LLM Client 之间的桥

### 5.2 Prometheus 指标（metrics.go）

```
ongrid_llm_requests_total{model,kind,result}
ongrid_llm_tokens_total{model,kind}        // kind: prompt|completion
ongrid_llm_request_seconds{model}          // histogram
```

**Label 严格限制**：仅 `model/kind/result`，禁 `user_id/org_id/session_id`（高基数）。

### 5.3 ProbeChatCompletion（probe.go）

- **20s 超时**：探测请求独立短超时
- **跳过 metrics/log**：探测不污染生产指标
- **用途**：`setting/llm_probe.go` 健康检查、admin 设置编辑后验证

### 5.4 noopClient（noop.go）

Null Object 模式：`Chat` 永远返回 `ErrNoAPIKey`。保证未配置 LLM 时系统可启动。

---

## 6. 嵌入层与 MCP 客户端

### 6.1 嵌入层（internal/pkg/embedding/）

源文件：`embedding.go`、`local.go`

```go
type Embedder interface {
    Embed(ctx context.Context, texts []string) ([][]float32, error)
    Dim() int
}
```

**两种实现**：

| 实现 | 文件 | 说明 |
|------|------|------|
| OpenAI 兼容 | `embedding.go` | 走 OpenAI `/embeddings` 端点，多 provider 可路由 |
| 本地 ONNX | `local.go` | fastembed，BGE-small-zh-v1.5，dim=512，`maxLocalEmbedChars=350` |

**本地 ONNX 特性**：
- 模型内嵌或外部加载
- `maxLocalEmbedChars=350`：超长文本截断
- 零外部 API 依赖，离线可用
- 用于知识库 RAG 的 embedding 生成

### 6.2 MCP 客户端（internal/pkg/mcpclient/）

源文件：`client.go`

**MCP（Model Context Protocol）Streamable HTTP 客户端**：

- **协议**：JSON-RPC 2.0 over HTTP
- **响应形态**：支持 SSE 双响应（streaming + 完整 JSON）
- **零外部依赖**：手写 HTTP client，不依赖 MCP SDK
- **工具调用**：`tools/call` method
- **资源读取**：`resources/read` method

**集成**：通过 `tools/mcp_basetool.go` 注册到工具系统，LLM 可调用外部 MCP server 暴露的工具。

---

## 7. 智谱 JWT 认证

源文件：`internal/pkg/zhipuauth/zhipuauth.go`

### 7.1 SignJWT

```go
func SignJWT(apiKey string, ttl time.Duration) (string, error)
```

**手写 HMAC-SHA256 JWT**（非标准 claims）：

- **Header**：`{"alg":"HS256","sign_type":"SIGN"}`
- **Payload**：`{"api_key":<id>, "exp":<ms>, "timestamp":<ms>}` —— **毫秒时间戳**（非标准 JWT 的秒）
- **签名**：`Base64(header) + "." + Base64(payload)` → HMAC-SHA256 → Base64
- **Token**：`<signature>.<header>.<payload>` —— **顺序非标准**（signature 在前）

### 7.2 集成

- `client.go` 的 `sdkFor` 通过 `LooksLikeZhipuURL` + `LooksLikeZhipuKey` 探测
- 自动装 `zhipuJWTTransport`，每次请求重签 JWT（1h TTL）
- caller 无感

### 7.3 注意

- 非标准 JWT，不能复用通用 JWT 库
- 毫秒时间戳易踩坑
- token 顺序 signature 在前

---

## 8. AIOps chatruntime 编排

源文件：`internal/manager/biz/aiops/chatruntime/`

### 8.1 Runtime.Handle 10 步主流程（runtime.go）

```go
func (r *Runtime) Handle(ctx, req *Request) (*Response, error)
```

1. **校验**：req 字段合法性
2. **加载 Skill/Agent/Pack/Policy**：`LoadAll` 统一加载器
3. **探测插件容器**：`pluginContainer` 探测 openclaw > claude > bare-skills
4. **构造 system_prompt**：分层拼装（agent persona + skill + tool list + locale + bound credentials）
5. **构造工具列表**：ToolBag two-tier deferral（core/specialty 分区 + ToolSearch）
6. **创建 Worker**：pending → running → terminal 状态机
7. **派发到 Kernel**：Legacy（agent.Agent 660 行 for-loop）或 Graph（chatruntime.Runtime + eino ReAct）
8. **SSE 流式输出**：callbacks/sse.go 推送 turn 事件
9. **持久化**：callbacks/persistence.go 保存 messages
10. **审计 + Metrics**：callbacks/audit.go + callbacks/metrics.go

### 8.2 Worker 状态机（worker.go）

```
pending → running → terminal
                  ↘ failed
                  ↘ cancelled
                  ↘ budget_exceeded
```

- **pending**：已创建未启动
- **running**：LLM 循环中
- **terminal**：正常完成
- **failed**：错误终止
- **cancelled**：用户取消或 ctx cancel
- **budget_exceeded**：预算超限

### 8.3 核心类型（types.go）

```go
type Skill struct {
    Name, Description string
    SystemPrompt string
    Tools []ToolDecl
    // ...
}

type Agent struct {
    Persona string
    SystemPrompt string
    // ...
}

type Pack struct { /* skill + agent bundle */ }
type Policy struct { /* tool policy */ }
type ToolDecl struct { /* tool declaration */ }
```

### 8.4 system_prompt 分层拼装（system_prompt.go）

```
1. Agent persona（自定义或内置）
2. Skill system prompt（SKILL.md 加载）
3. Tool list 描述
4. Locale（i18n 跟随）
5. Bound credentials 提示
6. 其他 ctx 传播内容
```

### 8.5 LoadAll 统一加载器（load_all.go）

- 一次性加载所有 Skill/Agent/Pack/Policy
- 从 DB + builtin vault + marketplace 加载
- 注册到对应 Registry

### 8.6 插件容器探测（plugin_container.go）

```
openclaw > claude > bare-skills
```

- **openclaw**：完整插件容器，支持 command + skill + agent
- **claude**：claude command 适配（command_parser.go）
- **bare-skills**：仅 SKILL.md，无 command

### 8.7 SKILL.md 解析（skill_parser.go）

- YAML frontmatter + Markdown body
- frontmatter → Skill 元数据（name/description/tools/...）
- body → SystemPrompt
- SkillRegistry RWMutex + 值拷贝快照（skill_registry.go）

### 8.8 agent_parser / agent_registry

- agent persona 解析（agent_parser.go）
- AgentRegistry 管理内置 + 自定义 agent（agent_registry.go）

### 8.9 command_parser（claude command 适配）

- 解析 claude command 格式
- 转换为 OnGrid Skill 形态

### 8.10 config_confirm —— 快捷路径绕过 LLM（config_confirm.go）

- 某些 config 操作（如确认 / 取消）不需要 LLM
- 直接走规则路径，绕过 LLM 调用
- 节省 token + 降低延迟

---

## 9. graph ReAct 图构造

源文件：`internal/manager/biz/aiops/graph/`

### 9.1 BuildReActGraph（react.go）

```go
func BuildReActGraph(cfg Config) (*graph.Graph, error)
```

基于 **eino ReAct** 模式构造：

```
User Input → LLM Reasoning → Tool Call? 
                                ├─ Yes → Tool Execute → LLM Reasoning (loop)
                                └─ No  → Final Answer
```

- **Input**：messages + tools + config
- **Output**：final assistant message
- **Config**：LLM model、tool list、callbacks、budget

### 9.2 tool_adapter + toolMemo（tool_adapter.go）

```go
type toolAdapter struct {
    tools map[string]tool.BaseTool
    memo  *toolMemo
}

type toolMemo struct {
    // 记录已调用工具，避免重复
}
```

- 适配 OnGrid Tool（BaseTool）到 eino `tool.BaseTool` 接口
- toolMemo 记录已调用工具，防重复

### 9.3 budget_stop_model —— 预算停止装饰器（budget_stop_model.go）

```go
type budgetStopModel struct {
    inner  model.ChatModel
    stop   *atomic.Bool
}
```

- **透明装饰器**：包装 `model.ChatModel`
- **stop flag**：由 `BudgetCallbackHandler` 设置
- **短路**：stop=true 时直接返回错误，不调用 LLM
- 下一 turn 预算超限时，避免无效 API 调用

### 9.4 Input/Output/Config（types.go）

```go
type Input struct {
    Messages []Message
    Tools    []tool.BaseTool
}

type Output struct {
    Answer Message
}

type Config struct {
    Model     model.ChatModel
    Callbacks []callbacks.Handler
    MaxTurns  int
}
```

---

## 10. callbacks 链

源文件：`internal/manager/biz/aiops/graph/callbacks/`

### 10.1 chain.go —— 链式组合

```go
func NewChain(handlers ...Handler) *Chain
```

按顺序调用所有 handler，任一返回 error 中断。

### 10.2 6 个 Handler

| 顺序 | Handler | 文件 | 职责 |
|------|---------|------|------|
| 1 | AlertDraftGuard | `alert_draft_guard.go` | 阻止非告警上下文写 alert draft |
| 2 | Persistence | `persistence.go` | 保存每条 message 到 DB |
| 3 | SSE | `sse.go` | 推送 turn 事件到前端 |
| 4 | Audit | `audit.go` | 审计日志（不含 content） |
| 5 | Metrics | `metrics.go` | Prometheus 指标 |
| 6 | Budget | `budget.go`（在 budget_callback.go） | 累加 tokens，超限设 stop flag |

### 10.3 设计要点

- **顺序敏感**：AlertDraftGuard 必须最先，防止越权写
- **Persistence 在 SSE 前**：先存后推，前端收到的事件一定可查
- **Audit 不含 content**：遵守架构红线
- **Budget 最后**：累加完才检查 stop

---

## 11. 工具注册表与 ToolBag

源文件：`internal/manager/biz/aiops/tools/`

### 11.1 双实现注册表

| 实现 | 文件 | 用途 |
|------|------|------|
| 闭包路径 | `registry.go` | 函数式注册，轻量 |
| BaseTool 路径 | `registry_basetool.go` | 类型安全，支持 decorators |

两套实现并存，因历史演进：早期是闭包，后引入 BaseTool 支持 decorators。新工具推荐 BaseTool。

### 11.2 ToolBag —— Two-tier deferral（toolbag.go）

```go
type ToolBag struct {
    core      []tool.BaseTool  // 始终加载
    specialty []tool.BaseTool  // 按需加载
    search    *ToolSearchTool   // 元工具
}
```

**核心思想**：

- **core**：基础工具（query/log/bash 等），始终暴露给 LLM
- **specialty**：专业工具（k8s/host_files/...），不直接暴露
- **ToolSearch**：元工具，LLM 调用它搜索 specialty 工具，按需加载

**收益**：避免 tool schema 过多撑爆 context window（DeepSeek v4 strict provider 400 错误的根因之一）。

### 11.3 ToolSearch 元工具（tool_search_tool.go）

```go
type ToolSearchTool struct {
    bag *ToolBag
}

// LLM 调用：tool_search(query="k8s logs")
// 返回：匹配的 specialty 工具的 schema
// LLM 再调用具体工具
```

---

## 12. BaseTool 基类与 ctx 传播

源文件：`internal/manager/biz/aiops/tools/basetool/`

### 12.1 BaseTool（basetool.go）

```go
type BaseTool struct {
    name, description string
    paramsSchema      json.RawMessage
    handler           func(ctx, args) (string, error)
}
```

实现 eino `tool.BaseTool` 接口。

### 12.2 7 个 ctx 传播子包

| 子包 | 文件 | 传播内容 |
|------|------|----------|
| artifact_source | `artifact_source.go` | 工件来源（哪个 turn 产生） |
| bound_credentials | `bound_credentials.go` | 绑定的凭据 |
| filtered_tools | `filtered_tools.go` | 过滤后的工具列表 |
| host_write | `host_write.go` | 主机写权限标记 |
| llm_choice | `llm_choice.go` | LLM 选择（provider/model） |
| locale | `locale.go` | i18n locale |
| session | `session.go` | session ID |

**设计**：每个子包提供 `WithXxx(ctx, xxx)` 和 `FromContext(ctx)` 对，工具通过 ctx 拿到运行时上下文，避免在 tool schema 中暴露这些参数给 LLM。

---

## 13. decorators 装饰器链

源文件：`internal/manager/biz/aiops/tools/decorators/`

### 13.1 chain.go —— 装饰器组合

```go
func Chain(decorators ...Decorator) Decorator
```

### 13.2 7 个 Decorator

| 顺序 | Decorator | 文件 | 职责 |
|------|-----------|------|------|
| 1 | tenant_bind | `tenant_bind.go` | 注入 tenant_id 到 ctx |
| 2 | review_gate | `review_gate.go` | 高危操作走审批（approval） |
| 3 | timeout | `timeout.go` | 工具执行超时控制 |
| 4 | audit | `audit.go` | 审计日志 |
| 5 | ratelimit | `ratelimit.go` | 速率限制 |
| 6 | metric | `metric.go` | Prometheus 指标 |
| 7 | （业务装饰器按需） | — | 如 host_write 检查 |

### 13.3 执行顺序

```
请求 → tenant_bind → review_gate → timeout → audit → ratelimit → metric → 实际工具
```

- **tenant_bind 最先**：后续装饰器需要 tenant_id
- **review_gate 次之**：高危操作拦截
- **timeout**：包裹实际执行
- **audit/metric 最后**：记录结果

---

## 14. 关键工具实现

### 14.1 agent_tool —— 委托给子 agent（agent_tool.go）

LLM 调用此工具可启动子 agent 处理复杂任务，实现 agent 嵌套。

### 14.2 bash_basetool / cloud_bash_basetool

- `bash_basetool.go`：在 edge agent 上执行 bash
- `cloud_bash_basetool.go`：在云端执行 bash（无 edge）
- 都走 decorators 链（review_gate 必走）

### 14.3 query_* 系列

| 工具 | 文件 | 数据源 |
|------|------|--------|
| query_promql | `query_promql.go` + `_basetool.go` | Prometheus |
| query_logql | `query_logql.go` + `_basetool.go` | Loki |
| query_traceql | `query_traceql.go` + `_basetool.go` | Tempo |
| query_k8s_logs | `query_k8s_logs.go` | k8s logs |
| query_k8s_snapshot | `query_k8s_snapshot.go` | k8s 资源快照 |
| query_alert_rules | `query_alert_rules.go` + `_basetool.go` | 告警规则 |
| query_change_events | `query_change_events_basetool.go` | 变更事件 |
| query_edges | `query_edges.go` + `_basetool.go` | edge 列表 |
| query_incidents | `query_incidents.go` + `_basetool.go` | 事件列表 |
| query_knowledge | `query_knowledge_basetool.go` | 知识库 RAG |

### 14.4 host_* 系列

| 工具 | 文件 | 用途 |
|------|------|------|
| host_files | `host_files_basetool.go` + `host_files_register.go` | 文件操作 |
| host_load | `host_load.go` + `_basetool.go` | 负载 |
| host_processes | `host_processes.go` + `_basetool.go` | 进程列表 |

### 14.5 topology / incident 系列

| 工具 | 文件 |
|------|------|
| get_topology / get_topology_basetool | 拓扑图 |
| expand_topology_basetool | 展开拓扑 |
| find_topology_node_basetool | 查找拓扑节点 |
| get_edge_summary / _basetool | edge 概要 |
| get_incident_detail / _basetool | 事件详情 |
| correlate_incident / _basetool | 关联事件 |
| find_outlier_edges / _basetool | 异常 edge |
| rank_edges / _basetool | edge 排序 |

### 14.6 其他工具

| 工具 | 文件 | 用途 |
|------|------|------|
| analyze_database_status | `analyze_database_status.go` | 数据库状态分析 |
| code_source_basetool | `code_source_basetool.go` | 代码浏览 |
| config_tools | `config_tools.go` | 配置工具 |
| describe_k8s_resource | `describe_k8s_resource.go` | k8s 资源描述 |
| device_resolver | `device_resolver.go` | 设备解析 |
| execute_k8s_action | `execute_k8s_action.go` | k8s 操作 |
| inventory_bridge | `inventory_bridge.go` | 资产桥接 |
| install_skill_basetool | `install_skill_basetool.go` | 安装 skill |
| mcp_basetool | `mcp_basetool.go` | MCP 工具桥接 |
| metric_catalog_tool | `metric_catalog_tool.go` | 指标目录 |
| redirect_stub | `redirect_stub.go` | 重定向桩 |
| restart_service_basetool | `restart_service_basetool.go` | 重启服务 |
| send_im_message_basetool | `send_im_message_basetool.go` | 发 IM 消息 |
| send_message_tool | `send_message_tool.go` | 发消息 |
| serve_page_basetool | `serve_page_basetool.go` | 渲染页面 |
| skill_bridge | `skill_bridge.go` | skill 桥接 |
| task_stop_tool | `task_stop_tool.go` | 停止任务 |

---

## 15. LLM 设置与探测

源文件：`internal/manager/biz/setting/`

### 15.1 llm.go —— LLM provider 解析器

```go
func ParseLLMProvider(raw string) (*LLMProvider, error)
```

- 解析 admin 配置的 LLM provider 设置（JSON）
- 支持 OpenAI/Anthropic/Zhipu/Gemini/DeepSeek/Kimi/Custom
- 输出 provider name + Config（APIKey/Model/BaseURL）

### 15.2 llm_probe.go —— LLM provider 探针

```go
func ProbeLLM(ctx, cfg llm.Config) error
```

- 调用 `llm.ProbeChatCompletion`（20s 超时）
- 跳过 metrics/log
- admin 编辑设置后验证可用性

### 15.3 agent.go —— AgentWriteEnabled fail-safe

```go
type AgentSettings struct {
    WriteEnabled bool  // 是否允许 agent 写操作
}
```

**fail-safe 设计**：默认 `WriteEnabled=false`，必须显式开启。配合 `review_gate` decorator，高危操作强制走审批。

---

## 16. service/aiops 双 kernel 派发

源文件：`internal/manager/service/aiops/service.go`、`user_agent.go`

### 16.1 双 kernel

```go
type Service struct {
    legacyKernel *agent.Agent        // legacy 660 行 for-loop
    graphKernel  *chatruntime.Runtime // eino ReAct
    featureFlag  func(orgID) bool     // 路由决策
}
```

- **Legacy**：`internal/manager/biz/aiops/agent/agent.go`，660 行 for-loop，成熟稳定
- **Graph**：`internal/manager/biz/aiops/chatruntime/` + `graph/`，eino ReAct，新特性主战场
- **feature flag**：按 org 路由，灰度切换

### 16.2 HLD-021 turn 解耦

```go
func (s *Service) Handle(w, r) {
    // 关键：context.WithoutCancel 把 turn 从 HTTP 请求生命周期剥离
    turnCtx := context.WithoutCancel(r.Context())
    go s.kernel.Handle(turnCtx, req)
    // HTTP 立即返回，前端通过 SSE 接收 turn 事件
}
```

**HLD-021**：HTTP 请求生命周期与 LLM turn 生命周期解耦。HTTP 立即返回，turn 在后台 goroutine 跑，前端通过 SSE 接收事件。避免 HTTP 超时中断 LLM。

### 16.3 user_agent.go —— 自定义 persona

- 用户可自定义 agent persona
- 注入 system_prompt 分层拼装

---

## 17. Investigator 与 structured RCA

### 17.1 Legacy Investigator

源文件：`internal/manager/biz/aiops/investigator/investigator.go`

- 告警触发后，AI 自动 initial diagnosis
- 单轮 LLM 调用，输入告警 + 上下文，输出诊断
- 不带工具调用（纯文本生成）

### 17.2 Structured RCA Investigator

源文件：`internal/manager/biz/alert/investigator/usecase.go`

- 结构化根因分析
- 字段：`ForceEnqueue` / `isNew` 触发 / `AffectedWindow` / `PinpointedTarget` 等
- 多轮 LLM + 工具调用（query_promql / query_logql / query_traceql）
- 输出结构化 RCA 报告

### 17.3 report_extractor

源文件：`internal/manager/biz/alert/investigator/report_extractor.go`

- 从 RCA 输出提取结构化字段
- 用于后续聚合分析

---

## 18. 告警规则 AI 辅助编辑

源文件：`internal/manager/biz/aiops/alertconfig/`、`internal/manager/biz/aiops/alertdraft/`

### 18.1 alert_rule_manager（alertconfig/alert_rule_manager.go）

- AI 辅助编辑告警规则
- LLM 生成 PromQL draft，用户确认

### 18.2 draft_store_memory（alertconfig/draft_store_memory.go）

- 内存存储 draft（未确认的告警规则草案）
- per-user 隔离

### 18.3 draft_validation（alertconfig/draft_validation.go）

- 校验 draft 合法性
- PromQL 语法、阈值范围等

### 18.4 alertdraft 编译器（alertdraft/compiler.go + 多个文件）

| 文件 | 职责 |
|------|------|
| `compiler.go` | draft → 正式 alert rule |
| `metric_raw.go` | 原始指标提取 |
| `promql.go` | PromQL 生成 |
| `regex.go` | 正则提取 |
| `scope.go` | 范围限定 |
| `request_hints.go` | 请求提示 |
| `spec_normalize.go` | 规范化 |
| `defaults.go` | 默认值 |

**流程**：用户自然语言 → LLM 生成 draft → draft_validation 校验 → compiler 编译 → 用户确认 → 入库。

---

## 19. 知识库 RAG

源文件：`internal/manager/biz/knowledge/`

### 19.1 usecase.go —— 知识库主逻辑

- **双存储**：MySQL 关系 + qdrant 向量
- **写入**：文档 → MySQL metadata + qdrant embedding
- **查询**：query → embedding → qdrant 相似搜索 → MySQL 取 metadata

### 19.2 builtin_vault.go —— 内置知识库

- `embed.FS` 嵌入内置文档
- 系统启动时自动加载
- 不可删除，可被查询

### 19.3 code_browse.go —— 代码浏览

- 浏览用户代码库作为知识源
- 配合 `code_source_basetool` 工具

### 19.4 ssh_identity.go —— SSH 身份

- 知识库可绑定 SSH 凭据
- 用于访问私有代码仓库

### 19.5 与工具集成

- `query_knowledge_basetool.go`：LLM 调用此工具查询知识库
- 走 RAG 流程：embed query → 相似搜索 → 返回 top-k 文档

---

## 20. Report 反幻觉生成

源文件：`internal/manager/biz/report/`

### 20.1 generator.go —— 报告生成主逻辑

```go
func (g *Generator) Generate(ctx, task *ReportTask) (*Report, error)
```

- 定时任务驱动（cron.go）
- 输入：时间窗口 + 范围
- 输出：结构化报告

### 20.2 facts.go —— 反幻觉核心

```go
type Facts struct {
    // 从实际数据查询得到的结构化事实
    Metrics map[string]float64
    Incidents []IncidentFact
    // ...
}
```

**反幻觉机制**：

1. 先查实际数据（query.go）→ Facts
2. LLM 生成报告草稿
3. **content.go 用 Facts 覆盖 LLM 输出的数字字段**
4. 确保 report 中的数字与 Facts 一致

### 20.3 content.go —— 内容渲染

- 用 Facts 覆盖 LLM 输出的数字字段
- 防止 LLM 编造数字

### 20.4 其他文件

| 文件 | 职责 |
|------|------|
| `cron.go` | 定时调度 |
| `delivery.go` | 报告投递（IM/邮件） |
| `period.go` | 报告周期 |
| `query.go` | 数据查询 |
| `scheduler.go` | 调度器 |
| `task.go` | 任务定义 |
| `usecase.go` | 用例 |

---

## 21. Flow 生成与 IM Bridge

### 21.1 Flow 生成（internal/manager/biz/flow/generate.go）

```go
func GenerateFlow(ctx, desc string) (*Flow, error)
```

- 用户自然语言描述 → LLM 生成 Flow（节点 + 边）
- 输出 JSON → 解析为 Flow 结构
- 用于自动化流程编排

### 21.2 IM Bridge（internal/manager/biz/imbridge/）

源文件：`bridge.go`、`adapter.go`

- 支持 飞书 / Slack / 钉钉 / Telegram
- `bridge.go`：主桥接逻辑
- `adapter.go`：IM 消息 ↔ OnGrid 消息适配
- `dedup.go`：去重
- `imformat/format.go`：格式化
- `sender.go`：发送
- `stream_supervisor.go`：流式监管
- `usecase.go`：用例

**与 LLM 交互**：

- IM 消息进来 → 转 OnGrid chat request → 派发到 AIOps kernel
- LLM 响应 → 通过 `send_im_message_basetool` 或 `sender.go` 推回 IM

### 21.3 provider 子包

```
provider/
├── dingtalk/stream.go
├── feishu/client.go + stream.go + verify.go
├── slack/client.go + stream.go
└── telegram/client.go + stream.go
```

每个 provider 实现 stream 接收 + client 发送。

---

## 22. cmd/ongrid/main.go 装配流程

源文件：`cmd/ongrid/main.go`

### 22.1 LLM 装配步骤

1. **加载 system_settings**：从 DB 读 LLM provider 配置
2. **构造 Resolver**：`setting.ParseLLMProvider` → Resolver
3. **构造 BudgetChecker**：`llm.NewInMemoryBudget(dailyLimit)`
4. **构造 Client**：`llm.NewWithResolver(cfg, resolver, budget, promRegistry)`
5. **构造 MultiClient**：`llm.NewMultiClient(clients, defaultClient, catalog)`
6. **构造 RoutingChatModel**：`llm.NewRoutingChatModel(multiClient)` —— eino 适配
7. **构造 ToolBag**：注册所有工具 + decorators
8. **构造 chatruntime.Runtime**：注入 MultiClient + ToolBag + callbacks
9. **构造 graph.BuildReActGraph**：注入 RoutingChatModel + tools + callbacks
10. **构造 service/aiops.Service**：双 kernel 派发
11. **注册 HTTP 路由**：`server/aiops/http.go`

### 22.2 关键依赖注入

```
Resolver → Client → MultiClient → RoutingChatModel → ReActGraph
                                    ↓
ToolBag → ToolAdapter → graph
                ↓
        callbacks.Chain(AlertDraftGuard, Persistence, SSE, Audit, Metrics, Budget)
                                    ↓
                            chatruntime.Runtime
                                    ↓
                        service/aiops.Service（双 kernel）
```

---

## 23. 并发与资源管理

### 23.1 锁

| 锁 | 文件 | 保护对象 |
|----|------|----------|
| `sdkMu` (Mutex) | `llm/client.go` | sdkCache |
| `resolveMu` (Mutex) | `llm/client.go` | resolved/resolvedAt TTL 缓存 |
| `noSamplingMu` (RWMutex) | `llm/client.go` | noSampling map（读多写少） |
| `mu` (RWMutex) | `llm/router.go` | MultiClient clients/catalog |
| `mu` (Mutex) | `llm/budget.go` | InMemoryBudget used map |
| RWMutex | `chatruntime/skill_registry.go` | SkillRegistry（值拷贝快照） |
| RWMutex | `chatruntime/agent_registry.go` | AgentRegistry |

### 23.2 atomic

| 字段 | 文件 | 用途 |
|------|------|------|
| `atomic.Uint64` | `llm/budget_callback.go` | BudgetCallbackHandler used tokens |
| `atomic.Bool` | `graph/budget_stop_model.go` | stop flag |

### 23.3 缓存

| 缓存 | TTL | 失效方式 |
|------|-----|----------|
| SDK 实例缓存 | 永不失效 | 按 (apiKey, baseURL) key |
| Resolver 缓存 | 60s | `MultiClient.Invalidate()` 立即刷新 |
| Catalog 缓存 | 60s | 同上 |
| SkillRegistry 快照 | 持久 | 值拷贝，写入时整体替换 |

### 23.4 ctx 透传

- **所有 IO 函数第一个参数是 `context.Context`**
- **HLD-021 `context.WithoutCancel`**：turn 与 HTTP 生命周期解耦
- **ctx 传播子包**：7 个子包通过 ctx 传递运行时上下文（artifact_source/bound_credentials/filtered_tools/host_write/llm_choice/locale/session）

### 23.5 goroutine

- **turn 执行**：`go s.kernel.Handle(turnCtx, req)` —— 每 turn 一个 goroutine
- **Worker 状态机**：pending → running → terminal，状态转换线程安全
- **SSE 推送**：callbacks/sse.go 在 turn goroutine 内同步推送

---

## 24. 设计模式与架构红线

### 24.1 设计模式

| 模式 | 应用 |
|------|------|
| Null Object | `noopClient`（无 APIKey 时优雅降级） |
| Adapter | `clientChatModel`（Client → eino ChatModel）、`tool_adapter`（OnGrid Tool → eino Tool） |
| Decorator | `decorators/`（7 个装饰器链）、`budget_stop_model`（透明装饰 LLM） |
| Chain of Responsibility | `callbacks/chain.go`（6 个 handler 链） |
| Strategy | 双 kernel 派发（Legacy / Graph） |
| Registry | SkillRegistry / AgentRegistry / 工具注册表 |
| Factory | `LoadAll` 统一加载器 |
| Template Method | `BaseTool` 基类 |
| Two-tier deferral | `ToolBag`（core/specialty + ToolSearch 元工具） |

### 24.2 响应式自愈

- **reasoning model 采样参数**：名字启发式 + reactive 学习，首次 400 后记 map + 重试
- **toolreplay HOIST**：DeepSeek v4+ strict provider 400 错误的根因修复，tool_call id 配对 + HOIST 索引（`internal/manager/biz/aiops/toolreplay/resolve.go`）

### 24.3 反幻觉覆盖

- **Report generator**：用 Facts 覆盖 LLM 输出的数字字段（`report/content.go`）

### 24.4 架构红线（再次强调）

1. **永不记录用户消息 content** —— log/metrics/audit 任何地方不出现 content
2. **预算门控前置** —— 网络调用前 Check，超限不消耗 API 配额
3. **无 provider 抽象** —— 接口跟随 OpenAI 形状，所有 provider 走 OpenAI-compatible
4. **Prom label 严格限制** —— 仅 model/kind/result，禁 user_id/org_id/session_id
5. **无 auto-retry** —— tools are not idempotent，唯一重试是采样参数剥离自愈
6. **120s defaultTimeout** —— 因 reasoning 慢模型，caller 可用 ctx deadline 覆盖
7. **no streaming** —— 当前同步 completion（chatruntime 通过 callbacks 推 SSE，但 LLM 调用本身是同步）
8. **AgentWriteEnabled fail-safe** —— 默认 false，高危操作强制 review_gate
9. **接口在消费方定义** —— `Client`/`BudgetChecker`/`Resolver`/`Embedder` 等接口在调用方包定义
10. **禁止跨层调用** —— cmd → web → controlplane → repo → model，LLM 层在 internal/pkg + internal/manager/biz/aiops

### 24.5 关键决策

| 决策 | 理由 |
|------|------|
| OpenAI-compatible 而非多 SDK | 减少 provider 适配成本，所有 provider 都提供 OpenAI 兼容端点 |
| eino ReAct 而非自研 agent loop | 复用 eino 生态，聚焦业务工具 |
| 双 kernel 灰度 | 平滑迁移，Legacy 兜底 |
| HLD-021 turn 解耦 | 避免 HTTP 超时中断 LLM |
| ToolBag two-tier | 避免 tool schema 撑爆 context window |
| 本地 ONNX fastembed | 离线可用，零 API 依赖 |
| 手写 Zhipu JWT | 非标准 JWT，通用库不兼容 |
| Facts 反幻觉覆盖 | LLM 数字不可信，用实际数据覆盖 |

---

## 附录：关键文件索引

### A.1 LLM 客户端

| 文件 | 职责 |
|------|------|
| `internal/pkg/llm/client.go` | openaiClient 17 步 Chat 流程 |
| `internal/pkg/llm/router.go` | MultiClient 多 provider 路由 |
| `internal/pkg/llm/budget.go` | InMemoryBudget per-UTC-day 上限 |
| `internal/pkg/llm/budget_callback.go` | eino-side 预算回调 |
| `internal/pkg/llm/probe.go` | ProbeChatCompletion 20s 超时 |
| `internal/pkg/llm/eino_routing.go` | RoutingChatModel + clientChatModel |
| `internal/pkg/llm/metrics.go` | ongrid_llm_* 指标 |
| `internal/pkg/llm/noop.go` | noopClient Null Object |
| `internal/pkg/llm/doc.go` | 三条架构红线 |
| `internal/pkg/zhipuauth/zhipuauth.go` | SignJWT 手写 JWT |

### A.2 嵌入与 MCP

| 文件 | 职责 |
|------|------|
| `internal/pkg/embedding/embedding.go` | Embedder 接口 + OpenAI 兼容 |
| `internal/pkg/embedding/local.go` | localEmbedder ONNX fastembed |
| `internal/pkg/mcpclient/client.go` | MCP Streamable HTTP 客户端 |

### A.3 chatruntime

| 文件 | 职责 |
|------|------|
| `internal/manager/biz/aiops/chatruntime/runtime.go` | 10 步主流程 Handle |
| `internal/manager/biz/aiops/chatruntime/worker.go` | Worker 状态机 |
| `internal/manager/biz/aiops/chatruntime/types.go` | 核心类型 |
| `internal/manager/biz/aiops/chatruntime/system_prompt.go` | 分层拼装 |
| `internal/manager/biz/aiops/chatruntime/load_all.go` | LoadAll 统一加载器 |
| `internal/manager/biz/aiops/chatruntime/plugin_container.go` | 插件容器探测 |
| `internal/manager/biz/aiops/chatruntime/skill_parser.go` | SKILL.md 解析 |
| `internal/manager/biz/aiops/chatruntime/skill_registry.go` | SkillRegistry |
| `internal/manager/biz/aiops/chatruntime/agent_parser.go` | agent persona 解析 |
| `internal/manager/biz/aiops/chatruntime/agent_registry.go` | AgentRegistry |
| `internal/manager/biz/aiops/chatruntime/command_parser.go` | claude command 适配 |
| `internal/manager/biz/aiops/chatruntime/config_confirm.go` | 快捷路径绕过 LLM |

### A.4 graph

| 文件 | 职责 |
|------|------|
| `internal/manager/biz/aiops/graph/react.go` | BuildReActGraph eino ReAct |
| `internal/manager/biz/aiops/graph/types.go` | Input/Output/Config |
| `internal/manager/biz/aiops/graph/tool_adapter.go` | 工具适配器 + toolMemo |
| `internal/manager/biz/aiops/graph/budget_stop_model.go` | 预算停止装饰器 |
| `internal/manager/biz/aiops/graph/callbacks/chain.go` | 链式组合 |
| `internal/manager/biz/aiops/graph/callbacks/alert_draft_guard.go` | 告警 draft 守卫 |
| `internal/manager/biz/aiops/graph/callbacks/persistence.go` | 持久化 |
| `internal/manager/biz/aiops/graph/callbacks/sse.go` | SSE 推送 |
| `internal/manager/biz/aiops/graph/callbacks/audit.go` | 审计 |
| `internal/manager/biz/aiops/graph/callbacks/metrics.go` | Metrics |

### A.5 tools

| 文件 | 职责 |
|------|------|
| `internal/manager/biz/aiops/tools/registry.go` | 闭包路径注册表 |
| `internal/manager/biz/aiops/tools/registry_basetool.go` | BaseTool 路径 |
| `internal/manager/biz/aiops/tools/toolbag.go` | Two-tier deferral |
| `internal/manager/biz/aiops/tools/tool_search_tool.go` | 元工具 |
| `internal/manager/biz/aiops/tools/basetool/basetool.go` | BaseTool 基类 |
| `internal/manager/biz/aiops/tools/basetool/artifact_source.go` | 工件来源 ctx |
| `internal/manager/biz/aiops/tools/basetool/bound_credentials.go` | 凭据 ctx |
| `internal/manager/biz/aiops/tools/basetool/filtered_tools.go` | 过滤工具 ctx |
| `internal/manager/biz/aiops/tools/basetool/host_write.go` | 主机写 ctx |
| `internal/manager/biz/aiops/tools/basetool/llm_choice.go` | LLM 选择 ctx |
| `internal/manager/biz/aiops/tools/basetool/locale.go` | locale ctx |
| `internal/manager/biz/aiops/tools/basetool/session.go` | session ctx |
| `internal/manager/biz/aiops/tools/decorators/chain.go` | 装饰器组合 |
| `internal/manager/biz/aiops/tools/decorators/tenant_bind.go` | tenant 绑定 |
| `internal/manager/biz/aiops/tools/decorators/review_gate.go` | 审批门 |
| `internal/manager/biz/aiops/tools/decorators/timeout.go` | 超时 |
| `internal/manager/biz/aiops/tools/decorators/audit.go` | 审计 |
| `internal/manager/biz/aiops/tools/decorators/ratelimit.go` | 速率限制 |
| `internal/manager/biz/aiops/tools/decorators/metric.go` | 指标 |

### A.6 setting / service

| 文件 | 职责 |
|------|------|
| `internal/manager/biz/setting/llm.go` | LLM provider 解析器 |
| `internal/manager/biz/setting/llm_probe.go` | LLM provider 探针 |
| `internal/manager/biz/setting/agent.go` | AgentWriteEnabled fail-safe |
| `internal/manager/service/aiops/service.go` | 双 kernel 派发 + turn 解耦 |
| `internal/manager/service/aiops/user_agent.go` | 自定义 persona |

### A.7 业务消费

| 文件 | 职责 |
|------|------|
| `internal/manager/biz/aiops/investigator/investigator.go` | legacy AI initial diagnosis |
| `internal/manager/biz/alert/investigator/usecase.go` | structured RCA |
| `internal/manager/biz/alert/investigator/report_extractor.go` | RCA 字段提取 |
| `internal/manager/biz/aiops/alertconfig/alert_rule_manager.go` | 告警规则管理 |
| `internal/manager/biz/aiops/alertconfig/draft_store_memory.go` | draft 存储 |
| `internal/manager/biz/aiops/alertconfig/draft_validation.go` | draft 校验 |
| `internal/manager/biz/aiops/alertdraft/compiler.go` | draft 编译器 |
| `internal/manager/biz/knowledge/usecase.go` | 知识库 RAG |
| `internal/manager/biz/knowledge/builtin_vault.go` | 内置知识库 |
| `internal/manager/biz/knowledge/code_browse.go` | 代码浏览 |
| `internal/manager/biz/knowledge/ssh_identity.go` | SSH 身份 |
| `internal/manager/biz/report/generator.go` | 报告生成 |
| `internal/manager/biz/report/content.go` | 反幻觉覆盖 |
| `internal/manager/biz/report/facts.go` | 结构化事实 |
| `internal/manager/biz/flow/generate.go` | LLM 生成 flow |
| `internal/manager/biz/imbridge/bridge.go` | IM 桥接 |
| `internal/manager/biz/imbridge/adapter.go` | 消息适配 |
| `internal/manager/biz/aiops/toolreplay/resolve.go` | tool_call id 配对 + HOIST |
| `internal/manager/biz/aiops/mentions/search.go` | @-mention 搜索 |
| `internal/manager/biz/aiops/agent/agent.go` | legacy 660 行 for-loop |
| `internal/manager/server/aiops/http.go` | HTTP API |
| `internal/manager/server/aiops/query_translate.go` | 查询翻译 |
| `internal/manager/service/aiopsconfig/alert_rule_adapter.go` | 防腐层适配器 |
| `cmd/ongrid/main.go` | LLM 装配流程 |

---

> 本文档基于 OnGrid 源码（522 个非测试 .go 文件）的 .go.md 技术实现文档编写，覆盖 LLM 子系统的全部模块。如需深入某个模块，参考附录文件索引定位对应的 .go.md 文档。
