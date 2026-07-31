# query_incidents_basetool.go

## 1. 概述

本文件实现 `query_incidents` 工具的 BaseTool 形态。镜像 `executeQueryIncidents`（见 `query_incidents.go`）。当 LLM 收到用户问 "过去 24h 有几条 critical incident" 或 "show open incidents on edge X" 时调它。

WhenToUse 明确反 guard：**NOT** for 单 incident 时间线（用 `get_incident_detail`）/ 完整诊断 bundle（用 `correlate_incident`）/ 原始 metric/log/trace（用对应 `query_*` 工具）。

## 2. 包信息

- **包名**：`tools`
- **路径**：`internal/manager/biz/aiops/tools/query_incidents_basetool.go`
- **导入**：
  - `basetool`
  - `alertbiz` / `alertmodel`（同闭包路径）
  - 额外 `log/slog`
- **Class**：`read`

## 3. 关键类型与接口

### `QueryIncidentsTool`

```go
type QueryIncidentsTool struct {
    alertUC AlertUsecase
    log     *slog.Logger
}
```

注意：BaseTool 形态用 `AlertUsecase` 接口（而非 `*alertbiz.Usecase` 具体类型），与 `QueryEdgesTool` 直接持有具体 usecase 的做法不同——这是因为 `AlertUsecase` 接口在 tools 包内已经存在，复用即可。

`QueryIncidentsArgs` / `IncidentRow` / `QueryIncidentsSchema` / `QueryIncidentsDescription` / `ToolNameQueryIncidents` / `queryIncidentsCallTimeout` 均复用 `query_incidents.go` 的定义。

## 4. 关键函数与流程

### `NewQueryIncidentsTool(alertUC, log)`

`log == nil` → `slog.Default()`。

### `queryIncidentsWhenToUse`（常量）

英文 LLM-facing 文案，反 guard 强：
- 用途：LIST recent alert incidents（"过去 24h 有几条 critical incident" / "show open incidents on edge X"）
- NOT for：单 incident 时间线（`get_incident_detail`）/ 完整诊断 bundle（`correlate_incident`）/ 原始 metric/log/trace（`query_promql` / `query_logql` / `query_traceql`）

### `Info(_ context.Context)`

返回 `ToolInfo`：Name=`query_incidents`，Class=`read`，含 WhenToUse。

### `InvokableRun(ctx, argsJSON, _ ...)`

主流程与 `executeQueryIncidents` 完全镜像：

1. 校验 `alertUC == nil` → error。
2. Unmarshal，校正 limit（50 / 500）。
3. `SinceMinutes ≤ 0` → 1440。`cutoff = now - since_minutes`。
4. Severity / Status 白名单校验。
5. 构造 `IncidentFilter{Status, Severity, RuleKey, Limit: in.Limit * 4}`。
6. `devID = DeviceID; if 0 → EdgeID; if >0 → f.DeviceID = &devID`。
7. `context.WithTimeout(ctx, 15s)`，调 `alertUC.ListIncidents`。
8. 内存过滤 `LastFiredAt.Before(cutoff)`，截断到 `Limit`。
9. Marshal `{incidents: rows, count: len(rows)}` 返回。

## 5. 依赖关系

| 方向 | 依赖 | 说明 |
|------|------|------|
| 上游 | `basetool.BaseTool` / `ToolInfo` | 实现 BaseTool 接口 |
| 下游 | `AlertUsecase.ListIncidents` | 数据源（接口） |
| 共享 | `query_incidents.go` 中的 `QueryIncidentsArgs` / `IncidentRow` / `QueryIncidentsSchema` / `ToolNameQueryIncidents` / `queryIncidentsCallTimeout` | 避免重复定义 |

## 6. 并发与资源管理

- `context.WithTimeout(ctx, 15s)` per call。
- 无共享可变状态，并发安全。
- Limit cap 500 + `Limit * 4` 拉取策略，DB 端最多 2000 行。

## 7. 设计模式与亮点

- **镜像承诺**：注释明示 "Mirror of executeQueryIncidents"，与闭包路径语义等价。共享 schema / args / row 类型避免 schema drift。
- **WhenToUse 三重反 guard**：明确列出三个 NOT for（单 incident / 完整 bundle / 原始 metric/log/trace），引导 LLM 在不同 incident 相关问题里选对工具。
- **接口依赖**：用 `AlertUsecase` 接口而非具体类型，便于 mock 测试（与 `QueryAlertRulesTool` 同模式）。
- **零行为差**：所有校验顺序、limit 修正、`Limit * 4` 策略、内存过滤逻辑都与闭包路径一致，保证 byte-for-byte 输出。

## 8. 注意事项

- **drift 风险**：与 `query_incidents.go` 是两份并行实现，任何一边改逻辑必须同步另一边。
- **无 batch refactor**：与 `get_incident_detail_basetool.go`（走 N+15 batch）不同，这个工具本身就是 list 语义，不需要 `incident_ids[]` batch 协议。
- **15s 超时**：相对较长，因为 `ListIncidents` 可能 join events 表；高峰期要注意。
- **`Limit * 4` 策略继承**：和闭包路径同样的预拉策略，同样的 silently 漏数据风险（极端高峰场景）。
- **未走 `basetool.InvokeOption`**：`_ ...basetool.InvokeOption` 忽略 opts，因为这个工具不需要 tenant_bind / locale 等 ctx value（query 只读，无 mutation，无 i18n）。
