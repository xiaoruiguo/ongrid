# llm_probe.go

## 1. 概述

`llm_probe.go` 实现 setting 包的 LLM provider 探针 —— 在保存前验证 draft 配置可工作。两个入口：
- `LLMConfigProbe.Probe` —— 仅验证不持久化
- `LLMConfigurationService.Save` —— 验证 + 原子持久化（绑定 probed draft 到 persisted tuple，防 UI 验证一个值存另一个）

探针对每个 exposed model 跑 bounded `llm.ProbeChatCompletion` 调用，分类失败为 language-neutral `Code`（SPA 本地化展示）。`sanitizeLLMProbeDetail` 把 API key 从 detail 中 redact。

空 API key 是 deliberate disable override，skip upstream call 让 broken credential 总能移除。

## 2. 包信息

- 包名：`setting`
- 路径：`internal/manager/biz/setting`

## 3. 关键类型与接口

### Code 常量

```go
const (
    LLMProbeCodeOK                  = "ok"
    LLMProbeCodeDisabled            = "disabled"
    LLMProbeCodeUnsupportedProvider = "unsupported-provider"
    LLMProbeCodeMissingAPIKey       = "missing-api-key"
    LLMProbeCodeMissingModel        = "missing-model"
    LLMProbeCodeMissingBaseURL      = "missing-base-url"
    LLMProbeCodeInvalidBaseURL      = "invalid-base-url"
    LLMProbeCodeAuthentication      = "authentication-failed"
    LLMProbeCodePermission          = "permission-denied"
    LLMProbeCodeModelNotFound       = "model-not-found"
    LLMProbeCodeQuotaExceeded       = "quota-exceeded"
    LLMProbeCodeRateLimited         = "rate-limited"
    LLMProbeCodeTimeout             = "timeout"
    LLMProbeCodeCanceled            = "request-canceled"
    LLMProbeCodeDNS                 = "dns-failed"
    LLMProbeCodeConnection          = "connection-failed"
    LLMProbeCodeTLS                 = "tls-failed"
    LLMProbeCodeEndpointNotFound    = "endpoint-not-found"
    LLMProbeCodeProviderUnavailable = "provider-unavailable"
    LLMProbeCodeInvalidRequest      = "invalid-request"
    LLMProbeCodeInvalidResponse     = "invalid-response"
    LLMProbeCodeUpstream            = "upstream-error"
)
```

### 限制常量

```go
const (
    defaultLLMProbeTimeout = 20 * time.Second
    maxLLMAPIKeyBytes      = 16 << 10  // 16 KB
    maxLLMBaseURLBytes     = 2048
    maxLLMModelBytes       = 256
    maxLLMModels           = 32
    maxLLMProbeDetailRunes = 240
)
```

### LLMProbeInput / LLMProbeResult

```go
type LLMProbeInput struct {
    Provider, APIKey, BaseURL, DefaultModel string
    Models []string
}

type LLMProbeResult struct {
    Valid     bool
    Code      string
    Provider  string
    Model     string
    Detail    string
    LatencyMS int64
    Saved     bool
    Disabled  bool
}
```

### LLMConfigProbe

```go
type LLMConfigProbe struct {
    defaults map[string]EnvProviderDefaults
    timeout  time.Duration
    call     llmProbeCall  // llm.ProbeChatCompletion，测试可注入
}
```

### LLMConfigurationService

```go
type LLMConfigurationService struct {
    probe    *LLMConfigProbe
    settings *Service
}
```

注释：binds the exact draft that was probed to the exact tuple persisted by `Service.SetBatch`。防 UI/API client 验证一个值存另一个。

## 4. 关键函数与流程

### NewLLMConfigurationService

构造 probe + settings。`defaults` 是 env-backed provider map（同 `LLMSettingsResolver` 用的）。

### Probe

```go
func (s *LLMConfigurationService) Probe(ctx, in LLMProbeInput) (LLMProbeResult, error)
```

仅验证不持久化。委派 `s.probe.Probe(ctx, in)`。

### Save

```go
func (s *LLMConfigurationService) Save(ctx, in LLMProbeInput) (LLMProbeResult, error)
```

1. `operational = TrimSpace(in.APIKey) != ""`
2. `validateInput(in, operational)` —— 失败返回 result（不调 upstream）
3. `operational`：`probeValidated(ctx, cfg)` —— 失败返回 result
4. `!operational`：`cfg.apiKey = ""` + `result.Valid = true` + `Code = Disabled` + `Disabled = true`（deliberate disable，skip upstream）
5. `providerKeysByID(cfg.provider)` 取 keys —— 失败 `Code = UnsupportedProvider`
6. `EncodeModelsList(cfg.models)`
7. `settings.SetBatch([{apiKey, Sensitive:true}, {baseURL}, {defaultModel}, {models}])` —— 原子持久化
8. `result.Saved = true`

### Probe（LLMConfigProbe）

```go
func (p *LLMConfigProbe) Probe(ctx, in LLMProbeInput) (LLMProbeResult, error)
```

`validateInput(in, true)` + `probeValidated(ctx, cfg)`。

### validateInput

校验：
- `provider` 小写 trim，必须 `isKnownLLMProvider`
- `operational` 时 APIKey 非空
- APIKey ≤ 16KB
- baseURL ≤ 2KB
- models：trim、丢空、去重、≤ 32 个、每个 ≤ 256 字节
- `!operational` 时 defaultModel 空取 models[0]
- defaultModel ≤ 256 字节
- `operational` 时 defaultModel + models 非空，且 defaultModel in models
- effectiveBaseURL = stored || env default
- custom provider operational 时 effectiveBaseURL 非空
- effectiveBaseURL 非空时 `validateLLMBaseURL`

### probeValidated

```go
func (p *LLMConfigProbe) probeValidated(ctx, cfg) LLMProbeResult
```

对 `cfg.models`（defaultModel 在首位）逐个调 `p.call(ctx, llm.Config{APIKey, Model, BaseURL, Timeout})`：
- 任一失败 → `result.Model = modelName` + `classifyLLMProbeError` + return
- 全部成功 → `result.Valid = true` + `LatencyMS`

### classifyLLMProbeError

分类 error 为 (Code, Detail)：
- `context.DeadlineExceeded` → Timeout
- `context.Canceled` → Canceled
- `llm.ErrNoAPIKey` → MissingAPIKey
- `io.EOF` / `io.ErrUnexpectedEOF` → InvalidResponse
- `*net.DNSError` → DNS
- `isLLMTLSError` → TLS
- `net.Error` Timeout → Timeout
- `*net.OpError` → Connection
- `*openai.APIError` → `classifyLLMHTTPError`
- `*openai.RequestError` → `classifyLLMHTTPError(unstructured=true)`
- msg contains "empty choices"/"decode"/"unmarshal" → InvalidResponse
- msg contains "invalid api key" → Authentication
- msg contains "model" + "not found" → ModelNotFound
- default → Upstream

### classifyLLMHTTPError

按 HTTP status + searchable string 分类：401/403/404/408/429/5xx 等映射到对应 Code。`sanitizeLLMProbeDetail` redact API key。

### validateLLMBaseURL

校验：scheme http/https、host 非空、无 userinfo、无 query/fragment。

### sanitizeLLMProbeDetail

```go
func sanitizeLLMProbeDetail(detail, apiKey string) string
```

1. trim
2. `strings.ReplaceAll(detail, apiKey, "[redacted]")` —— redact API key
3. `strings.Fields` 折叠空白
4. > 240 runes 截断 + "…"

## 5. 依赖关系

### 外部包

- `context` / `crypto/tls` / `crypto/x509` / `errors` / `fmt` / `io` / `net` / `net/url` / `strings` / `time` / `unicode/utf8`
- `openai "github.com/sashabaranov/go-openai"` —— `APIError` / `RequestError` 类型

### 内部包

- `settingmodel "github.com/ongridio/ongrid/internal/manager/model/setting"` —— `LLMProvider*` 常量
- `github.com/ongridio/ongrid/internal/pkg/llm` —— `Config` / `ProbeResult` / `ProbeChatCompletion` / `ErrNoAPIKey`
- 同包：`Service` / `EnvProviderDefaults` / `EncodeModelsList` / `allProviderKeys` / `providerKeys` / `containsString`

### 被谁调用

- HTTP handler（`/v1/settings/llm/*`）调 `Probe` + `Save`

## 6. 并发与资源管理

- **无锁**：`LLMConfigProbe` / `LLMConfigurationService` 无状态（除 env defaults，构造后只读）
- **timeout context**：`probeValidated` 用 `context.WithTimeout(ctx, p.timeout)` 限制单次 probe
- **无 goroutine**：所有方法同步
- **`call` 注入**：测试可注入 fake，不需起真 HTTP server

## 7. 设计模式与亮点

### Probe + Save 绑定 draft

`LLMConfigurationService.Save` 先 `validateInput` + `probeValidated`，再 `SetBatch` 持久化。注释：binds the exact draft that was probed to the exact tuple persisted。防 UI/API client 验证一个值存另一个。

### 空 API key = deliberate disable

`!operational` 时 skip upstream，`Code = Disabled`，`cfg.apiKey = ""`。注释：让 broken credential 总能移除。这是安全设计 —— 不让坏 key 卡住 disable 操作。

### Language-neutral Code

`LLMProbeResult.Code` 是 language-neutral 字符串（`"authentication-failed"` 等）。注释：SPA maps Code to localized guidance。让后端不依赖 locale，前端按 locale 本地化。

### sanitizeLLMProbeDetail redact API key

`sanitizeLLMProbeDetail` 把 API key 从 detail 中 `ReplaceAll` 为 `[redacted]`。注释：APIKey may be persisted only by Save; it must never be logged or copied into LLMProbeResult。这是安全红线 —— 防探针错误消息泄漏 key。

### 逐 model probe

`probeValidated` 对每个 exposed model 调 `ProbeChatCompletion`。注释：validates every model that would be exposed after saving。让保存前确认所有 model 可用。

### defaultModel 首位 probe

`probeValidated` 把 `defaultModel` 放首位。若 defaultModel 失败，立即返回，不 probe 其它。让 default 优先验证。

### 分类细致

`classifyLLMProbeError` + `classifyLLMHTTPError` 把 error 分到 20+ Code。让 SPA 能给精确本地化提示（如 Authentication vs Permission vs QuotaExceeded）。

### validateLLMBaseURL 严格

校验 scheme http/https、host 非空、无 userinfo、无 query/fragment。防 SSRF 与 SDK 误解析。

### `call` 注入测试

`LLMConfigProbe.call` 是 `llmProbeCall` 类型，测试可注入 fake 返回 canned error，不需起真 HTTP server。

## 8. 注意事项

- **`Save` 绑定 draft**：UI 不能 probe 一个值存另一个。`Save` 内部 re-validate + re-probe
- **空 API key = disable**：`!operational` skip upstream。让 broken credential 总能移除
- **`sanitizeLLMProbeDetail` redact**：API key 不进 `LLMProbeResult.Detail`。但若 error message 含其它敏感信息（如 base URL），不 redact
- **`probeValidated` 逐 model**：多个 model 时 probe 时间累加。`timeout` 是单次 probe 上限，不是总上限
- **`defaultLLMProbeTimeout` 20s**：硬编码。慢 provider 可能 timeout
- **`maxLLMModels` 32**：硬编码上限。超过 reject
- **`classifyLLMProbeError` msg 匹配脆弱**：依赖 error message 字符串匹配（如 "invalid api key"）。provider 改 message 可能误分类
- **`isLLMTLSError` 检查 4 种 x509/tls error**：覆盖常见 TLS 失败。新增 TLS error 类型需扩展
- **`Save` `SetBatch` 原子**：4 个 setting 行原子持久化。若 DB 不支持事务，部分持久化可能 —— 应检查 `Service.SetBatch` 实现
- **`APIKey` `Sensitive: true`**：`SetBatch` 时标记 sensitive，让 audit log 不记 value
