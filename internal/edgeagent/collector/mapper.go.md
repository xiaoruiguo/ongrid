# `mapper.go` 技术实现文档

> 源文件：`internal/edgeagent/collector/mapper.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/collector`

## 1. 概述

本文件实现 `Mapper`（从 node_exporter 命名 MetricFamily 提取 8 字段 `HostMetricPoint` 快路径，含 counter→rate 转换的 CPU%/NetRxBps/NetTxBps 状态）和 `FlattenSamples`（把 MetricFamily 展平为 `[]tunnel.PromSample` 供 push_prom_samples，支持 counter/gauge/untyped/summary/histogram 全类型）。是 embedded 与 scrape 共用的转换层。

## 2. 包信息

- **包名**：`collector`
- **所属模块**：edgeagent metric 转换层
- **依赖方向**：被同包 `embedded.go` / `scrape.go` / `composite.go` 调用；调用 `tunnel`

## 3. 关键类型与接口

```go
// Mapper 提取快路径 + 维护 counter 缓存用于 rate 计算
type Mapper struct {
    mu   sync.Mutex
    last map[string]counterSample  // key = metric_name + sorted labels
}

// counterSample 记录单次 counter 读取
type counterSample struct {
    t time.Time
    v float64
}
```

## 4. 关键函数与流程

### `MapToHostPoint`
- **签名**：`func (m *Mapper) MapToHostPoint(now time.Time, families []*dto.MetricFamily) tunnel.HostMetricPoint`
- **职责**：提取 8 字段快路径
- **流程**：
  1. `mu.Lock` 串行化（保证 delta 基于稳定 last-snapshot）
  2. `indexFamilies` 建 name→family 索引
  3. 各字段独立计算：CPUPct（counter rate）/ MemPct（gauge）/ Load1/5/15（gauge）/ NetRxBps+NetTxBps（counter rate）/ DiskUsedPct（gauge）
- **错误处理**：缺失 family 返回零值；永不返回错误

### `FlattenSamples`
- **签名**：`func FlattenSamples(now time.Time, source string, families []*dto.MetricFamily, extraLabels map[string]string) []tunnel.PromSample`
- **职责**：展平 MetricFamily 为 `[]PromSample`
- **流程**：遍历每个 family 的每个 metric：
  - gauge / counter / untyped：单 sample
  - summary：每个 quantile + `_sum` + `_count`
  - histogram：每个 bucket (`_bucket` + `le` label) + `_sum` + `_count`
  - extraLabels 合并（producer labels 优先，不覆盖）
  - NaN/Inf 跳过（JSON 不支持）
- **错误处理**：nil family / nil name 跳过；nil metric 类型字段跳过

### `cpuPct`
- **签名**：`func (m *Mapper) cpuPct(now time.Time, idx map[string]*dto.MetricFamily) float64`
- **职责**：计算 CPU 忙时百分比
- **流程**：
  1. 遍历 `node_cpu_seconds_total{cpu,mode}` 所有 metric
  2. key = `counterKey("node_cpu_seconds_total", labels)` 维持同 (cpu,mode) 跨调用映射
  3. 缓存上次值；delta = max(0, v - prev.v)
  4. `totalDelta += delta`；mode=="idle" 时 `idleDelta += delta`
  5. `(totalDelta - idleDelta) / totalDelta * 100`
- **错误处理**：首次调用无 prev 返回 0；totalDelta<=0 返回 0

### `netRate`
- **签名**：`func (m *Mapper) netRate(now time.Time, idx map[string]*dto.MetricFamily, name string) uint64`
- **职责**：计算网络 bytes/sec
- **流程**：
  1. 遍历 `node_network_{receive,transmit}_bytes_total{device}`
  2. 排除 `lo`（loopback）
  3. 同 cpuPct 缓存 + delta
  4. `deltaSum / dtMin`（取最小 dt）
- **错误处理**：首次调用返回 0；dtMin<=0 返回 0

### `memPct`
- **签名**：`func memPct(idx map[string]*dto.MetricFamily) float64`
- **职责**：内存使用率
- **流程**：`(1 - MemAvailable / MemTotal) * 100`；MemAvailable 缺失时回退 `MemFree + Buffers + Cached`
- **错误处理**：MemTotal<=0 返回 0；avail>=total 返回 0

### `diskUsedPct`
- **签名**：`func diskUsedPct(idx map[string]*dto.MetricFamily) float64`
- **职责**：根分区使用率
- **流程**：`matchGauge` 查 `mountpoint="/"` 的 `node_filesystem_size_bytes` 和 `node_filesystem_avail_bytes`；`(1 - avail/size) * 100`
- **错误处理**：size<=0 返回 0；avail>=size 返回 0

### helper 函数
- `indexFamilies`：name→family map
- `simpleGauge`：首个 gauge 值
- `matchGauge`：按 label key=value 匹配的 gauge 值
- `counterKey`：`name|k1=v1,k2=v2` 稳定 key（排序后拼接）
- `labelValue`：按名取 label value
- `mergedLabels`：合并 `[]LabelPair` + extras map（extras 不覆盖）
- `cloneLabels`：深拷贝 map
- `strconvF64`：Prometheus 文本格式 canonical float 字符串（`strconv.FormatFloat(f, 'g', -1, 64)`）
- `appendPromSample`：跳过 NaN/Inf

## 5. 依赖关系

- **内部包**：`internal/pkg/tunnel`
- **外部库**：`github.com/prometheus/client_model/go`（dto 类型）、标准库 `math`、`sort`、`strconv`、`strings`、`sync`、`time`
- **被调用方**：同包 `embedded.go::CollectAll/GetHostLoad`、`scrape.go::CollectAll/GetHostLoad`、`composite.go`

## 6. 并发与资源管理

- **`sync.Mutex`**：`MapToHostPoint` 串行化，保证 counter delta 基于稳定 last-snapshot；防止两个并发 tick 计算出错误 delta（如 tick1 读 v1，tick2 读 v1 + 计算 delta against v0，但 v0 已被 tick1 更新为 v1）
- **`FlattenSamples` 是纯函数**：无锁，可并发调用；不依赖 Mapper 状态
- **counter 缓存 `last`**：map 持久化（不清理），长期运行会积累 key（每个 (cpu,mode) / device 一个 entry）；但基数低（CPU 数 * 8 模式 + NIC 数 * 4 metric），内存可忽略

## 7. 设计模式与亮点

- **counter→rate 转换的状态化**：Mapper 持有 `last` map 缓存上次 counter 值 + 时间戳；`cpuPct` / `netRate` 用 delta/dt 计算 rate
- **首次调用返回 0**：rate-derived 字段（CPUPct/NetRxBps/NetTxBps）首次调用无 prev 返回 0；cloud 侧应容忍首个点为零
- **max(0, delta)**：counter 重置（如重启）会产生负 delta，clamp 到 0 避免 rate 爆炸
- **loopback 排除**：`lo` 接口的 rx/tx 不计入 NetRxBps/NetTxBps，避免 loopback 流量虚高
- **memPct 双公式**：优先 `(1 - MemAvailable/MemTotal)`；MemAvailable 缺失（老内核）回退 `(1 - (MemFree+Buffers+Cached)/MemTotal)`
- **diskUsedPct 只看 `/`**：根分区是 dashboard 关心指标；其他 mountpoint 在 Samples 中可见但快路径不计算
- **FlattenSamples 全类型支持**：counter/gauge/untyped/summary/histogram 都正确展平；summary 的 quantile + `_sum` + `_count`；histogram 的 bucket + `le` label + `_sum` + `_count`
- **NaN/Inf 过滤**：JSON 不支持 NaN/Inf；`appendPromSample` 跳过这些值，避免序列化失败
- **extraLabels 不覆盖**：scrape 的 static_labels 合并时 producer labels 优先；防止 static_labels 覆盖真实 metric labels

## 8. 注意事项

- `cpuPct` 的 key 包含 sorted labels——同一 (cpu, mode) 跨调用映射稳定；但若 metric 额外 labels 变化（如新增 `instance` label），key 会变，delta 计算中断一次（重新走「首次调用」返回 0）
- `netRate` 的 `dtMin` 取最小 dt——多设备时取最短采样间隔；通常所有设备同时采样，dt 一致
- `memPct` 的回退公式 `MemFree+Buffers+Cached` 不等于 `MemAvailable`——老内核会低估内存使用（MemAvailable 考虑 reclaimable），但近似可用
- `diskUsedPct` 只看 `mountpoint="/"`——容器化部署可能没有 `/` 挂载点，快路径返回 0；operator 应通过 scrape 上报容器文件系统
- `counterKey` 的拼接格式 `name|k1=v1,k2=v2`——若 label value 含 `|` 或 `=` 会破坏 key 唯一性；Prometheus label value 不允许这些字符，所以安全
- `FlattenSamples` 的 histogram bucket 用 `le` label——与 Prometheus 文本格式一致；cloud 侧 PromQL `histogram_quantile()` 可正确计算
- Mapper 长期运行不清理 `last` map——若 metric label 集合变化（如 NIC 移除），旧 key 仍保留；内存增长可控但理论上有泄漏；可考虑加 TTL 清理（当前未实现）
