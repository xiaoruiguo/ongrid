# graph/callbacks/chain.go

## 1. 概述

本文件是 callbacks 包的装配中心，负责把分散在多个文件的 handler 实例组合成默认有序 callback 链。`NewDefaultHandlers` 是 cutover 层（NEXT PR）每请求调用一次的入口，返回 `[]callbacks.Handler` 切片，最终通过 `compose.WithCallbacks` 在 Invoke 时挂载到 graph。

附带职责：
- `assistantIDRelay`：跨 handler 共享 `chat_messages.id` 的原子指针中继（PersistenceHandler 写入 → SSEHandler 读取）
- `FinalizeBatches`：请求结束后扫一遍 handler 链触发终态清理（当前仅 PersistenceHandler 需要）
- `registerOrExisting`：Prometheus collector 注册去重工具函数

## 2. 包信息

- **包名**：`callbacks`
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/biz/aiops/graph/callbacks`
- **角色**：callback 链装配 + 跨 handler 共享状态
- **依赖**：
  - 标准库 `context`、`errors`、`sync/atomic`
  - `github.com/cloudwego/eino/callbacks`
  - `github.com/prometheus/client_golang/prometheus`
  - `github.com/ongridio/ongrid/internal/pkg/llm`（`llm.BudgetChecker`/`NewBudgetCallbackHandler`）

## 3. 关键类型与接口

### `assistantIDRelay` 结构体（未导出）

```go
type assistantIDRelay struct {
    id atomic.Pointer[string]
}
```

跨 handler 共享的 chat_messages.id 中继。`PersistenceHandler` 在 `OnEnd` 写入 assistant 行后 `store(row.ID)`，`SSEHandler` 在 `OnEnd` 发 `assistant_end` 帧前 `load()` 拿到真实 DB id 附到 SSE payload。

注释明确："Both handlers run on the same goroutine for a given graph iteration (persistence is registered before SSE in NewDefaultHandlers and eino preserves registration order on OnEnd), so a plain string with atomic Load/Store is sufficient — no race between writer and reader within an iteration."

历史背景："Frontend ChatThread used to dedupe by a synthetic assistant-iter-N id (v0.7.63 hotfix) because this field was empty. Now it can use the real DB id."

### `Deps` 结构体

```go
type Deps struct {
    AlertDraftGuard AlertDraftGuardDeps
    Persistence     PersistenceDeps
    SSE             SSEEmitter
    Audit           AuditDeps
    Metrics         MetricsDeps
    BudgetChecker   llm.BudgetChecker
    BudgetUserID    uint64
}
```

每个字段对应一个 handler 的依赖。**每个 handler 把 nil 依赖视作"跳过我"**，所以传部分 Deps 只装配部分 handler（如测试只需 metrics）。

## 4. 关键函数与流程

### `NewDefaultHandlers(deps Deps) []callbacks.Handler`

装配默认有序 callback 链：

```go
out := make([]callbacks.Handler, 0, 6)

if h := NewAlertDraftGuardHandler(deps.AlertDraftGuard); h != nil {
    out = append(out, h)
}

relay := &assistantIDRelay{}  // 跨 handler 共享

if h := NewPersistenceHandler(deps.Persistence); h != nil {
    h.assistantIDRelay = relay
    out = append(out, h)
}
if h := NewSSEHandler(deps.SSE); h != nil {
    h.assistantIDRelay = relay
    out = append(out, h)
}
if h := NewAuditHandler(deps.Audit); h != nil {
    out = append(out, h)
}
if h := NewMetricsHandler(deps.Metrics); h != nil {
    out = append(out, h)
}
if deps.BudgetChecker != nil {
    out = append(out, llm.NewBudgetCallbackHandler(deps.BudgetChecker, deps.BudgetUserID))
}
return out
```

**装配顺序**：AlertDraftGuard → Persistence → SSE → Audit → Metrics → Budget。

注释明确："Order is incidental — eino does not document a guaranteed handler ordering — but the slice is stable across calls so tests can assert positional indices."

→ eino 不保证 handler 顺序，但本切片稳定，便于测试用位置索引断言。

### `FinalizeBatches(ctx, handlers)`

请求结束后扫一遍 handler 链，对支持 finalize 的 handler 调用清理：

```go
func FinalizeBatches(ctx context.Context, handlers []callbacks.Handler) {
    for _, h := range handlers {
        if p, ok := h.(*PersistenceHandler); ok {
            p.FinalizeBatch(ctx)
        }
    }
}
```

当前仅 `PersistenceHandler.FinalizeBatch` 实现。用途：处理"user closed browser mid-batch, request cancels"——`ChatModel.OnStart` hook 只能 in-session autoheal，session 中断时由 chatruntime defer 调用本函数保证 batch 总能 flush。

### `registerOrExisting(reg, c) prometheus.Collector`

包私有 register 工具，镜像 `llm/metrics.go::registerOrExisting` + `tools/decorators/metric.go::regOrExist`：

```go
func registerOrExisting(reg prometheus.Registerer, c prometheus.Collector) prometheus.Collector {
    if reg == nil {
        reg = prometheus.DefaultRegisterer
    }
    err := reg.Register(c)
    if err == nil {
        return c
    }
    var are prometheus.AlreadyRegisteredError
    if errors.As(err, &are) {
        return are.ExistingCollector
    }
    panic(err)
}
```

注释解释放在本包的原因："lives here so persistence.go can use it without triggering an import cycle (both packages are siblings of llm but llm/metrics.go's helper is unexported)."

### `ErrNoHandlers`

```go
var ErrNoHandlers = errors.New("graph/callbacks: no handlers configured")
```

预留给未来使用——要求链非空的 helper 会返回此错误。PR-6 调用方容忍空切片。

## 5. 依赖关系

### 上游
- chatruntime（NEXT PR）每请求调用 `NewDefaultHandlers` 一次，把返回切片通过 `compose.WithCallbacks(handlers...)` 注入 Invoke
- chatruntime 在 `compose.Invoke` 返回后 defer 调用 `FinalizeBatches` 保证 batch flush

### 下游（装配的 handler）
- `AlertDraftGuardHandler`（`alert_draft_guard.go`）
- `PersistenceHandler`（`persistence.go`）
- `SSEHandler`（`sse.go`）
- `AuditHandler`（`audit.go`）
- `MetricsHandler`（`metrics.go`）
- `llm.NewBudgetCallbackHandler`（PR-1，从 `internal/pkg/llm` 包引入）

## 6. 并发与资源管理

### `assistantIDRelay.atomic.Pointer[string]`

```go
func (r *assistantIDRelay) store(id string) {
    if r == nil { return }
    r.id.Store(&id)
}

func (r *assistantIDRelay) load() string {
    if r == nil { return "" }
    if p := r.id.Load(); p != nil {
        return *p
    }
    return ""
}
```

`atomic.Pointer[string]` 提供 load/store 的原子语义。每个 graph iteration 内 Persistence 先写、SSE 后读（eino 保持注册顺序），无竞争；但跨 iteration 仍用 atomic 防止内存可见性问题。

### per-request handler 生命周期

注释明确："Cutover layer (NEXT PR) calls this once per request, threads the returned slice into compose.WithCallbacks at Invoke time, and discards the handlers when the request finishes (so per-call state is bounded by the request lifetime)."

→ 每请求新建 handler 切片，请求结束后丢弃。handler 内部状态（如 PersistenceHandler 的 `toolCalls` map、`currentBatch`）天然 scope 到请求生命周期，无跨请求泄漏。

### 无 goroutine

本文件不开 goroutine，仅装配和共享原子状态。SSEHandler 的 `drainStream` 是 SSEHandler 自己开 goroutine，与本装配层无关。

## 7. 设计模式与亮点

### 部分装配模式

每个 handler 的构造函数返回 nil 表示"依赖不全，跳过我"。`NewDefaultHandlers` 检查 nil 跳过追加。这让测试可以传部分 Deps（如只测 metrics）：

```go
NewDefaultHandlers(Deps{Metrics: MetricsDeps{Registerer: reg}})
// → 只装 MetricsHandler，其他全 nil
```

→ 灵活的依赖注入，无需 mock 全部 handler。

### 跨 handler 共享：assistantIDRelay

```go
relay := &assistantIDRelay{}
if h := NewPersistenceHandler(deps.Persistence); h != nil {
    h.assistantIDRelay = relay
}
if h := NewSSEHandler(deps.SSE); h != nil {
    h.assistantIDRelay = relay
}
```

Persistence 写 chat_messages.id 到 relay，SSE 读 relay 把 id 附到 `assistant_end` 帧。这是避免 SSE 反查 DB 拿 id 的轻量级方案——handler 链装配时建立共享，运行时直接传值。

### FinalizeBatches 的类型断言扫链

```go
for _, h := range handlers {
    if p, ok := h.(*PersistenceHandler); ok {
        p.FinalizeBatch(ctx)
    }
}
```

不引入"Finalizer 接口"，而是类型断言扫链——简单直接。若未来有更多 handler 需要 finalize，加 `|| f, ok := h.(*FooHandler)` 即可。

### registerOrExisting 三处镜像

注释明确：本函数在 `callbacks/chain.go`、`llm/metrics.go`、`tools/decorators/metric.go` 三处独立实现。原因是 `llm/metrics.go` 的 helper 未导出，siblings 包不能直接复用。这是 Go 包边界设计的常见折衷——重复代码 vs 循环依赖。

## 8. 注意事项

### eino handler 顺序未文档化保证

注释三次强调：
- "Order is incidental — eino does not document a guaranteed handler ordering"
- "persistence is registered before SSE in NewDefaultHandlers and eino preserves registration order on OnEnd"
- "the slice is stable across calls so tests can assert positional indices"

→ 当前依赖 eino 保持注册顺序（Persistence 在 SSE 前），但这是实现细节不是契约。若未来 eino 改变行为，`assistantIDRelay` 的"Persistence 写完 → SSE 读"假设可能失效。

### `assistantIDRelay` 跨 iteration 复用

`relay` 在 `NewDefaultHandlers` 中创建一次，所有 iteration 共享。`PersistenceHandler.persistAssistant` 每次 `store(row.ID)` 覆盖前一次，`SSEHandler.OnEnd` 读取当前值。若 Persistence 未写入（如 AssistantMessage 为 nil），SSE 会读到上一 iteration 的 id——可能产生错误关联。当前代码未防御此情况。

### Budget handler 来自 `internal/pkg/llm`

```go
if deps.BudgetChecker != nil {
    out = append(out, llm.NewBudgetCallbackHandler(deps.BudgetChecker, deps.BudgetUserID))
}
```

BudgetCallbackHandler 是 PR-1 的产物，不在 callbacks 包内，从 `llm` 包引入。这是 PR-1 与 PR-6 的衔接点——BudgetChecker 接口在 llm 包定义。

### `ErrNoHandlers` 未使用

```go
var ErrNoHandlers = errors.New("graph/callbacks: no handlers configured")
```

注释明确："Reserved for future use; PR-6 callers tolerate an empty slice."——PR-6 调用方容忍空切片，本错误暂未使用。未来若有 helper 要求链非空，可启用。

### `FinalizeBatches` 仅扫 PersistenceHandler

当前实现仅对 `*PersistenceHandler` 做 finalize。若未来 MetricsHandler / AuditHandler 也需要终态清理，需要扩展本函数（或引入 Finalizer 接口）。

### `registerOrExisting` panic 不可恢复

```go
panic(err)
```

`Register` 失败且非 `AlreadyRegisteredError` 时 panic。这是合理的——Prometheus register 失败通常是编程错误（collector 描述符冲突），fail-fast 优于 silent ignore。但生产环境若发生会终止进程。
