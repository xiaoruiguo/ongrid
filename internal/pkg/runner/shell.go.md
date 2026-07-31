# `shell.go` 技术实现文档

> 源文件：`internal/pkg/runner/shell.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/runner`

## 1. 概述

本文件实现 `Runner` 接口的 `IsolationNone` 后端：`ShellRunner`，一个 in-process 子进程执行器。环境变量完全替换（不继承 manager env），输出大小与时间均受限，避免失控命令 OOM 或挂死 manager。设计上镜像 `internal/skill/subprocess.go` 的成熟旋钮，泛化到任意 argv/script。

## 2. 包信息

- **包名**：`runner`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被 skill 执行器、cloud-shell 工具调用；依赖标准库 `os/exec`、`bytes`、`sort`、`time`

## 3. 关键类型与接口

```go
type ShellRunner struct{} // 无字段；状态由 Spec 传入

// 内部：限长输出 buffer
type capBuffer struct {
    buf       bytes.Buffer
    max       int
    truncated bool
}
```

## 4. 关键函数与流程

### `NewShellRunner`
- **签名**：`func NewShellRunner() *ShellRunner`
- **职责**：构造无状态 shell 后端

### `ShellRunner.Isolation`
- **签名**：`func (*ShellRunner) Isolation() Isolation`
- **职责**：返回 `IsolationNone`

### `ShellRunner.Run`
- **签名**：`func (r *ShellRunner) Run(ctx context.Context, spec Spec) (Result, error)`
- **职责**：执行 Spec，返回受限的 Result
- **流程**：
  1. 校验：Argv 与 Script 都空时报错
  2. 应用默认值：timeout=DefaultTimeout（2min），maxOut=DefaultMaxOutput（1MiB）
  3. `context.WithTimeout` 派生 cctx，defer cancel
  4. 构造 `exec.Cmd`：
     - Script 模式：默认 `/bin/sh -c`，可通过 `spec.Shell` 覆盖
     - Argv 模式：`exec.CommandContext(cctx, Argv[0], Argv[1:]...)`
  5. Workdir：caller-supplied 必须存在；空则 `os.MkdirTemp("runner-")`，defer `os.RemoveAll`
  6. **Env 完全替换**：`cmd.Env = buildEnv(spec.Env)`，不继承 manager env
  7. Stdin：非空则用 `bytes.NewReader`
  8. Stdout/Stderr：用 `capBuffer` 限长
  9. `cmd.Run()`，记录 Duration
  10. 结果组装：
      - `cctx.Err() == DeadlineExceeded` → ExitCode=-1，返回 timeout error
      - `runErr` 非空且为 `*exec.ExitError` → 填 ExitCode，**返回 nil error**（非零退出是 Result 不是 runner 错误）
      - 其他 `runErr` → 返回 `runner: exec: %w`
- **错误处理**：mkdtemp 失败用 `%w` 包装；exec 失败区分 ExitError（正常结果）与系统错误（runner 错误）

### `buildEnv`
- **签名**：`func buildEnv(env map[string]string) []string`
- **职责**：env map → 排序后的 `KEY=VALUE` 切片
- **流程**：若 env 中无 `PATH`，复制一份并补默认 PATH（`/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`）；遍历拼接 `k+"="+v`；`sort.Strings` 排序
- **错误处理**：无（不会失败）

### `capBuffer.Write`
- **签名**：`func (c *capBuffer) Write(p []byte) (int, error)`
- **职责**：写入但不超过 max；超限后仍返回 `len(p), nil`（"假装消费"）防止 pipe 阻塞
- **流程**：
  - 已达 max → 标记 truncated，返回 `len(p), nil`
  - 部分超限 → 写入 `p[:room]`，标记 truncated，返回 `len(p), nil`
  - 未超限 → 正常 `buf.Write`

## 5. 依赖关系

- **内部包**：无
- **外部库**：无
- **被调用方**：skill 执行器、cloud-shell 工具

## 6. 并发与资源管理

- **`exec.CommandContext`**：cctx 派生自 caller ctx + timeout；超时后 SDK 会发送 SIGKILL 给子进程
- **临时目录清理**：`defer os.RemoveAll(tmp)` 保证不残留
- **`capBuffer` 假装消费**：避免 pipe 因满阻塞导致子进程 hang
- **`ShellRunner` 无状态**：可被多 goroutine 共享调用 `Run`
- **不持有任何全局可变状态**：符合"禁止全局可变变量"约束

## 7. 设计模式与亮点

- **Env 完全替换**：注释明示"NEVER inherit the manager process env"，防止 `ONGRID_SECRET_KEY` / DB DSN 等泄漏到子进程
- **ExitError vs runner error 区分**：非零退出是 Result 不是 error，让 caller 能区分"命令失败"与"执行器故障"
- **`capBuffer` 假装消费**：标准 `bytes.Buffer` 超限会返回 error，会让 `cmd.Run` 误判为失败；这里返回 nil 让管道继续排空
- **buildEnv 的不可变性**：当 caller 传入的 env 缺 PATH 时，先复制再修改，避免污染 caller 的 map
- **默认 PATH**：保证常见二进制可解析，caller 不必每次都设 PATH

## 8. 注意事项

- **`/bin/sh` 假设**：默认 shell 是 Unix sh；Windows 部署需 caller 显式传 `spec.Shell`
- **超时 SIGKILL**：`exec.CommandContext` 在 ctx 取消时发送 SIGKILL，子进程无法清理；若需要 graceful 需自行处理 SIGTERM
- **临时目录权限**：`os.MkdirTemp` 默认 0700（实际 Go 实现）；若子进程需要其他 uid 访问需 caller 自管 workdir
- **ExitCode=-1 表示超时**：调用方需特殊处理此值，不能假设 ExitCode >= 0
- **MaxOutputBytes 仅限 stdout/stderr 各 1MiB**：大输出场景需 caller 自行设更大的 `MaxOutputBytes` 或重定向到文件
- **不捕获子进程的子进程输出**：仅直接 stdout/stderr；若脚本启动后台进程输出会丢失
