# `subprocess_process_windows.go` 技术实现文档

> 源文件：`internal/skill/subprocess_process_windows.go`
> 包路径：`github.com/ongridio/ongrid/internal/skill`

## 1. 概述

`subprocess_process_windows.go` 是 `SubprocessSkill` 在 Windows 平台的占位实现：`configureSubprocessCommand` 为空函数，依赖 `exec.CommandContext` 的默认 cancel 行为。Windows 缺乏 Unix 风格的进程组与信号机制，平台特定的进程组级 SIGKILL 语义在此平台不可用。

## 2. 包信息

- **包名**：`skill`
- **所属模块**：`internal/skill`（框架核心层，平台特定子文件）
- **依赖方向**：被同包 `subprocess.go` 的 `realSubprocessRunner.Run` 调用 `configureSubprocessCommand`

## 3. 关键类型与接口

无显著类型定义。本文件仅提供一个平台特定函数：

```go
//go:build windows

func configureSubprocessCommand(*exec.Cmd)
```

## 4. 关键函数与流程

### `configureSubprocessCommand`
- **签名**：`func configureSubprocessCommand(*exec.Cmd)`
- **职责**：无操作（no-op）。函数体为空，仅接收 `*exec.Cmd` 参数。
- **流程**：直接返回，不对 `cmd` 做任何修改。
- **错误处理**：无。
- **设计意图**：保持与 Unix 版本同名的平台抽象函数存在，让 `subprocess.go` 平台无关代码无需 `//go:build` 分支即可调用；Windows 上完全依赖 `exec.CommandContext` 的默认 cancel（`cmd.Cancel` 为 nil 时，`exec` 包内部用 `cmd.Process.Kill()` 杀直接子进程）。

## 5. 依赖关系

- **内部包**：无
- **外部库**：`os/exec`（仅用于函数签名）
- **被调用方**：`subprocess.go` 的 `realSubprocessRunner.Run`

## 6. 并发与资源管理

无并发控制。本函数为 no-op，不涉及任何资源分配或释放。子进程的 cancel 行为由 `exec.CommandContext` 默认实现负责。

## 7. 设计模式与亮点

- **Platform-specific build tag**：`//go:build windows` 确保本文件只在 Windows 平台编译，与 `subprocess_process_unix.go` 形成互补。
- **No-op 占位模式**：保持平台抽象函数在所有平台都存在，调用方无需条件编译；Windows 上的语义降级由 `exec` 包默认行为兜底。
- **依赖标准库默认 cancel**：Go 1.20+ 的 `exec.CommandContext` 在 `cmd.Cancel` 为 nil 时会自动调用 `cmd.Process.Kill()`，足以覆盖"超时杀子进程"的基本需求。

## 8. 注意事项

- **无进程组语义**：Windows 不支持 Unix 进程组，`cmd.Process.Kill()` 只杀直接子进程；若子进程 fork 了后代（如 shell 调用子命令），后代可能成为孤儿继续运行。这是 Windows 平台的已知降级。
- **无 SIGKILL 强制收尸**：Windows 上 `Kill` 等价于 `TerminateProcess`，无法处理进程组；`cmd.WaitDelay`（在 `subprocess.go` 中设为 1s）仍生效，可强制收尸僵尸句柄。
- **Windows 上 skill pack 通常为 .bat/.ps1/.exe**：`SubprocessSkill.Execute` 的 `0o111` 可执行位校验在 Windows 上意义不大（Windows 不用 Unix 权限位），但 `os.Stat` 返回的 mode 在 Windows 上有可执行位模拟，校验仍可通过。
- **生产建议**：ongrid 生产部署通常在 Linux 上运行 manager/edge，Windows 支持主要为开发/测试便利；若需在 Windows 上跑外部 skill pack，需自行确认子进程的子进程清理策略。
- **未来扩展**：若要在 Windows 上实现进程树级 kill，需用 `JOBOBJECT_BASIC_LIMIT_INFORMATION` + `JobObjectAssociateCompletionPortInformation` 等 Job Object API，目前未实现。
