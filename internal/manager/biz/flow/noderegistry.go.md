# `noderegistry.go` 技术实现文档

> 源文件：`internal/manager/biz/flow/noderegistry.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/flow`

## 1. 概述

本文件实现 node-type 注册表（HLD-016 node 抽象）。Node type 是 first-class、self-describing 实体：一个 `NodeSpec` 声明结构行为（Kind）、palette 分组（Category）、control ports、executor。engine / graph validation / trigger detection 都**从注册表派生** —— 无 per-type switch、无 "trigger." string-prefix 约定、无 knownTypes map。新增 node type = `RegisterNode(one spec)`；核心 engine 不变。

## 2. 包信息

- **包名**：`flow`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 graph.go（portsFor/isTriggerType）、engine.go（execute dispatch）、nodes.go（Executors.execute）调用；依赖 `context`、`encoding/json`、`fmt`、`strings`

## 3. 关键类型与接口

```go
type NodeKind string

const (
    KindTrigger NodeKind = "trigger"  // entry point：无入边，无 error port
    KindAction  NodeKind = "action"   // 调能力：agent / llm / tool / notify
    KindControl NodeKind = "control"  // 分支/合并：condition / (future) merge
    KindData    NodeKind = "data"     // 塑数据：set / (future) transform
)

type ExecuteFunc func(ctx, x Executors, cfg map[string]any, rc *RunContext) (NodeResult, error)

type ConfigFieldSpec struct {
    Key, LabelZh, LabelEn, Kind, Placeholder string
    Options []string  // for kind=select
}

type NodeSpec struct {
    Type         string
    Kind         NodeKind
    Category     string
    LabelZh, LabelEn string
    Ports        []string              // 默认 [next]；condition=[true,false]
    ConfigFields []ConfigFieldSpec     // tool 节点空（args 从 BaseTool schema）
    OutputShape  []string              // 静态输出路径；[] 动态（transform/agent）
    Execute      ExecuteFunc
}

var nodeRegistry = map[string]*NodeSpec{}
```

## 4. 关键函数与流程

### `RegisterNode`
- **签名**：`func RegisterNode(s *NodeSpec)`
- **职责**：加/替换 node type；Ports 空 → 默认 `[PortNext]`
- **流程**：s nil 或 Type 空 → return；Ports 空 → 设 `[PortNext]`；`nodeRegistry[s.Type] = s`

### `LookupNode`
- **签名**：`func LookupNode(t string) *NodeSpec`
- **职责**：返回 type 的 spec；未注册 nil

### `AllNodeSpecs`
- **签名**：`func AllNodeSpecs() []*NodeSpec`
- **职责**：返回所有注册 spec（palette / node-types API）

### `init`
- **签名**：`func init() { registerBuiltins() }`
- **职责**：包初始化时注册内置 node types

### `registerBuiltins`
- **签名**：`func registerBuiltins()`
- **职责**：注册所有内置 node types
- **流程**：`RegisterNode` 11 个 spec：
  - trigger.manual（KindTrigger，Category=trigger，ConfigFields 空）
  - trigger.alert_fired（KindTrigger，ConfigFields=[rule, min_severity]，OutputShape=[incident_id, rule, severity, edge_id, device_id, labels, fired_at]）
  - trigger.cron（KindTrigger，ConfigFields=[cron]，OutputShape=[fired_at, cron]）
  - agent（KindAction，Category=ai，ConfigFields=[persona, instruction, output_schema]，OutputShape=[answer]）
  - llm（KindAction，Category=ai，ConfigFields=[system, prompt, output_schema]，OutputShape=[answer]）
  - tool（KindAction，Category=action，ConfigFields 空（args 从 BaseTool schema），OutputShape=[result]）
  - condition（KindControl，Category=flow，Ports=[true, false]，ConfigFields=[expr]，OutputShape=[result]）
  - notify（KindAction，Category=action，ConfigFields=[channel_ids, title, message]，OutputShape=[sent, channels]）
  - set（KindData，Category=data，ConfigFields=[name, value]，OutputShape=[name, value]）
  - transform（KindData，Category=data，ConfigFields=[fields]，OutputShape 空（动态））
  - http_request（KindAction，Category=action，ConfigFields=[method, url, headers, body, timeout_seconds]，OutputShape=[status, body, headers]）

### 内置 executors

#### `execTrigger`
- **签名**：`func execTrigger(_, _, _, rc *RunContext) (NodeResult, error)`
- **职责**：trigger 节点"output"是 trigger payload 自身；下游可用 `{{trigger.x}}` 或 `{{nodes.<id>.output.x}}`

#### `execAgent`
- **签名**：`func execAgent(ctx, x, cfg, _) (NodeResult, error)`
- **职责**：跑同步 agent worker 返回 final answer
- **流程**：
  1. x.Agent nil → error "runner not wired"
  2. persona 空 → "default"
  3. instruction 空 → error
  4. output_schema 存在 → 拼接 JSON Schema 指令
  5. `context.WithTimeout(ctx, agentNodeTimeout)`（15min）
  6. `x.Agent.RunAgent(actx, persona, prompt)`
  7. 有 schema → `parseLooseJSON(answer)` → structured；失败 error
  8. 返回 `{answer, structured?}`

#### `execLLM`
- **签名**：`func execLLM(ctx, x, cfg, _) (NodeResult, error)`
- **职责**：单次 chat completion（无 tool 无多轮）
- **流程**：类似 execAgent；timeout 3min；output_schema 同 agent

#### `execTool`
- **签名**：`func execTool(ctx, x, cfg, _) (NodeResult, error)`
- **职责**：按名 dispatch BaseTool
- **流程**：
  1. x.Tools nil → error
  2. name 空 → error
  3. args marshal
  4. `context.WithTimeout(ctx, defaultNodeTimeout)`（2min）
  5. `x.Tools.InvokeTool(tctx, name, ab)`
  6. unmarshal res；非 JSON 保 string
  7. 返回 `{result}`

#### `execCondition`
- **签名**：`func execCondition(_, _, cfg, rc) (NodeResult, error)`
- **职责**：分支
- **流程**：
  1. expr 空 → error
  2. `rc.EvalCondition(expr)`
  3. ok → PortTrue；否则 PortFalse
  4. 返回 `{result: ok}`

#### `execNotify`
- **签名**：`func execNotify(ctx, x, cfg, _) (NodeResult, error)`
- **职责**：发通知
- **流程**：
  1. x.Notify nil → error
  2. message 空 → error
  3. `toUint64s(cfg["channel_ids"])` 空 → error
  4. `context.WithTimeout(ctx, defaultNodeTimeout)`
  5. `x.Notify.Notify(nctx, ids, title, message)`
  6. 返回 `{sent: true, channels: len(ids)}`

#### `execSet`
- **签名**：`func execSet(_, _, cfg, _) (NodeResult, error)`
- **职责**：写变量
- **流程**：
  1. name 空 → error
  2. 返回 `NodeResult{Output: {name, value}, Vars: {name: val}}`
- **关键设计**：Vars 写回交给 engine 在 mu 内应用；executor 不直接改 rc.Vars

#### `execTransform`
- **签名**：`func execTransform(_, _, cfg, _) (NodeResult, error)`
- **职责**：字段映射；engine 已 template-resolve 每 value
- **流程**：fields nil → 空 map；返回 `NodeResult{Output: fields}`

## 5. 依赖关系

- **外部库**：`context`、`encoding/json`、`fmt`、`strings`、`time`
- **协作**：`Executors`（nodes.go）、`RunContext`（expr.go）、`NodeResult`（nodes.go）
- **被调用方**：`init()` 包初始化；`graph.go`（portsFor/isTriggerType）；`engine.go`（execute dispatch）；`catalog.go::ListNodeTypes`（AllNodeSpecs）

## 6. 并发与资源管理

- **`nodeRegistry` 全局 map**：`init()` 注册后只读；运行期不增删
- **无锁**：注册期在 init；运行期纯读
- **ExecuteFunc 无状态**：seams 通过 Executors 参数传入，不捕获；spec 可全局注册

## 7. 设计模式与亮点

- **node type first-class**：NodeSpec 自描述；engine/validator/frontend 都从它派生
- **无 per-type switch**：execute 纯 dispatch `spec.Execute(ctx, x, cfg, rc)`；新增 type 不改 engine
- **无 string-prefix 约定**：trigger 检测基于 NodeSpec.Kind 而非 "trigger." 前缀
- **无 knownTypes map**：graph.Validate 用 LookupNode；未知 type 拒绝
- **Kind 四分类**：trigger/action/control/data；engine 和 validator 按 Kind 行为
- **ExecuteFunc stateless**：seams 通过 Executors 传入；spec 可全局注册
- **ConfigFields 驱动前端 form**：前端从 NodeSpec.ConfigFields 渲染 config drawer 而非硬编码
- **OutputShape 静态 vs 动态**：transform/agent 动态；其他静态；前端用此显示 referenceable fields
- **output_schema 网关**：agent/llm 声明 schema 后下游可引用 structured 字段；parseLooseJSON 容忍围栏/散文
- **execSet Vars 交 engine 应用**：executor 不直接改 rc.Vars；engine 在 mu 内应用防 race

## 8. 注意事项

- **nodeRegistry 全局 map**：`init()` 注册后只读；动态注册需加锁（当前无此需求）
- **Ports 空 → 默认 [next]**：RegisterNode 兜底；condition 显式 [true, false]
- **trigger 无 error port**：KindTrigger 定义；graph.Validate 检查 trigger 无入边
- **output_schema 解析失败 → error**：agent/llm 声明 schema 但 answer 非 JSON → 节点失败
- **parseLooseJSON 容忍围栏/散文**：模型可能加 ``` 或散文；提取 outermost JSON
- **execSet 不直接改 rc.Vars**：返回 res.Vars；engine 在 mu 内应用；防 fan-out race
- **execTransform 动态 OutputShape**：前端读 config.fields keys 显示 referenceable fields
- **agent timeout 15min**：ReAct worker 合法跑分钟；llm 3min（单次 completion）；其他 2min
- **tool args 从 BaseTool schema**：ConfigFields 空；前端从 ToolMeta.Parameters 渲染 form
