# `bash_basetool.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/bash_basetool.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件实现 `host_bash` BaseTool：N+15 batch refactor 后的 fleet 语义——`device_ids[]` + 单条 `cmd`，一条命令跑在多台设备。读命令直接走 edge 只读 sandbox；写命令在 admin write gate 开启时排队 inline approval card，批准后才执行。关键红线：cmd 不是数组（fleet 语义是一条 cmd × N devices，多 cmd 让 LLM bundle 不相关命令）；Class="read"（v1 cmdpolicy 在 edge 拒写）；写命令识别 `isHostBashWriteCommand` 走 approval flow。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 Registry 注册和 agent loop 调用；依赖 `basetool`、`devicebiz`、`edgebiz`、`cloudwego/eino/compose`、`tunnel`

## 3. 关键类型与接口

```go
const (
    ToolNameBash       = "host_bash"
    bashCallTimeout    = 60 * time.Second  // manager → edge 单次 round-trip
    bashBatchTimeout   = 120 * time.Second // 整个 batch 上限
)

type bashBatchArgs struct {
    DeviceIDs      []uint64
    DeviceID       uint64  // 兼容旧参数
    Cmd            string
    TimeoutSeconds int
}

type HostBashProposer interface {
    ProposeAndAwait(ctx, deviceIDs []uint64, command string, timeoutSeconds int,
        sessionID, toolCallID string, userID uint64) (string, error)
}

type BashResultEntry struct {
    DeviceID                  uint64
    Allowed, Truncated        bool
    Reason, Stdout, Stderr    string
    ExitCode                  int
    DurationMs                int64
    Error                     string
}

type BashBatchResponse struct {
    Cmd                       string
    SuccessCount, ErrorCount  int
    Results                   []BashResultEntry
}

type BashTool struct {
    caller   Caller
    resolver hostFilesDeviceResolver
    proposer HostBashProposer
    log      *slog.Logger
}
```

## 4. 关键函数与流程

### `NewBashTool / NewBashToolWithProposer`
- 构造 BashTool；log nil → slog.Default()；resolver = `deviceResolverAdapter{inner: NewDeviceResolver(d, e)}`；proposer 可 nil

### `Info`
- 返回 `ToolInfo{Name: ToolNameBash, Description, WhenToUse: bashWhenToUse, Parameters: BashSchema, Class: "read"}`

### `singleBash`
- **签名**：`func (t *BashTool) singleBash(ctx, deviceID uint64, cmd string, timeout int, unrestricted bool) BashResultEntry`
- **职责**：在单台设备跑 cmd
- **流程**：
  1. deviceID 0 → entry.Error
  2. `resolver.LookupHostEdge(ctx, deviceID)` → edgeID；err/0 → entry.Error
  3. 构 `tunnel.BashExecRequest{Cmd, Timeout, Unrestricted}`；marshal
  4. `context.WithTimeout(ctx, bashCallTimeout)` callCtx
  5. `caller.Call(callCtx, edgeID, MethodBashExec, body)` → respBody；err → entry.Error "dispatch"
  6. unmarshal `tunnel.BashExecResponse`；err → entry.Error "decode resp"
  7. 填 entry.Allowed/Reason/Stdout/Stderr/ExitCode/Truncated/DurationMs
- **错误处理**：Allowed=false（sandbox 拒绝）不算 batch error，fold 进 entry.Allowed/Reason；resolver/dispatch 错误才算 entry.Error

### `RunApproved`
- **签名**：`func (t *BashTool) RunApproved(ctx, deviceIDs []uint64, cmd string, timeout int) (string, error)`
- **职责**：approval 后执行已批准 payload（不递归 propose）
- **流程**：caller nil → error；validateBatchIDs；cmd 空 → error；timeout 钳位 [0,300]；`context.WithTimeout(ctx, bashBatchTimeout)`；runBatch singleBash(unrestricted=true)；marshalBashEnvelope

### `InvokableRun`
- **签名**：`func (t *BashTool) InvokableRun(ctx, argsJSON string, opts ...basetool.InvokeOption) (string, error)`
- **职责**：parse → validate → 写命令识别 → fan-out → marshal
- **流程**：
  1. caller nil → error
  2. unmarshal args；DeviceIDs 空 + DeviceID 非 0 → `[]uint64{DeviceID}` 兼容
  3. validateBatchIDs；cmd 空 → error；timeout 钳位 [0,300]
  4. **写命令检查** `isHostBashWriteCommand(cmd)`：
     - `!HostWriteAllowedFromContext(ctx)` → 返回 `{"status":"blocked","message":"Agent write actions are disabled..."}` JSON（不报错，让 LLM 知道被门控）
     - proposer nil → error "approval inbox not wired"
     - `proposer.ProposeAndAwait(ctx, DeviceIDs, Cmd, Timeout, SessionIDFromContext(ctx), compose.GetToolCallID(ctx), cfg.UserID)` 返回 approval 结果
  5. 读命令：`context.WithTimeout(ctx, bashBatchTimeout)`；runBatch singleBash(unrestricted=false)；marshalBashEnvelope

### `marshalBashEnvelope`
- 构 BashBatchResponse；遍历 results 统计 success/error count；marshal

### `isHostBashWriteCommand`
- **签名**：`func isHostBashWriteCommand(cmd string) bool`
- **职责**：识别写命令触发 approval
- **流程**：Fields 取首 token；sudo 跳过取次；strip path 取 basename；命中 writeBins（rm/mv/cp/chmod/chown/dd/truncate/tee/mkdir/rmdir/touch/ln）→ true；systemctl + start/stop/restart/reload/enable/disable/mask/unmask/kill/reset-failed/daemon-reload/edit → true

### `AppendBashTool`
- **签名**：`func AppendBashTool(out []basetool.BaseTool, c Caller, e *edgebiz.Usecase, d *devicebiz.Usecase, log *slog.Logger) []basetool.BaseTool`
- **职责**：deps 任一 nil → 返回 out 不变（graceful degradation）；否则 append NewBashTool

## 5. 依赖关系

- **内部包**：`basetool`（HostWriteAllowedFromContext / SessionIDFromContext）、`devicebiz`、`edgebiz`、`tunnel`
- **外部库**：`github.com/cloudwego/eino/compose`（GetToolCallID）、标准库 `context`、`encoding/json`、`fmt`、`log/slog`、`slices`、`strings`、`time`
- **被调用方**：Registry 注册；approval executor 调 RunApproved

## 6. 并发与资源管理

- `runBatch`（batch_helper.go）fan-out，4 并发上限
- 每次 singleBash 独立 `context.WithTimeout(ctx, bashCallTimeout)` 限定单次 round-trip
- BashTool immutable，多 goroutine 共享安全
- approval flow 同步阻塞，由 proposer.ProposeAndAwait 实现

## 7. 设计模式与亮点

- **fleet 语义一条 cmd × N devices**：cmd 不是数组，避免 LLM bundle 不相关命令；多 cmd 分多次调用
- **cmd envelope-level echo**：cmd 在 BashBatchResponse.Cmd 重复一次，每 entry 不带 cmd——节省 60-200 字节 × N
- **Class="read" 但识别写命令**：v1 cmdpolicy 在 edge 拒写；manager 端额外识别写命令走 approval（双层防御）
- **blocked 状态 JSON 返回**：write gate 关闭时不报错而返 `{"status":"blocked"}`，让 LLM 知道被门控而非技术故障
- **RunApproved 分离**：approval executor 调用，不递归 propose；与 InvokableRun 路径隔离
- **Allowed=false 不算 batch error**：sandbox 拒绝是 per-device 正常响应；resolver/dispatch 错误才算 error_count
- **兼容旧 device_id**：DeviceIDs 空 + DeviceID 非 0 自动转单元素数组
- **isHostBashWriteCommand 启发式**：sudo 跳过、path strip、systemctl 子命令识别

## 8. 注意事项

- **bashCallTimeout 60s / bashBatchTimeout 120s**：per-id 与 batch 上限；batch 更宽让 4 并发能完成
- **timeout_seconds 钳位 [0,300]**：max 300s；0 用 edge 默认 30s
- **写命令识别局限**：仅看首 token + systemctl 子命令；复杂 pipe/heredoc 写操作可能漏识别（依赖 edge cmdpolicy 兜底）
- **proposer nil 时写命令报错**：approval inbox 必须 wire 才能跑写命令
- **Class="read" 不进 review_gate**：v1 cmdpolicy 拒写保证安全；future writable bash 走独立 tool name 设 Class="write"
- **edge 端独立验证**：N 个设备 N 次独立 edge-side cmdpolicy 校验，非 manager 一次
- **compose.GetToolCallID**：从 ctx 取 tool call id 作为 approval 关联键
