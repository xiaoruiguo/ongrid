# `get_incident_detail.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/get_incident_detail.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件实现 `get_incident_detail` 工具（闭包路径，挂在 `Registry.executeGetIncidentDetail`）：返回单条 incident 的完整行 + 事件时间线（firing / ack / resolve / notification_sent / notification_failed 等）。当问题聚焦 "某个具体 incident id 发生了什么" 时使用。10s 调用超时。一次 `GetIncident` + 一次 `ListEvents(limit=200)` 拼合响应，timeline 转成 trimmed `IncidentEventRow`。`ExecuteResult.DeviceID` 从 `inc.DeviceID` 回传上层 graph/audit 用于关联。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 Registry 闭包路径调用；依赖 `AlertUsecase`（`GetIncident` / `ListEvents`，接口在 `correlate_incident.go` 定义）。与 `get_incident_detail_basetool.go`（BaseTool 镜像）并存。

## 3. 关键类型与接口

```go
type GetIncidentDetailArgs struct {
    IncidentID uint64 `json:"incident_id"` // 必填，minimum 1
}

// 时间线单条事件，trimmed 自 alertmodel.Event。
type IncidentEventRow struct {
    ID          uint64    `json:"id"`
    EventType   string    `json:"event_type"`     // firing/ack/resolve/notification_sent/notification_failed/...
    StatusAfter string    `json:"status_after"`
    Severity    string    `json:"severity"`
    Title       string    `json:"title"`
    Message     *string   `json:"message,omitempty"`
    ActorType   string    `json:"actor_type"`     // user/system/reviewer/...
    ActorID     *uint64   `json:"actor_id,omitempty"`
    Reason      string    `json:"reason,omitempty"`
    OccurredAt  time.Time `json:"occurred_at"`
}

const incidentDetailCallTimeout = 10 * time.Second
```

依赖 `AlertUsecase` 接口（`correlate_incident.go` 定义）：
```go
type AlertUsecase interface {
    GetIncident(ctx, id uint64) (*alertmodel.Incident, error)
    ListEvents(ctx, incidentID uint64, limit int) ([]*alertmodel.Event, error)
    // ... ListIncidents / ListRules 略
}
```

## 4. 关键函数与流程

```go
func (r *Registry) executeGetIncidentDetail(ctx, args json.RawMessage) (ExecuteResult, error)
```

流程：
1. 守门 `r.alertUC != nil`。
2. Unmarshal → `GetIncidentDetailArgs`；`IncidentID == 0` → 报错 "incident_id required"。
3. `context.WithTimeout(ctx, incidentDetailCallTimeout=10s)`。
4. `r.alertUC.GetIncident(callCtx, IncidentID)` → `*alertmodel.Incident`。err 上抛 "get: %w"。
5. `r.alertUC.ListEvents(callCtx, IncidentID, 200)` → `[]*alertmodel.Event`。err 上抛 "events: %w"。
6. 转_timeline：遍历 events → `IncidentEventRow{ID, EventType, StatusAfter, Severity, Title, Message, ActorType, ActorID, Reason, OccurredAt}`。
7. 构造 `out = {"incident": {完整字段 map}, "timeline": timeline}`。incident map 包含 id/rule/rule_name/title/severity/status/scope_type/device_id/summary/description/value/threshold/event_count/first_fired_at/last_fired_at/last_notified_at/silenced_until/acknowledged_at/resolved_at/runbook_url。
8. Marshal → `ExecuteResult{ResultJSON: body, DeviceID: edgeID}`。`edgeID` 从 `inc.DeviceID`（`*uint64`）复制出来，nil 则不填。

## 5. 依赖关系

- **AlertUsecase**：`GetIncident` / `ListEvents`。接口在消费方（tools 包）定义，符合架构红线。
- **alertmodel.Incident**：完整 incident 行（含 Rule/RuleName/Severity/Status/ScopeType/DeviceID/Summary/Description/Value/Threshold/EventCount/FirstFiredAt/LastFiredAt/LastNotifiedAt/SilencedUntil/AcknowledgedAt/ResolvedAt/RunbookURL）。
- **alertmodel.Event**：事件行（ID/EventType/StatusAfter/Severity/Title/Message/ActorType/ActorID/Reason/OccurredAt）。
- 不依赖 devicebiz / edgebiz / prom / log / trace / tunnel。

## 6. 并发与资源管理

- **无锁、无共享可变状态**：`Registry` 字段不变，`executeGetIncidentDetail` 内变量局部。多 goroutine 可并发调用。
- **无 goroutine**：串行 `GetIncident` → `ListEvents`，10s 超时覆盖。两次 DB 查询都受 `callCtx` 控制。
- **`ListEvents(limit=200)` 截断**：超过 200 条事件的 incident 会丢数据。当前规模够用；若需更多应分页或加 `offset` 参数。

## 7. 设计模式与亮点

- **One-shot stitch**：一次调用拼合 incident + timeline，省 LLM 两轮调用。比 `query_incidents` + 单独查事件更省 roundtrip。
- **timeline trimmed envelope**：`IncidentEventRow` 只保留 LLM 推理最常用字段，省 prompt token。完整 event 行（如有更多 metadata）可另查。
- **`ExecuteResult.DeviceID` 回传**：从 `inc.DeviceID` 提取 edge_id 给上层 graph/audit，便于按 edge 维度关联审计与统计。这是闭包路径 `ExecuteResult` 的设计意图。
- **`*string` / `*uint64` 可选字段**：`Message` / `ActorID` 用指针区分 "未设置" 与 "零值"，避免零值歧义。`omitempty` 让 JSON 紧凑。
- **10s 超时偏紧**：纯 DB 查询，应秒级返回；10s 兜底慢查询。
- **incident map 扁平**：所有 incident 字段铺平到一个 map[string]any，无嵌套，LLM 易读。

## 8. 注意事项

- **`ListEvents(limit=200)` 截断**：长生命周期 incident（如持续告警数月）可能事件数 > 200，timeline 不全。当前未暴露 offset/分页参数，LLM 无法翻页。若需全量应扩展 schema 加 `offset` 或 `event_type` 过滤。
- **无 `event_type` 过滤**：`ListEvents` 拉所有类型事件（firing/ack/resolve/notification_sent/notification_failed/...）。LLM 无法在工具层过滤，需在 prompt 里自行筛选。
- **闭包路径独有**：本文件是 `Registry.executeGetIncidentDetail`。BaseTool 形态在 `get_incident_detail_basetool.go`，后者支持 batch（`incident_ids[]`）。
- **`inc.DeviceID` 可能 nil**：scope_type 不是 device 的 incident（如 cluster 级、tenant 级告警）DeviceID 为 nil，`ExecuteResult.DeviceID` 不填。上层 graph/audit 需容忍 nil。
- **无 tenant 过滤**：依赖 `AlertUsecase` 内部按 ctx tenant 过滤。`tenant_bind` 装饰器注入 tenant_id 到 ctx，alert UC 应从 ctx 读取。`GetIncident(id)` 看似无 tenant 参数，但 UC 实现应校验 incident 归属当前 tenant。
- **`runbook_url` 字段**：incident 行含 runbook URL，LLM 可直接引用给用户。若 nil 则不出现该字段。
- **timeline 顺序**：`ListEvents` 返回顺序由 alertbiz 实现，本工具不重排。LLM 若需按 OccurredAt 排序应自行处理。
