# `llm_choice.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/basetool/llm_choice.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools/basetool`

## 1. 概述

本文件实现 cluster 当前激活的 LLM provider+model 选择的 ctx 传播，与 locale.go 对称。chatruntime 在 coordinator 的 ctx 上 set 该值；`tools/agent_tool` 在 `InvokableRun` 内 read，填充 sub-agent 的 `SpawnWorkerRequest`。chatruntime 不能 import tools（会闭依赖环），所以需要 basetool 作为两边都能依赖的叶子包。

## 2. 包信息

- **包名**：`basetool`
- **所属模块**：叶子包
- **依赖方向**：被 chatruntime（set）、`tools/agent_tool`（read）调用；依赖标准库 `context`

## 3. 关键类型与接口

```go
type llmProviderCtxKeyT struct{}
type llmModelCtxKeyT struct{}

var (
    llmProviderCtxKey = llmProviderCtxKeyT{}
    llmModelCtxKey    = llmModelCtxKeyT{}
)
```

provider 和 model 各用独立 ctx key。

## 4. 关键函数与流程

### `WithLLMChoice`
- **签名**：`func WithLLMChoice(ctx context.Context, provider, model string) context.Context`
- **职责**：stamp coordinator 为自己 ChatModel 调用解析的 (provider, model) 对
- **流程**：`provider != ""` → WithValue provider；`model != ""` → WithValue model；空字段为 no-op，保留 back-compat
- **错误处理**：无

### `LLMProviderFromContext / LLMModelFromContext`
- **签名**：`func LLMProviderFromContext(ctx context.Context) string` / `func LLMModelFromContext(ctx context.Context) string`
- **职责**：返回 coordinator 的 provider id / model 名，无则 `""`
- **流程**：类型断言取值

## 5. 依赖关系

- **内部包**：无
- **外部库**：标准库 `context`
- **被调用方**：`chatruntime`（producer）、`tools/agent_tool.go`（consumer）

## 6. 并发与资源管理

- 不可变字符串 ctx value，并发安全
- 无锁

## 7. 设计模式与亮点

- **解决 RoutingChatModel fallback bug**：未传递时 runWorker 的 g.Invoke 不 thread chatModelOpts，RoutingChatModel 回退默认 "openai"，无 OpenAI key 的 install 看 specialist 失败报 `provider "openai" not configured`。本机制把 cluster default（deepseek/zhipu 等）透传给 sub-agent
- **空字段 no-op**：保留 investigator auto-spawn 路径 back-compat（该路径无显式 choice）
- **provider/model 分离存储**：caller 可只 set 一个，另一字段保留原默认

## 8. 注意事项

- **空字符串 = 不覆盖**：caller 传空字符串不会清空已存在的值
- **决策时机**：coordinator 解析后立即 stamp；sub-agent 拿到的是 coordinator 解析时的快照
- **未透传后果**：未 stamp 时 specialist 用 worker 自身默认 provider，可能导致配置不匹配
