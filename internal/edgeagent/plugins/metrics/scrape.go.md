# `metrics/scrape.go` 技术实现文档

> 源文件：`internal/edgeagent/plugins/metrics/scrape.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/plugins/metrics`

## 1. 概述

本文件是 metrics 插件的 spec 解析与抓取层：`parseSpec` 把 `PluginConfig.Spec` 解析为强类型 `specView`（含多 URL 抓取目标、interval/timeout、TLS、bearer、extra_labels、文件系统去重开关）；`scrapeOnce` 执行单次 HTTP GET + Prometheus 文本解析 + `collector.FlattenSamples` 扁平化 + 可选 `dedupeFilesystemsByDevice` 去重，返回 `[]tunnel.PromSample`。默认抓取 node_exporter:9102 + process_exporter:9256 双目标。

## 2. 包信息

- **包名**：`metrics`
- **所属模块**：`internal/edgeagent/plugins/metrics`
- **依赖方向**：被本包 `plugin.go` 的 `Configure`/`runLoop`/`scrapeAndPushOne` 调用；调用 `internal/edgeagent/collector.FlattenSamples`、`internal/pkg/tunnel.PromSample`、prometheus client_model/expfmt

## 3. 关键类型与接口

```go
type specView struct {
    URLs                      []string
    Interval, Timeout         time.Duration
    TLSInsecure               bool
    BearerToken               string
    ExtraLabels               map[string]string
    SourceLabel               string // 默认空，manager ingester 不附 ongrid_source label
    DedupeFilesystemsByDevice bool
}

var defaultURLs = []string{
    "http://127.0.0.1:9102/metrics", // node_exporter
    "http://127.0.0.1:9256/metrics", // process_exporter
}

const (
    defaultInterval = 15 * time.Second
    defaultTimeout  = 5 * time.Second
)
```

## 4. 关键函数与流程

### `parseSpec(spec) (specView, error)`
- **职责**：解析 spec，应用默认值。
- **流程**：
  1. 初始化 out 为默认（URLs=defaultURLs, Interval=15s, Timeout=5s）
  2. spec==nil 直接返回默认
  3. URLs：`target_urls` 数组优先 > `target_url` 单值（legacy）> defaultURLs
  4. `scrape_interval`/`scrape_timeout` 走 `time.ParseDuration`，≤0 报错
  5. `tls_insecure`/`bearer_token`/`extra_labels`/`dedupe_filesystems_by_device`/`source_label` 取值
  6. timeout > interval 时 clamp 到 interval（防抓取跨越周期）
  7. 遍历 URLs `url.Parse` 校验
- **错误处理**：duration 解析失败、≤0、URL 解析失败均报错。

### `sourceLabelForURL(raw) string`
- **职责**：构造 wire 侧 source label。
- **流程**：解析 URL，返回 `"metrics:<host>"`；解析失败或 host 空 → `"metrics:unknown"`。
- **注意**：当前 `specView.SourceLabel` 默认空（spec.source_label 未设），此函数实际未被 plugin.go 调用——plugin.go 用 `spec.SourceLabel` 直接传给 push RPC。

### `scrapeOnce(ctx, spec, targetURL) ([]PromSample, string, error)`
- **职责**：单次抓取 + 解析 + 扁平化。
- **流程**：
  1. `http.NewRequestWithContext(ctx, GET, targetURL, nil)` → `Accept: text/plain`（expfmt.TextPlain）
  2. BearerToken 非空加 `Authorization: Bearer`
  3. `newClient(spec).Do(req)` → 非 2xx drain body 后报 "http status N"
  4. `expfmt.TextParser.TextToMetricFamilies(resp.Body)` 解析
  5. `familiesToSlice(families)` 按 name 排序转 slice（稳定顺序）
  6. `collector.FlattenSamples(now, spec.SourceLabel, mfs, spec.ExtraLabels)` 扁平化为 `[]PromSample`
  7. 若 `DedupeFilesystemsByDevice` → `dedupeFilesystemSamplesByDevice(samples)`
  8. 返回 `(samples, spec.SourceLabel, nil)`
- **错误处理**：request build / http / parse 失败用 `%w` 包装返回；非 2xx 报 status。

### `dedupeFilesystemSamplesByDevice(samples) []PromSample`
- **职责**：每个物理块设备只保留一个 mountpoint 的 node_filesystem_* 样本。
- **流程**：
  1. 第一轮：遍历样本，对 `node_filesystem_*` 且 device 以 `/dev/` 开头的，按 device 维度选 preferred mountpoint（`/` 优先，否则按路径长度升序，同长度字典序）
  2. 第二轮：in-place 过滤，物理设备的非 preferred mountpoint 样本 skip
- **错误处理**：无。
- **亮点**：保留 tmpfs/virtiofs 等虚拟设备（`isPhysicalBlockDevice` 仅识别 `/dev/` 前缀），避免误删独立文件系统。

### `newClient(spec) *http.Client`
- **职责**：构造 per-scrape HTTP client。
- **流程**：`http.Transport{MaxIdleConns:2, MaxIdleConnsPerHost:2, IdleConnTimeout:90s, TLSClientConfig:{MinVersion:TLS12}, DialContext:{Timeout:2s}}`；TLSInsecure 时 `InsecureSkipVerify=true`；`Client.Timeout = spec.Timeout`。
- **设计**：不池化，每次 new——单 target 场景 keep-alive 收益不抵生命周期复杂度。

### `familiesToSlice(in) []*dto.MetricFamily`
- **职责**：map[name]*MetricFamily → 按 name 排序的 slice，保证顺序稳定。

### 工具函数
- `stringFrom`/`boolFrom`/`stringSlice`/`stringMap`：容忍 JSON interface{} 形态的 spec 取值助手

## 5. 依赖关系

- **内部包**：`internal/edgeagent/collector`（FlattenSamples）、`internal/edgeagent/plugins`（PluginConfig）、`internal/pkg/tunnel`（PromSample）
- **外部库**：`github.com/prometheus/client_model/go`（dto.MetricFamily）、`github.com/prometheus/common/expfmt`（TextParser/NewFormat）、标准库 `context`/`crypto/tls`/`fmt`/`io`/`net`/`net/http`/`net/url`/`sort`/`strings`/`time`
- **被调用方**：本包 `plugin.go`

## 6. 并发与资源管理

无并发控制。`scrapeOnce` 每次 new client，无共享状态。`parseSpec` 纯函数。`dedupeFilesystemSamplesByDevice` 用 `samples[:0]` in-place 复用底层数组——调用方持有唯一引用，安全。

## 7. 设计模式与亮点

- **默认双目标**：defaultURLs 同时抓 node_exporter + process_exporter，fresh edge 无需操作员配置即产生主机 + 进程级指标。
- **timeout clamp**：timeout > interval 强制等于 interval，避免抓取跨越周期导致重叠 hammer 目标。
- **文件系统去重**：容器/VM 运行时同 `/dev/...` 通过多个 bind mount 暴露会倍增 `node_filesystem_*` series；`dedupeFilesystemsByDevice` 按 device 选 preferred mountpoint（`/` 优先），减少无信息冗余。
- **稳定排序**：`familiesToSlice` 按 name 排序，保证 FlattenSamples 输出顺序确定，测试可断言。
- **SourceLabel 默认空**：默认不附 `ongrid_source` label，因为"retired host.docker.internal scrape 后只有一个 source，无需消歧"——减少 label 噪声。
- **early URL 校验**：parseSpec 期 `url.Parse` 所有 URLs，让坏配置在 HealthSnapshot 阶段暴露而非每 tick HTTP 错误。
- **in-place 切片复用**：`dedupeFilesystemSamplesByDevice` 用 `samples[:0]` 复用底层数组，零分配。

## 8. 注意事项

- `sourceLabelForURL` 函数定义但未被 plugin.go 调用——dead code，可删或在 plugin.go 改用之以支持多 URL 区分 source。
- `newClient` 每次 new `*http.Client`，对 localhost 单 target 无性能问题；多 target 远程抓取场景连接池化收益显著。
- `dedupeFilesystemSamplesByDevice` 用 `samples[:0]` in-place 修改入参切片——调用方 `scrapeOnce` 内 `samples` 是 `FlattenSamples` 新建切片，修改安全；但若未来调用方复用 samples 需注意。
- `parseSpec` 对 `target_url`（单值 legacy）与 `target_urls`（数组）的处理是"先数组后单值"，若同时设两个 spec key 数组优先——文档需明确。
- `url.Parse` 对绝大多数 malformed URL 返回 error，但对 `http://` 这种空 host 不报错——后续 `scrapeOnce` 的 `http.NewRequestWithContext` 会失败，仍会在运行时报错。
- `stringFrom` 对非 string 形态（如 `scrape_interval: 15` 数字）返回空串，导致 `time.ParseDuration("")` 失败——操作员需用字符串 `"15s"`。
- `expfmt.TextParser.TextToMetricFamilies` 对 Prometheus 文本格式严格解析，目标端点返回非标准格式（如带注释行格式错误）会报错；node_exporter/process_exporter 输出符合标准。
- compile-time guard `var _ plugins.Plugin = (*Plugin)(nil)` 在本文件末尾——实际应在 plugin.go，但放 scrape.go 也能编译期检查。
