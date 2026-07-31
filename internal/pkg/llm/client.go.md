# `client.go` 技术实现文档

> 源文件：`internal/pkg/llm/client.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/llm`

## 1. 概述

本文件是 LLM 客户端的核心实现：基于 `github.com/sashabaranov/go-openai` SDK 的 OpenAI 风格 chat / tool-calling 客户端，带 per-day token 预算门控、Prometheus 监控、动态 Resolver（admin 设置热更新）、Zhipu JWT transport 适配、reasoning model 自动采样参数剥离。红线：无 provider 抽象（接口跟随 OpenAI 形状）、Prom label 禁含 user_id/org_id/session_id、永不记录用户消息内容。

## 2. 包信息

- **包名**：`llm`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被 AIOps agent loop、`MultiClient`（router.go）调用；依赖 `go-openai` SDK、`prometheus`、`zhipuauth`

## 3. 关键类型与接口

```go
type Config struct {
    APIKey, Model, BaseURL string
    Timeout time.Duration
}

type Message struct {
    Role, Content string
    ToolCalls []ToolCall
    ToolCallID, ToolName string
}

type ToolCall struct {
    ID, Name string
    Args json.RawMessage
}

type ToolSchema struct {
    Name, Description string
    Parameters json.RawMessage
}

type Usage struct {
    PromptTokens, CompletionTokens, TotalTokens int
}

type ChatReq struct {
    Model, Provider string
    Messages []Message
    Tools []ToolSchema
    Temperature float32
    UserID uint64
}

type ChatResp struct {
    Assistant Message
    Usage Usage
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

type openaiClient struct {
    cfg Config
    resolver Resolver
    budget BudgetChecker
    metrics *metrics
    log *slog.Logger
    sdkMu sync.Mutex
    sdkCache map[sdkKey]*openai.Client
    resolveTTL time.Duration
    resolveMu sync.Mutex
    resolved resolvedCreds
    resolvedAt time.Time
    noSamplingMu sync.RWMutex
    noSampling map[string]bool
}

type zhipuJWTTransport struct {
    apiKey string
    base http.RoundTripper
}
```

Sentinel：`ErrBudgetExceeded`、`ErrNoAPIKey`。`defaultTimeout = 120 * time.Second`。

## 4. 关键函数与流程

### `New / NewWithResolver`
- **签名**：`func New(cfg Config, budget BudgetChecker, reg *prometheus.Registry) Client` + `NewWithResolver`
- **职责**：构造 Client；空 APIKey 且无 resolver → 返回 `noopClient`
- **流程**：
  1. log = `slog.Default().With("component", "llm")`
  2. Timeout <= 0 → defaultTimeout (120s)
  3. resolver nil 且 cfg.APIKey 空 → Warn + 返回 noopClient
  4. 否则返回 `&openaiClient{...}`，resolveTTL=60s

### `openaiClient.Chat`
- **签名**：`func (c *openaiClient) Chat(ctx, req ChatReq) (*ChatResp, error)`
- **职责**：执行一次 chat completion，带预算门控、metrics、log
- **流程**：
  1. `effectiveCreds(ctx)` 解析（resolver 覆盖 cfg；空回退 cfg；TTL 60s 缓存）
  2. apiKey 空 → `ErrNoAPIKey`
  3. model 空 → 用 defaultModel
  4. **预算门控**：`budget.Check(ctx, req.UserID, estimatePromptTokens(req.Messages))`；失败 metrics `budget_exceeded` + Warn + 返回 ErrBudgetExceeded
  5. `toOpenAIReq(req, model)` 翻译；失败 `%w`
  6. ctx 无 deadline → `context.WithTimeout(ctx, cfg.Timeout)`
  7. `sdkFor(apiKey, baseURL)` 取缓存 SDK
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
- **错误处理**：预算超限、网络错误、空 choices、decode 失败均返回 error；Record 失败仅 Warn

### `effectiveCreds`
- **签名**：`func (c *openaiClient) effectiveCreds(ctx) (apiKey, model, baseURL string, error)`
- **职责**：解析有效凭据；resolver 覆盖 cfg；TTL 缓存防热路径 DB round-trip
- **流程**：resolver nil → 直接返回 cfg；否则检查 `resolvedAt` + TTL；过期调 `resolver.Resolve(ctx)`，失败 Warn + 回退 cfg；空字段回退 cfg；缓存 `resolved`

### `sdkFor`
- **签名**：`func (c *openaiClient) sdkFor(apiKey, baseURL string) *openai.Client`
- **职责**：按 (apiKey, baseURL) 缓存 SDK 实例
- **流程**：
  1. `normalizeOpenAIBaseURL(baseURL)`
  2. sdkMu.Lock；cache 命中返回；否则构造 `openai.DefaultConfig(apiKey)` + BaseURL
  3. 若 `LooksLikeZhipuURL(baseURL) && LooksLikeZhipuKey(apiKey)` → 装 `zhipuJWTTransport`
  4. `openai.NewClientWithConfig`；缓存；返回
- **错误处理**：无错误返回

### `normalizeOpenAIBaseURL`
- **签名**：`func normalizeOpenAIBaseURL(raw string) string`
- **职责**：补 `/v1` 路径段，适配 Ollama / vLLM 等裸地址
- **流程**：parse URL；若 path 为空 → 设 `/v1`；有 path 信任原样

### `zhipuJWTTransport.RoundTrip`
- **签名**：`func (t *zhipuJWTTransport) RoundTrip(req) (*http.Response, error)`
- **职责**：每次请求重签智谱 JWT，覆盖 Authorization
- **流程**：`zhipuauth.SignJWT(apiKey, 1h)`；`req.Clone(ctx)`；`Header.Set("Authorization", "Bearer "+token)`；`base.RoundTrip(cloned)`
- **错误处理**：SignJWT 失败返回 error

### `toOpenAIReq`
- **签名**：`func (c *openaiClient) toOpenAIReq(req ChatReq, model string) (openai.ChatCompletionRequest, error)`
- **职责**：翻译 ChatReq → SDK 请求；处理 reasoning model 采样
- **流程**：
  1. temp 计算：`isReasoningModel(model) || modelRejectsSampling(model)` → temp=0；否则 0（req.Temperature==0）→ 默认 0.1
  2. 翻译 messages（含 ToolCalls）
  3. 翻译 tools：Parameters 是 `json.RawMessage`，先 unmarshal 验证再作为 `json.RawMessage` 传给 SDK
  4. 返回 `ChatCompletionRequest{Model, Messages, Tools, Temperature}`

### `isReasoningModel / modelRejectsSampling / rememberNoSampling / isSamplingParamError / hasCustomSampling / stripSamplingParams`
- **职责**：reasoning model 采样参数处理
- **流程**：
  - `isReasoningModel`：按名前缀匹配 o1/o3/o4/gpt-5/kimi-k2/kimi-k3/reasoner/reasoning
  - `modelRejectsSampling`：查 `noSampling` map（reactive 学习）
  - `rememberNoSampling`：写入 map
  - `isSamplingParamError`：err msg 含 "temperature"/"top_p"/"sampling" 且含 "fixed at 1"/"only 1 is allowed"/"beta-limitations"/... → true
  - `hasCustomSampling`：req 任一采样参数非零
  - `stripSamplingParams`：全部清零

### `estimatePromptTokens`
- **签名**：`func estimatePromptTokens(msgs []Message) int`
- **职责**：粗估 prompt tokens（~4 字符/token + per-msg overhead 4）；含 ToolCall.Name/Arguments

## 5. 依赖关系

- **内部包**：`internal/pkg/zhipuauth`
- **外部库**：`github.com/sashabaranov/go-openai`、`github.com/prometheus/client_golang`
- **被调用方**：AIOps agent loop、`MultiClient`（router.go 作为 sub-client）

## 6. 并发与资源管理

- **`sdkMu`（Mutex）**：保护 sdkCache
- **`resolveMu`（Mutex）**：保护 resolved/resolvedAt TTL 缓存
- **`noSamplingMu`（RWMutex）**：保护 noSampling map（读多写少）
- **SDK 客户端缓存**：按 (apiKey, baseURL) key，跨调用复用连接池
- **`http.Client` 共享**：Zhipu transport 用 `http.DefaultTransport` 作为 base
- **ctx 透传**：`http.NewRequestWithContext`；无 deadline 时补 cfg.Timeout

## 7. 设计模式与亮点

- **无 provider 抽象**：注释明示"interface follows OpenAI's shape"；所有 provider 走 OpenAI-compatible 端点
- **Resolver 热更新**：admin 编辑 system_settings 后 60s 内生效（TTL 缓存）；resolver 失败软回退 cfg
- **响应式自愈 reasoning model**：名字启发式 + reactive 学习；首次 400 后记 map + 重试，后续主动省采样参数
- **Zhipu JWT transport 透明适配**：通过 `LooksLikeZhipuURL` + `LooksLikeZhipuKey` 探测，自动装 JWT transport，caller 无感
- **`normalizeOpenAIBaseURL` 补 `/v1`**：解决 Ollama/vLLM 裸地址 404 问题
- **永不记录 content**：注释明示"NEVER log user content — we only note the fact and the user bucket"
- **预算门控前置**：网络调用前 Check，超限不消耗 API 配额
- **Record 失败不 fail 请求**：与预算门控前置对称，用户请求优先
- **Prom label 严格限制**：仅 model/kind/result，禁 user_id/org_id/session_id
- **noopClient 优雅降级**：无 APIKey 时返回 noop，caller 拿 `ErrNoAPIKey` 而非 401

## 8. 注意事项

- **120s defaultTimeout**：注释解释从 30s 提到 120s 是因 DeepSeek v4 reasoning 等慢模型；caller 可用 ctx deadline 覆盖
- **无重试**：注释明示"no auto-retry (tools are not idempotent)"；唯一重试是采样参数剥离的自愈
- **无 streaming**：注释明示"no streaming"；当前是同步 completion
- **SDK 缓存永不清**：注释提到"tops out at a handful of entries even across a year of edits"
- **resolveTTL 60s**：admin 改设置后最长 60s 生效；可通过 `MultiClient.Invalidate` 立即刷新（router.go）
- **reasoning model 名启发式**：仅覆盖常见家族；未匹配的靠 reactive 自愈，但会多吃一次失败 round-trip
- **`isSamplingParamError` 字符串匹配**：跨 gateway（dmxapi/Azure/...）消息形态不同；新增 gateway 需扩展匹配
- **Zhipu JWT 每次重签**：1h TTL 内重复签名开销可忽略，但若高频调用可考虑缓存 token
- **`estimatePromptTokens` 粗估**：~4 字符/token 是英文经验值；中文/代码可能偏差；仅用于预算门控，真实计费是返回的 Usage
- **`toOpenAIReq` 默认 temp 0.1**：注释明示"AIOps loop relies on" 确定性；reasoning model 强制 0
