# query_incidents.go

## 1. 概述

本文件实现 `query_incidents` 工具的闭包路径，列 ongrid alert incidents 按 severity / status / edge / rule_key / since-window 过滤。典型用例："过去 24h 有几条 critical incident" 或 "show open incidents on edge X"。

返回 trimmed envelope `{id, title, severity, status, rule, rule_name, device_id, scope_type, first_fired_at, last_fired_at, event_count, acknowledged_at, resolved_at}`，不含 incident 的 payload / context 等大字段（那些走 `get_incident_detail`）。

## 2. 包信息

- **包名**：`tools`
- **路径**：`internal/manager/biz/aiops/tools/query_incidents.go`
- **导入**：
  - `alertbiz`（`internal/manager/biz/alert`）—— `IncidentFilter`
  - `alertmodel`（`internal/manager/model/alert`）—— status 常量 + 校验
- **Class**：`read`

## 3. 关键类型与接口

### `QueryIncidentsArgs`

```go
type QueryIncidentsArgs struct {
    Severity     string `json:"severity,omitempty"`     // info/warning/critical
    Status       string `json:"status,omitempty"`       // open/acknowledged/silenced/resolved
    SinceMinutes int    `json:"since_minutes,omitempty"` // 默认 1440（24h）
    // DeviceID 接受新 "device_id" key 和 legacy "edge_id" key（兼容旧 prompt）
    DeviceID uint64 `json:"device_id,omitempty"`
    EdgeID   uint64 `json:"edge_id,omitempty"`
    RuleKey  string `json:"rule_key,omitempty"`
    Limit    int    `json:"limit,omitempty"`            // 默认 50，cap 500
}
```

**双键兼容**：`DeviceID` 和 `EdgeID` 同时存在，运行时 `devID = DeviceID; if devID == 0 { devID = EdgeID }`，让旧 prompt 写 `edge_id` 仍能工作。

### `IncidentRow`（trimmed envelope）

```go
type IncidentRow struct {
    ID             uint64     `json:"id"`
    Title          string     `json:"title"`
    Severity       string     `json:"severity"`
    Status         string     `json:"status"`
    Rule           string     `json:"rule"`
    RuleName       string     `json:"rule_name"`
    DeviceID       *uint64    `json:"device_id,omitempty"`
    ScopeType      string     `json:"scope_type"`
    FirstFiredAt   time.Time  `json:"first_fired_at"`
    LastFiredAt    time.Time  `json:"last_fired_at"`
    EventCount     uint64     `json:"event_count"`
    AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
    ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
}
```

`DeviceID` 是 `*uint64`（指针），`nil` 时 omitempty 省略——兼容 scope_type 不是 device 的 incident（如 scope=device_group）。

## 4. 关键函数与流程

### `executeQueryIncidents(ctx, args) (ExecuteResult, error)`

1. 校验 `r.alertUC == nil` → error。
2. Unmarshal，校正 limit（≤0 → 50，>500 → 500）。
3. `SinceMinutes ≤ 0` → 1440（24h）。`cutoff = now - since_minutes`。
4. Severity 白名单校验（`info/warning/critical`，否则 error）。
5. Status 白名单校验（用 `alertmodel.IncidentStatus*` 常量，否则 error）。
6. 构造 `alertbiz.IncidentFilter{Status, Severity, RuleKey, Limit: in.Limit * 4}`。
   - **Limit × 4**：`IncidentFilter` 不支持 `since_minutes` 原生过滤，所以拉 4 倍再在内存按 `LastFiredAt` 过滤，给 since 过滤留余量。
7. `devID = DeviceID; if devID == 0 { devID = EdgeID }; if devID > 0 { f.DeviceID = &devID }`。
8. `context.WithTimeout(ctx, 15s)`，调 `r.alertUC.ListIncidents(callCtx, f)`。
9. 遍历 `all`，跳过 `inc.LastFiredAt.Before(cutoff)`，构造 `IncidentRow`，截断到 `in.Limit`。
10. Marshal `{incidents: rows, count: len(rows)}` 返回。

## 5. 依赖关系

| 方向 | 依赖 | 说明 |
|------|------|------|
| 上游 | `Registry`（闭包持有者） | `r.alertUC` |
| 下游 | `AlertUsecase.ListIncidents` | 数据源 |
| 类型 | `alertbiz.IncidentFilter` | 过滤参数 |
| 类型 | `alertmodel.IncidentStatus*` 常量 | status 白名单 |

## 6. 并发与资源管理

- `context.WithTimeout(ctx, 15s)` per call。
- 无共享可变状态。
- Limit cap 500 + `Limit * 4` 拉取策略 → DB 端最多拉 2000 行，内存过滤后 ≤ 500。

## 7. 设计模式与亮点

- **`Limit * 4` 预拉策略**：因为 `IncidentFilter` 不原生支持 `since_minutes`，代码用 4 倍 limit 拉取再在内存过滤。注释明示这个 trade-off，是个常见的"DB 过滤能力不足 → 内存补"模式。
- **双键兼容（`DeviceID` / `EdgeID`）**：post-split 后字段名从 `edge_id` 改 `device_id`，但保留 legacy 键以兼容旧 prompt——这种"新键优先 + legacy fallback"在 ongrid 多处出现（如 `EdgeRow.ID` 的 json tag 是 `device_id`）。
- **`DeviceID *uint64` 指针**：`IncidentRow.DeviceID` 是 `*uint64`，能区分"未设"和"0"，且 omitempty 省略——兼容 scope 不是 device 的 incident。
- **白名单校验 fail fast**：Severity / Status 不在白名单直接 error，避免 `ListIncidents` 收到未知值返回空被 LLM 误解为"没数据"。
- **trimmed envelope**：只暴露 LLM 推理需要的字段，不传 payload/context（那些走 `get_incident_detail`），控制上下文成本。

## 8. 注意事项

- **`Limit * 4` 上限是 2000**：若 24h 内 incident 数远超 2000 且 since_minutes 较大，4 倍预拉可能仍不够——会 silently 漏数据。生产场景下 24h × 2000 通常足够，但高峰期要注意。
- **15s 超时**：比 `query_alert_rules` 的 10s 长，因为 `ListIncidents` 可能要 join events 表算 `EventCount`，相对慢。
- **`LastFiredAt` 而非 `FirstFiredAt` 做 since 过滤**：语义是"最近一次触发在窗口内"，不是"首次触发在窗口内"——长 running incident 不会被排除。
- **闭包路径与 BaseTool 路径并存**：见 `query_incidents_basetool.go`，两路径 byte-for-byte 等价，drift 风险同其他工具。
- **`ScopeType` 字段透传**：返回行带 `scope_type`，让 LLM 知道 incident 是 device-scoped 还是 group-scoped，配合 `DeviceID *uint64` 的 omitempty 处理 group-scoped 场景。
- **`AcknowledgedAt` / `ResolvedAt` 用 `*time.Time`**：能区分"未 ack"和"零时刻"，omitempty 自动省略。
