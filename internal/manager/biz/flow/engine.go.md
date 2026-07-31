# `engine.go` 技术实现文档

> 源文件：`internal/manager/biz/flow/engine.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/flow`

## 1. 概述

本文件实现 DAG 执行器 `Engine`。**确定性骨架 + 概率节点内部**（HLD-016）：engine 本身从不问 LLM 任何事；只有 agent/llm 节点内部非确定。调度语义：从 trigger 节点启动；节点完成触发一个 control port，每条边激活 target（fan-out 并发上限 `maxConcurrentNodes=4`）；**OR-join + execute-once**：节点首次被任一入边激活时运行，后续 no-op；节点 error 走 "error" port（已连则分支处理，未连则 run 失败）。

## 2. 包信息

- **包名**：`flow`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 `Usecase.triggerRun` 调用；依赖 `model/flow`、`log/slog`

## 3. 关键类型与接口

```go
const maxConcurrentNodes = 4

type Engine struct {
    exec Executors
    runs RunRepo
    log  *slog.Logger
}

type runState struct {
    mu       sync.Mutex
    rc       *RunContext
    executed map[string]bool
    failed   bool
    firstErr error
    wg       sync.WaitGroup
    sem      chan struct{}  // buffered chan 作为计数信号量
}
```

Sentinel：`maxConcurrentNodes = 4`（fan-out 并发上限，防 wide graph 无限 spawn agent worker）。

## 4. 关键函数与流程

### `NewEngine`
- **签名**：`func NewEngine(exec Executors, runs RunRepo, log *slog.Logger) *Engine`
- **职责**：构造 Engine；log nil → Default
- **流程**：返回 `&Engine{exec, runs, log}`

### `Execute`
- **签名**：`func (e *Engine) Execute(ctx, run *model.FlowRun, g *Graph, entryType string) (status string, runErr error)`
- **职责**：同步执行 graph 到完成；返回 terminal run status
- **流程**：
  1. **defer recover**：panic → status=Failed, runErr=panic error, Error log + stack
  2. 过滤 triggers：entryType=="" 起所有；否则仅匹配 type
  3. 无匹配 trigger → Failed, "graph has no X trigger node"
  4. unmarshal run.TriggerJSON → trigger map
  5. 构造 runState{rc, executed, sem: make(chan struct{}, maxConcurrentNodes)}
  6. 构建 byID map（node id → GraphNode）
  7. 每个 trigger 节点 `activate`
  8. `st.wg.Wait()` 等所有 fan-out 完成
  9. st.failed → Failed, firstErr；否则 Succeeded, nil
- **关键设计**：entryType 让 flow 同时有 manual + alert trigger 时，alert 不 double-fire manual 分支
- **错误处理**：panic recover 兜底；wg.Wait 后查 st.failed

### `activate`
- **签名**：`func (e *Engine) activate(ctx, run, g, byID, st, id string)`
- **职责**：调度节点 id 执行（若未执行且未 failed）
- **流程**：
  1. mu.Lock
  2. executed[id] || failed → Unlock + return（execute-once OR-join；failed 后不启新节点）
  3. executed[id] = true；Unlock
  4. byID[id] 不存在 → return
  5. `wg.Add(1)`；`go func`：
     - defer wg.Done
     - **defer recover**：node panic → Error log + stack + st.failed=true + firstErr
     - `sem <- struct{}{}`（acquire；阻塞等空 slot）
     - defer `<-sem`（release）
     - `runNode(...)`
- **关键设计**：execute-once 在 mu 内检查+标记；sem 在 goroutine 内 acquire（阻塞而非丢弃，保证最终执行）

### `runNode`
- **签名**：`func (e *Engine) runNode(ctx, run, g, byID, st, node GraphNode)`
- **职责**：解析 config + execute + 持久化 row + 触发 control port
- **流程**：
  1. 构造 FlowRunNode row（Status=Running, InputJSON="{}", OutputJSON="{}", StartedAt=now）
  2. **mu.Lock 内** resolve config：unmarshal node.Config → rc.ResolveValue → cfg
  3. rcSnapshot = st.rc（拷贝引用供 executor 用）
  4. mu.Unlock
  5. cfg nil → 空 map；marshal InputJSON
  6. `runs.CreateNode(row)`
  7. resolveErr 非 nil → execErr = resolveErr；否则 `exec.execute(ctx, node, cfg, rcSnapshot)`
  8. finished = now；row.FinishedAt
  9. execErr 非 nil：
     - row.Status=Failed, Error=truncate(err, 2000), FiredPort=PortError
  10. 否则：
      - row.Status=Succeeded, FiredPort=res.Port
      - **mu.Lock 内** `rc.Nodes[node.ID] = res.Output`；应用 res.Vars 到 rc.Vars
      - marshal OutputJSON
  11. `runs.UpdateNode(row)`
  12. execErr 非 nil：
      - targets = `g.EdgesFrom(node.ID, PortError)`
      - 无 targets → **mu.Lock** st.failed=true + firstErr；return（run 失败）
      - 有 targets → **mu.Lock** `rc.Nodes[node.ID] = {"error": err.Error()}`；Unlock；遍历 targets activate（分支处理）
  13. 无 err：`g.EdgesFrom(node.ID, res.Port)` 遍历 activate
- **关键设计**：config resolve 在 mu 内（读 rc）；executor 在 mu 外（慢：agent/tool）；Vars 写回在 mu 内（rc.Vars 唯一 mutation 点）

### `RunSingle`
- **签名**：`func (e *Engine) RunSingle(ctx, node GraphNode, rc *RunContext) (NodeResult, error)`
- **职责**：隔离执行单节点（editor "test run" 用）；无持久化无 edge traversal
- **流程**：
  1. unmarshal node.Config → rc.ResolveValue → cfg
  2. `exec.execute(ctx, node, cfg, rc)`

### `truncate`
- **签名**：`func truncate(s string, n int) string`
- **职责**：硬截断 error message 到 2000 字符

### `entryTypeLabel`
- **签名**：`func entryTypeLabel(t string) string`
- **职责**：t=="" → "any"；否则原样

## 5. 依赖关系

- **内部包**：`model/flow`（FlowRun / FlowRunNode / status 常量）
- **外部库**：`encoding/json`、`fmt`、`log/slog`、`runtime/debug`、`sync`、`time`
- **协作**：`Executors`（nodes.go）、`RunRepo`（usecase.go）、`Graph`（graph.go）、`RunContext`（expr.go）

## 6. 并发与资源管理

- **`runState.mu sync.Mutex`**：保护 rc / executed / failed / firstErr；config resolve + Vars 写回在 mu 内
- **`runState.wg sync.WaitGroup`**：等所有 fan-out goroutine 完成
- **`runState.sem chan struct{}` 容量 4**：buffered chan 作计数信号量；acquire 阻塞等空 slot；release 非阻塞
- **executor 在 mu 外执行**：agent/tool 慢；不能持锁
- **Vars 写回唯一 mutation 点**：executor 返回 res.Vars；engine 在 mu 内应用；executor 绝不直接改 rc.Vars
- **双层 panic recover**：Execute 级 recover 守护主 goroutine；activate 级 recover 守护 fan-out goroutine（防 node panic crash 整个 manager）

## 7. 设计模式与亮点

- **确定性骨架 + 概率内部**：engine 不问 LLM；agent/llm 节点内部非确定
- **OR-join + execute-once**：节点首次激活运行，后续 no-op；diamond after condition 不死锁（无需 merge 节点）
- **fan-out 并发上限 4**：sem 信号量防 wide graph 无限 spawn
- **双层 panic recover**：Execute 级 + activate 级；node panic 不 crash manager
- **entryType 过滤 trigger**：flow 同时有 manual + alert trigger 时，alert 不 double-fire manual 分支
- **error port 分支处理**：节点 err 走 "error" port；已连则分支处理（不 fail run），未连则 run 失败
- **config resolve 在 mu 内**：读 rc；executor 在 mu 外（慢）；Vars 写回在 mu 内（唯一 mutation）
- **FlowRunNode row 始终持久化**：InputJSON/OutputJSON 非 NULL 默认 "{}"；Error 截断 2000
- **RunSingle 无持久化**：editor test-run 用；无副作用（但 read-class tool 实际执行）

## 8. 注意事项

- **maxConcurrentNodes=4**：fan-out 第 5 个节点阻塞等 slot；不会丢弃但会延迟
- **execute-once per run**：节点不会重执行；diamond 后的 merge 节点首次激活后 no-op
- **error port 未连 → run 失败**：其他 in-flight 分支会跑完再 fail
- **error port 已连 → 分支处理**：error 暴露给 handler 分支 `rc.Nodes[node.ID] = {"error": err}`
- **node panic 不 crash manager**：activate 级 recover；firstErr 记录 panic
- **InputJSON/OutputJSON 默认 "{}"**：TEXT NOT NULL column；必须非空
- **Error 截断 2000**：DB column 限制
- **Vars 写回在 mu 内**：executor 返回 res.Vars；engine 应用；executor 绝不直接改 rc.Vars
- **RunSingle 无持久化**：test-run 不写 row；但 read-class tool 实际执行（有 side effect 的 tool 被 usecase.testRunSideEffect 拦截）
- **entryType="" 起所有 trigger**：legacy manual 行为；alert/cron 路径传具体 type
