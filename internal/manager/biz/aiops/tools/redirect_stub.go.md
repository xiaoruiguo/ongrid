# redirect_stub.go

## 1. 概述

本文件实现 `RedirectStub`——一个 sentinel BaseTool，占用 coordinator toolbag 里的"deep-dive"工具名但**不实际执行查询**。它的存在是为了 catch LLM hallucinate 训练时工具名的习惯（如 LLM"记得" `host_bash` / `get_host_load` 即使这些没出现在 schema 里），把崩溃转成有用的 redirect：一句 tool result 告诉 model 真实工具在 specialist 上，应该通过 `AgentTool` 重新派活。

**问题背景**：没有 stub 时，eino graph runtime 会在 LLM 选 hallucinate 名字时立即 abort `[NodeRunError] tool X not found in toolsNode indexes`，整轮浪费。有 stub 时 model 看到正常 tool message，从中学习，下一轮用 `AgentTool` 重试。

每个 stub 只注册在 coordinator 的 toolbag。Workers（specialists）通过自己的 whitelist 拿真实工具。

## 2. 包信息

- **包名**：`tools`
- **路径**：`internal/manager/biz/aiops/tools/redirect_stub.go`
- **导入**：
  - `basetool`
  - `encoding/json`、`fmt`
- **Class**：`read`（sentinel，不实际执行）

## 3. 关键类型与接口

### `RedirectStub`

```go
type RedirectStub struct {
    ToolName   string  // wire-level 名字，必须匹配 LLM 会 hallucinate 的真实工具名
    Specialist string  // subagent_type，如 "specialist-compute"
    Reason     string  // 一行"为何这工具不在这"的解释，进 redirect message
}
```

字段都是 pure data，便于 `CoordinatorRedirectStubs()` 列表化维护。

## 4. 关键函数与流程

### `Info(_ context.Context)`

返回 `ToolInfo`：

- `Name = s.ToolName`（hallucinate 的真实工具名）
- `Description = "[路由提示] 当前 coordinator 没有直接持有这个工具；复杂诊断请通过 AgentTool 派给 <Specialist>。"`
- `WhenToUse` = "这是路由提示，不是真实业务工具结果。如果用户的问题需要 <Reason> 类能力，用 AgentTool(subagent_type=\"<Specialist>\", ...) 派活；不要把这个提示解释成业务失败。"
- `Parameters = {"type":"object","additionalProperties":true}`（permissive，接受任何 args，因为从不读 args）
- `Class = "read"`

### `InvokableRun(_ context.Context, _ string, _ ...) (string, error)`

返回固定 redirect message，**不检查 argsJSON**——目标是让 LLM 脱困，不是模拟工具。message 包含 suggested `AgentTool` 调用模板，让下一轮有具体模板可用：

```go
payload := map[string]any{
    "status":         "routing_hint",
    "hint":           fmt.Sprintf("This is an internal routing hint, not a business failure. If the user needs this deep-dive capability, call AgentTool to dispatch to %s.", s.Specialist),
    "reason":         s.Reason,
    "suggested_call": fmt.Sprintf(`AgentTool(description="…", subagent_type=%q, prompt="<self-contained task>")`, s.Specialist),
}
```

Marshal 返回 JSON 字符串。

### `CoordinatorRedirectStubs() []basetool.BaseTool`

返回 canonical stub 集合，安装到 coordinator toolbag。每个 entry 映射一个 hallucination-prone 工具名到 owning specialist：

| ToolName | Specialist | Reason |
|----------|------------|--------|
| get_host_load | specialist-compute | host CPU/mem/load 实时快照 |
| get_host_processes | specialist-compute | host top 进程 |
| get_edge_summary | specialist-compute | single-host 综合快照 |
| host_du_summary | specialist-disk | 目录占用分析 |
| host_find_large_files | specialist-disk | 大文件定位 |
| host_stat_file | specialist-disk | 单文件 stat |
| host_probe_http | specialist-network | HTTP 探测 |
| host_probe_dns | specialist-network | DNS 探测 |
| host_probe_tcp | specialist-network | TCP 端口探测 |
| host_netns_inspect | specialist-network | netns 内部状态 |
| host_bash | specialist-ops | host shell（读类命令一般走 ops，写类要走 reviewer） |
| host_restart_service | specialist-ops | systemd 重启（会走 reviewer 二审） |
| correlate_incident | incident-investigator | incident 多信号关联 |
| get_incident_detail | incident-investigator | incident 详情 |
| rank_edges | specialist-sre | 集群按指标排序 |
| find_outlier_edges | specialist-sre | 集群异常机检测 |
| query_promql | incident-investigator | PromQL 查询；如果只关心健康度可派 specialist-sre |
| query_logql | incident-investigator | LogQL 查询 |
| query_traceql | incident-investigator | TraceQL 查询 |

注释明示列表是 conservative 的——只列 LLM 在 evaluation 中被观察到 hallucinate 的工具。不 shadow 整个 specialist bag，因为：

- 每个 stub 占 LLM-presented schema list 一个 slot
- shadow 太多会让 LLM prompt budget 膨胀，可能把 stub 当成"valid options to consider"

注释："Pure data; safe to append over time as new hallucinations show up"。

### Compile-time guard

```go
var _ basetool.BaseTool = (*RedirectStub)(nil)
```

确保 `RedirectStub` 实现 `basetool.BaseTool` 接口。

## 5. 依赖关系

| 方向 | 依赖 | 说明 |
|------|------|------|
| 上游 | `basetool.BaseTool` / `ToolInfo` | 实现 BaseTool 接口 |
| 下游 | 无 | 不调用任何业务逻辑，纯 sentinel |

## 6. 并发与资源管理

- 无 I/O，无 timeout，无共享状态。
- `InvokableRun` 是 pure function（除了 JSON marshal），并发安全。

## 7. 设计模式与亮点

- **Sentinel pattern**：用占位工具 catch LLM hallucinate，把 graph runtime abort 转成有用 redirect——这是 eino graph + LLM hallucination 的工程化 mitigation。
- **Learning loop**：redirect message 包含 `suggested_call` 模板，让 LLM 下一轮有具体模板可用，而不是泛泛"try again"——加速收敛。
- **Conservative 列表**：只 shadow 观察到的 hallucinate 工具，不 shadow 整个 specialist bag——平衡"catch hallucination"和"不膨胀 prompt budget"。
- **Pure data 维护**：`CoordinatorRedirectStubs` 是 data-driven 的，新增 hallucination 只需加一行——注释明示 "Pure data; safe to append over time"。
- **Description / WhenToUse 双重提示**：`Description` 标 `[路由提示]` 前缀，`WhenToUse` 解释"这是路由提示，不是真实业务工具结果"——双管齐下让 LLM 理解这是 redirect 不是 failure。
- **permissive schema**：`{"type":"object","additionalProperties":true}` 接受任何 args，因为从不读 args——避免 LLM 写错 args 时 stub 自己崩溃。
- **specialist 路由策略**：query_promql/logql/traceql 默认路由到 incident-investigator（最宽 read scope），并在 Reason 里提示"如果只关心健康度可派 specialist-sre"——给 LLM 选择空间。

## 8. 注意事项

- **不 shadow 整个 specialist bag**：注释明示 trade-off——shadow 太多会膨胀 prompt budget，可能让 LLM 把 stub 当成 valid option。新增 stub 要谨慎。
- **`Reason` 字段是 LLM-facing**：会进 redirect message，写时要对 LLM 友好（中文 + 简短解释）。
- **`Specialist` 必须是有效 subagent_type**：如果 specialist 不存在，AgentTool 会失败——列表维护要与 `subagent_registry` 同步。
- **`Class = "read"`**：虽然是 sentinel，但标 read 让它不被 ReviewGate 拦截——sentinel 不应该走 reviewer 流程。
- **无 audit**：`InvokableRun` 不走 audit decorator（因为不执行真实操作），audit 行会显示 redirect hint 而非业务结果——这是有意的，让审计能追溯 LLM hallucinate 路径。
- **列表演化**：注释明示 "safe to append over time as new hallucinations show up"——需要持续观察 LLM evaluation，新增 hallucinate 工具时加 stub。
- **不替代 ToolSearch**：ToolSearch 是主动 deferred-loading 机制（LLM 用它查 redacted schema），RedirectStub 是被动 catch（LLM hallucinate 时 fallback）——两者互补，不冲突。
