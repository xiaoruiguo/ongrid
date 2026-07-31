# `timeout.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/decorators/timeout.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools/decorators`

## 1. 概述

本文件实现 timeout 装饰器：用 `context.WithTimeout(t.timeout)` 包 inner.InvokableRun，deadline 触发时返回 wrap `ErrToolTimeout` 的错误让 agent loop 在 chat_tool_calls 里分类为 `status=timeout`。默认 15s。belt-and-braces：成功返回后再检查一次 deadline，防 inner 忽略 ctx 临界越过。

## 2. 包信息

- **包名**：`decorators`
- **所属模块**：`internal/manager/biz/aiops/tools/decorators`
- **依赖方向**：被 `chain.go` 的 `Wrap` 调用；依赖 `basetool`、标准库

## 3. 关键类型与接口

```go
const DefaultTimeout = 15 * time.Second

var ErrToolTimeout = errors.New("tool timed out")

type TimeoutTool struct {
    inner   basetool.BaseTool
    timeout time.Duration
}
```

## 4. 关键函数与流程

### `WithTimeout`
- **签名**：`func WithTimeout(inner basetool.BaseTool, d time.Duration) basetool.BaseTool`
- **职责**：包装 inner，run 受 d 时间预算约束
- **流程**：d <= 0 → DefaultTimeout；返回 `&TimeoutTool{inner, d}`

### `TimeoutTool.Info`
- **签名**：`func (t *TimeoutTool) Info(ctx) (*basetool.ToolInfo, error)`
- **职责**：透传 inner.Info（schema 不受 timeout 影响）

### `TimeoutTool.InvokableRun`
- **签名**：`func (t *TimeoutTool) InvokableRun(ctx, argsJSON string, opts ...basetool.InvokeOption) (string, error)`
- **职责**：在 callCtx 上设 deadline 跑 inner
- **流程**：
  1. `callCtx, cancel := context.WithTimeout(ctx, t.timeout)`；defer cancel
  2. `inner.InvokableRun(callCtx, argsJSON, opts...)`
  3. err 非 nil：
     - 若 `errors.Is(callCtx.Err(), DeadlineExceeded) && ctx.Err() == nil`（callCtx 死了但 parent 活着 → 我们设的 timeout 是因）→ 返回 `fmt.Errorf("%w after %s: %v", ErrToolTimeout, t.timeout, err)`
     - 否则原样返回 err
  4. err nil：再检查 `errors.Is(callCtx.Err(), DeadlineExceeded) && ctx.Err() == nil` → 返回 `fmt.Errorf("%w after %s", ErrToolTimeout, t.timeout)`（belt-and-braces 防 inner 忽略 ctx 临界越过）
  5. 返回 out, nil
- **错误处理**：区分 decorator-imposed timeout vs tool-level error；parent ctx 已死则原样返回（不算我们的 timeout）

## 5. 依赖关系

- **内部包**：`basetool`
- **外部库**：标准库 `context`、`errors`、`fmt`、`time`
- **被调用方**：`chain.go` 的 `Wrap`

## 6. 并发与资源管理

- `TimeoutTool` immutable，多 goroutine 共享安全
- 每次 InvokableRun 创建独立 callCtx + cancel；defer cancel 防泄漏

## 7. 设计模式与亮点

- **区分 decorator timeout vs tool error**：`errors.Is(callCtx.Err(), DeadlineExceeded) && ctx.Err() == nil` 判定"我们的 timeout 触发"，agent loop 可 `errors.Is(err, ErrToolTimeout)` 分类为 status=timeout
- **belt-and-braces**：成功返回后再查 deadline，防 inner 忽略 ctx 临界越过（well-behaved tool 应尊重 ctx）
- **parent ctx 死不归类**：parent ctx 已 cancelled 时原样返回 err，不算我们的 timeout
- **chain 中位置**：在 audit 外、review_gate 内——audit-pending 行在 timeout 时仍写入（保留 timeout 分类）

## 8. 注意事项

- **DefaultTimeout 15s**：经验值；某些工具（correlate_incident）需在注册时 override
- **不传播 ctx.Done() 快速 shutdown**：runBatch 等场景 inner 自己的 per-tool timeout 主导
- **well-behaved tool 假设**：belt-and-braces 是防御性，工具应主动尊重 ctx
- **timeout 覆盖**：`WithTimeout` 传入 d<=0 时回退默认；注册时显式传 d 可覆盖
