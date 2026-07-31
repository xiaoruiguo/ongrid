# query_change_events_basetool.go

## 1. 概述

本文件实现 `query_change_events` 工具的 BaseTool 形态，属于 HLD-013 Phase 2。它给 RCA（根因分析）调查者一个"症状时间附近发生了什么变更"的信号，数据源是 HLD-010 的 audit log。常见动机是："症状发生前有人改了规则 / 设置 / 设备"，把这类产品介导的变更暴露给因果回溯循环作为根因候选。

**Scope honesty**：audit log 只捕获"经 ongrid（admin UI / API）发起的变更"。主机上的外部改动（SSH 编辑、out-of-band 部署、orchestrator 容器 churn）这里看不到——需要 edge-side change feed（未来）。工具描述里明确写明这一点，防止 LLM 对空结果过度信任。

只提供 BaseTool 形态，无对应闭包路径。

## 2. 包信息

- **包名**：`tools`
- **路径**：`internal/manager/biz/aiops/tools/query_change_events_basetool.go`
- **导入**：
  - `basetool`（`internal/manager/biz/aiops/tools/basetool`）—— BaseTool 接口
  - `auditmodel`（`internal/manager/model/audit`）—— `audit.Log` 类型
- **Class**：`read`（纯查看，无 mutation）

## 3. 关键类型与接口

### `AuditLister`（接口）

```go
type AuditLister interface {
    ListChanges(ctx context.Context, from, to time.Time, resourceType, action string, limit int) ([]auditmodel.Log, error)
}
```

窄接口 seam，由 `*biz/audit.Usecase` 直接满足。故意使用 primitive 参数（`time.Time`、`string`、`int`），让 tools 包不依赖 data/store 层，只需 model 类型。

### `QueryChangeEventsArgs`

```go
type QueryChangeEventsArgs struct {
    AroundTS     string `json:"around_ts"`      // RFC3339 锚点，省略取 now
    WindowMin    int    `json:"window_minutes"` // 半窗口分钟数，默认 30
    ResourceType string `json:"resource_type"`  // rule/device/setting/...
    Action       string `json:"action"`         // rule_update/setting_update/...
    Limit        int    `json:"limit"`          // 默认 50，cap 200
}
```

窗口以 `around_ts` 为中心，取前后各 `window_minutes` 分钟。

### `changeEventRow`

```go
type changeEventRow struct {
    OccurredAt   string `json:"occurred_at"`
    Actor        string `json:"actor"`            // user_email
    Role         string `json:"role,omitempty"`
    Action       string `json:"action"`
    ResourceType string `json:"resource_type"`
    ResourceID   string `json:"resource_id,omitempty"`
    ResourceName string `json:"resource_name,omitempty"`
    Status       string `json:"status"`
    Payload      string `json:"payload,omitempty"` // payload_json 原样
}
```

### `QueryChangeEventsTool`

```go
type QueryChangeEventsTool struct {
    audit AuditLister
    log   *slog.Logger
}
```

## 4. 关键函数与流程

### `NewQueryChangeEventsTool(a, log)`

构造器。`log == nil` 时回退到 `slog.Default()`。`a` 可为 nil（仅在测试里允许，生产路径会 fail）。

### `Info(_ context.Context)`

返回 `ToolInfo`：Name=`query_change_events`，Class=`read`，含 `WhenToUse` 反 guard 文案。

### `queryChangeEventsWhenToUse`（常量）

中文 LLM-facing 文案，引导：用 incident `fired_at` 作 `around_ts`，查前后 ±30 分钟内有没有 `rule_update` / `setting_update` / `device_update`。**强调**：空结果也是有效发现（这段时间没有产品侧变更）。NOT for 主机外部变更 / 指标趋势 / 日志。

### `InvokableRun(ctx, argsJSON, _ ...)`

主流程：

1. 校验 `audit` 非空，否则 `"audit lister not configured"`。
2. Unmarshal args。
3. 解析 `around_ts`：trim 后非空则 `time.Parse(time.RFC3339)`，失败带原始字符串报错；空则取 `time.Now().UTC()`。
4. `WindowMin ≤ 0` → 30；`Limit ≤ 0` → 50；`Limit > 200` → 200。
5. 计算 `half = window_minutes * time.Minute`，`from = anchor - half`，`to = anchor + half`（UTC）。
6. `context.WithTimeout(ctx, 5*time.Second)`，调 `audit.ListChanges(callCtx, from, to, resourceType, action, limit)`。
7. 遍历 `logs` 构造 `changeEventRow`：`OccurredAt` 用 `.UTC().Format(time.RFC3339)`，其余字段直传。
8. Marshal `{window: {from, to}, changes: rows, count: len(rows)}` 返回 JSON 字符串。

## 5. 依赖关系

| 方向 | 依赖 | 说明 |
|------|------|------|
| 上游 | `basetool.BaseTool`、`ToolInfo`、`InvokeOption` | 实现 BaseTool 接口 |
| 下游 | `AuditLister`（由 `*biz/audit.Usecase` 实现） | 数据源 |
| 类型 | `auditmodel.Log` | 仅用 model 类型，不依赖 store |

## 6. 并发与资源管理

- 每次调用新建独立 `context.WithTimeout(ctx, 5s)`，自动 `defer cancel()`。
- 工具本身无状态、无锁，多 goroutine 并发安全（依赖 `AuditLister` 实现的线程安全性）。
- `Limit` cap 200，防止 audit log 大量返回撑爆 LLM 上下文。

## 7. 设计模式与亮点

- **窄接口 seam（`AuditLister`）**：tools 包不直接 import `biz/audit`，只用 primitive 参数 + model 类型，避免循环依赖和过度耦合。
- **Scope honesty 显式声明**：工具描述直接告诉 LLM "只覆盖经 ongrid 产品发起的变更，不含主机外部改动"，避免空结果被误读为"没改过任何东西"。
- **窗口以锚点为中心**：`window_minutes` 是"半窗口"，锚点前后各取一份，符合 RCA"时间附近"的直觉。
- **WhenToUse 反 guard**：明确 NOT for 主机外部变更 / 指标 / 日志，防止 LLM 把它当万能"时间窗查询"工具。
- **空结果也是有效发现**：WhenToUse 文案里强调"返回空也是有效发现（这段时间没有产品侧变更）"，引导 LLM 不要把空当成失败重试。

## 8. 注意事项

- **5s 超时**：相对其他工具（10s/15s/90s）偏紧，因为 audit log 查询是 DB 范围扫描 + index，预期快；若 audit 表膨胀可能需要调大。
- **`Limit * 1` 而非 `* 4`**：与 `query_incidents` 不同（那里 `Limit * 4` 预留给内存过滤），这里 audit `ListChanges` 直接按 limit 截断，无后续过滤，1:1。
- **RFC3339 强制**：`around_ts` 必须是 RFC3339，不带时区会失败；LLM 偶尔会传 `2024-01-01 12:00:00` 这种格式，会被 reject。
- **Payload 原样透传**：`PayloadJSON` 字段直接放进 `payload` 字段，可能是大字符串（rule diff 等），LLM 上下文成本要注意。
- **无闭包路径**：此工具只走 BaseTool 形态，没有 `Registry.executeQueryChangeEvents` 对应物，可能是因为它晚于 PR-7 闭包路径清理期加入。
- **ResourceType / Action 不做白名单校验**：与 `query_alert_rules` 的 `IsKnownKind` 不同，这里直接透传给 `ListChanges`，后端容忍空串和未知值（返回空结果），降低 LLM 写错的成本。
