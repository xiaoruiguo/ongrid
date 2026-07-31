# `read_journal.go` 技术实现文档

> 源文件：`internal/skill/builtin/read_journal.go`
> 包路径：`github.com/ongridio/ongrid/internal/skill/builtin`

## 1. 概述

`read_journal.go` 实现 `host_read_journal` skill：包装 `journalctl` 命令读取 systemd-journald 日志，支持按 unit、回溯时长、行数过滤。属于只读 safe 类 skill，跑在 edge 上，仅 Linux 支持。是排查 edge 服务日志的主力工具。

## 2. 包信息

- **包名**：`builtin`
- **所属模块**：`internal/skill/builtin`（内置 skill 实现层）
- **依赖方向**：被 `builtin` 包 `init()` 自注册；依赖 `internal/skill` 框架类型

## 3. 关键类型与接口

```go
// skill 实现，无状态
type ReadJournal struct{}

// 输入参数
type readJournalParams struct {
    Unit  string `json:"unit"`
    Since string `json:"since"`
    Lines int    `json:"lines"`
}

// 输出结果
type readJournalResult struct {
    Lines      []string `json:"lines"`
    TotalLines int      `json:"total_lines"`
    Command    string   `json:"command"`
    Error      string   `json:"error,omitempty"`
}
```

## 4. 关键函数与流程

### `init()`
- **签名**：`func init() { skill.Register(&ReadJournal{}) }`
- **职责**：自注册到全局 Registry。

### `ReadJournal.Metadata`
- **签名**：`func (ReadJournal) Metadata() skill.Metadata`
- **职责**：返回元数据。Key=`host_read_journal`，Class=`ClassSafe`，Category=`filesystem`。
- **参数**：`unit`（可选 string，systemd unit 名）、`since`（duration，默认 `"10m"`）、`lines`（int，默认 200）。
- **Scope**：零值 = `ScopeHost`。

### `ReadJournal.Execute`
- **签名**：`func (ReadJournal) Execute(ctx context.Context, params json.RawMessage) (json.RawMessage, error)`
- **职责**：跑 `journalctl` 并按行返回 stdout。
- **流程**：
  1. 解码 params（空 params 跳过）；
  2. `lines <= 0` → 默认 200；`since == ""` → 默认 `"10m"`；
  3. `res := readJournalResult{Lines: []string{}}`；
  4. `runtime.GOOS != "linux"` → `{Error: "journal only supported on linux"}` JSON；
  5. `context.WithTimeout(ctx, 30s)`；
  6. 构造 argv：`["--no-pager", "--output=short-iso", "-n", lines]`，按需 append `--unit`、`--since`；
  7. `exec.CommandContext(cctx, "journalctl", args...)`；
  8. `res.Command = "journalctl " + strings.Join(args, " ")`（审计可见实际命令）；
  9. `cmd.Output()`：失败 → `{Error: err.Error()}` JSON；
  10. stdout 按行 split，trim 末尾换行，空 stdout → `[]string{}`；
  11. `res.Lines = all`，`res.TotalLines = len(all)`；
  12. `json.Marshal(res)` 返回。
- **错误处理**：参数解码失败返回 Go error；非 linux/journalctl 执行失败返回 `{Error:...}` JSON；30s 超时由 ctx 兜底。

## 5. 依赖关系

- **内部包**：`github.com/ongridio/ongrid/internal/skill`
- **外部库**：`context`、`encoding/json`、`fmt`、`os/exec`、`runtime`、`strconv`、`strings`、`time`
- **被调用方**：通过 `skill.Registry` 被 `internal/edgeagent` 调度派发（ScopeHost）

## 6. 并发与资源管理

- **`context.WithTimeout(ctx, 30s)`** 硬上限保护 dispatcher，防 journalctl 卡死。
- `exec.CommandContext` 让 ctx 取消能传播到子进程（kill）。
- `ReadJournal` 无状态，并发调用安全；每次 `Execute` 独立构造 `exec.Cmd`。

## 7. 设计模式与亮点

- **审计可见的 Command 字段**：结果中带 `Command` 字段记录实际执行的 `journalctl ...` 字符串，运维与 LLM 都能审计到底跑了什么。
- **非 linux 优雅降级**：返回结构化 error JSON 而非 Go error，让 LLM 能向用户解释"该 skill 仅 Linux 支持"。
- **`Lines` 初始化为空 slice**：避免 JSON 序列化出 `null`，调用方解析时无需 nil 检查。
- **参数白名单 argv 拼接**：`unit`/`since` 作为独立 argv 元素传入 `exec.Command`，不经 shell，无注入风险（os/exec 直接 exec）。
- **`--no-pager` + `--output=short-iso`**：禁用 pager 避免阻塞，short-iso 输出带 ISO 时间戳便于排序。

## 8. 注意事项

- **仅 Linux 支持**：`runtime.GOOS != "linux"` 时返回 `{Error:...}` JSON；edge 通常为 Linux，manager 上调用会得到该错误。
- **`unit` 参数无白名单校验**：直接拼入 argv，虽无 shell 注入风险（os/exec），但恶意 LLM 可传特殊字符触发 journalctl 报错；建议未来加 unit 名正则校验。
- **`since` 参数透传 journalctl 语法**：用户可传 `"10m"`/`"1h"`/`"2024-01-01"` 等多种格式，由 journalctl 解析；非法格式会导致 journalctl 报错。
- **30s 超时硬编码**：journal 量大时可能不够；当前未暴露超时参数，上层需自行控制 ctx。
- **`lines` 上限未校验**：用户可传巨大值，journalctl `-n` 支持但会消耗内存；建议上层做范围校验。
- **`cmd.Output()` 仅捕获 stdout**：stderr 被丢弃，错误信息依赖 `err.Error()`；若需 stderr 详情需改用 `CombinedOutput` 或单独捕获。
- **`Lines` 字段无截断**：返回全部匹配行，行数大时 LLM token 消耗高；建议未来加 `max_chars` 截断参数。
- **结果 `Command` 字段含敏感信息风险**：若 `unit` 含敏感名，会进入审计日志；当前设计认为 unit 名是运维信息，可接受。
