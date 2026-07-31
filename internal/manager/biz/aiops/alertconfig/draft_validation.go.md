# `draft_validation.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/alertconfig/draft_validation.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/alertconfig`

## 1. 概述

本文件实现 `validateAlertRuleDraft` —— 告警规则草案的**多维度静态/动态校验**，是 `AlertRuleManager` 在生成草案后、返回给 LLM 前的最后一道质量门。校验维度：(1) preview 是否完成；(2) preview contract（host 作用域必须有 device_id label）；(3) scope 一致性（global vs host 推荐）；(4) 按 kind 分支：`metric_raw` 检查比较谓词/量纲/持续性/稀疏 counter，`log_match`/`log_volume` 检查 query 规范化/泛关键字。返回 `ConfigValidationResult{Status, Issues}`，severity error → status=failed，warning → status=warning，否则 passed。

## 2. 包信息

- **包名**：`alertconfig`
- **所属模块**：`internal/manager/biz/aiops/alertconfig`
- **依赖方向**：被 `alert_rule_manager.go` 调用；依赖 `aiops/tools`（类型）、`aiops/alertdraft`（`ShouldBlockCreateOnPreviewSkip`、`HostScopeRecommended`）

## 3. 关键类型与接口

```go
const (
    validationSeverityError   = "error"
    validationSeverityWarning = "warning"
)

var metricRawTrailingComparisonRE = regexp.MustCompile(
    `^(.+?)\s*(==|!=|>=?|<=?)\s*([+-]?[0-9.]+(?:[eE][+-]?\d+)?)\s*$`,
)
```

复用类型（来自 `aiops/tools`）：`AlertRuleConfigInput`、`ConfigValidationResult{Status, Issues}`、`ConfigValidationIssue{Severity, Code, Message, Suggestion}`、`PreviewResult`。`PreviewResult`（含 FireCount/Series/Samples/SkippedReason）由 `alert_rule_manager.go` 定义。

## 4. 关键函数与流程

### `validateAlertRuleDraft`
- **签名**：`func validateAlertRuleDraft(rule, requestText string, preview *PreviewResult) ConfigValidationResult`
- **职责**：总入口；汇总所有维度 issue；status = passed/warning/failed
- **流程**：
  1. preview.SkippedReason 非空 → issue `preview_skipped`（severity 由 `ShouldBlockCreateOnPreviewSkip` 决定：阻塞关键词 → error，否则 warning）
  2. `validatePreviewContract` → host 作用域预览必须含 device_id label
  3. `validateScopeConsistency` → global 作用域但 host 推荐 → warning
  4. 按 kind 分支：`metric_raw` → `validateMetricRawDraft`；`log_match`/`log_volume` → `validateLogMatchDraft`
  5. 任意 error → status=failed；只有 warning → warning；无 issue → passed

### `validateMetricRawDraft`
- **职责**：metric_raw 专属校验，6 类 issue
- **流程**：
  1. `hasMetricRawComparisonPredicate(expr)` false → error `metric_raw_predicate_missing`（裸序列会被误触发）
  2. `splitMetricRawComparison` 拆 LHS + threshold：
     - 预览无序列 + LHS 含算术运算符 → error `metric_raw_no_preview_series`（label 不匹配 / 分子缺失）
     - `suspiciousMagnitudeIssue` 检查 NaN/Inf / 量纲异常 → error
  3. `mentionsSustained`（请求文本含"持续/连续/for/sustained"）且无 `_over_time` → warning `sustained_window_missing`
  4. preview.FireCount > 100 → warning `high_preview_fire_count`
  5. `sparseCounterMinRateIssue` → warning `sparse_counter_min_rate`（`min_over_time(rate(...)) > 0` 对稀疏 counter 要求持续增长）

### `validateLogMatchDraft`
- **职责**：log_match/log_volume 专属校验
- **流程**：
  1. spec.query 非空但 line_filter 空 → error `log_query_not_normalized`（evaluator 只跑 stream_selector + line_filter，query 会被忽略）
  2. broad filter（含 error/panic/exception）+ stream 未限 unit/service_name/identifier → warning `broad_log_match`（避免把 exporter 噪声当业务故障）

### `validatePreviewContract`
- **职责**：host 作用域 + 预览有 fire + 有 samples 但无 device_id label → error `host_preview_missing_device_id`（创建后无法写主机告警事件）

### `validateScopeConsistency`
- **职责**：scope_type=global + `HostScopeRecommended` true + 预览含 device_id → warning `host_scope_recommended`

### `hasMetricRawComparisonPredicate`
- **职责**：扫描 expr 字符流，判断是否含布尔比较谓词（>、<、>=、<=、==、!=）
- **流程**：跳过引号字符串、跳过 `{...}` selector 内部；找到顶层 `>`、`<` → true；`=`/`!` 且下一个字符是 `=` → true。**关键**：selector 内的 `=` 不算谓词

### `splitMetricRawComparison`
- **职责**：用 `metricRawTrailingComparisonRE` 拆 expr 为 (LHS, threshold)；解析 threshold 为 float64

### `suspiciousMagnitudeIssue`
- **职责**：预览样本量纲异常检测
- **流程**：
  - 阈值绝对值 > 1000 → 跳过（避免误报大阈值）
  - limit = max(10000, |threshold|*1000)
  - 任一 sample NaN/Inf → error `metric_raw_non_finite_value`（分母为 0）
  - |sample.Value| > limit → error `metric_raw_suspicious_magnitude`（量纲/分母错误）

### `sparseCounterMinRateIssue`
- **职责**：识别 `min_over_time(rate(...)) > 0` 对稀疏 counter（_total/deadlock/error/fail）的反模式
- **流程**：lowercase + 去空格后子串匹配 → warning `sparse_counter_min_rate`；建议改用 `increase(counter[窗口]) > 0`

### `mentionsSustained / hasSustainedPromQL`
- **职责**：识别"持续"语义；检查 PromQL 是否含 `_over_time`/`count_over_time`/`[...:...]` 子查询

### 辅助函数
- `metricRawExpr(rule)`：从 spec 取 expr/promql/query
- `previewHasDeviceIDLabel(preview)`、`noPreviewSignal(preview)`、`looksLikeArithmetic(expr)`
- `stringFromSpec(spec, keys...)`：按 keys 顺序取第一个非空字符串
- `validationHasErrors / validationWarnings / validationErrorMessage`：聚合结果访问器

## 5. 依赖关系

- **内部包**：
  - `internal/manager/biz/aiops/tools`（`AlertRuleConfigInput`、`ConfigValidationResult`、`ConfigValidationIssue`、`ConfigCaller`）
  - `internal/manager/biz/aiops/alertdraft`（`ShouldBlockCreateOnPreviewSkip`、`HostScopeRecommended`）
- **外部库**：标准库 `regexp`、`math`、`fmt`、`strings`

## 6. 并发与资源管理

- **纯函数**：所有方法无状态、无 IO、无锁；可被多个 goroutine 并发调用
- **正则常量**：`metricRawTrailingComparisonRE` 包级 var，编译一次
- **无 ctx 参数**：纯计算无 IO

## 7. 设计模式与亮点

- **多维 issue 累积**：一次校验返回所有问题（不止首个错误），让 LLM/用户一次性修正
- **severity 三态**：error 阻断 apply，warning 仅提示；`ShouldBlockCreateOnPreviewSkip` 决定 preview_skipped 的 severity
- **预览结果联动**：动态结合 PreviewResult（FireCount/Samples/Series）做语义检查，而非纯语法
- **字符流扫描器 `hasMetricRawComparisonPredicate`**：自己写状态机（quote/selector depth），避免正则解析 PromQL 的歧义
- **量纲异常启发式**：`max(10000, |threshold|*1000)` 自适应阈值，避免小阈值误报
- **稀疏 counter 反模式检测**：识别 `min_over_time(rate(...)) > 0` 对稀疏事件漏报问题
- **持续窗口语义识别**：中英文关键词 + PromQL `_over_time` 检测
- **作用域一致性**：global 作用域但 host 推荐 → warning（不强制阻断，给用户选择空间）
- **issue 三段式**：Message + Suggestion 分离，Suggestion 给可执行建议（提高 UX）

## 8. 注意事项

- **正则仅解析尾部比较**：`metricRawTrailingComparisonRE` 假设比较在 expr 末尾；前置比较（如 `1 < rate(x[5m])`）不会被识别
- **`hasMetricRawComparisonPredicate` 不支持复杂嵌套**：括号内比较会被识别为顶层（depth 未跟踪 `()`），可能误判
- **`sparseCounterMinRateIssue` 子串匹配**：去空格后子串匹配，可能匹配到注释或字符串字面量（实际 PromQL 注释少见）
- **`mentionsSustained` 关键词覆盖**：仅中英文常见词，方言/缩写可能漏检
- **`suspiciousMagnitudeIssue` 阈值 > 1000 跳过**：避免大阈值（如字节单位）误报，但也可能漏掉真异常
- **`validatePreviewContract` 仅 host 作用域**：global 作用域不需要 device_id label
- **依赖 PreviewResult 字段完整**：PreviewResult 由 alert_rule_manager 预览阶段填充，若预览失败 SkippedReason 必须非空才能触发 `preview_skipped` issue
