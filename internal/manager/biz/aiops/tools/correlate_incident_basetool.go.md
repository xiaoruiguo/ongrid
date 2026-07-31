# `correlate_incident_basetool.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/correlate_incident_basetool.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件是 `correlate_incident` 的 N+15 batch refactor BaseTool 形式：外层 fan-out 多 incident_id，每个 incident 内部跑完整 metric+log+trace+edge 关联诊断（逻辑与闭包路径 executeCorrelateIncident 相同）。schema cap 16 但 WhenToUse 强烈建议 2-4——每个 inner 已 3 路并发，给 16 个成本爆炸。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 Registry 注册和 agent loop 调用；依赖 `basetool`、`alertbiz`、`devicebiz`、`edgebiz`、`devicemodel`、`logquery`、`tracequery`

## 3. 关键类型与接口

```go
type CorrelateIncidentTool struct {
    alertUC    AlertUsecase
    promQuery  PromQuerier
    logQuery   LogQuerier
    traceQuery TraceQuerier
    edges      *edgebiz.Usecase
    devices    *devicebiz.Usecase
    log        *slog.Logger
}

type CorrelateIncidentBatchArgs struct {
    IncidentIDs   []uint64
    WindowMinutes int
}

type CorrelateIncidentResultEntry struct {
    IncidentID uint64
    Bundle     *correlateIncidentBundle
    Error      string
}

type CorrelateIncidentBatchResponse struct {
    SuccessCount, ErrorCount int
    Results                   []CorrelateIncidentResultEntry
}
```

## 4. 关键函数与流程

### `NewCorrelateIncidentTool`
- 构造 Tool，注入 alertUC/promQuery/logQuery/traceQuery/edges/devices；log nil → slog.Default()

### `Info`
- 返回 `ToolInfo{Name: ToolNameCorrelateIncident, Description, WhenToUse: correlateIncidentWhenToUse, Parameters: CorrelateIncidentBatchSchema, Class: "read"}`

### `singleCorrelate`
- **签名**：`func (t *CorrelateIncidentTool) singleCorrelate(ctx, incidentID uint64, window int) CorrelateIncidentResultEntry`
- **职责**：per-incident bundle 组装（与闭包 executeCorrelateIncident 同逻辑）
- **流程**：
  1. incidentID 0 → entry.Error
  2. `context.WithTimeout(ctx, correlateIncidentTimeout)`
  3. alertUC.GetIncident；err/nil → entry.Error
  4. 算窗口 half = window/2 分钟
  5. 构 bundle skeleton
  6. Metric panel：promQuery 非 nil 且 promExprForIncident 命中 → queryMetricPanel
  7. Log panel：logQuery 非 nil 且 DeviceID 非 nil → queryLogPanel
  8. Trace panel：traceQuery 非 nil 且 service label 非空 → queryTracePanel
  9. Edge snapshot：DeviceID 非 nil 且 edges 非 nil → queryEdgeSnapshot
  10. Skipped/Truncated 空 → 设 nil
  11. entry.Bundle = bundle
- **错误处理**：所有错误 fold 进 entry.Error，不让 fan-out 全局失败

### `InvokableRun`
- **签名**：`func (t *CorrelateIncidentTool) InvokableRun(ctx, argsJSON string, _ ...basetool.InvokeOption) (string, error)`
- **职责**：parse → validate → fan-out → marshal envelope
- **流程**：
  1. alertUC nil → error
  2. unmarshal CorrelateIncidentBatchArgs
  3. validateBatchIDs("incident_ids", IncidentIDs)
  4. window 钳位 [1,240]，默认 30
  5. `runBatch(ctx, IncidentIDs, func(ctx, id) { return t.singleCorrelate(ctx, id, window) })`
  6. 统计 success/error count
  7. marshal CorrelateIncidentBatchResponse

### `queryMetricPanel / queryLogPanel / queryTracePanel / queryEdgeSnapshot`
- **镜像 Registry 同名方法**：逻辑与闭包路径完全一致（注释明示"mirrors Registry.xxx"）
- 之所以复制而非共享：basetool 路径持有自己的 deps（alertUC/promQuery/...），不依赖 Registry

## 5. 依赖关系

- **内部包**：`basetool`、`alertbiz`、`devicebiz`、`edgebiz`、`devicemodel`、`logquery`、`tracequery`
- **外部库**：标准库 `context`、`encoding/json`、`errors`、`fmt`、`log/slog`、`sort`、`strings`、`time`
- **被调用方**：Registry 注册；agent loop

## 6. 并发与资源管理

- 外层 `runBatch` 4 并发上限（batch_helper.go）
- 每个 inner `singleCorrelate` 独立 60s timeout
- inner 内部 edge snapshot 用独立 5s/3s timeout 隔离慢 probe
- IIFE 隔离 ListIncidents 的 WithTimeout cancel
- Tool struct immutable，多 goroutine 共享安全

## 7. 设计模式与亮点

- **batch fan-out + 内层并发**：外层 4 并发 fan-out incidents；每个 inner 内部 3 路并发（prom+log+trace）——双层并发
- **WhenToUse 强烈建议 2-4**：schema cap 16 但每个 inner 重，给 16 个成本爆炸
- **镜像闭包逻辑**：singleCorrelate 与 executeCorrelateIncident 逻辑一致；4 个 query 方法镜像 Registry 同名方法
- **partial failure in-band**：inner 错误 fold 进 entry.Error，不让 fan-out 全局失败
- **shared envelope**：success_count/error_count + results 切片，caller 一次遍历统计
- **Class="read"**：correlate 不 mutate，走只读路径

## 8. 注意事项

- **cap 16 但建议 2-4**：每个 inner 3 路并发 + 60s timeout，16 个会成本爆炸
- **window_minutes 钳位 [1,240]**：默认 30，共享给所有 id
- **镜像闭包方法**：queryMetricPanel 等复制自 Registry，改一处需同步两处
- **inner 60s timeout**：correlateIncidentTimeout 60s 是 per-incident 上限
- **edge snapshot best-effort**：慢 probe 不阻塞 bundle
- **closure 路径不变**：executeCorrelateIncident 仍存在，basetool 是新增路径
