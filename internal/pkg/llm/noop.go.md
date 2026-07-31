# `noop.go` 技术实现文档

> 源文件：`internal/pkg/llm/noop.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/llm`

## 1. 概述

本文件实现 `Client` 接口的 no-op 占位实现：`noopClient`。当 `OPENAI_API_KEY` 未设且无 Resolver 时，`New` 返回 `noopClient`，其 `Chat` 永远返回 `ErrNoAPIKey`。让本地开发环境无需配 key 也能编译运行 agent wiring，直到操作者提供真实 key。

## 2. 包信息

- **包名**：`llm`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被 `client.go` 的 `New` / `NewWithResolver` 在无 APIKey 时返回；仅依赖标准库 `context`

## 3. 关键类型与接口

```go
type noopClient struct{}

// 实现 Client 接口
func (noopClient) Chat(ctx context.Context, req ChatReq) (*ChatResp, error)
```

## 4. 关键函数与流程

### `noopClient.Chat`
- **签名**：`func (noopClient) Chat(ctx context.Context, req ChatReq) (*ChatResp, error)`
- **职责**：永远返回 `ErrNoAPIKey`
- **流程**：
  1. `_ = ctx`（显式忽略）
  2. `_ = req`（显式忽略）
  3. 返回 `nil, ErrNoAPIKey`
- **错误处理**：固定返回 sentinel error

## 5. 依赖关系

- **内部包**：同包 `client.go`（`Client` 接口、`ChatReq` / `ChatResp` 类型、`ErrNoAPIKey` sentinel）
- **外部库**：无
- **被调用方**：`client.go` 的 `New` / `NewWithResolver` 在无 APIKey 且无 resolver 时返回 `&noopClient{}`

## 6. 并发与资源管理

无并发控制。`noopClient` 是无状态空结构体，可被多 goroutine 安全共享。`Chat` 是纯函数式（固定返回值）。

## 7. 设计模式与亮点

- **Null Object 模式**：用空实现替代 nil Client，让 caller 无需 nil 检查；`Chat` 直接返回明确 sentinel error
- **本地开发友好**：注释明示"the main binary uses this during local dev so the agent wiring still compiles and runs until an operator provides a real key"
- **sentinel error 统一**：`ErrNoAPIKey` 与 `openaiClient.Chat` 在 apiKey 空时返回的 sentinel 一致，caller 用单一 `errors.Is` 判定
- **`_ = ctx` / `_ = req` 显式忽略**：符合 lint 要求（不忽略参数需显式标注）

## 8. 注意事项

- **永远失败**：`noopClient.Chat` 永远返回 `ErrNoAPIKey`；caller 不应假设它可能成功
- **无 metrics / log**：与 `openaiClient` 不同，noop 不记 metrics 不 log；若 caller 期望 metrics 需自行处理
- **无预算门控**：noop 不调 `budget.Check` / `Record`；caller 不应依赖 noop 触发预算逻辑
- **`ErrNoAPIKey` 是 sentinel**：caller 用 `errors.Is(err, ErrNoAPIKey)` 判定；noop 不包装
- **构造时机**：`New` 与 `NewWithResolver` 在 resolver nil 且 cfg.APIKey 空 时返回 noop；若 resolver 非 nil 但 Resolve 返回空 apiKey，`openaiClient.Chat` 自己返回 `ErrNoAPIKey`（不退化为 noop）
- **无状态**：noop 无字段；多次 `Chat` 调用之间无副作用
