# `host_files_basetool.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/host_files_basetool.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件实现三个 edge-scope 文件系统检查 BaseTool：`host_find_large_files` / `host_du_summary` / `host_stat_file`。每个工具：unmarshal LLM argsJSON → 通过 `DeviceResolver` 解析 `device_id` → host `edge_id` → `Caller.Call` 派发到 frontier tunnel → 返回 edge 端 JSON（包装成带 `device_id` + `success_count` + `error_count` 的 envelope）。**Batch 协议**：每个 schema 接受 `paths: string[]`（1..16），LLM 被强 nudged 每次传 5..8 个相关路径；per-path 失败 surface 为 `Results[i].Error`，partial success 可观测无需额外 roundtrip。60s 单次 tunnel round-trip 超时（mirror edge 端 30s per-path + 60s whole-batch ceiling）。三个工具放一个文件因 wiring 完全对称（同 resolver / 同 Caller / 同 error envelope）。**BaseTool-native from day one**（改进点 #1）——不走闭包路径 `Registry.Tool`。

`host_du_summary` 额外有 **coverage sanity-check**：manager 端计算 "扫描路径解释了 root-fs used capacity 的百分之几"，<80% 且未扫 "/" 时生成 `Hint` 显式告诉 LLM "re-call with paths=['/'] at depth=1"。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 `AppendHostFilesTools`（`host_files_register.go`）注册到 ToolBag；依赖 `basetool`、`devicebiz.Usecase` / `edgebiz.Usecase`（via `DeviceResolver`）、`pkg/tunnel`（FindLargeFiles/DuSummary/StatFile Request/Response + Method 常量）、`Caller`、`path/filepath`。

## 3. 关键类型与接口

```go
const (
    hostFilesCallTimeout   = 60 * time.Second
    hostFilesMaxBatchPaths = 16
)

// 窄接口，DeviceResolver 与 test fake 都满足。
type hostFilesDeviceResolver interface {
    LookupHostEdge(ctx, deviceID uint64) (uint64, error)
}

// 适配 DeviceResolver → hostFilesDeviceResolver。
type deviceResolverAdapter struct{ inner DeviceResolver }

type FindLargeFilesTool struct {
    caller   Caller
    resolver hostFilesDeviceResolver
    log      *slog.Logger
}
type DuSummaryTool  struct { /* 同上 */ }
type StatFileTool   struct { /* 同上 */ }

// 各工具的 args / envelope：
type findLargeFilesArgs struct {
    DeviceID     uint64
    Paths        []string
    TopN         int      // 默认 20，[1,100]
    MinSizeBytes int64    // 默认 1MiB
    ExcludePaths []string // 默认 ["/proc","/sys","/dev","/run"]
}
type findLargeFilesResultEnvelope struct {
    DeviceID     uint64
    SuccessCount, ErrorCount int
    Results      []tunnel.FindLargeFilesResultEntry
}

type duSummaryArgs struct {
    DeviceID uint64
    Paths    []string
    Depth    int      // 默认 1，[1,5]
}
type duSummaryResultEnvelope struct {
    DeviceID, SuccessCount, ErrorCount int
    Results     []tunnel.DuSummaryResultEntry
    Filesystems []tunnel.HostFilesystem
    Coverage    *duCoverage  // manager 端计算的 sanity-check
}

type duCoverage struct {
    RootMount              string
    FsUsedBytes, ScannedBytes int64
    FsUsedHuman, ScannedHuman string
    ExplainedPct           float64
    Hint                   string  // <80% 且未扫 "/" 时非空
}

type statFileArgs struct {
    DeviceID uint64
    Paths    []string
}
type statFileResultEnvelope struct {
    DeviceID, SuccessCount, ErrorCount int
    Results     []tunnel.StatFileResultEntry
}
```

## 4. 关键函数与流程

```go
func dispatchEdgeCall(ctx, caller, edgeID, method, req, toolName) ([]byte, error) // 共享 tunnel 派发
func validateBatchPaths(toolName, paths []string) error                            // 1..16 校验
func computeDuCoverage(reqPaths, results, fs) *duCoverage                          // du_summary 专属
func humanBytesSimple(n int64) string                                              // mirror edge's humanBytes
func roundOnePlace(x float64) float64

// 各工具：
func (t *FindLargeFilesTool) InvokableRun(ctx, argsJSON, _ ...InvokeOption) (string, error)
func (t *DuSummaryTool)    InvokableRun(ctx, argsJSON, _ ...InvokeOption) (string, error)
func (t *StatFileTool)     InvokableRun(ctx, argsJSON, _ ...InvokeOption) (string, error)
```

**通用流程**（三工具一致）：
1. 守门 `caller != nil`。
2. Unmarshal → args；`DeviceID == 0` → 报错；`validateBatchPaths` 校验 paths 1..16 且非空。
3. 工具特定默认值（TopN/MinSizeBytes/ExcludePaths/Depth）。
4. `resolver.LookupHostEdge(ctx, DeviceID)` → `edgeID`。`edgeID == 0` → 报错 "device_id=%d has no host-edge link (try query_devices to list available device ids)"。
5. `dispatchEdgeCall(ctx, caller, edgeID, Method*, req, toolName)` → `respBody`。
6. Unmarshal `tunnel.*Response`，包装 envelope（`DeviceID` + `Results` + `SuccessCount`/`ErrorCount`）。
7. （仅 `DuSummaryTool`）`computeDuCoverage` 计算 coverage hint。
8. Marshal envelope 返回。

**`computeDuCoverage` 流程**：
1. 找 `fs` 中 `Mountpoint == "/"` 的 root fs。无则返回 nil。
2. 构建 `nonRootMounts` set（其他 mountpoint）。
3. 遍历 `results`：跳过 `Error != ""` 或 `TotalSizeBytes <= 0`；跳过 path 在 `nonRootMounts` 中的；累加 `scanned`；若 path 是 "/" 置 `scannedRoot = true`。
4. `pct = 100 * scanned / root.UsedBytes`，cap 100（防重叠路径 over-count）。
5. 构造 `duCoverage{RootMount, FsUsedBytes, FsUsedHuman, ScannedBytes, ScannedHuman, ExplainedPct}`。
6. `!scannedRoot && pct < 80` → 生成 `Hint`："INCOMPLETE: ... Re-call host_du_summary with paths=['/'] at depth=1 ... Do not finalize an answer until coverage is ≥80% or you've explicitly identified where the unaccounted space lives."。

## 5. 依赖关系

- **basetool**：`ToolInfo`（`Class="read"`）、`InvokeOption`（忽略，注释明示 "opts are accepted but not consulted for routing — device_id comes from the LLM-supplied args"）。
- **DeviceResolver**（`device_resolver.go`）：`ResolveEdgeID(ctx, deviceID)`。通过 `deviceResolverAdapter` 适配到 `hostFilesDeviceResolver` 接口。
- **Caller**：`Call(ctx, edgeID, method, body)` 派发到 frontier tunnel。
- **pkg/tunnel**：`FindLargeFilesRequest/Response` / `DuSummaryRequest/Response` / `StatFileRequest/Response` / `HostFilesystem` / `MethodFindLargeFiles` / `MethodDuSummary` / `MethodStatFile`。
- **path/filepath**：`computeDuCoverage` 用 `filepath.Clean` 规范化 path。

## 6. 并发与资源管理

- **无锁、无共享可变状态**：三个 Tool struct 仅持有不变依赖（caller / resolver / log），多 goroutine 可并发调用。
- **无 goroutine**：单次 `Caller.Call`，60s 超时覆盖。edge 端内部并发处理 4 paths，manager 端不并发。
- **`dispatchEdgeCall` 共享超时**：每次调用 `context.WithTimeout(ctx, hostFilesCallTimeout=60s)`，与 edge 端 60s whole-batch ceiling 对齐。
- **`resolver.LookupHostEdge` 无独立超时**：受 `ctx` 控制，若 caller ctx 已接近 60s 上限，resolver 可能超时。

## 7. 设计模式与亮点

- **Batch 协议（paths[]）**：三个工具都接受 `paths: string[]`（1..16），schema description 强 nudge "ALWAYS batch 5..8 related paths per call instead of calling once per path — saves LLM round-trips"。per-path 失败 surface 为 `Results[i].Error`，partial success 可观测。
- **三个工具一个文件**：wiring 完全对称（同 resolver / 同 Caller / 同 envelope 结构），放一起让对称性可见。注释明示 "they share identical wiring so putting them side-by-side makes the symmetry visible"。
- **BaseTool-native from day one（改进点 #1）**：不走闭包路径 `Registry.Tool`，直接 BaseTool。注释明示 "The closure-style legacy registry path (registry.go::Tool) is NOT used"。
- **`deviceResolverAdapter` 适配器**：让 test fake 保持 `LookupHostEdge` seam 名，生产 wiring 走 `DeviceResolver`。适配器存在是为了不强制 test fake 实现 `DeviceResolver` 接口。
- **`duCoverage` sanity-check**：manager 端计算 "扫描路径解释了 root-fs used 的百分之几"，<80% 且未扫 "/" 时生成 `Hint` 显式告诉 LLM re-call。这是给 weak model 的 guardrail——防 LLM 拿 /var/* 的扫描结果就下结论。
- **`humanBytesSimple` mirror edge's humanBytes**：不 import edge 包到 manager，本地实现二进制前缀（KiB/MiB/GiB...）。
- **`validateBatchPaths` belt-and-braces**：schema `minItems/maxItems` 已覆盖 LLM happy path，本函数兜底空数组 / 超 16 / 空字符串 path。
- **`Class="read"`**：三工具都只读（find/du/stat 不 mutate），sandbox 只允许读白名单路径。
- **`ExcludePaths` 默认虚拟文件系统**：`["/proc","/sys","/dev","/run"]`，防 find/du 扫虚拟 fs 卡死。`WhenToUse` 明示 "NEVER call against /proc /sys (the scan never finishes)"。

## 8. 注意事项

- **60s 超时偏宽**：find/du 在大树上可能饱和 60s。stat 总是 cheap。若 tenant 有超大目录树，find/du 可能超时。
- **`paths[]` maxItems 16**：与 edge 端 `hostFilesMaxBatchPaths` mirror，defense in depth。修改需同步两处。
- **`resolver.LookupHostEdge` 返回 (0, nil)**：device 存在但无 host-edge link 时返回 0 + nil error，工具报 "device_id=%d has no host-edge link"。这是 `DeviceResolver` 的 (0, nil) 双义设计。
- **`computeDuCoverage` 启发式**：用 "absolute path that isn't itself a known non-root mount" 判断 path 是否在 root fs 上，cheap 但不精确。若 path 是非 root mount 的子目录（如 `/mnt/data/foo`），会被误算入 root scanned。注释明示 "good enough for the common case"。
- **`pct > 100` cap**：多个重叠路径（如 `["/var","/var/log"]`）会 over-count，cap 到 100 防止 `ExplainedPct` 出现 120% 这种荒谬值。
- **`InvokeOption` 被忽略**：注释明示 "device_id comes from the LLM-supplied args, not from the per-call invoke context"。装饰器链仍消费 opts（audit/ratelimit）。
- **`ExcludePaths` 仅 find_large_files 有**：du_summary / stat_file 无 ExcludePaths。du_summary 的 `WhenToUse` 用文字提示 "NEVER call against /proc /sys"，但不强制。
- **edge 端 mirror layout**：`internal/edgeagent/host_files/` 镜像本文件三工具布局，修改 schema/envelope 需同步 edge 端。
