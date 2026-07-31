# `rules.go` 技术实现文档

> 源文件：`internal/manager/biz/alert/rules.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/alert`

## 1. 概述

本文件是 alert 子域的规则编译层：把 DB 行 `model.Rule` 编译成 evaluator 用的 runtime 视图（`MetricRawRule` / `MetricAnomalyRule` / `MetricForecastRule` / `MetricBurnRateRule` / `LogMatchRule` / `LogVolumeRule` / `TraceLatencyRule` / `TraceErrorRateRule`）。同时定义 `RulesProvider` 接口（8 个 accessor，按 kind 分桶）+ 两个实现：`StaticRulesProvider`（测试用）+ `CachedRulesProvider`（生产用，`atomic.Pointer[rulesSnapshot]` 原子交换 + 30s 刷新）。`Refresh` 容错：单条编译失败行单独跳过（Warn），不让一个坏行禁用整个告警子系统。所有 compile 函数严格校验 spec 字段，填充默认值（如 method=zscore、window=5m、deviation=3）。

## 2. 包信息

- **包名**：`alert`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 `pipeline.go::PipelineEvaluator` 持有 + 调用；依赖 `internal/manager/model/alert`

## 3. 关键类型与接口

```go
// 8 个 compile 后的 rule 类型
type MetricRawRule struct { ID, RuleKey, Name, Severity, ScopeType, RunbookURL string; Labels map[string]string; Expr string }
type MetricAnomalyRule struct { ...; Metric, Selector, Method, BaselineWindow, BaselineStep string; Deviation float64; ForSeconds int }
type MetricForecastRule struct { ...; Metric, Selector, FitWindow, Operator string; PredictSeconds int; Threshold float64; ForSeconds int }
type MetricBurnRateRule struct { ...; SLI string; SLO float64; Burns []BurnRateWindow }
type BurnRateWindow struct { Window string; Multiplier float64 }
type LogMatchRule struct { ...; StreamSelector, LineFilter, Window, Operator string; Threshold float64 }
type LogVolumeRule struct { ...; StreamSelector, LineFilter, Window, Operator string; Threshold float64 }
type TraceLatencyRule struct { ...; Expr string; Spec traceLatencySpec }
type TraceErrorRateRule struct { ...; Expr string; Spec traceErrorRateSpec }

// RulesProvider 把当前启用规则按 kind 分桶给 evaluator。
// 调用方必须把返回 slice 当只读——cache 跨读复用底层数组。
type RulesProvider interface {
    MetricRawRules() []MetricRawRule
    MetricAnomalyRules() []MetricAnomalyRule
    MetricForecastRules() []MetricForecastRule
    MetricBurnRateRules() []MetricBurnRateRule
    LogMatchRules() []LogMatchRule
    LogVolumeRules() []LogVolumeRule
    TraceLatencyRules() []TraceLatencyRule
    TraceErrorRateRules() []TraceErrorRateRule
}

type RuleSource interface {
    ListAllEnabledRules(ctx context.Context) ([]*model.Rule, error)
}

type rulesSnapshot struct { metricRaw, metricAnomaly, ... } // 8 个 slice

type StaticRulesProvider struct { snap rulesSnapshot }
type CachedRulesProvider struct {
    src RuleSource
    interval time.Duration
    log *slog.Logger
    snap atomic.Pointer[rulesSnapshot]
    mu sync.Mutex
    loaded bool
}
```

## 4. 关键函数与流程

### `effectiveScope`

- **签名**：`func effectiveScope(scope, kind string) string`
- **职责**：返回 rule 的 scope_type，空时按 kind 默认
- **流程**：scope 非空返回 scope；否则 `defaultScopeForKind(kind)`

### `StaticRulesProvider` + `With*Rules` 选项

- **签名**：`func NewStaticRulesProvider(opts ...StaticOption) *StaticRulesProvider`
- **职责**：测试用 fixed snapshot
- **流程**：functional option 模式；每个 `With*Rules(rs)` 把 slice 拷贝到 snap
- **错误处理**：无

### `CachedRulesProvider.NewCachedRulesProvider`

- **签名**：`func NewCachedRulesProvider(src RuleSource, interval time.Duration, log *slog.Logger) *CachedRulesProvider`
- **职责**：构造 cache
- **流程**：interval<=0 → 30s；log==nil → `slog.Default()`

### `CachedRulesProvider.load`

- **签名**：`func (c *CachedRulesProvider) load() *rulesSnapshot`
- **职责**：原子读 snapshot；nil 时返回空 snapshot
- **流程**：`c.snap.Load()`；nil → `&rulesSnapshot{}`

### `CachedRulesProvider.Refresh`

- **签名**：`func (c *CachedRulesProvider) Refresh(ctx context.Context) error`
- **职责**：从 RuleSource 加载最新规则集到 cache
- **流程**：
  1. `c.src.ListAllEnabledRules(ctx)`；err → `%w`
  2. 构建新 `snap := rulesSnapshot{}`
  3. per-row：`switch NormalizeKind(row.Kind)` 分派到 8 个 compile 函数
  4. compile 失败 → Warn + continue（**单坏行不禁用整个告警**）
  5. 未知 kind → Warn + skip
  6. `c.snap.Store(&snap)` 原子交换
  7. `c.mu.Lock; c.loaded = true; Unlock`
- **错误处理**：list 失败 return err；单行 compile 失败 Warn + skip

### `CachedRulesProvider.Loop`

- **签名**：`func (c *CachedRulesProvider) Loop(ctx context.Context) error`
- **职责**：tick 驱动 Refresh
- **流程**：
  1. **首次同步 Refresh**——保证 snapshot 在 metric ingestion 到达 evaluator 前非空
  2. `time.NewTicker(c.interval)` + defer Stop
  3. for：select ctx.Done 返回 nil / tick.C → Refresh（失败 Warn）
- **错误处理**：首次失败 Warn 但继续循环；后续失败 Warn

### 8 个 compile 函数

通用流程（以 `compileMetricRawRule` 为例）：
1. `r.RuleKey == ""` → err `"rule_key empty"`
2. json.Unmarshal `r.ConditionsJSON` 到 spec struct
3. 校验必填字段
4. 填默认值（如 method=zscore、window=5m）
5. 构造 runtime rule struct
6. `r.RunbookURL != nil` → 解引用填入
7. `r.LabelsJSON` 非空 → json.Unmarshal 到 `out.Labels`（忽略错误，空 labels 安全）
8. 返回

#### `compileMetricRawRule`
- spec：`{Expr string}`
- 校验：`Expr` 非空
- 注释：Phase-3 collapse——expr 即谓词，不校验 operator/threshold/for_seconds（PromQL parser 在 query time 拒绝 malformed expr）

#### `compileMetricAnomalyRule`
- spec：`{Metric, Selector, Method, BaselineWindow, BaselineStep, Deviation, ForSeconds}`
- 默认：method=zscore、baseline_window=1h、baseline_step=5m、deviation=3
- 校验：metric 必填；method ∈ {zscore, mad}

#### `compileMetricForecastRule`
- spec：`{Metric, Selector, FitWindow, PredictSeconds, Operator, Threshold, ForSeconds}`
- 默认：fit_window=1h
- 校验：metric 必填、predict_seconds>0、operator `validHostOperator`

#### `compileMetricBurnRateRule`
- spec：`{SLI, SLO, Burns []burnRateWindowJS}`
- 流程：
  1. `normalizeBurnRateSLIExpression(spec.SLI)`（简单 `[dur]` → `[$window]`）
  2. `burnRateSLIUsesWindow(spec.SLI)` 必须为 true
  3. `normalizeBurnRateSLOPercent(spec.SLO)`（0~1 → 0~100）
  4. SLO ∈ (0, 100)
  5. burns 至少一条；per-burn：window 必填、multiplier>0
- 错误：sli 空 / 不含 $window / SLO 越界 / burns 空

#### `compileLogMatchRule` / `compileLogVolumeRule`
- spec：`{StreamSelector, LineFilter, Window, Operator, Threshold}` / `{..., RatioOp, RatioThreshold}`
- 默认：window=5m、operator=`>=`
- 校验：stream_selector 必填、operator `validHostOperator`
- log_volume 注释：v1 与 log_match 同 shape（绝对阈值），保留 per-kind type 作 schema gate

#### `compileTraceLatencyRule`
- spec：`{Service, Operation, Quantile, Window, ThresholdMs}`
- 默认：window=5m
- 校验：service 必填、threshold_ms>0
- 流程：
  1. `quantileFloat(spec.Quantile)` → p50=0.5 / p95=0.95（默认）/ p99=0.99
  2. selector：`service_name="svc"` 或 `service_name="svc",span_name="op"`
  3. expr：`histogram_quantile(q, sum by (le) (rate(traces_spanmetrics_latency_bucket{selector}[window]))) * 1000 > threshold_ms`

#### `compileTraceErrorRateRule`
- spec：`{Service, Window, Operator, ThresholdPct}`
- 默认：window=5m、operator=`>`
- 校验：service 必填、threshold_pct>0、operator `validHostOperator`
- expr：`100 * (sum by (service_name) (rate(traces_spanmetrics_calls_total{selector,status_code="STATUS_CODE_ERROR"}[w])) / sum by (service_name) (rate(traces_spanmetrics_calls_total{selector}[w]))) op threshold`

### `validHostOperator` / `quantileFloat`

- `validHostOperator(op)`：op ∈ {`>`, `>=`, `<`, `<=`, `==`, `!=`}
- `quantileFloat(q)`：p50/0.5→0.5、p99/0.99→0.99、default/p95→0.95

## 5. 依赖关系

- **内部包**：`internal/manager/model/alert`（`Rule` / `NormalizeKind` / `IsKnownKind` / `RuleKind*` / `RuleScope*` 常量）
- **外部库**：`context` / `encoding/json` / `fmt` / `log/slog` / `strings` / `sync` / `sync/atomic` / `time`
- **被调用方**：`pipeline.go::PipelineEvaluator`（持有 `RulesProvider`）、`preview.go::PreviewRule`（调 compile 函数）
- **依赖本包**：`burn_rate_sli.go`（`normalizeBurnRateSLIExpression` / `burnRateSLIUsesWindow` / `normalizeBurnRateSLOPercent`）

## 6. 并发与资源管理

- **`atomic.Pointer[rulesSnapshot]`**：`Refresh` 用 `Store` 原子交换；`load` 用 `Load` 原子读——无锁读路径
- **`sync.Mutex` + `loaded`**：仅用于标记首次加载完成（`Loop` 启动时同步 Refresh）
- **slice 复用**：注释明示"caller MUST treat as read-only"——cache 跨读复用底层数组，evaluator 不应修改返回 slice
- **`Loop` 单 goroutine**：ticker 驱动 Refresh；并发读 `load()` 无阻塞

## 7. 设计模式与亮点

- **8 个 compile 函数严格校验**：每个 kind 一个 compile 函数，校验 + 默认值 + 运行时 struct 构造——单一职责
- **`Refresh` 容错**：单行 compile 失败 Warn + skip，不让一个坏行禁用整个告警子系统——可用性优先
- **`atomic.Pointer` 无锁读**：evaluator 每 tick 读 snapshot 无锁；Refresh 用 Store 原子交换
- **首次同步 Refresh**：`Loop` 启动时同步调 Refresh，保证 snapshot 在 metric ingestion 到达前非空——避免冷启动窗口
- **functional option**：`StaticRulesProvider` 用 `With*Rules` 选项，让测试按需注入 kind
- **`effectiveScope` 集中默认**：evaluator 代码可信任 `rule.ScopeType` 非空
- **`LabelsJSON` 忽略解析错误**：空 labels 安全；坏 JSON 不让规则失效
- **trace_* 预编译 Expr**：`compileTraceLatencyRule` / `compileTraceErrorRateRule` 在编译期构造完整 PromQL，evaluator 直接查询——避免运行时拼字符串
- **`compileMetricBurnRateRule` 复用 `burn_rate_sli.go`**：SLI/SLO 归一化逻辑集中
- **`StaticOption` 拷贝 slice**：`append([]T(nil), rs...)` 防外部修改

## 8. 注意事项

- **`RulesProvider` 返回 slice 只读**：cache 复用底层数组；evaluator 不应修改
- **`Refresh` 不返回单行错误**：单行 compile 失败 Warn + skip；只有 `ListAllEnabledRules` 失败才 return err
- **默认值**：method=zscore、baseline_window=1h、baseline_step=5m、deviation=3、fit_window=1h、window=5m、operator=`>=` 或 `>`
- **`compileMetricRawRule` 不校验 expr 语法**：Phase-3 collapse——PromQL parser 在 query time 拒绝 malformed expr；让用户写任何合法谓词
- **`compileMetricBurnRateRule` 强制 `$window` 或范围选择器**：`burnRateSLIUsesWindow` 必须为 true
- **SLO 归一化**：0~1 → 0~100；`slo == 1` 视为已百分化（1%），不转 100%
- **trace_latency Quantile 默认 p95**：`quantileFloat` 默认返回 0.95
- **`CachedRulesProvider.Loop` 首次失败不退出**：Warn 但继续 ticker；保证后续 retry 能加载
- **`StaticRulesProvider` 不支持动态更新**：测试用；生产用 `CachedRulesProvider`
- **`burns` 数组校验**：per-burn window 必填、multiplier>0；空 burns 拒绝
