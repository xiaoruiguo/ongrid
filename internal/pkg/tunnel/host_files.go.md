# `host_files.go` 技术实现文档

> 源文件：`internal/pkg/tunnel/host_files.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/tunnel`

## 1. 概述

本文件定义三个 host 文件系统检查工具的 wire 协议常量与请求/响应结构：`find_large_files`（top-N 大文件）、`du_summary`（子目录大小汇总）、`stat_file`（文件元数据）。采用批量协议（2026-05-07 引入）：每个请求带 1..16 个 path，响应按相同顺序返回 per-path 结果；per-path 失败不终止整批，让 LLM 决定重试哪个。

## 2. 包信息

- **包名**：`tunnel`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被 manager 侧 `internal/manager/biz/aiops/tools/host_files_basetool.go` 调用；被 edge 侧 handler 处理；本文件仅依赖标准库 `time`

## 3. 关键类型与接口

```go
const (
    MethodFindLargeFiles = "host_files.find_large_files"
    MethodDuSummary      = "host_files.du_summary"
    MethodStatFile       = "host_files.stat_file"
)

type HostFileInfo struct {
    Path      string
    SizeBytes int64
    SizeHuman string
    Mtime     time.Time
    Owner     string
}

type HostFilesystem struct {
    Mountpoint string
    UsedBytes  int64
    SizeBytes  int64
    UsedHuman  string
    SizeHuman  string
}

type HostDuEntry struct {
    Subpath   string
    SizeBytes int64
    SizeHuman string
}
```

三个 Request/Response 对：
- `FindLargeFilesRequest` / `FindLargeFilesResponse`（含 `[]FindLargeFilesResultEntry`）
- `DuSummaryRequest` / `DuSummaryResponse`（含 `[]DuSummaryResultEntry` + `[]HostFilesystem`）
- `StatFileRequest` / `StatFileResponse`（含 `[]StatFileResultEntry`）

## 4. 关键函数与流程

无函数定义（纯 wire 协议常量与结构）。

文档要点（三个 method）：
- **`MethodFindLargeFiles`**：top-N 大文件
  - Request：`Paths []string`（1..16）、`TopN`（1..100，默认 20）、`MinSizeBytes`（默认 1 MiB）、`ExcludePaths`（默认虚拟 fs /proc /sys /dev /run）
  - ResultEntry：`Path`（回显）、`ScannedPath`、`Files []HostFileInfo`、`Error`
- **`MethodDuSummary`**：子目录大小汇总
  - Request：`Paths []string`（1..16）、`Depth`（1..5，默认 1）
  - ResultEntry：`Path`、`Subpaths []HostDuEntry`、`TotalSizeBytes`、`TotalSizeHuman`、`Error`
  - Response 额外：`Filesystems []HostFilesystem` — per-mountpoint 容量，用于 LLM "挖太浅" 覆盖警告
- **`MethodStatFile`**：文件元数据
  - Request：`Paths []string`（1..16）
  - ResultEntry：`Path`、`Type`（file/dir/symlink）、`SizeBytes`、`SizeHuman`、`Mtime`、`Atime`、`Mode`（octal）、`Owner`、`Group`、`Error`

**批量协议约定**：
- 每请求 1..N path（N=16）
- 响应 `Results` 长度与顺序匹配 `Paths`
- per-path 失败（沙箱拒绝 / find 非 0 / 缺文件）表面为 `Error`，不终止其他 path
- edge 并发执行 entries（bounded）

## 5. 依赖关系

- **内部包**：无
- **外部库**：无（仅 `time`）
- **被调用方**：
  - manager：`internal/manager/biz/aiops/tools/host_files_basetool.go`（BaseTool 填充请求）
  - edge：host_files handler（注册处理三个 method）

## 6. 并发与资源管理

无并发控制（纯类型定义）。Wire 结构是值类型，可安全并发传递。批量执行由 edge handler 负责（注释提到"edge runs entries concurrently (bounded)"）。

## 7. 设计模式与亮点

- **批量协议**：1..16 path 一次请求，减少 LLM 多次 round-trip；per-path 失败不终止整批
- **`Path` 回显**：注释明示"defense in depth" — caller 既可按 slice index 也可按字符串关联请求与结果
- **`ScannedPath` 与 `Path` 分离**：成功时 `ScannedPath` 是 edge 实际遍历的路径（可能与 `Path` 同）；失败时省略
- **`Filesystems` 覆盖警告**：DuSummaryResponse 额外返回 per-mountpoint 容量，让 manager 计算"扫描 du 总和仅占 fs_used 一小部分"的覆盖警告，捕捉"LLM 挖太浅"典型场景
- **`StatFile` 纯 Go 无 subprocess**：注释明示"edge stat'ing is pure Go (no subprocess) but the batch interface is symmetric with the other two so the LLM only learns one shape"
- **`HostFileInfo` 共享**：FindLargeFiles 与 StatFile 共用同一行结构（字段足够重叠，单一 struct 比两个近重复更清爽）
- **`TotalSizeBytes` 独立来源**：注释明示"peeled off from du output, not the sum of Subpaths" — du --max-depth 单独列 path-itself 行，该数字是权威 total

## 8. 注意事项

- **`Paths` 上限 16**：超过需分批；BaseTool 负责钳制
- **`TopN` 上限 100**：避免返回超大列表撑爆 LLM context
- **`Depth` 上限 5**：防止 du 在深目录树爆炸
- **`Filesystems` 是 best-effort**：df 失败时切片空但 per-path du 仍返回
- **`Mode` 是 octal 字符串**：如 "0644"；LLM 需理解 octal 表示
- **`Owner` / `Group` 是文本**：uid/gid 已解析为文本；若 edge 无法解析可能为空
- **manager 侧不 re-clamp**：注释明示"the edge does NOT re-clamp (single source of truth on the manager side keeps the behaviour testable in one place)" — 边界校验单点维护
- **`StatFileResultEntry` 字段 omitempty**：失败时仅 `Path` 与 `Error` 非空，其余零值不序列化
