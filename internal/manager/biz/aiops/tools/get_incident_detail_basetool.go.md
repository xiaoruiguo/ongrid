# `get_incident_detail_basetool.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/get_incident_detail_basetool.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件是 `get_incident_detail` 工具的 **BaseTool 形态**，N+15 batch refactor 后支持 `incident_ids[]`（1..16）。每个 inner 调用是纯 DB 读（`alertUC.GetIncident` + `ListEvents`），fan-out 几乎零成本。LLM 经常同时关联 3-5 个 incident，batch 省去逐个调用的 4-8 轮 roundtrip。`runBatch` 4 并发 fan-out，per-id 10s 超时（`incidentDetailCallTimeout`，复用闭包路径常量）。`Class="read"`。`WhenToUse` 中文，强调 "一次拉一组" 与反 guard "NOT for 列 incidents / 关联诊断 / ad-hoc 查询"。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 Registry BaseTool 路径注册调用；依赖 `basetool`、`AlertUsecase`（`GetIncident` / `ListEvents`）、`runBatch`（`batch_helper.go`）、`validateBatchIDs`。与闭包路径 `get_incident_detail.go` 并存。

## 3. 关键类型与接口

```go
type GetIncidentDetailTool struct {
    alertUC AlertUsecase
    log     *slog.Logger
}

type GetIncidentDetailBatchArgs struct {
    IncidentIDs []uint64 `json:"incident_ids"` // 1..16
}

type IncidentDetailResultEntry struct {
    IncidentID uint64             `json:"incident_id"`
    Incident   map[string]any     `json:"incident,omitempty"`   // 完整 incident 行
    Timeline   []IncidentEventRow `json:"timeline,omitempty"`   // 事件时间线
    Error      string             `json:"error,omitempty"`      // 仅失败才填
}

type IncidentDetailBatchResponse struct {
    SuccessCount, ErrorCount int
    Results                  []IncidentDetailResultEntry
}
```

`GetIncidentDetailBatchSchema`：`maxItems: 16`，description 中文明示 "LLM 经常同时关联多个 alert，一次拉一组比逐个调省 4-8 轮"。

复用 `get_incident_detail.go` 定义的共享类型：`IncidentEventRow`、`ToolNameGetIncidentDetail`、`GetIncidentDetailDescription`、`incidentDetailCallTimeout`。

## 4. 关键函数与流程

```go
func NewGetIncidentDetailTool(alertUC AlertUsecase, log *slog.Logger) *GetIncidentDetailTool
func (t *GetIncidentDetailTool) Info(_ context.Context) (*basetool.ToolInfo, error)
func (t *GetIncidentDetailTool) singleIncidentDetail(ctx, incidentID uint64) IncidentDetailResultEntry
func (t *GetIncidentDetailTool) InvokableRun(ctx, argsJSON, _ ...InvokeOption) (string, error)
```

**`InvokableRun` 流程**：
1. 守门 `alertUC != nil`。
2. Unmarshal → `GetIncidentDetailBatchArgs`。
3. `validateBatchIDs("incident_ids", IncidentIDs)` 校验（maxItems 16）。
4. `runBatch(ctx, IncidentIDs, t.singleIncidentDetail)` 4 并发 fan-out，返回有序 `[]IncidentDetailResultEntry`。
5. 统计 `SuccessCount` / `ErrorCount`，Marshal `IncidentDetailBatchResponse` 返回。

**`singleIncidentDetail` 流程**（与闭包路径 `executeGetIncidentDetail` 字节级一致）：
1. `incidentID == 0` → `Error = "incident_id must be > 0"`。
2. `context.WithTimeout(ctx, incidentDetailCallTimeout=10s)`。
3. `alertUC.GetIncident(callCtx, incidentID)` → 失败 → `Error = "get: %v"`；`inc == nil` → `Error = "incident %d not found"`。
4. `alertUC.ListEvents(callCtx, incidentID, 200)` → 失败 → `Error = "events: %v"`。
5. 转 `timeline []IncidentEventRow`（字段映射与闭包路径一致）。
6. 构造 `Incident map[string]any`（含 id/rule/rule_name/title/severity/status/scope_type/device_id/summary/description/value/threshold/event_count/first_fired_at/last_fired_at/last_notified_at/silenced_until/acknowledged_at/resolved_at/runbook_url）。
7. `entry.Incident = inc map`，`entry.Timeline = timeline`。

## 5. 依赖关系

- **basetool**：`ToolInfo`（`Class="read"`）、`InvokeOption`（忽略）。
- **AlertUsecase**：`GetIncident` / `ListEvents`。接口在 `correlate_incident.go` 定义。
- **runBatch**（`batch_helper.go`）：泛型 fan-out，4 并发，有序切片。
- **validateBatchIDs**（`batch_helper.go`）：maxItems 16 校验。
- 不依赖 devicebiz / edgebiz / prom / log / trace / tunnel。

## 6. 并发与资源管理

- **`runBatch` 4 并发**：`batchConcurrency=4`，最多 4 个 `singleIncidentDetail` 同时跑。16 个 incident 最坏 `⌈16/4⌉=4` 轮，每轮 10s → 40s，纯 DB 读实际更快。
- **per-id 10s 超时**：`singleIncidentDetail` 内 `context.WithTimeout(ctx, incidentDetailCallTimeout=10s)`。即便 outer 未到，单 id 也不会超 10s。
- **无锁**：`GetIncidentDetailTool` 字段不变，`singleIncidentDetail` 内变量局部。`runBatch` 内部用 semaphore + waitgroup，无共享可变状态。
- **失败 fold 进 `Error`**：`GetIncident` / `ListEvents` 失败不 panic，转成 `entry.Error` 字符串，其他 id 仍正常返回。

## 7. 设计模式与亮点

- **Batch-first refactor（N+15）**：从单 id 升级到 `incident_ids[]` 一次最多 16 个。注释明示 "LLM commonly correlates 3-5 incidents at once and was burning rounds doing them sequentially"——batch 直接省 4-8 轮。
- **inner 与闭包路径字节级一致**：`singleIncidentDetail` 的逻辑（GetIncident + ListEvents + timeline 转换 + incident map 构造）与 `Registry.executeGetIncidentDetail` 完全相同。未来应抽出共享 helper 避免 drift。
- **`SuccessCount` / `ErrorCount` envelope**：批量响应统一信封，LLM 能快速判断 "几个成功几个失败"。
- **`Error` 仅失败才填**：成功时 `Incident` + `Timeline` 非空，`Error` 空；失败时 `Incident`/`Timeline` 空，`Error` 非空。`omitempty` 让 JSON 紧凑。
- **`WhenToUse` 中文 + 反 guard**：明确 "NOT for: 列 incidents（用 query_incidents）/ 关联诊断（用 correlate_incident）/ ad-hoc metric/log 查询（用 query_promql / query_logql）"，引导 LLM 正确路由。
- **纯 DB 读 fan-out 几乎零成本**：注释明示 "Each inner call is a pure DB read so fan-out is essentially free"——与 `correlate_incident`（每个 inner 3 路上游 probe）不同，本工具的 batch 没有成本爆炸风险。

## 8. 注意事项

- **`ListEvents(limit=200)` 截断**：与闭包路径一致，长生命周期 incident 可能事件数 > 200，timeline 不全。batch 下若多个 incident 都超 200，LLM 需 awareness。
- **inner 与闭包路径 drift 风险**：`singleIncidentDetail` 与 `executeGetIncidentDetail` 是两份实现，任何 incident map 字段 / timeline 转换修改需同步两处。未来应抽 `singleIncidentDetail(ctx, alertUC, id) (map, []IncidentEventRow, error)` 让两路径都代理调用。
- **`InvokeOption` 被忽略**：BaseTool 路径下 `opts` 不影响行为。
- **batch 顺序保证**：`runBatch` 返回有序切片，`results[i]` 对应 `IncidentIDs[i]`，LLM 可对位解读。
- **无 `ExecuteResult.DeviceID` 回传**：与闭包路径不同，BaseTool 路径返回纯字符串（`InvokableRun` 签名），无法回传 `DeviceID` 给上层 graph/audit。若需 device 维度统计，依赖 audit 装饰器从 args/结果解析。
- **`incident_ids` 必填**：schema `minItems: 1`，不支持 "all incidents" 模式（与 `get_edge_summary_basetool` 的 "all edges" 不同）。LLM 需先用 `query_incidents` 拿 id 列表。
