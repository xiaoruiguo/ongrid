# `scope.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/alertdraft/scope.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/alertdraft`

## 1. 概述

本文件实现 alertdraft 包的**告警作用域推断**：`HostScopeRecommended` 是核心公开函数，根据 rule 结构 + 用户请求文本判断是否应使用 host 作用域（而非 global）。被 `compiler.go` 的 `normalizeAlertRuleConfigInputForRequest` 和 `alertconfig/draft_validation.go` 的 `validateScopeConsistency` 复用。判断逻辑：kind 不支持 host → false；显式 global 请求 → false；closed-set host metric / 数据库 metric host scoped / log host scoped / metric_raw host scoped → true。

## 2. 包信息

- **包名**：`alertdraft`
- **所属模块**：`internal/manager/biz/aiops/alertdraft`
- **依赖方向**：被 `compiler.go`、`alertconfig/draft_validation.go` 调用；依赖 `defaults.go`（`isClosedSetAlertMetric`/`canonicalAlertMetric`/`isDatabaseMetricName`）、`metric_raw.go`（`metricRawExprContainsDatabaseMetric`/`firstSpecString`）

## 3. 关键类型与接口

无类型定义。

## 4. 关键函数与流程

### `normalizeAlertScopeForRequest`
- **签名**：`func normalizeAlertScopeForRequest(in RuleConfigInput, requestText string) RuleConfigInput`
- **职责**：若 `HostScopeRecommended` true → 强制 scope_type=host
- **流程**：单行 if 判断 + 赋值

### `HostScopeRecommended`
- **签名**：`func HostScopeRecommended(in RuleConfigInput, requestText string) bool`
- **职责**：核心判断函数，决定是否推荐 host 作用域
- **流程**（短路 OR）：
  1. `kindCanUseHostScope(in.Kind)` false → false（log_volume/trace_*/metric_burn_rate 不支持 host）
  2. `in.ScopeType == "monitoring_pipeline"` → false（pipeline 作用域优先）
  3. `explicitGlobalScopeRequested(requestText)` 且非 `explicitHostScopeRequested` → false（用户显式要 global）
  4. `ruleUsesClosedSetHostMetric(in)` → true（closed-set 指标天然 host 维度）
  5. `databaseRuleLooksHostScoped(in, requestText)` → true（数据库指标 + 请求提及数据库）
  6. `logRuleLooksHostScoped(in, requestText)` → true（log + journald + 主机资源请求）
  7. `metricRawLooksHostScoped(in, requestText)` → true（metric_raw + device_id/node_*/host_*）

### `kindCanUseHostScope`
- **职责**：kind 是否支持 host 作用域
- **支持**：metric_threshold/metric_raw/metric_anomaly/metric_forecast/log_match/log_volume
- **不支持**：trace_latency/trace_error_rate/metric_burn_rate（服务级，非主机级）

### `ruleUsesClosedSetHostMetric`
- **职责**：rule 是否使用 closed-set host metric
- **流程**：有 conditions → true；spec.metric 是 closed-set → true

### `databaseRuleLooksHostScoped`
- **职责**：数据库规则看起来是 host scoped
- **流程**：
  1. `requestLooksDatabaseScoped(requestText)` false → false
  2. spec.metric 是数据库指标名 → true
  3. spec.expr 含数据库指标 → true

### `metricRawLooksHostScoped`
- **职责**：metric_raw 表达式看起来是 host scoped
- **流程**：
  1. kind 非 metric_raw → false
  2. expr 空 → false
  3. exprLower 含 "device_id" → true
  4. exprLower 含 node_/process_/disk_/filesystem_/host_ 且 `requestLooksHostResourceScoped` → true

### `logRuleLooksHostScoped`
- **职责**：log 规则看起来是 host scoped
- **流程**：
  1. kind 非 log_match/log_volume → false
  2. stream_selector 含 "device_id" → true
  3. stream_selector 含 "journald" 且 `requestLooksHostResourceScoped` → true

### `explicitHostScopeRequested`
- **职责**：请求文本显式提及主机
- **关键词**：主机/机器/节点/服务器/每台/各台/单台/某台/按主机/按机器/按节点 + host/node/server/machine/device

### `explicitGlobalScopeRequested`
- **职责**：请求文本显式提及全局
- **关键词**：全局/整体/集群/汇总/总量/总数/服务级/系统整体 + global/overall/aggregate/aggregated/cluster/fleet/service-level

### `requestLooksHostResourceScoped`
- **职责**：请求文本看起来是主机资源范围
- **流程**：`explicitHostScopeRequested` OR 含 cpu/内存/mem/磁盘/硬盘/分区/根分区/文件系统/disk/filesystem/mountpoint/load/负载/swap/网络/网卡/network/interface/系统日志/journald/syslog

### `requestLooksDatabaseScoped`
- **职责**：请求文本看起来是数据库范围
- **关键词**：mysql/postgres/postgresql/redis/mongodb/mongo/数据库/慢查询/连接数/sql

### `containsAny`
- **签名**：`func containsAny(s string, needles []string) bool`
- **职责**：s 是否含 needles 中任一子串

## 5. 依赖关系

- **包内依赖**：
  - `defaults.go`：`isClosedSetAlertMetric`、`canonicalAlertMetric`、`isDatabaseMetricName`
  - `metric_raw.go`：`metricRawExprContainsDatabaseMetric`、`firstSpecString`（注：`firstSpecString` 实际在 promql.go 定义）
- **外部库**：标准库 `strings`

## 6. 并发与资源管理

- **纯函数**：无状态、无 IO、无锁
- **无 ctx 参数**：纯计算

## 7. 设计模式与亮点

- **短路 OR 链**：`HostScopeRecommended` 用 7 个判断短路返回，优先级清晰
- **kind 白名单**：`kindCanUseHostScope` 明确哪些 kind 支持 host 作用域，避免误推
- **用户意图优先**：`explicitGlobalScopeRequested` 且非 `explicitHostScopeRequested` → false，尊重用户显式 global 意图
- **多维度信号融合**：closed-set metric / 数据库 metric / log journald / metric_raw device_id 多路径判断
- **中英文关键词覆盖**：host/node/server/machine + 主机/机器/节点/服务器，覆盖中英文用户输入
- **monitoring_pipeline 优先**：已标 monitoring_pipeline 的规则不强制改 host
- **数据库指标特判**：`databaseRuleLooksHostScoped` 仅在请求提及数据库时才推荐 host（数据库指标也可能是服务级）

## 8. 注意事项

- **`HostScopeRecommended` 是推荐而非强制**：`normalizeAlertScopeForRequest` 会强制改 host，但 `validateScopeConsistency`（alertconfig）仅 warning
- **`explicitGlobalScopeRequested` 与 `explicitHostScopeRequested` 不互斥**：用户可能同时提及"全局"和"主机"，此时 host 优先（因判断 3 要求"global 且非 host"）
- **`metricRawLooksHostScoped` 关键词宽泛**：`node_`/`process_`/`disk_` 等前缀可能匹配非 host 指标
- **`logRuleLooksHostScoped` 仅识别 journald**：其他日志源（如 syslog）不识别
- **`requestLooksDatabaseScoped` 关键词硬编码**：新增数据库类型需手动加
- **`containsAny` 子串匹配**：`"mongo"` 会匹配 `"mongodb"`，但也可能匹配 `"mongolia"` 等无关词
- **无 spec 检查兜底**：spec nil 时 `ruleUsesClosedSetHostMetric` 返回 false，进入下游判断
