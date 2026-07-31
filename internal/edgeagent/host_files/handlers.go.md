# `handlers.go` 技术实现文档

> 源文件：`internal/edgeagent/host_files/handlers.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/host_files`

## 1. 概述

本文件实现三个 edge-side handler：`find_large_files`（shell out `find`，Linux 用 `-printf`、Darwin 用 `-exec stat`）、`du_summary`（shell out `du` + 附带 `df` 文件系统覆盖快照）、`stat_file`（纯 Go `os.Lstat` + `syscall.Stat_t` 取 owner/group）。三个 handler 都支持批量协议（1-16 路径并行），通过 `SandboxConfig` 校验路径，per-path 失败不中断整批。

## 2. 包信息

- **包名**：`host_files`
- **所属模块**：edgeagent 文件系统能力层
- **依赖方向**：被 `cmd/ongrid-edge` 调用 `Register`；调用 `tunnel`

## 3. 关键类型与接口

无显著导出类型。仅常量：

```go
const (
    hostFilesPerPathTimeout     = 30 * time.Second  // 单路径上限
    hostFilesBatchTimeout       = 60 * time.Second  // 整批上限
    hostFilesBatchConcurrency   = 4                 // 并行上限
    hostFilesMaxBatchPaths      = 16                // 路径数硬上限
)
```

## 4. 关键函数与流程

### `Register`
- **签名**：`func Register(client tunnel.Client, log *slog.Logger) error`
- **职责**：装配 SandboxConfig 并注册三个 handler
- **流程**：`DefaultSandboxConfig()` → `Validate()` → 注册 `MethodFindLargeFiles` / `MethodDuSummary` / `MethodStatFile`
- **错误处理**：sandbox 不健康（无 find/du）返回错误让 main 决定是否致命

### `runBatch`（泛型）
- **签名**：`func runBatch[T any](ctx context.Context, paths []string, out []T, work func(ctx context.Context, idx int, path string)) error`
- **职责**：共享并发原语，并行执行 work，per-path 错误进 `out[i].Error`
- **流程**：`errgroup.WithContext` + `SetLimit(hostFilesBatchConcurrency)`；`work` 永不返回 error（进 out）；`g.Wait()` 透传
- **错误处理**：整批 ctx 超时取消所有未完成 work

### find_large_files handler
- **签名**：`func makeFindLargeFilesHandler(sb *SandboxConfig, log *slog.Logger) tunnel.Handler`
- **流程**：
  1. 解析 `FindLargeFilesRequest`；校验 paths 1-16；TopN 默认 20；MinSizeBytes 默认 1MiB
  2. `sb.ResolveBinary("find")` 失败整批报错
  3. `runBatch` 并行每路径 `runFindOnePath`
- **错误处理**：sandbox reject / find error 进 `Results[i].Error`

### `runFindOnePath`
- **签名**：`func runFindOnePath(ctx context.Context, sb *SandboxConfig, findBin, path string, req tunnel.FindLargeFilesRequest) tunnel.FindLargeFilesResultEntry`
- **流程**：`ValidatePath` → `context.WithTimeout(30s)` → `runFindLargeFiles` → 按 size 降序排序 → 截断 TopN → `humanBytes` 格式化

### `runFindLargeFiles`
- **签名**：`func runFindLargeFiles(ctx context.Context, findBin, path string, minSizeBytes int64, excludePaths []string) ([]tunnel.HostFileInfo, error)`
- **流程**：组装 args（path + `-path <exclude>* -prune -o` 每个 exclude + `-type f -size +Nc`）；按 GOOS 分流 Linux（`-printf %s|%T@|%u|%p\n`）/ Darwin（`-exec stat -f %z|%m|%Su|%N`）
- **错误处理**：unsupported GOOS 报错；find ExitError 带 stderr

### `runFindLinux` / `runFindDarwin`
- 调 `exec.CommandContext` + `cmd.Output()`；非零 exit 但有输出视为 partial-success；`parseFindOutput` 用对应 parser

### `parseFindOutput` / `parseLinuxFindLine` / `parseDarwinFindLine`
- **职责**：解析 `size|mtime|owner|path` 行
- **流程**：`SplitN(line, "|", 4)`；`parseEpochFloat` 处理 epoch + 可选小数（GNU `%T@` vs BSD `%m`）

### du_summary handler
- **签名**：`func makeDuSummaryHandler(sb *SandboxConfig, log *slog.Logger) tunnel.Handler`
- **流程**：解析 + 校验 + `ResolveBinary("du")` + `runBatch` 每路径 `runDuOnePath`；最后 `collectFilesystems` 跑 `df` 取覆盖快照
- **错误处理**：du 失败进 `Results[i].Error`；df 失败静默跳过

### `runDu`
- **签名**：`func runDu(ctx context.Context, duBin, path string, depth int) ([]tunnel.HostDuEntry, int64, error)`
- **流程**：Linux 用 `--max-depth=N -B1`（bytes）；Darwin 用 `-d N -k`（KiB → bytes）；解析 `<size>\t<subpath>`；path 本身行剥为 total
- **错误处理**：ExitError 带 stderr；scanner.Err 透传

### `collectFilesystems` / `runDfOne`
- **职责**：best-effort df 快照（`/` + 请求路径所在的其他 mountpoint）
- **流程**：Linux `df -B1 --output=target,used,size`；Darwin `df -kP`（POSIX 列）；解析第二行

### stat_file handler
- **签名**：`func makeStatFileHandler(sb *SandboxConfig, log *slog.Logger) tunnel.Handler`
- **流程**：解析 + 校验 + `runBatch` 每路径 `runStatOnePath`

### `runStatOnePath`
- **签名**：`func runStatOnePath(_ context.Context, sb *SandboxConfig, path string) tunnel.StatFileResultEntry`
- **流程**：`ValidatePath` → `os.Lstat` → 类型判定（symlink/dir/file）→ `syscall.Stat_t` 取 Uid/Gid → `user.LookupId` / `LookupGroupId` → `fileTimes(li)` 取 mtime/atime → 填 entry
- **错误处理**：Lstat 失败进 `Entry.Error`

### `humanBytes`
- **签名**：`func humanBytes(n int64) string`
- **职责**：返回 `12.3 MiB` 格式二进制大小字符串

## 5. 依赖关系

- **内部包**：`internal/pkg/tunnel`
- **外部库**：`golang.org/x/sync/errgroup`、标准库 `bufio`、`bytes`、`context`、`encoding/json`、`errors`、`fmt`、`log/slog`、`os`、`os/exec`、`os/user`、`path/filepath`、`runtime`、`sort`、`strconv`、`strings`、`syscall`、`time`
- **被调用方**：`cmd/ongrid-edge` 主程序调 `Register`

## 6. 并发与资源管理

- **`runBatch` + errgroup + SetLimit**：per-path 并发上限 4；超出排队
- **per-path `context.WithTimeout(30s)`**：单路径不超 30s；整批 60s 上限
- **per-path 错误隔离**：work 函数永不返回 error，所有错误进 `out[i].Error`；整批不中断
- **`runBatch` 是泛型**：`T` 是 `FindLargeFilesResultEntry` / `DuSummaryResultEntry` / `StatFileResultEntry`——避免为每个 handler 写并发原语

## 7. 设计模式与亮点

- **泛型 batch 原语**：`runBatch[T any]` 让三个 handler 共享并发逻辑；调用方提供 work 闭包，错误进 out 而非返回——简化错误处理
- **per-path 错误隔离**：sandbox reject / find non-zero / missing file 都进 `Results[i].Error`，其他路径继续；manager BaseTool 把 partial batch 直接 surface 给 LLM
- **OS 分流**：Linux GNU find `-printf` vs Darwin BSD `-exec stat`；du 的 `--max-depth` vs `-d`；df 的 `--output` vs `-kP`——通过 `runtime.GOOS` switch
- **path 本身行剥为 total**：du 输出中 `path` 自身行（size 等于总）单独提为 `TotalSizeBytes`，其余为子路径 entries
- **`-prune -o` exclude 模式**：`-path <prefix>* -prune -o` 必须在 `-type f` 前，否则 find 仍会 descend；通配符让 `/proc/123/...` 也被 prune
- **df 覆盖快照**：du_summary 附带 `/` + 请求路径所在 mountpoint 的 df 信息，让 manager 计算「scanned du totals only explain X% of fs_used」覆盖提示
- **stat 纯 Go**：`os.Lstat` + `syscall.Stat_t` 避免 subprocess 开销；`fileTimes` 用 build tag 分离 Linux/Darwin 的 atime 字段名差异
- **partial-success 容忍**：find / du ExitError 但有输出时仍解析输出（permission denied 等不影响其他子树）
- **`humanBytes` 本地实现**：避免依赖 `internal/pkg/humanize`，让包零外部依赖

## 8. 注意事项

- `hostFilesBatchTimeout=60s` 与 manager BaseTool 的 tunnel timeout 一致——edge 侧 work 不能超过此预算
- `hostFilesBatchConcurrency=4` 是为小型 / 共享 edge box 调优——find/du 是 CPU + 磁盘密集型，更高并发通常拖慢墙钟时间
- `hostFilesMaxBatchPaths=16` 与 manager schema `maxItems=16` 一致——edge 侧重复校验是 defense in depth
- `runFindLargeFiles` 中 `excludePaths` 的 `-path <prefix>*` 通配符——`/proc` 会 prune `/proc` 和 `/proc/123/...`，但不会 prune `/procxyz`
- `parseLinuxFindLine` 用 `SplitN(line, "|", 4)`——path 可能含 `|`，但前 3 个字段不会；安全
- `parseEpochFloat` 返回 UTC time——避免 `time.Local` 让测试输出不稳定
- `runDu` 的 path 本身行通过 `filepath.Clean(sub) == cleanReq` 匹配——路径规范化后比较，容忍 trailing slash
- `runDfOne` 的 Linux `--output=target,used,size` 只取 3 列——其他列（avail / pcent / mounted-on）被丢弃
- `runStatOnePath` 用 `os.Lstat` 而非 `os.Stat`——symlink 不会被 follow，类型能正确识别为 "symlink"
- `syscall.Stat_t` 的 Uid/Gid 是 uint32——用 `strconv.FormatUint(uint64(...), 10)` 转字符串查 user.LookupId
- `user.LookupId` 失败时 fallback 用数字 UID 字符串——保证总有 owner 字段
