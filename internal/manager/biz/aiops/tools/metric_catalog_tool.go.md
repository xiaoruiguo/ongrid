# `metric_catalog_tool.go` 技术实现文档

> 源文件：`internal/manager\biz\aiops\tools\metric_catalog_tool.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件实现 `list_metric_catalog` 工具（BaseTool + 闭包路径双形态）：列出当前被 Prometheus 抓取的 metric 名 + 代表性 label。是 `draft_config_change` 的前置工具——创建 metric-based alert rule 前先用本工具拿到当前 metric 名与 label key，让 draft 能用真实 metric。通过 `count by (...) ({__name__=~"...", ...})` PromQL 一次拉所有 series 数，Go 端聚合 + 按 natural-language query 打分排序。支持 `prefixes` / `metric_regex` / `selector` / `labels` 过滤，`max_metrics` 默认 80 cap 200，`include_label_samples` 默认 true（每 metric 最多 3 个 sample label）。**HTTP status label 检测**：query 含 "error rate"/"5xx"/"slo" 时检查返回 metric 是否暴露 status/code label，否则生成 instruction 提示 LLM 不要臆造 label。复用 `queryPromqlCallTimeout`。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 Registry BaseTool 路径 + 闭包路径调用；依赖 `basetool`、`PromQuerier`（`Query`）、`regexp`、`sort`、`unicode`。与 `query_promql.go` 共享 `queryPromqlCallTimeout` / `instantValues` / `promInstantValue`。

## 3. 关键类型与接口

```go
type ListMetricCatalogTool struct {
    promQuery PromQuerier
    log       *slog.Logger
}

type ListMetricCatalogArgs struct {
    Query               string                 `json:"query,omitempty"`          // natural-language hint
    Prefixes            []string               `json:"prefixes,omitempty"`        // ['mysql_', 'pg_']
    MetricRegex         string                 `json:"metric_regex,omitempty"`    // RE2，不能匹配空串
    Selector            string                 `json:"selector,omitempty"`        // 'job="api"' 或 '{device_id="5"}'
    Labels              map[string]interface{} `json:"labels,omitempty"`          // {'device_id': 5}
    MaxMetrics          int                    `json:"max_metrics,omitempty"`      // 默认 80，cap 200
    IncludeLabelSamples *bool                  `json:"include_label_samples,omitempty"` // 默认 true
}

type MetricCatalogResponse struct {
    Status, Instruction string
    GeneratedAt         time.Time
    Query, Selector     string
    PromQL              string  // 实际执行的 PromQL
    MetricCount, Returned int   // 总匹配 / 实际返回
    Truncated           bool
    Metrics             []MetricCatalogItem
}

type MetricCatalogItem struct {
    Name         string              `json:"name"`
    SeriesCount  int                 `json:"series_count,omitempty"`
    SampleLabels []map[string]string `json:"sample_labels,omitempty"` // 最多 3 个
}

const (
    ToolNameListMetricCatalog   = "list_metric_catalog"
    metricCatalogMaxSampleLabels = 3
)
```

内部类型 `metricCatalogRunner`（封装 run 逻辑，让 BaseTool 与闭包路径共用）、`scoredMetricCatalogItem`（打分排序用）。

## 4. 关键函数与流程

```go
func NewListMetricCatalogTool(p PromQuerier, log *slog.Logger) *ListMetricCatalogTool
func (t *ListMetricCatalogTool) Info(_ context.Context) (*basetool.ToolInfo, error)
func (t *ListMetricCatalogTool) InvokableRun(ctx, argsJSON, _ ...InvokeOption) (string, error)
func (r *Registry) executeListMetricCatalog(ctx, args json.RawMessage) (ExecuteResult, error)
func (r metricCatalogRunner) run(ctx, args []byte) ([]byte, error)

// 辅助函数（节选）：
func metricCatalogNameRegex(metricRegex string, prefixes []string) (*regexp.Regexp, string, error)
func metricCatalogSelector(nameRegex, selector string, labels map[string]string) string
func metricCatalogGroupLabels() []string  // __name__ + device_id + edge_id + ongrid_source + job + instance + service + namespace + pod + dimension labels
func metricCatalogDimensionLabels() []string  // datname/db/conn_type/.../method/status/code/le
func aggregateMetricCatalog(vals, nameRE, exactLabels, includeSamples) []MetricCatalogItem
func chooseMetricCatalogSampleLabels(metric, candidates, limit) []map[string]string
func metricCatalogSamplePriority(metric, labels) int  // 优先 mountpoint=/, conn_type=current 等
func metricCatalogQueryTokens(q string) []string  // 分词 + stop words 过滤
func metricCatalogQueryAliases(q string) []string  // mysql/pg/redis/mongodb/connection/usage/latency/... 别名
func metricCatalogScore(name string, tokens, aliases []string) int
func metricCatalogNeedsHTTPStatusLabel(query string) bool  // error rate/5xx/slo → true
func metricCatalogItemsHaveAnySampleLabel(items, labels ...string) bool
```

**`metricCatalogRunner.run` 流程**：
1. 守门 `promQuery != nil`。
2. Unmarshal → `ListMetricCatalogArgs`；`maxMetrics` 默认 80，cap 200；`includeSamples` 默认 true。
3. `metricCatalogNameRegex(MetricRegex, Prefixes)` → `nameRE` + `nameREString`。`MetricRegex` 非空时编译 RE2，校验不匹配空串，转 Prom regex；否则用 `Prefixes` 构造 `^(prefix1|prefix2).*`，无 Prefixes 则 `.+`。
4. `exactLabels := normalizeMetricCatalogLabels(Labels)`（`float64` / `bool` / `string` / `nil` 转 string）。
5. `selector := metricCatalogSelector(nameREString, Selector, exactLabels)` 拼接 `__name__=~"..."` + selector + labels。
6. `expr := fmt.Sprintf("count by (%s) ({%s})", join(groupLabels, ", "), selector)`。
7. `context.WithTimeout(ctx, queryPromqlCallTimeout)`；`promQuery.Query(callCtx, expr, time.Now())`。
8. `aggregateMetricCatalog(instantValues(res), nameRE, exactLabels, includeSamples)` → `items`。聚合：按 `__name__` 分组，累加 `SeriesCount`（`count by` 返回的 value），收集 sample label candidates。
9. `tokens := metricCatalogQueryTokens(Query)` + `aliases := metricCatalogQueryAliases(Query)`。
10. 打分排序：`metricCatalogScore(name, tokens, aliases)`——`tokens` 命名包含 +2，`aliases` 命名包含 +3，`up` metric +1。按 score 降序，同 score 按 name 升序。
11. 取 `maxMetrics` 个 → `outItems`。
12. **Instruction 生成**：
    - `len(items) == 0` → `status="empty"`，instruction 提示 "Do not invent metric names. ... ask the user to configure collection or provide an exact metric/PromQL."
    - `metricCatalogNeedsHTTPStatusLabel(Query) && !metricCatalogItemsHaveAnySampleLabel(outItems, httpStatusLabels...)` → instruction 提示 "metrics do not expose any HTTP status/code label ... Do not invent missing labels"。
13. 构造 `MetricCatalogResponse{Status, Instruction, GeneratedAt, Query, Selector, PromQL: expr, MetricCount: len(items), Returned: len(outItems), Truncated, Metrics: outItems}`，Marshal 返回。

## 5. 依赖关系

- **basetool**：`ToolInfo`（`Class="read"`）、`InvokeOption`（忽略）。
- **PromQuerier**：`Query(ctx, expr, time)`。`queryPromqlCallTimeout` 来自 `query_promql.go`。
- **instantValues / promInstantValue**（`query_promql.go`）：解码 Prom instant vector。
- **regexp / sort / unicode / math / strconv**：标准库。
- 不依赖 alertbiz / devicebiz / edgebiz / log / trace / tunnel。

## 6. 并发与资源管理

- **无锁、无共享可变状态**：`ListMetricCatalogTool` 仅持有不变 `promQuery` 指针，`metricCatalogRunner` 是 value type，多 goroutine 可并发调用。
- **无 goroutine**：单次 `promQuery.Query` + Go 端聚合排序，`queryPromqlCallTimeout` 覆盖。
- **`metricCatalogRunner` value type**：每次 `run` 创建新 runner，无共享状态。

## 7. 设计模式与亮点

- **`count by (...) ({...})` 一次拉全**：用 PromQL `count by` 让 Prom 端聚合 series 数，Go 端只需解码 instant vector，省带宽。`groupLabels` 含 `__name__` + device_id + edge_id + ongrid_source + job + instance + service + namespace + pod + dimension labels（datname/db/conn_type/method/status/code/le 等），覆盖数据库 exporter 与 HTTP histogram 常见维度。
- **Natural-language 打分排序**：`metricCatalogQueryTokens` 分词 + stop words 过滤；`metricCatalogQueryAliases` 把 "mysql"/"connection"/"usage"/"latency"/"error"/"cache"/"replication" 等自然语言词映射到 metric 名子串别名（含中文 "连接"/"使用率"/"延迟"/"错误"/"缓存"/"命中率"/"复制"/"主从"）；`metricCatalogScore` 按 token +2 / alias +3 打分。让 "MySQL connection usage" 排到 `mysql_threads_connected` 前面。
- **Sample label 优先级**：`metricCatalogSamplePriority` 给 `node_filesystem_*` 的 `mountpoint=/` +100、`/var`/`/home` +60；`mongodb_ss_connections` 的 `conn_type=current` +100、`available` +90；`count_type=total` +20；`state=current` +20。让 sample label 展示最有信息量的组合，而非随机 3 个。
- **HTTP status label 检测**：`metricCatalogNeedsHTTPStatusLabel` 识别 "error rate"/"5xx"/"slo"/"错误率"/"错误预算" 等 query；若返回 metric 无 status/code label，生成 instruction 提示 LLM "Do not invent missing labels"。这是防 LLM 臆造 label 的 guardrail。
- **`Instruction` 而非 error**：empty 结果或缺 label 时用 `Instruction` 字段提示 LLM 下一步，不破坏响应结构。LLM 能据此判断 "要不要问用户要 exact PromQL" 或 "要不要 draft_config_change 让 validation 校验"。
- **`metricCatalogNameRegex` 双模式**：`MetricRegex` 非空时用 RE2（校验不匹配空串防 Prom 全匹配）；否则用 `Prefixes` 构造 `^(prefix1|prefix2).*`。两模式都转 Prom regex 形式。
- **`metricCatalogGroupLabels` 含 dimension labels**：`datname`/`db`/`conn_type`/`count_type`/`state`/`command`/`operation`/`role`/`mode`/`mountpoint`/`fstype`/`device`/`method`/`status`/`code`/`status_code`/`http_status_code`/`http_response_status_code`/`response_code`/`status_class`/`le`。覆盖数据库 exporter 与 HTTP histogram 常见维度，让 `count by` 保留这些 label 让 LLM 看到。
- **BaseTool + 闭包双形态共用 runner**：`metricCatalogRunner` 封装 run 逻辑，`InvokableRun` 与 `executeListMetricCatalog` 都代理调用，避免 drift。
- **`WhenToUse` 引导 draft_config_change**：明示 "Use returned names and label keys as evidence for draft_config_change; call analyze_database_status afterwards only if source or capability context is still needed. NOT for executing PromQL values or trends; use query_promql for raw metric values after the metric name is known."

## 8. 注意事项

- **`metric_regex` 不能匹配空串**：`re.MatchString("")` 命中则报错 "must not match the empty string"，防 Prom 全匹配返回海量 series。
- **`Labels` 值类型转换**：`normalizeMetricCatalogLabels` 处理 `float64`（整数 vs 小数）/ `bool` / `string` / `nil`（跳过）/ 其他（`fmt.Sprint`）。LLM 传 `{"device_id": 5}` 会被转 `{"device_id": "5"}`。
- **`Selector` 容错**：`normalizeMetricCatalogSelectorPart` 去掉首尾 `{` `}` 与空格，让 LLM 传 `'job="api"'` 或 `'{job="api"}'` 都能工作。
- **`max_metrics` cap 200**：超过 200 强制截断。`Truncated` 字段告知 LLM 是否有更多。
- **`SeriesCount` 来自 `count by` value**：`aggregateMetricCatalog` 累加 `row.Value`（`count by` 返回的 series 数）。`NaN`/`Inf`/负值跳过。这反映 "该 metric 在 Prom 里有多少 series"，让 LLM 判断 metric 活跃度。
- **`groupLabels` 列表硬编码**：新增 exporter 维度需手动加。当前覆盖 node_exporter / mysql_exporter / postgres_exporter / redis_exporter / mongodb_exporter / HTTP histogram 常见 label。
- **打分启发式**：`tokens` / `aliases` 命名包含打分，不完美。如 "mysql connection" 会让 `mysql_threads_connected` 与 `mysql_global_status_threads_connected` 都高分，LLM 需自行判断。
- **`metricCatalogStopWords` 列表**：`alert`/`rule`/`create`/`prometheus`/`metric`/`custom` 等 stop words 被过滤，不参与打分。新增 stop word 需手动加。
- **`Instruction` 是 LLM-facing**：LLM 应遵循 instruction（"Do not invent metric names" / "ask the user"），但 LLM 可能忽略。draft_config_change 的 validation 是最后防线。
- **`queryPromqlCallTimeout` 复用**：与 `query_promql` 共享超时常量。若 `count by` 查询慢（海量 series），可能超时。
