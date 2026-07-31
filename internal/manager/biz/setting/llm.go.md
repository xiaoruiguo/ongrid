# llm.go

## 1. 概述

`llm.go` 实现 setting 包的 LLM provider 解析器 —— 把 `system_settings.llm.*` 行 shape 成 `llm.ProvidersResolver` 契约（multi-provider routing）。

分层：env-seeded defaults（构造时传入）形成 fallback；DB 行 per-field override。**存在的空 API-key 行是 authoritative 的 disable**，缺行则继承 env。

缓存由底层 `setting.Service`（60s TTL）+ `llm.MultiClient`（60s TTL on top）负责；`LLMSettingsResolver` 本身无状态（除 env defaults）。

## 2. 包信息

- 包名：`setting`
- 路径：`internal/manager/biz/setting`

## 3. 关键类型与接口

### LLMSettingsResolver

```go
type LLMSettingsResolver struct {
    svc               *Service
    defaults          map[string]EnvProviderDefaults  // env-seeded fallback
    envDefaultProvider string                          // env-seeded default provider id
}
```

### EnvProviderDefaults

```go
type EnvProviderDefaults struct {
    Label   string   // "OpenAI" / "Anthropic" / "智谱 GLM" / "Gemini"
    APIKey  string   // env-seeded key
    Model   string   // env-seeded default model
    BaseURL string   // env-seeded base URL
    Models  []string // env-seeded model list
}
```

### providerKeys（内部）

```go
type providerKeys struct {
    id, label, apiKey, baseURL, models, defaultModel string
    legacyModelKey string  // pre-2026-05 single-model key for OpenAI only
}
```

## 4. 关键函数与流程

### NewLLMSettingsResolver

```go
func NewLLMSettingsResolver(svc *Service, defaults map[string]EnvProviderDefaults, envDefaultProvider string) *LLMSettingsResolver
```

### allProviderKeys

返回 7 个 provider 的 keys：openai / anthropic / zhipu / gemini / deepseek / kimi / custom。OpenAI 有 `legacyModelKey`（pre-2026-05 单 model key）兼容旧部署。

### ResolveProviders（核心）

```go
func (r *LLMSettingsResolver) ResolveProviders(ctx context.Context) ([]llm.ProviderConfig, string, error)
```

对每个 provider：
1. `def = r.defaults[pk.id]` 取 env defaults
2. `apiKey = svc.Get(CategoryLLM, pk.apiKey)` —— err 或 not found 用 `def.APIKey`
3. `apiKey` trim 空 → skip（provider 未配置）
4. `baseURL` 同理，空用 `def.BaseURL`
5. **custom provider 无 default endpoint** —— baseURL 空 skip（防 SDK 静默 fall back 到 OpenAI，把 operator key 发到错主机）
6. `models`：DB JSON > env defaults > legacy single model（openai only）
7. `dedupeStrings(models)` 去重（防 SPA picker 显示同 model 两次）
8. `defaultModel`：DB > env default > legacy > first model
9. `defaultModel` 不在 models 列表 → prepend
10. `dedupStrings(models)` 保序去重
11. `label`：env defaults > pk.label（custom）

返回 `(providers, defaultProvider, nil)`：
- `defaultProvider`：DB > env > ""（router pick first sorted）
- 空 providers slice 是 authoritative no-provider catalog（explicit disable overrides honored）

### EncodeModelsList

```go
func EncodeModelsList(models []string) (string, error)
```

`json.Marshal(models)`。注释：order preserved verbatim 让 SPA "default" pin 不被 reshuffle。

### 辅助函数

- `dedupeStrings` / `dedupStrings`：去重保序（前者丢空，后者不丢空）
- `decodeModelsList`：JSON 解析 + trim + 丢空
- `containsString`：slice contains

## 5. 依赖关系

### 外部包

- `context` / `encoding/json` / `strings`

### 内部包

- `model "github.com/ongridio/ongrid/internal/manager/model/setting"` —— `CategoryLLM` / `Key*` 常量 / `LLMProvider*` 常量
- `github.com/ongridio/ongrid/internal/pkg/llm` —— `ProviderConfig` / `ProvidersResolver` 契约
- 同包：`Service`（在 `service.go`）

### 被谁调用

- `llm.MultiClient` 调 `ResolveProviders`（60s TTL 缓存）做 multi-provider routing
- HTTP handler 调 `EncodeModelsList` 持久化 model 列表

## 6. 并发与资源管理

- **无锁**：`LLMSettingsResolver` 无状态（除 env defaults，构造后只读）
- **缓存由底层负责**：`setting.Service` 60s TTL + `llm.MultiClient` 60s TTL on top
- **每次调用返回 fresh catalog**：注释明确，router 缓存结果

## 7. 设计模式与亮点

### env defaults + DB override 分层

env-seeded defaults 是 fallback；DB 行 per-field override。这让现有部署无需 admin 填 UI 先存活。注释：existing deployments survive without an admin filling in the UI first。

### 空 API-key 行 = authoritative disable

注释：present empty API-key row 是 authoritative disable，覆盖 env。absent row 仍继承 env。这让 admin 能显式 disable 某 provider（设空 key）而非删行。

### Custom provider 强制 baseURL

注释：custom provider 无 default endpoint —— 无 baseURL skip。防 SDK 静默 fall back 到 OpenAI，把 operator key 发到错主机。这是安全设计。

### Legacy model key 兼容

OpenAI 有 `legacyModelKey`（pre-2026-05 单 model key）。`defaultModel` 空时 fallback 到 legacy。注释：让旧部署仍工作。

### 模型去重

`dedupeStrings` / `dedupStrings` 去重保序。注释：防 SPA picker 显示同 model 两次。out-of-box OpenAI list 曾 seeded `[gpt-4o, gpt-4o, gpt-4-turbo]`，deduping 在 read time 治愈。

### defaultModel 不在列表则 prepend

`defaultModel` 不在 models 列表 → prepend。注释：让 SPA dropdown 能 highlight default。dedup 保序。

### 空 providers = authoritative no-provider

注释：空 providers slice 是 authoritative no-provider catalog，explicit disable overrides honored。不回退 env defaults。

### 缓存分层

`LLMSettingsResolver` 无状态；缓存由 `setting.Service`（60s）+ `llm.MultiClient`（60s on top）负责。两层缓存让 DB 读不频繁。

## 8. 注意事项

- **空 API-key 行 = disable**：admin 设空 key disable provider。删行则继承 env。两者语义不同
- **custom provider 无 baseURL skip**：防 SDK fall back OpenAI。admin 配 custom 必须 baseURL
- **legacy model key 仅 OpenAI**：pre-2026-05 部署兼容。新部署不应依赖
- **`dedupeStrings` vs `dedupStrings`**：前者丢空，后者不丢空。models 用前者（丢空 model 名），defaultModel prepend 后用后者（defaultModel 已非空）
- **`defaultModel` prepend 不 append**：让 default 在列表首位，SPA highlight 更显眼
- **`ResolveProviders` 不返回 error**：注释：transient DB error 单 provider fall back env；global error 罕见。空 providers 是 authoritative
- **`EncodeModelsList` 保序**：注释：order preserved 让 SPA default pin 不 reshuffle
- **`envDefaultProvider` 空 = first sorted**：router 在 default 为空时 pick first sorted provider。匹配 legacy 行为
