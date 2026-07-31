# `evaluators_phaseA.go` 技术实现文档

> 源文件：`internal/manager/biz/alert/evaluators_phaseA.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/alert`

## 1. 概述

本文件是 Phase-A evaluator 集合：metric_anomaly / metric_forecast / metric_burn_rate 三类规则的运行时实现。所有 evaluator 都把规则编译成 PromQL 表达式，调 `PromQuerier.Query` 即时查询，按返回 vector 的每个 label-set 触发一个独立 incident（dedupe key = `pipeline:<rule_key>:<labelSetKey>`）。同时提供共享辅助：`observeEval`（per-rule 延迟计时器）、`metricExprFor`（closed-set metric 名 → PromQL 表达式映射）、PromQL selector 合并工具、`runVectorRule` helper、`windowedSLI`（burn-rate SLI 模板替换）。burn_rate 实现遵循 Google SRE Workbook 多窗口多 burn-rate 模式：所有窗口必须 AND 触发。

## 2. 包信息

- **包名**：`alert`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 `pipeline.go::evaluate` 调用；依赖 `internal/manager/model/alert` + `internal/pkg/notify` + `internal/pkg/prom` + `encoding/json` + `regexp`

## 3. 关键类型与接口

```go
// vectorRule 把 metric_anomaly / metric_forecast 等 vector-style 规则的共享字段打包。
type vectorRule struct {
    ruleKey       string
    ruleName      string
    severity      string
    scopeType     string // host / global / monitoring_pipeline
    runbook       string
    labels        map[string]string
    expr          string
    fmtSummary    func(labels map[string]string, value float64) string
    resolveReason string
}

var metricExprNodeSelectorRE = regexp.MustCompile(`\b(node_[a-zA-Z0-9_:]+)(\{[^}]*\})?`)
```

`PipelineEvaluator` 结构体本身在 `pipeline.go` 声明；本文件是它的方法集合。`PromQuerier` 接口也在 `pipeline.go` 声明。

## 4. 关键函数与流程

### `observeEval`

- **签名**：`func observeEval(kind string, evalErr *error) func()`
- **职责**：per-rule 延迟计时器，返回闭包，应在每条规则迭代顶部 defer 调用
- **流程**：
  1. 记录 `start := time.Now()`
  2. 返回闭包：取 `*evalErr`（by reference，让循环体后填的错误也能上 histogram），调 `prom.ObserveAlertEvaluator(kind, elapsed, err)`
- **错误处理**：闭包内 nil err → result=success 上报

### `metricExprFor`

- **签名**：`func metricExprFor(metric string) (string, bool)`
- **职责**：closed-set canonical metric 名 → PromQL 表达式映射
- **支持**：`cpu_pct` / `mem_pct` / `disk_used_pct` / `disk_avail_bytes` / `load1` / `load5` / `load15` / `net_rx_bps` / `net_tx_bps`
- **流程**：switch 返回对应 PromQL（如 `cpu_pct` → `100 * (1 - avg by (device_id) (rate(node_cpu_seconds_total{mode="idle"}[5m])))`）；未匹配返回 `("", false)`
- **错误处理**：调用方应在编译期拒绝，但 evaluator 双重检查运行时也拒绝

### `applyClosedSetMetricSelector`

- **签名**：`func applyClosedSetMetricSelector(expr, selector string) string`
- **职责**：把用户附加的 selector（如 `device_id="123"`）合并到 closed-set 表达式里的每个 `node_*` metric 上
- **流程**：
  1. `normalizePromSelectorFragment(selector)` 去花括号
  2. 用 `metricExprNodeSelectorRE` 匹配表达式中所有 `node_xxx{...}` 片段
  3. 对每个匹配，调 `mergePromSelectorFragments(existing, add)` 合并 selector（add 优先级高，覆盖同名 matcher）
- **错误处理**：selector 为空时原样返回 expr

### `normalizePromSelectorFragment` / `splitPromSelectorFragment` / `mergePromSelectorFragments` / `promMatcherKey`

- **职责**：PromQL selector 片段的解析、合并工具
- `normalizePromSelectorFragment`：去首尾花括号 + TrimSpace
- `splitPromSelectorFragment`：按 `,` 切分 matcher 列表
- `mergePromSelectorFragments(existing, add)`：合并两个 selector；`add` 中同名 key 覆盖 `existing`，其余保留
- `promMatcherKey(matcher)`：从 `key="value"` / `key=~"regex"` 提取 `key`，用于覆盖判定
- **流程**：用 `addKeys` map 跟踪 add 的 key，遍历 existing 跳过被覆盖的，再追加 add 全部
- **错误处理**：空 selector 合并返回 `""` 或 `"{}"`

### `evaluateMetricAnomaly`

- **签名**：`func (e *PipelineEvaluator) evaluateMetricAnomaly(ctx context.Context, now time.Time)`
- **职责**：把每个 metric_anomaly 规则编译成 PromQL 异常检测表达式并查询
- **流程**：
  1. `rules := e.rules.MetricAnomalyRules()`；空则返回
  2. per-rule：`observeEval` 启动计时器
  3. `metricExprFor(rule.Metric)` 取 closed-set 表达式；未匹配 → Warn + continue
  4. `applyClosedSetMetricSelector(base, rule.Selector)` 合并用户 selector
  5. 按 `rule.Method` 渲染 PromQL：
     - `mad`：`abs((base) - (quantile_over_time(0.5, (base)[bw:bs]))) > dev * (avg_over_time((abs((base) - (median)))[bw:bs]))`（用 `quantile_over_time(0.5)` 近似中位数 + MAD）
     - 默认 `zscore`：`abs((base) - (avg_over_time((base)[bw:bs]))) > dev * (stddev_over_time((base)[bw:bs]))`
  6. 调 `runVectorRule` 执行
  7. `done()` 关闭计时器
- **错误处理**：metric 不在 closed-set → Warn + 跳过该规则

### `evaluateMetricForecast`

- **签名**：`func (e *PipelineEvaluator) evaluateMetricForecast(ctx context.Context, now time.Time)`
- **职责**：渲染 `predict_linear((expr)[fit:step], predict_seconds) <op> <threshold>` 并查询
- **流程**：
  1. 取 rules；per-rule 计时
  2. `metricExprFor` + `applyClosedSetMetricSelector`
  3. **固定 5m 子查询 step**：注释解释"fit window 仍控制斜率历史，step 仅影响采样分辨率"
  4. 表达式：`predict_linear((base)[fit:5m], predict_seconds) op threshold`
  5. `runVectorRule` 执行
- **错误处理**：同上

### `evaluateMetricBurnRate`

- **签名**：`func (e *PipelineEvaluator) evaluateMetricBurnRate(ctx context.Context, now time.Time)`
- **职责**：SRE Workbook 多窗口多 burn-rate 实现
- **流程**：
  1. 取 rules；per-rule 计时
  2. `budget := func(slo) float64 { return 1 - slo/100 }` 计算 error budget
  3. per-rule：`fired := true`、`firstNonFiringReason`、`maxBurn` 跟踪
  4. **遍历所有 Burns 窗口（AND 语义）**：
     - 表达式：`(1 - (windowedSLI(rule.SLI, b.Window))) >= b.Multiplier * budget(rule.SLO)`
     - `e.prom.Query` 查询；err → `fired=false` + `firstNonFiringReason="burn_rate query failed"` + `evalErr=err` + break
     - 非 vector 或空 → `fired=false` + reason + break
     - 取首个 entry 的 value 跟踪 `maxBurn`（用于 firing summary）
  5. **dedupe key = `pipeline:<rule_key>`**（per-rule，无 label 后缀——burn-rate 规则通常一个 SLO 一条）
  6. fired → `RecordFiring` + `notify`（severity 走 `ruleSev(rule.Severity, SeverityCritical)` 默认 critical）
  7. 未 fired → `SystemResolveIncident(dedupeKey, "burn_rate cleared: "+reason, now)` 自动恢复
- **错误处理**：单个窗口查询失败 → 整条规则本 tick 不触发；`RecordFiring` 失败 Warn + continue；`SystemResolveIncident` 失败 Warn

### `runVectorRule`

- **签名**：`func (e *PipelineEvaluator) runVectorRule(ctx context.Context, vr vectorRule, now time.Time) error`
- **职责**：metric_anomaly / metric_forecast 共享的执行 helper
- **流程**：
  1. `e.prom.Query(ctx, vr.expr, now)`；err → Warn + return err
  2. 非 vector → return nil
  3. json.Unmarshal 到 `[]vectorEntry{Metric, Value}`
  4. per-entry：
     - `promFirstNumeric(ent.Value)` 取数值
     - `dedupeKey := fmt.Sprintf("pipeline:%s:%s", vr.ruleKey, labelSetKey(ent.Metric))`（per-label-set 独立 incident）
     - `vr.fmtSummary(ent.Metric, v)` 渲染 summary
     - host scope 时从 `ent.Metric["device_id"]` 解析 devID
     - `RecordFiring` + `notify`
  5. **不自动 resolve**：注释明示"PR-A 不自动 resolve vector-rule incidents；anomaly/forecast 查询不会干净地发出 per-series 'value cleared' 信号；操作员手动 ack/resolve；PR-A2 将加 'did fire last tick?' sweep"
  6. `_ = seen`（保留 seen map 给未来 PR-A2 用）
- **错误处理**：`RecordFiring` 失败 Warn + continue 下个 entry

### `promFirstNumeric`

- **签名**：`func promFirstNumeric(value []json.RawMessage) (float64, bool)`
- **职责**：从 Prom vector entry 的 `[<unix_ts>, "<float>"]` 取 float
- **流程**：Unmarshal 第二元素到 string，再 `parseFloat`

### `windowedSLI`

- **签名**：`func windowedSLI(sli, window string) string`
- **职责**：把 SLI 模板套上具体窗口
- **流程**：
  1. 含 `$window` → `strings.ReplaceAll(sli, "$window", window)`
  2. 否则 → `sli + "[" + window + "]"`（fallback，对 raw counter 有效，但会困惑用户——UI 应提示 `$window` 约定）
- **错误处理**：无

## 5. 依赖关系

- **内部包**：`internal/manager/model/alert`（`RuleKind*` 常量、`RuleScopeHost`）、`internal/pkg/notify`（`SeverityCritical` / `SeverityWarning`）、`internal/pkg/prom`（`ObserveAlertEvaluator`）
- **外部库**：`encoding/json` / `fmt` / `log/slog` / `regexp` / `strconv` / `strings` / `time`
- **被调用方**：`pipeline.go::evaluate`（按顺序：metricAnomaly → metricForecast → metricBurnRate）
- **依赖**：`pipeline.go` 的 `PipelineEvaluator` / `PromQuerier` / `e.uc.RecordFiring` / `e.uc.SystemResolveIncident` / `e.notify` / `labelSetKey` / `mergeLabels` / `ruleSev` / `effectiveScope`

## 6. 并发与资源管理

- `evaluate*` 由 `PipelineEvaluator.Loop` 单 goroutine 驱动（ticker），无内部并发
- `EvaluateOnce` 暴露给测试，但测试典型串行调用
- `observeEval` 闭包捕获 `*error` by reference，让循环体后填的错误也能上 histogram——defer 语义保证闭包在循环体返回后才执行
- 每次 `e.prom.Query` 走 ctx，由调用方控制超时

## 7. 设计模式与亮点

- **closed-set metric 抽象**：用户在 UI 选 `cpu_pct`，evaluator 翻译为完整 PromQL（含 `avg by (device_id)`）；既保证 query 质量又限制用户输入面
- **selector 合并工具**：用户可附加 `device_id="X"` 缩窄 closed-set 表达式；`mergePromSelectorFragments` 处理覆盖语义（同名 key 用户优先）
- **MAD 近似**：PromQL 没有原生中位数/MAD，用 `quantile_over_time(0.5)` 近似中位数 + `avg_over_time(abs(x - median))` 近似 MAD；注释解释设计取舍
- **predict_linear 固定 5m step**：fit window 控制斜率历史，step 仅影响采样分辨率；统一 5m 简化用户输入
- **SRE Workbook AND 语义**：burn_rate 所有窗口必须同时触发——这是多 burn 模式的核心，过滤短暂 blip 同时对真实故障快速响应
- **per-rule dedupe（burn_rate）vs per-label-set dedupe（anomaly/forecast）**：burn_rate 通常一个 SLO 一条规则，per-rule dedupe 合理；anomaly/forecast 按 label-set（如不同 device）独立 incident
- **maxBurn 跟踪**：burn_rate 触发时把多窗口中最大的 burn 值写入 incident.Value，操作员能看到"burn 到 14.4×"
- **不自动 resolve（anomaly/forecast）**：诚实标注 PR-A 限制——这些查询不会干净发出恢复信号，留给 PR-A2 用 sweep 模式（与 evaluatePromQuery 的 firingSnapshot 模式相同）
- **observeEval 的 *error 捕获**：让循环体在 `evalErr = err` 后，defer 的闭包仍能把错误上 histogram——result=error 与 WARN log 对齐

## 8. 注意事项

- **closed-set 限制**：仅 9 个 metric 名；用户写 `cpu_usage` 之类会被 `metricExprFor` 拒绝（编译期 + 运行时双检查）
- **MAD 是近似**：`quantile_over_time(0.5)` 不是真中位数（PromQL 限制），是对 P50 的近似；对极端分布可能偏差
- **predict_linear step 固定 5m**：用户写的 `fit_window` 控制 fit 历史长度，但 step 不接受用户配置
- **burn_rate 自动 resolve**：未 fired 时自动 `SystemResolveIncident`——与 anomaly/forecast 不同，burn_rate 的 AND 语义让"未 fired"等价于"恢复"，可以安全 resolve
- **burn_rate severity 默认 critical**：`ruleSev(rule.Severity, SeverityCritical)`——burn_rate 是 SLO 级告警，默认 critical 合理
- **`runVectorRule` 的 `_ = seen`**：保留 seen map 给 PR-A2，当前不自动 resolve；操作员需手动 ack/resolve
- **selector 合并的 key 覆盖**：用户 selector 中的 `device_id` 会覆盖 closed-set 表达式里已有的；这是有意为之，让用户能缩窄到单设备
- **`windowedSLI` 的 fallback**：SLI 不含 `$window` 时追加 `[window]`——对 raw counter 有效，但对 `sum(rate(...))` 形式会出错；UI 必须引导用户使用 `$window`
