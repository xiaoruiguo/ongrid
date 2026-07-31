# registry.go

## 1. 概述

本文件是 `tools` 包的核心——`Registry` 持有所有闭包路径工具的依赖 + 注册表。包注释明示：每个 tool 是 (name, description, JSON-schema, executor) 四元组；registry 暴露 `Schemas()` 给 `llm.Chat`，`Invoke(name, args)` 给 agent loop dispatch。

**架构定位**：reverse calls 通过 `Caller`（具体是 `frontierbound.Client` wrapping `github.com/singchia/frontier`），所以本包不依赖任何 geminio/SDK-level 类型。Cross-subdomain import of `manager/biz/edge` 是 explicit and deliberate（same-BC subdomains may import each other）。

**两套实现并存**：闭包路径（本文件 + 各 `xxx.go`）与 BaseTool 路径（`registry_basetool.go` + 各 `xxx_basetool.go`）并行运行，未来一行 type alias 切换。

## 2. 包信息

- **包名**：`tools`（package tools）
- **路径**：`internal/manager/biz/aiops/tools/registry.go`
- **导入**：
  - `devicebiz` / `edgebiz` / `topologybiz`——业务 usecase
  - `errs`（`internal/pkg/errs`）—— `ErrNotFound`
  - `llm`（`internal/pkg/llm`）—— `ToolSchema`
- **包注释**：详细说明 registry 角色 + frontierbound 解耦设计

## 3. 关键类型与接口

### `Caller`（接口）

```go
type Caller interface {
    Call(ctx context.Context, edgeID uint64, method string, body []byte) ([]byte, error)
}
```

frontierbound SDK wrapper 的窄 seam，本地声明让测试 inject fake 而不启动真实 Client。

### `ExecuteResult`

```go
type ExecuteResult struct {
    ResultJSON json.RawMessage
    DeviceID   *uint64  // 用于 audit 列 chat_tool_calls.device_id
}
```

Post-split（May 2026）：`EdgeID → DeviceID` 重命名。数值相同，legacy `chat_tool_calls.edge_id` 列保留为 storage column name（audit-only），语义是 host device id。

### `Tool`

```go
type Tool struct {
    Name        string
    Description string
    Schema      json.RawMessage
    Execute     func(ctx context.Context, args json.RawMessage) (ExecuteResult, error)
}
```

闭包路径工具的四元组。

### `Registry`

```go
type Registry struct {
    caller     Caller
    edges      *edgebiz.Usecase
    devices    *devicebiz.Usecase
    alertUC    AlertUsecase
    promQuery  PromQuerier
    logQuery   LogQuerier
    traceQuery TraceQuerier
    knowledge  KnowledgeSearcher
    topology   TopologyInfo  // 部署级事实（manager version、backend URL、channel-count callback）
    topologyGraph *topologybiz.Usecase  // 业务拓扑（expand_topology / find_topology_node 用）
    spawner    WorkerSpawner  // AgentTool / SendMessage / TaskStop 用
    subagentRegistry SubagentRegistry
    auditLister AuditLister  // query_change_events
    pluginConfigs PluginConfigLister  // database metrics source discovery
    configManager ConfigManager  // conversational config draft/apply
    cloudBashProposer CloudBashProposer
    hostBashProposer HostBashProposer
    imSender IMSender
    pageStore PageStore
    k8sSnapshot K8sSnapshotReader
    log   *slog.Logger
    tools map[string]Tool
}
```

依赖众多但分组清晰：核心 caller/edges/devices、signal query clients（prom/log/trace）、alertUC、knowledge、topology（部署级 + 业务级）、coordinator-only（spawner）、各种 post-construction setter 字段。

## 4. 关键函数与流程

### `NewRegistry(caller, edges, devices, promQuery, logQuery, traceQuery, alertUC, log)`

构造 + 自动注册 MVP 工具：

- **always**：`get_host_load` / `get_process_list`
- **promQuery != nil**：`query_promql` / `list_metric_catalog`
- **logQuery != nil**：`query_logql`
- **traceQuery != nil**：`query_traceql`
- **edges != nil**：`query_devices` / `get_topology`
- **edges != nil && promQuery != nil**：`rank_edges` / `find_outlier_edges`
- **alertUC != nil**：`query_incidents` / `get_incident_detail` / `query_alert_rules`
- **edges != nil**：`get_edge_summary`
- **alertUC + prom + log + trace ALL set**：`correlate_incident`（四源 rule，防半空 bundle 误导 LLM）

注释明示 nil-gating 让单元测试传 nil 时不注册对应工具。

### Post-construction Setters

- `SetCloudBashProposer(p)` / `SetHostBashProposer(p)` / `SetIMSender(s)` / `SetPageStore(p)`——wires 对应工具
- `SetK8sSnapshotReader(k)`——注册 `query_k8s_snapshot`，且若 `caller != nil` 同时注册 `describe_k8s_resource` / `query_k8s_logs`
- `SetAuditLister(a)`——wires `query_change_events`（HLD-013 Phase 2）
- `SetPluginConfigLister(p)`——注册 `list_database_sources`，且若 `promQuery != nil` 同时注册 `analyze_database_status`
- `SetConfigManager(m)`——wires conversational config tools
- `SetTopologyInfo(info)`——填 `get_topology` 部署级事实
- `SetWorkerSpawner(s, registry)`——wires AgentTool / SendMessage / TaskStop
- `SetKnowledgeSearcher(k)`——wires `query_knowledge`
- `SetTopologyGraph(t)`——wires `expand_topology` / `find_topology_node`

注释明示这些 setter 是为了避免 `NewRegistry` signature 膨胀——cmd/main.go 在构造各 service 后调用，避开循环 wiring order。

### `Register(t Tool)`

`Name == "" || Execute == nil` 静默跳过；否则 `r.tools[t.Name] = t`（re-registration 静默 overwrite，caller 负责 uniqueness）。

### `Schemas() []llm.ToolSchema`

map 遍历转 `[]llm.ToolSchema`，顺序 unspecified（map iteration）。

### `Invoke(ctx, name, args) (ExecuteResult, error)`

- `name` 不在 map → `errs.ErrNotFound` wrapped error
- `len(args) == 0` → 替换为 `{}`（匹配 OpenAI tool_call 零参数 shape）
- 调 `t.Execute(ctx, args)`

## 5. 依赖关系

| 方向 | 依赖 | 说明 |
|------|------|------|
| 上游 | cmd/main.go | 构造 + 调 setters |
| 下游 | `Caller`（frontierbound） | tunnel dispatch |
| 下游 | `*edgebiz.Usecase` / `*devicebiz.Usecase` / `*topologybiz.Usecase` | 业务查询 |
| 下游 | `PromQuerier` / `LogQuerier` / `TraceQuerier` | signal query |
| 下游 | `AlertUsecase` / `KnowledgeSearcher` / `K8sSnapshotReader` 等 | 各工具数据源 |
| 共享 | 各 `xxx.go` 的 `executeXxx` 闭包 | 注册时引用 |

## 6. 并发与资源管理

- `Registry` 持有 `tools map[string]Tool`，**未加锁**——注释明示注册阶段是 boot 时单线程，运行期只读，所以无需锁。
- 每个 `executeXxx` 内部用 `context.WithTimeout` 独立超时。
- 无共享可变状态（除 boot 期 `Register`）。

## 7. 设计模式与亮点

- **nil-gating 注册**：每个工具注册前检查依赖，nil 时不注册——单元测试传 nil 依赖不会触发 nil panic，部署时缺失某 signal client 也能 graceful degradation。
- **四源 rule for correlate_incident**：`alertUC + prom + log + trace ALL set` 才注册——注释明示"never returns a half-empty bundle that confuses the LLM"。这是 prompt 工程的强约束：LLM 看到 correlate_incident 就知道四源都有，不会收到半空 bundle 误判。
- **Post-construction setter 模式**：避免 `NewRegistry` signature 膨胀（已经 8 参数），cmd/main.go 在 service 构造后调 setter——避开循环 wiring order。注释明示"Registry exists earlier in the boot sequence"。
- **`DeviceID *uint64` in ExecuteResult**：post-split 重命名，audit 列保留 `edge_id` storage name 但语义是 device_id——这种"storage 名 vs 语义名"分离降低迁移成本。
- **`Invoke` 零参数替换为 `{}`**：匹配 OpenAI tool_call shape，让 LLM 调零参数工具时不必写 `{}`。
- **包注释解释架构**：frontierbound 解耦、same-BC subdomain import 的设计 rationale 都写在包注释里，便于新 contributor 理解。
- **闭包路径与 BaseTool 路径并存**：`Registry` 是闭包路径核心；`registry_basetool.go` 的 `BuildBaseTools` 是 BaseTool 路径核心。两路径并行，未来一行 type alias 切换。

## 8. 注意事项

- **`tools map` 无锁**：boot 期单线程注册，运行期只读——若未来支持 hot-reload 工具需要加锁。
- **`Register` 静默 overwrite**：re-registration 不报错，caller 负责 uniqueness——若同名工具被注册两次，后者静默覆盖前者，难调试。
- **`Schemas()` 顺序 unspecified**：map 遍历，LLM 看到的 schema 顺序不稳定——若 LLM 对顺序敏感需要排序。
- **`NewRegistry` 已 8 参数**：注释明示"already-heavy constructor"，所以后续依赖走 setter——这个边界要守住，不要往 constructor 加参数。
- **`correlate_incident` 四源 rule 是软约束**：注释明示"mirrors the four-source rule"，但若某部署故意只配三源，correlate_incident 不注册，LLM 无法用它——这是有意的 trade-off。
- **`ExecuteResult.DeviceID` 命名**：post-split 重命名后，闭包路径各 `executeXxx` 仍可能用 `EdgeID` 字段名（已废弃）——重构时要同步。
- **闭包路径与 BaseTool 路径并存**：drift 风险——闭包路径的 `executeXxx` 与 BaseTool 路径的 `XxxTool.InvokableRun` 是两份实现，任何一边改逻辑必须同步另一边。
