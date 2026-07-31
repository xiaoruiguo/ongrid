# usage.go

## 1. 概述

本文件实现 `UsageUsecase` —— AIOps 包内的"今天烧了多少 token"聚合器。一个小的只读 usecase，从 `chat_messages` 表汇总 token 消耗。MVP 阶段 dashboard pill 消费的是 cluster-global total；未来 per-user / per-org 预算功能会复用同一个 `SessionRepo.SumTokensSince` hook，搭配更丰富的 filter。

## 2. 包信息

- **包名**：`aiops`（`internal/manager/biz/aiops`）—— 注意是 `aiops` 包，**不是** `aiops/tools` 子包
- **文件**：`usage.go`
- **行数**：约 52 行
- **导入**：`context`、`fmt`、`log/slog`、`time`
- **导出符号**：`UsageUsecase`、`DailyUsage`、`NewUsageUsecase`

## 3. 关键类型与接口

### `UsageUsecase`（结构体）

```go
type UsageUsecase struct {
    repo SessionRepo
    log  *slog.Logger
}
```

仅持有 `SessionRepo`（数据访问 seam）与 logger。无可变状态，线程安全。

### `DailyUsage`（结构体）

```go
type DailyUsage struct {
    Date             time.Time // UTC start-of-day
    PromptTokens     int64
    CompletionTokens int64
    TotalTokens      int64
    Requests         int64
}
```

24 小时窗口的完全汇总 token 报告，`Date` 为 UTC 午夜。`TotalTokens = PromptTokens + CompletionTokens`（在 `Today()` 中计算，不在 repo 层）。

### `SessionRepo`（接口，定义于同包其他文件）

`UsageUsecase` 依赖的窄接口，关键方法 `SumTokensSince(ctx, since) (*TokenSums, error)` 是 hook point——未来 per-user / per-org budgeting 复用此 hook 搭配更丰富 filter。

## 4. 关键函数与流程

### `NewUsageUsecase(repo, log)`

构造器，直接赋值返回。

### `Today(ctx) (*DailyUsage, error)`

唯一业务方法：

```go
func (u *UsageUsecase) Today(ctx context.Context) (*DailyUsage, error) {
    since := time.Now().UTC().Truncate(24 * time.Hour)
    sums, err := u.repo.SumTokensSince(ctx, since)
    if err != nil {
        return nil, fmt.Errorf("aiops usage: sum tokens since %s: %w", since.Format(time.RFC3339), err)
    }
    return &DailyUsage{
        Date:             since,
        PromptTokens:     sums.PromptTokens,
        CompletionTokens: sums.CompletionTokens,
        TotalTokens:      sums.PromptTokens + sums.CompletionTokens,
        Requests:         sums.Requests,
    }, nil
}
```

流程：
1. **UTC 午夜计算**：`time.Now().UTC().Truncate(24 * time.Hour)` 得到今日 UTC 0:00。使用 UTC 而非本地时区，保证跨时区集群口径一致
2. 调 `repo.SumTokensSince(ctx, since)` 拿到 `TokenSums`（`PromptTokens` / `CompletionTokens` / `Requests`）
3. 错误用 `fmt.Errorf` + `%w` 包装，并在消息中带上 `since.Format(time.RFC3339)` 便于排障
4. 在 usecase 层计算 `TotalTokens`（`Prompt + Completion`），不依赖 repo 层做加法
5. 返回 `DailyUsage`，`Date = since`（即 UTC 午夜）

## 5. 依赖关系

- **`SessionRepo`**（同包）：数据访问 seam，`SumTokensSince` 是关键 hook
- **dashboard pill**（消费方）：MVP 数据源，显示 cluster-global total
- **未来 per-user / per-org budgeting**：会复用同一 `SumTokensSince` hook，搭配更丰富 filter

## 6. 并发与资源管理

- `UsageUsecase` 无可变状态，**线程安全**
- `repo SessionRepo` 实现负责自身并发安全
- 无 goroutine、无锁、无 cache
- `Today()` 是只读聚合，无 timeout（依赖 `repo` 层与 ctx 传递）

## 7. 设计模式与亮点

### UTC 午夜口径
显式 `.UTC().Truncate(24h)` 而非本地时区——跨时区集群口径一致，dashboard 数据可比较。

### UseCase 层做加法
`TotalTokens` 在 usecase 层计算而非 repo 层。`TokenSums` 只返回原始三个字段，usecase 组合出 `Total`。这避免 repo 层重复实现加法逻辑，也方便未来调整口径（例如是否包含 cache hit token）。

### 错误包装带时间上下文
`fmt.Errorf("... since %s: %w", since.Format(time.RFC3339), err)` ——错误消息中带查询起点，运维排障无需再翻日志找上下文。

### 窄接口 seam
`SessionRepo.SumTokensSince` 是 hook point——未来 per-user / per-org budgeting 不必新增 usecase，只需扩展 `SumTokensSince` 的 filter 参数（或新增并列方法）。

### MVP 极简
仅 52 行，只暴露一个 `Today()` 方法。不做 per-user、不做时间范围查询、不做 cache——MVP 阶段 dashboard pill 只需要 cluster-global total，YAGNI 原则。

## 8. 注意事项

- **包位置**：本文件在 `aiops` 包，**不在** `aiops/tools` 子包——usage 是更高层的聚合，不属于工具体系
- **UTC 而非本地时区**：跨时区集群必须用 UTC，否则不同 region 的 dashboard 数据不可比较
- **`TotalTokens` 在 usecase 层算**：不要在 repo 层重复实现，避免口径分裂
- **依赖 `SessionRepo.SumTokensSince` 的正确性**：本 usecase 是薄包装，数据准确性由 repo 层保证。若 dashboard 数字异常，先查 repo 层 SQL
- **无 per-call timeout**：依赖 ctx 传递与 repo 层自身的超时控制
- **`Requests` 字段含义**：是 chat_messages 行数（每行 = 一次 LLM call），不是 session 数
- **未来扩展点**：per-user / per-org budgeting 应通过扩展 `SumTokensSince` filter 实现，而非新增 usecase——避免代码分裂
- **`time.Now()` 是 cluster wall clock**：若 manager 节点时钟漂移，"今日"边界会偏移。生产环境应确保 NTP 同步
