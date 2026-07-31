# `alert_rule_adapter.go` 技术实现文档

> 源文件：`internal/manager/service/aiopsconfig/alert_rule_adapter.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/service/aiopsconfig`

## 1. 概述

本文件是 AIOps 工具链与 alert service 之间的防腐层适配器。`aiopstools.ConfigManager`（工具层契约）期望调用 `alertconfig.RuleInput` / `alertconfig.PreviewResult`；而实际业务实现位于 `manager/service/alert`（其 DTO 是 `managersvcalert.RuleInput` 等）。本文件通过定义本地接口 + 字段级翻译，让 aiopstools 不直接 import alert service，反向亦然，避免循环依赖。红线：仅做形状翻译，不做业务校验；nil alertSvc 时返回一个 nil-internal 的 manager（下游用 `ErrNotWiredYet` 兜底）。

## 2. 包信息

- **包名**：`aiopsconfig`
- **所属模块**：`internal/manager/service/aiopsconfig`
- **依赖方向**：被 aiops tool 层（`biz/aiops/tools`）调用；自身依赖 `biz/aiops/alertconfig`、`biz/aiops/tools`、`manager/service/alert`（仅类型引用）

## 3. 关键类型与接口

```go
type alertRuleService interface {
    PreviewRule(ctx, caller managersvcalert.Caller, in managersvcalert.RuleInput, lookbackSeconds int) (*managersvcalert.PreviewResult, error)
    CreateRule(ctx, caller managersvcalert.Caller, in managersvcalert.RuleInput) (*managersvcalert.Rule, error)
}

type alertRulePort struct {
    alert alertRuleService
}
```

`alertRuleService` 是 service-private 接口，让 `*managersvcalert.Service`（通过 PreviewRule / CreateRule 方法）以结构化方式满足，无需 import 形成环。

## 4. 关键函数与流程

### `NewAlertRuleManager(alertSvc alertRuleService) aiopstools.ConfigManager`

- **职责**：构造适配器，返回 `aiopstools.ConfigManager` 接口实例。
- **流程**：
  1. `alertSvc == nil` → 返回 `alertconfig.NewAlertRuleManager(nil)`（下游 manager 自身处理 nil 为 `ErrNotWiredYet`）。
  2. 否则返回 `alertconfig.NewAlertRuleManager(alertRulePort{alert: alertSvc})`。
- **错误处理**：无错误返回。

### `alertRulePort.PreviewRule(ctx, caller, in alertconfig.RuleInput, lookbackSeconds)`

- **流程**：
  1. `toAlertServiceCaller(caller)` 把 `aiopstools.ConfigCaller` 翻译为 `managersvcalert.Caller`（仅复制 UserID + Role）。
  2. `toAlertServiceRuleInput(in)` 翻译 RuleInput（含逐条 RuleCondition 拷贝）。
  3. 调 `p.alert.PreviewRule`；err 直接透传（不 wrap，让上层诊断）。
  4. `fromAlertServicePreview(res)` 翻译回 `alertconfig.PreviewResult`。

### `alertRulePort.CreateRule(ctx, caller, in alertconfig.RuleInput)`

- 同 PreviewRule 流程，但返回 `&alertconfig.Rule{ID, Kind, Name}` —— 仅复制三个字段，丢弃其他（如 Conditions / Spec），适配 aiopstools 仅需要 ID + 形状元数据。

### 翻译函数

- **`toAlertServiceCaller(c)`**：`managersvcalert.Caller{UserID, Role}`。
- **`toAlertServiceRuleInput(in)`**：逐字段拷贝 RuleKey / Kind / Name / ScopeType / JoinMode / Severity / Enabled / Spec / Labels / RunbookURL / NotifyChannelIDs / NotifyWindowSeconds / NotifyMinFires；Conditions 用 `make(..., 0, len)` 预分配后 append。
- **`fromAlertServicePreview(in)`**：nil 安全；拷贝 FireCount / FirstFireAt / LastFireAt / Threshold / Unit / SkippedReason；Samples 与 Series 各自 `make + append`，逐字段拷贝。

## 5. 依赖关系

- **内部包**：`biz/aiops/alertconfig`（被构造的 manager 类型）、`biz/aiops/tools`（ConfigManager 接口 + ConfigCaller）、`manager/service/alert`（DTO 类型引用，无运行时调用）
- **外部库**：仅 `context`
- **被调用方**：aiops 工具层（`biz/aiops/tools`）通过 `ConfigManager` 接口调用

## 6. 并发与资源管理

- **无状态**：`alertRulePort` 是值类型，仅持 `alert` 接口引用；无锁、无 channel、无缓存。
- **ctx 透传**：所有方法首参 `context.Context`。

## 7. 设计模式与亮点

- **防腐层适配器**：`aiopstools` 与 `managersvcalert` 双方都不直接 import 对方；本文件提供形状翻译，避免循环依赖与跨层耦合。
- **service-private 接口**：`alertRuleService` 仅本文件需要，定义在 consumer 侧符合 gospec"接口在消费方定义"规范。
- **nil 优雅降级**：`alertSvc == nil` 时返回 nil-internal manager，下游统一 `ErrNotWiredYet`。
- **CreateRule 仅回 ID/Kind/Name**：aiopstools 不需要完整 Rule，最小化契约表面积。
- **err 不 wrap**：PreviewRule / CreateRule 直接透传 err —— 上层已能从 errs.ErrInvalid 等识别类型，wrap 一层反而稀释。

## 8. 注意事项

- **不做业务校验**：所有 validation 在 `managersvcalert.Service` 内部完成（如 `validateRuleInput` / `validatePreviewInput`），本层只翻译。
- **RuleCondition 字段一对一**：Metric / Operator / Threshold / Window / For / Aggregator；新增字段需同步两端 DTO + 翻译函数。
- **PreviewResult 字段一对一**：FireCount / FirstFireAt / LastFireAt / Threshold / Unit / SkippedReason / Samples / Series；Samples 与 Series 各自有子结构体翻译。
- **`alertRulePort` 是值类型**：方法 receiver 为值，无需指针；alert 接口引用本身不可变。
- **依赖方向严格**：本包是 service 之间的桥，不允许反向 import；aiops tool 层只看到 `aiopstools.ConfigManager`。
