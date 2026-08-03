# OnGrid OpenAI 模型提供与会话全链路技术实现

> 本文档以 OpenAI 模型为例，完整分析 OnGrid 中模型提供、凭据管理、前后端会话交互的全链路技术实现。

---

## 目录

1. [架构总览](#1-架构总览)
2. [Provider 定义与枚举](#2-provider-定义与枚举)
3. [LLM Client 构造与 SDK 初始化](#3-llm-client-构造与-sdk-初始化)
4. [凭据管理与 Resolver 缓存](#4-凭据管理与-resolver-缓存)
5. [Chat() 方法完整流程](#5-chat-方法完整流程)
6. [MultiClient 路由与动态目录](#6-multiclient-路由与动态目录)
7. [eino 路由层适配](#7-eino-路由层适配)
8. [会话创建与消息发送全链路](#8-会话创建与消息发送全链路)
9. [系统提示词与工具绑定](#9-系统提示词与工具绑定)
10. [前端模型选择与会话交互](#10-前端模型选择与会话交互)
11. [Token 用量追踪与预算门控](#11-token-用量追踪与预算门控)
12. [错误处理与推理模型自愈](#12-错误处理与推理模型自愈)
13. [配置优先级与首次引导](#13-配置优先级与首次引导)
14. [架构红线](#14-架构红线)
15. [关键文件索引](#15-关键文件索引)

---

## 1. 架构总览

### 1.1 全链路数据流

```
用户输入 (ChatInput)
  │ provider/model 选择 → SendOptions
  ▼
前端 fetch POST /v1/chat/sessions/{id}/messages/stream
  │ body: { content, provider, model, mentions, web_search_enabled, locale }
  ▼
http.go::postMessageStream
  │ SSE 握手 + emit 闭包
  ▼
service.go::runWithKernel
  │ context.WithoutCancel (HLD-021) + context.WithCancel (显式可停止)
  ▼
runtime.go::Handle
  │ 所有权检查 → @-mention 渲染 → 用户消息持久化 → 历史加载
  │ 技能解析 + 系统提示词组装 → 构建 eino 历史
  │ 构建 ReAct Graph → 组装回调链
  ▼
react.go::BuildReActGraph → g.Invoke
  │ compose.WithChatModelOption(WithProvider("openai"), model.WithModel("gpt-5.4"))
  ▼
RoutingChatModel.Generate
  │ pick("openai") → inner["openai"].Generate
  ▼
clientChatModel.Generate
  │ buildChatReq → inner.Chat(ctx, req)
  ▼
openaiClient.Chat
  │ effectiveCreds → Resolver → setting.Service → DB
  │ budget.Check → toOpenAIReq → context.WithTimeout(120s)
  │ sdk.CreateChatCompletion → OpenAI API
  │ reasoning model 自愈重试 → budget.Record
  ▼
OpenAI API: POST https://api.openai.com/v1/chat/completions
  │
  ▼ 回调链:
  PersistenceHandler → SSEHandler → AuditHandler → MetricsHandler → BudgetCallbackHandler
  │
  ▼
writeSSE → "event: assistant\ndata: {...}\n\n" → Flush → 前端
```

### 1.2 分层架构

```
┌─────────────────────────────────────────────────────────┐
│ 前端 (React + TypeScript)                                │
│   ChatInput → ModelDropdown → streamMessage → dispatchFrame │
├─────────────────────────────────────────────────────────┤
│ HTTP Handler (server/aiops/http.go)                      │
│   postMessageStream → writeSSE                           │
├─────────────────────────────────────────────────────────┤
│ Service (service/aiops/service.go)                       │
│   runWithKernel → runGraph                               │
├─────────────────────────────────────────────────────────┤
│ ChatRuntime (biz/aiops/chatruntime/runtime.go)           │
│   Handle → 技能解析 → 系统提示词 → 历史 → Graph 构建     │
├─────────────────────────────────────────────────────────┤
│ Graph (biz/aiops/graph/react.go)                         │
│   BuildReActGraph → eino ReAct Agent → g.Invoke          │
├─────────────────────────────────────────────────────────┤
│ eino 路由 (pkg/llm/eino_routing.go)                      │
│   RoutingChatModel → clientChatModel                     │
├─────────────────────────────────────────────────────────┤
│ LLM Client (pkg/llm/client.go)                           │
│   openaiClient.Chat → effectiveCreds → SDK → OpenAI API  │
├─────────────────────────────────────────────────────────┤
│ MultiClient 路由 (pkg/llm/router.go)                     │
│   Chat → activeSubs → sub["openai"].Chat                 │
├─────────────────────────────────────────────────────────┤
│ 凭据层                                                   │
│   Resolver → setting.Service → DB (system_settings)      │
│   三层缓存: Service(进程内) → openaiClient(60s) → MultiClient(60s) │
└─────────────────────────────────────────────────────────┘
```

---

## 2. Provider 定义与枚举

### 2.1 常量定义

**文件**: [eino_routing.go](file:///d:/claude/ongrid/internal/pkg/llm/eino_routing.go) 行 40-51

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

共 7 个 Provider，**所有 provider 均走 OpenAI 兼容协议**（`/v1/chat/completions`），包括 Anthropic、Zhipu、Gemini、DeepSeek、Kimi 和 Custom（通用 OpenAI 兼容端点，如 Ollama/vLLM）。

### 2.2 DB 模型层镜像常量

**文件**: [model.go](file:///d:/claude/ongrid/internal/manager/model/setting/model.go) 行 123-131

```go
const (
    LLMProviderOpenAI    = "openai"
    LLMProviderAnthropic = "anthropic"
    // ... 与 eino_routing.go 保持一致
)
```

### 2.3 前端 Provider ID

**文件**: [LLM.tsx](file:///d:/claude/ongrid/web/src/pages/settings/LLM.tsx) 行 23

```typescript
type LLMProviderID = 'openai' | 'anthropic' | 'zhipu' | 'gemini' | 'deepseek' | 'kimi' | 'custom'
```

三层常量保持同步。

---

## 3. LLM Client 构造与 SDK 初始化

### 3.1 构造入口

**文件**: [client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go)

**`New()`** (行 154-156): 简单入口，委托给 `NewWithResolver`

**`NewWithResolver()`** (行 165-187): 核心构造逻辑
- `cfg.Timeout <= 0` → 默认 `120s` (行 44)
- `resolver == nil && cfg.APIKey == ""` → 返回 `noopClient`（始终返回 `ErrNoAPIKey`）
- 否则构造 `openaiClient`，`resolveTTL` 固定 `60s`

### 3.2 openaiClient 结构体

**文件**: [client.go](file:///&claude/ongrid/internal/pkg/llm/client.go) 行 189-217

```go
type openaiClient struct {
    cfg      Config
    resolver Resolver
    budget   BudgetChecker
    metrics  *metrics
    log      *slog.Logger

    sdkMu    sync.Mutex
    sdkCache map[sdkKey]*openai.Client     // 按 (apiKey, baseURL) 缓存 SDK 实例

    resolveTTL time.Duration               // 固定 60s
    resolveMu  sync.Mutex
    resolved   resolvedCreds               // 上次解析的凭据
    resolvedAt time.Time                   // 上次解析时间

    noSamplingMu sync.RWMutex
    noSampling   map[string]bool           // 运行时发现的拒绝采样参数的模型
}
```

### 3.3 SDK 初始化 (sdkFor)

**文件**: [client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go) 行 273-296

1. 调用 `normalizeOpenAIBaseURL(baseURL)` 规范化 URL
2. 以 `sdkKey{apiKey, baseURL}` 为键查缓存
3. 未命中时用 `openai.DefaultConfig(apiKey)` 创建 SDK 配置
4. `baseURL != ""` → 设置 `sdkCfg.BaseURL`
5. **Zhipu 特殊处理**：`zhipuauth.LooksLikeZhipuURL(baseURL)` → 安装 `zhipuJWTTransport`
6. 缓存永不过期（条目极少）

### 3.4 URL 规范化

**文件**: [client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go) 行 315-331

```go
func normalizeOpenAIBaseURL(raw string) string {
    u, err := url.Parse(strings.TrimSpace(raw))
    if err != nil || u.Host == "" { return raw }
    if strings.Trim(u.Path, "/") == "" {
        u.Path = "/v1"  // 裸地址自动追加 /v1
        return u.String()
    }
    return raw  // 已有路径的 URL 不动
}
```

解决 Ollama/vLLM 等本地服务裸地址 404 问题。

### 3.5 main.go 中的装配

**文件**: [main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) 行 661-667

```go
llmResolver := newLLMResolver(settingSvc)
openaiClient := llm.NewWithResolver(
    llm.Config{APIKey: cfg.OpenAI.APIKey, Model: cfg.OpenAI.Model, BaseURL: cfg.OpenAI.BaseURL},
    llmResolver, nil, reg,
)
```

环境变量来源 ([config.go](file:///d:/claude/ongrid/internal/pkg/config/config.go) 行 398-400):
```go
c.OpenAI.APIKey  = getEnv("ONGRID_OPENAI_API_KEY", "")
c.OpenAI.Model   = getEnv("ONGRID_OPENAI_MODEL", "gpt-5.4")
c.OpenAI.BaseURL = getEnv("ONGRID_OPENAI_BASE_URL", "")
```

---

## 4. 凭据管理与 Resolver 缓存

### 4.1 两套 Resolver 接口

**单 Provider Resolver** ([client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go) 行 139-141):
```go
type Resolver interface {
    Resolve(ctx context.Context) (apiKey, model, baseURL string, err error)
}
```

**多 Provider Resolver** ([router.go](file:///d:/claude/ongrid/internal/pkg/llm/router.go) 行 57-59):
```go
type ProvidersResolver interface {
    ResolveProviders(ctx context.Context) (providers []ProviderConfig, defaultProvider string, err error)
}
```

### 4.2 LLMSettingsResolver 实现

**文件**: [llm.go](file:///d:/claude/ongrid/internal/manager/biz/setting/llm.go) 行 22-53

`ResolveProviders()` (行 133-224) 核心逻辑：
1. 遍历 7 个 provider
2. 对每个 provider：DB 读 `api_key`，缺失回退 env 默认值
3. API key 为空 → 跳过该 provider（未配置）
4. Custom provider 无 base_url → 跳过（防止 key 误发到 OpenAI）
5. 读 `base_url`/`models`/`default_model`，逐字段回退
6. 返回 `[]ProviderConfig` + 默认 provider ID

### 4.3 effectiveCreds — 单 Provider 缓存

**文件**: [client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go) 行 233-261

```go
func (c *openaiClient) effectiveCreds(ctx context.Context) (string, string, string, error) {
    if c.resolver == nil {
        return c.cfg.APIKey, c.cfg.Model, c.cfg.BaseURL, nil
    }
    c.resolveMu.Lock()
    defer c.resolveMu.Unlock()
    if !c.resolvedAt.IsZero() && time.Since(c.resolvedAt) < c.resolveTTL {
        return c.resolved.apiKey, c.resolved.model, c.resolved.baseURL, nil
    }
    apiKey, model, baseURL, err := c.resolver.Resolve(ctx)
    if err != nil {
        c.log.Warn("resolver failed; falling back to env-seeded cfg")
        apiKey, model, baseURL = "", "", ""
    }
    // 空字段回退到 cfg
    if apiKey == "" { apiKey = c.cfg.APIKey }
    if model == "" { model = c.cfg.Model }
    if baseURL == "" { baseURL = c.cfg.BaseURL }
    c.resolved = resolvedCreds{apiKey, model, baseURL}
    c.resolvedAt = time.Now()
    return apiKey, model, baseURL, nil
}
```

**字段级回退**：Resolver 非空字段覆盖 cfg，空字段继承 cfg（env 种子值）。

### 4.4 三层缓存架构

| 层级 | 位置 | TTL | 失效方式 |
|------|------|-----|---------|
| 1 | setting.Service 内存缓存 | 永不过期 | Set/SetBatch/Delete 时精确删除 |
| 2 | openaiClient.resolveTTL | 60s | TTL 过期自动刷新 |
| 3 | MultiClient.resolveTTL | 60s | TTL 过期或 Invalidate() 强制刷新 |

### 4.5 完整失效链

管理员保存 LLM 配置时：

```
HTTP handler validateAndSaveLLMConfiguration()
  → LLMConfigurationService.Save()
    → setting.Service.SetBatch()           ← 清除第 1 层缓存（4 个 key）
  → llmRouter.Invalidate()                 ← 清除第 3 层缓存
    （下次 Chat 时 activeSubs() 重新调用 ResolveProviders）
      → setting.Service.Get()              ← 第 1 层已清，重新从 DB 读取
```

### 4.6 API Key 安全

| 场景 | 处理 |
|------|------|
| 存储 | `system_settings` 表明文，标记 `Sensitive=true` |
| 日志 | 绝不记录 API Key，只记录 model/user_id/tokens/duration |
| API 响应 | `List()` 时脱敏：`sk-1***CDEF`（保留前4后4） |
| 内部读取 | `Get()` 返回明文（Resolver 正常工作所需） |
| Probe 错误 | `sanitizeLLMProbeDetail()` 用 `[redacted]` 替换 API Key |

---

## 5. Chat() 方法完整流程

**文件**: [client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go) 行 355-474

### 步骤 1: 解析凭据 (行 358-367)

```go
apiKey, defaultModel, baseURL, _ := c.effectiveCreds(ctx)
if apiKey == "" { return nil, ErrNoAPIKey }
model := req.Model
if model == "" { model = defaultModel }
```

### 步骤 2: 预算门控 (行 370-380)

```go
if c.budget != nil {
    if err := c.budget.Check(ctx, req.UserID, estimatePromptTokens(req.Messages)); err != nil {
        return nil, err  // ErrBudgetExceeded
    }
}
```

### 步骤 3: 构建 SDK 请求 (行 383-386)

```go
sdkReq, err := c.toOpenAIReq(req, model)
```

- Temperature 处理：推理模型 → 省略；非推理模型 temp=0 → 默认 0.1
- Tool schema 通过 `json.RawMessage` 透传
- ToolCall.Type 固定 `openai.ToolTypeFunction`

### 步骤 4: 超时绑定 (行 389-394)

```go
callCtx := ctx
if _, ok := ctx.Deadline(); !ok {
    callCtx, cancel = context.WithTimeout(ctx, c.cfg.Timeout)  // 默认 120s
    defer cancel()
}
```

### 步骤 5: 发起 API 调用 (行 397-399)

```go
sdk := c.sdkFor(apiKey, baseURL)
sdkResp, err := sdk.CreateChatCompletion(callCtx, sdkReq)
```

### 步骤 6: 推理模型自愈重试 (行 408-414)

```go
if err != nil && isSamplingParamError(err) && hasCustomSampling(sdkReq) {
    c.rememberNoSampling(model)
    stripSamplingParams(&sdkReq)
    sdkResp, err = sdk.CreateChatCompletion(callCtx, sdkReq)
}
```

仅重试一次，且仅对采样参数错误（工具不幂等不重试）。

### 步骤 7: 错误处理 (行 419-433)

- 错误路径：记录 metrics + 日志，不重试
- 空 choices 也视为错误

### 步骤 8: 响应翻译 + 预算记录 (行 436-473)

- `fromOpenAIMessage` 翻译回 `Message`
- 提取 `Usage`（PromptTokens/CompletionTokens/TotalTokens）
- 记录 Prometheus metrics
- `budget.Record()`（失败只 warn，不影响请求）
- 结构化日志（绝不记录消息内容）

---

## 6. MultiClient 路由与动态目录

### 6.1 MultiClient 结构

**文件**: [router.go](file:///d:/claude/ongrid/internal/pkg/llm/router.go) 行 67-87

```go
type MultiClient struct {
    staticSubs  map[string]Client     // 构造时注入的静态子客户端
    staticInfos []ProviderInfo
    staticDefID string
    fallback    Client                // 传统单 provider 回退

    resolver   ProvidersResolver      // 动态目录源
    resolveTTL time.Duration          // 60s

    mu          sync.RWMutex
    dynSubs     map[string]Client     // 动态解析的子客户端
    dynInfos    []ProviderInfo
    dynDefID    string
    dynLoadedAt time.Time
    dynActive   bool
}
```

### 6.2 Chat 路由逻辑

**文件**: [router.go](file:///d:/claude/ongrid/internal/pkg/llm/router.go) 行 284-329

1. `activeSubs(ctx)` 获取当前生效的子客户端映射
2. `id = req.Provider`，空则用 `defID`
3. `id == ""` → 使用 `fallback`（即 `openaiClient`）
4. 否则 `subs[id].Chat(ctx, req)`

### 6.3 activeSubs 动态刷新

**文件**: [router.go](file:///d:/claude/ongrid/internal/pkg/llm/router.go) 行 158-225

- 无 resolver → 返回 staticSubs
- 缓存未过期（60s TTL）→ 返回 dynSubs
- 缓存过期 → `resolver.ResolveProviders(ctx)` 重建
- resolver 出错 → 软降级回 staticSubs
- **成功的空结果是权威的**（禁用所有 provider）

### 6.4 Invalidate 立即刷新

**文件**: [router.go](file:///d:/claude/ongrid/internal/pkg/llm/router.go) 行 230-238

管理员保存 LLM 配置后调用，强制下次 Chat 刷新。

---

## 7. eino 路由层适配

### 7.1 RoutingChatModel

**文件**: [eino_routing.go](file:///d:/claude/ongrid/internal/pkg/llm/eino_routing.go) 行 89-101

```go
type RoutingChatModel struct {
    inner           map[string]model.ChatModel  // provider id → 内部 ChatModel
    defaultProvider string
    defaultResolver func(context.Context) (provider, mdl string)
}
```

- `inner` 映射 7 个 provider → 对应 `clientChatModel`
- `defaultResolver` 动态读取当前默认 provider/model

### 7.2 路由选择

- **WithProvider** (行 72-76): 通过 eino `model.Option` 携带 provider 名
- **pick** (行 173-184): 提取 provider 名，空则回退 `defaultProvider`，从 `inner` map 取 ChatModel
- **withDynamicDefault** (行 151-170): 未指定 provider 时调用 `defaultResolver`

### 7.3 clientChatModel 适配器

**文件**: [einoD_routing.go](file:///d:/claude/ongrid/internal/pkg/llm/eino_routing.go) 行 316-542

```go
type clientChatModel struct {
    inner      Client           // 现有 llm.Client
    model      string           // 默认模型名
    userID     uint64           // 预算门控
    boundTools []*schema.ToolInfo
}
```

- **Generate** (行 357-368): `buildChatReq` → `inner.Chat(ctx, req)` → `einoMessageFromChatResp(resp)`
- **Stream** (行 372-378): 伪流式，调用 Generate 后包装为单 chunk StreamReader
- **BindTools** (行 382-385): 存储 boundTools，后续每次 Generate 自动携带

### 7.4 Provider/Model 覆盖传递链

```
前端 SendOptions.provider/model
  → HTTP body { provider, model }
    → service.go runGraph → chatruntime.Request { Provider, Model }
      → runtime.go chatModelOpts(req)
        → compose.WithChatModelOption(WithProvider(p), model.WithModel(m))
          → RoutingChatModel.Generate → pick(p) → inner[p].Generate
            → clientChatModel.buildChatReq → req.Model = m
              → openaiClient.Chat → model = req.Model (非空则覆盖 defaultModel)
```

---

## 8. 会话创建与消息发送全链路

### 8.1 Session 创建

**HTTP**: `POST /v1/chat/sessions` ([http.go](file:///d:/claude/ongrid/internal/manager/server/aiops/http.go) 行 307-329)

**Service**: [service.go](file:///d:/claude/ongrid/internal/manager/service/aiops/service.go) 行 204-232

**数据模型**: [model.go](file:///d:/claude/ongrid/internal/manager/model/aiops/model.go) 行 49-73

```go
type Session struct {
    ID                string     `gorm:"primaryKey;type:char(36)"`
    UserID            uint64     `gorm:"index;not null"`
    Title             string     `gorm:"size:256;not null"`
    AgentID           *string    `gorm:"size:128;index"`
    Kind              string     `gorm:"size:16;not null;default:'user'"`
    CreatedAt         time.Time
    UpdatedAt         time.Time
    // ...
}
```

### 8.2 消息发送完整流程

**HTTP Handler** → **Service runWithKernel** → **chatruntime Handle**:

1. **所有权检查** (行 486-494)
2. **@-mention 渲染** (行 497-508)
3. **用户消息持久化** (行 512-521): 先写磁盘再调 LLM（crash 安全）
4. **加载历史** (行 524-527): `ListMessages(ctx, sess.ID, HistoryLimit)`
5. **技能解析 + 系统提示词组装** (行 532-658)
6. **构建 eino 历史** (行 662): `buildEinoHistory(history)`
7. **构建 ReAct Graph** (行 668-683)
8. **组装回调链** (行 689-701): `NewDefaultHandlers(deps)`
9. **图调用** (行 727-781): `g.Invoke(ctx, &graph.Input{...})`
10. **错误软着陆** (行 782-816): 图级错误时写入道歉消息 + emit Done

### 8.3 历史消息构建

**文件**: [runtime.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go) 行 940-1088

`buildEinoHistory` 核心逻辑：
1. 裁剪尾部 user 行（避免在 graph assembler 中重复）
2. ToolCall ID 解析 + Tool 消息索引
3. 遍历消息：user/system 直接转换；assistant 填充 ToolCalls + **hoist**（tool 响应紧跟 assistant）
4. 完整性预检：缺少 tool 响应的 assistant turn 被丢弃

---

## 9. 系统提示词与工具绑定

### 9.1 系统提示词组装

**文件**: [system_prompt.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/system_prompt.go) 行 36-84

分层组装：
1. **basePrompt**: 运行时全局前导词
2. **coordinatorToolRouting**: 仅 coordinator 注入，指导工具选型
3. **agentProfile.SystemPrompt**: worker persona 的系统提示词
4. **agentProfile.CriticalReminder**: 包装在 `<critical-reminder>` 标签中
5. **activeSkills**: 每个激活技能以 `[能力: <name>]` 为标题

### 9.2 ReAct Graph 构建

**文件**: [react.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/react.go) 行 78-188

拓扑：`START → MessageAssembler → ReActSubgraph → OutputProjector → END`

- `MaxStep = cfg.MaxIterations*2 + 2`（默认 MaxIterations=30）
- `UnknownToolsHandler`: 幻觉工具名返回 "not available" 而非终止图
- `ToolTimeout`: 默认 15s

### 9.3 工具绑定

- eino 层：`react.AgentConfig.ToolsConfig.Tools` 传入
- tool_adapter 层：`WrapBaseTools` 包装为 `einoToolAdapter`
- Memo 缓存：对 read 工具的相同 (tool, args) 调用返回缓存结果
- 预算门控：超过 `maxToolCallsPerRun`(30) 次返回 `call_budget_exceeded`

---

## 10. 前端模型选择与会话交互

### 10.1 LLM 设置页

**文件**: [LLM.tsx](file:///d:/claude/ongrid/web/src/pages/settings/LLM.tsx)

7 个 Provider 静态配置 (行 58-154)，每个包含中英文提示、Base URL 占位符、模型占位符、4 个 settings key 映射。

| 表单字段 | 行号 | 说明 |
|---------|------|------|
| API Key | 441-454 | sensitive，支持 reveal 切换 |
| Base URL | 455-483 | custom 直接显示，命名 provider 收在折叠区 |
| 模型列表 | 486-555 | 列表 + 添加/删除/设默认 |
| 测试按钮 | 335-371 | 调用 testLLMConfiguration 验证草稿 |
| 保存按钮 | 373-419 | 调用 saveLLMConfiguration 原子保存 |

Provider 状态 (行 423-430)：已配置显示绿色 Chip，未配置灰色 Chip。

### 10.2 聊天中模型选择

**文件**: [ChatInput.tsx](file:///d:/claude/ongrid/web/src/components/ChatInput.tsx) 行 605-725

**ModelDropdown** 组件：
- 触发按钮：`[ModelIcon] modelSlug + ChevronDown`
- 下拉面板：**扁平模型列表**（无 provider 分组头），`providers.flatMap(p => p.models)`
- 无 provider 时：提示文案 + Link 到 `/settings/integrations`

**ModelIcon 品牌识别** ([Provider.tsx](file:///d:/claude/ongrid/web/src/components/icons/Provider.tsx) 行 186-195):
- `gpt-/o1/o3` → OpenAI
- `claude` → Anthropic
- `glm/chatglm` → 智谱
- `gemini/palm` → Gemini
- `deepseek` → DeepSeek
- `moonshot/kimi` → Kimi

### 10.3 Provider/Model 在请求中的传递

**文件**: [chat.ts](file:///d:/claude/ongrid/web/src/api/chat.ts) 行 267-272

```typescript
const body: Record<string, unknown> = { content };
if (opts.provider) body.provider = opts.provider;
if (opts.model) body.model = opts.model;
```

**ChatThread 调用** (行 383-389):
```typescript
{ provider: selectedModel?.provider, model: selectedModel?.model }
```

### 10.4 模型选择持久化

**文件**: [modelSelection.ts](file:///d:/claude/ongrid/web/src/store/modelSelection.ts)

Zustand + persist store，localStorage key: `ongrid.model-selection`

- **Home 页**：选择模型时额外写入 `system_settings.llm.default_provider` + `invalidateLLMRouter()`
- **ChatThread**：只写 localStorage，不写服务端（会话内选择是瞬态的）

### 10.5 会话创建流程

**Home** → `)createSession({ title, agent_id: 'default' })` → `navigate('/chat/${session.id}', { state: { initialPrompt: content } })`

**ChatThread** 接收 `initialPrompt` 后自动调用 `send(initialPrompt, [])`

### 10.6 API 客户端

**文件**: [settings.ts](file:///d:/claude/ongrid/web/src/api/settings.ts)

| 函数 | HTTP | 端点 | 用途 |
|------|------|------|------|
| `listSettings('llm')` | GET | `/system-settings?category=llm` | 列出 LLM 设置 |
| `revealSetting('llm', key)` | GET | `/system-settings/llm/{key}/reveal` | 获取 API Key 明文 |
| `testLLMConfiguration(input)` | POST | `/integrations/llm/test` | 验证草稿 |
| `saveLLMConfiguration(input)` | POST | `/integrations/llm/validate-and-save` | 验证并保存 |
| `invalidateLLMRouter()` | POST | `/integrations/llm/invalidate` | 刷新路由缓存 |
| `listModels()` | GET | `/aiops/models` | 获取模型目录 |

---

## 11. Token 用量追踪与预算门控

### 11.1 用量追踪链

```
OpenAI API 响应 → sdkResp.Usage
  → fromOpenAIMessage → schema.Message.ResponseMeta.Usage
    → PersistenceHandler.OnEnd → chat_messages.prompt_tokens/completion_tokens
      → OutputProjector → graph.Output.Usage
        → runtime.Reply.Usage → agent.Reply.Usage → done SSE 帧
```

### 11.2 每日聚合

**文件**: [usage.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/usage.go) 行 39-52

`Today()`: `SELECT SUM(prompt_tokens), SUM(completion_tokens), COUNT(*) FROM chat_messages WHERE role='assistant' AND created_at >= ?`

**HTTP**: `GET /v1/usage/today` ([http.go](file:///d:/claude/ongrid/internal/manager/server/aiops/http.go) 行 716-733)

**前端**: [StatusRow.tsx](file:///d:/claude/ongrid/web/src/components/StatusRow.tsx) — 30 秒轮询，显示 "今日 Xk token"

### 11.3 预算门控

**文件**: [budget.go](file:///d:/claude/ongrid/internal/pkg/llm/budget.go)

- `BudgetChecker` 接口：`Check()` + `Record()`
- `InMemoryBudget`：全局单桶，按 UTC 日分组
- Chat 中：调用前 `Check()` 拒绝 → `ErrBudgetExceeded`；调用后 `Record()` 失败只 warn
- eino 侧：`BudgetCallbackHandler` 在 `OnStart` 检查，`OnEnd` 记录
- **当前装配**：`nil`（Phase 2 未接入）

---

## 12. 错误处理与推理模型自愈

### 12.1 推理模型检测

**文件**: [client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go) 行 595-613

```go
func isReasoningModel(model string) bool {
    m := strings.ToLower(strings.TrimSpace(model))
    switch {
    case m == "o1" || m == "o3" || m == "o4": return true
    case strings.HasPrefix(m, "o1-") || strings.HasPrefix(m, "o3-") || strings.HasPrefix(m, "o4-"): return true
    case strings.HasPrefix(m, "gpt-5"): return true
    case strings.HasPrefix(m, "kimi-k2") || strings.HasPrefix(m, "kimi-k3"): return true
    case strings.Contains(m, "reasoner") || strings.Contains(m, "reasoning"): return true
    }
    return false
}
```

### 12.2 采样参数错误自愈

**文件**: [client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go) 行 644-661

`isSamplingParamError()` 匹配关键词组合：错误消息包含 `temperature`/`top_p`/`sampling` 之一 + `fixed at 1`/`only 1 is allowed`/`does not support` 之一。

触发后：`rememberNoSampling(model)` + `stripSamplingParams` + 重试一次。

### 12.3 Router 层错误分类

**文件**: [router.go](file:///d:/claude/ongrid/internal/pkg/llm/router.go) 行 334-346

| 错误 | 分类 |
|------|------|
| nil | `"ok"` |
| `context.DeadlineExceeded` / `context.Canceled` | `"timeout"` |
| 包含 `"rate limit"` 或 `"429"` | `"rate_limited"` |
| 其他 | `"error"` |

### 12.4 Probe 错误分类

**文件**: [llm_probe.go](file:///d:/claude/ongrid/internal/manager/biz/setting/llm_probe.go) 行 368-453

20+ 种细粒度 code：`authentication-failed`, `model-not-found`, `rate-limited`, `timeout`, `dns-failed`, `tls-failed`, `endpoint-not-found`, `invalid-response` 等。

### 12.5 Graph 错误翻译

**文件**: [runtime.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go) 行 1699-1778

`buildGraphErrorApology` 将技术错误翻译为用户友好中文提示。

---

## 13. 配置优先级与首次引导

### 13.1 三级优先级

```
运行时可变 (UI → system_settings) > 启动固定 (env → Config) > 首次引导种子 (env → DB)
```

**在 Resolver 中的体现**：
- DB 行存在且非空 → 使用 DB 值
- DB 行不存在 → 回退到 env 默认值
- DB 中空 API Key 行是**权威的**（管理员主动禁用）

### 13.2 首次引导种子

**文件**: [main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go)

OpenAI 传统三字段 (行 517-525):
```go
settingSvc.SetIfAbsent(rootCtx, "llm", "openai_api_key", cfg.OpenAI.APIKey, true)
settingSvc.SetIfAbsent(rootCtx, "llm", "openai_model", cfg.OpenAI.Model, false)
settingSvc.SetIfAbsent(rootCtx, "llm", "openai_base_url", cfg.OpenAI.BaseURL, false)
```

多 Provider 扩展字段 (行 749-816): 为每个 provider 播种 api_key/base_url/default_model/models。

`SetIfAbsent` 保护语义：空值跳过，行已存在跳过，只有首次启动且 env 有值时写入。

### 13.3 环境变量

| 变量 | 默认值 |
|------|--------|
| `ONGRID_OPENAI_API_KEY` | 空 |
| `ONGRID_OPENAI_MODEL` | `gpt-5.4` |
| `ONGRID_OPENAI_BASE_URL` | 空 |
| `ONGRID_LLM_DEFAULT_PROVIDER` | 空 |

---

## 14. 架构红线

| # | 红线 | 说明 |
|---|------|------|
| 1 | **所有 provider 走 OpenAI 兼容协议** | 包括 Anthropic/Zhipu/Gemini/DeepSeek/Kimi/Custom |
| 2 | **工具不幂等不重试** | Chat() 仅对采样参数错误重试一次 |
| 3 | **绝不记录消息内容和 API Key** | 日志只记录 model/user_id/tokens/duration |
| 4 | **凭据三层缓存最长 60s** | 管理员保存后 Invalidate 立即生效 |
| 5 | **空 API Key 行是权威的** | 表示管理员主动禁用 provider |
| 6 | **Custom provider 无 base_url 跳过** | 防止 key 误发到 OpenAI |
| 7 | **请求脱离 HTTP 生命周期** | `context.WithoutCancel` (HLD-021) |
| 8 | **每 session 最多一个活跃 turn** | cancels map 以 sessionID 为 key |
| 9 | **裸 URL 自动补 /v1** | `normalizeOpenAIBaseURL` |
| 10 | **当前无真正 token-by-token 流式** | clientChatModel.Stream 是伪流式 |
| 11 | **assistant_delta 当前丢弃** | 待 SPA 支持后启用 |
| 12 | **Probe 用草稿直连，Session 用 DB 配置** | 两条路径配置源不同 |

---

## 15. 关键文件索引

### 后端 — LLM Client 层

| 文件 | 行号 | 内容 |
|------|------|------|
| [client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go) | 40-51 | Provider 常量 |
| [client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go) | 44 | `defaultTimeout = 120s` |
| [client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go) | 139-141 | Resolver 接口 |
| [client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go) | 154-187 | New / NewWithResolver |
| [client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go) | 189-217 | openaiClient 结构体 |
| [client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go) | 233-261 | effectiveCreds |
| [client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go) | 273-296 | sdkFor SDK 初始化 |
| [client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go) | 315-331 | normalizeOpenAIBaseURL |
| [client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go) | 355-474 | Chat() 完整流程 |
| [client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go) | 477-535 | toOpenAIReq 请求构建 |
| [client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go) | 595-613 | isReasoningModel |
| [client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go) | 644-661 | isSamplingParamError |

### 后端 — 路由层

| 文件 | 行号 | 内容 |
|------|------|------|
| [router.go](file:///d:/claude/ongrid/internal/pkg/llm/router.go) | 57-59 | ProvidersResolver 接口 |
| [router.go](file:///d:/claude/ongrid/internal/pkg/llm/router.go) | 67-87 | MultiClient 结构体 |
| [router.go](file:///d:/claude/ongrid/internal/pkg/llm/router.go) | 158-225 | activeSubs 动态刷新 |
| [router.go](file:///d:/claude/ongrid/internal/pkg/llm/router.go) | 230-238 | Invalidate |
| [router.go](file:///d:/claude/ongrid/internal/pkg/llm/router.go) | 284-329 | Chat 路由 |
| [eino_routing.go](file:///d:/claude/ongrid/internal/pkg/llm/eino_routing.go) | 40-51 | Provider 常量 |
| [eino-rotuing.go](file:///d:/claude/ongrid/internal/pkg/llm/eino_routing.go) | 89-101 | RoutingChatModel |
| [eino_routing.go](file:///d:/claude/ongrid/internal/pkg/llm/eino_routing.go) | 316-542 | clientChatModel 适配器 |

### 后端 — 凭据层

| 文件 | 行号 | 内容 |
|------|------|------|
| [llm.go](file:///d:/claude/ongrid/internal/manager/biz/setting/llm.go) | 22-53 | LLMSettingsResolver |
| [llm.go](file:///d:/claude/ongrid/internal/manager/biz/setting/llm.go) | 133-224 | ResolveProviders |
| [llm_probe.go](file:///d:/claude/ongrid/internal/manager/biz/setting/llm_probe.go) | 194-203 | Probe |
| [llm_probe.go](file:///d:/claude/ongrid/internal/manager/biz/setting/llm_probe.go) | 139-180 | Save |
| [service.go](file:///d:/claude/ongrid/internal/manager/biz/setting/service.go) | 51-57 | setting.Service 缓存 |
| [model.go](file:///d:/claude/ongrid/internal/manager/model/setting/model.go) | 19-33 | Setting DB 模型 |
| [model.go](file:///d:/claude/ongrid/internal/manager/model/setting/model.go) | 74-119 | LLM Key 常量 |

### 后端 — 会话层

| 文件 |!行号 | 内容 |
|------|------|------|
| [http.go](file:///d:/claude/ongrid/internal/manager/server/aiops/http.go) | 307-329 | 创建 Session |
| [http.go](file:///d:/claude/ongrid/internal/manager/server/aiops/http.go) | 414-477 | postMessageStream |
| [service.go](file:///d:/claude/ongrid/internal/manager/service/aiops/service.go) | 204-232 | CreateSession |
| [service.go](file:///d:/claude/ongrid/internal/manager/service/aiops/service.go) | 334-376 | runWithKernel |
| [service.go](file:///d:/claude/ongrid/internal/manager/service/aiops/service.go) | 434-467 | runGraph |
| [runtime.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go) | 468-851 | Handle |
| [runtime.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go) | 940-1088 | buildEinoHistory |
| [react.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/react.go) | 78-188 | BuildReActGraph |
| [system_prompt.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/system_prompt.go) | 36-84 | ComposeSystemPrompt |

### 后端 — 装配

| 文件 | 行号 | 内容 |
|------|------|------|
| [main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) | 517-525 | OpenAI 种子播种 |
| [main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) | 661-667 | openaiClient 构造 |
| [main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) | 749-816 | 多 Provider 播种 |
| [config.go](file:///d:/claude/ongrid/internal/pkg/config/config.go) | 398-400 | 环境变量读取 |

### 前端

| 文件 | 行号 | 内容 |
|------|------|------|
| [LLM.tsx](file:///d:/claude/ongrid/web/src/pages/settings/LLM.tsx) | 23-50 | Provider 类型与元数据 |
| [LLM.tsx](file:///d:/claude/ongrid/web/src/pages/settings/LLM.tsx) | 58-154 | 7 个 Provider 配置 |
| [LLM.tsx](file:///d:/claude/ongrid/web/src/pages/settings/LLM.tsx) | 373-419 | 保存逻辑 |
| [ChatInput.tsx](file:///d:/claude/ongrid/web/src/components/ChatInput.tsx) | 605-725 | ModelDropdown |
| [chat.ts](file:///d:/claude/ongrid/web/src/api/chat.ts) | 80-87 | createSession API |
| [chat.ts](file:///d:/claude/ongrid/web/src/api/chat.ts) | 133-145 | SendOptions 类型 |
| [chat.ts](file:///d:/claude/ongrid/web/src/api/chat.ts) | 252-317 | streamMessage |
| [settings.ts](file:///3:/claude/ongrid/web/src/api/settings.ts) | 138-151 | LLM 配置 API |
| [modelSelection.ts](file:///d:/claude/ongrid/web/src/store/modelSelection.ts) | 1-31 | 模型选择 store |
| [Home.tsx](file:///d:/claude/ongrid/web/src/pages/Home.tsx) | 212-224 | 模型变更写服务端 |
| [Home.tsx](file:///d:/claude/ongrid/web/src/pages/Home.tsx) | 226-245 | 创建会话 |
| [ChatThread.tsx](file:///d:/claude/ongrid/web/src/pages/ChatThread.tsx) | 246-381 | SSE 事件处理 |
| [StatusRow.tsx](file:///d:/claude/ongrid/web/src/components/StatusRow.tsx) | 99-109 | 用量轮询 |
| [Provider.tsx](file:///d:/claude/ongrid/web/src/components/icons/Provider.tsx) | 186-195 | brandFromModel |
