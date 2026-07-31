# `bash.go` 技术实现文档

> 源文件：`internal/pkg/tunnel/bash.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/tunnel`

## 1. 概述

本文件定义通用 shell 执行的 wire 协议常量与请求/响应结构（method `bash.exec`）。v1 走只读策略（由 edge 端 cmdpolicy 沙箱强制），未来可变体 bash 复用同一 wire 但换策略预设。Wire shape 极简：一个 Cmd 字符串加可选 timeout，解析与校验全部在 edge 侧完成（单一真理源）。

## 2. 包信息

- **包名**：`tunnel`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被 manager 侧 `internal/manager/biz/aiops/tools/bash_basetool.go` 调用；被 edge 侧 `internal/edgeagent/bash/handlers.go` 处理；本文件无 import

## 3. 关键类型与接口

```go
const MethodBashExec = "bash.exec"

type BashExecRequest struct {
    Cmd          string // shell 风格命令串；管道支持，其他 shell 元字符被 edge tokenizer 拒绝
    Timeout      int    // 0 → 沙箱默认 30s；负数当 0
    Unrestricted bool   // true → 绕过 cmdpolicy 全部沙箱（管理员"允许 Agent 写操作"门控）
}

type BashExecResponse struct {
    Allowed    bool   // 策略/路径/网络检查是否通过
    Reason     string // 拒绝原因（Allowed=false 时）
    Stdout     string // 可能被截断的 stdout
    Stderr     string // 可能被截断的 stderr
    ExitCode   int
    Truncated  bool   // 是否触及 Policy.StdoutCap/StderrCap
    DurationMs int64  // 进程管线 wall-clock，不含策略决策时间
}
```

## 4. 关键函数与流程

无函数定义（纯 wire 协议常量与结构）。

文档要点：
- **`MethodBashExec`** = `"bash.exec"`：cloud→edge 的 RPC method 名
- **`BashExecRequest`**：
  - `Cmd`：shell 风格命令串；管道支持；其他元字符（redirect / && / || / $() / 反引号）被 edge tokenizer 拒绝。完整语法见 `cmdpolicy.SplitPipes`
  - `Timeout`：覆盖策略默认每调用上限
  - `Unrestricted`：admin-gated 逃生舱，绕过 binary allowlist / denied-class / path allowlist / shell 元字符语法；仅当"允许 Agent 写操作"开关 ON 时 manager 才设 true
- **`BashExecResponse`**：镜像 `cmdpolicy.ShellResult`，仅 JSON key 改 snake_case 匹配 ongrid wire 约定

## 5. 依赖关系

- **内部包**：无（纯类型定义）
- **外部库**：无
- **被调用方**：
  - manager：`internal/manager/biz/aiops/tools/bash_basetool.go`（BaseTool 填充请求）
  - edge：`internal/edgeagent/bash/handlers.go`（注册 handler 处理请求）

## 6. 并发与资源管理

无并发控制（纯类型定义）。Wire 结构是值类型，可安全并发传递。

## 7. 设计模式与亮点

- **wire shape 极简**：单一 Cmd 字符串 + 可选 timeout，把解析/校验推迟到 edge 侧"单一真理源"
- **`Unrestricted` 显式逃生舱**：注释明示"deliberate, admin-gated escape hatch"，让管理员在了解风险下绕过沙箱；默认 false 是只读 cmdpolicy 路径
- **`Reason` 可被 LLM 解析**：注释提到拒绝原因"Stable enough to be parsed by the LLM (e.g. 'binary rm is in denied class')"，让模型调整下一次调用
- **`DurationMs` 排除策略决策**：策略决策是微秒级，DurationMs 只反映进程管线 wall-clock
- **响应镜像 cmdpolicy.ShellResult**：byte-for-byte 一致，仅 JSON key 蛇形化

## 8. 注意事项

- **`Unrestricted=true` 风险**：edge agent 以 root 或高权限运行时，绕过沙箱等于把全部权限交给 LLM；必须严格门控（"allow Agent write actions"开关）
- **管道支持的语法边界**：注释明示仅管道支持，其他元字符被拒；调用方需了解 `cmdpolicy.SplitPipes` 完整语法
- **Timeout 负数当 0**：调用方传 -1 会被当 0 处理，使用沙箱默认 30s
- **`Truncated` 是两流或**：任一流触及 cap 即 true，无法区分是 stdout 还是 stderr
- **未来变体**：注释提到"future mutating-bash variants would reuse this same wire under a different policy preset"，扩展时不改 wire 而是换 method 名或策略预设
