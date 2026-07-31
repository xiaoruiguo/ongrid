# `embedded.go` 技术实现文档

> 源文件：`internal/edgeagent/collector/embedded.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/collector`

## 1. 概述

本文件实现 `EmbeddedCollector`：通过 gopsutil 进程内采集 cpu/mem/load/net/disk 五类资源 + 静态 host info，产出 node_exporter 兼容命名的 `*dto.MetricFamily`。每个 tick 同时输出 legacy 8 字段 `HostMetricPoint`（通过 Mapper）和 flat `PromSample`（通过 FlattenSamples）。

## 2. 包信息

- **包名**：`collector`
- **所属模块**：edgeagent metric 采集层（embedded 模式）
- **依赖方向**：被 `cmd/ongrid-edge` 或 `CompositeCollector` 构造；调用 `gopsutil/v3`、`prometheus/client_model`

## 3. 关键类型与接口

```go
// EmbeddedCollector 进程内采集 + Mapper 派生 HostMetricPoint
type EmbeddedCollector struct {
    log    *slog.Logger
    mapper *Mapper
    mu     sync.Mutex  // 序列化 snapshot 调用，保证 Mapper 计算增量基于稳定 last-snapshot
}
```

## 4. 关键函数与流程

### `NewEmbedded`
- **签名**：`func NewEmbedded(log *slog.Logger) (*EmbeddedCollector, error)`
- **职责**：构造 EmbeddedCollector
- **流程**：log nil → default；返回结构体（mapper = NewMapper()）
- **错误处理**：永远返回 nil error

### `CollectAll`
- **签名**：`func (c *EmbeddedCollector) CollectAll(ctx context.Context) ([]CollectorOutput, error)`
- **职责**：单 tick 采集
- **流程**：
  1. `snapshot(ctx)` 读 5 类资源 → `[]*dto.MetricFamily`
  2. `mapper.MapToHostPoint(now, families)` 派生 8 字段快路径
  3. `FlattenSamples(now, SourceEmbedded, families, nil)` 展平为 PromSample 切片
  4. 返回单元素 `[]CollectorOutput`（Source="embedded"）
- **错误处理**：snapshot 失败仅 Warn；families 为空返回错误「snapshot empty」

### `HostInfo`
- **签名**：`func (c *EmbeddedCollector) HostInfo(ctx context.Context) (tunnel.HostInfo, error)`
- **职责**：采集 register_edge 用的静态主机描述
- **流程**：
  1. `runtime.GOOS` / `GOARCH` / `NumCPU` 基础字段
  2. `host.InfoWithContext`：Hostname / KernelVersion / OS / HostID（platform-stable id：Linux machine-id / macOS IOPlatformUUID / Windows MachineGuid）
  3. `hardwareFingerprint()`：clone-resistant MAC|CPU|disk hash（处理克隆 VM 的 HostID 重复问题）
  4. `primaryIPv4()`：首选物理 NIC 的 IPv4；回退任意非 loopback
  5. `mem.VirtualMemoryWithContext`：MemTotalBytes
  6. `cpu.CountsWithContext(true)`：逻辑 CPU 数
- **错误处理**：每个 gopsutil 调用失败保留默认值；从不返回错误

### `GetHostLoad`
- **签名**：`func (c *EmbeddedCollector) GetHostLoad(ctx context.Context) (tunnel.GetHostLoadResponse, error)`
- **职责**：服务 cloud→edge `get_host_load` RPC
- **流程**：snapshot + MapToHostPoint，取 CPUPct/MemPct/DiskUsedPct/Load1/5/15
- **错误处理**：snapshot 错误忽略，返回部分零值

### `GetProcessList`
- **签名**：`func (c *EmbeddedCollector) GetProcessList(ctx context.Context, topN int, sortBy string) (tunnel.GetProcessListResponse, error)`
- **职责**：服务 `get_process_list` RPC
- **流程**：
  1. `process.ProcessesWithContext` 拉全表
  2. 对每个进程取 Name / Cmdline / Username / CPUPercent / MemoryPercent
  3. 按 `sortBy`（CPU / Mem）排序，截断到 topN
- **错误处理**：`process.ProcessesWithContext` 失败返回错误；单进程字段失败保留默认

### `snapshot`
- **签名**：`func (c *EmbeddedCollector) snapshot(ctx context.Context) ([]*dto.MetricFamily, error)`
- **职责**：读所有 gopsutil 源并转为 node_exporter 命名 MetricFamily
- **流程**：`mu.Lock` 序列化；依次调 cpuFamilies / memFamilies / loadFamilies / netFamilies / fsFamilies / timeFamilies；每个失败收集到 errs 但继续；用 `errors.Join` 返回
- **错误处理**：单个资源失败不阻塞其他；len(errs)>0 时返回 families + joined error

### 资源 builder 函数
- `cpuFamilies`：`cpu.TimesWithContext(true)` → `node_cpu_seconds_total{cpu,mode}` counter（user/system/idle/nice/iowait/irq/softirq/steal 8 模式）
- `memFamilies`：`mem.VirtualMemoryWithContext` → 5 个 gauge（MemTotal/MemAvailable/MemFree/Buffers/Cached）
- `loadFamilies`：`load.AvgWithContext` → 3 个 gauge（node_load1/5/15）
- `netFamilies`：`gnet.IOCountersWithContext(true)` → 4 个 counter（rx/tx bytes/packets，按 device label）
- `fsFamilies`：`disk.PartitionsWithContext(false)` + `disk.UsageWithContext` → 3 个 gauge（size/avail/free，按 mountpoint/fstype/device label）
- `timeFamilies`：`node_time_seconds` + `node_boot_time_seconds`（host.BootTimeWithContext）

### helper 函数
- `stripCPUPrefix`：`cpu0` → `0`（node_exporter 命名约定）
- `label` / `newCounterFamily` / `newGaugeFamily` / `gaugeFamily` / `appendCounter` / `appendGauge` / `ptrStr` / `ptrF64`：构造 `*dto.MetricFamily` 的辅助

## 5. 依赖关系

- **内部包**：`internal/pkg/tunnel`
- **外部库**：`github.com/shirou/gopsutil/v3`（cpu/disk/host/load/mem/net/process）、`github.com/prometheus/client_model/go`（dto 类型）
- **被调用方**：`cmd/ongrid-edge` 直接构造；或经 `CompositeCollector` 包装

## 6. 并发与资源管理

- **`sync.Mutex`**：序列化 `snapshot` 调用，让 Mapper 的 counter 增量计算基于稳定 last-snapshot；防止两个并发 tick 计算出错误 delta
- **context 传播**：所有 gopsutil 调用都带 `_WithContext(ctx)`；上层可取消
- **错误聚合**：`errors.Join(errs...)` 让多个资源失败汇总成一个 error 返回，同时保留 partial families

## 7. 设计模式与亮点

- **node_exporter 兼容命名**：所有 metric 名遵循 `node_*` 约定，让 cloud-side mapper / PromQL byte-for-byte 匹配 embedded 或 scrape 输出
- **gopsutil HostID 作为 device fingerprint**：Linux machine-id / macOS IOPlatformUUID / Windows MachineGuid；re-install edge agent 在同一主机保持映射到同一 device row
- **clone-resistant hardwareFingerprint**：MAC|CPU|disk SHA256 处理克隆 VM 的 HostID 重复（SMBIOS product_uuid 被复制）；与 HostID 并行上报让 cloud 优先用 hw fingerprint 但能迁移老 device row
- **错误聚合但 partial 返回**：单资源失败不阻塞其他；Mapper 对缺失 family 返回零值；保证 agent 永远有数据可推
- **stripCPUPrefix 命名对齐**：gopsutil 返回 `cpu0`，node_exporter 标签是 `0`；helper 做转换让 mapper 侧无感知
- **builder 函数分离**：每类资源一个 builder，便于单测和未来扩展（添加新 metric 只需新增一个 builder + 在 snapshot 中 append）

## 8. 注意事项

- `snapshot` 加锁但 `HostInfo` / `GetHostLoad` / `GetProcessList` 不加锁——它们各自调 gopsutil 不依赖 Mapper 状态，可并发；但若 Mapper 被并发调用 `MapToHostPoint`（GetHostLoad 和 CollectAll 同时）会因 Mapper 内部 mu 串行化，无 race
- `HostInfo` 中 gopsutil 失败保留默认值——但 `runtime.GOOS` / `NumCPU` 始终可用，保证 register_edge 不会因 gopsutil 失败而阻塞
- `hardwareFingerprint` 在克隆 VM 上仍可能碰撞（如 NIC MAC 也被复制）——这是已知限制，cloud 侧应提供手动合并 / 重注册工具
- `primaryIPv4` 的两遍扫描：先物理 NIC 后任意非 loopback；cloud-only VM 主 NIC 可能不匹配 isPhysicalNIC 启发式，走 fallback
- `fsFamilies` 中 `disk.PartitionsWithContext(false)` 不显示虚拟文件系统；`disk.UsageWithContext` 失败（如不可读 mount）静默跳过
- `timeFamilies` 中 `host.BootTimeWithContext` 失败时 `node_boot_time_seconds` 不输出——cloud 侧不应假设该 metric 始终存在
