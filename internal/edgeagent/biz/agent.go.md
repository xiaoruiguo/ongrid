# `agent.go` 技术实现文档

> 源文件：`internal/edgeagent/biz/agent.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/biz`

## 1. 概述

本文件定义 edge agent 的核心运行循环 `Agent`：负责注册云端→边缘的 RPC handler、拨号隧道、`register_edge` 握手、周期性心跳与指标推送，并通过升级信号通道驱动 systemd 优雅退出。它是 edge 进程的「主循环 + 编排器」。

## 2. 包信息

- **包名**：`biz`
- **所属模块**：edgeagent 顶层运行循环层
- **依赖方向**：被 `cmd/ongrid-edge` 构造并调用 `Run`；调用 `collector`、`skill`、`tunnel`

## 3. 关键类型与接口

```go
// Collector 是 metric 源的契约；embedded 与 scrape 两种实现都满足
type Collector interface {
    CollectAll(ctx context.Context) ([]CollectorOutput, error)
    HostInfo(ctx context.Context) (tunnel.HostInfo, error)
    GetHostLoad(ctx context.Context) (tunnel.GetHostLoadResponse, error)
    GetProcessList(ctx context.Context, topN int, sortBy string) (tunnel.GetProcessListResponse, error)
}

// CollectorOutput 是单个源的采集结果：HostPoint 走 legacy 快路径，Samples 走 prom 远写
type CollectorOutput struct {
    Source         string
    HostPoint      tunnel.HostMetricPoint
    HostPointValid bool
    Samples        []tunnel.PromSample
}

// Config 是 agent 运行参数；零值会被 NewAgent 填默认
type Config struct {
    HeartbeatInterval time.Duration // 默认 30s
    MetricsInterval  time.Duration // 默认 10s
    MetricsBatchSize int           // 默认 30
    AgentVersion     string
    Kubernetes       *tunnel.KubernetesInfo
    UpgradeStageDir  string // 空值禁用 MethodAgentUpgrade
}

// Agent 拥有 tunnel.Client、collector、配置、edge_id、升级信号通道
type Agent struct {
    client           tunnel.Client
    collector        Collector
    cfg              Config
    log              *slog.Logger
    edgeID           uint64
    mu               sync.RWMutex       // 保护 edgeID + pluginHealthFn
    registerMu       sync.Mutex         // 串行化 registerEdge
    upgradeRequested chan struct{}       // buffer 1，handler 触发后 Run 退出
    pluginHealthFn   func() []tunnel.PluginHealthWire
}
```

## 4. 关键函数与流程

### `NewAgent`
- **签名**：`func NewAgent(client tunnel.Client, collector Collector, cfg Config, log *slog.Logger) *Agent`
- **职责**：构造 Agent，为 Config 零值字段填默认（30s 心跳 / 10s 指标 / 30 batch）
- **流程**：直接返回结构体字面量，`upgradeRequested` 初始化为 buffer=1 通道
- **错误处理**：无错误返回；log 为 nil 时 fallback `slog.Default()`

### `Run`
- **签名**：`func (a *Agent) Run(ctx context.Context) error`
- **职责**：驱动 agent 完整生命周期
- **流程**：
  1. `registerHandlers()` 注册所有 cloud→edge handler（在 Dial 前完成，保证端点就绪）
  2. 注册 `OnReconnect` 回调：隧道重建后自动 `registerEdge` 重新绑定 edge_id
  3. `client.Dial(ctx)` 阻塞拨号；`context.Canceled` 时返回 nil
  4. `registerEdge` 握手；失败仅告警继续运行（周期循环会重试）；成功后写 `healthy_marker`（供 apply-pending-upgrade.sh 判断回滚）
  5. `errgroup.WithContext` 起三个 goroutine：`heartbeatLoop`、`metricsLoop`、`upgradeRequested` 监视
  6. 升级监视 goroutine 在收到信号时返回哨兵错误 `errUpgradeRequested`，触发 errgroup 取消兄弟 goroutine；Run 在返回前过滤哨兵回 nil，让 systemd 视为干净退出
- **错误处理**：Dial 失败返回 `fmt.Errorf("agent dial: %w", err)`；其他错误吞掉由循环重试

### `registerHandlers`
- **签名**：`func (a *Agent) registerHandlers()`
- **职责**：注册 `get_host_load`、`get_process_list`、`execute_skill`、`agent_upgrade`、`fetch_package`、`apply_package` 等 handler
- **流程**：通过 `client.RegisterHandler` 注册；`execute_skill` 走 `skilldispatch.Dispatch`；升级相关 handler 仅在 `UpgradeStageDir != ""` 时注册（dev/no-systemd 主机上 manager 看见 "method not found"）
- **错误处理**：handler 闭包内 `jsonDecode`/`jsonEncode` 错误直接返回

### `registerEdge`
- **签名**：`func (a *Agent) registerEdge(ctx context.Context) error`
- **职责**：执行 register_edge RPC 并存储 edge_id
- **流程**：`registerMu` 串行化；调 `collector.HostInfo` 收集主机身份；`applyKubernetesHostIdentity` 注入 k8s node 身份；发起 `MethodRegisterEdge` RPC；成功后写 `a.edgeID`
- **错误处理**：HostInfo 失败仅告警；RPC 错误返回

### `heartbeatLoop`
- **签名**：`func (a *Agent) heartbeatLoop(ctx context.Context) error`
- **职责**：每 HeartbeatInterval 发送一次心跳
- **流程**：
  1. 周期 tick；若 `edgeID==0`（首次注册失败）则先重试 `registerEdge`
  2. 调 `pluginHealthFn`（RLock 下读取）采集插件健康度
  3. 发 `MethodHeartbeat` RPC
  4. 失败时计数；连续失败触发 `registerEdge` 重新绑定传输；超过 `tunnelStuckThreshold=5` 后返回 `errTunnelStuck`
- **错误处理**：连续失败 ≥5 返回错误让 errgroup 取消兄弟

### `metricsLoop`
- **签名**：`func (a *Agent) metricsLoop(ctx context.Context) error`
- **职责**：周期采样并推送指标
- **流程**：tick → `collector.CollectAll` → 对每个 output 调 `pushOne`
- **错误处理**：CollectAll 失败仅告警，仍消费 partial slice

### `pushOne`
- **签名**：`func (a *Agent) pushOne(ctx context.Context, out CollectorOutput)`
- **职责**：将一个 CollectorOutput 的两半推送上云
- **流程**：
  1. HostPointValid → 发 `MethodPushHostMetrics`（15s 超时）
  2. Samples 非空 → 发 `MethodPushPromSamples`（15s 超时）
- **错误处理**：错误仅 Warn，下一 tick 用新数据重试；不缓冲 Prom 样本（过期样本无用）

### `writeHealthMarker`
- **签名**：`func (a *Agent) writeHealthMarker()`
- **职责**：在 `<stage>/healthy_marker` 写入 agent 版本，供下次启动 apply 脚本判断回滚
- **错误处理**：空 stage dir / 空版本直接 return；mkdir / 写文件失败仅 Debug 日志

## 5. 依赖关系

- **内部包**：`internal/edgeagent/skill`、`internal/pkg/tunnel`
- **外部库**：`golang.org/x/sync/errgroup`、标准库 `sync`、`log/slog`、`os`、`time`、`context`
- **被调用方**：`cmd/ongrid-edge` 主程序

## 6. 并发与资源管理

- **errgroup + 哨兵错误**：`errUpgradeRequested` 是哨兵，让 errgroup 取消兄弟 goroutine；Run 在返回前过滤回 nil 让 systemd 视为干净退出（关键：返回 nil 会让 ticker 永不停止，eg.Wait 永远阻塞——E2E 时发现的坑）
- **`sync.RWMutex`**：保护 `edgeID` 和 `pluginHealthFn`；心跳 goroutine RLock 读取，构造期 Lock 写入
- **`sync.Mutex` (`registerMu`)**：串行化 registerEdge，防止 OnReconnect 与心跳重试并发执行导致竞态
- **buffer=1 通道**：`upgradeRequested` 缓冲 1，handler 非阻塞发送；重复信号 harmless

## 7. 设计模式与亮点

- **哨兵错误 + errgroup 取消**：通过返回非 nil 错误让 errgroup 取消其他 goroutine，再在 Run 返回处过滤回 nil；避免手动管理 goroutine 生命周期
- **健康标记驱动 systemd 回滚**：apply-pending-upgrade.sh 在下次启动时读 `healthy_marker`，缺失或版本不匹配则回滚 `.previous`——agent 不关心回滚逻辑，只提供「新二进制启动 + register 成功」这一最强信号
- **Kubernetes node 身份注入**：`applyKubernetesHostIdentity` 用 `k8s-node:<clusterID>:<nodeUID>` 作为指纹，让 k8s 部署的 edge 节点身份稳定
- **handler 注册前置**：在 Dial 前注册，确保隧道一通即可接受 RPC，避免竞态窗口
- **recover 路径下沉到隧道层**：agent 不再自己匹配错误模式；tunnel.Client 透明处理 broker route invalidation → redial → fire OnReconnect

## 8. 注意事项

- `tunnelStuckThreshold=5`：连续 5 次心跳失败 + 重注册失败会 return error 让进程退出；运维需关注此阈值的告警
- `noopCollector`：Phase 1 兼容遗留 `New()` 构造器，返回零值；新代码应优先用 `NewAgent`
- `UpgradeStageDir=""` 时升级相关 handler 不注册，manager 看见 "method not found"——dev 环境的预期行为
- `healthy_marker` 写失败是 best-effort（dev / no-systemd 启动时 stage dir 不可写），不阻塞启动
- 心跳失败后的 re-register 会调 `writeHealthMarker`——若此前 marker 缺失会被补上
