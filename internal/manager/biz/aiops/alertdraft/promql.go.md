# `promql.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/alertdraft/promql.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/alertdraft`

## 1. 概述

本文件是 alertdraft 包的 **PromQL 解析与重写工具集**，被 `metric_raw.go`/`spec_normalize.go`/`request_hints.go` 复用。核心能力：(1) `mergeSelectorIntoPromQL` 把 spec selector 合并进 PromQL 表达式的每个 metric；(2) `rewriteSimpleHostMetricRawExpr`/`rewriteFriendlyHostMetricPromQL` 把友好指标名（cpu/mem/disk）重写为 node_exporter 原始 PromQL；(3) `parseSimpleHostMetricPredicate` 解析 `cpu > 80 and mem > 90` 这类简单谓词链；(4) 大量 selector 解析辅助（`readPromSelector`/`consumePromQuotedString`/`splitPromSelectorMatchers`）和 spec 值提取辅助（`firstSpecString`/`firstSpecNumber`/`alertSpecSelector`）。

## 2. 包信息

- **包名**：`alertdraft`
- **所属模块**：`internal/manager/biz/aiops/alertdraft`
- **依赖方向**：被 `metric_raw.go`、`spec_normalize.go`、`request_hints.go`、`compiler.go`、`defaults.go`、`scope.go` 调用；依赖 `regex.go`（`simpleHostMetricPredicateJoinRE`/`simpleHostMetricPredicatePartRE`/`promMetricNameRE`/`promTrailingComparisonRE`/`promTokenRE`）、`defaults.go`（`canonicalAlertMetric`/`isClosedSetAlertMetric`）

## 3. 关键类型与接口

无新类型定义。

## 4. 关键函数与流程

### `mergeSelectorIntoPromQL`
- **签名**：`func mergeSelectorIntoPromQL(expr, selector string) (string, bool)`
- **职责**：把 selector 合并进 expr 中每个 metric 的 selector
- **流程**（字符流状态机，与 `stripLeakedMetricSourceIdentityMatchersFromPromQL` 同构）：
  1. 跳过引号字符串
  2. 跟踪 labelListDepth（by/without/on/ignoring 后的 `(...)`）
  3. 识别 identifier：
     - 后跟 `(` → 函数，原样输出
     - 后跟 `{` → `readPromSelector` 读取 → `mergeMetricSelectorFragments(existing, add)` 合并（add 覆盖 existing 同 key）
     - 无 selector → `metricSelector(token, selector)` 补上
  4. label list modifier（by/without/on/ignoring/group_left/group_right）→ 标记 skipNextLabelList
  5. promQLKeyword（sum/avg/and/or/...）→ 原样输出

### `mergeMetricSelectorFragments`
- **职责**：合并两个 selector 字符串，add 中的 key 覆盖 existing 中的同 key
- **流程**：拆分两边 → 收集 addKeys → existing 中跳过 addKeys 的项 → append addParts → `{...}` 重组

### `rewriteSimpleHostMetricRawExpr`
- **签名**：`func rewriteSimpleHostMetricRawExpr(in RuleConfigInput) RuleConfigInput`
- **职责**：把简单谓词（`cpu > 80`）转 metric_threshold；把友好 metric 名（cpu/mem/disk）的 metric_raw expr 重写为 node_exporter PromQL
- **流程**：
  1. 有 conditions → 跳过
  2. kind 非 metric_raw 且非空 → 跳过
  3. spec.expr 不存在 → 跳过
  4. `parseSimpleHostMetricPredicate(expr)`：
     - 成功 → kind=metric_threshold + conditions + joinMode + 清空 spec
     - 失败 → `rewriteFriendlyHostMetricPromQL` 重写友好名

### `parseSimpleHostMetricPredicate`
- **签名**：`func parseSimpleHostMetricPredicate(expr string) ([]RuleCondition, string, bool)`
- **职责**：解析 `cpu > 80 and mem > 90` 这类简单谓词链
- **流程**：
  1. 含 `{}`/`[]` → 失败（太复杂）
  2. `simpleHostMetricPredicateJoinRE` 匹配 `and`/`or` 连接符 → joinMode（or→any，and→all）
  3. 拆分 parts → 每个 part 用 `simpleHostMetricPredicatePartRE` 匹配 `(metric op threshold)`
  4. metric 必须 `isClosedSetAlertMetric(canonicalAlertMetric(...))`
  5. 返回 conditions + joinMode

### `rewriteFriendlyHostMetricPromQL`
- **签名**：`func rewriteFriendlyHostMetricPromQL(expr string) (string, bool)`
- **职责**：扫描 expr，把友好 metric 名替换为 node_exporter PromQL
- **流程**（字符流扫描）：
  1. 跳过引号、selector `{...}`
  2. 识别 identifier：
     - 后跟 `[` → range selector，原样保留
     - 否则 `friendlyHostMetricPromQL(token, selector)` 重写
  3. 重写成功 → `(<replacement>)` 包裹

### `friendlyHostMetricPromQL`
- **签名**：`func friendlyHostMetricPromQL(metric, selector string) (string, bool)`
- **职责**：友好 metric 名 → node_exporter PromQL 模板
- **映射**：
  - cpu_pct → `100 * (1 - avg by (device_id) (rate(node_cpu_seconds_total{selector,mode="idle"}[5m])))`
  - mem_pct → `100 * (1 - node_memory_MemAvailable_bytes{selector} / node_memory_MemTotal_bytes{selector})`
  - disk_used_pct → `100 * (1 - node_filesystem_avail_bytes{selector} / node_filesystem_size_bytes{selector})`
  - disk_avail_bytes → `node_filesystem_avail_bytes{selector}`
  - load1/load5/load15 → `node_load1/5/15{selector}`
  - net_rx_bps → `sum by (device_id) (rate(node_network_receive_bytes_total{selector}[1m]))`
  - net_tx_bps → `sum by (device_id) (rate(node_network_transmit_bytes_total{selector}[1m]))`

### `readPromSelector`
- **签名**：`func readPromSelector(expr string, start int) (string, int, bool)`
- **职责**：从 `start` 位置读取完整 `{...}` selector（处理嵌套 `{}` + 引号 + 转义）
- **返回**：selector 字符串 + 结束位置 + 是否成功

### `consumePromQuotedString`
- **签名**：`func consumePromQuotedString(expr string, start int) int`
- **职责**：从 `start`（引号位置）读取完整引号字符串，返回结束位置+1
- **流程**：处理 `\\` 转义；匹配同种引号闭合

### `isPromQuote` / `isPromIdentStart` / `isPromIdentPart` / `skipPromSpaces`
- **`isPromQuote`**：`"`/`'`/`` ` ``
- **`isPromIdentStart`**：a-z/A-Z/`_`/`:`
- **`isPromIdentPart`**：identStart + 0-9
- **`skipPromSpaces`**：跳过空格/制表/换行

### `promQLKeyword` / `promQLLabelListModifier`
- **`promQLKeyword`**：and/or/unless/bool/offset + 聚合函数 sum/avg/min/max/count/group/stddev/stdvar/topk/bottomk/quantile/count_values
- **`promQLLabelListModifier`**：by/without/on/ignoring/group_left/group_right

### `alertSpecSelector` / `selectorFromSpecValue` / `selectorFromSpecLabels`
- **`alertSpecSelector`**：从 spec 取 selector（按优先级尝试 selector/label_selector/matchers/labels/match_labels）
- **`selectorFromSpecValue`**：处理 string/[]string/[]interface{} → 拼接 selector
- **`selectorFromSpecLabels`**：map → `key="value"` 排序拼接

### `normalizeSelectorPart`
- **职责**：trim + 去首尾 `{}`

### `firstSpecString` / `firstSpecNumber` / `alertSpecStringValue` / `hasAnySpecKey`
- **`firstSpecString`**：按 keys 顺序取首个非空字符串
- **`firstSpecNumber`**：支持 float64/float32/int/int64/int32/json.Number/string 多类型
- **`alertSpecStringValue`**：string 或 fmt.Stringer
- **`hasAnySpecKey`**：spec 是否含任一 key

### `setSpecDefaultString` / `setSpecDefaultNumber`
- **职责**：spec 字段为空时设默认值

### `normalizeAlertOperator` / `formatAlertFloat` / `defaultAlertScope`
- **`normalizeAlertOperator`**：操作符规范化（`=`/`eq` → `==`；`gte`/`=>` → `>=`；default → `>`）
- **`formatAlertFloat`**：`strconv.FormatFloat(n, 'f', -1, 64)`（最短表示）
- **`defaultAlertScope`**：metric_threshold/metric_anomaly/metric_forecast → host；其他 → global

### `alertSpecString`
- **签名**：`func alertSpecString(spec map[string]interface{}, key string) (string, bool)`
- **职责**：strict string 取值（必须是 string 类型 + trim 后非空）

## 5. 依赖关系

- **包内依赖**：
  - `regex.go`：`simpleHostMetricPredicateJoinRE`、`simpleHostMetricPredicatePartRE`、`promMetricNameRE`、`promTrailingComparisonRE`、`promTokenRE`、`nodeFilesystemMetricSelectorRE`、`promSimpleRangeSelectorRE`
  - `defaults.go`：`canonicalAlertMetric`、`isClosedSetAlertMetric`
- **外部库**：标准库 `encoding/json`、`fmt`、`sort`、`strconv`、`strings`

## 6. 并发与资源管理

- **纯函数**：无状态、无 IO、无锁
- **字符流扫描**：所有 PromQL 解析用 byte 级状态机
- **正则常量**：包级 var，编译一次

## 7. 设计模式与亮点

- **字符流状态机模式**：`mergeSelectorIntoPromQL`/`rewriteFriendlyHostMetricPromQL`/`stripLeakedMetricSourceIdentityMatchersFromPromQL` 三兄弟同构，共用 quote/ident/labelListDepth 处理逻辑
- **友好名 → PromQL 模板表**：`friendlyHostMetricPromQL` 用 switch 把 9 个友好名映射到 node_exporter PromQL，含 `device_id` by 标签保留
- **简单谓词解析**：`parseSimpleHostMetricPredicate` 识别 `cpu > 80 and mem > 90` 链式谓词，转 metric_threshold
- **selector 合并去重**：`mergeMetricSelectorFragments` add 覆盖 existing，避免 label 冲突
- **多类型数值提取**：`firstSpecNumber` 兼容 float64/float32/int/int64/int32/json.Number/string
- **operator 别名归一**：`=`/`eq` → `==`；`gte`/`=>` → `>=`；覆盖中英文缩写
- **range selector 保留**：`rewriteFriendlyHostMetricPromQL` 遇到 `[...]` 原样保留，避免误重写

## 8. 注意事项

- **`mergeSelectorIntoPromQL` 不解析 `(...)` 嵌套**：只跟踪 labelListDepth，函数内的 selector 也会被合并（可能是预期行为）
- **`readPromSelector` 不处理嵌套 `{}`**：depth 计数，但 PromQL selector 通常不嵌套
- **`consumePromQuotedString` 不处理跨行**：PromQL 通常单行
- **`friendlyHostMetricPromQL` 硬编码 `device_id` by 标签**：ongrid 平台特有，其他平台需调整
- **`parseSimpleHostMetricPredicate` 拒绝 `{}`/`[]`**：复杂 PromQL 不走此路径，回退到 `rewriteFriendlyHostMetricPromQL`
- **`normalizeAlertOperator` default `>`**：未知操作符默认 `>`，可能改变语义；但 LLM 通常生成标准操作符
- **`firstSpecNumber` json.Number 处理**：仅 `json.Unmarshal` 用 `UseNumber` 时才会出现 json.Number
- **`alertSpecString` strict 类型**：非 string 返回 false，与 `firstSpecString` 的 `alertSpecStringValue`（支持 Stringer）不同
