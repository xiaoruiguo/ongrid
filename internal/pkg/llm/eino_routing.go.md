# `eino_routing.go` 技术实现文档

> 源文件：`internal/pkg/llm/eino_routing.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/llm`

## 1. 概述

本文件是 eino-typed LLM 层（Agent eino skill framework）：提供 `RoutingChatModel`（按 provider id 分发到预构建 inner ChatModel 的 eino `model.ChatModel`）、`WithProvider` option（per-call 选 provider）、`NewClientChatModel`（把现有 `llm.Client` 适配为 eino `model.ToolCallingChatModel`）。PR-1 仅 scaffolding，**不在 live agent loop 接入**；wiring 是后续 PR。6 个 provider 全部 OpenAI-compatible，单一适配器足够。

## 2. 包信息

- **包名**：`llm`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被 eino graph（后续 PR）调用；依赖 `github.com/cloudwego/eino/components/model`、`github.com/cloudwego/eino/schema`

## 3. 关键类型与接口

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

var ErrUnknownProvider = errors.New("llm: unknown provider")

type providerOpts struct{ provider string }  // 私有 option bag

type RoutingChatModel struct {
    inner           map[string]model.ChatModel
    defaultProvider string
    defaultResolver func(ctx) (provider, mdl string)  // 动态默认
}

type RoutingChatModelConfig struct {
    Inner           map[string]model.ChatModel
    DefaultProvider string
    DefaultResolver func(ctx) (provider, mdl string)
}

type clientChatModel struct {
    inner       Client
    model       string
    userID      uint64
    boundTools  []*schema.ToolInfo
}

type derivedChatModel struct{ tcm model.ToolCallingChatModel }  // WithTools 派生适配器
```

实现接口（编译期断言）：
- `model.ChatModel`
- `model.ToolCallingChatModel`

## 4. 关键函数与流程

### `WithProvider`
- **签名**：`func WithProvider(provider string) model.Option`
- **职责**：返回 eino option，per-call 选 inner provider
- **流程**：`model.WrapImplSpecificOptFn(func(o *providerOpts) { o.provider = provider })`

### `NewRoutingChatModel`
- **签名**：`func NewRoutingChatModel(cfg RoutingChatModelConfig) (*RoutingChatModel, error)`
- **职责**：构造路由器；校验 Inner 非空、DefaultProvider 在 Inner 中
- **流程**：
  1. Inner 空 → 报错
  2. DefaultProvider 空 → 报错
  3. DefaultProvider 不在 Inner → 报错
  4. 防御性 copy Inner map；nil 值报错
  5. 返回 `&RoutingChatModel{inner: cp, defaultProvider, defaultResolver}`
- **错误处理**：配置错误返回 error

### `withDynamicDefault`
- **签名**：`func (r *RoutingChatModel) withDynamicDefault(ctx, opts) []model.Option`
- **职责**：为未指定 provider 的调用注入 live 配置的默认 provider + model
- **流程**：
  1. defaultResolver nil → 返回原 opts
  2. 已指定 provider → 返回原 opts
  3. `defaultResolver(ctx)` 取 (prov, mdl)；prov 空 → 返回原 opts
  4. prov 不在 inner → 返回原 opts
  5. 追加 `WithProvider(prov)`；若 mdl 非空且调用方未指定 Model → 追加 `model.WithModel(mdl)`
- **设计意图**：让 home-page model picker 的运行时修改对 background consumer（RCA investigator、query_translate）生效

### `pick`
- **签名**：`func (r *RoutingChatModel) pick(opts ...) (model.ChatModel, string, error)`
- **职责**：解析 inner ChatModel
- **流程**：从 opts 取 providerOpts；空 → defaultProvider；从 inner map 取；不存在 → `ErrUnknownProvider`

### `Generate / Stream`
- **签名**：`func (r *RoutingChatModel) Generate/Stream(ctx, input, opts ...) (...)`
- **职责**：分发到 inner model
- **流程**：`withDynamicDefault` → `pick` → `inner.Generate/Stream`

### `BindTools`
- **签名**：`func (r *RoutingChatModel) BindTools(tools []*schema.ToolInfo) error`
- **职责**：legacy 路径，fan-out 到所有 inner
- **流程**：遍历 inner 调 `BindTools`；失败 `%w`
- **注意**：注释提到"inherits eino's non-atomic caveat"；新代码应优先 `WithTools`

### `WithTools`
- **签名**：`func (r *RoutingChatModel) WithTools(tools) (model.ToolCallingChatModel, error)`
- **职责**：返回新 RoutingChatModel，inner 全部绑定 tools；receiver 不变
- **流程**：
  1. 浅拷贝 RoutingChatModel
  2. 遍历 inner：
     - 若实现 `ToolCallingChatModel` → `tcm.WithTools(tools)` 派生，包成 `derivedChatModel`
     - 否则 legacy `BindTools` in-place
  3. 返回新实例
- **错误处理**：任一 provider 失败 `%w`

### `Providers / DefaultProvider`
- **职责**：返回注册 provider 列表 / 默认 provider id（admin UI 与测试用）

### `NewClientChatModel`
- **签名**：`func NewClientChatModel(cfg ClientChatModelConfig) (model.ChatModel, error)`
- **职责**：把 `llm.Client` 适配为 eino ChatModel
- **流程**：cfg.Client nil → 报错；否则返回 `&clientChatModel{inner, model, userID}`

### `clientChatModel.Generate / Stream / BindTools / WithTools / buildChatReq`
- **职责**：eino ↔ llm.Client 适配
- **流程**：
  - `Generate`：`buildChatReq` → `inner.Chat` → `einoMessageFromChatResp`
  - `Stream`：调 Generate，包成单 chunk `StreamReader`（PR-1 scaffolding，真实 streaming 后续 PR）
  - `BindTools`：存 `boundTools`，每次 Generate 转发（除非 per-call WithTools 覆盖）
  - `WithTools`：浅拷贝 + 设 boundTools，receiver 不变
  - `buildChatReq`：翻译 messages（`einoMessageToLLM`）、tools（`einoToolsToLLM`）、temperature、model

### `einoMessageToLLM / einoMessageFromChatResp / einoToolsToLLM / paramsToJSONSchema`
- **职责**：eino ↔ llm 类型转换
- **流程**：
  - `einoMessageToLLM`：保留 Role/Content/ToolCallID/ToolName；ToolCalls 转 llm.ToolCall；空 args → `{}`
  - `einoMessageFromChatResp`：填 ResponseMeta.Usage 让 BudgetCallbackHandler 读
  - `einoToolsToLLM`：ParamsOneOf → JSON Schema
  - `paramsToJSONSchema`：nil → `{"type":"object","properties":{}}`；否则 `p.ToJSONSchema()`

## 5. 依赖关系

- **内部包**：同包 `client.go`（`Client` / `ChatReq` / `Message` / `ToolCall` / `ToolSchema` / `Usage` / `ChatResp`）
- **外部库**：
  - `github.com/cloudwego/eino/components/model`
  - `github.com/cloudwego/eino/schema`
- **被调用方**：eino graph（后续 PR wiring）、admin UI、测试

## 6. 并发与资源管理

- **`RoutingChatModel.inner` 构造后只读**：注释明示"inner ChatModels are never reassigned"；Generate/Stream 并发安全
- **`BindTools` 非原子**：注释明示"inherits eino's documented non-atomic caveat"
- **`WithTools` 派生新实例**：receiver 不变，并发安全
- **`clientChatModel` 浅拷贝**：`WithTools` 用 `cp := *c`，boundTools 指针共享但替换为 new slice，安全
- **`defaultResolver` 调用未加锁**：注释未明示；假设 resolver 实现自身线程安全

## 7. 设计模式与亮点

- **路由 + 适配器模式**：RoutingChatModel 路由，clientChatModel 适配现有 llm.Client；避免引入 `eino-ext/components/model/openai`（注释提到"its dep surface is heavy and PR-1 is scaffolding only"）
- **`WithProvider` impl-specific option**：用 eino 的 `WrapImplSpecificOptFn` 把 provider 选择塞进 option 流，与 `model.WithTemperature` 等共存
- **动态默认 resolver**：让 home-page model picker 的运行时修改对 background consumer 生效；pinned provider 不被覆盖
- **`derivedChatModel` 桥接**：`WithTools` 派生的 `ToolCallingChatModel` 无 `BindTools`，包成 `derivedChatModel` 满足 inner map 的 `model.ChatModel` 类型（`BindTools` no-op stub）
- **`Stream` 单 chunk 适配**：PR-1 scaffolding，把 Generate 结果包成 `StreamReaderFromArray`，满足 eino 接口；真实 streaming 后续 PR
- **`paramsToJSONSchema` nil 兜底**：nil 时返回 `{"type":"object","properties":{}}`，因某些 OpenAI-compatible provider 拒绝缺失 schema
- **编译期接口断言**：`_ model.ChatModel = (*RoutingChatModel)(nil)` 等

## 8. 注意事项

- **PR-1 不接入 live agent loop**：注释明示"MUST NOT be wired into the live agent loop in this PR — wiring is a later PR"
- **`Stream` 非真实 streaming**：注释明示"Real token-by-token streaming arrives in a later PR"；当前是 Generate + 单 chunk
- **`BindTools` 非原子**：新代码应优先 `WithTools`；legacy 路径有并发 caveat
- **`defaultResolver` 不加锁**：假设实现线程安全；若 resolver 有内部状态需自行加锁
- **6 provider 全 OpenAI-compatible**：注释明示"all OpenAI-compatible"；单一适配器足够；若未来引入非兼容 provider 需扩展
- **`einoMessageToLLM` 丢 multimodal**：注释明示"multimodal content is dropped on the floor in PR-1 (text-only first)"
- **`paramsToJSONSchema` 依赖 `p.ToJSONSchema()`**：eino schema 包的公共 API；版本升级可能影响
- **`WithTools` 派生 `derivedChatModel`**：其 `BindTools` 是 no-op stub；若后续代码假设 BindTools 生效会出错
- **`ErrUnknownProvider` 是 sentinel**：caller 用 `errors.Is` 判定；本文件不包装
- **provider 列表与 admin settings 锁定**：注释明示"Keep in lockstep with the admin settings page (6 providers)"
