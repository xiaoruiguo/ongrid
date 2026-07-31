# registry_basetool.go

## 1. 概述

本文件实现 `BuildBaseTools()`——BaseTool 路径的核心构造器。把每个 BaseTool 实现 paired with dependencies，wrap 成 `*ToolBag`（deferred-loading layer）。

**两套实现并存**：与 `registry.go` 的 `NewRegistry`（闭包路径）并行运行。nil-gating mirrors `NewRegistry` exactly——每个工具的注册条件与闭包路径一致。

**ToolSearch 无条件注册**：通过 `bag.WithExtra`。deferral off 时是 harmless no-op；deferral on 时是 LLM 拿 redacted schema 的 load-bearing entry。

## 2. 包信息

- **包名**：`tools`
- **路径**：`internal/manager/biz/aiops/tools/registry_basetool.go`
- **导入**：
  - `basetool`
  - `os` / `strconv`（用于 `envIntDefault`）
- **Class**：N/A（这是构造器，不是工具）

## 3. 关键类型与接口

### `envIntDefault(name, def) int`

本地 helper，解析 env-var-string 为 int。注释明示"deliberately keep this helper local——the only call site is BuildBaseTools' threshold knob and there's no value in pulling in a generic config layer just for one int"。

### `defaultDeferralThreshold = 30`

default toolBag size above which deferral kicks in。注释解释：30 chosen because May 2026 roster（14 core + 4 alert/host-files + 1 mutating ≈ 19）sits well below it；threshold only trips once one or two marketplace packs land。Operators can override via `ONGRID_TOOLBAG_DEFERRAL_THRESHOLD`。

## 4. 关键函数与流程

### `BuildBaseTools() *ToolBag`

构造所有 BaseTool，按 PR-7 文档顺序：

1. **host_load + process_list**（always）：`NewGetHostLoadTool` / `NewGetProcessListTool`，传 `caller + edges + devices`（post N+15 batch refactor，schemas take `device_ids[]`）
2. **query_promql + list_metric_catalog**（`promQuery != nil`）
3. **list_database_sources**（`edges + pluginConfigs`）
4. **analyze_database_status**（`promQuery + edges + pluginConfigs`）
5. **query_logql**（`logQuery != nil`）
6. **query_traceql**（`traceQuery != nil`）
7. **query_knowledge**（`knowledge != nil`）
8. **code-aware analysis**（`knowledge.(CodeBrowser)` type-assert）：`list_repo_sources` / `read_source` / `grep_source`
9. **Kubernetes**（`k8sSnapshot != nil`）：`query_k8s_snapshot` always，`describe_k8s_resource` / `query_k8s_logs` / `execute_k8s_action` 需 `caller != nil`
10. **query_devices**（`edges || devices`）
11. **get_topology**（`edges`，传 `alertUC` best-effort + `topology`）
12. **rank_edges + find_outlier_edges**（`promQuery + edges`）
13. **query_incidents + get_incident_detail + query_alert_rules**（`alertUC`）
14. **query_change_events**（`auditLister`）
15. **get_edge_summary**（`edges`）
16. **correlate_incident**（`alertUC + prom + log + trace ALL set`，四源 rule）
17. **draft_config_change + apply_config_change**（`configManager`，apply 是 Class="write"）
18. **AgentTool + SendMessage + TaskStop**（`spawner`，coordinator-only）
19. **restart_service**（`caller + edges + devices`，第一个 MUTATING BaseTool，Class="write"）
20. **expand_topology + find_topology_node**（`topologyGraph`）
21. **bash**（`caller + edges + devices`，Class="read"，mutating 走 hostBashProposer）
22. **cloud_bash**（`cloudBashProposer`）
23. **send_im_message**（`imSender`）
24. **serve_page**（`pageStore`）

最后：

```go
threshold := envIntDefault("ONGRID_TOOLBAG_DEFERRAL_THRESHOLD", defaultDeferralThreshold)
bag := NewToolBag(out, threshold)
bag = bag.WithExtra(NewToolSearchTool(bag, r.log))
return bag
```

ToolSearch 无条件注册（`WithExtra`），deferral off 时 harmless，deferral on 时 load-bearing。

## 5. 依赖关系

| 方向 | 依赖 | 说明 |
|------|------|------|
| 上游 | `Registry`（持有所有依赖） | `r.caller` / `r.edges` / `r.devices` / ... |
| 下游 | 各 `NewXxxTool` 构造器 | 创建 BaseTool 实例 |
| 下游 | `NewToolBag` + `WithExtra` | 包装成 deferred-loading bag |
| 共享 | `Registry` 字段（与闭包路径共享） | nil-gating 一致 |

## 6. 并发与资源管理

- 构造期单线程，无锁。
- `ToolBag` 自身有 deferred-loading 机制（见 `toolbag.go`），运行期 thread-safe。
- 各 BaseTool 内部用 `context.WithTimeout` 独立超时。

## 7. 设计模式与亮点

- **nil-gating mirrors NewRegistry**：注释明示每个工具的注册条件与闭包路径一致——这是有意对齐，便于未来一行 type alias 切换时验证等价。
- **PR-7 顺序**：注释明示"Order of the slice follows the documented PR-7 list (host_load first, correlate_incident last)"——稳定顺序便于 `len()` check 或 stable index 反映 spec。
- **`envIntDefault` 本地化**：不引入 generic config layer，单一 call site 不值得——这种"local helper over framework"的取舍体现 ongrid 的 anti-over-engineering。
- **`defaultDeferralThreshold = 30`**：注释解释 30 的来由（May 2026 roster 19 个工具，30 留余量）——可调阈值让 operator 按 roster size 调整。
- **ToolSearch 无条件注册**：deferral off 时是 no-op，deferral on 时是 load-bearing——单一构造路径覆盖两种模式，避免分支。
- **`CodeBrowser` type-assert**：`knowledge.(CodeBrowser)` 让 test fakes（search-only）leave code tools unregistered——type assertion 作为 capability 检测，比 separate constructor 优雅。
- **`WithExtra` 注册 ToolSearch**：ToolSearch 不在主 `out` slice 里，而是通过 `WithExtra` 后置添加——因为它需要 `bag` 引用自身（self-reference），构造顺序决定必须后置。

## 8. 注意事项

- **顺序敏感性**：注释明示顺序遵循 PR-7 list，但实际顺序对 LLM schema list 可能有影响（LLM 对靠前 schema 更敏感）——改顺序要谨慎。
- **`defaultDeferralThreshold = 30` 是软约束**：roster 超过 30 时 deferral 自动开启，operator 可能不知道——建议在启动 log 里打印实际 threshold。
- **`CodeBrowser` type-assert 静默失败**：test fakes 不实现 CodeBrowser 时 code tools 不注册，无 warning——可能让测试者困惑。
- **`NewToolBag` + `WithExtra` 顺序**：ToolSearch 必须在 bag 构造后注册（self-reference），顺序不能换。
- **闭包路径与 BaseTool 路径并存**：`NewRegistry`（闭包）和 `BuildBaseTools`（BaseTool）都注册工具，drift 风险——任何一边改注册条件必须同步另一边。
- **`correlate_incident` 四源 rule 继承**：与 `NewRegistry` 同样的 `alertUC + prom + log + trace ALL set` 约束——保证两路径行为一致。
- **未列出所有工具的注释**：构造器注释列了主要工具的 gating，但实际 `out = append(out, ...)` 有 20+ 个工具——注释与代码的同步要小心。
