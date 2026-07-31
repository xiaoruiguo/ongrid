# `nodes.go` 技术实现文档

> 源文件：`internal/manager/biz/flow/nodes.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/flow`

## 1. 概述

本文件定义 node executors + 它们调用的 seams。每个 executor：resolved config in → (data output, control port) out。Seams 在 `cmd/ongrid/main.go` 接现有子系统（chatruntime worker spawn、tools.Registry、notification channels）—— nil seams 让该 node type 退化为 config-time error，engine 本身可 fake 测试。

## 2. 包信息

- **包名**：`flow`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 noderegistry.go（executors）+ engine.go（execute dispatch）调用；依赖 `context`、`encoding/json`、`fmt`、`strings`、`time`

## 3. 关键类型与接口

```go
type AgentRunner interface {
    RunAgent(ctx, persona, prompt string) (answer string, err error)
}

type ToolInvoker interface {
    InvokeTool(ctx, name string, args json.RawMessage) (json.RawMessage, error)
}

type Notifier interface {
    Notify(ctx, channelIDs []uint64, title, message string) error
}

type LLMRunner interface {
    RunLLM(ctx, system, user string) (string, error)
}

type NodeResult struct {
    Output any
    Port   string
    Vars   map[string]any  // set 节点变量写；engine 在 mu 内应用；nil 常见 case
}

type Executors struct {
    Agent  AgentRunner
    LLM    LLMRunner
    Tools  ToolInvoker
    Notify Notifier
}

const (
    defaultNodeTimeout = 2 * time.Minute
    agentNodeTimeout   = 15 * time.Minute
    llmNodeTimeout     = 3 * time.Minute
)
```

Sentinel：`defaultNodeTimeout=2min`（非 agent 节点）、`agentNodeTimeout=15min`（ReAct worker 合法跑分钟）、`llmNodeTimeout=3min`（单次 completion 无 tool loop）。

## 4. 关键函数与流程

### `execute`
- **签名**：`func (x Executors) execute(ctx, node GraphNode, cfg map[string]any, rc *RunContext) (NodeResult, error)`
- **职责**：纯 dispatch 到注册的 NodeSpec.Execute
- **流程**：
  1. `LookupNode(node.Type)` → spec
  2. spec nil 或 Execute nil → error "unknown node type"
  3. `spec.Execute(ctx, x, cfg, rc)`
- **关键设计**：无 per-type switch；行为在 noderegistry.go

### `parseLooseJSON`
- **签名**：`func parseLooseJSON(s string) (map[string]any, error)`
- **职责**：容忍围栏/散文的 JSON 提取
- **流程**：
  1. `start = strings.Index(s, "{")`；`end = strings.LastIndex(s, "}")`
  2. start<0 或 end<=start → error "no JSON object found"
  3. `json.Unmarshal(s[start:end+1], &out)`
- **用途**：agent/llm 节点 output_schema 声明后，模型可能加围栏/散文；robust 提取

### `toUint64s`
- **签名**：`func toUint64s(v any) []uint64`
- **职责**：any → []uint64
- **流程**：v 不是 []any → nil；遍历元素 toFloat + >0 → uint64

## 5. 依赖关系

- **外部库**：`context`、`encoding/json`、`fmt`、`strings`、`time`
- **桥接接口**：`AgentRunner`（chatruntime.Runtime.SpawnWorker）、`ToolInvoker`（tools.Registry.Invoke）、`Notifier`（notify router + channel store）、`LLMRunner`（routing llm.Client）
- **被调用方**：`noderegistry.go`（executors 用 Executors seam + NodeResult + parseLooseJSON + toUint64s）、`engine.go`（execute dispatch）

## 6. 并发与资源管理

- **Executors zero value 可用**：engine 测试用 fake；nil seam 让该 node type 报 config-time error
- **无共享状态**：Executors 是值类型；seams 不可变
- **per-node context.WithTimeout**：executor 内部加 timeout；不依赖外部 ctx deadline
- **NodeResult.Vars 交 engine 应用**：executor 不直接改 rc.Vars；engine 在 mu 内应用防 race

## 7. 设计模式与亮点

- **seam 接口窄**：AgentRunner/ToolInvoker/Notifier/LLMRunner 各 1 方法；测试 fake 简单
- **nil seam 退化 error**：engine 本身可 fake 测试；production wiring 在 main.go
- **NodeResult.Vars 交 engine**：executor 不直接改 rc.Vars；engine 在 mu 内应用；防 fan-out race
- **per-node timeout 分层**：agent 15min / llm 3min / 其他 2min；匹配各自特性
- **parseLooseJSON 容忍**：模型可能加围栏/散文 despite 指令；robust 提取
- **execute 纯 dispatch**：无 per-type switch；新增 type 不改此处
- **Executors bundle**：4 个 seam 打包；zero value 可用；测试时部分 nil 部分填

## 8. 注意事项

- **seam nil → config-time error**：agent/llm/tool/notify 节点 nil seam 报错；engine 本身不 crash
- **per-node timeout**：agent 15min / llm 3min / 其他 2min；超时 ctx cancel
- **NodeResult.Vars 不可直接改 rc.Vars**：必须返回 Vars；engine 在 mu 内应用
- **parseLooseJSON 提取 outermost { }**：嵌套 JSON 取最外层；模型输出多个 {} 时取首末
- **toUint64s 过滤 <=0**：notify channel_ids 必须正；0 或负被过滤
- **Executors zero value 可用**：测试可部分填；production 全填
- **LLMRunner 与 GenLLM 签名兼容**：generate.go 的 GenLLM 可复用 LLMRunner 实现
