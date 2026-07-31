# `request_hints.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/alertdraft/request_hints.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/alertdraft`

## 1. 概述

本文件提供 alertdraft 包的**请求上下文感知规范化**：根据用户原始请求文本（requestText）调整 rule spec。三大职责：(1) `normalizeMetricSourceScopeForRequest` 清理 LLM 误标为"显式 scope"的 metric source 身份标签（若用户请求未显式提及这些身份）；(2) `applyAlertRuleRequestHints` 按 kind 应用提示——log_match/log_volume 从请求提取 stream_selector label，metric_forecast 重写 disk available percent；(3) `normalizeMetricForecastRequestHints`/`rewriteMetricForecastDiskAvailablePercent` 把"磁盘可用空间百分比"预测重写为"磁盘使用率"阈值（语义等价但引擎支持更好）。

## 2. 包信息

- **包名**：`alertdraft`
- **所属模块**：`internal/manager/biz/aiops/alertdraft`
- **依赖方向**：被 `compiler.go` 的 `normalizeAlertRuleConfigInputForRequest` 调用；依赖 `promql.go`（selector 解析）、`regex.go`（`promMetricSourceIdentityMatcherRE`/`nodeFilesystemMetricSelectorRE`）、`defaults.go`（`canonicalAlertMetric`）、`metric_raw.go`（`sanitizeDatabaseIdentitySpecSelectors`）

## 3. 关键类型与接口

```go
type metricSourceIdentityMatcher struct {
    key   string
    value string
}
```

## 4. 关键函数与流程

### `normalizeMetricSourceScopeForRequest`
- **签名**：`func normalizeMetricSourceScopeForRequest(in RuleConfigInput, requestText string) RuleConfigInput`
- **职责**：清理 LLM 误标的"显式 scope"标记 + 身份标签
- **流程**：
  1. spec nil / requestText 空 / spec 未标 `sourceSelectorExplicitlyScoped` → 跳过
  2. spec 不含 metric source 身份标签 OR 请求显式 scope metric source → 跳过
  3. 否则 `clearSourceExplicitSpecFlags(spec)` 清除 8 个显式 scope 标记 key

### `clearSourceExplicitSpecFlags`
- **职责**：删除 spec 中的 8 个显式 scope 标记 key（source_explicit/selector_explicit/scope_explicit/explicit_source/explicit_selector/selector_source/scope_source/source_scope）

### `metricSourceIdentityMatchersFromSpec`
- **签名**：`func metricSourceIdentityMatchersFromSpec(spec) []metricSourceIdentityMatcher`
- **职责**：从 spec 的 selector + expr/promql/query 提取所有 metric source 身份标签 matcher
- **流程**：
  1. 从 `alertSpecSelector(spec)` 拆分 matcher，过滤 `isMetricSourceRequestIdentityLabel` key
  2. 从 expr/promql/query 用 `promMetricSourceIdentityMatcherRE` 正则提取
  3. 合并返回

### `isMetricSourceRequestIdentityLabel`
- **职责**：身份标签识别（ongrid_source/device_id/job/instance/service）

### `requestExplicitlyScopesMetricSource`
- **签名**：`func requestExplicitlyScopesMetricSource(requestText, spec) bool`
- **职责**：用户请求是否显式 scope metric source
- **流程**：
  1. text lowercase + trim
  2. 含 "ongrid_source"/"device_id"/"db:"/"custom:" → true
  3. 遍历 spec 中的身份 matcher → `requestMentionsMetricSourceMatcher`

### `requestMentionsMetricSourceMatcher`
- **职责**：请求文本是否提及某个身份 matcher 的 value
- **流程**（按 key 分支）：
  - ongrid_source：text 含 value；或 value 是 `db:xxx`/`custom:xxx` 前缀，text 含 xxx 且 `requestHasSourceScopeWord`
  - device_id：text 含 "device_id" 且含 value
  - job/instance/service：text 含 key 且含 value

### `requestHasSourceScopeWord`
- **职责**：请求是否含"source/采集源/来源/实例/instance"关键词

### `applyAlertRuleRequestHints`
- **签名**：`func applyAlertRuleRequestHints(in RuleConfigInput, requestText string) RuleConfigInput`
- **职责**：按 kind 应用请求提示
- **流程**：
  - log_match/log_volume → `logSelectorHintsFromRequest` 提取 stream_selector，`mergeLogStreamSelector` 合并
  - metric_forecast → `normalizeMetricForecastRequestHints`

### `logSelectorHintsFromRequest`
- **签名**：`func logSelectorHintsFromRequest(text string) string`
- **职责**：从请求文本提取 level/unit/identifier/service_name/filename/device_id 6 个字段的值
- **流程**：每字段用正则 `(?i)(?:^|[^\pL\pN_])<field>\s*(?:=|:|：|为|是)\s*["']?([A-Za-z0-9_.:/-]+)["']?` 匹配 → `formatPromLabelMatcher` 拼接

### `logSelectorHintValueFromRequest`
- **职责**：单字段提取；正则支持中英文分隔符（=/:/：/为/是）

### `normalizeMetricForecastRequestHints`
- **签名**：`func normalizeMetricForecastRequestHints(spec, requestText)`
- **职责**：metric_forecast disk available percent 重写
- **流程**：
  1. spec nil OR 请求未提及 filesystem available percent → 跳过
  2. metric 非 disk_avail_bytes → 跳过
  3. selector 空 → `filesystemSelectorFromRequest` 推断（含"根分区/根目录/ /"→ `mountpoint="/"`）
  4. `rewriteMetricForecastDiskAvailablePercent(spec, selector)`

### `normalizeMetricForecastSpec`
- **签名**：`func normalizeMetricForecastSpec(spec map[string]interface{})`
- **职责**：spec 级别的 metric_forecast 重写（无请求文本）
- **流程**：expr 是 filesystem available percent expr → `commonNodeFilesystemSelector` 提取公共 selector → 重写

### `filesystemAvailablePercentExpr`
- **职责**：expr 含 `node_filesystem_size_bytes` + (`node_filesystem_avail_bytes`|`node_filesystem_free_bytes`) + `/` → true

### `requestMentionsFilesystemAvailablePercent`
- **职责**：请求文本提及 disk/filesystem/磁盘/文件系统/分区 + avail/free/可用/剩余/空闲 + %/百分比/使用率/占比

### `filesystemSelectorFromRequest`
- **职责**：含"根分区/根目录/ /"→ `mountpoint="/"`

### `rewriteMetricForecastDiskAvailablePercent`
- **签名**：`func rewriteMetricForecastDiskAvailablePercent(spec, selector)`
- **职责**：把"可用空间百分比"重写为"使用率"阈值
- **流程**：
  1. 取 threshold（0-1 → 乘 100）
  2. threshold 不在 0-100 → 跳过
  3. spec.metric = "disk_used_pct"
  4. spec.operator = `invertAvailablePercentOperator`（< → >，<= → >=，反之亦然）
  5. spec.threshold = 100 - threshold
  6. selector 非空 → spec.selector = selector
  7. delete spec.expr/promql/query

### `invertAvailablePercentOperator`
- **职责**：可用百分比操作符反转（< → >，因为"可用 < 20%" 等价于 "使用率 > 80%"）

### `commonNodeFilesystemSelector`
- **签名**：`func commonNodeFilesystemSelector(expr string) string`
- **职责**：从 expr 中所有 `node_filesystem_*_bytes{...}` 提取公共 label
- **流程**：`nodeFilesystemMetricSelectorRE` 匹配所有 → 第一个的 labels 作为初始 common → 后续取交集 → `selectorFromSpecLabels`

## 5. 依赖关系

- **包内依赖**：
  - `promql.go`：`alertSpecSelector`、`normalizeSelectorPart`、`splitPromSelectorMatchers`、`parsePromLabelMatcherWithOperator`、`firstSpecString`、`firstSpecNumber`、`alertSpecStringValue`、`normalizeAlertOperator`、`selectorFromSpecLabels`、`exactPromSelectorLabels`、`formatPromLabelMatcher`、`mergeLogStreamSelector`
  - `regex.go`：`promMetricSourceIdentityMatcherRE`、`nodeFilesystemMetricSelectorRE`
  - `defaults.go`：`canonicalAlertMetric`
  - `metric_raw.go`：`sanitizeDatabaseIdentitySpecSelectors`（注：实际由 `normalizeMetricRawSpec` 调用，本文件不直接调用）
- **外部库**：标准库 `fmt`、`regexp`、`strings`

## 6. 并发与资源管理

- **纯函数**：无状态、无 IO、无锁
- **正则动态编译**：`logSelectorHintValueFromRequest` 内 `regexp.MustCompile` 每次调用编译（潜在性能问题，见注意事项）

## 7. 设计模式与亮点

- **请求上下文感知**：根据用户原话调整 spec，而非纯结构化输入
- **身份标签清理**：LLM 倾向于把示例中的 ongrid_source/device_id 标为"显式 scope"，本函数清理误标
- **多语言关键词识别**：`requestMentionsFilesystemAvailablePercent` 同时识别中英文（disk/磁盘、available/可用、%/百分比）
- **log selector 自动提取**：从"level=error"这类请求文本提取 stream_selector，减少 LLM 负担
- **语义等价重写**：`rewriteMetricForecastDiskAvailablePercent` 把"可用 < 20%"重写为"使用率 > 80%"，操作符反转 + 阈值取补
- **公共 selector 提取**：`commonNodeFilesystemSelector` 从多个 filesystem metric 提取公共 label，避免 selector 丢失
- **显式 scope 标记清理**：8 个 key 全清，避免半清理状态

## 8. 注意事项

- **`logSelectorHintValueFromRequest` 正则每次编译**：`regexp.MustCompile` 在函数内调用，高频场景有性能开销；可考虑包级预编译
- **`requestMentionsMetricSourceMatcher` ongrid_source 特殊处理**：`db:xxx`/`custom:xxx` 前缀剥离后匹配，需 `requestHasSourceScopeWord` 二次确认
- **`rewriteMetricForecastDiskAvailablePercent` 阈值取补**：`100 - threshold`，threshold 必须 0-100；否则跳过
- **`filesystemSelectorFromRequest` 仅识别根分区**：`mountpoint="/"`；其他分区不识别
- **`commonNodeFilesystemSelector` 无 selector 时返回空**：`selectorFromSpecLabels(nil)` 返回空字符串
- **`normalizeMetricSourceScopeForRequest` 仅清理标记**：不清理 selector 本身的身份标签（那是 `sanitizeDatabaseIdentitySpecSelectors` 的职责）
- **`applyAlertRuleRequestHints` 仅处理 3 种 kind**：其他 kind 不应用提示
