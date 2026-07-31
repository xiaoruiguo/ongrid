# `evaluators_phaseB.go` 技术实现文档

> 源文件：`internal/manager/biz/alert/evaluators_phaseB.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/alert`

## 1. 概述

本文件是 Phase-B evaluator 集合：log_match / log_volume（查 Loki）、trace_latency / trace_error_rate（查 Prom spanmetrics）。四个 evaluator 共享 metric_raw 的 recovery 模式：跟踪每 tick per-rule 触发的 dedupe key 集合，本 tick 缺失的 key 触发 `SystemResolveIncident`。`firingSnapshot[ruleKey]` 是 per-rule 的"上 tick fired set"，与 `evaluatePromQuery` 共享同一 map（rule_key 跨 kind 唯一，不会冲突）。文件还提供共享 helper：`sweepRecovery`（恢复扫描）、`runPromInstant` / `runLokiInstant`（即时查询封装）、`deviceIDFromLabels`、`buildLogMatchQuery`（LogQL 拼接）。

## 2. 包信息

- **包名**：`alert`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 `pipeline.go::evaluate` 调用；依赖 `internal/manager/model/alert` + `internal/pkg/logquery` + `internal/pkg/notify`

## 3. 关键类型与接口

```go
// vectorEntry 是 promquery / loki matrix 单 series 的本地解释：
// 一个 label set + 一个数值（matrix 最新样本或 instant vector 唯一样本）。
type vectorEntry struct {
    Labels map[string]string
    Value  float64
}
```

`LogQuerier` 接口在 `pipeline.go` 声明；`PipelineEvaluator.logq` 字段持有 Loki 客户端。

## 4. 关键函数与流程

### `evaluateLogMatch`

- **签名**：`func (e *PipelineEvaluator) evaluateLogMatch(ctx context.Context, now time.Time)`
- **职责**：每个 log_match 规则的 `count_over_time` Loki 查询，对满足 `operator+threshold` 的 label-set 触发 incident
- **流程**：
  1. `rules := e.rules.LogMatchRules()`；空返回
  2. per-rule：`observeEval` 启动计时
  3. `expr := buildLogMatchQuery(rule.StreamSelector, rule.LineFilter, rule.Window)`
  4. `entries, err := runLokiInstant(ctx, e.logq, expr, now)`；err → Warn + `evalErr=err` + continue
  5. `scope := effectiveScope(rule.ScopeType, RuleKindLogMatch)`
  6. `fired := make(map[string]struct{}, len(entries))` 跟踪本 tick 触发的 dedupe key
  7. per-entry：
     - `compareFloat(ent.Value, rule.Operator, rule.Threshold)` 不满足 → 跳过
     - `dedupeKey := fmt.Sprintf("pipeline:%s:%s", rule.RuleKey, labelSetKey(ent.Labels))`
     - `fired[dedupeKey] = struct{}{}`
     - 渲染 summary
     - host scope 时 `devID := deviceIDFromLabels(ent.Labels)`
     - `RecordFiring` + `notify`
  8. `e.sweepRecovery(ctx, rule.RuleKey, fired, "log_match condition cleared", now)`
- **错误处理**：Loki 查询失败 → Warn + continue；`RecordFiring` 失败 Warn + continue 下个 entry

### `evaluateLogVolume`

- **签名**：`func (e *PipelineEvaluator) evaluateLogVolume(ctx context.Context, now time.Time)`
- **职责**：v1 实现：与 log_match 同 shape（current-window count vs 绝对阈值）
- **流程**：与 `evaluateLogMatch` 几乎完全相同；唯一差异：summary 文案 + resolveReason `"log_volume condition cleared"`
- **注释**：原始 spec 的"vs previous window ratio"语义留给未来；当前 shape 已覆盖"日志量超 N"的常见诉求；per-kind rule type 保留 schema gate 以便后续收紧语义而不破坏 UI
- **错误处理**：同上

### `evaluateTraceLatency`

- **签名**：`func (e *PipelineEvaluator) evaluateTraceLatency(ctx context.Context, now time.Time)`
- **职责**：每个 trace_latency 规则的预编译 `histogram_quantile() > threshold_ms` Prom 查询
- **流程**：
  1. 取 rules；per-rule 计时
  2. `entries, err := runPromInstant(ctx, e.prom, rule.Expr, now)`；err → Warn + continue
  3. scope = effectiveScope
  4. per-entry：
     - `dedupeKey := fmt.Sprintf("pipeline:%s:%s", rule.RuleKey, labelSetKey(ent.Labels))`
     - summary：`<rule_key>: <service> <quantile> 延迟 %.1fms > %gms`
     - `RecordFiring` + `notify`（不传 DeviceID——trace 是 service 聚合）
  5. `sweepRecovery(ctx, rule.RuleKey, fired, "trace_latency condition cleared", now)`
- **注释**：rule.Expr 含 `> threshold_ms` 比较，让 Prom 只返回 breaching series——与 evaluatePromQuery 同模式
- **错误处理**：同上

### `evaluateTraceErrorRate`

- **签名**：`func (e *PipelineEvaluator) evaluateTraceErrorRate(ctx context.Context, now time.Time)`
- **职责**：trace_error_rate 对称实现
- **流程**：与 `evaluateTraceLatency` 同 shape；summary：`<rule_key>: <service> 错误率 %.2f%% <op> %g%%`
- **错误处理**：同上

### `sweepRecovery`

- **签名**：`func (e *PipelineEvaluator) sweepRecovery(ctx context.Context, ruleKey string, fired map[string]struct{}, reason string, now time.Time)`
- **职责**：resolve 上 tick 触发但本 tick 缺失的 dedupe key 对应 incident
- **流程**：
  1. `e.firingSnapshot` nil → 初始化
  2. `prev := e.firingSnapshot[ruleKey]`
  3. per-prevKey：若仍在本 tick `fired` 中 → 跳过；否则 `SystemResolveIncident(ctx, prevKey, reason, now)`
  4. `e.firingSnapshot[ruleKey] = fired`（用本 tick 集合覆盖）
- **错误处理**：`SystemResolveIncident` 失败 Warn（不阻断循环）

### `runPromInstant`

- **签名**：`func runPromInstant(ctx context.Context, p PromQuerier, expr string, now time.Time) ([]vectorEntry, error)`
- **职责**：在 `now` 跑 Prom 即时查询，解码 vector 到 `[]vectorEntry`
- **流程**：
  1. `p == nil` → 返回 nil, nil
  2. `p.Query(ctx, expr, now)`；err → return
  3. 非 vector → 返回 nil, nil
  4. json.Unmarshal 到 `[]promEntry{Metric, Value}`
  5. per-entry：从 `Value[1]` Unmarshal 到 string，`strconv.ParseFloat` → 数值
  6. 返回 `[]vectorEntry`
- **错误处理**：解码失败 → `fmt.Errorf("decode prom vector: %w", err)`

### `runLokiInstant`

- **签名**：`func runLokiInstant(ctx context.Context, l LogQuerier, expr string, now time.Time) ([]vectorEntry, error)`
- **职责**：用 `QueryRange` 跑 `[now-60s, now]` 窗口、30s step 的紧窗口查询，取每个 matrix series 的最新样本——LogQL 里"as of now 评估 count_over_time"的最接近近似
- **流程**：
  1. `l == nil` → 返回 nil, nil
  2. `l.QueryRange(ctx, QueryRangeOptions{Query: expr, Start: now.Add(-60s), End: now, Step: 30s, Limit: 1000})`
  3. err → return
  4. 非 matrix → 返回 nil, nil
  5. json.Unmarshal 到 `[]lokiEntry{Metric, Values}`
  6. per-series：取 `Values[len-1]`（最新样本），Unmarshal 第二元素到 string，ParseFloat → 数值
  7. 返回 `[]vectorEntry`
- **错误处理**：解码失败 → `fmt.Errorf("decode loki matrix: %w", err)`

### `deviceIDFromLabels`

- **签名**：`func deviceIDFromLabels(labels map[string]string) *uint64`
- **职责**：从 labels 的 `device_id` 解析 uint64
- **流程**：TrimSpace；空 → nil；`ParseUint`；err 或 0 → nil；否则 `&id`
- **错误处理**：解析失败返回 nil（让 `validateFiring` 拒绝 host-scope 无 device_id）

### `buildLogMatchQuery`

- **签名**：`func buildLogMatchQuery(stream, filter, window string) string`
- **职责**：拼接 log_match / log_volume 的 LogQL
- **流程**：
  1. `window == ""` → 默认 `"5m"`
  2. `filter == ""` → `count_over_time(<stream> [<window>])`
  3. 否则 → `count_over_time(<stream> |~ <filter-quoted> [<window>])`（regex 匹配）
- **错误处理**：无

## 5. 依赖关系

- **内部包**：`internal/manager/model/alert`（`RuleKind*`、`RuleScopeHost`）、`internal/pkg/logquery`（`QueryRangeOptions` / `QueryRangeResult`）、`internal/pkg/notify`（`SeverityWarning`）
- **外部库**：`encoding/json` / `fmt` / `log/slog` / `strconv` / `strings` / `time`
- **被调用方**：`pipeline.go::evaluate`（在 `if e.logq != nil` / `if e.prom != nil` gate 内调用）
- **依赖**：`pipeline.go` 的 `PipelineEvaluator` / `e.rules` / `e.logq` / `e.prom` / `e.uc` / `e.notify` / `e.firingSnapshot` / `effectiveScope` / `labelSetKey` / `mergeLabels` / `ruleSev` / `compareFloat` / `observeEval`

## 6. 并发与资源管理

- `evaluate*` 由 `PipelineEvaluator.Loop` 单 goroutine 驱动
- `firingSnapshot` 在 `evaluatePromQuery` 与本文件四个 evaluator 间共享——但 rule_key 跨 kind 唯一，不会冲突
- `firingSnapshot` 无额外 mutex（注释明示 Loop 单 goroutine，测试 `EvaluateOnce` 也是）
- `runLokiInstant` / `runPromInstant` 走 ctx，由调用方控制超时

## 7. 设计模式与亮点

- **统一 recovery 模式**：四个 evaluator 都用 `sweepRecovery`——上 tick 触发但本 tick 缺失的 dedupe key 自动 resolve。与 `evaluatePromQuery` 共享 `firingSnapshot` map，所有 kind 走同一恢复机制
- **LogQL 即时查询的紧窗口近似**：LogQL 没有真正的 instant query，用 `[now-60s, now]` 30s step 的 QueryRange + 取最新样本来近似"as of now"——足够接近实时，避免 full range 查询的开销
- **trace 走 Prom 不走 Tempo**：spanmetrics generator 把 Tempo spans 刮到 Prom 的 `traces_spanmetrics_*`，evaluator 查 Prom 享受 Prom 的缓存 + 性能，且 `> threshold` 让 Prom 过滤到只 breaching series
- **`rule.Expr` 预编译**：trace_latency / trace_error_rate 的 `Expr` 在 `rules.go::compileTraceLatencyRule` 编译期已构造好，evaluator 直接查询——避免运行时拼字符串出错
- **log_volume 与 log_match 同 shape**：v1 故意复用 log_match shape；保留 per-kind rule type 作为 schema gate，将来收紧"vs previous window ratio"语义时不破坏 UI
- **per-label-set dedupe**：所有四个 kind 的 dedupe key 都是 `pipeline:<rule_key>:<labelSetKey>`——同规则不同 service/device 独立 incident
- **`buildLogMatchQuery` 的 regex 转义**：用 `%q` 把 filter 包成 PromQL 字符串字面量，自动处理双引号转义

## 8. 注意事项

- **`firingSnapshot` 跨 kind 共享**：依赖 rule_key 跨 kind 唯一；如果将来允许同 rule_key 跨 kind（不建议），需要改 per-kind 维度
- **Loki 紧窗口 60s/30s step**：是"as of now"的近似；对窗口 ≥5m 的 `count_over_time` 影响很小，但对极端短窗口（如 30s）可能丢失样本
- **`runLokiInstant` 的 Limit=1000**：单查询最多 1000 series；超大 cardinality stream 可能被截断，导致部分 label-set 不触发 incident
- **log_volume v1 是绝对阈值**：不是"vs previous window ratio"；UI 文案需明确，避免操作员误解
- **trace_* 不传 DeviceID**：trace 是 service 聚合，没有 device 概念；`FiringInput.DeviceID` 留空，scope=global
- **`sweepRecovery` 用 `e.firingSnapshot[ruleKey] = fired`**：直接覆盖（不是 merge）——本 tick 的 fired 集合就是下 tick 的 prev；map 复用避免分配
- **`_ = evalErr` 在每个 evaluator 末尾**：`observeEval` 通过 `*error` 引用捕获，无需显式传递；显式 `_ = evalErr` 是为了 go vet 静默未使用变量警告
- **`deviceIDFromLabels` 严格解析**：`device_id` 必须是正整数；空、0、非数字都返回 nil——让 `validateFiring` 拒绝 host-scope 无 device_id，避免脏数据
