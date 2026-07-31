# `query_alert_rules_basetool.go` 技术实现文档

> 源文件：`internal/manager\biz\aiops\tools\query_alert_rules_basetool.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件是 `query_alert_rules` 工具的 **BaseTool 形态**，镜像 `query_alert_rules.go::executeQueryAlertRules` 闭包路径。两者逻辑字节级一致：list alert rules，按 `kind` / `enabled` / `name_contains` 过滤，`limit` 默认 100 cap 500，10s 超时。`Class="read"`。`WhenToUse` 反 guard 区分 `query_incidents`（incidents vs rules 是不同对象）/ 单个 rule 的 firing history（用 `query_incidents` with rule_key）/ 编辑 rules（read-only）。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 Registry BaseTool 路径注册调用；依赖 `basetool`、`AlertUsecase`、`alertmodel.IsKnownKind` / `NormalizeKind`。与闭包路径 `query_alert_rules.go` 并存。

## 3. 关键类型与接口

```go
type QueryAlertRulesTool struct {
    alertUC AlertUsecase
    log     *slog.Logger
}
```

复用 `query_alert_rules.go` 定义的共享类型：`QueryAlertRulesArgs`、`AlertRuleRow`、`QueryAlertRulesSchema`、`ToolNameQueryAlertRules`、`QueryAlertRulesDescription`、`queryAlertRulesCallTimeout`。

## 4. 关键函数与流程

```go
func NewQueryAlertRulesTool(alertUC AlertUsecase, log *slog.Logger) *QueryAlertRulesTool
func (t *QueryAlertRulesTool) Info(_ context.Context) (*basetool.ToolInfo, error)
func (t *QueryAlertRulesTool) InvokableRun(ctx, argsJSON, _ ...InvokeOption) (string, error)
```

`InvokableRun` 流程（与 `Registry.executeQueryAlertRules` 字节级一致）：
1. 守门 `alertUC != nil`。
2. Unmarshal → `QueryAlertRulesArgs`。
3. `Limit` 默认 100，cap 500。
4. `Kind` 非空时 `IsKnownKind(Kind)` 校验，无效报错。
5. `context.WithTimeout(ctx, queryAlertRulesCallTimeout=10s)`。
6. `alertUC.ListRules(callCtx, "")` 拉全量。
7. Go 端过滤（Kind 归一化 / Enabled 指针 / NameContains 双字段）。
8. 转 `[]AlertRuleRow`，`len(rows) >= Limit` break。
9. Marshal `{"rules": rows, "count": len(rows)}` 返回字符串。

## 5. 依赖关系

- **basetool**：`ToolInfo`（`Class="read"`）、`InvokeOption`（忽略）。
- **AlertUsecase**：`ListRules(ctx, scopeType string)`。
- **alertmodel**：`Rule` / `IsKnownKind` / `NormalizeKind`。
- 不依赖 devicebiz / edgebiz / prom / log / trace / tunnel。

## 6. 并发与资源管理

- **无锁、无共享可变状态**：`QueryAlertRulesTool` 仅持有不变 `alertUC` 指针，多 goroutine 可并发调用。
- **无 goroutine**：单次 `ListRules` + Go 端过滤，10s 超时覆盖。
- **`Limit` 截断**：Go 端过滤后 break。

## 7. 设计模式与亮点

- **闭包/BaseTool 双轨镜像**：本文件是 BaseTool 形态，`query_alert_rules.go` 是闭包形态。两套实现字节级一致，符合 ongrid closure path / basetool path 共存约定。未来一行 type alias 即可切换。
- **`WhenToUse` 反 guard 丰富**：明确 "NOT for incidents themselves (use query_incidents). NOT for an individual rule's history of firings (use query_incidents with rule_key). NOT for editing rules (read-only)."，引导 LLM 区分 rules vs incidents。
- **复用闭包路径常量**：`queryAlertRulesCallTimeout` / `ToolNameQueryAlertRules` / `QueryAlertRulesDescription` / `QueryAlertRulesSchema` / `QueryAlertRulesArgs` / `AlertRuleRow` 都来自 `query_alert_rules.go`，确保两路径 metadata 一致。
- **`Enabled *bool` 指针语义**：与闭包路径一致，区分 "未传" 与 "显式 false"。
- **`NameContains` 双字段匹配**：与闭包路径一致，同时匹配 name 与 rule_key。
- **`Kind` 校验 + 归一化**：与闭包路径一致。

## 8. 注意事项

- **与闭包路径行为一致**：任何过滤逻辑 / Limit / Kind 校验修改需同步两处（`query_alert_rules.go` + 本文件）。未来应抽 `singleQueryAlertRules(ctx, alertUC, in) (string, error)` 让两路径都代理调用，避免 drift。
- **`ListRules("")` 拉全量**：与闭包路径一致，rule 数巨大时 Go 端过滤可能慢。
- **`InvokeOption` 被忽略**：BaseTool 路径下 `opts` 不影响行为。
- **无 `ExecuteResult.DeviceID` 回传**：与闭包路径不同，BaseTool 返回纯字符串。本工具与 device 无关。
- **read-only**：`WhenToUse` 明示 "NOT for editing rules (read-only)"。编辑走 `draft_config_change` + `apply_config_change`。
- **无 tenant 过滤**：依赖 `AlertUsecase` 内部按 ctx tenant 过滤。
- **`count` 字段**：返回实际 rows 数（非总匹配数）。若 `Limit` 截断，LLM 无法知道总匹配数。
