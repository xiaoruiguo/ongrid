# query_logql_basetool.go

## 1. 概述

本文件实现 `query_logql` 工具的 BaseTool 形态。镜像闭包路径（见 `query_logql.go`）：相同 args、相同超时、相同输出字节。注释明示 "same args, same timeouts, same output bytes"。

WhenToUse 明示：用 log 内容查询（grep error / panic / fatal，看 service 失败的行文本，统计 log volume）。NOT for filesystem state（用 host_files skill）/ metric trends（用 query_promql）/ traces（用 query_traceql）。

## 2. 包信息

- **包名**：`tools`
- **路径**：`internal/manager/biz/aiops/tools/query_logql_basetool.go`
- **导入**：
  - `basetool`
  - `logquery`（同闭包路径）
  - 额外 `log/slog`
- **Class**：`read`

## 3. 关键类型与接口

### `QueryLogQLTool`

```go
type QueryLogQLTool struct {
    logQuery LogQuerier  // 复用 query_logql.go 声明的接口
    log      *slog.Logger
}
```

`LogQuerier` / `QueryLogQLArgs` / `QueryLogQLSchema` / `QueryLogQLDescription` / `ToolNameQueryLogQL` / `queryLogqlCallTimeout` / `parseLogQLTime` 均复用 `query_logql.go` 的定义。

## 4. 关键函数与流程

### `NewQueryLogQLTool(lq, log)`

`log == nil` → `slog.Default()`。

### `queryLogQLWhenToUse`（常量）

英文 LLM-facing 文案，反 guard 明确：

- 用途：log CONTENT 查询（grep error/panic/fatal，看 service 失败行文本，统计 log volume over time）
- NOT for：filesystem state / file names / file sizes（用 host_files skill）
- NOT for：metric trends like cpu/mem（用 query_promql）
- NOT for：traces / span timelines（用 query_traceql）

### `Info(_ context.Context)`

返回 `ToolInfo`：Name=`query_logql`，Class=`read`，含 WhenToUse。

### `InvokableRun(ctx, argsJSON, _ ...)`

主流程与 `executeQueryLogQL` 完全镜像：

1. 校验 `logQuery == nil` → error。
2. Unmarshal `QueryLogQLArgs`。
3. `Query` trim 后空 → error。
4. 时间解析：
   - `end = time.Now()`，`start = end.Add(-time.Hour)`
   - `in.End` 非空 → `parseLogQLTime(in.End, end)` 覆盖 end
   - `in.Start` 非空 → `parseLogQLTime(in.Start, start)` 覆盖 start
   - else if `in.End != ""` → `start = end.Add(-time.Hour)`（保持 1h 窗口相对 end）
5. `Limit ≤ 0 → 200`，`Direction` 空 → "backward"。
6. `context.WithTimeout(ctx, 30s)`，调 `logQuery.QueryRange`。
7. Marshal `*QueryRangeResult` 原样返回。

## 5. 依赖关系

| 方向 | 依赖 | 说明 |
|------|------|------|
| 上游 | `basetool.BaseTool` / `ToolInfo` | 实现 BaseTool 接口 |
| 下游 | `LogQuerier.QueryRange`（接口，复用自 `query_logql.go`） | 数据源 |
| 共享 | `query_logql.go` 中的 `LogQuerier` / `QueryLogQLArgs` / `QueryLogQLSchema` / `parseLogQLTime` / `queryLogqlCallTimeout` | 避免重复定义 |

## 6. 并发与资源管理

- `context.WithTimeout(ctx, 30s)` per call。
- `Limit` cap 5000。
- 无共享状态，并发安全。

## 7. 设计模式与亮点

- **byte-for-byte 镜像承诺**：注释明示 "same args, same timeouts, same output bytes"。所有逻辑（时间解析、limit 修正、direction 默认、QueryRange 调用、Marshal 响应）都与闭包路径一致，便于未来一行 type alias 切换。
- **WhenToUse 强反 guard**：明确三个 NOT for（filesystem / metric / trace），引导 LLM 在 log 问题时选对工具。
- **复用 `LogQuerier` 接口**：不在 BaseTool 文件重复声明，直接 import 自 `query_logql.go`，避免接口 drift。
- **复用 `parseLogQLTime` helper**：时间解析逻辑共享，保证两路径对 `now-1h` 这类相对时间的解析完全一致。

## 8. 注意事项

- **drift 风险**：与 `query_logql.go` 是两份并行实现，任何一边改逻辑（如调整默认窗口、加 query 校验）必须同步另一边，否则 "byte-for-byte" 承诺破裂。
- **未走 batch refactor（N+15）**：与 `get_edge_summary_basetool.go` 不同，这个工具本身就是 range query 语义（一次返回多行 log），不需要 `device_ids[]` batch 协议。
- **30s 超时**：与闭包路径一致，Loki 大查询可能不够。
- **无 query 白名单**：同闭包路径，依赖 Loki 侧限流。
- **响应原样透传**：同闭包路径，无 cap，LLM 上下文成本要注意。
- **`_ ...basetool.InvokeOption` 忽略 opts**：同其他 query 工具，不需要 tenant_bind / locale 等 ctx value。
