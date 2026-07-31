# query_logql.go

## 1. 概述

本文件实现 `query_logql` 工具的闭包路径，对 Loki 跑 LogQL range query。用于调查 log patterns、error counts、per-edge 过滤等——raw log 内容查询，返回 Loki 原始响应（streams 或 matrix）。

是 `query_promql` 的 log 对应物：metric 走 promql，log 走 logql，trace 走 traceql——三种 signal 各有专门工具。

## 2. 包信息

- **包名**：`tools`
- **路径**：`internal/manager/biz/aiops/tools/query_logql.go`
- **导入**：
  - `logquery`（`internal/pkg/logquery`）—— `QueryRangeOptions` / `QueryRangeResult`
- **Class**：`read`
- **超时**：`queryLogqlCallTimeout = 30 * time.Second`（与 `query_promql` 对称）

## 3. 关键类型与接口

### `QueryLogQLArgs`

```go
type QueryLogQLArgs struct {
    Query     string `json:"query"`           // LogQL 表达式
    Start     string `json:"start,omitempty"`  // RFC3339 或 now/now-1h/now+1h
    End       string `json:"end,omitempty"`
    Limit     int    `json:"limit,omitempty"`  // 默认 200，cap 5000
    Direction string `json:"direction,omitempty"` // backward（默认）/forward
}
```

### `LogQuerier`（接口）

```go
type LogQuerier interface {
    QueryRange(ctx context.Context, opts logquery.QueryRangeOptions) (*logquery.QueryRangeResult, error)
}
```

注释明示：这是 `r.logQuery` 的类型，`*logquery.Client` 满足。声明在 query_logql.go 里是为了让测试能 inject fake。

## 4. 关键函数与流程

### `parseLogQLTime(value, fallback) (time.Time, error)`

时间解析 helper，支持多种格式：

- 空 / "now"（大小写不敏感）→ `fallback`
- "now-1h" / "now-30m" → `time.Now().Add(-d)`
- "now+1h" → `time.Now().Add(d)`
- 其他 → `time.Parse(time.RFC3339, value)`

这是 LogQL/Loki 风格的时间语法，让 LLM 不必每次算 RFC3339 时间戳。

### `executeQueryLogQL(ctx, args) (ExecuteResult, error)`

1. 校验 `r.logQuery == nil` → error（防御性，正常不会发生因为 NewRegistry 时 tool 不会注册）。
2. Unmarshal `QueryLogQLArgs`。
3. `Query` trim 后空 → error。
4. 时间解析：
   - `end = time.Now()`，`start = end.Add(-time.Hour)`（默认 1h 窗口）
   - `in.End != ""` → `parseLogQLTime(in.End, end)` 覆盖 end
   - `in.Start != ""` → `parseLogQLTime(in.Start, start)` 覆盖 start
   - **else if `in.End != ""`**：用户只传 end 不传 start，`start = end.Add(-time.Hour)` 保持 1h 窗口相对 end
5. `Limit ≤ 0 → 200`。
6. `Direction` 空 → "backward"（newest first）。
7. `context.WithTimeout(ctx, 30s)`，调 `r.logQuery.QueryRange(callCtx, QueryRangeOptions{Query, Start, End, Limit, Direction})`。
8. Marshal `*QueryRangeResult` 原样返回（Loki 响应直传）。

## 5. 依赖关系

| 方向 | 依赖 | 说明 |
|------|------|------|
| 上游 | `Registry`（闭包持有者） | `r.logQuery` |
| 下游 | `LogQuerier.QueryRange` | 数据源 |
| 类型 | `logquery.QueryRangeOptions` / `QueryRangeResult` | wire 协议 |

## 6. 并发与资源管理

- `context.WithTimeout(ctx, 30s)` per call——Loki range query 可能慢（大窗口 + 高 cardinality label）。
- `Limit` cap 5000，防止 Loki 返回海量 log 行撑爆 LLM 上下文。
- 无共享状态，并发安全。

## 7. 设计模式与亮点

- **三 signal 对称设计**：promql/logql/traceql 三个工具共享相同的结构（args、timeout 30s、`parseTime` helper、原样返回后端响应），降低 LLM 学习成本——会一个就会另两个。
- **`parseLogQLTime` 多格式时间解析**：支持 RFC3339 + now/now-1h/now+1h 三类格式，前者给精确时间，后者给相对时间（LLM 常用"过去 1 小时"这种语义）。`CutPrefix` + `ToLower` 实现 case-insensitive。
- **"用户只传 end 不传 start" 特殊处理**：`else if in.End != ""` 分支保持 1h 窗口相对 end——避免用户传 end=start+2h 但忘传 start 时窗口变成默认 1h 相对 now（与 end 不连续）。
- **`Direction` 默认 backward**：newest first，符合 log 排查"先看最新错误"的直觉。
- **响应原样透传**：不裁剪、不重构，直接 Marshal `*QueryRangeResult`——Loki 的 streams/matrix 结构对 LLM 是透明的，工具层不引入信息损失。
- **`LogQuerier` 接口声明在闭包文件**：注释明示"declared here so tests can inject a fake"——即使 BaseTool 形态（`query_logql_basetool.go`）复用此接口，声明位置仍在闭包文件，是历史遗留。

## 8. 注意事项

- **30s 超时**：相对长，但 Loki 大窗口 + 高 cardinality 查询可能仍不够；超时后 LLM 可能重试，要注意 Loki 侧的查询取消。
- **`Limit` cap 5000**：单次 5000 行 log 可能撑爆 LLM 上下文（每行几百字节就是 MB 级），LLM 应配合 `Direction=backward` + 小 Limit（如 100）先看样本。
- **`parseLogQLTime` 不校验 start < end**：用户传 `start=now-1h, end=now-2h` 时 Loki 会返回空或 error，工具层不拦——依赖 Loki 自身校验。
- **闭包路径与 BaseTool 路径并存**：见 `query_logql_basetool.go`，两路径 byte-for-byte 等价（注释明示 "same args, same timeouts, same output bytes"），drift 风险同其他工具。
- **无 query 白名单校验**：LogQL 是图灵完备的（pipe + line_format + label_format），LLM 可能写危险查询（如 `{__name__=""}` 全量扫描）——依赖 Loki 侧的 query frontend 限流。
- **`*QueryRangeResult` 原样 Marshal**：Loki 响应可能很大（streams 多行 log），工具层不做 cap，LLM 上下文成本要注意。
- **`LogQuerier` 接口在闭包文件声明**：BaseTool 形态也用这个接口，但声明在 `query_logql.go`——重构时要注意不要破坏 BaseTool 路径的 import。
