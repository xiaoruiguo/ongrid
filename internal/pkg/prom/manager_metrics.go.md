# `manager_metrics.go` 技术实现文档

> 源文件：`internal/pkg/prom/manager_metrics.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/prom`

## 1. 概述

该文件定义 manager 侧自观测（self-observability）metrics 集合与注册逻辑。所有 collector 为包级 var，热路径调用点（alert evaluator tick、promwrite Push、HTTP middleware、LLM router）无需通过构造函数穿透 struct handle。`RegisterManagerMetrics` 在 `cmd/ongrid/main.go` 启动时调用一次；重复调用降级为 warn 而非 panic。Label cardinality 严格闭环（kind / result / status 等有限集）。

## 2. 包信息

- **包名**：`prom`
- **所属模块**：`internal/pkg/`（基础设施层）
- 依赖方向：被 `cmd/ongrid` 启动注册、热路径代码（alert evaluator、promwrite、HTTP middleware、LLM router、chatruntime、investigator）调用；依赖 `prometheus/client_golang`。

## 3. 关键类型与接口

无显著类型定义（仅包级 var collector 与函数）。

### 包级 collector 变量

| 变量 | 类型 | 用途 |
|---|---|---|
| `AlertEvaluatorLatency` | `*HistogramVec` | evaluator 迭代延迟（kind / result） |
| `PromWriteTotal` | `*CounterVec` | remote_write 调用计数（result） |
| `DeviceLastSeenSecondsAgo` | `*GaugeVec` | 设备心跳新鲜度（device_id / device_name） |
| `AlertEventsTotal` | `*CounterVec` | alert_events 写入计数（event_type / severity / rule） |
| `HTTPRequestsTotal` | `*CounterVec` | API 请求计数（method / route / status） |
| `HTTPRequestDuration` | `*HistogramVec` | API 请求延迟（method / route） |
| `DBPoolOpenConns` / `DBPoolInUse` / `DBPoolIdle` | `Gauge` | DB 连接池采样 |
| `DBPoolWaitCountTotal` | `Counter` | DB 等待计数 |
| `LLMCallsTotal` | `*CounterVec` | LLM 调用计数（provider / model / status） |
| `LLMCallDuration` | `*HistogramVec` | LLM 调用延迟（provider / model） |
| `LLMTokensTotal` | `*CounterVec` | LLM token 消耗（provider / model / kind） |
| `ChatRuntimeWorkerSessions` | `*GaugeVec` | chatruntime worker 数（status） |
| `AlertEvalTicksTotal` | `*CounterVec` | evaluator tick 计数（rule_kind / status） |
| `EdgeConnections` | `*GaugeVec` | 隧道连接数（status） |
| `InvestigatorInflight` | `Gauge` | RCA investigator 并发数 |

### 包级 bucket 常量

- `alertEvaluatorBuckets`：`[0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10]`
- `httpRequestBuckets`：`[0.005, ..., 30]` 覆盖快速控制面到慢 LLM 代理
- `llmCallBuckets`：`[0.1, ..., 120]` LLM 通常秒级，尾部更粗

## 4. 关键函数与流程

### `RegisterManagerMetrics`
- **签名**：`func RegisterManagerMetrics(reg *prometheus.Registry, log *slog.Logger)`
- **职责**：构建并注册所有 manager 自观测 collector。
- **流程**：
  1. `reg == nil` → 回退 `prometheus.DefaultRegisterer` + warn。
  2. 用 `prometheus.NewHistogramVec` / `NewCounterVec` / `NewGaugeVec` / `NewGauge` / `NewCounter` 构建每个 collector。
  3. 通过 `registerOrExisting*` 辅助函数注册：成功则赋值包级 var；`AlreadyRegisteredError` 则 warn + 复用 existing；其他错误 panic。
  4. 注册 Go runtime + process collector：`NewGoCollector` + `NewProcessCollector`；`AlreadyRegisteredError` 静默忽略（避免测试 / 嵌入模式二次注册 panic）。
- **设计理由**：包级 var 让热路径无需穿透 handle；幂等注册让测试与嵌入模式安全。

### `registerOrExisting*` 家族（私有）
为各类型（Gauge / Counter / GaugeVec / CounterVec / HistogramVec）提供统一的"注册或复用 existing"逻辑：
- 注册成功 → 返回新 collector。
- `AlreadyRegisteredError` → warn + 返回 `are.ExistingCollector`（类型断言）。
- 其他错误 → panic。

### Observe / Inc / Set 辅助函数
| 函数 | 用途 |
|---|---|
| `ObserveHTTP(method, route, status, seconds)` | 记录一个 API 请求观测；status 用 `statusClass` 分桶为 2xx/3xx/4xx/5xx |
| `ObserveLLMCall(provider, model, status, seconds, inputTokens, outputTokens)` | 记录 LLM 调用 + token 消耗 |
| `SetWorkerSessions(running, pending)` | 更新 chatruntime worker gauge |
| `IncAlertEvalTick(ruleKind, status)` | 记录 evaluator tick |
| `SetEdgeConnections(connected, disconnected)` | 更新隧道 gauge |
| `ObserveAlertEvaluator(kind, seconds, err)` | 记录 evaluator 延迟（err 决定 result） |
| `IncPromWrite(err)` | 记录 remote_write 调用 |
| `SetDeviceLastSeenSecondsAgo(deviceID, deviceName, secondsAgo)` | 更新设备心跳 gauge |
| `DeleteDeviceLastSeenSecondsAgo(deviceID, deviceName)` | 删除已移除设备的 gauge series |
| `IncAlertEvent(eventType, severity, rule)` | 记录 alert_events 写入 |

所有辅助函数 nil-check 容错：collector 未注册（nil）时静默返回，避免热路径 panic。

### `statusClass`
- **签名**：`func statusClass(status int) string`
- **职责**：把 HTTP 状态码分桶为 `2xx` / `3xx` / `4xx` / `5xx` / `other`，控制 status label cardinality。

## 5. 依赖关系

- **内部包**：无。
- **外部库**：`github.com/prometheus/client_golang/prometheus` + `prometheus/collectors`；标准库 `errors` / `log/slog`。
- **被调用方**：`cmd/ongrid/main.go`（启动注册）、HTTP middleware、alert evaluator、promwrite ingester、LLM router、chatruntime、investigator、heartbeat ticker。

## 6. 并发与资源管理

无显式锁。prometheus collector 自身并发安全（内部原子操作）；包级 var 在 `RegisterManagerMetrics` 后只读。热路径调用 `WithLabelValues` / `Inc` / `Observe` / `Set` 都是并发安全的。

## 7. 设计模式与亮点

- **包级 var 模式**：所有 collector 是包级 var，热路径直接调用辅助函数，无需构造函数穿透 struct handle——降低 wiring 复杂度。
- **幂等注册**：`AlreadyRegisteredError` 降级为 warn + 复用 existing，让测试 / 嵌入模式多次注册安全。
- **nil-check 容错**：所有辅助函数检查 collector nil，未注册时静默返回，避免热路径 panic。
- **status 分桶**：`statusClass` 把 HTTP 状态码分桶为 2xx/3xx/4xx/5xx，控制 cardinality（避免每状态码一个 series）。
- **status label 取舍**：`HTTPRequestDuration` 不带 status label，成功与失败共享 histogram，避免 4× cardinality 爆炸。
- **cardinality 闭环**：注释明确每个 label 的闭集（kind / result / status / event_type 等），符合 gospec "高基数字段禁止作为 label" 红线。
- **历史命名留痕**：注释记录如 `device_last_seen_seconds_ago` 重命名（Edge ↔ Device 实体拆分）、`ongrid_llm_router_tokens_total` 命名冲突解决（v0.7.45 升级 panic 教训）。
- **Go runtime + process collector**：免费获得 goroutines / heap / GC / fd 指标。
- **DBPool 采样模式**：gauge 由 10s ticker 采样 `DBStats`，而非每查询观测，避免热路径开销。

## 8. 注意事项

- **`RegisterManagerMetrics` 必须启动时调用**：未调用时所有 collector 为 nil，辅助函数静默返回，metrics 全部缺失；运维需确认 main.go 调用。
- **panic on 非 AlreadyRegistered 错误**：注册失败（除已注册外）直接 panic，启动失败比静默丢指标更安全。
- **`InvestigatorInflight` 单 gauge 无 label**：MaxConcurrent 上限需从配置或日志另行查询。
- **`DeviceLastSeenSecondsAgo` 需手动清理**：设备删除后需调 `DeleteDeviceLastSeenSecondsAgo`，否则 series 永久残留（注释明确）。
- **`ongrid_llm_router_tokens_total` 命名特殊**：为避免与 legacy `ongrid_llm_tokens_total` 冲突加 `router` 后缀；新增 LLM metrics 需注意命名空间。
- **包级 var 全局可变**：虽符合"只读单例"例外，但仍违反"禁止全局可变变量"红线的精神；测试间共享全局 state 需注意隔离。
- **HTTP route label cardinality**：依赖 chi route template（如 `/v1/devices/{id}`）而非 full path，已闭环；但若 handler 未用 chi 或 route 含高基数参数需警惕。
- **`EdgeConnections` disconnected 短暂残留**：disconnected series 仅短暂存在用于优雅断开遥测，稳态查询应用 `status=connected`。
