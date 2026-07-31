# `metric_raw.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/alertdraft/metric_raw.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/alertdraft`

## 1. 概述

本文件是 alertdraft 包的**metric_raw 规则 PromQL 处理核心**：负责把 LLM 生成的 metric_raw spec 规范化为可信 PromQL。三大职责：(1) `normalizeMetricRawSpec` 主入口，处理 expr/metric 两种输入路径；(2) `stripLeakedMetricSourceIdentityMatchersFromPromQL` 从 PromQL 中剥离 LLM 泄漏的 metric source 身份标签（ongrid_source/device_id/job/instance/service），防止草案绑定到特定采集源；(3) `sanitizeDatabaseIdentitySpecSelectors` 清理 spec 中各种 selector 字段格式的身份标签。配套大量 PromQL 解析辅助（selector 拆分、label matcher 解析、selector 合并）。

## 2. 包信息

- **包名**：`alertdraft`
- **所属模块**：`internal/manager/biz/aiops/alertdraft`
- **依赖方向**：被 `spec_normalize.go` 调用；依赖 `promql.go`（selector 解析辅助）、`regex.go`（`promMetricNameRE`/`promTrailingComparisonRE`/`promTokenRE`）、`defaults.go`（`canonicalAlertMetric`/`isClosedSetAlertMetric`/`isDatabaseMetricName`）

## 3. 关键类型与接口

无新类型定义。复用 `RuleConfigInput`、`RuleCondition`。

## 4. 关键函数与流程

### `normalizeMetricRawSpec`
- **签名**：`func normalizeMetricRawSpec(in RuleConfigInput) RuleConfigInput`
- **职责**：metric_raw spec 规范化主入口
- **流程**：
  1. 若 spec 有 expr/promql/query：
     - 判断是否需要清理身份标签（`shouldStripImplicitMetricSourceSelector` 或数据库指标 + 数据库身份标签）
     - 清理 spec selector → `sanitizeDatabaseIdentitySpecSelectors`
     - 若 selector 空 + 未显式 scope → `stripLeakedMetricSourceIdentityMatchersFromPromQL` 剥离 expr 内联身份标签
     - 否则 `mergeSelectorIntoPromQL` 把 selector 合并进 expr
     - `appendMetricRawComparisonFromSpec` 若 expr 无尾部比较，从 spec 补 operator/threshold
     - 写回 spec.expr
  2. 若 spec 有 metric + 是 closed-set → `normalizeSpecMetricCondition`（转 metric_threshold）
  3. 否则 `buildMetricRawExprFromSpec` 构造 expr（metric + selector + operator + threshold）
  4. fallback `normalizeSpecMetricCondition`

### `stripLeakedMetricSourceIdentityMatchersFromPromQL`
- **签名**：`func stripLeakedMetricSourceIdentityMatchersFromPromQL(expr string) (string, bool)`
- **职责**：扫描 PromQL，剥离 metric 后的内联 selector 中的身份标签
- **流程**（字符流状态机）：
  1. 跳过引号字符串 `consumePromQuotedString`
  2. 跟踪 `labelListDepth`（by/without/on/ignoring 后的 `(...)` 内部不处理）
  3. 识别 identifier（metric 名或 label key）
  4. identifier 后跟 `(` → 函数，跳过
  5. identifier 后跟 `{` → `readPromSelector` 读取完整 selector
  6. `shouldStripInlineSourceIdentityMatchers(metric, selector)` 决定是否剥离：
     - 数据库 metric + selector 含数据库身份标签 → 剥离
     - selector 含隐式 metric source 身份标签 → 剥离
     - 自定义 metric + selector 含 `db:`/`custom:` ongrid_source → 剥离
  7. `stripDatabaseIdentityMatchers` 实际剥离身份 label，重写 selector
  8. 返回新 expr + changed 标记

### `stripDatabaseIdentityMatchers`
- **签名**：`func stripDatabaseIdentityMatchers(selector string) (string, bool)`
- **职责**：从 selector 字符串中删除身份标签 matcher
- **流程**：`splitPromSelectorMatchers` 拆分 → 遍历 `parsePromLabelMatcherWithOperator` → `isDatabaseIdentityLabel(key)` 跳过 → 重组

### `isDatabaseIdentityLabel` / `isMetricSourceIdentityLabel`
- **职责**：身份标签识别
- **`isDatabaseIdentityLabel`**：ongrid_source、device_id、job、instance、service
- **`isMetricSourceRequestIdentityLabel`**：同上（request_hints.go 用）
- **`isMetricSourceIdentityLabel`**：ongrid_source、device_id、instance（更窄，用于隐式检测）

### `isDatabaseMetricName` / `isCustomMetricName`
- **`isDatabaseMetricName`**：前缀 mysql_/pg_/postgres_/postgresql_/redis_/mongodb_/mongo_
- **`isCustomMetricName`**：前缀 custom_

### `sanitizeDatabaseIdentitySpecSelectors`
- **签名**：`func sanitizeDatabaseIdentitySpecSelectors(spec map[string]interface{}) bool`
- **职责**：清理 spec 中 5 种 selector 字段格式的身份标签
- **流程**：遍历 `selector`/`label_selector`/`matchers`/`labels`/`match_labels` → `sanitizeDatabaseIdentitySelectorValue` 递归处理 string/[]string/[]interface{}/map[string]string/map[string]interface{} → 空 → delete key

### `sanitizeDatabaseIdentitySelectorValue`
- **签名**：`func sanitizeDatabaseIdentitySelectorValue(raw interface{}) (interface{}, bool, bool)`
- **职责**：递归处理各种 selector 值类型
- **返回**：(sanitized, empty, changed)
- **流程**：
  - string → `stripDatabaseIdentityMatchers`
  - []string / []interface{} → 逐项递归，过滤空项
  - map[string]string / map[string]interface{} → 删除身份 key 的项

### `splitPromSelectorMatchers` / `parsePromLabelMatcherWithOperator` / `parseExactPromLabelMatcher`
- **`splitPromSelectorMatchers`**：按 `,` 拆分 selector（跳过引号内逗号 + 处理转义）
- **`parsePromLabelMatcherWithOperator`**：解析 `key op "value"`，op 优先级 `!=` > `=~` > `!~` > `=`；`strconv.Unquote` 解析 value
- **`parseExactPromLabelMatcher`**：仅 op=`=` 的精确匹配

### `exactPromSelectorLabels`
- **职责**：selector → `map[string]string`（仅 `=` 精确匹配的 key-value）

### `normalizeSpecMetricCondition`
- **职责**：spec.metric + operator + threshold + closed-set → 转为 conditions（metric_threshold kind）
- **流程**：metric 是 closed-set → kind=metric_threshold + conditions=[{metric, operator, threshold, window=5m, aggregator=avg}] + 清空 spec；否则尝试 `buildMetricRawExprFromSpec`

### `buildMetricRawExprFromSpec`
- **职责**：从 spec 构造 `(metric{selector}) op threshold` 表达式
- **流程**：先清理身份标签 → 取 metric + selector + operator + threshold → `fmt.Sprintf("(%s) %s %s", base, op, threshold)`

### `appendMetricRawComparisonFromSpec`
- **职责**：expr 无尾部比较时，从 spec 补 operator/threshold
- **流程**：`promTrailingComparisonRE` 已匹配 → 跳过；否则拼接 `(<expr>) op threshold`

### `mergeSelector` / `metricSelector`
- **`mergeSelector(a, b)`**：合并两个 selector 字符串（去 `{}`，逗号连接）
- **`metricSelector(metric, selector)`**：`metric{selector}` 格式化

### 辅助函数
- `shouldStripImplicitMetricSourceSelector` / `sourceSelectorExplicitlyScoped` / `selectorContainsImplicitMetricSourceIdentity`
- `selectorContainsDatabaseIdentityMatcher` / `selectorContainsJobMatcher` / `selectorContainsKnownCollectedSource`
- `shouldStripInlineSourceIdentityMatchers`
- `metricRawExprContainsDatabaseMetric`
- `normalizePercentThresholdNumber`（0-1 → 0-100）
- `specBoolValue`（bool/string → bool，识别 "true"/"yes"/"explicit"/"user" 等）

## 5. 依赖关系

- **包内依赖**：
  - `promql.go`：`mergeSelectorIntoPromQL`、`readPromSelector`、`consumePromQuotedString`、`isPromQuote`、`isPromIdentStart`、`isPromIdentPart`、`skipPromSpaces`、`promQLLabelListModifier`、`promQLKeyword`、`normalizeSelectorPart`、`alertSpecSelector`、`selectorFromSpecValue`、`firstSpecString`、`firstSpecNumber`、`normalizeAlertOperator`、`formatAlertFloat`、`defaultAlertScope`
  - `regex.go`：`promMetricNameRE`、`promTrailingComparisonRE`、`promTokenRE`
  - `defaults.go`：`canonicalAlertMetric`、`isClosedSetAlertMetric`、`isDatabaseMetricName`、`firstPromMetricName`、`sanitizeRuleKey`

## 6. 并发与资源管理

- **纯函数**：无状态、无 IO、无锁
- **字符流扫描**：所有 PromQL 解析用 byte 级状态机，避免正则歧义
- **递归处理**：`sanitizeDatabaseIdentitySelectorValue` 递归处理嵌套数据结构

## 7. 设计模式与亮点

- **身份标签剥离**：核心安全设计——LLM 倾向于把示例中的 ongrid_source/device_id 写进 PromQL，会导致草案绑定到特定采集源；本文件多路径剥离（expr 内联 + spec selector 字段）
- **数据库指标特殊处理**：数据库指标（mysql_/pg_/...）的身份标签更敏感，单独识别
- **字符流状态机**：`stripLeakedMetricSourceIdentityMatchersFromPromQL` 用 byte 级扫描 + labelListDepth 跟踪，正确处理 by/without/on/ignoring 后的 label list
- **selector 合并去重**：`mergeMetricSelectorFragments` 合并时 add 覆盖 existing 同 key，避免冲突
- **多格式 selector 兼容**：spec 支持 string/[]string/[]interface{}/map[string]string/map[string]interface{} 五种格式，递归处理
- **closed-set vs 自定义分流**：closed-set 走 metric_threshold 简化路径，自定义走 metric_raw
- **expr 尾部比较补全**：LLM 可能只给 expr 不给比较，从 spec.operator/threshold 补全
- **`sourceSelectorExplicitlyScoped` 多 key 识别**：source_explicit/selector_explicit/scope_explicit 等多种 key + 值关键词（user/explicit/requested）识别用户显式 scope 意图

## 8. 注意事项

- **`stripLeakedMetricSourceIdentityMatchersFromPromQL` 不解析 `(...)` 嵌套**：只跟踪 labelListDepth（by/without 后的括号），普通函数内的 selector 也会被处理（可能是预期行为）
- **`parsePromLabelMatcherWithOperator` op 优先级**：`!=` 在 `=~`/`!~` 前检查，避免 `!=` 被误解析为 `!` + `=`
- **`isDatabaseIdentityLabel` vs `isMetricSourceIdentityLabel`**：前者含 job/service（更宽），后者不含；用途不同
- **`sanitizeDatabaseIdentitySpecSelectors` 5 个 key 硬编码**：新增 selector 字段名需手动加
- **`sourceSelectorExplicitlyScoped` 值关键词识别**：string "true"/"yes"/"explicit"/"user"/"requested"/"specified" 都视为 true；但 "false"/"no"/"implicit"/"sample" 视为 false；其他字符串不识别
- **`specBoolValue` 字符串匹配**：识别 "1"/"y"/"yes" 为 true，但 "true "（带空格）会先 trim
- **`normalizeSpecMetricCondition` 清空 spec**：转 metric_threshold 后 `in.Spec = nil`，spec 中的其他字段（labels/runbook）会丢失；但 `normalizeAlertRuleConfigInput` 在更外层处理这些字段
- **`appendMetricRawComparisonFromSpec` 双括号问题**：若 expr 已有 `(...)`，会变成 `((...)) op threshold`，语法合法但冗余
