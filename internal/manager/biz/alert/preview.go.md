# `preview.go` 技术实现文档

> 源文件：`internal/manager/biz/alert/preview.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/alert`

## 1. 概述

本文件是规则编辑器「试算」按钮的只读 side-channel：`PreviewRule` 校验 + 编译输入规则（复用 `buildRuleRow` 同一代码路径），然后按 kind 分派到 per-kind previewer，跑 Prom/Loki range 查询，返回 `PreviewResult`（fire_count、首末 fire 时间、最近 5 条 samples、代表 series 时间序列点、阈值参考线、单位）。永不持久化——`RuleKey` 可为空 / scratch。per-kind previewer 复用 `walkPromMatrix` / `walkLokiMatrix` 矩阵遍历器，对每个 (ts, labels, value) 用 `keep` 谓词判定是否 fire，用 `fmtSummary` 渲染文案。`pickRepresentativeSeries` 选择点数最多（平手取均值最高）的 series 作为图表代表线，`downsampleSeries` 用 stride 降采样到 ≤1500 点。

## 2. 包信息

- **包名**：`alert`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 HTTP handler 调用；依赖 `internal/manager/model/alert` + `internal/pkg/logquery` + `internal/pkg/promquery`

## 3. 关键类型与接口

```go
type PreviewInput struct {
    Input           RuleInput
    LookbackSeconds int
}

type PreviewSample struct {
    Timestamp time.Time         `json:"ts"`
    Labels    map[string]string `json:"labels,omitempty"`
    Value     float64           `json:"value"`
    Summary   string            `json:"summary"`
}

type PreviewSeriesPoint struct {
    Timestamp time.Time `json:"ts"`
    Value     float64   `json:"value"`
}

type PreviewResult struct {
    FireCount     int                 `json:"fire_count"`
    FirstFireAt   *time.Time          `json:"first_fire_at,omitempty"`
    LastFireAt    *time.Time          `json:"last_fire_at,omitempty"`
    Samples       []PreviewSample     `json:"samples,omitempty"`
    SkippedReason string              `json:"skipped_reason,omitempty"`
    Series        []PreviewSeriesPoint `json:"series,omitempty"`
    Threshold     *float64            `json:"threshold,omitempty"`
    Unit          string              `json:"unit,omitempty"`
}

type PreviewPromQuerier interface {
    QueryRange(ctx, expr string, start, end time.Time, step time.Duration) (*promquery.InstantResult, error)
}
type PreviewLogQuerier interface {
    QueryRange(ctx, opts logquery.QueryRangeOptions) (*logquery.QueryRangeResult, error)
}

type PreviewDeps struct {
    Prom PreviewPromQuerier
    Log  PreviewLogQuerier
    Now  func() time.Time
}

var metricRawComparisonRE = regexp.MustCompile(`^(.+?)\s*(==|!=|>=?|<=?)\s*([+-]?[0-9.]+(?:[eE][+-]?\d+)?)\s*$`)

const (
    maxPreviewSeriesPoints        = 1500
    defaultPreviewLookbackSeconds = 86400
    minPreviewLookbackSeconds     = 60
    maxPreviewLookbackSeconds     = 604800
)
```

## 4. 关键函数与流程

### `PreviewRule`

- **签名**：`func PreviewRule(ctx context.Context, in PreviewInput, deps PreviewDeps) (*PreviewResult, error)`
- **职责**：试算主入口；校验+编译+分派
- **流程**：
  1. `buildRuleRow(in.Input, false)` 校验+编译；err → return
  2. `!IsKnownKind(row.Kind)` → err
  3. 填占位（RuleKey=`__preview__`、Name=`preview`、Severity=`info`）让 compile 不绊脚
  4. `now := deps.Now() ?? time.Now().UTC()`
  5. `lookback := normalizePreviewLookbackSeconds(in.LookbackSeconds)`
  6. `start := now.Add(-lookback * Second)`
  7. switch row.Kind 分派到 `previewMetricAnomaly` / `previewMetricForecast` / `previewMetricBurnRate` / `previewMetricRaw` / `previewLogMatch` / `previewLogVolume` / `previewTraceLatency` / `previewTraceErrorRate`
  8. 未知 kind → `SkippedReason: "kind not supported by preview"`
- **错误处理**：buildRuleRow 失败 return err；per-kind 内部失败返回 `SkippedReason`（不抛错）

### `normalizePreviewLookbackSeconds`

- **签名**：`func normalizePreviewLookbackSeconds(lookback int) int`
- **职责**：钳制 lookback 到 [60, 604800]；默认 86400（24h）

### `walkPromMatrix`

- **签名**：`func walkPromMatrix(ctx, prom, expr, start, end, step, keep, fmtSummary) (*PreviewResult, error)`
- **职责**：跑 Prom range 查询，对每个 (ts, labels, value) 调 `keep`，命中则收集为 fire；同时挑代表 series
- **流程**：
  1. `prom == nil` → `SkippedReason: "Prometheus client 未配置 — 无法试算"`
  2. `prom.QueryRange(ctx, expr, start, end, step)`；err → `%w`
  3. 非 matrix → SkippedReason
  4. json.Unmarshal 到 `[]matrixSeries{Metric, Values}`
  5. per-series per-sample：
     - `decodeMatrixSample(v)` 解码 `[<unix_ts>, "<float>"]`
     - 收集到 `previewSeries.points`（用于挑代表）
     - `keep != nil && !keep(value)` → 跳过
     - `fmtSummary` 渲染 → 收集到 `hits`
  6. `out.Series = pickRepresentativeSeries(all)`
  7. 无 hits → 返回 out（仅 Series，无 fire）
  8. `sort.Slice(hits)` 按 ts 升序
  9. `FireCount`、`FirstFireAt`、`LastFireAt`
  10. 取最近 5 条（newest first）填 `Samples`
- **错误处理**：查询失败 `%w`；解码失败 `%w`

### `pickRepresentativeSeries`

- **签名**：`func pickRepresentativeSeries(all []previewSeries) []PreviewSeriesPoint`
- **职责**：从多 series matrix 挑一条代表线
- **流程**：
  1. bestIdx = 0、bestLen = len(all[0].points)、bestMean = meanOf(all[0])
  2. per-series：点数更多 → 更新；点数相同且均值更高 → 更新（"最响"的线）
  3. 拷贝 bestIdx 的 points；按 ts 升序排序
  4. `downsampleSeries(pts, maxPreviewSeriesPoints)` 降采样
- **错误处理**：空输入返回 nil

### `meanOf` / `downsampleSeries`

- `meanOf(pts)`：算术均值
- `downsampleSeries(pts, cap)`：
  1. `cap <= 0 || len(pts) <= cap` → 原样返回
  2. `stride = ceil(len/cap)`
  3. 每 stride 取一个点
  4. 始终保留末点（锚定图表右边缘）
- 注释：naive stride 避免 LTTB/min-max 的复杂度，仍能保留整体形状供编辑器预览

### `decodeMatrixSample`

- **签名**：`func decodeMatrixSample(v []json.RawMessage) (time.Time, float64, bool)`
- **职责**：解码 `[<unix_seconds>, "<float>"]`
- **流程**：Unmarshal 两元素；`time.Unix(sec, nsec).UTC()`；parseFloat

### `previewMetricAnomaly` / `previewMetricForecast` / `previewMetricBurnRate`

- 职责：per-kind 编译 + 渲染 PromQL + 调 `walkPromMatrix`
- 流程：
  1. `compileMetricAnomalyRule(row)` 等；err → `SkippedReason: "请补全规则字段：" + err`
  2. `metricExprFor` + `applyClosedSetMetricSelector`
  3. 按 method 渲染 PromQL（与 `evaluators_phaseA.go` 完全一致的 zscore/mad/predict_linear/burn_rate 表达式）
  4. `walkPromMatrix`（step 60s 或 5min）
- **burn_rate 特殊**：preview 最短窗口（领先指标），注释"fastest signal is the most useful for tuning"
- **错误处理**：metric 不在 closed-set → SkippedReason

### `previewMetricRaw`

- **签名**：`func previewMetricRaw(ctx, row, start, end, deps) (*PreviewResult, error)`
- **职责**：expr 即谓词，跑它作为 fire 检测；启发式提取 `<lhs> <op> <number>` 拆出图表线 + 阈值参考
- **流程**：
  1. `compileMetricRawRule(row)`；err → SkippedReason
  2. `prom == nil` → SkippedReason
  3. `walkPromMatrix(ctx, deps.Prom, r.Expr, start, end, 60s, nil, fmtSummary)`（keep=nil，每个点都算 fire）
  4. `metricRawComparisonRE.FindStringSubmatch(r.Expr)` 提取 lhs + op + threshold
  5. 解析 threshold 成功且 lhs 非空 → 再跑 `walkPromMatrix(lhs)` 取图表线，覆盖 `res.Series`；`res.Threshold = &thr`
  6. 无 comparison 匹配 → `res.Series = nil`（仍渲染 fire_count + samples）
- **错误处理**：lhs 查询失败保留原 Series

### `previewLogMatch` / `previewLogVolume`

- 职责：解码 ConditionsJSON；拼 `count_over_time` LogQL；调 `walkLokiMatrix`
- 流程：
  1. `deps.Log == nil` → SkippedReason
  2. json.Unmarshal ConditionsJSON
  3. `stream_selector` 空 → SkippedReason
  4. 拼 expr（含 `|~ filter`）
  5. `walkLokiMatrix(ctx, deps.Log, expr, start, end, 5min, keep, fmtSummary)`
- **log_volume 注释**：preview 无法 backfill previous-window ratio，直接展示 volume；操作员对照绝对计数调阈值

### `previewTraceLatency` / `previewTraceErrorRate`

- 职责：解码 ConditionsJSON；查 spanmetrics 是否存在；拼 `histogram_quantile() > threshold_ms` 或错误率表达式
- 流程：
  1. `service` 空 → SkippedReason
  2. `traceSpanMetricsSelectorHasPoints(ctx, deps.Prom, metric, selector, start, end)` 检查 spanmetrics 是否有数据
  3. 无数据 → `traceSpanMetricsMissingReason`（"当前 X 未发现 Y"）
  4. 拼 expr；`walkPromMatrix`
- **错误处理**：spanmetrics 缺失给操作员清晰提示

### `traceSpanMetricsSelectorHasPoints`

- **签名**：`func traceSpanMetricsSelectorHasPoints(ctx, prom, metric, selector, start, end) (bool, error)`
- **职责**：检查 spanmetrics 是否有数据点
- **流程**：`count by (service_name[, span_name]) (metric{selector})` range 查询；任意 series 有可解码样本 → true

### `walkLokiMatrix`

- **签名**：`func walkLokiMatrix(ctx, logc, expr, start, end, step, keep, fmtSummary) (*PreviewResult, error)`
- **职责**：Loki 版矩阵遍历；与 `walkPromMatrix` 类似但不挑代表 series（无 Series 字段）
- **流程**：QueryRange；非 matrix → SkippedReason；per-series per-sample 调 keep + fmtSummary；sort by ts；填 FireCount / First/LastFireAt / Samples
- **错误处理**：查询失败 `%w`

### `shorterWindow` / `promDurationSeconds`

- `shorterWindow(a, b)`：比较两个 PromQL duration 字符串
- `promDurationSeconds(d)`：解析 `5m` / `1h` / `2d` 等；返回秒数；解析失败返回 `(0, false)`
- **fallback**：解析失败用字典序比较，避免 panic

## 5. 依赖关系

- **内部包**：`internal/manager/model/alert`、`internal/pkg/logquery`、`internal/pkg/promquery`
- **外部库**：`encoding/json` / `fmt` / `log/slog`（实际未用）/ `regexp` / `sort` / `strconv` / `strings` / `time`
- **被调用方**：HTTP handler `/v1/alerts/rules/preview`
- **依赖本包**：`usecase.go::buildRuleRow` / `rules.go::compile*Rule` / `evaluators_phaseA.go::metricExprFor/applyClosedSetMetricSelector/windowedSLI` / `pipeline.go::labelSetKey/parseFloat/compareFloat`

## 6. 并发与资源管理

- `PreviewRule` 是只读、无状态、可并发调用
- 每次 Prom/Loki range 查询走调用方 ctx
- 无锁、无共享状态

## 7. 设计模式与亮点

- **只读 side-channel**：永不持久化；复用 `buildRuleRow` 校验+编译路径，与生产 CreateRule 同一入口，保证试算 = 生产语义
- **占位字段**：RuleKey/Name/Severity 留空时填占位，让用户在命名规则前就能试算查询
- **per-kind 分派**：switch 分派到 8 个 previewer；未知 kind 给 SkippedReason 而非 err
- **代表 series 选择**：多 series matrix 时挑点数最多（平手取均值最高）的线——"最大的线"，操作员自然先看的
- **stride 降采样**：naive stride + 保留末点；避免 LTTB 复杂度，仍保留整体形状
- **metric_raw 阈值参考线**：`metricRawComparisonRE` 启发式提取 `<lhs> <op> <number>`，让 UI 画"线 + 水平阈值参考"；不匹配时仍渲染 fire_count + samples
- **burn_rate preview 最短窗口**：领先指标最有用调参
- **spanmetrics 缺失提示**：`traceSpanMetricsSelectorHasPoints` 先检查，给操作员"当前 X 未发现 Y"的清晰提示，避免空查询
- **`walkPromMatrix` 的 keep=nil**：metric_raw 谓词查询每个返回点都算 fire（PromQL 已过滤），不需 client 再判
- **`SkippedReason` 而非 err**：配置缺失（Prom/Loki 未配、字段缺失）返回 SkippedResult，让 UI 显示原因而非报错

## 8. 注意事项

- **`maxPreviewSeriesPoints = 1500`**：24h/60s step = 1440 点，1500 留 headroom；更大范围降采样
- **`defaultPreviewLookbackSeconds = 86400`**（24h）；min 60s、max 604800（7d）
- **`metricRawComparisonRE` 启发式**：仅匹配简单 `<lhs> <op> <number>`；复合表达式（`and` / `or` / `up == 0`）不匹配，图表线留空——仍渲染 fire_count
- **`previewLogVolume` 不算 ratio**：v1 直接展示 volume；操作员对照绝对计数调阈值
- **`walkLokiMatrix` 无 Series 字段**：Loki matrix 不挑代表线（与 Prom 不同），只返回 fire_count + samples
- **`promDurationSeconds` 单位有限**：仅识别 s/m/h/d；w/y 不支持（burn-rate 窗口通常不跨周）
- **`decodeMatrixSample` 用 `time.Unix(sec, nsec)`**：秒数带小数部分转纳秒；UTC 时区
- **`previewMetricBurnRate` 只 preview 最短窗口**：不 AND 多窗口（生产是 AND）；preview 目的是调参，最短窗口最敏感
- **`pickRepresentativeSeries` 平手取均值最高**：点数相同时选"最响"的线——操作员自然关注的异常线
- **不持久化**：所有 preview 是 read-only；RuleKey 可为空 / scratch，不写 DB
