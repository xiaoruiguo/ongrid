# `graph.go` 技术实现文档

> 源文件：`internal/manager/biz/flow/graph.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/flow`

## 1. 概述

本文件定义 canvas wire format + 校验。前端保存正是此形状；engine 每次 run 重新解析。Node I/O 契约（HLD-016）：节点 resolve config 模板后执行，emit (dataOutput, controlPort)；数据流通过 shared run context（**不沿边**）；边仅控制流。Join 语义 OR + execute-once：节点首次被任一入边激活运行，run 内不再重执（diamond after condition 不死锁，无需 merge 节点）。

## 2. 包信息

- **包名**：`flow`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 engine / dispatcher / scheduler / usecase（Create/Update/Trigger）调用；依赖 `encoding/json`、`fmt`、`regexp`、`strings`

## 3. 关键类型与接口

```go
// Node types
const (
    NodeTriggerManual = "trigger.manual"
    NodeTriggerAlert  = "trigger.alert_fired"
    NodeTriggerCron   = "trigger.cron"
    NodeAgent         = "agent"
    NodeLLM           = "llm"
    NodeTool          = "tool"
    NodeCondition     = "condition"
    NodeNotify        = "notify"
    NodeSet           = "set"
    NodeTransform     = "transform"
    NodeHTTP          = "http_request"
)

// Control ports
const (
    PortNext  = "next"
    PortTrue  = "true"
    PortFalse = "false"
    PortError = "error"
)

type GraphNode struct {
    ID       string          `json:"id"`
    Type     string          `json:"type"`
    Name     string          `json:"name,omitempty"`
    Config   json.RawMessage `json:"config,omitempty"`
    Position *Position       `json:"position,omitempty"`
}

type Position struct{ X, Y float64 }

type GraphEdge struct {
    ID         string `json:"id"`
    Source     string `json:"source"`
    SourcePort string `json:"sourcePort,omitempty"`
    Target     string `json:"target"`
}

type Graph struct {
    Nodes []GraphNode `json:"nodes"`
    Edges []GraphEdge `json:"edges"`
}

var nodeIDRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
```

## 4. 关键函数与流程

### `portsFor`
- **签名**：`func portsFor(typ string) []string`
- **职责**：列 node type 的合法 source port（除 "error" 隐式允许）
- **流程**：`LookupNode(typ)` 返回 spec.Ports；未注册 fallback `[PortNext]`

### `isTriggerType`
- **签名**：`func isTriggerType(typ string) bool`
- **职责**：判断是否 trigger（entry point）；基于 NodeSpec.Kind（**无 string-prefix 约定**）
- **流程**：`LookupNode(typ).Kind == KindTrigger`

### `ParseGraph`
- **签名**：`func ParseGraph(raw string) (*Graph, error)`
- **职责**：decode + validate canvas 文档；Save 和 Execute 的单一 gate
- **流程**：
  1. TrimSpace 空 或 "{}" → 返回空 Graph
  2. unmarshal → Graph
  3. `g.Validate()`
- **关键设计**：手编 DB row 必过此 gate 才能到 executor

### `Validate`
- **签名**：`func (g *Graph) Validate() error`
- **职责**：强结构不变量
- **流程**：
  1. 遍历 nodes：
     - `nodeIDRe.MatchString(ID)` → 否则 "bad node id"
     - duplicate ID → "duplicate node id"
     - `LookupNode(Type)` nil → "unknown node type"
  2. 构建 byID map + adj + indeg
  3. 遍历 edges：
     - source/target 必须存在
     - target 不能是 trigger（trigger 无入边）
     - port 空 → PortNext
     - port != PortError → 必须在 `portsFor(src.Type)` 列表
  4. **Kahn 拓扑排序 cycle check**：seen != len(nodes) → "cycle detected"
- **错误处理**：所有错误带 edge id / node id 便于定位

### `Triggers`
- **签名**：`func (g *Graph) Triggers() []GraphNode`
- **职责**：返回 trigger 节点（execution entry points）；基于 `isTriggerType`

### `EdgesFrom`
- **签名**：`func (g *Graph) EdgesFrom(id, port string) []string`
- **职责**：返回从 node id 经 port 可达的 targets
- **流程**：遍历 edges；SourcePort 空 → PortNext；匹配 Source==id && port → 收集 Target

## 5. 依赖关系

- **外部库**：`encoding/json`、`fmt`、`regexp`、`strings`
- **协作**：`noderegistry.go`（LookupNode / NodeSpec.Kind / Ports）
- **被调用方**：engine.Execute / usecase.Create / usecase.Update / usecase.triggerRun / dispatcher.dispatch / scheduler.tick

## 6. 并发与资源管理

- **纯数据结构**：Graph 是值类型；无共享状态
- **无锁**：ParseGraph 返回新 *Graph；调用方持有
- **无 IO**：纯内存校验

## 7. 设计模式与亮点

- **数据流不沿边**：注释明示"data flows through shared run context, NOT along edges; edges are control flow only"
- **OR-join + execute-once**：节点首次激活运行；diamond after condition 不死锁无需 merge
- **node type 字符串**：wire format 用 plain string；palette 可增长无需 schema migration
- **port 由 NodeSpec.Ports 派生**：`portsFor` 查注册表；无硬编码 switch
- **trigger 由 NodeSpec.Kind 派生**：`isTriggerType` 查 Kind；无 string-prefix 约定
- **ParseGraph 单一 gate**：Save + Execute 都过此；手编 DB row 不能绕过校验
- **Kahn cycle check**：拓扑排序；seen != len → cycle
- **trigger 无入边**：Validate 拒绝 edge target 是 trigger
- **Position canvas-only**：engine 忽略；前端 React Flow 用

## 8. 注意事项

- **nodeIDRe `^[a-zA-Z0-9_-]{1,64}$`**：ID 限 64 字符；防过长 + 防特殊字符
- **unknown node type 拒绝**：LookupNode nil → Validate 失败；新增 type 必须先 RegisterNode
- **port "error" 隐式允许**：所有 node type 都可有 error 出边；不需在 Ports 列
- **trigger 无入边**：edge target 是 trigger → Validate 失败
- **cycle 拒绝**：Kahn 拓扑排序检测；自环也拒
- **空 graph 或 "{}" 合法**：返回空 Graph；无 trigger 的 graph Execute 时报 "no trigger"
- **Position 被忽略**：engine 不用；纯前端持久化
- **SourcePort 空 → PortNext**：默认 next port
- **数据流通过 run context**：边仅控制流；`{{nodes.x.output.path}}` 跨节点引用数据
