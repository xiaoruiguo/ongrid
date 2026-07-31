# `metricscommon/text_stream.go` 技术实现文档

> 源文件：`internal/edgeagent/plugins/metricscommon/text_stream.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/plugins/metricscommon`

## 1. 概述

本文件实现 Prometheus 文本格式的流式解析器。`streamTextSamples` 用 `bufio.Scanner` 逐行读取响应体，`parseTextSample` 手写状态机解析每行（metric name + `{labels}` + value + 可选 timestamp），达到 `SampleLimit` 立即 break。手写解析器而非用 `expfmt.TextParser` 是为了流式 + 早停——`TextParser` 一次性读全 + 全量 MetricFamily，对大响应（kube-state-metrics 等）内存爆炸。

## 2. 包信息

- **包名**：`metricscommon`
- **所属模块**：`internal/edgeagent/plugins/metricscommon`
- **依赖方向**：被本包 `scrape.go` 的 `ScrapeIncremental` 调用；依赖 `internal/pkg/tunnel.PromSample`、`prometheus/common/model`（名称/标签校验）

## 3. 关键类型与接口

```go
const (
    streamSampleChunkSize    = 1000
    maxPrometheusTextLineLen = 1 << 20 // 1MiB
)
```

无导出类型。

## 4. 关键函数与流程

### `streamTextSamples(ctx, r, target, consume) (ScrapeStats, error)`
- **职责**：流式解析 Prometheus 文本，逐 chunk 调 consume。
- **流程**：
  1. `bufio.Scanner` + `Buffer(64KiB, 1MiB)`（单行最大 1MiB）
  2. `defaultTimestamp = time.Now().UnixMilli()`（无 timestamp 行用此）
  3. `chunk := make([]PromSample, 0, 1000)`
  4. 遍历 scanner.Scan()：
     - `ctx.Err()` 检查取消
     - `parseTextSample(line, defaultTimestamp, target.ExtraLabels)` → sample/ok/err
     - err 返回带 "parse line N" 前缀
     - !ok 跳过（空行 / 注释行）
     - `stats.Observed++`
     - SampleLimit > 0 且 admitted >= limit → `stats.LimitExceeded = true` + break
     - `chunk = append(chunk, sample)` + `admitted++`
     - chunk 满 1000：`applyLabelDrop(chunk, target.LabelDrop)` → `consume(chunk)` → 累加 Accepted → `clear(chunk)` + `chunk = chunk[:0]`
  5. scanner.Err() 返回
  6. 剩余 chunk > 0：applyLabelDrop + consume + 累加 Accepted
- **错误处理**：parse 错误 / ctx 取消 / consume 错误 / scanner 错误均返回。

### `parseTextSample(line, defaultTimestamp, extraLabels) (PromSample, bool, error)`
- **职责**：解析单行。
- **流程**：
  1. trim；空行 / `#` 开头返回 (zero, false, nil)
  2. 解析 metric name：扫描到 `{` 或空白为止；`model.IsValidMetricName` 校验
  3. 初始化 `labels` map（容量 extraLabels+4）
  4. 若 `{`：`parseTextLabels` 解析 label set
  5. 合并 extraLabels：仅当 labels 中不存在该 key 时填入（样本自带 label 优先）
  6. 解析 value：扫描到空白为止；`strconv.ParseFloat`
  7. 解析可选 timestamp：扫描到空白为止；`strconv.ParseInt`；>0 时覆盖 defaultTimestamp；trailing 数据报错
  8. NaN/Inf 样本跳过（返回 false）
  9. labels 空 → nil
  10. 返回 (sample, true, nil)
- **错误处理**：name 缺失 / 无效 / value 缺失 / 无效 / timestamp 无效 / trailing 数据均报错。

### `parseTextLabels(line, position, labels) (int, error)`
- **职责**：解析 `{k="v",k2="v2"}` label set。
- **流程**：
  1. 跳过 `{`
  2. 循环：
     - skipBlanks；position >= len 报 "unterminated"
     - `}` 返回 position+1
     - 解析 label name：扫描到 `=` 或空白；`model.LabelName.IsValid` 校验；拒绝 `__name__`；重复 name 报错
     - skipBlanks；期望 `=`；skipBlanks；期望 `"`
     - `parseTextLabelValue` 解析 quoted value（处理 `\\`/`\"`/`\n` 转义）
     - `model.LabelValue.IsValid` 校验
     - 写入 labels
     - skipBlanks；`,` 继续 / `}` 返回 / 其他报错
- **错误处理**：所有 parse 错误带位置信息返回。

### `parseTextLabelValue(line, position) (string, int, error)`
- **职责**：解析双引号内的 label value，处理转义。
- **流程**：遍历到 `"`：
  - `\\`：读下一字符，`\\`/`"` 直接写，`n` 写换行，其他报 "invalid escape sequence"
  - `"`：返回 value + position+1
  - 其他：直接写
- **错误处理**：未闭合引号报 "unterminated quoted label value"。

### `skipBlanks(line, position) int`
- 跳过空格和 tab。

### `isBlank(value byte) bool`
- `value == ' ' || value == '\t'`。

## 5. 依赖关系

- **内部包**：`internal/pkg/tunnel`（PromSample）
- **外部库**：`github.com/prometheus/common/model`（IsValidMetricName/LabelName/LabelValue/MetricNameLabel）、标准库 `bufio`/`bytes`/`context`/`fmt`/`io`/`math`/`strconv`/`strings`/`time`
- **被调用方**：本包 `scrape.go`

## 6. 并发与资源管理

无共享状态。`streamTextSamples` 在调用栈上持有 `chunk` 切片，无跨 goroutine 共享。`bufio.Scanner` 单 goroutine 使用，非线程安全。

## 7. 设计模式与亮点

- **手写状态机解析器**：相比 `expfmt.TextParser` 的全量读 + 全量 MetricFamily，本解析器逐行 + 流式 + 早停，内存 bounded；代价是不支持 histogram/summary 的 `_bucket`/`_sum`/`_count` 聚合语义（但 PromSample 扁平化也不需要）。
- **SampleLimit 早停**：达到 limit 立即 break scanner，不读完响应——对 kube-state-metrics 这类大响应目标关键，避免 OOM。
- **chunked consume**：每 1000 样本调一次 consume，平衡内存（chunk 上限 1000）与 RPC 次数。
- **extraLabels 合并语义**：样本自带 label 优先，extraLabels 仅填缺失 key——避免 extraLabels 覆盖目标特有 label。
- **NaN/Inf 跳过**：NaN/Inf 样本静默跳过（不报错也不计入），避免上送无意义值。
- **timestamp 默认值**：无 timestamp 行用 `time.Now().UnixMilli()`，与 Prometheus 行为一致。
- **trailing 数据检测**：timestamp 后若有非空白 trailing 数据报错，避免静默吞掉格式错误。
- **label 转义处理**：`\\`/`\"`/`\n` 三种转义，符合 Prometheus 文本格式规范。
- **1MiB 单行上限**：`scanner.Buffer(64KiB, 1MiB)` 防止超大行 OOM。

## 8. 注意事项

- **不支持 HELP/TYPE 注释**：`#` 开头行直接跳过，不解析 HELP/TYPE 元信息——PromSample 扁平化不需要，但若未来需要 type 信息需扩展。
- **不支持 histogram/summary 聚合**：`_bucket`/`_sum`/`_count` 被当作普通样本扁平化，调用方（`collector.FlattenSamples`）负责类型语义——本解析器不调 FlattenSamples，custommetrics/databasemetrics 直接上送扁平样本。
- **label name `__name__` 拒绝**：`parseTextLabels` 显式拒绝 `__name__` 作为 label key，符合 Prometheus 规范（name 在 name 字段不在 labels）。
- **`chunk = chunk[:0]` 复用底层数组**：`clear(chunk)` 在 Go 1.21+ 把元素置零，然后 `chunk[:0]` 复用——零分配，但若 consume 异步持有 chunk 引用会出问题；当前 consume 同步使用，OK。
- **`applyLabelDrop` 每 chunk 调一次**：drops 列表大且样本多时性能损耗可见（每样本遍历 drop set 删除）；可在 parse 阶段直接跳过 label 优化。
- **scanner.Buffer 上限 1MiB**：对绝大多数 Prometheus 指标行足够（histogram bucket label 可能较长），但若遇到超长行会被截断报错。
- **`parseTextSample` 对 trailing 数据严格**：timestamp 后非空白报错，但 value 后无 timestamp 是合法的（value 后直接换行）——position 检查 `>= len(line)` 时跳过 timestamp 解析。
- **未处理 `\r\n` 行尾**：`bytes.TrimSpace` 去掉 `\r`，OK。
- **`extraLabels` 合并是"样本优先"**：若样本自带 `instance` label，extraLabels 的 `instance` 不覆盖——这是正确语义，但调用方需知 extraLabels 是默认值非强制值。
