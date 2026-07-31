# `runner.go` 技术实现文档

> 源文件：`internal/pkg/runner/runner.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/runner`

## 1. 概述

本文件定义统一的执行沙箱抽象（HLD-017）：所有"运行命令/脚本并注入 env"的路径（已安装的 skill、cloud-shell 工具、未来的外部 MCP server 启动）都通过这一个 `Runner` 接口，可插拔隔离后端（当前是 in-process shell，未来扩展 container / microVM）而不改任何调用方。本包不关心凭据解析（由 caller 通过 `Spec.Env` 注入已解析的凭据），保持依赖-free 与可单测。

## 2. 包信息

- **包名**：`runner`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被 skill 执行器、cloud-shell 工具调用；仅依赖标准库 `context`、`time`

## 3. 关键类型与接口

```go
type Isolation int

const (
    IsolationNone     Isolation = iota // in-process subprocess
    IsolationContainer                 // future
    IsolationMicroVM                   // future (firecracker/gVisor)
)

type Spec struct {
    Argv           []string          // argv[0]=binary；与 Script 互斥
    Script         string            // 走 /bin/sh -c
    Shell          []string          // 覆盖默认 shell
    Env            map[string]string // 完整 env（不继承 manager env）
    Workdir        string            // 空 → 临时目录
    Stdin          []byte
    Timeout        time.Duration     // 0 → DefaultTimeout
    MaxOutputBytes int                // 0 → DefaultMaxOutput
}

type Result struct {
    Stdout    string
    Stderr    string
    ExitCode  int
    Truncated bool
    Duration  time.Duration
}

type Runner interface {
    Run(ctx context.Context, spec Spec) (Result, error)
    Isolation() Isolation
}

const (
    DefaultTimeout   = 2 * time.Minute
    DefaultMaxOutput = 1 << 20 // 1 MiB per stream
)
```

## 4. 关键函数与流程

### `Isolation.String`
- **签名**：`func (i Isolation) String() string`
- **职责**：人类可读形式，用于日志与策略提示
- **流程**：switch 返回 `"shell"` / `"container"` / `"microvm"`

### （接口方法 `Run` / `Isolation`）
- 接口本身在此文件定义，实现见 `shell.go`（`ShellRunner`）

## 5. 依赖关系

- **内部包**：无
- **外部库**：无
- **被调用方**：skill 执行器、cloud-shell 工具、未来的 MCP server 启动器

## 6. 并发与资源管理

接口层无并发控制。`Spec` 是值类型，可安全并发传递。具体实现（`ShellRunner`）在 `shell.go` 中通过 `exec.CommandContext` + cap buffer 管理子进程资源。

## 7. 设计模式与亮点

- **策略模式 + 接口隔离**：`Runner` 接口屏蔽后端差异；`Isolation()` 让调用方/策略层决定命令类别是否被允许（如 `terraform apply` 可能要求 `>= IsolationContainer` 或人工 review）
- **凭据解耦**：runner 不解析凭据，只执行；caller 把 `biz/secret.ResolveInjection` 的结果填入 `Spec.Env`。这让 runner 保持依赖-free 与可单测
- **Env 完全替换语义**：注释明示 runner **不继承** manager 进程 env，防止 manager 的 `ONGRID_SECRET_KEY` / DB DSN 泄漏到子进程
- **DefaultMaxOutput 1 MiB/stream**：防止失控命令 OOM manager

## 8. 注意事项

- **`IsolationContainer` / `IsolationMicroVM` 是未来扩展点**：当前只有 `ShellRunner` 实现，调用方在策略层不能假设 container 后端已就绪
- **Workdir 必须存在**：注释要求 caller-supplied workdir 必须存在；空则 runner 自建临时目录并在结束后清理
- **ExitCode vs error 的语义**：非零退出是 Result 而非 error（见 `shell.go`）；调用方需检查 `Result.ExitCode` 而非只看 error
- **Timeout 0 → DefaultTimeout**：caller 必须显式设置更长 timeout 才能突破 2min 上限
- **Script 与 Argv 互斥**：注释要求两者只能设其一，但接口层未强制；实现层（`shell.go`）在两者都空时报错
