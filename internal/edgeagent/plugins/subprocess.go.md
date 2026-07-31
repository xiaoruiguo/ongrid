# `subprocess.go` 技术实现文档

> 源文件：`internal/edgeagent/plugins/subprocess.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/plugins`

## 1. 概述

`SubprocessPlugin` 是子进程类插件（promtail / otelcol / node_exporter / process_exporter / mysqld_exporter 等）的通用底座。具体插件只提供二进制路径、配置渲染函数、CLI args 构造器；本文件统一负责：渲染配置写盘、启动子进程、捕获 stdout/stderr 到日志文件、崩溃后指数退避重启、健康快照维护。

## 2. 包信息

- **包名**：`plugins`
- **所属模块**：`internal/edgeagent/plugins`
- **依赖方向**：被 `logs`/`traces`/`hostmetrics`/`procmetrics` 等子进程插件包通过 `NewSubprocess` 构造；被 `Supervisor` 通过 `Plugin` 接口调用

## 3. 关键类型与接口

```go
type SubprocessPlugin struct {
    // 静态（构造时注入）
    name         string
    binary       string
    workDir      string
    configFile   string
    configRender func(PluginConfig) ([]byte, error)                 // nil 表示不写配置文件（如 node_exporter）
    args         func(cfg PluginConfig, configFile string) []string
    log          *slog.Logger

    // 可变运行态（受 mu 保护）
    mu          sync.Mutex
    cfg         PluginConfig
    cmd         *exec.Cmd
    cancelRun   context.CancelFunc
    health      PluginHealth
    wantRunning bool
    stoppedCh   chan struct{}
}

type SubprocessOpts struct {
    Name, Binary, WorkDir, ConfigFile string
    ConfigRender func(PluginConfig) ([]byte, error)
    Args          func(cfg PluginConfig, configFile string) []string
    Log           *slog.Logger
}
```

## 4. 关键函数与流程

### `NewSubprocess(opts SubprocessOpts) *SubprocessPlugin`
- **职责**：构造实例，初始化 health 状态为 `StateStopped`。
- **流程**：Logger 兜底为 `slog.Default()`；ConfigFile 缺省时为 `workDir/<name>.yaml`。
- **错误处理**：构造期不做 IO，不返回错误。

### `Configure(cfg PluginConfig) error`
- **职责**：把 `cfg` 渲染并写到 `configFile`；不主动启停子进程。
- **流程**：`mu.Lock` → `os.MkdirAll(workDir, 0o755)` → 若 `configRender != nil` 则渲染并 `os.WriteFile(configFile, body, 0o600)` → 保存 `cfg`。
- **错误处理**：mkdir / render / write 任一失败均用 `%w` 包装返回，Supervisor 据此决定是否标记 crashed。

### `Start(ctx) error`
- **职责**：启动子进程并 arm 崩溃重启循环；幂等（已运行则 no-op）。
- **流程**：加锁检查 `wantRunning` → `os.Stat(binary)` 确认二进制存在 → 置 `wantRunning=true` → 派生 `runCtx` 与 `cancelRun` → 创建 `stoppedCh` → 启 goroutine `runLoop(runCtx, stopped)`。
- **错误处理**：二进制缺失立即返回错误，不进入 runLoop。

### `Stop(ctx) error`
- **职责**：信号子进程退出并等 run loop 收尾；未运行时 no-op。
- **流程**：置 `wantRunning=false` → 调 `cancelRun()` → 在 `stopped`/`ctx.Done`/15s 超时三者间 select。
- **错误处理**：超时仅 Warn 后继续返回，避免 Supervisor 关停被卡住的子进程劫持。

### `runLoop(ctx, stopped)`
- **职责**：保持子进程存活，崩溃后指数退避重启。
- **流程**：`defer close(stopped)`；backoff 从 1s 起，封顶 5min。每轮：`setState(Starting)` → `runOnce(ctx)` → 若 `!stillWanted || ctx.Err()!=nil` 则 Stopped 退出 → 否则 `setState(Crashed, err)` → `select { ctx.Done | time.After(backoff) }` → `bumpRestart()` → backoff *= 2（封顶 5min）。
- **错误处理**：runOnce 返回的任何错误（包括"clean exit while running"）都视为崩溃。

### `runOnce(ctx) error`
- **职责**：spawn 一次子进程，捕获输出到日志文件，阻塞至其退出。
- **流程**：`exec.CommandContext(ctx, binary, args...)` → `cmd.Dir=workDir` → `cmd.Cancel = SIGTERM`、`cmd.WaitDelay=10s`（SIGKILL 兜底）→ 打开 `workDir/<name>.log` 追加模式 → `cmd.Stdout/Stderr = logFile` → `cmd.Start()` → 记录 PID/StartedAt → `setState(Running)` → `cmd.Wait()`。
- **错误处理**：`ctx.Canceled` 视为正常退出返回 nil；`*exec.ExitError` 报告退出码；其余错误直接返回。**clean exit 也视为崩溃**（业务期望子进程常驻），返回错误以触发重启。

### `HealthSnapshot() PluginHealth`
- 加锁拷贝 health，刷新 `UpdatedAt`。

## 5. 依赖关系

- **内部包**：无（同包 `Plugin`/`PluginConfig`/`PluginHealth`/`PluginState` 常量）
- **外部库**：标准库 `context`/`errors`/`fmt`/`io`/`log/slog`/`os`/`os/exec`/`path/filepath`/`sync`/`syscall`/`time`
- **被调用方**：`logs.New`、`traces.New`、`hostmetrics.New`、`procmetrics.New`

## 6. 并发与资源管理

- `mu sync.Mutex` 保护 `cfg`/`cmd`/`cancelRun`/`health`/`wantRunning`/`stoppedCh`。
- 每个子进程独占一个 goroutine (`runLoop`)；`Start`/`Stop`/`Configure`/`HealthSnapshot` 由 Supervisor 串行调用，但 `runLoop` 与上述方法并发访问共享态，故全部经 `mu`。
- `stoppedCh` 是 unbuffered close-channel，用于 `Stop` 等待 `runLoop` 退出。
- `context.WithCancel(ctx)` 派生 runCtx；`Stop` 调 cancel 触发 `cmd.Cancel`(SIGTERM) 与 runLoop 退出。
- 日志文件句柄在 `runOnce` 内 `defer close`，每次子进程退出后释放。

## 7. 设计模式与亮点

- **模板方法**：`SubprocessOpts` 把"如何渲染配置 / 构造 argv"留给子类，骨架统一处理生命周期。
- **指数退避重启**：1s→2s→4s→…→5min，RestartCount 单调递增，仅在 manager 推送新配置由 disabled→enabled 时清零。
- **clean exit 也算崩溃**：业务上子进程应常驻，意外退出（即便 exit 0）也触发重启。
- **SIGTERM + WaitDelay**：`cmd.Cancel` 发 SIGTERM，`WaitDelay=10s` 后由 exec 包自动 SIGKILL，平衡优雅退出与防卡死。
- **输出捕获到文件**：`workDir/<name>.log`，便于事后排查；不通过 ongrid-edge 结构化日志（避免大量子进程日志淹没主日志）。

## 8. 注意事项

- `Configure` 写配置文件用 `0o600`，但 workDir 用 `0o755`；若配置含敏感字段（如 traces 的 AuthPass）需确保 workDir 不被任意用户读取（部署期通过 ongrid-edge 用户 owner 控制）。
- `runOnce` 中打开日志文件失败会直接返回错误触发重启循环，极端情况下日志盘满会导致子进程永远起不来——可考虑日志文件创建失败时 fallback 到 `io.Discard`。
- `setState` 在 `st != StateRunning` 时清零 `health.PID`，避免上报僵尸 PID。
- 文件末尾的 `captureLine` 是占位（`var _ = func(r io.Reader) {}`），当前未使用，未来若要把子进程输出尾部 tail 进结构化日志可作为挂载点。
