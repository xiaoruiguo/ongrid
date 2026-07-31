# `router.go` 技术实现文档

> 源文件：`internal/pkg/llm/router.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/llm`

## 1. 概述

本文件实现 `MultiClient`：多 provider 路由 Client，按 `ChatReq.Provider` 分发到 N 个预构建 sub-client。所有支持的 provider（OpenAI / Anthropic / Zhipu / Gemini / DeepSeek / Kimi / Custom）都暴露 OpenAI-compatible chat completions API，因此 sub-client 都是 `*openaiClient` 实例，按 (apiKey, baseURL) 路由。支持 `ProvidersResolver` 动态 catalog（admin DB 编辑无需重启）+ 60s TTL 缓存；向后兼容：`Provider==""` 回退 default，再回退 constructor-supplied fallback。

## 2. 包信息

- **包名**：`llm`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被 AIOps agent loop、HTTP `/v1/aiops/models` 端点调用；依赖 `internal/pkg/prom`、同包 `client.go`

## 3. 关键类型与接口

```go
type ProviderConfig struct {
    ID, Label, APIKey, Model, BaseURL string
    Models []string  // UI selector 闭集
}

type ProviderInfo struct {
    ID, Label, Model string
    Models []string
}

type ProvidersResolver interface {
    ResolveProviders(ctx) (providers []ProviderConfig, defaultProvider string, err error)
}

type MultiClient struct {
    staticSubs  map[string]Client
    staticInfos []ProviderInfo
    staticDefID string
    fallback    Client

    resolver   ProvidersResolver
    resolveTTL time.Duration

    mu          sync.RWMutex
    dynSubs     map[string]Client
    dynInfos    []ProviderInfo
    dynDefID    string
    dynLoadedAt time.Time
    dynActive   bool
}

type ProviderInfoToWire struct {
    ID, Label string
    Models    []string
    Model     string
}
```

`MultiClient` 实现 `Client` 接口（`Chat`）。

## 4. 关键函数与流程

### `NewMultiClient`
- **签名**：`func NewMultiClient(providers []ProviderConfig, defaultProvider string, fallback Client) *MultiClient`
- **职责**：构造路由器；空 APIKey 的 provider 跳过（不在 `/v1/aiops/models` 暴露）
- **流程**：
  1. 初始化 `staticSubs` map、`resolveTTL=60s`
  2. 遍历 providers：APIKey trim 空跳过；`New(Config{APIKey, Model, BaseURL}, nil, nil)` 建 sub-client；收集 ProviderInfo（Models 空则用 `[Model]`）
  3. `sort.Slice(staticInfos)` by ID 稳定 JSON 输出
  4. defaultProvider 非空且在 staticSubs → 用之；否则取排序首项
  5. 返回 MultiClient

### `SetProvidersResolver`
- **签名**：`func (m *MultiClient) SetProvidersResolver(r ProvidersResolver)`
- **职责**：wire 动态 catalog 源；nil 清除
- **流程**：mu.Lock；设 resolver；清空 dyn 缓存（dynSubs/dynInfos/dynDefID/dynLoadedAt/dynActive）；Unlock

### `SetResolveTTL`
- **签名**：`func (m *MultiClient) SetResolveTTL(d time.Duration)`
- **职责**：覆盖 TTL（测试用）

### `activeSubs`
- **签名**：`func (m *MultiClient) activeSubs(ctx) (map[string]Client, []ProviderInfo, string, bool)`
- **职责**：返回生效 catalog + 是否允许 fallback
- **流程**：
  1. RLock 读 resolver/ttl/loadedAt/dynActive/subs/infos/defID；RUnlock
  2. resolver nil → 返回 static 集 + allowFallback=true
  3. dynActive 且未过期 → 返回 dyn 集 + allowFallback=false
  4. 调 `resolver.ResolveProviders(ctx)`：
     - err → 软回退 static（设 dynLoadedAt 防 flaky resolver 撞 DB；allowFallback=true）
     - 成功 → 重建 newSubs/newInfos（APIKey 空跳过；Models 空用 [Model]；sort by ID）；解析 resolvedDef（def 非空且在 newSubs 用之，否则排序首项）；写 dyn 缓存；返回 + allowFallback=false
- **错误处理**：resolver 失败软回退 static；成功空结果也是 authoritative（disable static）

### `Invalidate`
- **签名**：`func (m *MultiClient) Invalidate()`
- **职责**：强制下次 Chat/Providers/Default 刷新；admin 保存配置后调用让编辑立即生效

### `Providers / Default / HasProvider`
- **签名**：`func (m *MultiClient) Providers() []ProviderInfo` / `Default() (string, string)` / `HasProvider(id string) bool`
- **职责**：返回当前 catalog / 默认 (provider, model) / 是否 wired
- **流程**：调 `activeSubs(context.Background())` 取生效集

### `Chat`
- **签名**：`func (m *MultiClient) Chat(ctx, req ChatReq) (*ChatResp, error)`
- **职责**：路由到 sub-client；记录 `prom.ObserveLLMCall`
- **流程**：
  1. `activeSubs(ctx)` 取 subs/defID/allowFallback
  2. `id = trim(req.Provider)`；空 → 用 defID
  3. start := time.Now()
  4. switch：
     - id 空：allowFallback 且 fallback 非 nil → `fallback.Chat`；否则报错 "no providers configured"
     - default：subs[id] 不存在 → "provider %q not configured"；存在 → `sub.Chat`
  5. providerLabel = id（空则 "fallback"）；modelLabel = trim(req.Model)（空则 "(default)"）
  6. status = `llmStatusFor(err)`
  7. 从 resp 取 PromptTokens/CompletionTokens
  8. `prom.ObserveLLMCall(providerLabel, modelLabel, status, duration, inp, out)`
  9. 返回 resp, err
- **错误处理**：无 provider 报错；provider 不存在报错；sub 错误透传；所有路径都记 prom

### `llmStatusFor`
- **签名**：`func llmStatusFor(err error) string`
- **职责**：err → bounded status label
- **流程**：nil → "ok"；DeadlineExceeded/Canceled → "timeout"；msg 含 "rate limit"/"429" → "rate_limited"；其他 → "error"

### `AsWire`
- **签名**：`func (m *MultiClient) AsWire() []ProviderInfoToWire`
- **职责**：渲染 catalog 为 SPA 期望的 JSON DTO

## 5. 依赖关系

- **内部包**：`internal/pkg/prom`（`ObserveLLMCall`）；同包 `client.go`（`Client` / `Config` / `ChatReq` / `ChatResp` / `New`）
- **外部库**：无
- **被调用方**：AIOps agent loop（作为 Client）、HTTP `/v1/aiops/models` 端点（`AsWire`）、admin LLM 配置保存路径（`SetProvidersResolver` + `Invalidate`）

## 6. 并发与资源管理

- **`sync.RWMutex`**：保护 dyn 缓存字段；读多写少（activeSubs 读路径用 RLock，刷新用 Lock）
- **`staticSubs/Infos/DefID` 构造后只读**：无需锁
- **`fallback` 构造后只读**：无需锁
- **`resolveTTL` 60s**：防 flaky resolver 撞 DB；`SetResolveTTL` 加锁写
- **`Invalidate` 加锁清缓存**：让下次 activeSubs 重建
- **sub-client 自身线程安全**：`openaiClient` 内部有 sdkMu/resolveMu/noSamplingMu

## 7. 设计模式与亮点

- **路由 + fallback 模式**：MultiClient 路由，空 provider 回退 default，再回退 fallback（legacy 单 provider client），向后兼容
- **动态 catalog + TTL 缓存**：admin DB 编辑 60s 内生效；`Invalidate` 立即生效；resolver 失败软回退 static
- **空结果 authoritative**：注释明示"A successful empty slice is an authoritative 'no providers configured' result"；仅 resolver error 回退 static
- **`prom.ObserveLLMCall` 全路径记录**：注释解释 2026-05-16 LLM resolver provider/model mismatch bug 因 legacy metrics 无 provider label 而不可见；MultiClient 在外层记录含 provider label
- **`llmStatusFor` bounded status**：ok/timeout/rate_limited/error 四态，让 dashboard 按"provider slow"vs"provider broken"分组
- **APIKey 空跳过**：注释明示"they stay invisible to /v1/aiops/models so the UI doesn't surface unusable options"
- **sort by ID 稳定输出**：让 JSON 输出与测试 deterministic
- **`ProviderInfoToWire` DTO co-located**：注释明示"Lives here so the wire shape is co-located with the router definition"

## 8. 注意事项

- **60s TTL**：admin 改设置后最长 60s 生效；需调 `Invalidate` 立即生效
- **空 catalog authoritative**：resolver 返回空切片时 static 也禁用；若 resolver bug 返回空可能让生产 LLM 不可用；需监控 resolver 健康
- **fallback 仅 static 模式**：dyn active 后 fallback 不再用（allowFallback=false）；若 dyn catalog 突然空，Chat 报 "no providers configured" 而非回退 fallback
- **`prom.ObserveLLMCall` 与 sub-client 内部 metrics 双重记录**：sub-client 内部仍记 `ongrid_llm_*`（无 provider label）；MultiClient 外层记 `ObserveLLMCall`（有 provider label）；两者互补
- **`providerLabel="fallback"` 时无 model label**：fallback 路径 modelLabel 用 "(default)"，dashboard 需理解
- **`llmStatusFor` 字符串匹配**：rate limit 检测靠 "rate limit"/"429" 子串；跨 gateway 消息形态不同，新增 gateway 需扩展
- **sub-client 无 budget**：`NewMultiClient` 内 `New(Config, nil, nil)` 传 nil budget；预算门控需在 fallback 或上层统一处理
- **sub-client 无 metrics reg**：传 nil reg → sub-client 用 DefaultRegisterer；可能与 MultiClient 期望不一致
- **`HasProvider("")` 返回 false**：空 id 直接 false，不查 catalog
- **`Providers()` 返回 copy**：注释明示"Read-only; safe to share"；caller 修改不影响内部
