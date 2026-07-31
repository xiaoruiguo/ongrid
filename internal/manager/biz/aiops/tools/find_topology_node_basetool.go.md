# `find_topology_node_basetool.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/find_topology_node_basetool.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件实现 `find_topology_node` 工具：按 name 子串（case-insensitive）搜索业务拓扑节点，返回 `node_id` / `name` / `type`。是 `expand_topology` 的前置工具——当用户只给人类可读名字（"loki-write 依赖什么？"）时，先用本工具拿到 `node_id` 再喂给 `expand_topology`。可选 `type` 精确过滤（service / cluster / app / device / rack），`limit` 默认 20、cap 50。5s 调用超时。仅 BaseTool 形态（无闭包路径），`Class="read"`。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 Registry BaseTool 路径注册调用；依赖 `basetool`、`topologybiz.Usecase`（`ListNodes`）。

## 3. 关键类型与接口

```go
type FindTopologyNodeTool struct {
    topology *topologybiz.Usecase
    log      *slog.Logger
}

type findTopologyNodeArgs struct {
    Name  string `json:"name"`        // 必填，case-insensitive 子串
    Type  string `json:"type,omitempty"` // 可选，精确匹配 Node.Type
    Limit int    `json:"limit,omitempty"` // 默认 20，cap 50
}

type findTopologyNodeHit struct {
    NodeID uint64 `json:"node_id"`
    Name   string `json:"name"`
    Type   string `json:"type"`
}

type findTopologyNodeResult struct {
    Query    string                `json:"query"`
    Type     string                `json:"type_filter,omitempty"`
    Total    int64                 `json:"total"`     // 总匹配数（可能 > Returned）
    Returned int                   `json:"returned"`  // 实际返回数
    Hits     []findTopologyNodeHit `json:"hits"`
    Note     string                `json:"note,omitempty"` // limit clamped / 有更多结果
}
```

常量：`findTopologyNodeMaxLimit=50`、`findTopologyNodeDefaultLimit=20`、`findTopologyNodeCallTimeout=5s`。

## 4. 关键函数与流程

```go
func NewFindTopologyNodeTool(topology *topologybiz.Usecase, log *slog.Logger) *FindTopologyNodeTool
func (t *FindTopologyNodeTool) Info(_ context.Context) (*basetool.ToolInfo, error)
func (t *FindTopologyNodeTool) InvokableRun(ctx, argsJSON, _ ...InvokeOption) (string, error)
```

`InvokableRun` 流程：
1. 守门 `topology != nil`。
2. Unmarshal → `findTopologyNodeArgs`；`strings.TrimSpace(in.Name)`，空则报错 "name is required"。
3. `limit` 默认 20，超过 50 clamp 到 50 并置 `clamped=true`。
4. `context.WithTimeout(ctx, findTopologyNodeCallTimeout=5s)`。
5. `topology.ListNodes(callCtx, NodeListFilter{Type: trim(in.Type), Q: in.Name, Limit: in.Limit})` → `nodes []` + `total int64`。
6. 转 `[]findTopologyNodeHit`。
7. 构造 `findTopologyNodeResult{Query, Type, Total, Returned, Hits}`：
   - `clamped` → `Note = "limit clamped to 50"`。
   - 否则若 `total > len(hits)` → `Note = "%d more results not returned — narrow the query or raise limit"`。
8. Marshal 返回。

## 5. 依赖关系

- **basetool**：`ToolInfo`（`Class="read"`）、`InvokeOption`（忽略）。
- **topologybiz.Usecase**：`ListNodes(ctx, NodeListFilter{Type, Q, Limit})` 返回 `([]*Node, total int64, err)`。`Q` 是 name 子串（case-insensitive），`Type` 精确匹配。
- 不依赖 devicebiz / edgebiz / alertbiz / prom / log / trace。

## 6. 并发与资源管理

- **无锁、无共享可变状态**：`FindTopologyNodeTool` 仅持有不变 `topology` 指针，多 goroutine 可并发调用。
- **无 goroutine**：单次 `ListNodes` 同步调用，5s 超时。
- **无资源持有**：纯查询，无 cursor / iterator。

## 7. 设计模式与亮点

- **`expand_topology` 前置工具**：`WhenToUse` 明示 "Before expand_topology when you only have a human-given name"，给完整示例 "user says 'what does loki-write depend on?' → call find_topology_node{name='loki-write'} to get its node_id, then expand_topology on that"。引导 LLM 形成两步调用链。
- **`Note` 而非 error**：limit 被 clamp 或有更多结果时用 `Note` 告知 LLM，不破坏响应结构。LLM 能据此判断 "要不要 raise limit 重查"。
- **`Total` vs `Returned`**：返回总匹配数与实际返回数，让 LLM 知道是否有遗漏。`Total > Returned` 时 `Note` 提示 narrow query 或 raise limit。
- **`type` 可选过滤**：LLM 可传 "service" / "cluster" / "app" / "device" / "rack" 精确过滤，避免同名跨类型混淆。留空则跨所有类型搜。
- **name trim + 必填**：`strings.TrimSpace` 后空则报错，防止 LLM 传纯空格。
- **5s 超时偏紧**：纯 DB LIKE 查询，应秒级返回；5s 足够兜底慢查询。

## 8. 注意事项

- **无闭包路径**：本工具仅 BaseTool 形态，无 `Registry.executeFindTopologyNode`。若闭包路径需要，需新增。
- **`type` 精确匹配**：不是子串匹配，LLM 传 "svc" 不会匹配 "service"。schema `examples` 列出合法值引导。
- **`Q` 是子串 case-insensitive**：`topologybiz.NodeListFilter.Q` 的语义由 topologybiz 实现，本工具假设是 ILIKE '%Q%'。若需精确匹配应用 `type` + name 组合。
- **`limit` cap 50**：超过 50 强制截断，LLM 无法一次拉更多。如需更多应 narrow query 或分页（本工具不支持 offset）。
- **无 tenant 过滤**：依赖 `topologybiz.Usecase` 内部按 ctx tenant 过滤。`tenant_bind` 装饰器注入 tenant_id 到 ctx。
- **`InvokeOption` 被忽略**：BaseTool 路径下 `opts` 不影响行为。
- **返回的 `node_id` 直接可喂给 `expand_topology`**：无需额外转换，两个工具的 `node_id` 字段同源（topology.Node.ID）。
