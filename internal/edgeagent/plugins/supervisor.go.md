# `supervisor.go` 技术实现文档

> 源文件：`internal/edgeagent/plugins/supervisor.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/plugins`

## 1. 概述

`Supervisor` 拥有插件注册表，并周期性地（或被 `TriggerReload` 唤醒）从 `ConfigFetcher` 拉取最新配置快照，与当前已应用状态做 diff，对每个插件执行 Configure / Start / Stop 的 reconcile。它是 ongrid-edge 主进程内唯一的插件编排者，对外暴露 `HealthSnapshots()` 供心跳路径上报。

## 2. 包信息

- **包名**：`plugins`
- **所属模块**：`internal/edgeagent/plugins`
- **依赖方向**：被 `internal/edgeagent` main 调用（`NewSupervisor`/`Register`/`Run`/`TriggerReload`/`HealthSnapshots`）；调用各具体插件包的 `Plugin` 实现

## 3. 关键类型与接口

```go
type Supervisor struct {
    fetcher        ConfigFetcher
    reloadInterval time.Duration
    log            *slog.Logger

    mu      sync.Mutex
    plugins map[string]Plugin           // 注册表（启动期填充）
    current map[string]PluginConfig     // 上次已应用配置
    running map[string]bool             // 上次已启动状态

    reloadSignal chan struct{}           // 容量 1，coalesce 多次 TriggerReload
}

type SupervisorOpts struct {
    Fetcher        ConfigFetcher
    ReloadInterval time.Duration // 默认 60s
    Log            *slog.Logger
}
```

## 4. 关键函数与流程

### `NewSupervisor(opts) *Supervisor`
- **职责**：构造 Supervisor，初始化空注册表与 reloadSignal。
- **流程**：Logger 兜底 default；ReloadInterval ≤ 0 时取 60s；reloadSignal 容量 1（用于 coalesce）。

### `Register(p Plugin)`
- **职责**：把插件加入注册表，必须在 `Run` 前调用。同名后注册覆盖前者（便于测试）。

### `TriggerReload()`
- **职责**：通知 Supervisor 重新拉配置（如 manager 推 `plugin_config_changed` RPC 时）。
- **流程**：非阻塞 send 到 `reloadSignal`；槽满则丢弃（已有待处理 reload，coalesce 生效）。

### `HealthSnapshots() []PluginHealth`
- **职责**：返回所有注册插件的当前健康快照，供心跳上报。
- **流程**：加锁拷贝插件列表 → 释放锁逐个调 `HealthSnapshot()` → 按 Name 排序返回（稳定输出便于 diff / 测试）。

### `Run(ctx) error`
- **职责**：reconcile 主循环，直到 ctx 取消。
- **流程**：
  1. 立即 `reconcile(ctx)` 一次（冷启动应用配置）
  2. `time.NewTicker(reloadInterval)` 兜底轮询
  3. `select { ctx.Done → shutdown() return nil | reloadSignal → reconcile | tick.C → reconcile }`

### `reconcile(ctx)`
- **职责**：拉取 desired 配置，与 current diff，对每个插件执行 stop / configure+start / no-op。
- **流程**：
  1. `fetcher.Fetch(ctx)`；失败仅 Warn 并保留旧态（不重启插件）
  2. 日志输出 desired_count / enabled_count / enabled_names 用于诊断
  3. 加锁快照 `plugins`/`current`/`running` 三份副本后释放锁
  4. 遍历每个注册插件：
     - desired 缺失或 `Enabled=false` 且 `wasRunning=true` → `Stop(30s ctx)` → 更新 running/current
     - desired 启用 → `cfgChanged = !configEqual(prev, desCfg)` → 若变化先 `Configure(desCfg)`（失败则 continue 不启停）→ `needRestart = !wasRunning || cfgChanged` → 若 wasRunning 先 Stop → `Start(ctx)`（失败 continue）→ 更新 running/current
- **错误处理**：单插件失败仅 Warn 不影响其他插件；Configure 失败时跳过 Start 保持原态。

### `shutdown()`
- **职责**：ctx 取消时停止所有插件，bounded 30s grace。
- **流程**：拷贝插件列表 → `context.WithTimeout(Background, 30s)` → 逐个 `Stop`（失败仅 Warn）。

### `configEqual(a, b PluginConfig) bool`
- **职责**：语义相等比较，决定是否需要重新 Configure。
- **流程**：比较 Enabled/EdgeID/Endpoint/AuthUser/AuthPass + Spec 长度 + 逐 key 用 `fmt.Sprintf("%v")` 比较。注释指出 spec 小时这样够用，spec 变大可换 reflect.DeepEqual。

## 5. 依赖关系

- **内部包**：同包 `Plugin`/`PluginConfig`/`ConfigFetcher`
- **外部库**：标准库 `context`/`fmt`/`log/slog`/`sort`/`sync`/`time`
- **被调用方**：`internal/edgeagent` main、heartbeat 路径

## 6. 并发与资源管理

- `mu sync.Mutex` 保护 `plugins`/`current`/`running`。`reconcile` 在外层快照三份 map 后释放锁，逐插件操作期间不持锁——避免插件 Start/Stop 阻塞时锁住整个 Supervisor。
- `reloadSignal` 容量 1 channel，coalesce 短时间内多次 TriggerReload。
- `Run` 在独立 goroutine 运行；`Register`/`TriggerReload`/`HealthSnapshots` 由其他 goroutine 调用，全部经 `mu` 或 channel 同步。
- reconcile 中给 Stop 单独派生 30s timeout ctx，防止单插件卡死阻塞循环。

## 7. 设计模式与亮点

- **reconcile loop 模式**：desired vs current diff，类似 Kubernetes controller；安全网兜底 ticker 即便错失信号也会 60s 内自愈。
- **失败隔离**：单插件 Configure/Start/Stop 失败不影响其他插件，对应"一个插件坏不拉其他陪葬"的运维目标。
- **配置相等性短路**：`configEqual` 避免无变化时的无谓 Stop+Start 抖动（对 promtail 这类 Stop 会丢 positions 的插件尤其重要）。
- **shutdown 30s 上限**：防止单个 stuck 子进程劫持整个 edge 退出。

## 8. 注意事项

- `configEqual` 用 `fmt.Sprintf("%v")` 比较 Spec 值，对嵌套 map / slice 不可靠（顺序敏感）；当前 Spec 都是扁平 key→scalar，可接受，spec 复杂化后需替换。
- `reconcile` 中 `p.Stop(stopCtx)` 的错误被 `_ =` 忽略——注释里红线要求"禁止 `_ = fn()` 忽略错误（确实想丢弃必须注释说明）"，这里虽有注释说明但严格说违反规范，可考虑改 `if err := p.Stop(stopCtx); err != nil { log.Warn(...) }`。
- `Run` 在 ctx 取消后 `return nil`，不返回 shutdown 错误；调用方需通过日志感知关停过程。
- `Register` 后置覆盖同名插件是为测试方便，生产路径不应依赖该行为。
