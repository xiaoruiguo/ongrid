# `spec_normalize.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/alertdraft/spec_normalize.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/alertdraft`

## 1. 概述

本文件是 alertdraft 包的**规则 spec 规范化核心**：`normalizeAlertRuleSpec` 按 kind 分支为 9 种告警类型（metric_raw/metric_anomaly/metric_forecast/metric_burn_rate/log_match/log_volume/trace_latency/trace_error_rate/空）设置默认值 + 规范化字段。配套 `normalizeAlertRuleKind`/`inferAlertRuleKind`（kind 别名归一 + 从 spec keys 推断）、`normalizeAlertScopeType`/`normalizeAlertScopeForKind`（scope 规范化）、`normalizeLogQuerySpec`/`normalizeLogLineFilter`/`parseLogLineFilterChain`（LogQL 解析）、`normalizeBurnRateSLIExpression`（burn rate $window 占位符）、`sanitizeLogStreamSelector`（log selector 白名单过滤）。

## 2. 包信息

- **包名**：`alertdraft`
- **所属模块**：`internal/manager/biz/aiops/alertdraft`
- **依赖方向**：被 `compiler.go` 调用；依赖 `metric_raw.go`（`normalizeMetricRawSpec`）、`promql.go`（selector/spec 辅助）、`defaults.go`（`canonicalAlertMetric`）、`regex.go`（多个正则）、`request_hints.go`（`normalizeMetricForecastSpec`）

## 3. 关键类型与接口

```go
type parsedLogLineFilter struct {
    op   string
    body string
}
```

## 4. 关键函数与流程

### `normalizeAlertScopeType`
- **签名**：`func normalizeAlertScopeType(scopeType, kind string) string`
- **职责**：scope 别名归一
- **映射**：
  - global/all/any/source/sources/database/databases/db/service/cluster → `global`
  - host/device/node/edge/machine/server → `host`
  - monitoring_pipeline/pipeline/scrape/collector → `monitoring_pipeline`
  - 空 → 空；default → 规范化后原样

### `normalizeAlertScopeForKind`
- **职责**：按 kind 调整 scope（log/trace/burn_rate 不允许 monitoring_pipeline → 强制 global）

### `normalizeAlertRuleKind`
- **签名**：`func normalizeAlertRuleKind(kind string) string`
- **职责**：kind 别名归一（lowercase + 替换 `-`/空格 为 `_`）
- **映射**：threshold→metric_threshold；promql/prom_query/raw_promql/raw_metric/custom_metric→metric_raw；anomaly/baseline→metric_anomaly；forecast/predict/prediction→metric_forecast；burn_rate/slo_burn_rate→metric_burn_rate；log/log_regex/log_error/log_keyword→log_match；log_rate/log_count/log_spike→log_volume；latency/trace_p95/trace_p99→trace_latency；error_rate/trace_errors/trace_error→trace_error_rate

### `inferAlertRuleKind`
- **签名**：`func inferAlertRuleKind(in RuleConfigInput) string`
- **职责**：从 spec keys 推断 kind
- **流程**（按优先级）：
  - stream_selector + ratio_op/ratio_threshold → log_volume
  - stream_selector/line_filter/filter/regex/pattern → log_match
  - threshold_ms/latency_ms/quantile/operation → trace_latency
  - threshold_pct/error_rate_pct/error_rate_threshold → trace_error_rate
  - sli/slo/burns → metric_burn_rate
  - predict_seconds/fit_window → metric_forecast
  - baseline_window/baseline_step/deviation/method → metric_anomaly
  - expr/promql/query/metric_key/metric/metric_name → metric_raw
  - default → 空

### `normalizeAlertRuleSpec`
- **签名**：`func normalizeAlertRuleSpec(in RuleConfigInput) RuleConfigInput`
- **职责**：按 kind 分支规范化 spec（核心）
- **流程**：
  - kind 空 → `inferAlertRuleKind`
  - **metric_raw** → `normalizeMetricRawSpec`（委托 metric_raw.go）
  - **metric_anomaly** → 默认 method=zscore、baseline_window=1h、baseline_step=5m、deviation=3；canonical metric
  - **metric_forecast** → 默认 fit_window=1h、predict_seconds=21600、operator=<=；canonical metric；`normalizeMetricForecastSpec`
  - **metric_burn_rate** → `normalizeBurnRateSLIExpression`；默认 slo=99.9；默认 burns=[{1h,14.4},{6h,6}]
  - **log_match** → `normalizeLogQuerySpec`；filter→line_filter 折叠；`normalizeLogLineFilter` 拆 filter + selector；`mergeLogStreamSelector` 合并；默认 window=5m、operator=>=、threshold=1
  - **log_volume** → 类似 log_match；ratio_op/ratio_threshold；默认 window=5m、ratio_op=>=、ratio_threshold=2
  - **trace_latency** → threshold→threshold_ms；默认 quantile=p95、window=5m、threshold_ms=500
  - **trace_error_rate** → threshold→threshold_pct；默认 window=5m、operator=>=、threshold_pct=1
  - **空** → `normalizeSpecMetricCondition`

### `normalizeLogQuerySpec`
- **职责**：把 spec.query 拆成 stream_selector + line_filter
- **流程**：`splitSimpleLogQLQuery` 拆分 → 填 stream_selector/line_filter → delete query

### `splitSimpleLogQLQuery`
- **职责**：拆简单 LogQL `{stream} |filter` 为 (stream, filter)
- **流程**：`readPromSelector` 取 `{...}` → rest 空 → (stream, "", true)；rest 有 → `parseLogLineFilterChain` 非空 → `normalizeLogLineFilter` 拆 (filter, selector) → `mergeLogStreamSelector`

### `normalizeLogLineFilter`
- **签名**：`func normalizeLogLineFilter(raw string) (string, string)`
- **职责**：规范化行过滤器，返回 (filter, selector)
- **流程**：
  1. `parseLogLineFilterChain` 解析链式 `|=xxx|~yyy` 
  2. 链非空 → 遍历每 part：
     - `normalizePlainLogLineFilter` 提取 selector → 追加 selectorParts；filter 非空 → regexParts
     - `logSelectorMatcherFromLineFilter` 提取 selector → selectorParts
     - 否则：`|=` → `regexp.QuoteMeta(body)`；`|~` → body 原样；default → body
     - 返回 `strings.Join(regexParts, "|")` + `strings.Join(selectorParts, ",")`
  3. 链空 → `logLineFilterExprRE` 匹配单过滤器 → 处理
  4. fallback → `normalizePlainLogLineFilter`

### `normalizePlainLogLineFilter`
- **职责**：识别 `level=error rest` 这类 label 前缀过滤器
- **流程**：`logLabelPrefixFilterRE` 匹配 → key 是已知 log label → 返回 (rest, `key="value"`)；否则 `normalizeAlternationLogLineFilter`

### `normalizeAlternationLogLineFilter`
- **职责**：识别 `(level|severity)=error` 这类交替 label 过滤器
- **流程**：`logLabelAlternationPrefixFilterRE` 匹配 → 遍历交替项找已知 label → 返回 (rest, `key="value"`)

### `parseLogLineFilterChain`
- **职责**：解析 `|=xxx|~yyy` 链为 `[]parsedLogLineFilter`
- **流程**：循环识别 op（|=~/|=/|~/!=/!~）→ `|=~` → `|~` → 取 body（引号 or 下一个 op 前）→ 追加

### `logSelectorMatcherFromLineFilter`
- **职责**：从 `key op "value"` 提取 selector（key 必须是已知 log label）

### `mergeLogStreamSelector`
- **职责**：合并两个 stream selector，base 优先，add 补充新 key

### `normalizeBurnRateSLIExpression`
- **职责**：burn rate SLI 表达式 $window 占位符化
- **流程**：含 `$window` → 原样；否则 `promSimpleRangeSelectorRE` 替换 `[5m]` 为 `[$$window]`

### `normalizeBurnRateSLOPercent`
- **职责**：SLO 0-1 → 0-100

### `normalizeLogStreamSelector`
- **职责**：log stream selector 规范化（空 → fallback；journald job selector → 猜测；`sanitizeLogStreamSelector` 白名单过滤）

### `normalizeGuessedJournaldLogSelector`
- **职责**：`job=journal` → `ongrid_source=~"journald(:.*)?"`

### `looksLikeJournaldJobSelector`
- **职责**：selector 含 `job="journal..."` → true

### `isKnownLogSelectorLabel`
- **职责**：log label 白名单（detected_level/device_id/filename/identifier/level/ongrid_source/service_name/unit）

### `sanitizeLogStreamSelector`
- **职责**：过滤 selector 仅保留已知 label + 去重

### `normalizeLogSelectorLabelKey`
- **职责**：priority/severity → level

### `formatPromLabelMatcher`
- **职责**：`key op "value"` 格式化

## 5. 依赖关系

- **包内依赖**：
  - `metric_raw.go`：`normalizeMetricRawSpec`、`normalizeSpecMetricCondition`
  - `promql.go`：`firstSpecString`、`firstSpecNumber`、`alertSpecStringValue`、`alertSpecSelector`、`normalizeAlertOperator`、`readPromSelector`、`consumePromQuotedString`、`splitPromSelectorMatchers`、`parsePromLabelMatcherWithOperator`、`selectorFromSpecLabels`、`setSpecDefaultString`、`setSpecDefaultNumber`、`hasAnySpecKey`
  - `defaults.go`：`canonicalAlertMetric`
  - `regex.go`：`logLineFilterExprRE`、`logLabelPrefixFilterRE`、`logLabelAlternationPrefixFilterRE`、`promSimpleRangeSelectorRE`
  - `request_hints.go`：`normalizeMetricForecastSpec`
- **外部库**：标准库 `fmt`、`regexp`、`strconv`、`strings`

## 6. 并发与资源管理

- **纯函数**：无状态、无 IO、无锁
- **正则包级常量**：编译一次

## 7. 设计模式与亮点

- **kind 分派**：`normalizeAlertRuleSpec` 按 kind switch，每种 kind 独立处理，职责清晰
- **kind 双向推断**：`normalizeAlertRuleKind`（显式归一）+ `inferAlertRuleKind`（从 spec keys 推断）
- **LogQL 拆分**：`splitSimpleLogQLQuery` 把 `{stream} |filter` 拆为 stream_selector + line_filter，匹配 evaluator 能力
- **链式 filter 解析**：`parseLogLineFilterChain` 解析 `|=xxx|~yyy` 链，逐 part 提取 selector + 累积 regex
- **label 前缀提取**：`normalizePlainLogLineFilter` 识别 `level=error rest` → 提取 selector + 留 rest 作 regex
- **交替 label 处理**：`normalizeAlternationLogLineFilter` 处理 `(level|severity)=error` 交替
- **log label 白名单**：`sanitizeLogStreamSelector` 仅保留已知 label，防止 LLM 注入非法 label
- **burn rate $window 占位符**：`normalizeBurnRateSLIExpression` 把 `[5m]` 替换为 `[$$window]`，供引擎按 burn window 替换
- **SLO 百分比归一**：0-1 → 0-100，兼容小数和整数输入
- **journald job 猜测**：`job=journal` → `ongrid_source=~"journald(:.*)?"`，适配 ongrid 日志管道

## 8. 注意事项

- **`normalizeAlertRuleSpec` 会原地修改 spec map**：调用方需注意引用语义
- **`inferAlertRuleKind` 优先级**：log_volume（stream_selector + ratio）优先于 log_match（stream_selector）；trace_latency（threshold_ms）优先于 trace_error_rate（threshold_pct）；顺序敏感
- **`normalizeLogLineFilter` 链式解析复杂**：`parseLogLineFilterChain` 失败返回 nil，fallback 到单过滤器匹配
- **`logLabelPrefixFilterRE` 值格式限制**：`[A-Za-z0-9_:/-]+` 不支持中文/Unicode 值
- **`sanitizeLogStreamSelector` 丢弃未知 label**：LLM 生成的非白名单 label 会被静默丢弃，可能导致 selector 过宽
- **`normalizeBurnRateSLIExpression` 不处理已含 $window 的表达式**：认为已规范化
- **`normalizeLogStreamSelector` fallback 链**：空 → fallback；journald job → 猜测；其他 → sanitize
- **`normalizeAlertScopeForKind` 仅处理 monitoring_pipeline**：其他 scope 不调整
- **`formatPromLabelMatcher` 默认 op=`=`**：op 空时补 `=`
