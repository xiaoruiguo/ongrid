# `inhibit.go` 技术实现文档

> 源文件：`internal/manager/biz/alert/inhibit.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/alert`

## 1. 概述

本文件实现 alert 子域的「抑制（inhibition）」机制：当一个高优先级 incident 已经触发时，与之相关联的低优先级 incident 的通知应当被压制，避免告警风暴淹没操作员。当前实现 `BuiltinInhibitor` 内置两条规则：(1) `edge_offline` 抑制同设备的所有 host-scope incident（edge 不可达时，CPU 高/内存高等都是噪声）；(2) `pipeline:prom_ingest_fail` 抑制所有 `pipeline:scrape_down:*` incident（remote_write 自身宕掉时，所有 scrape target 都会显示为 down，只有根因是信号）。inhibition 只压制通知，不影响 incident 行的写入或事件时间线。

## 2. 包信息

- **包名**：`alert`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 `usecase.go::MaybeNotify` 调用；依赖 `internal/manager/model/alert`

## 3. 关键类型与接口

```go
// Inhibitor 决定一个新触发 incident 的通知是否应被抑制。
// 返回 (reason, true) 抑制；(_, false) 放行。
type Inhibitor interface {
    Suppress(ctx context.Context, incident *model.Incident) (string, bool)
}

// IncidentLookup 是 BuiltinInhibitor 需要的 Repo 子集。
type IncidentLookup interface {
    GetIncidentByDedupeKey(ctx context.Context, dedupeKey string) (*model.Incident, error)
}

type BuiltinInhibitor struct {
    repo IncidentLookup
}
```

`Inhibitor` 接口故意窄：仅一个方法，便于将来扩展 admin 自定义 inhibition_rules 表时不破坏现有调用方。`IncidentLookup` 仅暴露 `GetIncidentByDedupeKey`，让测试 fake 不必满足完整 `Repo` 接口。

## 4. 关键函数与流程

### `NewBuiltinInhibitor`

- **签名**：`func NewBuiltinInhibitor(repo IncidentLookup) *BuiltinInhibitor`
- **职责**：构造函数
- **流程**：直接返回 `&BuiltinInhibitor{repo: repo}`
- **错误处理**：无

### `BuiltinInhibitor.Suppress`

- **签名**：`func (i *BuiltinInhibitor) Suppress(ctx context.Context, incident *model.Incident) (string, bool)`
- **职责**：应用两条内置抑制规则
- **流程**：
  1. nil 守卫：`i == nil || i.repo == nil || incident == nil` → `("", false)`
  2. switch 两条规则：
     - **规则 1（edge_offline 抑制 host:X:*）**：
       - 触发条件：`incident.ScopeType == RuleScopeHost && incident.Rule != "edge_offline" && incident.DeviceID != nil`
       - 构造 dedupe key `host:<device_id>:edge_offline`
       - 调 `activeIncident` 查是否仍有未 resolved 的 edge_offline incident
       - 命中 → 返回 `("inhibited by edge_offline incident #<id>", true)`
     - **规则 2（prom_ingest_fail 抑制 scrape_down）**：
       - 触发条件：`incident.ScopeType == RuleScopeMonitoringPipeline && strings.HasPrefix(incident.DedupeKey, "pipeline:scrape_down:")`
       - 查 dedupe key `pipeline:prom_ingest_fail` 的 active incident
       - 命中 → 返回 `("inhibited by prom_ingest_fail incident #<id>", true)`
  3. 都不命中 → `("", false)`
- **错误处理**：`activeIncident` 内部吞掉 DB 错误（返回 false 让通知放行）；抑制失败安全（fail-open）

### `BuiltinInhibitor.activeIncident`

- **签名**：`func (i *BuiltinInhibitor) activeIncident(ctx context.Context, dedupeKey string) (*model.Incident, bool)`
- **职责**：查 dedupe key 对应的非 resolved incident
- **流程**：
  1. `i.repo.GetIncidentByDedupeKey(ctx, dedupeKey)`
  2. err 或 nil incident → `(nil, false)`
  3. `incident.Status == IncidentStatusResolved` → `(nil, false)`（已恢复的根因不再抑制）
  4. 否则 → `(incident, true)`
- **错误处理**：DB 错误吞掉返回 false，让通知放行——宁可多发一次通知，不可错失根因信号

## 5. 依赖关系

- **内部包**：`internal/manager/model/alert`（仅类型）
- **外部库**：标准库 `context` / `fmt` / `strings`
- **被调用方**：`usecase.go::MaybeNotify`（在 silence + cooldown + dampening 闸门之后调用）

## 6. 并发与资源管理

- `BuiltinInhibitor` 无状态、无锁；`Suppress` 可被多 goroutine 并发调用
- 每次调用一次 DB 读（`GetIncidentByDedupeKey`），由调用方的 ctx 控制超时

## 7. 设计模式与亮点

- **接口窄化**：`Inhibitor` 仅一个方法，`IncidentLookup` 仅一个方法，便于测试 fake
- **Fail-open**：DB 错误时返回 false 放行通知，避免 inhibitor 故障导致告警丢失
- **状态机过滤**：只匹配非 resolved 状态的根因 incident——已恢复的根因不再抑制，避免"旧根因已解但下游告警仍被压制"的死锁
- **dedupe key 前缀匹配**：`pipeline:scrape_down:*` 用 `strings.HasPrefix` 匹配，覆盖所有 per-target scrape_down 派生 incident（如 `pipeline:scrape_down:host:9100:node-exporter`）
- **reason 含 incident ID**：返回字符串里带根因 incident 的 #ID，写入 `alert_events` 的 `reason` 字段，操作员在时间线里能看到"被 #N 抑制"的直接线索
- **per-rule pinning 与 inhibition 正交**：`router.go` 的 per-rule channel pinning 在前，inhibition 在通知闸门最后一步；两者职责分离，互不干扰

## 8. 注意事项

- **仅两条内置规则**：当前覆盖了两大操作员痛点；admin 自定义 inhibition（按 scope/rule/label 组合）预留接口但未实现，将来扩展时不应破坏 `Inhibitor` 接口
- **不抑制 incident 写入**：抑制只作用于通知步骤；incident 行、firing 事件仍正常写入，时间线完整
- **抑制事件仍写入时间线**：`MaybeNotify` 检测到抑制时会写一条 `EventTypeInhibited` 事件（带 reason），操作员能看到"为什么没收到通知"
- **edge_offline 规则的 dedupe key 形式**：`host:<device_id>:edge_offline`，与 `usecase.go::buildDedupeKey` 对 host scope 的形式一致；如果将来 buildDedupeKey 改格式，这里要同步
- **scrape_down 的 dedupe key 前缀**：依赖 `pipeline:scrape_down:` 前缀；如果 pipeline evaluator 将来改 dedupe key 形式，前缀匹配要同步
