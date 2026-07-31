# `sandbox.go` 技术实现文档

> 源文件：`internal/edgeagent/cmdpolicy/sandbox.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/cmdpolicy`

## 1. 概述

本文件是 cmdpolicy 的执行层：`Sandbox` 把 `Policy` 决策转化为真实的 `exec.Command` 行为。叠加 path 校验（复用 `host_files.SandboxConfig` 通过 `PathValidator` 接口）、network host allowlist 校验、exec.Command 管道接线（stdin→stdout Linux 风格）、scrubbed env、context 超时、输出 cap。提供 `Exec`（走 cmdpolicy）和 `ExecRaw`（admin write gate 旁路）两条路径。

## 2. 包信息

- **包名**：`cmdpolicy`
- **所属模块**：edgeagent 命令策略 + sandbox 执行层
- **依赖方向**：被 `bash/handlers.go` 调用；调用同包 `policy.go`、`types.go`

## 3. 关键类型与接口

```go
// PathValidator 是从 host_files.SandboxConfig 借用的窄接口
// cmdpolicy 不重新实现路径校验逻辑，host_files 是单一真相源
type PathValidator interface {
    ValidatePath(path string) error
}

// Sandbox 装配 Policy + PathValidator + Logger
type Sandbox struct {
    Policy        *Policy
    PathValidator PathValidator
    Logger        *slog.Logger
}

// ShellResult 是 wire-friendly 执行结果
type ShellResult struct {
    Allowed    bool   `json:"allowed"`
    Reason     string `json:"reason,omitempty"`
    Stdout     string `json:"stdout,omitempty"`
    Stderr     string `json:"stderr,omitempty"`
    ExitCode   int    `json:"exit_code"`
    Truncated  bool   `json:"truncated,omitempty"`
    DurationMs int64  `json:"duration_ms,omitempty"`
}

// capWriter 是带字节上限的 io.Writer
type capWriter struct {
    buf *bytes.Buffer
    cap int
}
```

## 4. 关键函数与流程

### `Sandbox.Decide`
- **签名**：`func (s *Sandbox) Decide(cmd string) Decision`
- **职责**：策略 + path + network 综合决策（dry-run UI / 审计用）
- **流程**：
  1. `s.Policy.Decide(cmd)` 策略层决策；不通过直接返回
  2. **Path 校验**：遍历所有 segment 的所有 argv（除 argv[0]），对 `/` 前缀 token 调 `PathValidator.ValidatePath`；nil validator 时跳过
  3. **Network host 校验**：对 ClassNetwork segment 用 `extractNetworkHost` 提取目标 host，`hostAllowed` 检查 allowlist；空 host（如 `dig` 无参）放行
- **错误处理**：path / network 不通过返回带 segment 索引 + arg 索引的 Reason

### `Sandbox.Exec`
- **签名**：`func (s *Sandbox) Exec(ctx context.Context, cmd string) (*ShellResult, error)`
- **职责**：走 cmdpolicy 执行命令
- **流程**：
  1. `Decide(cmd)`；不通过返回 `ShellResult{Allowed: false, Reason}`，error 为 nil
  2. `context.WithTimeout(ctx, Policy.Timeout)` 包装；`defer cancel()`
  3. `runPipeline(cctx, segments)` 真实执行
  4. 区分三种结果：
     - `context.DeadlineExceeded`：Stderr 写「command timed out」，ExitCode=-1
     - 其他 startup error：Stderr 写错误信息，ExitCode=-1
     - 正常完成：填 Stdout / Stderr / ExitCode / Truncated / DurationMs
- **错误处理**：error 仅表示「INTERNAL 失败」（启动失败）；策略拒绝 + 非零 exit 都通过 ShellResult 流转，**不**作为 error 返回

### `Sandbox.ExecRaw`
- **签名**：`func (s *Sandbox) ExecRaw(ctx context.Context, cmd string) (*ShellResult, error)`
- **职责**：admin write gate 旁路 — 通过 `/bin/sh -c` 执行，跳过所有 cmdpolicy 检查
- **流程**：
  1. 校验 sandbox 配置 + cmd 非空
  2. `context.WithTimeout(ctx, Policy.Timeout)`
  3. `exec.CommandContext(cctx, "/bin/sh", "-c", cmd)`
  4. scrubbed env（PATH / LANG / LC_ALL）
  5. `SysProcAttr.Setpgid = true` 设置进程组（便于超时时 kill 整条管道）
  6. capWriter 限流 stdout / stderr
  7. `c.Run()` 执行
  8. 区分超时 / startup error / 正常完成
- **错误处理**：与 Exec 一致；output caps + per-call timeout 仍生效（防止 runaway 命令卡隧道 slot 或淹没 LLM）

### `runPipeline`
- **签名**：`func (s *Sandbox) runPipeline(ctx context.Context, segments [][]string) (string, string, int, bool, error)`
- **职责**：把多段管道接成 stdin→stdout 链并运行
- **流程**：
  1. 每段查 `Policy.Lookup(seg[0]).AbsPath`，nil/empty 报错
  2. 构造 `exec.CommandContext(ctx, AbsPath, seg[1:]...)`；scrubbed env；Setpgid
  3. 接线：`cmds[i].StdoutPipe()` → `cmds[i+1].Stdin`
  4. 最后一段 stdout 写 capWriter；每段 stderr 各自 capWriter
  5. 顺序 Start（pipe 在 producer start 后才解析）
  6. 顺序 Wait；非末段的 broken pipe (SIGPIPE) 视为良性 continue
  7. 末段 `*exec.ExitError` 取 ExitCode
  8. ctx 超时返回 `context.DeadlineExceeded`
- **错误处理**：Start 失败时 kill 已启动的 cmd；Wait 错误按位置分类处理

### `capWriter`
- **签名**：`func (w *capWriter) Write(p []byte) (int, error)`
- **职责**：带字节上限的 writer，超限静默丢弃
- **流程**：cap <= 0 无限写；超限时假装成功（`return len(p), nil`）让上游继续流式不报错；截断通过 `capHit` 在结果中标记
- **错误处理**：从不返回错误，避免上游因 EPIPE 退出

### 网络辅助函数
- `extractNetworkHost(seg)`：按 binary 启发式提取目标 host（curl/wget 取首个 URL；dig/host/nslookup 取首个非 flag 非 `@` 参数；ping/traceroute 取最后 positional；nc 取首个 positional）
- `hostFromURL(s)`：剥 scheme / path / user@ / port，支持 IPv6 `[::1]:8080`
- `hostAllowed(host, allowlist)`：空 list 拒绝所有；CIDR / hostname suffix (`.internal`) / 精确匹配三种规则

## 5. 依赖关系

- **内部包**：无（PathValidator 是接口，由 host_files 注入实现）
- **外部库**：`bytes`、`context`、`errors`、`fmt`、`io`、`log/slog`、`net`、`os/exec`、`strings`、`syscall`、`time`
- **被调用方**：`bash/handlers.go::Register` 构造 `&cmdpolicy.Sandbox{...}` 并调用 `Exec` / `ExecRaw`

## 6. 并发与资源管理

- **Sandbox 是值类型共享**：Policy 不可变（构造后只读），PathValidator 也只读；多个 handler goroutine 可并发调用 `Exec` 而无需加锁
- **每调用独立 context**：`context.WithTimeout(ctx, Policy.Timeout)` + `defer cancel()`
- **进程组**：`SysProcAttr.Setpgid = true` 让超时时可 kill 整条管道（包括子进程）
- **capWriter 不返回错误**：避免上游因 EPIPE 提前退出导致输出截断被误判为「进程崩溃」

## 7. 设计模式与亮点

- **PathValidator 窄接口注入**：cmdpolicy 不重新实现路径校验，借用 `host_files.SandboxConfig.ValidatePath`——单一真相源，避免两套 allowlist 漂移
- **三层决策叠加**：Policy.Decide（策略）→ Sandbox.Decide 叠加 path 校验 → Sandbox.Decide 叠加 network 校验；每层可独立调用（dry-run UI 用 Decide，executor 用 Exec）
- **ExecRaw 旁路但仍守 output cap + timeout**：write gate 旁路策略是设计意图，但 cap/timeout 仍生效——防止 runaway 命令卡隧道 slot
- **broken pipe 良性处理**：管道中非末段因下游提前退出而 SIGPIPE 是正常行为（如 `cat huge | head -1`），不报错
- **scrubbed env**：仅传 `PATH` / `LANG` / `LC_ALL`，防止 LLM 通过 env 变量泄露 / 污染
- **host allowlist 三种规则**：CIDR（10.0.0.0/8）/ hostname suffix（`.internal`）/ 精确匹配；空 list = 拒绝所有出站（默认安全）
- **exec.Command 而非 shell**：argv 直接传给 execve，从不调用 `/bin/sh`（除非 ExecRaw 显式旁路）

## 8. 注意事项

- `ExecRaw` 完全绕过 cmdpolicy（含二进制 allowlist、denied class、path/network allowlist、shell 元字符语法）——仅 `BashExecRequest.Unrestricted=true` 时由 bash handler 调用，对应 operator 开启 write gate
- `capWriter` 假装成功（return len(p), nil）让上游不报错——但截断后下游会看到不完整输出；ShellResult.Truncated=true 让 LLM 知道被截断
- `extractNetworkHost` 是启发式——`curl --resolve foo:443:10.0.0.1 https://bar` 会取 `bar` 而非真实 IP；allowlist 校验可能误判。这是已知限制，operator 应在 allowlist 中允许域名后缀
- `hostAllowed` 的 hostname suffix 必须以 `.` 开头（`.internal`）；裸 `internal` 会精确匹配而非后缀
- IPv6 host 提取：`[::1]:8080` 取 `::1`；但 allowlist 中 IPv6 CIDR（`::1/128`）也支持
- 默认 `NetworkHostAllowlist=nil`（拒绝所有出站）；operator 必须显式配置才能让 LLM ping 内部服务
- `runPipeline` 中 Start 失败时 kill 已启动 cmd——但已 Start 的 cmd 的 Wait 仍需调用以回收僵尸进程；当前实现仅 Kill 不 Wait，可能短暂残留 zombie（systemd 会回收）
