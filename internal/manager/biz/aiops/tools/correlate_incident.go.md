# `correlate_incident.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/correlate_incident.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件实现 `correlate_incident` 的闭包路径（Registry.executeCorrelateIncident）：拉一个 incident 周围所有信号——触发规则的 metric series、同时间窗口的 error logs、慢/错误 traces、edge 状态变化——返回一个 bundled JSON 让 LLM 一次推理无需多轮 tool call。关键红线：60s 总 timeout；100KB response cap（超限先裁 logs 再 traces 最后 metric values）；metric panel 仅闭集 metric 名（cpu_pct 等）可生成 expr；trace panel 需 service label。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 Registry.executeCorrelateIncident 调用；依赖 `alertbiz`、`alertmodel`、`devicemodel`、`logquery`、`promquery`、`tracequery`

## 3. 关键类型与接口

```go
const (
    ToolNameCorrelateIncident       = "correlate_incident"
    correlateIncidentTimeout        = 60 * time.Second
    correlateMaxResponseBytes       = 100 * 1024
)

type AlertUsecase interface {
    GetIncident(ctx, id uint64) (*alertmodel.Incident, error)
    ListIncidents(ctx, f alertbiz.IncidentFilter) ([]*alertmodel.Incident, error)
    ListEvents(ctx, incidentID uint64, limit int) ([]*alertmodel.Event, error)
    ListRules(ctx, scopeType string) ([]*alertmodel.Rule, error)
}

type correlateIncidentBundle struct {
    Incident    incidentSummary
    Window      windowRange
    MetricPanel []metricSeries
    LogPanel    []logEntry
    TracePanel  []traceEntry
    Edge        *edgeSnapshot
    Skipped     map[string]string
    Truncated   map[string]int
}

type CorrelateIncidentArgs struct {
    IncidentID    uint64
    WindowMinutes int
}
```

## 4. 关键函数与流程

### `executeCorrelateIncident`
- **签名**：`func (r *Registry) executeCorrelateIncident(ctx, args json.RawMessage) (ExecuteResult, error)`
- **职责**：组装 bundle
- **流程**：
  1. alertUC nil → error
  2. unmarshal args；IncidentID 0 → error
  3. window 钳位 [1, 240]，默认 30
  4. `context.WithTimeout(ctx, correlateIncidentTimeout)`
  5. `alertUC.GetIncident` 取 incident；nil → "not found"
  6. 算窗口 half = window/2 分钟，wStart/wEnd 围绕 FirstFiredAt
  7. 构 bundle skeleton（incidentSummary + window + 空 Skipped/Truncated map）
  8. **Metric panel**：promQuery 非 nil 且 `promExprForIncident(inc)` 命中 → queryMetricPanel；err → Skipped["metric_panel"]
  9. **Log panel**：logQuery 非 nil 且 DeviceID 非 nil → queryLogPanel；err → Skipped
  10. **Trace panel**：traceQuery 非 nil 且 labels/annotations["service"] 非空 → queryTracePanel
  11. **Edge snapshot**：DeviceID 非 nil 且 edges 非 nil → queryEdgeSnapshot
  12. Skipped 空 → 设 nil
  13. `marshalBundleWithCap(bundle)` 钳 100KB
  14. 返回 ExecuteResult{ResultJSON, DeviceID}

### `promExprForIncident`
- **签名**：`func promExprForIncident(inc *alertmodel.Incident) (string, bool)`
- **职责**：从 incident 推 PromQL range 表达式
- **流程**：
  1. Heuristic 1：rule_key 匹配闭集 metric 名（cpu_pct/mem_pct/...）→ `metricExprFor` 取 expr + `wrapPerEdge` 加 edge_id 过滤
  2. Heuristic 2：labels["metric"] / annotations["metric"] hint → 同上
  3. 都不命中 → false（log_match/trace_* 等 kind 无法生成 expr）

### `metricExprFor`
- 闭集 metric 名 → PromQL 映射：cpu_pct/mem_pct/disk_used_pct/disk_avail_bytes/load1/load5/load15/net_rx_bps/net_tx_bps
- 注释明示镜像 alert/evaluators_phaseA.metricExprFor，新增闭集 metric 需同步

### `wrapPerEdge`
- 用 `and on(edge_id) (group by (edge_id) ({edge_id="..."}))` 交集形式过滤单 edge；对 raw selector 和 `by (edge_id)` 聚合都适用

### `queryMetricPanel`
- **流程**：`stepFor(seconds)` 算步长；promQuery.QueryRange；unmarshal matrix；top 3 by magnitude（max abs value）

### `queryLogPanel`
- **流程**：Loki 查询 `{edge_id=".."} |~ "(?i)error|panic|oom|fatal|fail"`；Direction=backward；Limit 50；解析 ns timestamp；truncateLine 200；按 timestamp 倒序；cap 50

### `queryTracePanel`
- **流程**：Tempo SearchTraces tags={"service.name"} Limit 20；解析 traceID/rootServiceName/rootTraceName/durationMs/startTimeUnixNano；cap 20

### `queryEdgeSnapshot`
- **流程**：
  1. `context.WithTimeout(ctx, 5s)` edgeCtx；edges.Get → Name/Status/LastSeenAt；devices 非 nil → DecodeRoles
  2. promQuery 非 nil：3 个 probe（cpu_pct/mem_pct/up），每个 3s timeout，best-effort
  3. alertUC 非 nil：IIFE 内 5s timeout 查 ListIncidents（DeviceID filter, Limit 100），post-filter first_fired_at >= firedAt-24h 算 RecentIncidents24h

### `marshalBundleWithCap`
- **流程**：marshal bundle；<=100KB 直接返回；超限先裁 LogPanel 到 10，再裁 TracePanel 到 5，最后清 MetricPanel.Values；每次裁剪记 Truncated map

### 辅助函数
- `seriesMagnitude`：算 series max abs value 用于 top-3 排序
- `parseLokiNanoTimestamp`：ns string → time.Time
- `truncateLine`：cap 行长 + 省略号
- `singleVectorValue`：取 instant vector 首个 sample float

## 5. 依赖关系

- **内部包**：`alertbiz`、`alertmodel`、`devicemodel`、`logquery`、`promquery`、`tracequery`
- **外部库**：标准库 `context`、`encoding/json`、`errors`、`fmt`、`log/slog`、`math`、`sort`、`strconv`、`strings`、`time`
- **被调用方**：Registry（闭包路径）；basetool 路径在 correlate_incident_basetool.go

## 6. 并发与资源管理

- 60s 总 timeout（correlateIncidentTimeout）
- edge snapshot 内部用独立 5s/3s timeout 隔离慢 probe，best-effort 不阻塞 bundle
- IIFE 隔离 WithTimeout cancel 时机，避免 defer 累积到函数尾
- 无锁、无共享可变状态

## 7. 设计模式与亮点

- **bundle 一次返回**：metric+log+trace+edge 一次给齐，避免 LLM 10 轮 tool call
- **闭集 metric 名映射**：rule_key 匹配 cpu_pct 等自动生成 PromQL；label/annotation metric hint 兜底
- **wrapPerEdge 交集形式**：`and on(edge_id)` 对 raw selector 和 aggregation 都适用，caller 不需知 expr 形状
- **top-3 by magnitude**：metric series 按最大绝对值排序取 top 3，去掉噪声
- **log backward direction**：最新优先，符合运维滚动习惯
- **trace 需 service label**：无 service 无法查 Tempo
- **edge snapshot best-effort**：每个 probe 独立 timeout，慢 probe 不阻塞 bundle
- **100KB response cap**：超限分级裁剪（logs → traces → metric values），记 Truncated 让 LLM 知道
- **Skipped map**：每个 panel 跳过原因（client 未配置 / 无 device_id / 查询失败）记入 Skipped

## 8. 注意事项

- **window_minutes 钳位 [1,240]**：默认 30
- **metricExprFor 镜像**：新增闭集 metric 需同步 alert/evaluators_phaseA.metricExprFor
- **incident 无 DeviceID**：log panel / edge snapshot 跳过（Skipped["log_panel"]="incident has no edge_id"）
- **trace 需 service label**：labels/annotations 都无 service 则跳过
- **Tempo 响应解析 best-effort**：unmarshal 失败返回 nil 而非 error（不同 Tempo 版本 shape 不同）
- **RecentIncidents24h post-filter**：biz IncidentFilter 无时间 bound，Limit 100 后客户端 filter
- **closure 与 basetool 共存**：闭包路径 executeCorrelateIncident 不变；basetool 路径在 correlate_incident_basetool.go（fan-out 多 incident）
