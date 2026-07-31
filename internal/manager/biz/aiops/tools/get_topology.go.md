# `get_topology.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/get_topology.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件实现 `get_topology` 工具（闭包路径，挂在 `Registry.executeGetTopology`）：返回部署级拓扑快照——manager version、edge fleet size + online count、configured Prom/Loki/Tempo URL、channel count、enabled rule count。无参数（schema 是空 object）。当问题聚焦 "fleet 多大"、"loki 配了吗"、"manager 什么版本" 时使用。10s 调用超时。所有子查询 best-effort：缺 edge/alert dep 则对应字段不出现。`TopologyInfo` 结构体承载 build-time ldflag（version）与 cmd/main.go 读取的配置 URL，`ChannelCounter` 是回调函数避免 alert repo 接口穿透 Registry。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 Registry 闭包路径调用；依赖 `edgebiz.Usecase`（List）、`AlertUsecase`（ListRules）、`edgemodel.StatusOnline`。与 `get_topology_basetool.go`（BaseTool 镜像）并存。

## 3. 关键类型与接口

```go
// 部署级事实，跨 biz 包不归属任何单一包。
type TopologyInfo struct {
    ManagerVersion     string
    ConfiguredPromURL  string
    ConfiguredLokiURL  string
    ConfiguredTempoURL string
    // ChannelCounter 回调，避免 alert repo 接口穿透 Registry。
    ChannelCounter func(ctx context.Context) (int, error)
}

const topologyCallTimeout = 10 * time.Second
```

`TopologyInfo` 所有字段可选，工具返回非空部分，其余 null/0/""。`ChannelCounter` 是函数类型而非接口，让调用方适配任意 repo/service 形状。

`GetTopologySchema` 是空 object `{"type":"object","properties":{}}`，无 args。注释明示 "everything else is rejected so the model's tool_call shape stays stable"。

## 4. 关键函数与流程

```go
func (r *Registry) executeGetTopology(ctx, _ json.RawMessage) (ExecuteResult, error)
```

流程：
1. `context.WithTimeout(ctx, topologyCallTimeout=10s)`。
2. 初始化 `out map[string]any`：填 `manager_version` / `configured_prom_url` / `configured_loki_url` / `configured_tempo_url`（来自 `r.topology`，可能为空字符串）。
3. **Edge fleet**：`r.edges != nil` 时 `edges.List(callCtx, ListFilter{Limit:5000})`，err 静默。遍历统计 `online`（`e.Status == StatusOnline`），填 `edge_count` / `online_count`。
4. **Enabled rules**：`r.alertUC != nil` 时 `alertUC.ListRules(callCtx, "")`（空 scopeType 拉所有），err 静默。遍历统计 `enabled`（`rule.Enabled`），填 `enabled_rule_count` / `rule_count`。
5. **Channel count**：`r.topology.ChannelCounter != nil` 时调用，err 静默，填 `channel_count`。
6. Marshal 返回 `ExecuteResult{ResultJSON: body}`（无 DeviceID，部署级工具）。

## 5. 依赖关系

- **edgebiz.Usecase**：`List(ctx, ListFilter{Limit:5000})`。Limit 5000 是硬编码，覆盖当前 tenant 规模。
- **AlertUsecase**：`ListRules(ctx, "")`。空 scopeType 拉所有 scope 的规则。
- **edgemodel.StatusOnline**：edge 在线状态常量。
- **TopologyInfo**：value type，由 Registry 构造时通过 `SetTopologyInfo` 注入（version 来自 build-time ldflag，URL 来自 cmd/main.go 读 cfg，ChannelCounter 是回调）。
- 不依赖 devicebiz / prom / log / trace / tunnel。

## 6. 并发与资源管理

- **无锁、无共享可变状态**：`Registry.topology` 是 value type（`TopologyInfo`），读时复制；`executeGetTopology` 内变量局部。多 goroutine 可并发调用。
- **无 goroutine**：串行 `edges.List` → `alertUC.ListRules` → `ChannelCounter`，10s 超时覆盖。
- **best-effort 子调用**：每个子查询 err 静默吞掉，对应字段不出现。保证 "缺 dep 仍返回部分拓扑" 而非整体 error。

## 7. 设计模式与亮点

- **部署级事实载体**：`TopologyInfo` 把跨 biz 包不归属任何单一包的事实（version、URL、channel count）集中。`ChannelCounter` 用函数而非接口，让调用方适配任意 repo/service 形状——"the alert repo doesn't need to be threaded through the Registry"。
- **best-effort 子调用**：缺 edge/alert dep 则对应字段不出现，LLM 仍能基于部分信息回答。这与 `get_edge_summary` 的 best-effort host_load 设计同源。
- **无参数 schema**：空 object，LLM 调用形状稳定。即使传额外字段也会被 schema 拒绝（注释明示）。
- **`edge_count` vs `online_count` 双值**：让 LLM 知道 "fleet 总数 + 当前在线数"，能判断 "是否大部分 edge offline"。
- **`enabled_rule_count` vs `rule_count` 双值**：让 LLM 知道 "总规则数 + 启用数"，能判断 "是否大部分规则被禁用"。
- **10s 超时偏紧**：3 次 DB 查询（edges.List / ListRules / ChannelCounter），应秒级返回；10s 兜底慢查询。

## 8. 注意事项

- **`edges.List(Limit:5000)` 截断**：超过 5000 edge 的 tenant 会丢计数。当前规模够用；若扩大需改用 `Count` API 或分页累加。
- **`ListRules("")` 拉全量**：scopeType 空拉所有 scope，规则数可能较大。当前未分页。
- **`ChannelCounter` 是回调**：若回调自身慢（如查 DB），10s 超时会触发。回调实现应自带超时或快速失败。
- **闭包路径独有**：本文件是 `Registry.executeGetTopology`。BaseTool 形态在 `get_topology_basetool.go`，两者逻辑字节级一致。
- **无 tenant 过滤**：`edges.List` / `ListRules` / `ChannelCounter` 都依赖内部按 ctx tenant 过滤。`tenant_bind` 装饰器注入 tenant_id 到 ctx。但本工具是部署级，tenant 维度可能不适用——视具体 tenant 是否共享 deployment 而定。
- **`TopologyInfo` 是 value type**：Registry 持有副本，构造后修改 `TopologyInfo` 字段不会影响已注入的 Registry。若需热更新（如 version 变更），需 `SetTopologyInfo` 重新注入。
- **`manager_version` 来自 build-time ldflag**：构建时通过 `-ldflags "-X ...ManagerVersion=v1.2.3"` 注入。未设置则为空字符串，LLM 会看到 `"manager_version": ""`。
- **不回传 `ExecuteResult.DeviceID`**：部署级工具，无 device 维度。
