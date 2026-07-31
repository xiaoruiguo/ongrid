# `budget.go` 技术实现文档

> 源文件：`internal/pkg/llm/budget.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/llm`

## 1. 概述

本文件实现 `BudgetChecker` 接口的 MVP 形态：`InMemoryBudget`，单全局 per-UTC-day token 上限。适合单租户 MVP；未来切换到 MySQL/sqlite `usage_daily` 表时是 drop-in 替换。上限是全局的（非 per-user），因为单租户 MVP 无 per-user billing 表面；userID 仍透传以便未来 per-user 后端即插即用。

## 2. 包信息

- **包名**：`llm`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被 `client.go`（`openaiClient.Chat` 调用 `Check` / `Record`）、`budget_callback.go`（eino 路径）调用；仅依赖标准库 `context`、`sync`、`time`

## 3. 关键类型与接口

```go
type InMemoryBudget struct {
    mu         sync.Mutex
    dailyLimit int            // tokens per UTC day；<=0 表示无限
    used       map[string]int // key = "YYYY-MM-DD" (UTC)
    now        func() time.Time
}
```

实现 `client.go` 定义的 `BudgetChecker` 接口：
```go
type BudgetChecker interface {
    Check(ctx context.Context, userID uint64, estPromptTokens int) error
    Record(ctx context.Context, userID uint64, usage Usage) error
}
```

## 4. 关键函数与流程

### `NewInMemoryBudget`
- **签名**：`func NewInMemoryBudget(dailyLimit int) *InMemoryBudget`
- **职责**：构造 InMemoryBudget；dailyLimit <= 0 表示无限
- **流程**：返回 `&InMemoryBudget{dailyLimit, map[string]int{}, time.Now}`

### `Check`
- **签名**：`func (b *InMemoryBudget) Check(ctx, userID uint64, estPromptTokens int) error`
- **职责**：若 estPromptTokens 会令今日累计超 dailyLimit，返回 `ErrBudgetExceeded`
- **流程**：
  1. `dailyLimit <= 0` → 返回 nil（无限）
  2. mu.Lock，defer Unlock
  3. `key = b.dayKey()`
  4. `b.used[key] + estPromptTokens > dailyLimit` → 返回 `ErrBudgetExceeded`
  5. 否则 nil
- **错误处理**：仅返回 `ErrBudgetExceeded`；不阻塞不 sleep
- **注意**：`ctx` 与 `userID` 被显式忽略（`_ = ctx`、`_ = userID`），因为当前是全局上限

### `Record`
- **签名**：`func (b *InMemoryBudget) Record(ctx, userID uint64, usage Usage) error`
- **职责**：把 usage.TotalTokens 累加到当前 UTC day bucket
- **流程**：mu.Lock，`b.used[dayKey()] += usage.TotalTokens`，Unlock
- **错误处理**：永不返回错误（map 操作不失败）

### `Used`
- **签名**：`func (b *InMemoryBudget) Used() int`
- **职责**：返回当前 UTC day 已用 token 数（测试用，best-effort gauge）

### `dayKey`
- **签名**：`func (b *InMemoryBudget) dayKey() string`
- **职责**：返回 UTC 当日 `"YYYY-MM-DD"` 字符串作为 bucket key

## 5. 依赖关系

- **内部包**：无（同包 `client.go` 定义 `BudgetChecker` / `Usage` / `ErrBudgetExceeded`）
- **外部库**：无
- **被调用方**：
  - `client.go`：`openaiClient.Chat` 在网络调用前 `Check`，成功后 `Record`
  - `budget_callback.go`：`BudgetCallbackHandler.OnStart` / `OnEnd` 调用

## 6. 并发与资源管理

- **`sync.Mutex`** 保护 `used` map，并发 Check/Record 安全
- **无 goroutine**：纯同步
- **`now func() time.Time`** 注入点，测试可替换为 fake clock
- **`ctx` 不影响逻辑**：Check/Record 忽略 ctx 取消，因为操作是非阻塞的 map 读写

## 7. 设计模式与亮点

- **接口 + 实现分离**：`BudgetChecker` 接口在 `client.go`，本文件提供 MVP 实现；未来 MySQL/sqlite 实现是 drop-in
- **dailyLimit <= 0 = 无限**：让"禁用预算"成为零值配置，caller 无需特判
- **全局上限而非 per-user**：注释明示"single-tenant pivot" — 当前无 per-user billing 表面；userID 透传以便未来扩展
- **UTC day bucket**：避免时区导致日界漂移；`now` 注入让测试可控
- **Check 不阻塞**：注释明示"never blocks or sleeps"，超限直接拒绝
- **Used 暴露给测试**：注释明示"Exposed for tests; callers should treat it as a best-effort gauge"

## 8. 注意事项

- **无 per-user 隔离**：当前所有 user 共享 global bucket；多用户场景需切换实现
- **进程内状态**：重启后 used 清零；若需持久化需切换 MySQL/sqlite 实现
- **无 day 清理**：`used` map 累积历史 day key，长期运行内存增长；生产需定期清理或换 DB 实现
- **Check 与 Record 非原子**：Check 后到 Record 之间可能有并发请求插入，导致实际超限；MVP 可接受，精确预算需原子事务
- **`ctx` 被忽略**：注释明示 `_ = ctx`；若需 ctx 取消响应需扩展
- **`userID` 被忽略**：注释明示 `_ = userID`；未来 per-user 实现时启用
- **`Usage` 字段**：`Record` 只累加 `TotalTokens`，不分别记 prompt/completion；若需细分需扩展
- **`ErrBudgetExceeded` 是 sentinel**：caller 用 `errors.Is` 判定；本文件不包装
