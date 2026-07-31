# `subprocess.go` 技术实现文档

> 源文件：`internal/skill/subprocess.go`
> 包路径：`github.com/ongridio/ongrid/internal/skill`

## 1. 概述

`subprocess.go` 实现 `SubprocessSkill`——把外部可执行文件包装为 ongrid skill。框架将 args JSON 通过 stdin 喂给子进程，捕获 stdout 作为结果 JSON，超时/非零退出/非法 JSON 均转换为带 stderr 尾部的 error。这是 skills.sh / openclaw 风格 skill pack 接入框架的运行时载体，配合 `loader.go` 完成"零运行时依赖"的外部技能扩展。

## 2. 包信息

- **包名**：`skill`
- **所属模块**：`internal/skill`（框架核心层）
- **依赖方向**：被 `loader.go` 构造并注册；被 `Registry` 派发 `Execute`；调用同包 `Metadata`、`Class`、`Scope`

## 3. 关键类型与接口

```go
type SubprocessSkill struct {
    Meta      Metadata          // 框架可见元数据，Scope 强制 ScopeManager
    Schema    json.RawMessage   // 原始 JSON Schema，非空时覆盖 Params 派生
    Entry     string            // 可执行文件绝对路径
    EnvAllow  []string          // 转发给子进程的环境变量名白名单
    Timeout   time.Duration     // 子进程超时，0 = DefaultSubprocessTimeout
    runner    subprocessRunner  // 测试注入缝，nil 走真实实现
}

// 测试缝：注入伪造的请求捕获/ canned 响应
type subprocessRunner interface {
    Run(ctx context.Context, entry string, env []string, stdin []byte) (stdout, stderr []byte, exitCode int, err error)
}

// 生产实现，使用 exec.CommandContext
type realSubprocessRunner struct{}

// 限流 Writer，超 cap 后静默丢弃
type cappedWriter struct {
    w   io.Writer
    max int
    n   int
}
```

常量：
- `MaxSubprocessStdout = 16 MiB`：stdout 上限，防止恶意子进程打爆内存
- `MaxSubprocessStderrTail = 4 KiB`：stderr 尾部保留量，用于 error envelope
- `DefaultSubprocessTimeout = 30s`：清单未指定超时时的默认值

## 4. 关键函数与流程

### `SubprocessSkill.Metadata`
- **签名**：`func (s *SubprocessSkill) Metadata() Metadata`
- **职责**：返回框架可见元数据，强制 `Scope=ScopeManager`（外部二进制不能跑 edge），`Class` 空时降级 `ClassSafe`。
- **设计意图**：即便作者手写 `SubprocessSkill` 时设了 `ScopeHost`，也在此处被覆盖，防止误声明。

### `SubprocessSkill.Execute`
- **签名**：`func (s *SubprocessSkill) Execute(ctx context.Context, params json.RawMessage) (json.RawMessage, error)`
- **职责**：spawn 子进程、喂 stdin、捕获 stdout，返回结果 JSON。
- **流程**：
  1. nil receiver / Entry 空 / 非绝对路径 → 返回 error；
  2. `os.Stat(Entry)`：校验存在、非目录、可执行位（`0o111`）；
  3. params 空白 → 默认 `{}`；非合法 JSON → error；
  4. `context.WithTimeout(ctx, Timeout)`（Timeout<=0 用默认）；
  5. `buildSubprocessEnv` 构造白名单 env；
  6. 调用 `runner.Run`（runner 为 nil 时用 `realSubprocessRunner{}`）；
  7. 检查 `cctx.Err()==DeadlineExceeded` → 超时 error；
  8. `err != nil` → error 带 stderr 尾部；
  9. `code != 0` → error 带 exit code + stderr 尾部；
  10. stdout 非合法 JSON → error 带前 200B；
  11. 返回 raw stdout。
- **错误处理**：三类错误分层——校验错误（无 audit envelope）、超时（wrap DeadlineExceeded）、非零退出（带 stderr 尾部供审计 + LLM 查看）。

### `buildSubprocessEnv`
- **签名**：`func buildSubprocessEnv(allow []string) []string`
- **职责**：从空白 env 起步，仅转发 `allow` 列表中且 `os.LookupEnv` 命中的变量。
- **设计意图**：deny-by-default，连 `PATH` 都需显式 opt-in；返回空 slice（非 nil）以确保 `Cmd.Env` 不继承 `os.Environ`。

### `tailString`
- **签名**：`func tailString(b []byte, n int) string`
- **职责**：返回 b 的末尾 n 字节字符串，超长时前置 `...`，用于 stderr/error 预览。

### `realSubprocessRunner.Run`
- **签名**：`func (realSubprocessRunner) Run(ctx context.Context, entry string, env []string, stdin []byte) ([]byte, []byte, int, error)`
- **职责**：用 `exec.CommandContext` 启动子进程，捕获限流 stdout/stderr。
- **流程**：
  1. `exec.CommandContext(ctx, entry)`；`configureSubprocessCommand(cmd)`（平台特定，设置进程组与 SIGKILL 取消）；
  2. `cmd.WaitDelay = 1s`（子进程被 cancel 后等 1s 强收尸）；
  3. `cmd.Env = env`、`cmd.Stdin = bytes.NewReader(stdin)`；
  4. stdout 走 `cappedWriter{max: MaxSubprocessStdout}`，stderr 走 `cappedWriter{max: MaxSubprocessStderrTail*4}`；
  5. `cmd.Start()` 失败 → 返回 start error；
  6. `cmd.Wait()`：若为 `*exec.ExitError` 提取 ExitCode 并清空 waitErr；
  7. 返回 (stdout, stderr, exitCode, waitErr)。
- **错误处理**：区分"启动失败"与"非零退出"——前者带 error，后者仅带非零 exitCode 不带 error。

### `cappedWriter.Write`
- **签名**：`func (c *cappedWriter) Write(p []byte) (int, error)`
- **职责**：限流写入，超 cap 后静默丢弃并返回 `len(p), nil`（伪装写成功）。
- **设计意图**：返回 `ErrShortWrite` 会被 `exec` 当作非致命错误，混淆"skill 是否成功"；静默丢弃让 skill 正常完成，结果由 stdout 决定。

## 5. 依赖关系

- **内部包**：同包 `Metadata`、`Class`、`Scope`、`ClassSafe`、`ScopeManager`、`DefaultSubprocessTimeout`、`Metadata.Validate`
- **外部库**：`bytes`、`context`、`encoding/json`、`errors`、`fmt`、`io`、`os`、`os/exec`、`path/filepath`、`strings`、`time`
- **被调用方**：`loader.go`（构建实例）、`Registry`（派发 Execute）；平台特定部分由 `subprocess_process_unix.go` / `subprocess_process_windows.go` 提供

## 6. 并发与资源管理

- **`context.WithTimeout`** 强制子进程运行上限；cancel 由 `exec.CommandContext` + 平台特定的 `cmd.Cancel`（unix 用 `SIGKILL` 杀进程组）实现。
- **`cmd.WaitDelay = 1s`**：cancel 后 1s 内子进程未退出则强制收尸，避免僵尸进程。
- **`cappedWriter`** 限制 stdout/stderr 内存占用，防恶意子进程 OOM。
- **`SubprocessSkill` 本身无锁**：字段在注册后视为只读；`runner` 字段是测试期注入点，生产路径不变。
- 每次 `Execute` 独立构造 `exec.Cmd`，无跨调用共享状态，天然并发安全。

## 7. 设计模式与亮点

- **Strategy 模式（runner 注入）**：`subprocessRunner` 接口 + `runner` 字段是测试缝，生产用 `realSubprocessRunner`，测试用伪造 double 捕获请求/返回 canned 响应，无需真实 spawn 子进程。
- **Defense in Depth**：
  - Loader 层做 allowlist + symlink 校验；
  - `Execute` 层再做绝对路径 + stat + 可执行位校验；
  - env deny-by-default；
  - stdout/stderr 限流。
- **错误分层**：校验错误、超时、非零退出、非法 stdout 四类分别包装，便于审计日志与 LLM 上下文区分。
- **平台抽象**：`configureSubprocessCommand` 在 unix/windows 分别实现（见 `subprocess_process_*.go`），让本文件保持平台无关。
- **"stdout IS the result"对称契约**：与原生 Go skill 一致，调用方无需区分 native vs subprocess 的结果处理逻辑。

## 8. 注意事项

- **`Scope` 强制为 `ScopeManager`**：外部子进程 skill 永远跑在 manager 上，不能跑 edge；这是安全约束（无法在 edge 上沙箱化任意二进制）。
- **`EnvAllow` 空白时子进程无任何环境变量**：包括 `PATH`，shell 脚本类 skill 必须显式 opt-in `PATH` 或在 manifest 中声明所需 env。
- **`cappedWriter` 静默丢弃**：stdout 超 16MiB 后被截断，可能导致 JSON 不完整；`Execute` 末尾的 `json.Valid(stdout)` 会捕获并返回 error。
- **`cmd.Cancel` 平台差异**：unix 用 `SIGKILL` 杀进程组（`Setpgid:true`），windows 实现为空函数（依赖 `exec.CommandContext` 默认行为），windows 上子进程的子进程可能不会被一并杀掉。
- **`Stat` 不跟随 symlink**：刻意为之——allowlist 校验在 Loader 层做（用 `EvalSymlinks`），此处只断言文件存在且可执行，避免双重 symlink 解析开销。
- **`DefaultSubprocessTimeout=30s`**：长任务 skill 需在 manifest 显式设置 `timeout_seconds`，否则被 30s 截断。
- **`runner` 字段为 nil 时走真实实现**：手写 `SubprocessSkill` 直接放入 Registry 会使用 `realSubprocessRunner`，测试时通过反射或构造函数注入 double。
