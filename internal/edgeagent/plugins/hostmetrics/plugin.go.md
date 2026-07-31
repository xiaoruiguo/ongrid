# `hostmetrics/plugin.go` 技术实现文档

> 源文件：`internal/edgeagent/plugins/hostmetrics/plugin.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/plugins/hostmetrics`

## 1. 概述

`hostmetrics` 是边缘端主机指标插件，包装 `node_exporter` 子进程在可配置端口（默认 :9102，避开 manager 容器占用的 9100）暴露 `node_*` 指标，供 manager 侧 Prometheus 通过 docker bridge 抓取。除子进程外，本插件还运行一个 in-process 补偿指标生产者：每 15s 把 `/proc/sys/net/netfilter/nf_conntrack_*` 写成 `textfile/conntrack.prom` 文件，让 node_exporter 的 textfile collector 拾取——绕过 node_exporter 1.8.2 在现代内核上 conntrack collector 静默失效的 bug。

## 2. 包信息

- **包名**：`hostmetrics`
- **所属模块**：`internal/edgeagent/plugins/hostmetrics`
- **依赖方向**：被 main 注册到 Supervisor；调用 `internal/edgeagent/plugins`（SubprocessPlugin/PluginConfig）

## 3. 关键类型与接口

```go
const (
    Name                  = "hostmetrics"
    DefaultListenAddress  = ":9102"
    supplementaryInterval = 15 * time.Second
    conntrackCountPath    = "/proc/sys/net/netfilter/nf_conntrack_count"
    conntrackMaxPath      = "/proc/sys/net/netfilter/nf_conntrack_max"
    conntrackLegacyCountPath = "/proc/sys/net/nf_conntrack_count"
)

type plugin struct {
    sub         plugins.Plugin       // 包装的 SubprocessPlugin
    textfileDir string
    log         *slog.Logger

    mu     sync.Mutex
    cancel context.CancelFunc
    wg     sync.WaitGroup
}
```

实现 `plugins.Plugin`（委托 sub + 自管 producer）。

## 4. 关键函数与流程

### `New(binDir, workDir, log) plugins.Plugin`
- **职责**：构造 SubprocessPlugin + 补偿生产者。
- **流程**：
  1. `pluginWorkDir = workDir/hostmetrics`、`textfileDir = pluginWorkDir/textfile`
  2. `NewSubprocess(SubprocessOpts)`：
     - Binary = `binDir/node_exporter`
     - WorkDir = pluginWorkDir
     - ConfigFile = `pluginWorkDir/spec.snapshot`（占位，ConfigRender=nil 不写文件）
     - Args 闭包：调 `buildArgs(cfg, path)` 后 append `--collector.textfile.directory=<textfileDir>`（放在末尾，让用户 extra_args 可覆盖）
  3. 返回 `&plugin{sub, textfileDir, log}`

### `Name()`/`Configure(cfg)`/`HealthSnapshot()`
- 直接委托给 `sub`。

### `Start(ctx) error`
- **职责**：起子进程 + 起补偿生产者 goroutine。
- **流程**：
  1. `os.MkdirAll(textfileDir, 0o755)` 确保 textfile 目录存在
  2. `sub.Start(ctx)`
  3. 加锁：若已有 cancel 取消旧 producer（防 re-Start 多实例）→ 派生 `pctx`（基于 Background，独立于 sub 的 ctx，使 producer 在 sub 重启时仍存活）→ 记 cancel
  4. `wg.Add(1)` → `go runSupplementaryProducer(pctx)`
- **错误处理**：mkdir/sub.Start 失败直接返回。

### `Stop(ctx) error`
- **职责**：先停 producer 再停 sub（反向避免 producer 写入已 teardown 的 workDir）。
- **流程**：加锁 cancel producer → `wg.Wait()` → `sub.Stop(ctx)`。

### `runSupplementaryProducer(ctx)`
- **职责**：周期写 conntrack textfile。
- **流程**：`defer wg.Done()` → 立即 `writeConntrackTextfile()` 一次 → `ticker = supplementaryInterval` → select `ctx.Done`/`tick.C`。
- **错误处理**：单次写失败仅 Warn，不影响下周期。

### `writeConntrackTextfile()`
- **职责**：写 `textfileDir/conntrack.prom`。
- **流程**：
  1. `os.Stat(conntrackLegacyCountPath)`：若存在直接 return（旧内核 node_exporter 内置 collector 已发，避免双发样本冲突）
  2. 读 `conntrackCountPath` 与 `conntrackMaxPath`：任一失败静默 return（模块未加载 / 容器环境，每 15s 日志会刷屏）
  3. 拼接 Prometheus 文本格式（HELP/TYPE + gauge 行）
  4. 写到 `target.tmp` → `os.Rename(tmp, target)` 原子替换（避免 node_exporter 读到半写文件）
- **错误处理**：写 / rename 失败 Warn。

### `buildArgs(cfg, _) []string`
- **职责**：构造 node_exporter CLI args。
- **流程**：`--web.listen-address=<listen>`（默认 :9102）+ `--collector.<c>` per collectors_enabled + `--no-collector.<c>` per collectors_disabled + extra_args 原样追加。

### `stringSpec(cfg, key, def)`/`stringSliceSpec(cfg, key)`
- 从 `cfg.Spec` 取值，容忍 JSON interface{} 形态。

## 5. 依赖关系

- **内部包**：`internal/edgeagent/plugins`
- **外部库**：标准库 `context`/`fmt`/`log/slog`/`os`/`path/filepath`/`strings`/`sync`/`time`
- **外部二进制**：`node_exporter`（由 `binDir` 提供）
- **被调用方**：main.go

## 6. 并发与资源管理

- `mu sync.Mutex` 保护 `cancel`；`wg sync.WaitGroup` 等待 producer goroutine 退出。
- producer 用独立 `context.Background()` 派生的 pctx，**不**继承 Start 的 ctx——这样 Supervisor 触发 sub 重启时（Stop→Start），producer 不会被取消；只有显式 Stop 或 re-Start 才取消旧 producer。
- `writeConntrackTextfile` 用 tmp + rename 原子写，避免与 node_exporter 并发读的竞态。
- textfile 目录权限 0o755，文件 0o644——node_exporter 进程需可读。

## 7. 设计模式与亮点

- **装饰器模式**：`plugin` 包装 `SubprocessPlugin`，委托 Name/Configure/HealthSnapshot，仅 Start/Stop 自管 producer——最小侵入扩展子进程插件。
- **textfile collector 桥接**：通过 node_exporter 的 textfile collector 把 in-process 生成的指标注入 /metrics，复用已有 scrape pipeline，无需 manager 侧改动。
- **legacy path 检测避免双发**：旧内核 node_exporter 内置 collector 已发 conntrack 指标，本插件 stat legacy path 存在则静默退出，避免 Prometheus 因重复样本拒绝整个 scrape。
- **producer 生命周期独立**：producer 用 Background ctx，sub 重启不影响 producer——避免重启瞬间 conntrack 指标短暂缺失。
- **Stop 顺序反向**：先停 producer 再停 sub，防止 producer 在 sub 已退出后仍尝试写 textfile（虽然写不会失败但 node_exporter 已不在读）。
- **CLI args 顺序**：textfile 目录参数放末尾，让用户 extra_args 可覆盖（escape hatch）。

## 8. 注意事项

- `writeConntrackTextfile` 在容器/最小主机上读 `/proc/sys/net/netfilter/nf_conntrack_count` 失败时静默 return，每 15s 一次无日志——正确行为（避免日志刷屏），但运维需知该指标在无 conntrack 模块时缺省。
- `DefaultListenAddress = ":9102"` 注释说明避开 9100（manager 容器占用），但若 edge 与 manager 同主机部署且 manager 也绑 9102 会冲突——部署期需注意端口规划。
- `Start` 中 `os.MkdirAll(textfileDir, 0o755)` 失败直接返回错误，但若 sub.Start 已在前面成功，会导致 sub 运行但 plugin.Start 返回错误——Supervisor 会标记 crashed 但 sub 实际在跑，下次 reconcile 可能重复 Start（SubprocessPlugin 幂等所以无害，但状态语义不准）。建议先 mkdir 再 sub.Start。
- `runSupplementaryProducer` 用 `context.Background()` 派生 pctx，意味着 edge 进程退出时若 Supervisor.Run 的 ctx 取消，sub.Stop 会被调但 producer 仅靠 plugin.Stop 取消——若 Supervisor.shutdown 调 plugin.Stop 流程正常，producer 会被 cancel，无泄漏。
- textfile 文件权限 0o644，host 上其他用户可读——conntrack 数量不算敏感，可接受；若未来扩展到其他补偿指标需评估。
- `buildArgs` 中 `extra_args` 原样追加，未做白名单校验——操作员可通过 extra_args 注入任意 node_exporter flag，manager UI 应做基本校验。
