# `defaults.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/alertdraft/defaults.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/alertdraft`

## 1. 概述

本文件提供 alertdraft 包的**指标名规范化 + 默认值生成**能力：`canonicalAlertMetric` 把 LLM/用户输入的各种指标别名（cpu/cpu_used_pct/node_cpu_seconds_total 等）统一映射到 ongrid closed-set 标准名；`isClosedSetAlertMetric` 判断指标是否属于预定义集合；`suggestedAlertRuleKey/Name/RunbookURL` 在 LLM 未提供时生成合理默认值；`sanitizeRuleKey` 把任意字符串清洗为合法 rule_key（小写 + a-z0-9 + 下划线 + 64 字符截断）；`specReferencesDatabaseMetric` 判断 spec 是否引用数据库指标。

## 2. 包信息

- **包名**：`alertdraft`
- **所属模块**：`internal/manager/biz/aiops/alertdraft`
- **依赖方向**：被 `compiler.go`、`metric_raw.go`、`spec_normalize.go`、`scope.go`、`request_hints.go` 调用；依赖 `regex.go` 的 `promTokenRE`

## 3. 关键类型与接口

无类型定义，纯函数集合。

## 4. 关键函数与流程

### `isClosedSetAlertMetric`
- **签名**：`func isClosedSetAlertMetric(metric string) bool`
- **职责**：判断 metric 是否属于 ongrid 预定义的 9 个 closed-set 指标（cpu_pct/mem_pct/disk_used_pct/disk_avail_bytes/load1/load5/load15/net_rx_bps/net_tx_bps）
- **用途**：closed-set 指标可走 `metric_threshold` 简化路径；非 closed-set 走 `metric_raw`

### `canonicalAlertMetric`
- **签名**：`func canonicalAlertMetric(metric string) string`
- **职责**：指标别名 → 标准名
- **流程**：
  1. lowercase + trim
  2. 替换 `-`/空格 为 `_`
  3. 含 `node_cpu_seconds_total` → `cpu_pct`
  4. 含 `node_filesystem_avail_bytes`/`node_filesystem_free_bytes` → `disk_avail_bytes`
  5. switch 各 closed-set 别名：
     - cpu/cpu_pct/cpu_percent/cpu_usage/cpu_used_pct/cpu_util/cpu_utilization → `cpu_pct`
     - mem/memory/mem_pct/mem_usage/mem_used_percent 等 → `mem_pct`
     - disk/disk_pct/disk_usage/disk_used/filesystem_used_percent → `disk_used_pct`
     - disk_free/disk_available/disk_avail/filesystem_available → `disk_avail_bytes`
     - load/load_1/load1/loadavg/load_avg → `load1`
     - load_5/load5 → `load5`；load_15/load15 → `load15`
     - net_rx/rx/rx_bps/network_rx/network_receive/network_in/net_in → `net_rx_bps`
     - net_tx/tx/tx_bps/network_tx/network_transmit/network_out/net_out → `net_tx_bps`
  6. default → 原样 trim 返回（非 closed-set）

### `suggestedAlertRuleKey`
- **签名**：`func suggestedAlertRuleKey(in RuleConfigInput) string`
- **职责**：生成默认 rule_key
- **流程**：
  1. 无 conditions → `suggestedAlertRuleKeyFromSpec`（按 kind 从 spec 取 metric/expr 推断）；空 → `custom_alert_rule`
  2. 有 conditions → 取首个 metric 的 canonical 名映射：
     - cpu_pct → `cpu_high`；mem_pct → `mem_high`；disk_used_pct → `disk_high`
     - disk_avail_bytes → `disk_low_available`
     - load1/load5/load15 → `<metric>_high`
     - net_rx_bps → `network_rx_high`；net_tx_bps → `network_tx_high`
     - default → `sanitizeRuleKey(metric)`

### `suggestedAlertRuleKeyFromSpec`
- **职责**：按 kind 从 spec 推断 rule_key
- **流程**：
  - metric_raw：spec.metric/metric_key/metric_name → sanitize；否则从 spec.expr 提取首个 PromQL metric 名
  - metric_anomaly/metric_forecast：`<kind>_<metric>` sanitize
  - metric_burn_rate → `slo_burn_rate`
  - log_match → `log_match`；log_volume → `log_volume`
  - trace_latency → `trace_latency_<service>` 或 `trace_latency`
  - trace_error_rate → `trace_error_rate_<service>` 或 `trace_error_rate`

### `firstPromMetricName`
- **签名**：`func firstPromMetricName(expr string) string`
- **职责**：从 PromQL 表达式提取首个 metric 名（跳过聚合函数/关键字）
- **流程**：`promTokenRE.FindAllString` 遍历 token，跳过 sum/avg/max/min/rate/increase/clamp_min/histogram_quantile/by/without/and/or/on，返回首个非关键字 token

### `specReferencesDatabaseMetric`
- **签名**：`func specReferencesDatabaseMetric(spec map[string]interface{}) bool`
- **职责**：spec 的 metric/metric_key/metric_name 是数据库指标（mysql_/pg_/postgres_/redis_/mongodb_/mongo_ 前缀）

### `sanitizeRuleKey`
- **签名**：`func sanitizeRuleKey(s string) string`
- **职责**：清洗字符串为合法 rule_key
- **流程**：
  1. lowercase + trim
  2. 遍历 rune：a-z/0-9 直写；其他字符写 `_`（连续非法字符只写一个 `_`，`lastUnderscore` 标记位）
  3. trim 首尾 `_`
  4. 空结果 → `custom_alert_rule`
  5. 长度 > 64 → 截断到 64 + trim `_`
  6. 截断后空 → `custom_alert_rule`

### `suggestedAlertRuleName` / `suggestedAlertRuleNameFromSpec`
- **职责**：生成人类可读的规则名（如 "CPU > 80%"、"Memory > 90%"、"Metric alert: node_load1"）
- **流程**：取 conditions[0] 或 spec.metric；按 metric 拼装 threshold（trim 尾部 0）；非 closed-set 用 metric + operator + threshold

### `suggestedAlertRunbookURL`
- **签名**：`func suggestedAlertRunbookURL(ruleKey string) string`
- **职责**：根据 rule_key 返回 runbook URL
- **流程**：host-metrics 类（cpu_high/mem_high/disk_high/load*_high）→ `https://github.com/ongridio/vault/blob/main/alerts/host-metrics.md#<key>`；default → `https://github.com/ongridio/vault/blob/main/concepts/alerting.md`

## 5. 依赖关系

- **内部包**：无
- **包内依赖**：`regex.go` 的 `promTokenRE`

## 6. 并发与资源管理

- **纯函数**：无状态、无 IO、无锁
- **正则使用**：`promTokenRE` 是包级常量，编译一次

## 7. 设计模式与亮点

- **别名归一化表**：`canonicalAlertMetric` 用大 switch 把数十种别名映射到 9 个 closed-set 名，覆盖中英文/缩写/PromQL 原始名
- **node_exporter 指标识别**：含 `node_cpu_seconds_total`/`node_filesystem_avail_bytes` 等 PromQL 原始名时直接映射，免去 LLM 转换
- **closed-set 概念**：明确区分"ongrid 预定义指标"和"自定义指标"，前者走简化 metric_threshold 路径
- **rule_key 三级 fallback**：conditions → spec.metric → spec.expr 提取 → `custom_alert_rule`
- **sanitize 保守策略**：连续非法字符只写一个 `_`，避免 `__` 重复；64 字符截断后再 trim `_`
- **runbook URL 锚点**：host-metrics 类带 `#<key>` 锚点直达章节
- **数据库指标前缀识别**：mysql_/pg_/redis_/mongodb_ 前缀，用于 scope 推断

## 8. 注意事项

- **`canonicalAlertMetric` 不识别非英文别名**：如中文"CPU 使用率"不会被映射；需 LLM 先翻译
- **`firstPromMetricName` 关键字表硬编码**：新增 PromQL 函数（如 `predict_linear`）需手动加入跳过表，否则会被当 metric 名
- **`sanitizeRuleKey` 64 字符截断**：截断点可能在单词中间，可读性下降；但 rule_key 是机器索引，不影响功能
- **`suggestedAlertRuleName` threshold 格式化**：`%.2f` trim 尾部 0，`80.00` → `80`，`80.50` → `80.5`
- **`suggestedAlertRunbookURL` 硬编码 URL**：仓库迁移需同步更新
- **closed-set 列表与 `canonicalAlertMetric` switch 必须同步**：新增 closed-set 指标需同时改两处
