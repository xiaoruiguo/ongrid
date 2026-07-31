# `pipeline.go` 技术实现文档

> 源文件：`internal/manager/biz/alert/pipeline.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/alert`

## 1. 概述

本文件是 alert 子域的 `PipelineEvaluator`——pipeline-class 规则的 tick 驱动评估器。每 tick（默认 5min）顺序执行：`refreshDeviceStalenessGauge`（更新 `device_last_seen_seconds_ago` gauge）→ `evaluatePromQuery`（metric_raw，Phase-3 collapse 后 host 阈值也走这里）→ `evaluateMetricAnomaly` → `evaluateMetricForecast` → `evaluateMetricBurnRate` → `evaluateTraceLatency` → `evaluateTraceErrorRate` → `evaluateLogMatch` → `evaluateLogVolume`。同时定义 4 个数据源接口（`EdgeLister` / `PromQuerier` / `LogQuerier` / `DeviceIdentityResolver`）和共享 helper（`labelSetKey` / `mergeLabels` / `parseFloat` / `ruleSev` / `compareFloat` / `nonIdentityLabels` / `deviceDisplay`）。Phase-3 final collapse 后，legacy `HostMetricDecorator` 删除，所有 host-metric 告警统一走 metric_raw evaluator 的 30s Prom tick。

## 2. 包信息

- **包名**：`alert`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 `cmd` / main 装配调用 `Loop`；依赖 `internal/manager/biz/edge`（`ListFilter`）+ `internal/manager/model/alert` + `internal/manager/model/edge` + `internal/pkg/logquery` + `internal/pkg/notify` + `internal/pkg/prom` + `internal/pkg/promquery`

## 3. 关键类型与接口

```go
type EdgeLister interface {
    List(ctx context.Context, f edgebiz.ListFilter) ([]*edgemodel.Edge, error)
}

type PromQuerier interface {
    Query(ctx context.Context, expr string, ts time.Time) (*promquery.InstantResult, error)
}

type LogQuerier interface {
    QueryRange(ctx context.Context, opts logquery.QueryRangeOptions) (*logquery.QueryRangeResult, error)
}

type DeviceIdentity struct {
    Name, Hostname, IPAddress string
}

type DeviceIdentityResolver func(ctx context.Context, deviceID uint64) (DeviceIdentity, error)

type PipelineEvaluatorOpts struct {
    Usecase         *Usecase
    Rules           RulesProvider
    Notifier        Notifier
    Resolver        ChannelResolver
    Inhibitor       Inhibitor
    DefaultChannels []string
    Cooldown        time.Duration
    Interval        time.Duration
    EdgeLister      EdgeLister
    PromQuerier     PromQuerier
    LogQuerier      LogQuerier
    DeviceIdentityResolver DeviceIdentityResolver
    Log             *slog.Logger
    Now             func() time.Time
}

type PipelineEvaluator struct {
    uc        *Usecase
    rules     RulesProvider
    notifier  Notifier
    resolver  ChannelResolver
    inhibitor Inhibitor
    channels  []string
    cooldown  time.Duration
    interval  time.Duration
    edges          EdgeLister
    prom           PromQuerier
    logq           LogQuerier
    deviceIdentity DeviceIdentityResolver
    gaugeMu       sync.Mutex
    gaugeSnapshot map[string]string
    firingSnapshot map[string]map[string]struct{}
    log *slog.Logger
    now func() time.Time
}

// nonIdentityLabels 是不能分裂 alert identity 的来源标签。
var nonIdentityLabels = map[string]struct{}{
    "__name__":      {},
    "ongrid_source": {},
}
```

Sentinel 默认值：`Interval = 5 * time.Minute`、`Cooldown = 10 * time.Minute`。

## 4. 关键函数与流程

### `NewPipelineEvaluator`

- **签名**：`func NewPipelineEvaluator(opts PipelineEvaluatorOpts) *PipelineEvaluator`
- **职责**：构造 evaluator，应用默认值
- **流程**：
  1. `Interval <= 0` → 5min
  2. `Cooldown <= 0` → 10min
  3. `Log == nil` → `slog.Default()`
  4. `Now == nil` → `func() time.Time { return time.Now().UTC() }`
  5. 返回 `&PipelineEvaluator{...}`，`channels` 用 `append([]string(nil), opts.DefaultChannels...)` 拷贝防外部修改
- **错误处理**：无

### `Loop`

- **签名**：`func (e *PipelineEvaluator) Loop(ctx context.Context) error`
- **职责**：tick 驱动主循环
- **流程**：
  1. `e.uc == nil || e.rules == nil` → 返回 nil（未装配）
  2. `time.NewTicker(e.interval)` + defer Stop
  3. 首次 `e.evaluate(ctx)` 立即执行
  4. for 循环：select ctx.Done 返回 nil / tick.C 触发 `evaluate`
- **错误处理**：单 tick 错误在 `evaluate` 内部 Warn，不退出循环

### `EvaluateOnce`

- **签名**：`func (e *PipelineEvaluator) EvaluateOnce(ctx context.Context)`
- **职责**：暴露给测试的单 tick 入口

### `evaluate`

- **签名**：`func (e *PipelineEvaluator) evaluate(ctx context.Context)`
- **职责**：单 tick 顺序执行所有 evaluator
- **流程**：
  1. `now := e.now()`
  2. `e.edges != nil` → `refreshDeviceStalenessGauge(ctx, now)`（先刷新 gauge，让本 tick 的 metric_raw 规则看到新鲜值；host-staleness 信号 = `device_last_seen_seconds_ago > 90` 的 metric_raw 规则，无专用 edge_absence evaluator；检测延迟 ≤ 60s）
  3. `e.prom != nil` → 顺序执行 `evaluatePromQuery` / `evaluateMetricAnomaly` / `evaluateMetricForecast` / `evaluateMetricBurnRate` / `evaluateTraceLatency` / `evaluateTraceErrorRate`（trace kinds 查 Prom spanmetrics，共享 `prom != nil` gate）
  4. `e.logq != nil` → `evaluateLogMatch` / `evaluateLogVolume`
- **错误处理**：各 evaluator 内部 Warn，不阻断后续

### `refreshDeviceStalenessGauge`

- **签名**：`func (e *PipelineEvaluator) refreshDeviceStalenessGauge(ctx context.Context, now time.Time)`
- **职责**：更新 `device_last_seen_seconds_ago` Prom gauge，每个注册设备一个 series；离场设备 series 删除
- **流程**：
  1. `e.edges.List(ctx, ListFilter{Limit: 1000})`；err → Warn + return
  2. 构建 `current := map[string]string`（device_id → device_name）
  3. per-edge：
     - `lastSeen = edge.LastSeenAt ?? edge.CreatedAt`
     - `secs := now.Sub(lastSeen).Seconds()`；负数截 0
     - `deviceID := edge.ID`，若 `edge.DeviceID != nil && != 0` 用它（pre-launch backfill 让两者相等，幂等）
     - `prom.SetDeviceLastSeenSecondsAgo(idStr, edge.Name, secs)`
     - `current[idStr] = edge.Name`
  4. `gaugeMu.Lock`；`prev := e.gaugeSnapshot`；`e.gaugeSnapshot = current`；Unlock
  5. per-prev：若不在 current → `prom.DeleteDeviceLastSeenSecondsAgo(id, name)`（GC 离场设备 series，避免 device_id 复用导致 series 双叠）
- **错误处理**：list 失败 Warn + 跳过本 tick；单设备 gauge 错误不影响循环

### `evaluatePromQuery`

- **签名**：`func (e *PipelineEvaluator) evaluatePromQuery(ctx context.Context, now time.Time)`
- **职责**：每个 metric_raw 规则的 `Query(Expr)`，每个返回 vector entry 触发一个 incident（per-label-set dedupe key）
- **流程**：
  1. `rules := e.rules.MetricRawRules()`；空返回
  2. per-rule：`observeEval` 计时
  3. `e.prom.Query(ctx, rule.Expr, now)`；err → Warn + `evalErr=err` + continue
  4. 非 vector → continue
  5. json.Unmarshal 到 `[]vectorEntry{Metric, Value}`
  6. `scope := effectiveScope(rule.ScopeType, RuleKindMetricRaw)`
  7. `fired := make(map[string]struct{})` 跟踪本 tick dedupe key
  8. per-entry：
     - `valStr` 从 `Value[1]` 解码
     - `value, hasValue := parseFloat(valStr)`（即使无 value，series 存在即"谓词满足"）
     - `dedupeKey := fmt.Sprintf("pipeline:%s:%s", rule.RuleKey, labelSetKey(ent.Metric))`
     - `fired[dedupeKey] = struct{}{}`
     - summary：`<rule_key>: <expr> ⇒ <labelSetKey> (value=<valStr>)`
     - host scope 时从 `ent.Metric["device_id"]` 解析 devID
     - `RecordFiring` + `notify`
  9. **Recovery sweep**：`firingSnapshot[rule.RuleKey]` 中的 prev key 若不在本 tick fired → `SystemResolveIncident(ctx, prevKey, "prom condition cleared", now)`
  10. `firingSnapshot[rule.RuleKey] = fired`
- **错误处理**：`RecordFiring` 失败 Warn + continue；resolve 失败 Warn

### `notify`

- **签名**：`func (e *PipelineEvaluator) notify(ctx context.Context, res *FiringResult, summary, source string, at time.Time)`
- **职责**：构造 `notify.Message`，解析设备身份，调 `MaybeNotify`
- **流程**：
  1. `res == nil || res.Incident == nil` → 返回
  2. 构造 `notify.Message{Subject: summary, Severity, Source, DedupeKey, OccurredAt, Labels: {rule, incident_id}}`
  3. `res.Incident.DeviceID != nil`：
     - `msg.Labels["device_id"] = fmt.Sprintf("%d", deviceID)`
     - `e.deviceIdentity != nil` → 调用解析；成功且 `deviceDisplay(identity) != ""` → 替换 Subject 里的 `device_id=N` 为 `device=<display>`，加 `device_hostname` / `device_ip` label
  4. `msg.Severity == ""` → `SeverityWarning`
  5. `e.uc.MaybeNotify(ctx, res, msg, NotifyOpts{Notifier, Resolver, DefaultChannels, Cooldown, Inhibitor})`
- **错误处理**：device identity 解析失败 Warn（不阻断通知）

### `deviceDisplay`

- **签名**：`func deviceDisplay(identity DeviceIdentity) string`
- **职责**：渲染设备显示串
- **流程**：host = Hostname ?? Name；host && ip → `host (ip)`；仅 host → host；仅 ip → ip

### `labelSetKey`

- **签名**：`func labelSetKey(m map[string]string) string`
- **职责**：把 label set 序列化为稳定字符串，用于 dedupe key 后缀
- **流程**：
  1. 空 → `"_"`
  2. 排除 `nonIdentityLabels`（`__name__` / `ongrid_source`）——避免同一主题被 embedded/cloud 两个 collector 报告时产生两个 incident
  3. 手动插入排序（避免 import `sort` 仅为一个调用点）
  4. `parts := ["k=v", ...]` 用 `,` join
  5. 全被排除 → `"_"`
- **错误处理**：无

### `mergeLabels` / `parseFloat` / `ruleSev` / `compareFloat`

- `mergeLabels(layers ...map[string]string)`：合并多层 label，跳过 `__name__`；后层覆盖前层
- `parseFloat(s)`：`fmt.Sscanf` 解析；空 → `(0, false)`
- `ruleSev(s, def)`：s 非空返回 s，否则返回 `string(def)`
- `compareFloat(v, op, threshold)`：支持 `>` / `>=` / `<` / `<=` / `==` / `!=`；未知 op 返回 false

## 5. 依赖关系

- **内部包**：
  - `internal/manager/biz/edge`（`ListFilter`，仅类型——不调 edge 方法）
  - `internal/manager/model/alert`（`Rule*` 常量）
  - `internal/manager/model/edge`（`Edge` 类型）
  - `internal/pkg/logquery`、`internal/pkg/notify`、`internal/pkg/prom`、`internal/pkg/promquery`
- **外部库**：`encoding/json` / `fmt` / `log/slog` / `strconv` / `strings` / `sync` / `time`
- **被调用方**：`cmd` / main 装配；`Loop` 是 alert 子域的主驱动
- **依赖本包**：`usecase.go`（`Usecase` / `FiringResult` / `FiringInput` / `MaybeNotify` / `NotifyOpts` / `Notifier` / `ChannelResolver` / `Inhibitor`）、`rules.go`（`RulesProvider` / `effectiveScope`）、`evaluators_phaseA.go` / `evaluators_phaseB.go`（四个 evaluator 方法）

## 6. 并发与资源管理

- **`gaugeMu`（Mutex）**：保护 `gaugeSnapshot`——Loop 单 goroutine，但测试 `EvaluateOnce` 可能并发，故加锁
- **`firingSnapshot` 无锁**：注释明示 Loop 单 goroutine，测试也是；只在 `evaluatePromQuery` / Phase-B evaluator 中读写
- **`gaugeSnapshot` map 复用**：每 tick 重建 `current` map，与 prev diff 后 GC 离场 series
- **ticker 释放**：`Loop` 用 `defer tick.Stop()` 保证 ctx 取消时释放
- **ctx 透传**：所有 evaluator 走调用方 ctx；`MaybeNotify` 内部对 IO 操作走自己的 ctx

## 7. 设计模式与亮点

- **Phase-3 collapse**：host-metric 阈值从专用 `HostMetricDecorator`（push path 实时评估）迁移到 `evaluatePromQuery` 的 30s Prom tick——一个 evaluator、一个存储 shape（metric_raw）、一个恢复机制；`NoopHostMetricIngester` 是 legacy push path 的占位
- **expr 即谓词**：metric_raw 的 `Expr` 本身含 PromQL 比较运算符（`up == 0` / `cpu_pct > 90`），PromQL 自动过滤非匹配 series；evaluator 只需对返回 vector 每个 entry 触发 incident——client 端不需重复工作
- **per-label-set dedupe**：`pipeline:<rule_key>:<labelSetKey>`——同规则不同 device/service 独立 incident
- **`nonIdentityLabels` 合并**：`__name__` / `ongrid_source` 不参与 dedupe——同一主题被 embedded + cloud 两个 collector 报告时合并为一个 incident，避免重复告警
- **Recovery sweep**：上 tick fired 但本 tick 缺失 → 自动 resolve。PromQL 比较失败时 Prom 把 series 从响应中丢弃，"series 缺失"即"谓词不再满足"——干净利落
- **设备身份解析**：`DeviceIdentityResolver` 是 best-effort——incident 和 dedupe key 用稳定数字 ID，通知 Subject 里替换为人类可读 `hostname (ip)`；解析失败不影响通知
- **顺序评估**：`refreshDeviceStalenessGauge` 最先，让本 tick 的 metric_raw 规则看到新鲜 staleness 值
- **接口窄化**：`EdgeLister` / `PromQuerier` / `LogQuerier` 都是单方法接口，便于测试 fake
- **`labelSetKey` 手动排序**：注释明示"避免 import sort 仅为一个调用点"——克制依赖

## 8. 注意事项

- **默认 Interval 5min、Cooldown 10min**：注释提到 30s ticker 是 metric_raw 的典型配置；Interval 由 `cmd` 装配覆盖
- **检测延迟 ≤ 60s**：staleness gauge 30s ticker + 30s evaluator tick = 最多 60s 才能检测到 host offline
- **`gaugeSnapshot` 复用 edge.ID 作 device_id**：pre-launch backfill 让 `edge.id == host_device.id`；post entity-split 后用 `edge.DeviceID` 优先——幂等
- **`firingSnapshot` 跨 kind 共享**：rule_key 跨 kind 唯一，所以 `evaluatePromQuery` 与 Phase-B evaluator 共享同一 map 不冲突
- **`labelSetKey` 全排除 → `"_"`**：避免空 label set 产生空字符串 dedupe key
- **`parseFloat` 用 `Sscanf`**：比 `strconv.ParseFloat` 宽松（容忍前后空白），但对科学计数法可能不如 ParseFloat 稳健；本场景足够
- **`compareFloat` 未知 op 返回 false**：让规则在编译期拒绝未知 op；运行时双保险
- **`notify` 不调 investigator**：注释明示"Auto root-cause investigation fan-out happens upstream in `Usecase.recordFire`"——Pipeline 不需自己的 hook，`RecordFiring` 在 isNew 时触发 investigator
- **device identity 解析失败不阻断**：identity 是 best-effort；incident 用数字 ID，通知替换为可读名——失败时 Subject 保留 `device_id=N`
