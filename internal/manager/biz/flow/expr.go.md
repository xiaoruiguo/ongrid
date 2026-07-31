# `expr.go` 技术实现文档

> 源文件：`internal/manager/biz/flow/expr.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/flow`

## 1. 概述

本文件实现 `{{...}}` 模板解析器 + 微型条件求值器。**故意小**：仅 paths + literals + 一个比较，无脚本引擎。更复杂的逻辑属于 agent / set 节点。支持三种 root：`trigger.x` / `nodes.<id>.output.<path>` / `vars.<name>`；whole-string 单模板返回原生类型（number/object）；mixed text 用 fmt/JSON 渲染。

## 2. 包信息

- **包名**：`flow`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 engine.go（config resolve）+ nodes.go（condition executor）调用；依赖 `encoding/json`、`regexp`、`strconv`、`strings`

## 3. 关键类型与接口

```go
type RunContext struct {
    Trigger map[string]any              // {{trigger.x}}
    Nodes   map[string]any              // {{nodes.<id>.output.<path>}}
    Vars    map[string]any              // {{vars.<name>}}
}

var tmplRe = regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}`)
var segRe = regexp.MustCompile(`\[(\d+)\]`)
```

## 4. 关键函数与流程

### `ResolveString`
- **签名**：`func (c *RunContext) ResolveString(s string) (any, error)`
- **职责**：替换 s 中所有 `{{path}}`
- **流程**：
  1. `tmplRe.FindAllStringSubmatchIndex(s, -1)`
  2. 无匹配 → 返回 s 原样
  3. **whole-string 单模板** → `c.lookup(trimmed)` 返回原生类型（tool arg 可接 number/object）
  4. mixed text → strings.Builder 拼；每段 lookup + stringify
- **关键设计**：whole-string 单模板返回原生类型；mixed text 返回 string
- **错误处理**：lookup 失败返回 error

### `ResolveValue`
- **签名**：`func (c *RunContext) ResolveValue(v any) (any, error)`
- **职责**：递归 walk decoded-JSON value 解析每个 string leaf
- **流程**：
  - string → `ResolveString`
  - map → 递归每个 value
  - []any → 递归每个元素
  - 其他 → 原样
- **用途**：解析整个 node config 对象

### `lookup`
- **签名**：`func (c *RunContext) lookup(path string) (any, error)`
- **职责**：解析 dotted path rooted at trigger/nodes/vars
- **流程**：
  1. split path by "."
  2. parts[0] 分派：
     - "trigger" → cur = Trigger map；parts = parts[1:]
     - "nodes" → 必须 `parts[2]=="output"`；cur = Nodes[parts[1]]；parts = parts[3:]
     - "vars" → cur = Vars map；parts = parts[1:]
     - 其他 → error "unknown root"
  3. 遍历剩余 parts：
     - `splitSegment(p)` → (key, idxs)
     - key 非空 → cur 必须是 map；cur = cur[key]
     - 每个 idx → cur 必须是 array；bounds check；cur = cur[idx]
- **错误处理**：未知 root / node 无 output / field missing / non-array index / out of range → error

### `splitSegment`
- **签名**：`func splitSegment(seg string) (string, []int)`
- **职责**：解析一个 dotted path 段为 object key + trailing array index
- **流程**："results[0]" → ("results", [0])；"[2]" → ("", [2])；"host_load" → ("host_load", nil)
- **关键设计**：支持 `result.results[0].host_load.cpu_pct` 这种 batch-tool 输出

### `EvalCondition`
- **签名**：`func (c *RunContext) EvalCondition(expr string) (bool, error)`
- **职责**：求值 `lhs OP rhs`；OP ∈ {==, !=, >, >=, <, <=, contains}；无 OP 则 truthy 测试
- **流程**：
  1. 遍历 ops 找 `findOperatorIndex`（top-level，忽略 quoted string 和 `{{...}}` 内）
  2. 找到 → evalOperand 左右 + compare
  3. 未找到 → evalOperand 整个 + truthy
- **错误处理**：operand 解析失败 / 不支持的 OP → error

### `findOperatorIndex`
- **签名**：`func findOperatorIndex(expr, op string) int`
- **职责**：返回 op 在 expr 中的 top-level 索引；忽略 quoted string 和 `{{...}}` 内
- **流程**：遍历字符；track quote byte + braces depth；top-level 匹配 op prefix → 返回 i
- **关键设计**：防 `"a==b"` 或 `{{nodes.x.output.op==1}}` 的 inner OP 被误判

### `evalOperand`
- **签名**：`func (c *RunContext) evalOperand(s string) (any, error)`
- **职责**：解析操作数
- **流程**：
  - quoted string → 去引号
  - float → ParseFloat
  - "true"/"false" → bool
  - 其他 → `ResolveString(s)`

### `compare`
- **签名**：`func compare(l, r any, op string) (bool, error)`
- **职责**：执行比较
- **流程**：
  - "contains" → `strings.Contains(stringify(l), stringify(r))`
  - 数字比较：toFloat 双方成功 → 比较
  - == / !=：数字不行则 stringify 比较
  - 其他 OP 非数字 → error
- **关键设计**：数字优先；非数字 == / != 用 stringify

### `toFloat / truthy / stringify / anyMap`
- **签名**：辅助函数
- **职责**：
  - toFloat：float64/int/string/bool → float64
  - truthy：nil→false；bool→自身；string→非空非"false"非"0"；float64→非0；其他→true
  - stringify：nil→""；string→自身；float64→FormatFloat；bool→FormatBool；其他→json.Marshal
  - anyMap：nil map → 空 map（防 nil deref）

## 5. 依赖关系

- **外部库**：`encoding/json`、`fmt`、`regexp`、`strconv`、`strings`
- **被调用方**：engine.go（ResolveValue 解析 node config）、nodes.go execCondition（EvalCondition）

## 6. 并发与资源管理

- **无共享状态**：RunContext 由 engine 持有；engine.mu 保护
- **无锁**：executor 收到 resolved values，不持 map
- **纯函数**：lookup/compare/truthy 等无副作用

## 7. 设计模式与亮点

- **whole-string 单模板返回原生类型**：`{{trigger.device_id}}` 返回 number；tool arg 可直接接 number/object
- **mixed text 返回 string**：`device {{trigger.device_id}} is hot` → stringify
- **三 root 设计**：trigger/nodes/vars；nodes 必须 `output` 段
- **array index 支持**：`results[0].host_load.cpu_pct`；batch-tool 输出可深挖
- **findOperatorIndex top-level**：防 quoted string 和 `{{...}}` 内的 OP 被误判
- **数字优先比较**：`==` 数字优先；非数字 fallback stringify
- **truthy 宽松**：string "false"/"0" 视为 false；其他非空 string true
- **故意小**：注释明示"no script engine"；复杂逻辑属于 agent/set 节点

## 8. 注意事项

- **whole-string 单模板才返回原生类型**：mixed text 总是 string
- **nodes.<id>.output.<path>**：必须含 `output` 段；`{{nodes.x.field}}` 不合法
- **node 无 output 报错**：未上游执行或上游无 output → error
- **array index bounds check**：越界 → error
- **findOperatorIndex 忽略 `{{...}}` 内**：`{{nodes.x.output.op==1}}` 的 `==` 不被误判
- **contains 用 stringify**：`lhs contains rhs` 双方 stringify 后子串匹配
- **非数字 > < >= <= 报错**：仅 == / != 支持 string 比较
- **truthy "false"/"0" 视为 false**：但 "False"/"FALSE" 不被识别（仅小写）
- **vars 写回唯一在 engine mu 内**：executor 不直接改 rc.Vars
