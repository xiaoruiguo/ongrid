# `probe.go` 技术实现文档

> 源文件：`internal/pkg/llm/probe.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/llm`

## 1. 概述

本文件实现 `ProbeChatCompletion`：发送一个最小请求穿过生产 LLM 客户端使用的相同 URL 规范化与认证路径，验证 provider 凭据有效性。刻意跳过 metrics / logs / retries / budgets — 无效凭据是预期的验证结果，不应污染运行时错误遥测或泄漏上游响应细节到日志。仅返回 token usage 证明 provider 返回结构合法的 chat completion；assistant 内容被丢弃。

## 2. 包信息

- **包名**：`llm`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被 manager 侧 LLM 配置保存/测试路径调用；依赖 `github.com/sashabaranov/go-openai`、`zhipuauth`

## 3. 关键类型与接口

```go
type ProbeResult struct {
    Usage Usage
}
```

导出函数 `ProbeChatCompletion` 是公共 API。

## 4. 关键函数与流程

### `ProbeChatCompletion`
- **签名**：`func ProbeChatCompletion(ctx context.Context, cfg Config) (*ProbeResult, error)`
- **职责**：发送最小请求验证凭据；跳过 metrics/log/retry/budget
- **流程**：
  1. `cfg.APIKey` trim 空 → 返回 `ErrNoAPIKey`
  2. `cfg.Model` trim 空 → 报错 `"llm: model is empty"`
  3. `cfg.Timeout <= 0` → 设 20s（探针专用，比生产 120s 短）
  4. `context.WithTimeout(ctx, cfg.Timeout)`，defer cancel
  5. `baseURL = normalizeOpenAIBaseURL(cfg.BaseURL)` — 复用 client.go 的规范化
  6. `sdkCfg = openai.DefaultConfig(cfg.APIKey)`；baseURL 非空则设
  7. transport 默认 `http.DefaultTransport`；若 `LooksLikeZhipuURL(baseURL) && LooksLikeZhipuKey(apiKey)` → 装 `zhipuJWTTransport`
  8. `sdkCfg.HTTPClient = &http.Client{Timeout, Transport}`
  9. `sdk = openai.NewClientWithConfig(sdkCfg)`
  10. `sdk.CreateChatCompletion(callCtx, ChatCompletionRequest{Model, Messages:[{Role:user, Content:"Reply with OK."}]})`
  11. err 非 nil → `%w` 包装 `"llm probe: chat completion: "`
  12. `len(Choices)==0` → 报错 `"llm probe: empty choices in response"`
  13. 返回 `&ProbeResult{Usage: {PromptTokens, CompletionTokens, TotalTokens}}`
- **错误处理**：
  - APIKey/Model 空返回 sentinel / 明确 error
  - 网络错误 `%w` 包装
  - 空 choices 报错
  - **不记 metrics**（注释明示"invalid credentials are an expected validation outcome and must not pollute runtime error telemetry"）
  - **不 log 上游响应细节**（注释明示"leak upstream response details into logs"）

## 5. 依赖关系

- **内部包**：`internal/pkg/zhipuauth`；同包 `client.go`（`Config` / `Usage` / `ErrNoAPIKey` / `normalizeOpenAIBaseURL` / `zhipuJWTTransport`）
- **外部库**：`github.com/sashabaranov/go-openai`
- **被调用方**：manager 侧 LLM 配置保存/测试路径（admin UI 的"测试连接"按钮）

## 6. 并发与资源管理

- **无共享状态**：`ProbeChatCompletion` 是无状态纯函数（除 HTTP 副作用）
- **每次新建 SDK 客户端**：与 `openaiClient.sdkFor` 缓存不同，probe 每次新建（探针频率低，无需缓存）
- **`context.WithTimeout`**：caller ctx + cfg.Timeout 派生；可取消
- **`http.Client` 独立**：probe 用独立 client，不共享生产连接池

## 7. 设计模式与亮点

- **复用生产路径**：`normalizeOpenAIBaseURL` + `zhipuJWTTransport` 与 `openaiClient.Chat` 完全一致，确保探针验证的就是生产将用的路径
- **跳过 metrics/log**：注释明示"invalid credentials are an expected validation outcome and must not pollute runtime error telemetry" — 探针失败是预期结果，不应触发 alert
- **最小请求**：`Messages:[{Role:user, Content:"Reply with OK."}]`，仅消耗极少 token
- **20s 超时**：比生产 120s 短，admin UI 不应等待太久
- **返回 Usage 而非 content**：注释明示"assistant content is deliberately discarded"；caller 只需证明结构合法
- **独立 HTTPClient**：避免探针失败影响生产连接池

## 8. 注意事项

- **20s 超时可能不够**：reasoning model 首次响应可能超 20s；探针可能误判为失败
- **消耗 token**：探针会消耗少量 token；高频探针需评估成本
- **不记 metrics**：探针失败不进 `ongrid_llm_requests_total`；dashboard 不显示探针流量
- **不 log 上游细节**：错误信息仅 `%w` 包装，不含上游响应 body；调试时需手动复现
- **`zhipuJWTTransport` 每次重签**：与生产一致，1h TTL 内重复探针开销可忽略
- **`ProbeResult` 仅 Usage**：不返回 model 名、provider 信息；caller 需自行记录
- **`ErrNoAPIKey` 共用**：与 `noopClient` / `openaiClient.Chat` 同 sentinel；caller 用 `errors.Is` 统一判定
- **不验证 model 合法性**：仅检查 Model 非空；model 名错误会由 provider 4xx 返回，探针 `%w` 包装
- **不验证 tools**：探针请求不带 tools；tools schema 错误不会被探针发现
