# `metricscommon/scrape.go` 技术实现文档

> 源文件：`internal/edgeagent/plugins/metricscommon/scrape.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/plugins/metricscommon`

## 1. 概述

本文件是 custommetrics 与 databasemetrics 共用的 Prometheus 端点抓取工具层。定义 `Target` 强类型、`Scrape`/`ScrapeIncremental` 流式抓取入口、`ScrapeUpSample`/`ScrapeStatusSamples` 合成健康样本生成器、`ValidateURL` URL 校验。核心是流式解析（`streamTextSamples` 在 text_stream.go）：逐行解析 Prometheus 文本格式，达到 `SampleLimit` 立即停止读取，响应大小 / 解析内存 / 扁平化工作均 bounded。

## 2. 包信息

- **包名**：`metricscommon`
- **所属模块**：`internal/edgeagent/plugins/metricscommon`
- **依赖方向**：被 `custommetrics`/`databasemetrics` 调用；依赖 `internal/pkg/tunnel.PromSample`、`prometheus/common/expfmt`

## 3. 关键类型与接口

```go
type Target struct {
    ID, Name, URL string
    Enabled       bool
    Interval, Timeout time.Duration
    TLSInsecure   bool
    BearerToken, BasicUsername, BasicPassword string
    SourceLabel   string
    ExtraLabels   map[string]string
    SampleLimit   int
    LabelDrop     []string
    Kind          string
    ReportScrapeStatus bool
}

const (
    DefaultInterval = 30 * time.Second
    DefaultTimeout  = 5 * time.Second
    ScrapeUpMetricName              = "up"
    ScrapePartialMetricName         = "ongrid_scrape_partial"
    ScrapeAcceptedSamplesMetricName = "ongrid_scrape_samples_accepted"
)

type SampleLimitError struct{ Observed, Limit int }
type ScrapeStats struct{ Observed, Accepted int; LimitExceeded bool }
```

## 4. 关键函数与流程

### `Scrape(ctx, target) ([]PromSample, error)`
- **职责**：一次性抓取并返回全部样本。
- **流程**：转调 `ScrapeIncremental(ctx, target, accumulator)` 把所有 chunk 累积到 `samples` → 若 `stats.LimitExceeded` 返回 `SampleLimitError`。
- **错误处理**：抓取错误原样返回；超限返回 `*SampleLimitError`（Observed 是下界，因解析在首个超限样本即停）。

### `ScrapeIncremental(ctx, target, consume) (ScrapeStats, error)`
- **职责**：流式抓取，逐 chunk 调 `consume`。
- **流程**：
  1. `consume == nil` 报错
  2. `openScrapeResponse(ctx, target)` 打开 HTTP 响应
  3. `streamTextSamples(ctx, resp.Body, target, consume)` 流式解析（见 text_stream.go）
  4. `resp.Body.Close()`；scrape 错误优先于 close 错误返回；close 错误用 `%w` 包装
- **错误处理**：scrape 错误与 close 错误分别处理，scrape 错误优先。

### `openScrapeResponse(ctx, target) (*http.Response, error)`
- **职责**：发起 HTTP GET。
- **流程**：
  1. URL 空报错
  2. `http.NewRequestWithContext` + `Accept: text/plain`
  3. BearerToken 加 `Authorization: Bearer`；BasicUsername/Password 用 `SetBasicAuth`
  4. `httpClient(target).Do(req)` → 非 2xx：drain body（LimitReader 64KiB）+ Close + 报 "http status N"
- **错误处理**：drain 与 close 错误都包装进返回错误。

### `ScrapeUpSample(now, plugin, target, up) PromSample`
- **职责**：生成合成 `up` 样本（值 0/1）。
- **流程**：`scrapeStatusLabels(plugin, target)` 拼标签（ExtraLabels + plugin + target_id + target_name + kind），值 1.0 或 0.0，TsMs=now.UnixMilli()。

### `ScrapeStatusSamples(now, plugin, target, partial, accepted) []PromSample`
- **职责**：生成 `ongrid_scrape_partial` + `ongrid_scrape_samples_accepted` 两个合成样本，让 central Prometheus 区分完整与截断抓取。
- **流程**：返回 2 个样本，partial 为 0/1，accepted 为 float64(accepted)。

### `scrapeStatusLabels(plugin, target) map[string]string`
- **职责**：合成状态样本的标签集。
- **流程**：拷贝 `target.ExtraLabels`（trim key）+ `plugin`/`target_id`/`target_name`/`kind` 非空字段。
- **亮点**：**不**含 target.URL 或 error 文本——避免高基数字段做 label。

### `ValidateURL(raw) error`
- **职责**：校验 URL 形状。
- **流程**：`url.Parse` + scheme 必须 http/https + host 非空。

### `httpClient(target) *http.Client`
- **职责**：构造 HTTP client。
- **流程**：`Transport{MaxIdleConns:2, MaxIdleConnsPerHost:2, IdleConnTimeout:90s, TLSClientConfig:{MinVersion:TLS12}, DialContext:{Timeout:2s}}`；TLSInsecure 时 `InsecureSkipVerify=true`；`Client.Timeout = target.Timeout`（≤0 取 DefaultTimeout）。

### `applyLabelDrop(samples, drops)`
- **职责**：从样本 labels 删除 drops 列表中的 key。
- **流程**：drops 转 set；遍历样本，对每个 drop key `delete(labels, key)`。

## 5. 依赖关系

- **内部包**：`internal/pkg/tunnel`（PromSample）
- **外部库**：`github.com/prometheus/common/expfmt`（NewFormat/TypeTextPlain）、标准库 `context`/`crypto/tls`/`fmt`/`io`/`net`/`net/http`/`net/url`/`strings`/`time`
- **被调用方**：`custommetrics/plugin.go`、`databasemetrics/plugin.go`

## 6. 并发与资源管理

无共享状态。`Scrape`/`ScrapeIncremental` 纯函数式（每次 new client）。`streamTextSamples` 内 `chunk` 切片在调用栈上，无跨 goroutine 共享。`resp.Body.Close()` 在 streamTextSamples 返回后调用，保证连接释放。

## 7. 设计模式与亮点

- **流式解析 + SampleLimit 早停**：`streamTextSamples` 达到 SampleLimit 立即 break，不读完整个响应——响应大小、解析内存、扁平化工作都 bounded，对大数据量目标（如 kube-state-metrics）关键。
- **合成 up 样本**：edge 侧抓取的 target 不被 central Prometheus 直接抓，故 edge 自己 push `up=0/1`，让 manager 侧 PromQL 能 `up{target_id=...} == 0` 告警。
- **合成 partial/accepted 样本**：`ScrapeStatusSamples` 暴露抓取是否截断与接受样本数，central Prometheus 可监控抓取完整性。
- **标签低基数**：`scrapeStatusLabels` 仅含 plugin/target_id/target_name/kind + ExtraLabels，**不含** target.URL 或 error 文本——避免高基数字段做 label。
- **chunked consume**：`ScrapeIncremental` 每 1000 样本调一次 consume，调用方可增量上送或累积；`streamSampleChunkSize=1000` 平衡内存与 RPC 次数。
- **applyLabelDrop**：支持目标级 label drop（如 databasemetrics 默认 drop query/statement），控制 cardinality。
- **httpClient 短 dial timeout**：2s dial timeout 让 localhost 抓取的整体 timeout 不被 TCP backoff 主导。

## 8. 注意事项

- `Scrape` 返回 `*SampleLimitError` 时仍返回已累积的 samples（未超限部分）——调用方需决定是否上送部分样本。custommetrics 当前直接返回错误不上送部分样本，databasemetrics 同样。
- `SampleLimitError.Observed` 是下界（解析在首个超限样本即停），实际样本数可能更多。
- `httpClient` 每次 new，不池化——与 metrics/scrape.go 同样设计，单 target 场景 OK。
- `applyLabelDrop` 在 `streamTextSamples` 内每个 chunk 调一次，对每样本 `delete(labels, key)`——若 drops 列表大且样本多，性能损耗可见；可考虑在 parse 阶段直接跳过 label。
- `openScrapeResponse` 非 2xx 时 drain 64KiB 后 Close——防止连接复用因未读 body 失败，但 64KiB 上限对大 error body 不够（如 HTML 错误页），可能让 keep-alive 失败，下次重新 dial。
- `Target.ReportScrapeStatus` 字段定义但本文件未使用——可能预留给未来调用方控制是否生成 partial/accepted 样本。
- `ScrapeUpSample` 的 labels 含 `target_id`/`target_name`/`kind`——若同 target_id 的 target 被多次抓取（不应发生），样本会冲突；spec.go 的去重校验保证 ID 唯一。
