# `usecase.go` 技术实现文档

> 源文件：`internal/manager/biz/flow/usecase.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/flow`

## 1. 概述

本文件是 flow-orchestration biz 层门面（HLD-016）。串联 definitions（Repo）、runs（RunRepo）和 engine（Engine）。核心职责：flow CRUD（含 graph 校验）、manual Trigger、event TriggerEvent（alert/cron 路径）、TestNode（editor 单节点 test-run，拦截 side-effect 节点）、HealStaleRuns（boot 清理）、PruneOldRuns（retention sweep）。run 在 detached goroutine 跑 `context.Background()`（请求 ctx 不能取消 in-flight work）。

## 2. 包信息

- **包名**：`flow`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 HTTP handler / dispatcher / scheduler / catalog 调用；依赖 `model/flow`、`pkg/errs`、`github.com/google/uuid`

## 3. 关键类型与接口

```go
type Repo interface {
    Create(ctx, f *model.Flow) error
    Update(ctx, f *model.Flow) error
    Get(ctx, id uint64) (*model.Flow, error)
    List(ctx, limit, offset int) ([]*model.Flow, int64, error)
    ListEnabled(ctx) ([]*model.Flow, error)
    Delete(ctx, id uint64) error
}

type RunRepo interface {
    CreateRun / UpdateRun / GetRun / ListRuns
    CreateNode / UpdateNode / ListNodes
    SweepStaleRunning(ctx, reason string) (int64, error)
    PruneRuns(ctx, before time.Time) (int64, error)
}

const defaultRunRetentionDays = 14

type Usecase struct {
    repo    Repo
    runs    RunRepo
    engine  *Engine
    catalog ToolCatalog  // 可 nil
    llm     GenLLM       // 可 nil
    log     *slog.Logger
}

type CreateInput struct {
    Name, Description, GraphJSON string
    CreatedBy *uint64
}
```

Sentinel：`defaultRunRetentionDays = 14`（可通过 `ONGRID_FLOW_RUN_RETENTION_DAYS` env 覆盖）。

## 4. 关键函数与流程

### `NewUsecase`
- **签名**：`func NewUsecase(repo Repo, runs RunRepo, engine *Engine, log *slog.Logger) *Usecase`
- **职责**：构造 biz facade；engine 可 nil（仅 CRUD 的测试）
- **流程**：log nil → Default；返回 `&Usecase{repo, runs, engine, log}`

### `Create`
- **签名**：`func (u *Usecase) Create(ctx, in CreateInput) (*model.Flow, error)`
- **职责**：校验 graph + 插入 definition
- **流程**：
  1. repo nil → ErrNotWiredYet
  2. name TrimSpace；空 → ErrInvalid "name required"
  3. graph TrimSpace；空 → "{}"
  4. `ParseGraph(graph)`；失败 → ErrInvalid
  5. 构造 `model.Flow{Enabled: true, Version: 1, ...}`
  6. `repo.Create`
- **错误处理**：name 空 / graph 校验失败 → ErrInvalid

### `Update`
- **签名**：`func (u *Usecase) Update(ctx, id uint64, in CreateInput) (*model.Flow, error)`
- **职责**：替换 name/description/graph；graph 变化 → Version++
- **流程**：
  1. repo nil → ErrNotWiredYet
  2. `repo.Get(id)`
  3. name 非空 → 覆盖
  4. description 覆盖
  5. graph 非空且 != 当前 → `ParseGraph` 校验 → 覆盖 + Version++
  6. `repo.Update`
- **关键设计**：graph 未变不 bump Version；Version 是 graph 修订版本

### `SetEnabled / Get / List / Delete`
- **签名**：thin passthroughs
- **职责**：SetEnabled 翻 enabled；List 限制 limit 1-200 默认 50

### `Trigger`
- **签名**：`func (u *Usecase) Trigger(ctx, id uint64, input map[string]any, by *uint64) (*model.FlowRun, error)`
- **职责**：manual run（HTTP "Run" 按钮）；thin wrapper over triggerRun pinned to manual entry
- **流程**：`triggerRun(ctx, id, NodeTriggerManual, input, by, true)`

### `TestNode`
- **签名**：`func (u *Usecase) TestNode(ctx, flowID uint64, nodeType string, configJSON json.RawMessage, triggerInput map[string]any) (any, error)`
- **职责**：editor "test run this node"；返回单节点 output；无持久化
- **流程**：
  1. engine nil → ErrNotWiredYet
  2. **seed upstream context**：runs 非 nil 且 flowID>0 → `ListRuns(flowID, 1)` 取最新 run → `ListNodes(runID)` → 构建 nodesCtx map（NodeID → OutputJSON unmarshal）
  3. `testRunSideEffect(nodeType, configJSON)` → 非空 reason → error（拦截 side-effect 节点）
  4. 构造 `RunContext{Trigger: triggerInput, Nodes: nodesCtx, Vars: {}}`
  5. `engine.RunSingle(ctx, GraphNode{ID: "test", Type: nodeType, Config: configJSON}, rc)`
  6. 返回 res.Output
- **关键设计**：upstream context 从最新 run 取；best-effort；side-effect 节点拦截

### `testRunSideEffect`
- **签名**：`func (u *Usecase) testRunSideEffect(nodeType string, configJSON json.RawMessage) string`
- **职责**：返回非空 reason 当节点不能 test-run（因会真实 side effect）
- **流程**：
  - NodeNotify → "notify node delivers a real message and cannot be test-run..."
  - NodeTool → 查 tool 的 Class；write/destructive → "tool %q is %s-class and cannot be test-run..."
  - 其他 → ""（可 test-run）
- **关键设计**：read-class tool / agent / llm / condition / set / transform 可 test-run；write/destructive tool + notify 不能

### `TriggerEvent`
- **签名**：`func (u *Usecase) TriggerEvent(ctx, flowID uint64, entryType string, payload map[string]any) (*model.FlowRun, error)`
- **职责**：非 manual 源（alert dispatcher / cron scheduler）启动 run
- **流程**：`triggerRun(ctx, flowID, entryType, payload, nil, false)`

### `triggerRun`
- **签名**：`func (u *Usecase) triggerRun(ctx, id uint64, entryType string, input map[string]any, by *uint64, requireEnabled bool) (*model.FlowRun, error)`
- **职责**：共享 run-launch 核心
- **流程**：
  1. repo/runs/engine nil → ErrNotWiredYet
  2. `repo.Get(id)`
  3. !Enabled：requireEnabled → ErrInvalid "flow disabled"；否则 return nil, nil（event path 良性 skip）
  4. `ParseGraph(f.GraphJSON)`；失败 ErrInvalid
  5. 确认 entryType trigger 存在；否则 ErrInvalid
  6. marshal input（nil → "{}"）
  7. 构造 `FlowRun{ID: uuid.NewString(), Status: Running, TriggerType: entryType, TriggerJSON, StartedAt: now}`
  8. `runs.CreateRun`
  9. **detached goroutine**：`context.Background()` + `engine.Execute(bg, run, g, entryType)` + UpdateRun（status/finished_at/error）
  10. 返回 run
- **关键设计**：detached goroutine 用 Background ctx；请求 ctx 不能取消 in-flight work；error 截断 2000

### `GetRun / ListRuns`
- **签名**：读路径
- **职责**：GetRun 返回 run + nodes；ListRuns 限制 limit 1-100 默认 20

### `HealStaleRuns`
- **签名**：`func (u *Usecase) HealStaleRuns(ctx)`
- **职责**：boot 清理 previous process 留下的 running 行
- **流程**：`runs.SweepStaleRunning(ctx, "manager restarted while run was in flight")`；n>0 Info

### `PruneOldRuns`
- **签名**：`func (u *Usecase) PruneOldRuns(ctx)`
- **职责**：retention sweep；scheduler hourly 调 + boot 调
- **流程**：
  1. days = `defaultRunRetentionDays`（14）；env `ONGRID_FLOW_RUN_RETENTION_DAYS` 覆盖（>0）
  2. before = now - days×24h
  3. `runs.PruneRuns(ctx, before)`；n>0 Info

## 5. 依赖关系

- **内部包**：`model/flow`、`pkg/errs`
- **外部库**：`github.com/google/uuid`、`encoding/json`、`fmt`、`log/slog`、`os`、`strconv`、`strings`、`time`
- **桥接接口**：`ToolCatalog`（catalog.go）、`GenLLM`（generate.go）
- **协作**：`Engine`（engine.go）、`Repo`/`RunRepo`（data 层实现）
- **被调用方**：HTTP handler（CRUD + Trigger + TestNode + GetRun/ListRuns）、dispatcher（TriggerEvent）、scheduler（TriggerEvent + PruneOldRuns）、`cmd/ongrid`（HealStaleRuns + PruneOldRuns at boot）

## 6. 并发与资源管理

- **无共享状态**：Usecase 持有不可变 repo/runs/engine/catalog/llm/log
- **detached goroutine**：triggerRun 启动 goroutine 跑 engine.Execute；用 `context.Background()` 不依赖请求 ctx
- **engine.Execute 内部并发**：fan-out goroutine + sem 信号量；engine.mu 保护 runState
- **HealStaleRuns / PruneOldRuns 无并发**：boot 调 + scheduler hourly 调；不并发

## 7. 设计模式与亮点

- **detached goroutine + Background ctx**：run 必须比请求活得长（同 chat workCtx 修复 rationale）；请求 ctx 关闭不能取消 in-flight work
- **requireEnabled 区分 manual/event**：manual 路径 surface "flow disabled" 为 user error；event 路径 benign skip
- **Version 是 graph 修订版本**：Update 仅 graph 实际变化才 bump；FlowRun 记录 FlowVersion 供审计
- **TestNode seed upstream context**：从最新 run 取 nodes output；best-effort；让 `{{nodes.x.output}}` 引用解析
- **testRunSideEffect 拦截**：notify + write/destructive tool 不能 test-run；防真实 side effect
- **HealStaleRuns boot 清理**：engine in-process；restart 后 running 行是 stale；SweepStaleRunning 翻 failed
- **PruneOldRuns env 覆盖**：`ONGRID_FLOW_RUN_RETENTION_DAYS`；默认 14 天
- **error 截断 2000**：DB column 限制；engine.go 同样截断
- **List limit 1-200 默认 50 / ListRuns 1-100 默认 20**：防过大查询

## 8. 注意事项

- **detached goroutine 用 Background ctx**：请求 ctx 不能取消 in-flight work；run 必须比请求活得长
- **Update Version++ only on graph change**：name/description 变不 bump；graph 变才 bump
- **TestNode 拦截 side-effect**：notify + write/destructive tool 不能 test-run；read-class tool 可以
- **HealStaleRuns boot 调一次**：engine in-process；restart 后 running 行是 stale
- **PruneOldRuns env 覆盖**：`ONGRID_FLOW_RUN_RETENTION_DAYS` 必须 >0 才生效
- **error 截断 2000**：DB column 限制；长 error 会被截
- **List limit 默认 50**：操作员可传 1-200；超出默认 50
- **ListRuns limit 默认 20**：操作员可传 1-100；超出默认 20
- **triggerRun event path benign skip**：disabled flow 返回 nil, nil；不报错
- **ParseGraph 在 triggerRun 再校验**：Create/Update 已校验；但 DB row 可能被手编；triggerRun 再校验防坏 graph 到 engine
- **entryType 必须存在 trigger**：triggerRun 检查 graph 有 entryType trigger；否则 ErrInvalid
