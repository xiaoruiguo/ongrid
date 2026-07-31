# `budget_callback.go` 技术实现文档

> 源文件：`internal/pkg/llm/budget_callback.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/llm`

## 1. 概述

本文件是 eino-side 预算门控：暴露 `callbacks.Handler` 让 eino graph（通过 `callbacks.AppendGlobalHandlers` 或 `compose.WithCallbacks` 接入）经过与 legacy `Client → BudgetChecker` 路径相同的 per-day token 上限。`OnStart` 估算 prompt tokens 调 `Check`，`OnEnd` 读 `ResponseMeta.Usage` 调 `Record`。eino Handler 契约无 early-exit，故拒绝信号通过 ctx 传递，graph node（PR-2）调用 `BudgetRejectionFromContext` 转 hard error。

## 2. 包信息

- **包名**：`llm`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被 eino graph 调用（通过 callbacks 注册）；依赖 `github.com/cloudwego/eino` 系列

## 3. 关键类型与接口

```go
type budgetRejectKey struct{}  // ctx key 携带拒绝 error

type BudgetCallbackHandler struct {
    checker BudgetChecker
    userID  uint64

    checks   atomic.Uint64
    rejects  atomic.Uint64
    records  atomic.Uint64
    tokensIn atomic.Uint64
}

type BudgetCallbackStats struct {
    Checks, Rejects, Records, TokensIn uint64
}
```

实现接口（编译期断言）：
- `callbacks.Handler`
- `callbacks.TimingChecker`

## 4. 关键函数与流程

### `NewBudgetCallbackHandler`
- **签名**：`func NewBudgetCallbackHandler(checker BudgetChecker, userID uint64) *BudgetCallbackHandler`
- **职责**：构造 handler；checker 为 nil 时是 no-op
- **流程**：返回 `&BudgetCallbackHandler{checker, userID}`

### `Needed`
- **签名**：`func (h *BudgetCallbackHandler) Needed(_ ctx, info *callbacks.RunInfo, timing callbacks.CallbackTiming) bool`
- **职责**：让 eino 跳过不关心的 timing 与非 ChatModel 组件
- **流程**：
  1. h nil 或 checker nil → false
  2. info.Component 非空且非 `ComponentOfChatModel` → false
  3. timing 为 `OnStart` 或 `OnEnd` → true；其他 → false

### `OnStart`
- **签名**：`func (h *BudgetCallbackHandler) OnStart(ctx, info, input) context.Context`
- **职责**：估算 prompt tokens 调 Check；拒绝时把 error 塞 ctx
- **流程**：
  1. h/checker nil → 返回 ctx
  2. 非 ChatModel → 返回 ctx
  3. `einomodel.ConvCallbackInput(input)` 转 model input；nil → 返回 ctx
  4. `checks.Add(1)`
  5. `est = estimateEinoPromptTokens(mi.Messages)`
  6. `checker.Check(ctx, userID, est)` 失败：`rejects.Add(1)`，`context.WithValue(ctx, budgetRejectKey{}, err)`
  7. 否则返回 ctx
- **错误处理**：拒绝不返回 error（eino 契约无 early-exit），通过 ctx 传递

### `OnEnd`
- **签名**：`func (h *BudgetCallbackHandler) OnEnd(ctx, info, output) context.Context`
- **职责**：从 model 输出读 Usage 调 Record
- **流程**：
  1. h/checker nil → 返回 ctx
  2. 非 ChatModel → 返回 ctx
  3. `einomodel.ConvCallbackOutput(output)` 转 model output；nil → 返回 ctx
  4. `extractUsage(mo)`；nil → 返回 ctx
  5. `records.Add(1)`、`tokensIn.Add(usage.TotalTokens)`
  6. `checker.Record(ctx, userID, *usage)` — 失败 swallow（注释明示"Recording failures are swallowed so they never fail a user-visible request"）
- **错误处理**：Record 失败仅 swallow

### `OnError / OnStartWithStreamInput / OnEndWithStreamOutput`
- **职责**：PR-1 no-op；stream 路径 drain + close 防 goroutine 泄漏
- **流程**：OnError 返回 ctx；stream 路径 `in.Close()` / `out.Close()`

### `Stats`
- **签名**：`func (h *BudgetCallbackHandler) Stats() BudgetCallbackStats`
- **职责**：返回当前计数快照（测试与 admin debug 用）

### `BudgetRejectionFromContext`
- **签名**：`func BudgetRejectionFromContext(ctx context.Context) error`
- **职责**：从 ctx 读 OnStart 写入的拒绝 error，无则 nil
- **流程**：`ctx.Value(budgetRejectKey{})`；断言为 error 返回

### `estimateEinoPromptTokens`
- **签名**：`func estimateEinoPromptTokens(msgs []*schema.Message) int`
- **职责**：eino-typed 消息切片的 prompt token 估算（私有，与 client.go:465 同款规则）
- **流程**：每消息 overhead 4 + content len/4 + 各 ToolCall.Name/Arguments len/4

### `extractUsage`
- **签名**：`func extractUsage(mo *einomodel.CallbackOutput) *Usage`
- **职责**：从 eino model output 读 Usage
- **流程**：
  1. `mo.TokenUsage` 非空 → 转成 `Usage`
  2. 否则若 `mo.Message.ResponseMeta.Usage` 非空 → 转成 `Usage`
  3. 都无 → nil

## 5. 依赖关系

- **内部包**：同包 `client.go`（`BudgetChecker` / `Usage`）
- **外部库**：
  - `github.com/cloudwego/eino/callbacks`
  - `github.com/cloudwego/eino/components`
  - `github.com/cloudwego/eino/components/model`
  - `github.com/cloudwego/eino/schema`
- **被调用方**：eino graph（PR-2 wiring）、测试

## 6. 并发与资源管理

- **`atomic.Uint64` 计数器**：checks/rejects/records/tokensIn 原子读写，无锁
- **`BudgetCallbackHandler` 字段只读**（除原子计数器），安全并发
- **ctx 传递拒绝**：`context.WithValue` 是不可变派生，并发安全
- **stream 路径 drain+close**：防 goroutine 泄漏（eino docs 要求）
- **Record 失败 swallow**：不阻塞用户请求

## 7. 设计模式与亮点

- **ctx 传递软拒绝**：eino Handler 契约无 early-exit，用 `context.WithValue` 把拒绝 error 从 OnStart 传到 graph node，node 调 `BudgetRejectionFromContext` 转 hard error。注释详述"PR-1 ships the plumbing; PR-2 wires the check into the node"
- **`TimingChecker` 接口**：实现 `Needed` 让 eino 跳过不关心的 timing（stream、OnError），减少开销
- **双路径防 double-counting**：注释明示"If both gates run for the same call, double-counting is avoided because the graph path goes through callbacks-only and the direct path goes through openaiClient.Chat directly"
- **Usage 双源 fallback**：`extractUsage` 优先 `mo.TokenUsage`，回退到 `mo.Message.ResponseMeta.Usage`，覆盖不同 eino 集成的填充习惯
- **stream drain**：注释明示"Drain & close so we don't leak goroutines — eino docs require it"
- **编译期接口断言**：`var _ callbacks.Handler = (*BudgetCallbackHandler)(nil)` 等
- **Stats 测试可见**：计数器暴露给测试与 admin debug

## 8. 注意事项

- **OnStart 不阻止调用**：eino 契约限制；graph node 必须主动调 `BudgetRejectionFromContext` 才能 fail-fast，否则模型调用仍会发生（仅 Record 不记）
- **PR-2 未接入前是软门控**：注释明示"PR-1 ships the plumbing; PR-2 wires the check"；当前 graph 未读取 ctx 拒绝信号时，预算超限不会真正阻止调用
- **stream 路径未实现**：注释明示"Stream accounting lands in a later PR"；当前 OnStartWithStreamInput / OnEndWithStreamOutput 仅 drain+close
- **userID 固定**：注释明示"PR-N may swap in a per-call resolver once tenancy lands"；当前单租户 pivot 用固定 bucket
- **`extractUsage` 可能 nil**：某些 provider 不返回 Usage；OnEnd 直接返回不 Record
- **`estimateEinoPromptTokens` 是粗估**：注释明示"real billing is the Usage that comes back on OnEnd"；估算仅用于 pre-call gate
- **Record 失败 swallow**：与 `client.go:341` 同款策略；预算记录失败不影响用户请求
- **`Needed` 检查 Component**：仅 ChatModel 触发；其他组件（如 retriever）不进入预算门控
