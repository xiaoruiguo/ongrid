# `tail_file.go` 技术实现文档

> 源文件：`internal/skill/builtin/tail_file.go`
> 包路径：`github.com/ongridio/ongrid/internal/skill/builtin`

## 1. 概述

`tail_file.go` 实现 `host_tail_file` skill：读取文件最后 N 行（类似 `tail -n`）。属于只读 safe 类 skill，跑在 edge 上。强制绝对路径 + 禁止 `..` 路径穿越，防止 AI agent 通过 `../../etc/shadow` 等参数读取敏感文件。

## 2. 包信息

- **包名**：`builtin`
- **所属模块**：`internal/skill/builtin`（内置 skill 实现层）
- **依赖方向**：被 `builtin` 包 `init()` 自注册；依赖 `internal/skill` 框架类型

## 3. 关键类型与接口

```go
// skill 实现，无状态
type TailFile struct{}

// 输入参数
type tailFileParams struct {
    Path     string `json:"path"`
    Lines    int    `json:"lines"`
    MaxBytes int64  `json:"max_bytes"`
}

// 输出结果
type tailFileResult struct {
    Lines              []string `json:"lines"`
    TotalLinesReturned int      `json:"total_lines_returned"`
    FileSize           int64    `json:"file_size"`
    Truncated          bool     `json:"truncated"`
    Error              string   `json:"error,omitempty"`
}
```

## 4. 关键函数与流程

### `init()`
- **签名**：`func init() { skill.Register(&TailFile{}) }`
- **职责**：自注册到全局 Registry。

### `TailFile.Metadata`
- **签名**：`func (TailFile) Metadata() skill.Metadata`
- **职责**：返回元数据。Key=`host_tail_file`，Class=`ClassSafe`，Category=`filesystem`。
- **参数**：`path`（必填 string，绝对路径，不含 `..`）、`lines`（int，默认 100）、`max_bytes`（int，默认 1048576 = 1MiB）。
- **Scope**：零值 = `ScopeHost`。

### `TailFile.Execute`
- **签名**：`func (TailFile) Execute(ctx context.Context, params json.RawMessage) (json.RawMessage, error)`
- **职责**：读取文件尾部 N 行，超 `max_bytes` 时截断。
- **流程**：
  1. 解码 params（空 params 跳过）；
  2. **安全校验**：`path` 非空、`filepath.IsAbs`、不含 `..`；任一失败返回 Go error；
  3. `lines <= 0` → 默认 100；`max_bytes <= 0` → 默认 1MiB；
  4. `res := tailFileResult{Lines: []string{}}`；
  5. `os.Open(path)`：失败 → `{Error:...}` JSON；
  6. `defer f.Close()`；
  7. `f.Stat()`：失败 → `{Error:...}` JSON；`res.FileSize = st.Size()`；
  8. 若 `st.Size() > max_bytes`：`res.Truncated = true`，`f.Seek(st.Size()-max_bytes, SeekStart)`，失败 → `{Error:...}` JSON；
  9. `io.ReadAll(f)` 读全部剩余字节；
  10. 按 `\n` split，trim 末尾换行；
  11. **截断修正**：若 `Truncated` 且 `len(all) > 0`，丢弃首行（seek 中点可能切断一行）；
  12. 若 `len(all) > lines`，取末尾 `lines` 行；
  13. 空 slice 修正：`len(all)==1 && all[0]==""` → `[]string{}`；
  14. `res.Lines = all`，`res.TotalLinesReturned = len(all)`；
  15. `json.Marshal(res)` 返回。
- **错误处理**：路径校验失败返回 Go error；文件操作失败返回 `{Error:...}` JSON；ctx 取消由 `io.ReadAll` 自然传播。

## 5. 依赖关系

- **内部包**：`github.com/ongridio/ongrid/internal/skill`
- **外部库**：`context`、`encoding/json`、`fmt`、`io`、`os`、`path/filepath`、`strings`
- **被调用方**：通过 `skill.Registry` 被 `internal/edgeagent` 调度派发（ScopeHost）

## 6. 并发与资源管理

- **`defer f.Close()`** 确保文件描述符释放。
- **`io.ReadAll` 限制为 `max_bytes`**：通过 seek 已限定读取范围，最坏情况读 1MiB，内存可控。
- `TailFile` 无状态，并发调用安全；每次 `Execute` 独立打开文件。
- 无显式超时：依赖父 ctx 取消（`io.ReadAll` 响应 ctx 取消）。

## 7. 设计模式与亮点

- **绝对路径 + 禁 `..` 双重防护**：`filepath.IsAbs` + `strings.Contains(path, "..")` 双校验，防止 AI agent 通过相对路径或路径穿越读取敏感文件。
- **截断中点行修正**：seek 到 `FileSize - max_bytes` 时可能恰好切断一行，丢弃首行确保每行完整；这是细节亮点。
- **`Truncated` 字段透明告知**：结果带 `Truncated` 标志，调用方知道是否被截断。
- **空 slice 初始化与修正**：`Lines` 初始化为 `[]string{}` 避免序列化出 `null`；空文件修正为 `[]string{}` 而非 `[""]`。
- **错误进结果而非 Go error**：文件打开/stat/读取失败返回 `{Error:...}` JSON，保持审计一致。
- **`FileSize` 字段**：返回文件总大小，调用方可判断是否需要翻页读取。

## 8. 注意事项

- **仅限绝对路径 + 禁 `..`**：这是安全策略，AI agent 不能读取相对路径文件；调用方需保证 path 符合要求。
- **无符号链接校验**：`filepath.IsAbs` 与 `..` 检查不能防止符号链接逃逸（如 `/var/log/symlink_to_etc_shadow`）；当前实现信任文件系统，未来可考虑加 `filepath.EvalSymlinks` 校验。
- **`max_bytes` 默认 1MiB**：大日志文件只能看尾部 1MiB，约 1 万行；需更多行需调大 `max_bytes`。
- **`lines` 上限未校验**：用户可传巨大值，受 `max_bytes` 间接限制（最多读 `max_bytes` 字节）。
- **无超时**：`io.ReadAll` 阻塞读，若文件在慢速网络挂载（NFS）上可能卡住；当前依赖父 ctx，建议上层设 deadline。
- **`strings.Contains(path, "..")` 过于宽松**：合法路径含 `..` 子串会被误拒（如 `/var/log/myapp..backup/log`）；当前实现优先安全，宁可误拒。
- **不支持二进制文件**：`strings.Split` 按字节处理，二进制文件结果可能含不可见字符；本 skill 定位是日志读取，二进制场景未考虑。
- **`Truncated` 时首行丢弃**：若文件恰好 1 行且被截断，会丢弃该行返回空；极端边界，实际场景罕见。
