# ingester.go

## 1. 概述

`ingester.go` 是 promwrite 包的唯一源文件，桥接 tunnel 的 `PromSample`（edge 推上来的 Prometheus 样本）与 cross-BC `promwrite.Client`（向云 Prom 实例 remote_write）。

职责：
- 把 `device_id` + `ongrid_source` 标签合并到每个样本（edge 不知道自己的数字 ID；多 collector 部署靠 source 标签区分）
- 按标签名字典序排序（remote_write 硬要求；Prom 拒绝未排序标签集）
- 委派给底层 `promwrite.Client`
- 跟踪 health snapshot（连续失败数 + 最后失败时间）供 alert pipeline 评估

## 2. 包信息

- 包名：`promwrite`
- 路径：`internal/manager/biz/promwrite`
- 包注释：明确桥接职责 + 依赖 `internal/pkg/promwrite` 与 `internal/pkg/tunnel`（cross-BC），不导入其它 manager/* 子域

## 3. 关键类型与接口

### Health

```go
type Health struct {
    Failures      int           // 连续失败数，成功后重置为 0
    LastFailureAt time.Time     // 最近失败时间，成功后不重置（"最近一次失败 ever"）
}
```

注释：`LastFailureAt` 成功后不重置，调用方按自己的 grace window 老化。

### Writer 接口

```go
type Writer interface {
    Write(ctx, samples []pkgpromwrite.Sample) error
}
```

本地声明，让测试注入 fake 不需起真 HTTP server。`*pkgpromwrite.Client` 结构性满足。

### Ingester

```go
type Ingester struct {
    w   Writer
    log *slog.Logger
    mu              sync.Mutex
    consecutiveFail int
    lastFailureAt   time.Time
}
```

### Reserved labels

```go
var (
    reservedHostLabels = map[string]struct{}{
        "__name__": {}, "device_id": {}, "ongrid_source": {},
    }
    reservedKubernetesLabels = map[string]struct{}{
        "__name__": {}, "cluster_id": {}, "device_id": {}, "edge_id": {}, "ongrid_source": {},
    }
)
```

edge 推上来的样本若含 reserved label，会被丢弃（cloud 强制覆盖）。

## 4. 关键函数与流程

### NewIngester

构造。log nil 回退 `slog.Default()`。Writer 必须非 nil（调用方传 configured client 或 fake）。

### Health / HealthSnapshot

```go
func (i *Ingester) Health() Health
func (i *Ingester) HealthSnapshot() (int, time.Time)
```

`Health` 返回结构体；`HealthSnapshot` 返回原语（`int` + `time.Time`）。注释：`HealthSnapshot` 是 alert `HealthReporter` 消费的形状，保持接口无 struct 类型让 alert 包与此包解耦。

### Push

```go
func (i *Ingester) Push(ctx, deviceID uint64, source string, samples []tunnel.PromSample) error
```

1. 空样本 no-op
2. `w == nil` 静默 drop（degraded 模式，Prom 禁用）
3. `fixedLabels = {"device_id": deviceID, "ongrid_source": source}`
4. `buildPromSamples(samples, fixedLabels, reservedHostLabels)`
5. `write(ctx, "device_id", deviceID, source, out)`

### PushKubernetes

同 Push 但写 `cluster_id` 标签而非 `device_id`。注释：controller edge 不是 host Device，不带 `device_id`。reserved 集合更大（含 `device_id` / `edge_id`）。

### buildPromSamples

```go
func buildPromSamples(samples, fixedLabels, reserved) []pkgpromwrite.Sample
```

对每个 sample：
1. `labels := []Label{{Name: "__name__", Value: s.Name}}`
2. 加 fixedLabels（值空的跳过）
3. 加 `s.Labels`（reserved 的跳过）
4. `sort.Slice(labels, by name)` —— Prom 要求排序
5. 输出 `Sample{Labels, Value, TsMs}`

### write

```go
func (i *Ingester) write(ctx, entityLabel, entityID, source, out) error
```

1. `w.Write(ctx, out)`：
   - 失败 → `recordFailure()` + `prom.IncPromWrite(err)` + warn log + return err
   - 成功 → `recordSuccess()` + `prom.IncPromWrite(nil)` + return nil

注释：`prom_write_total{result=fail}` 与 health snapshot（`Failures++` / `LastFailureAt`）一致，让两个 surface 报告同一失败事件。

### recordFailure / recordSuccess

`mu.Lock` 保护 `consecutiveFail` + `lastFailureAt`。失败 ++ + 记时间；成功重置 `consecutiveFail = 0`（不动 `lastFailureAt`）。

## 5. 依赖关系

### 外部包

- `context` / `log/slog` / `sort` / `strconv` / `sync` / `time`

### 内部包

- `github.com/ongridio/ongrid/internal/pkg/prom` —— `IncPromWrite(err)` 自观测指标
- `pkgpromwrite "github.com/ongridio/ongrid/internal/pkg/promwrite"` —— `Sample` / `Label` 类型 + `Client` 实现
- `github.com/ongridio/ongrid/internal/pkg/tunnel` —— `PromSample` 输入类型

### 被谁调用

- `frontierbound` 调 `Push`（host 样本）+ `PushKubernetes`（cluster 样本）
- alert pipeline 调 `HealthSnapshot` 评估 promwrite 健康

## 6. 并发与资源管理

- **`mu` 保护 health 字段**：`consecutiveFail` + `lastFailureAt` 在 `recordFailure` / `recordSuccess` 加锁
- **无 goroutine**：Ingester 不起 goroutine，调用方决定并发模型
- **无 batch**：每次 `Push` 一次性 `Write`，无内部缓冲。批量由调用方控制
- **无 retry**：失败直接 return err，调用方决定重试策略

## 7. 设计模式与亮点

### Reserved labels 防覆盖

edge 推上来的样本若含 `__name__` / `device_id` / `ongrid_source`，会被丢弃。注释：cloud 强制覆盖这些 label。这防 edge 伪造 ID（如把 `device_id` 改成别人的）。

### Host vs Kubernetes 分离

`Push` 写 `device_id`，`PushKubernetes` 写 `cluster_id`。注释：controller edge 不是 host Device，不带 `device_id`。Kubernetes reserved 集合更大（含 `device_id` / `edge_id`），防止 cluster 样本被误当 host 样本。

### 标签排序

`sort.Slice(labels, by name)`。Prom remote_write 硬要求标签按名排序，否则拒绝。这是协议层正确性。

### Health 双 surface

- `Health() Health` —— 结构体，本包内或紧密耦合调用方用
- `HealthSnapshot() (int, time.Time)` —— 原语，alert `HealthReporter` 用

注释：保持 alert 包接口无 struct 类型，让 alert 包不依赖此包的类型定义。

### `prom.IncPromWrite(err)` 与 health 一致

`write` 内 `prom.IncPromWrite(err)` 与 `recordFailure`/`recordSuccess` 同步调用。注释：让 `prom_write_total{result=fail}` counter 与 health snapshot（`Failures++`）报告同一事件，两个 surface 一致。

### nil Writer 静默 drop

`w == nil` 时 `Push` 静默 drop + debug log。注释："matches the spec's 'silent' choice"。degraded 模式不报错让 edge 不 spin on errors。

### `LastFailureAt` 成功不重置

`recordSuccess` 只重置 `consecutiveFail`，不动 `lastFailureAt`。注释：`lastFailureAt` 代表"最近一次失败 ever"，调用方按 grace window 老化。这让 alert 能区分"刚失败"与"很久前失败过"。

## 8. 注意事项

- **无 retry**：`write` 失败直接 return err。调用方（frontierbound）决定重试
- **无 batch 缓冲**：每次 `Push` 一次性 `Write`。高频小批量调用应积攒后批量 Push
- **`device_id` == legacy `edge_id`**：注释明确 post-split (May 2026) `deviceID` 是 HOST device 的 id，数字上等于 legacy `edge_id`（pre-launch backfill 复用整数）
- **reserved labels 静默丢弃**：edge 推上来的 reserved label 被丢，无 warning。这是安全设计（防伪造），但调试时可能困惑
- **`Health` 锁内 copy**：`Health()` 在锁内读两个字段返回结构体副本。安全，但高频调用可能有锁竞争
- **`PushKubernetes` reserved 含 `device_id`**：cluster 样本若带 `device_id` 会被丢。这是有意的 —— cluster 样本不该有 device_id
- **`source` 是 opaque string**：注释举例 "embedded:gopsutil" / "scrape:node-exporter"。manager 不解析，只当 label
- **`prom.IncPromWrite(nil)` 成功也计数**：成功时 `prom.IncPromWrite(nil)` 让 `prom_write_total{result=success}` 也增长，可算成功率
