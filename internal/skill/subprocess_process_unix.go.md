# `subprocess_process_unix.go` 技术实现文档

> 源文件：`internal/skill/subprocess_process_unix.go`
> 包路径：`github.com/ongridio/ongrid/internal/skill`

## 1. 概述

`subprocess_process_unix.go` 是 `SubprocessSkill` 在 Unix 系（Linux/macOS/BSD 等）的平台特定实现：为 `exec.Cmd` 配置进程组隔离与超时取消回调。它把"子进程被 cancel 时连同其子孙进程一起 SIGKILL"的语义落地，避免子进程 fork 出的后代在父超时后继续运行成为孤儿。

## 2. 包信息

- **包名**：`skill`
- **所属模块**：`internal/skill`（框架核心层，平台特定子文件）
- **依赖方向**：被同包 `subprocess.go` 的 `realSubprocessRunner.Run` 调用 `configureSubprocessCommand`

## 3. 关键类型与接口

无显著类型定义。本文件仅提供一个平台特定函数：

```go
//go:build !windows

func configureSubprocessCommand(cmd *exec.Cmd)
```

## 4. 关键函数与流程

### `configureSubprocessCommand`
- **签名**：`func configureSubprocessCommand(cmd *exec.Cmd)`
- **职责**：为 `exec.Cmd` 设置 Unix 平台的进程控制属性。
- **流程**：
  1. `cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}`——将子进程设为新进程组的 leader（PGID = 子进程 PID）；
  2. `cmd.Cancel = func() error { ... }`——当 `ctx` 被取消（含超时）时执行的回调：
     - `cmd.Process == nil` → 直接返回 nil（未启动）；
     - `syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)`——向负 PID（即进程组）发送 SIGKILL，杀掉子进程及其所有后代；
     - 若 `Kill` 返回 `ESRCH`（进程不存在）→ 返回 `os.ErrProcessDone`（告知 `exec` 已收尸）；
     - 其他 `Kill` 错误 → fallback 调用 `cmd.Process.Kill()`（单进程 kill）；
     - 成功 → 返回 nil。
- **错误处理**：`cmd.Cancel` 返回的 error 会被 `exec` 包记录但不会改变 `cmd.Wait()` 的语义；`ESRCH` 转 `os.ErrProcessDone` 是为了让 `exec` 跳过不必要的等待。

## 5. 依赖关系

- **内部包**：无
- **外部库**：`errors`、`os`、`os/exec`、`syscall`
- **被调用方**：`subprocess.go` 的 `realSubprocessRunner.Run`

## 6. 并发与资源管理

- **进程组隔离**：`Setpgid:true` 让子进程及其 fork 后代成为独立进程组，父进程超时 cancel 时通过 `-PID` 一次性杀整组。
- **`cmd.Cancel` 由 `exec.CommandContext` 在 ctx 取消时同步调用**，无需额外 goroutine。
- **`cmd.WaitDelay`**（在 `subprocess.go` 中设为 1s）配合 `Cancel`：cancel 后 1s 内子进程未退出则强制收尸，避免僵尸进程。
- **`ESRCH` 处理**：进程组在 cancel 触发前已自然退出时，`Kill` 返回 `ESRCH`，转 `os.ErrProcessDone` 避免 `exec` 误报 error。

## 7. 设计模式与亮点

- **Platform-specific build tag**：`//go:build !windows` 确保本文件只在非 Windows 平台编译，与 `subprocess_process_windows.go` 形成互补。
- **进程组级 SIGKILL**：用 `-PID` 杀进程组而非单 PID，解决"子进程 fork 了 shell，shell fork 了实际命令"的孤儿问题——单纯 `cmd.Process.Kill()` 只杀直接子进程，后代会继续运行。
- **ESRCH 优雅降级**：处理 cancel 与自然退出的竞态，避免无意义的 error 日志。
- **Fallback 链**：`Kill(-PGID)` → `cmd.Process.Kill()`，确保即便进程组信号失败（如权限问题）仍能尝试杀掉直接子进程。

## 8. 注意事项

- **`Setpgid:true` 让子进程脱离父进程的进程组**：父进程（ongrid manager）收到 Ctrl+C 时不会自动传递 SIGINT 给子进程；这是有意为之，避免 LLM skill 被运维信号意外打断。
- **SIGKILL 不可被捕获/忽略**：子进程无法做 graceful shutdown，正在写入的文件可能损坏；当前设计优先保证 manager 不会被恶意/卡死子进程拖累。
- **`cmd.Cancel` 在 Go 1.20+ 才支持**：项目需保证 Go 版本 ≥ 1.20。
- **macOS 上 `syscall.Kill(-PGID, SIGKILL)` 行为一致**：BSD 系 syscall 语义与 Linux 相同，无需额外分支。
- **进程组 PID = 子进程 PID**：依赖 `Setpgid:true` 后子进程 PID 即为 PGID；若未来改为 `Setpgid:false` 则 `-PID` 语义变化，需同步调整。
- **无资源清理回调**：子进程持有的文件锁/网络连接由 SIGKILL 强制释放，不保证 flush；长任务 skill 应自行处理信号（虽然 SIGKILL 无法捕获，但可用 `cmd.WaitDelay` 期间的自然退出路径）。
