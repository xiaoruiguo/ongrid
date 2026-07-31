# `expand_topology_basetool.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/expand_topology_basetool.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件实现 `expand_topology` 工具：从拓扑图某节点（或 device 关联节点）出发做 BFS，返回所有可达节点 + hop 数 + 关系类型 + AIOps 语义标签。默认 `depth=2`、`only_propagating=true`（仅跟随 `depends_on` / `deployed_on` / `routes_to` 等会传播故障的边）、`direction=both`（对称 blast radius）。用于回答 "X 挂了会影响谁"、"X 的 blast radius"、"X 依赖什么"。8s 调用超时。一次 `ListRelations`（limit 10000）+ 一次 `ListRelationTypes` 全量加载到内存做 BFS，避免 N+1。`fetchNodesByIDs` 因 topology Usecase 未暴露 `GetMany` 而走 N 次 `GetNode` 循环（注释明示若变热需加 GetMany）。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 Registry BaseTool 路径注册调用；依赖 `basetool`、`devicebiz.Usecase`（device_id → NodeID 解析）、`topologybiz.Usecase`（GetNode / ListRelationTypes / ListRelations）、`topologymodel`（Node / RelationType）。

## 3. 关键类型与接口

```go
type ExpandTopologyTool struct {
    topology *topologybiz.Usecase
    devices  *devicebiz.Usecase
    log      *slog.Logger
}

type expandTopologyArgs struct {
    NodeID          uint64 `json:"node_id,omitempty"`    // 与 device_id 互斥，二选一
    DeviceID        uint64 `json:"device_id,omitempty"`
    Depth           int    `json:"depth,omitempty"`       // 默认 2，cap 5
    OnlyPropagating *bool  `json:"only_propagating,omitempty"` // 默认 true
    Direction       string `json:"direction,omitempty"`   // both|downstream|upstream，默认 both
}

// 单条可达节点 + 路径元数据。扁平结构（无嵌套 neighbor）省 prompt token。
type expandTopologyHit struct {
    NodeID, Hops                                          uint64/int
    NodeName, NodeType                                    string
    RelationType, SemanticsTag                            string  // 走过的边类型 + 语义标签
    Propagates                                            bool    // 该边是否传播故障
    ReachedVia                                            string  // downstream|upstream
    ViaNodeID, ViaNodeName                                uint64/string
}

type expandTopologyResult struct {
    Center  expandTopologyHit   // 起点
    Hops    int                 // 实际使用的 depth（可能被 clamp）
    Count   int
    Hits    []expandTopologyHit
    Note    string              // 静默回退提示（depth clamped / 无 propagating 边）
}
```

## 4. 关键函数与流程

```go
func (t *ExpandTopologyTool) InvokableRun(ctx, argsJSON, _ ...InvokeOption) (string, error)
func (t *ExpandTopologyTool) fetchNodesByIDs(ctx, ids []uint64) (map[uint64]*topologymodel.Node, error)
func sortHitsByHopsThenName(hits []expandTopologyHit) // 插入排序，n 通常 <50
```

**`InvokableRun` 流程**：
1. 守门 `topology != nil`。
2. Unmarshal → `expandTopologyArgs`；校验 `node_id` 与 `device_id` 至少一个非零。
3. `depth` 默认 2，超过 5 clamp 到 5 并置 `clampedDepth=true`（用于后续 `Note`）。
4. `onlyPropagating` 默认 true；`direction` 默认 "both"，校验三选一。
5. `context.WithTimeout(ctx, expandTopologyCallTimeout=8s)`。
6. **起点解析**：`node_id != 0` 直接用；否则 `devices.Get(deviceID)` 取 `dev.NodeID`（PR-2 backfill 保证非 legacy 行非 nil；若 nil 报错提示 "topology.Migrate will backfill on next boot"）。
7. `topology.GetNode(startID)` 取 center。
8. `topology.ListRelationTypes` → 构建 `typeMeta map[name]*RelationType`（含 `PropagatesFailure` / `SemanticsTag`）。
9. `topology.ListRelations(limit=10000)` 一次性拉全量关系。
10. **BFS**：`visited map[uint64]visit{hops, relationType, semanticsTag, propagates, via, reachedVia}`，队列切片 `[]uint64`。每跳遍历 `allRel`，按 direction 匹配 `r.SrcID==cur`（downstream）或 `r.DstID==cur`（upstream），`onlyPropagating` 时跳过 `!rt.PropagatesFailure` 的边，已访问节点跳过。depth 用尽即停。
11. `fetchNodesByIDs` 批量 hydrate 非 center 节点（N 次 `GetNode`，missing 跳过防 stale relation 拖垮整工具）。
12. 组装 `hits`，`sortHitsByHopsThenName`（近者优先，同名次按 NodeName 字典序）。
13. 构造 `expandTopologyResult`，`clampedDepth` 或 `len(hits)==0 && onlyPropagating` 时加 `Note` 提示。
14. Marshal 返回。

## 5. 依赖关系

- **basetool**：`ToolInfo`（`Class="read"`）、`InvokeOption`（忽略）。
- **devicebiz.Usecase**：`Get(ctx, id)` 用于 device_id 路径解析 NodeID。
- **topologybiz.Usecase**：`GetNode` / `ListRelationTypes` / `ListRelations(RelationListFilter{Limit:10000})`。无 `GetMany`，因此 `fetchNodesByIDs` 走循环。
- **topologymodel**：`Node`（ID/Name/Type）、`RelationType`（Name/PropagatesFailure/SemanticsTag）、`Relation`（SrcID/DstID/Type）。
- 不依赖 alertbiz / edgebiz / prom / log / trace。

## 6. 并发与资源管理

- **无锁、无共享可变状态**：`ExpandTopologyTool` 仅持有不变 usecase 指针，`InvokableRun` 内所有变量都是局部。多 goroutine 可并发调用。
- **无 goroutine**：纯同步 BFS。所有 IO（`GetNode` / `ListRelationTypes` / `ListRelations` / `fetchNodesByIDs` 内 N 次 GetNode）串行，受 `callCtx` 8s 上限保护。
- **8s 超时**：`expandTopologyCallTimeout = 8 * time.Second`，覆盖全部 IO + BFS。BFS 本身是 CPU-bound 但 N 通常很小（≤10k 关系），单秒内完成。

## 7. 设计模式与亮点

- **BFS in-memory over 全量 relations**：注释明示 "tenant-scale (≤10k relations) cheaper than N+1 per-node lookups"。一次 `ListRelations(10000)` + 一次 `ListRelationTypes` 把图加载到内存，BFS 纯 CPU。Trade-off：超过 10k 关系会截断，但当前 tenant 规模够用。
- **`only_propagating` 默认 true**：只走 `PropagatesFailure=true` 的边（hard_dep / runtime_dep / traffic），结果即 "failure blast radius"。设 false 才包含 annotation / observation 边。这个默认值是 AIOps reasoning loop 实际需求的体现。
- **direction=both 对称 blast radius**：downstream（src 故障波及哪些 dst）+ upstream（哪些 src 能让 dst 故障）双向 BFS，给 LLM 完整影响面。
- **`Note` 字段而非 error**：depth 被 clamp / 无 propagating 边时用 `Note` 告知 LLM，不破坏响应结构。LLM 能继续推理 "要不要 `only_propagating=false` 重试"。
- **扁平 hit 结构**：`expandTopologyHit` 无嵌套 neighbor，省 prompt token——每条 hit 一行就能描述。
- **stable sort by hops then name**：`sortHitsByHopsThenName` 插入排序（n<50 时优于 stdlib sort），近者优先，同 hop 按 NodeName 字典序，给 LLM 稳定可预测的输出顺序。
- **fetchNodesByIDs 容错**：stale relation 指向已删节点时 `GetNode` err 被跳过，不让整工具失败——"shouldn't happen" 但仍兜底。

## 8. 注意事项

- **`fetchNodesByIDs` 是 N 次 GetNode**：注释明示 topology Usecase 未暴露 `GetMany`，若该工具变热需在 topologybiz 加 `GetMany(ctx, ids) ([]*Node, error)`。当前 N 通常 <50，可接受。
- **`ListRelations(Limit:10000)` 截断风险**：超过 10k 关系的 tenant 会丢数据，BFS 结果不全。当前假设 tenant 规模 ≤10k；如需支持更大规模需改成分页或加 `GetRelationsForSubgraph(startID, depth)` 服务端方法。
- **`device.NodeID` 可能为 nil**：PR-2 backfill 之前的 legacy 行 NodeID 为 NULL，工具会报错提示 "topology.Migrate will backfill on next boot"。新 register 通过 NodeMirror 填充。
- **8s 超时偏紧**：`fetchNodesByIDs` 的 N 次 GetNode 若遇 DB 慢查询可能超时。如发生，考虑加 `GetMany` 减少 roundtrip 或加大超时。
- **`only_propagating=false` 可能返回大量边**：annotation / observation 边数量可能远超 propagating 边，prompt 体积膨胀。LLM 应优先用默认 true，确实需要观察性边再设 false。
- **`direction` 三选一**：`both` 是对称 blast radius，`downstream` 是 "X 挂了影响谁"，`upstream` 是 "谁挂了影响 X"。LLM 需根据问题方向选对。
- **无 tenant 过滤**：本工具 args 无 `tenant_id`，依赖 `topologybiz.Usecase` 内部按 ctx tenant 过滤。`tenant_bind` 装饰器会注入 tenant_id 到 ctx，topology UC 应从 ctx 读取。
