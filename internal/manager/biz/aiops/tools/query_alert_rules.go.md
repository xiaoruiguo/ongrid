# `query_alert_rules.go` 技术实现文档

> 源文件：`internal/manager\biz\aiops\tools\query_alert_rules.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件实现 `query_alert_rules` 工具（闭包路径，挂在 `Registry.executeQueryAlertRules`）：list ongrid alert rules，按 `kind` / `enabled` / `name_contains`（substring against name OR rule_key）过滤。返回 `[]AlertRuleRow{id, rule_key, kind, name, scope_type, severity, enabled, source_type, updated_at}`。`limit` 默认 100 cap 500。10s 超时。当问题聚焦 "哪些规则存在" / "这条规则谁在用" / "show all metric_threshold rules" 时使用。**注意**：`AlertUsecase.ListRules` 仅支持 `scope_type` 过滤，本工具拉全量后 Go 端过滤——注释明示 "ListRules's only filter is scope_type, which we don't use here"。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 Registry 闭包路径调用；依赖 `AlertUsecase`（`ListRules`）、`alertmodel.IsKnownKind` / `NormalizeKind`。与 `query_alert_rules_basetool.go`（BaseTool 镜像）并存。

## 3. 关键类型与接口

```go
type QueryAlertRulesArgs struct {
    Kind         string `json:"kind,omitempty"`          // metric_threshold|metric_anomaly|...|trace_error_rate
    Enabled      *bool  `json:"enabled,omitempty"`        // 指针区分 "未传" 与 "显式 false"
    NameContains string `json:"name_contains,omitempty"`  // substring against name OR rule_key
    Limit        int    `json:"limit,omitempty"`          // 默认 100，cap 500
}

type AlertRuleRow struct {
    ID         uint64    `json:"id"`
    RuleKey    string    `json:"rule_key"`
    Kind       string    `json:"kind"`
    Name       string    `json:"name"`
    ScopeType  string    `json:"scope_type"`
    Severity   string    `json:"severity"`
    Enabled    bool      `json:"enabled"`
    SourceType string    `json:"source_type"`
    UpdatedAt  time.Time `json:"updated_at"`
}

const queryAlertRulesCallTimeout = 10 * time.Second
```

依赖 `AlertUsecase` 接口（`correlate_incident.go` 定义）：`ListRules(ctx, scopeType string) ([]*alertmodel.Rule, error)`。

## 4. 关键函数与流程

```go
func (r *Registry) executeQueryAlertRules(ctx, args json.RawMessage) (ExecuteResult, error)
```

流程：
1. 守门 `r.alertUC != nil`。
2. Unmarshal → `QueryAlertRulesArgs`。
3. `Limit` 默认 100，cap 500。
4. `Kind` 非空时 `alertmodel.IsKnownKind(Kind)` 校验，无效报错 "invalid kind %q"。
5. `context.WithTimeout(ctx, queryAlertRulesCallTimeout=10s)`。
6. `r.alertUC.ListRules(callCtx, "")`（空 scopeType 拉所有）→ `all []*alertmodel.Rule`。
7. Go 端过滤：
   - `Kind` 非空：`alertmodel.NormalizeKind(rule.Kind) != wantKind` 跳过（NormalizeKind 归一化，如 `metric_threshold` vs `metricthreshold`）。
   - `Enabled != nil`：`rule.Enabled != *Enabled` 跳过。
   - `NameContains` 非空：`!strings.Contains(rule.Name, NameContains) && !strings.Contains(rule.RuleKey, NameContains)` 跳过。
8. 转 `[]AlertRuleRow`，`len(rows) >= Limit` break。
9. Marshal `{"rules": rows, "count": len(rows)}` 返回 `ExecuteResult{ResultJSON: out}`。

## 5. 依赖关系

- **AlertUsecase**：`ListRules(ctx, scopeType string)`。接口在 `correlate_incident.go` 定义。
- **alertmodel**：`Rule`（ID/RuleKey/Kind/Name/ScopeType/Severity/Enabled/SourceType/UpdatedAt）、`IsKnownKind(kind) bool`、`NormalizeKind(kind) string`。
- 不依赖 devicebiz / edgebiz / prom / log / trace / tunnel。

## 6. 并发与资源管理

- **无锁、无共享可变状态**：`Registry` 字段不变，`executeQueryAlertRules` 内变量局部。多 goroutine 可并发调用。
- **无 goroutine**：单次 `ListRules` + Go 端过滤，10s 超时覆盖。
- **`Limit` 截断**：Go 端过滤后 `len(rows) >= Limit` break，避免返回过多。

## 7. 设计模式与亮点

- **`Enabled *bool` 指针语义**：区分 "未传"（不过滤）与 "显式 false"（仅禁用规则）。避免零值歧义。
- **`NameContains` 双字段匹配**：同时匹配 `rule.Name` 与 `rule.RuleKey`，让 LLM 用任一标识都能找到。
- **`Kind` 校验 + 归一化**：`IsKnownKind` 校验枚举值，`NormalizeKind` 归一化比较（防 `metric_threshold` vs `metricthreshold` 大小写/下划线差异）。
- **Go 端过滤**：`ListRules` 仅支持 scope_type 过滤，本工具拉全量后 Go 端过滤。注释明示这是 `ListRules` 接口限制。若 rule 数量大（>1000）可能拉全量慢，当前规模可接受。
- **`Limit` 默认 100 cap 500**：防 LLM 传过大值拖垮响应。schema `maximum: 500` 约束 LLM。
- **`AlertRuleRow` trimmed envelope**：只保留 LLM 推理常用字段，省 prompt token。完整 rule 行（含 ConditionsJSON 等）可另查。
- **10s 超时**：纯 DB 查询，应秒级返回。

## 8. 注意事项

- **`ListRules("")` 拉全量**：scopeType 空拉所有 scope 的规则。若 tenant rule 数巨大（>1000），Go 端过滤可能慢。当前未分页，依赖 `Limit` 截断。
- **Go 端过滤非最优**：DB 端应支持 kind/enabled/name 过滤，但 `ListRules` 接口仅 scope_type。未来扩展 `ListRules(filter)` 可让 DB 端过滤，省带宽。
- **`Kind` 校验严格**：`IsKnownKind` 不通过直接报错。LLM 需传合法 kind（schema description 列出 9 种：metric_threshold / metric_anomaly / metric_forecast / metric_burn_rate / metric_raw / log_match / log_volume / trace_latency / trace_error_rate）。
- **闭包路径独有**：本文件是 `Registry.executeQueryAlertRules`。BaseTool 形态在 `query_alert_rules_basetool.go`，两者逻辑字节级一致。
- **无 tenant 过滤**：依赖 `AlertUsecase` 内部按 ctx tenant 过滤。`tenant_bind` 装饰器注入 tenant_id 到 ctx。
- **`count` 字段**：返回实际 rows 数（非总匹配数）。若 `Limit` 截断，LLM 无法知道总匹配数。与 `find_topology_node` 的 `Total` vs `Returned` 设计不同。
- **`SourceType` 字段**：rule 来源类型（如 `ui` / `api` / `import`），让 LLM 知道 rule 是怎么创建的。
- **read-only**：`WhenToUse` 明示 "NOT for editing rules (read-only)"。编辑走 `draft_config_change` + `apply_config_change`。
