# `http.go` 技术实现文档

> 源文件：`internal/manager/server/flow/http.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/server/flow`

## 1. 概述

本文件是 workflow-orchestration（HLD-016）的 HTTP 层：暴露 `/v1/flows` CRUD、`/v1/flows/{id}/run`、`/v1/flow-runs/{run_id}`、`/v1/flow-tools`、`/v1/flow-node-types` 等端点。设计要点：读开放给任意已认证用户，写通过 `requireWriter` 中间件 gating（ADR-022：viewer 只读）；`generate` 端点把自然语言 prompt 转 workflow graph（LLM 草拟 + 校验 + 持久化）。关键红线：`MaxBytesReader` 限制 body 大小（create/update 512KB、generate 64KB、testNode 256KB、toggle 4KB）；`testNode` 执行错误返 200 + `{error}` 而非 HTTP 错误（让编辑器显示真实输出）。

## 2. 包信息

- **包名**：`flow`
- **所属模块**：`internal/manager/server/`（HTTP handler 层）
- **依赖方向**：被上层 router 装配调用 `NewHandler` + `Register`；依赖 `biz/flow`（`Usecase`、`CreateInput`、`ConfigFieldSpec`）、`model/flow`、`pkg/errs`、`pkg/tenantctx`

## 3. 关键类型与接口

```go
const roleViewer = "viewer"

type Handler struct {
    uc *bizflow.Usecase
}

// DTOs
type flowDTO struct {
    ID, Name, Description string
    Graph json.RawMessage // 仅 get/create/update 返回，list 省略
    Enabled bool
    Version, NodeCount int
    TriggerType string // 从 graph 提取的 trigger.* 节点类型
    CreatedAt, UpdatedAt string
}

type runDTO struct {
    ID, FlowID, FlowVersion, Status, TriggerType, Error string
    StartedAt, FinishedAt, CreatedAt string
}

type runNodeDTO struct {
    NodeID, NodeType, NodeName, Status, FiredPort, Error string
    Input, Output json.RawMessage
    StartedAt, FinishedAt string
}

type toolMetaDTO struct {
    Name, DisplayZh, Description, DescriptionZh, WhenToUse, Class, Category string
    Parameters json.RawMessage
}

type nodeTypeDTO struct {
    Type, Kind, Category, LabelZh, LabelEn string
    Ports, ConfigFields []...  // ConfigFields 是 bizflow.ConfigFieldSpec
    OutputShape []string
}

type writeBody struct {
    Name, Description string
    Graph json.RawMessage
}
```

## 4. 关键函数与流程

### `NewHandler` / `Register`
- **职责**：构造 + 挂载路由
- **流程**：
  - 读路径（任意已认证）：`GET /v1/flows`、`GET /v1/flows/{id}`、`GET /v1/flows/{id}/runs`、`GET /v1/flow-runs/{run_id}`、`GET /v1/flow-tools`、`GET /v1/flow-node-types`
  - 写路径（requireWriter）：`POST /v1/flows`、`POST /v1/flows/generate`、`PUT /v1/flows/{id}`、`DELETE /v1/flows/{id}`、`POST /v1/flows/{id}/toggle`、`POST /v1/flows/{id}/run`、`POST /v1/flows/{id}/test-node`

### `requireWriter`（中间件）
- **签名**：`func (h *Handler) requireWriter(next http.Handler) http.Handler`
- **职责**：viewer 角色拒写
- **流程**：`tenantctx.From` 失败 → 401；`t.Role == roleViewer` → 403；否则 next

### `toFlowDTO`
- **签名**：`func toFlowDTO(f *model.Flow, withGraph bool) flowDTO`
- **职责**：model → DTO；list 时 `withGraph=false` 省略完整 graph
- **流程**：浅解析 `GraphJSON` 提取 `NodeCount` + 首个 `trigger.*` 节点类型；`withGraph` 时附完整 `json.RawMessage`

### `list` / `get`
- **职责**：列表/详情
- **流程**：list 调 `h.uc.List(ctx, limit, offset)` 返 rows + total；get 调 `h.uc.Get(ctx, id)`；list 用 `toFlowDTO(f, false)`、get 用 `toFlowDTO(f, true)`

### `create` / `update`
- **职责**：创建/更新 flow
- **流程**：requireWriter → `MaxBytesReader(512KB)` → decode `writeBody` → `h.uc.Create/Update(ctx, bizflow.CreateInput{Name, Description, GraphJSON, CreatedBy})` → 201/200 + `toFlowDTO(f, true)`
- **关键**：`CreatedBy` 从 `tenantctx` 取 UserID

### `generate`
- **职责**：`POST /v1/flows/generate` —— NL prompt → workflow graph
- **流程**：requireWriter → `MaxBytesReader(64KB)` → decode `{prompt}` → `h.uc.GenerateGraph(ctx, prompt)` 得 draft → `draft.CreatedBy = &t.UserID` → `h.uc.Create(ctx, draft)` → 201 + `toFlowDTO(f, true)`
- **关键**：注释明示「model drafts a graph from the live tool catalog, we validate + persist it, return new flow so SPA opens it in editor for review」

### `del` / `toggle`
- **职责**：删除/启用-禁用
- **流程**：requireWriter → parseID → `h.uc.Delete(ctx, id)` / `h.uc.SetEnabled(ctx, id, in.Enabled)`；toggle 用 `MaxBytesReader(4KB)`

### `run`
- **职责**：`POST /v1/flows/{id}/run` —— 手动触发
- **流程**：requireWriter → `MaxBytesReader(64KB)` → decode `{input}`（**body 可选**，decode 错误忽略）→ `h.uc.Trigger(ctx, id, in.Input, &t.UserID)` → 202 + `toRunDTO(run)`

### `testNode`
- **职责**：`POST /v1/flows/{id}/test-node` —— 隔离运行单节点
- **流程**：requireWriter → `MaxBytesReader(256KB)` → decode `{node_type, config, trigger_input}` → `h.uc.TestNode(ctx, id, nodeType, config, triggerInput)` →
  - **执行错误**：200 + `{error: runErr.Error()}`（让编辑器显示真实输出）
  - 成功：200 + `{output: out}`
- **关键**：注释明示「Execution errors are 200 + {error} — only malformed requests are HTTP errors」

### `listRuns` / `getRun`
- **职责**：运行历史
- **流程**：listRuns 调 `h.uc.ListRuns(ctx, id, limit)`；getRun 调 `h.uc.GetRun(ctx, run_id)` 返 run + nodes

### `listTools` / `listNodeTypes`
- **职责**：工具/节点类型 palette（编辑器用）
- **流程**：任意已认证 → `h.uc.ListTools()` / `h.uc.ListNodeTypes()` → 翻译 DTO → 200
- **关键**：注释明示「empty list means tools runtime isn't wired (LLM provider absent) — canvas degrades gracefully」

### helpers
- `authed`：tenantctx 校验，返 bool
- `pathID`：chi URLParam → uint64
- `atoiDefault`：query int 解析，失败返默认
- `writeJSON` / `writeErr` / `errCode`：标准 helper（镜像 server/report）

## 5. 依赖关系

- **内部包**：`biz/flow`（`Usecase`、`CreateInput`、`ConfigFieldSpec`）、`model/flow`（`Flow`、`FlowRun`、`FlowRunNode`）、`pkg/errs`、`pkg/tenantctx`
- **外部库**：`github.com/go-chi/chi/v5`
- **被调用方**：上层 router 装配代码

## 6. 并发与资源管理

- **无共享状态**：Handler 仅持有 `uc` 指针
- **ctx 透传**：所有 usecase 调用透传 `r.Context()`
- **body 限制**：各端点按语义设不同 `MaxBytesReader` 上限（4KB~512KB）

## 7. 设计模式与亮点

- **ADR-022 viewer 只读**：`requireWriter` 中间件 gating 写路径；viewer 角色拒写
- **list 省略 graph**：`toFlowDTO(f, false)` 在 list 时不附完整 graph，仅提取 `NodeCount` + `TriggerType` 摘要——减小响应体
- **`generate` LLM 草拟**：自然语言 → workflow graph，model 从 live tool catalog 草拟，校验 + 持久化后返新 flow 给 SPA 编辑器 review
- **`testNode` 执行错误返 200**：注释明示「Execution errors are 200 + {error}」——让编辑器显示真实输出，而非 HTTP 错误
- **`run` body 可选**：`_ = json.NewDecoder(...).Decode(&in)` 忽略错误，支持 bare manual run
- **`MaxBytesReader` 分级**：create/update 512KB（graph 大）、generate 64KB（prompt 小）、testNode 256KB（config + trigger_input）、toggle 4KB（极小）
- **镜像 server/report helper**：`authed`/`pathID`/`atoiDefault`/`writeJSON`/`writeErr`/`errCode` 复制而非共享

## 8. 注意事项

- **`roleViewer = "viewer"`**：与 iam/model 角色名耦合；若 iam 改值需同步
- **`list` limit 默认 50**：`atoiDefault(q.Get("limit"), 50)`；上限由 usecase 控制
- **`generate` 持久化 draft**：LLM 草拟的 graph 直接 Create，SPA 在编辑器 review——不是 dry-run
- **`testNode` 不区分节点不存在 vs 执行失败**：都返 200 + `{error}`，前端需根据 error 内容判断
- **`run` body 可选**：decode 错误忽略，空 input 触发 bare run
- **`listTools` 空列表语义**：tools runtime 未 wire（LLM provider 缺失），canvas 优雅降级
- **`errCode` slug 表**：覆盖 `not-found`/`unauthorized`/`forbidden`/`invalid`/`not-wired-yet`/`internal`；新增 sentinel 需扩展
- **`MaxBytesReader` 各端点不同**：改 body 上限需逐端点改，无统一配置
