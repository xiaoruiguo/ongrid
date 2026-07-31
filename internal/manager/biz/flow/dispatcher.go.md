# `dispatcher.go` 技术实现文档

> 源文件：`internal/manager/biz/flow/dispatcher.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/flow`

## 1. 概述

本文件实现 `trigger.alert_fired` driver。当 alert 触发新 incident 时，biz/alert 通过窄 seam 调 `OnAlertFired`；dispatcher fan-out 到每个 enabled flow 的匹配 `trigger.alert_fired` 节点，启动 run，incident 上下文作为 `{{trigger.*}}`。**非阻塞** —— firing 路径不能等 flow 执行；scan + trigger 在 detached goroutine。签名用 plain types 让 `flow.Dispatcher` 隐式满足 `biz/alert.WorkflowDispatcher`（无 flow→alert import，无 main.go adapter）。

## 2. 包信息

- **包名**：`flow`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 biz/alert（通过 WorkflowDispatcher interface）调用；依赖 `encoding/json`、`log/slog`、`strings`、`time`

## 3. 关键类型与接口

```go
type Dispatcher struct {
    uc  *Usecase
    log *slog.Logger
}

type alertTriggerConfig struct {
    Rule        string `json:"rule"`         // 可选：rule 名 case-insensitive 子串
    MinSeverity string `json:"min_severity"` // 可选：warning/error/critical
}

var severityRank = map[string]int{"info": 0, "warning": 1, "error": 2, "critical": 3}
```

## 4. 关键函数与流程

### `NewDispatcher`
- **签名**：`func NewDispatcher(uc *Usecase, log *slog.Logger) *Dispatcher`
- **职责**：构造 dispatcher；log nil → Default
- **流程**：返回 `&Dispatcher{uc, log}`

### `OnAlertFired`
- **签名**：`func (d *Dispatcher) OnAlertFired(incidentID uint64, rule, severity string, edgeID, deviceID uint64, labels map[string]string, firedAt time.Time)`
- **职责**：非阻塞 fire-and-forget；签名匹配 `biz/alert.WorkflowDispatcher`
- **流程**：
  1. d nil 或 uc nil → return
  2. `go d.dispatch(...)` detached goroutine
- **关键设计**：注释明示"firing path can't wait on flow execution"

### `dispatch`
- **签名**：`func (d *Dispatcher) dispatch(incidentID, rule, severity, edgeID, deviceID, labels, firedAt)`
- **职责**：scan enabled flows + trigger 匹配的 alert_fired 节点
- **流程**：
  1. `ctx := context.Background()`
  2. `uc.ListEnabledFlows(ctx)`
  3. 构造 payload map（incident_id/rule/severity/edge_id/device_id/labels/fired_at RFC3339）
  4. 遍历 flows：
     - `ParseGraph(f.GraphJSON)`；失败 continue
     - 遍历 `g.Triggers()`：仅 `NodeTriggerAlert` 类型
     - `alertMatches(t.Config, rule, severity)`；不匹配 continue
     - `uc.TriggerEvent(ctx, f.ID, NodeTriggerAlert, payload)`；失败 Warn，成功 Info
     - **break** —— 一个 flow 每 alert 只触发一次 run
- **错误处理**：ListEnabledFlows 失败 Warn + return；ParseGraph 失败 continue；TriggerEvent 失败 Warn

### `alertMatches`
- **签名**：`func alertMatches(cfgRaw json.RawMessage, rule, severity string) bool`
- **职责**：trigger config 与 incident 匹配
- **流程**：
  1. unmarshal cfgRaw（len>0 时；忽略 err）
  2. cfg.Rule 非空 → rule 必须 case-insensitive 包含
  3. cfg.MinSeverity 非空 → severity rank >= min
- **关键设计**：Rule 子串匹配（操作员写"disk"匹配"disk_high"/"disk_full"）；MinSeverity 是 floor

## 5. 依赖关系

- **外部库**：`encoding/json`、`log/slog`、`strings`、`time`、`context`
- **被调用方**：biz/alert 通过 `WorkflowDispatcher` interface 调用 OnAlertFired
- **协作**：`Usecase.ListEnabledFlows` + `Usecase.TriggerEvent`

## 6. 并发与资源管理

- **detached goroutine**：OnAlertFired 启动 goroutine 后立即返回；fire-and-forget
- **`context.Background()`**：detached goroutine 不能用调用方 ctx（可能已取消）
- **无共享状态**：Dispatcher 仅持有 uc + log
- **无锁**：scan 读 flows snapshot

## 7. 设计模式与亮点

- **非阻塞 fire-and-forget**：firing 路径不等 flow 执行；goroutine + Background ctx
- **plain types 签名**：OnAlertFired 用 uint64/string/map 等基础类型；`flow.Dispatcher` 隐式满足 `biz/alert.WorkflowDispatcher`，无需 main.go adapter
- **一个 flow 每 alert 一次 run**：break 内层 trigger 循环；防同一 flow 多 trigger 节点重复触发
- **alertMatches 子串 + severity floor**：Rule 子串匹配操作员写"disk"覆盖多个 disk 规则；MinSeverity 是 floor 不是精确
- **severityRank 简单 map**：info<warning<error<critical；未知 severity rank=0（最低）
- **ParseGraph 失败 continue**：坏 graph 不阻塞其他 flow

## 8. 注意事项

- **非阻塞**：调用方（alert biz）不等 flow 执行；goroutine + Background ctx
- **一个 flow 每 alert 一次 run**：即使有多个匹配 trigger 节点也只触发一次
- **Rule 子串匹配**：操作员写"disk"匹配"disk_high"/"disk_full"等
- **MinSeverity floor**：未知 severity（如 "info"）rank=0；低于任何 min_severity
- **payload fired_at UTC RFC3339**：下游 `{{trigger.fired_at}}` 是 UTC ISO 字符串
- **ParseGraph 失败 silent skip**：坏 graph 不 Warn（操作员应在保存时校验）
- **detached goroutine 无 panic recover**：TriggerEvent 内部有 recover；dispatcher 本身不 recover
