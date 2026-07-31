# query_traceql.go

## 1. 概述

本文件实现 `query_traceql` 工具的闭包路径，对 Tempo 跑 TraceQL search。用于按 service / operation / latency / status 找 traces，返回 trace summaries（id、service、root span name、duration、span count）。

是 metric/log/trace 三 signal 对称设计的 trace 那一个——当 metrics + logs 无法定位"哪个 request 慢"时用它。

**强制过滤**：至少一个 filter（query / service / operation / min_duration / max_duration）必填，未过滤的 Tempo 搜索太贵，不能误 dump 给 LLM。

## 2. 包信息

- **包名**：`tools`
- **路径**：`internal/manager\biz/aiops/tools/query_traceql.go`
- **导入**：
  - `tracequery`（`internal/pkg/tracequery`）—— `SearchOptions` / `SearchResult`
- **Class**：`read`
- **超时**：`queryTraceqlCallTimeout = 30 * time.Second`（与 promql/logql 对称）

## 3. 关键类型与接口

### `QueryTraceQLArgs`

```go
type QueryTraceQLArgs struct {
    Query       string `json:"query,omitempty"`       // TraceQL 表达式，可空（tag-mode 兜底）
    Service     string `json:"service,omitempty"`     // resource.service.name tag
    Operation   string `json:"operation,omitempty"`   // span name tag
    Start       string `json:"start,omitempty"`       // RFC3339，默认 now-1h
    End         string `json:"end,omitempty"`         // RFC3339，默认 now
    Limit       int    `json:"limit,omitempty"`       // 默认 50，cap 1000
    MinDuration string `json:"min_duration,omitempty"` // Go duration string，如 "100ms"
    MaxDuration string `json:"max_duration,omitempty"`
}
```

### `TraceQuerier`（接口）

```go
type TraceQuerier interface {
    SearchTraces(ctx context.Context, opts tracequery.SearchOptions) (*tracequery.SearchResult, error)
}
```

注释明示：这是 `r.traceQuery` 的类型，`*tracequery.Client` 满足。

## 4. 关键函数与流程

### `executeQueryTraceQL(ctx, args) (ExecuteResult, error)`

1. 校验 `r.traceQuery == nil` → error（防御性）。
2. Unmarshal `QueryTraceQLArgs`。
3. **强制过滤校验**：`Query` / `Service` / `Operation` / `MinDuration` / `MaxDuration` 全空 → error。注释："An unfiltered Tempo search is too expensive to dump on the LLM by accident"。
4. 时间解析：
   - `end = now`，`start = end.Add(-time.Hour)`（默认 1h 窗口）
   - `in.End` 非空 → `time.Parse(time.RFC3339)` 覆盖 end
   - `in.Start` 非空 → `time.Parse(time.RFC3339)` 覆盖 start
   - else if `in.End != ""` → `start = end.Add(-time.Hour)`（保持 1h 窗口相对 end）
5. `Limit ≤ 0 → 50`。
6. Duration 解析：`MinDuration` / `MaxDuration` 用 `time.ParseDuration`，失败带字段名报错。
7. tags 构造：
   - `Service` 非空 → `tags["service.name"] = Service`
   - `Operation` 非空 → `tags["name"] = Operation`
   - 注释明示："Tempo's SearchTraces ignores Tags when Query is set — that's the desired precedence. We forward both and let the client choose"——Query 优先，Tags 兜底。
   - `len(tags) == 0 → tags = nil`
8. `context.WithTimeout(ctx, 30s)`，调 `r.traceQuery.SearchTraces(callCtx, SearchOptions{Query, Tags, Limit, Start, End, MinDuration, MaxDuration})`。
9. Marshal `*SearchResult` 原样返回。

## 5. 依赖关系

| 方向 | 依赖 | 说明 |
|------|------|------|
| 上游 | `Registry`（闭包持有者） | `r.traceQuery` |
| 下游 | `TraceQuerier.SearchTraces` | 数据源 |
| 类型 | `tracequery.SearchOptions` / `SearchResult` | wire 协议 |

## 6. 并发与资源管理

- `context.WithTimeout(ctx, 30s)` per call——Tempo search 可能慢（大窗口 + 高 cardinality tag）。
- `Limit` cap 1000，防止 Tempo 返回海量 trace summaries 撑爆 LLM 上下文。
- 无共享状态，并发安全。

## 7. 设计模式与亮点

- **强制过滤**：至少一个 filter 必填，是三 signal 工具里唯一有此约束的（promql/logql 不强制）。Tempo unfiltered search 太贵，这个 guard 防止 LLM 误触发全量扫描。
- **Query 与 Tags 优先级**：注释明示 Tempo `SearchTraces` 在 Query 非空时忽略 Tags——这是"desired precedence"，工具层 forward 两者让 client 决定，避免工具层重复实现优先级逻辑。
- **三 signal 对称**：promql/logql/traceql 共享相同结构（args、30s 超时、时间解析、原样返回后端响应），但 traceql 多了强制过滤和 duration 解析——这是因为 trace 搜索的 cost profile 不同。
- **Go duration string**：`MinDuration` / `MaxDuration` 用 `"100ms"` / `"2s"` 这种 Go duration，对 LLM 友好（不必算毫秒数）。
- **`tags["name"] = Operation`**：Tempo 的 span name 用 `name` tag，不是 `operation`——这种命名映射在工具层硬编码，LLM 用 `operation` 字段更直观。
- **响应原样透传**：不裁剪、不重构，直接 Marshal `*SearchResult`——Tempo 的 trace summary 结构对 LLM 透明。

## 8. 注意事项

- **30s 超时**：Tempo 大窗口 + 高 cardinality tag 查询可能不够；超时后 LLM 可能重试，要注意 Tempo 侧查询取消。
- **`Limit` cap 1000**：trace summary 每条可能含多个 span，1000 traces 可能撑爆 LLM 上下文；LLM 应配合小 Limit（如 20）先看样本。
- **时间解析用 RFC3339**：与 logql 的 `parseLogQLTime`（支持 now-1h）不同，traceql 只接受 RFC3339——LLM 要算时间戳，相对不那么友好。
- **无 query 白名单**：TraceQL 是图灵完备的（{ ... } && { ... }），LLM 可能写复杂查询——依赖 Tempo 侧限流。
- **闭包路径与 BaseTool 路径并存**：见 `query_traceql_basetool.go`，两路径 byte-for-byte 等价，drift 风险同其他工具。
- **强制过滤是软 guard**：LLM 仍可能传 `service="."` 这种空值过滤绕过——Tempo 侧会返回空，但不会报错；工具层不校验语义。
- **`tags = nil` when empty**：避免传 `map[string]string{}` 给 Tempo（某些 client 版本会把空 map 当成"匹配无 tag"），nil 更安全。
