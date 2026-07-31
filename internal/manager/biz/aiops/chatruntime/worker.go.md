# `worker.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/chatruntime/worker.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/chatruntime`

## 1. 概述

本文件实现 coordinator/worker 多智能体路径：`Runtime` 持有 in-memory `workers map[string]*Worker`，`SpawnWorker` 启动子智能体（sync 阻塞或 background fire-and-forget），每个 worker 是独立的 `graph.Invoke` 跑 persona-filtered toolbag。状态机：pending → running → completed/failed/killed。承载 ctx 传播（emit/locale/llm choice）、worker tool forwarder（tool 帧转发到 parent SSE）、prologue KB 自动注入、persona 白名单+黑名单过滤等。是 chatruntime 包第二大文件（1035 行）。

## 2. 包信息

- **包名**：`chatruntime`
- **所属模块**：`internal/manager/biz/aiops/chatruntime`
- **依赖方向**：被 tools/AgentTool 调用（SpawnWorker）；调用 graph / graph/callbacks / basetool / aiopsmodel

## 3. 关键类型与接口

```go
type WorkerStatus string
const (
    WorkerStatusPending   WorkerStatus = "pending"
    WorkerStatusRunning   WorkerStatus = "running"
    WorkerStatusCompleted WorkerStatus = "completed"
    WorkerStatusFailed    WorkerStatus = "failed"
    WorkerStatusKilled    WorkerStatus = "killed"
)

type Worker struct {
    ID, AgentName, SessionID, ParentSessionID, Prompt string
    Status WorkerStatus
    Background bool
    StartedAt time.Time
    EndedAt *time.Time
    Result, Err string
    cancel context.CancelFunc
    mu sync.Mutex
}

type SpawnRequest struct {
    AgentName, Prompt string
    Background bool
    ParentSession string
    ParentEmit Emit
    SessionKind string
    OwnerUserID uint64
    Locale, Provider, Model string
}

type TaskNotification struct {
    TaskID string
    Status WorkerStatus
    Summary, Result, Err string
    Usage map[string]any
}

const EventTaskNotification EventType = "task_notification"
const EventApprovalPending EventType = "approval_pending"

type ApprovalPending struct {
    ApprovalID, ToolCallID, Kind, Command string
    Credentials []string
}

// ctx 传播
type emitCtxKeyT struct{}
var emitCtxKey = emitCtxKeyT{}
func withEmit(ctx, emit) context.Context
func EmitFromContext(ctx) Emit
```

## 4. 关键函数与流程

### `SpawnWorker`
- **签名**：`func (rt *Runtime) SpawnWorker(ctx, req SpawnRequest) (*Worker, error)`
- **职责**：启动 worker，sync 阻塞或 background fire-and-forget
- **流程**：
  1. 校验 `rt.cfg.ChatModel` + `rt.cfg.AgentRegistry` 非空
  2. `AgentRegistry.ByName(req.AgentName)` 解析 persona
  3. `newWorkerID()` (agent-<8hex>) + `newWorkerSessionID(id)`
  4. **persist worker session row**：parent session lookup → ownerUserID + parentRefForRow；`Sessions.CreateSession` 写 chat_sessions（agent_id / parent_session_id / background / kind 列）
  5. 构造 `Worker{Status: Pending, cancel: ...}`
  6. 锁内 `rt.workers[w.ID] = w` + `w.Status = Running` + `StartedAt = now`
  7. **background=true**：`go rt.runWorker(...)` detached；立即返回 w（Status=Running）
  8. **background=false**：`runWorker` 同步调用；返回时 w 已 terminal
- **错误处理**：agent 未找到 / registry 未 wire 返回 error

### `runWorker`（内部）
- **职责**：worker 主循环——构建 persona-filtered toolbag + system prompt + graph + callbacks + invoke
- **流程**：
  1. `filterToolsForAgentRole(cfg.ToolBag, persona, isCoordinator=false, readOnly=false)` 白名单+黑名单过滤
  2. `ComposeSystemPrompt(persona.SystemPrompt, activeSkills, persona)`（agentProfile 非 nil → 走 worker persona 路径）
  3. `buildToolCapabilityDigest(filteredTools)` 动态能力清单
  4. `graph.BuildReActGraph(ChatModel, filteredTools, graphCfg)`（persona.MaxTurns 覆盖）
  5. persist user msg（worker session 内）
  6. `callbacks.NewDefaultHandlers(deps)` + **InitCallbacks ROOT FIX**（注释明示关键修复）
  7. `defer callbacks.FinalizeBatches(ctx, handlers)` autoheal
  8. `g.Invoke(ctx, &graph.Input{...})`
  9. terminal 状态：completed（Result 填充）/ failed（Err 填充）/ killed（cancel 触发，不覆盖 err）
  10. **background=true**：终态时通过 `req.ParentEmit` 发 `EventTaskNotification` SSE frame
- **ctx 解耦**：background worker 用 `context.Background()` 派生，与 parent 请求 ctx 解耦——parent 返回后 worker 仍跑

### `filterToolsForAgentRole`
- **签名**：`func filterToolsForAgentRole(bag, persona *Agent, isCoordinator, readOnly bool) []basetool.BaseTool`
- **职责**：按 persona 白名单/黑名单 + coordinator-only + always-available + readOnly 过滤工具
- **规则**：
  - `readOnly=true` → 剥离所有 `Class != "read"` 工具
  - `coordinatorOnlyTools`（AgentTool/SendMessage/TaskStop/ToolSearch）→ 仅 coordinator 保留，worker 全剥
  - `alwaysAvailableTools`（如 ToolSearch 部分场景）→ 保留
  - persona.Tools 白名单非空 → 仅保留白名单 + alwaysAvailable
  - persona.DisallowedTools 黑名单 → 剥离（**black wins**，优先级高于白名单）
  - 动态工具规则（如 draft_config_change 仅 coordinator）
- **worker 不可派生 worker**：AgentTool 在 coordinatorOnlyTools，worker 拿不到——防嵌套

### `workerToolForwarder`
- **签名**：`func workerToolForwarder(parentEmit Emit, parentSessionID, scopedWorkerToolCallID func(string) string) callbacks.SSEEmitter`
- **职责**：把 worker 的 tool 帧转发到 parent SSE，加 worker-scoped 前缀防 tool_call_id 冲突
- **流程**：worker tool event → `scopedWorkerToolCallID(id)` 加前缀 → parentEmit EventToolStart/End
- **目的**：parent SPA 能在 coordinator 的工具调用区看到 worker 的工具进度

### `prologueKBLookup`
- **签名**：`func prologueKBLookup(ctx, userText, locale) string`
- **职责**：worker 启动时自动查知识库，把结果作为 prologue 注入
- **流程**：调 `query_knowledge` 工具 → 命中则返回 markdown bullet 块
- **目的**：worker 无 parent 上下文，KB 自动注入弥补背景缺失

### `workerChatModelOpts`
- **签名**：`func workerChatModelOpts(ctx) (provider, model string, opts []model.Option)`
- **职责**：从 ctx 读取 parent 透传的 provider/model（`basetool.LLMChoiceFromContext`）
- **流程**：non-empty provider → `llm.WithProvider`；non-empty model → `model.WithModel`
- **目的**：sub-agent 用与 coordinator 同一 LLM，避免 routing 默认 openai 导致 specialist 失败

### `SendToWorker`
- **签名**：`func (rt *Runtime) SendToWorker(ctx, workerID, prompt) error`
- **职责**：向已存在 worker 继续发消息（多轮对话）
- **流程**：锁内查 worker → persist user msg → graph.Invoke（复用 worker session）
- **状态校验**：仅 Running 状态可继续

### `StopWorker`
- **签名**：`func (rt *Runtime) StopWorker(workerID) error`
- **职责**：幂等取消 worker
- **流程**：锁内查 worker → `w.cancel()` → 状态转 Killed（不覆盖已有 err）
- **幂等**：多次调用安全

### `CountWorkersByStatus`
- **签名**：`func (rt *Runtime) CountWorkersByStatus() map[WorkerStatus]int`
- **职责**：统计各状态 worker 数（UI tile 展示）

### `snapshotWorker`
- **签名**：`func snapshotWorker(w *Worker) Worker`
- **职责**：值拷贝 Worker（含 EndedAt 指针）供外部读取
- **流程**：锁内拷贝所有字段

### ID 生成
- `newWorkerID()`：`crypto/rand` 4 字节 → hex → `"agent-" + hex`
- `newWorkerSessionID(workerID)`：基于 workerID 派生 session id

## 5. 依赖关系

- **标准库**：`context`、`crypto/rand`、`encoding/hex`、`encoding/json`、`errors`、`fmt`、`strings`、`sync`、`time`
- **外部库**：`einocallbacks`、`model`、`compose`
- **内部包**：`biz/aiops/graph`、`biz/aiops/graph/callbacks`、`biz/aiops/tools/basetool`、`manager/model/aiops`、`pkg/llm`
- **被调用方**：tools/AgentTool.InvokableRun（SpawnWorker）、tools/SendMessageTool（SendToWorker）、tools/TaskStopTool（StopWorker）

## 6. 并发与资源管理

- **`workersMu sync.Mutex`**：保护 `workers map`（SpawnWorker/SendToWorker/StopWorker/CountWorkersByStatus 操作）
- **`Worker.mu sync.Mutex`**：保护 Worker 内部字段（Status/StartedAt/EndedAt/Result/Err）的读写
- **`Worker.cancel context.CancelFunc`**：StopWorker 调用触发 ctx 取消，runWorker 观察 ctx.Done() 转 Killed
- **background goroutine**：detached，用 `context.Background()` 派生，与 parent 请求 ctx 解耦
- **无 auto-eviction**：注释明示 workers 在内存中直到进程重启，未来加 TTL sweeper
- **`InitCallbacks` ROOT FIX**：注释明示关键修复，确保 callback 链正确初始化

## 7. 设计模式与亮点

- **状态机清晰**：pending → running → terminal（completed/failed/killed），转换在锁内原子完成
- **sync vs background 双模式**：sync 阻塞返回 terminal 状态；background 立即返回 running，detached goroutine 跑完后 SSE 通知
- **ctx 解耦**：background worker 用 `context.Background()`，parent 返回后仍跑——parent HTTP/gRPC handler 不被 worker 阻塞
- **worker 不可嵌套**：AgentTool 在 coordinatorOnlyTools，worker 拿不到——防递归派生
- **black wins**：persona.DisallowedTools 优先级高于 Tools 白名单——禁止项不可被白名单覆盖
- **coordinatorOnlyTools**：AgentTool/SendMessage/TaskStop/ToolSearch 仅 coordinator 可见，worker 全剥
- **worker tool forwarder**：worker 的 tool 帧加 worker-scoped 前缀转发到 parent SSE——parent SPA 能看 worker 进度
- **scopedWorkerToolCallID 前缀**：防 parent 与 worker 的 tool_call_id 冲突
- **prologueKBLookup**：worker 无 parent 上下文，KB 自动注入弥补背景缺失
- **workerChatModelOpts ctx 透传**：sub-agent 用与 coordinator 同一 LLM，避免 routing 默认 openai 失败
- **persist worker session row**：parent_session_id / agent_id / background 列落库，audit 可重建 parent → worker 树
- **ownerUserID 继承**：parent session lookup 失败时 req.OwnerUserID 兜底（investigator 用 0 标 system-owned）
- **SessionKind 标签**：investigator 用 "investigation" 让 auto-spawned RCA 不进 /chat 列表
- **Locale 透传**：parent coordinator 的 locale 传给 worker，保证 sub-agent 用同一语言回复（防 GLM 默认 zh）
- **StopWorker 幂等**：多次调用安全，cancel 触发后状态转 Killed 不覆盖已有 err
- **snapshotWorker 值拷贝**：外部读取 Worker 用拷贝，避免外部修改内部状态
- **defer FinalizeBatches**：worker 结束也 autoheal，覆盖"用户关浏览器 mid-tool-batch"场景
- **task_notification SSE frame**：background worker 终态通过 ParentEmit 通知 SPA，UI tile 实时更新

## 8. 注意事项

- **1035 行大文件**：状态机 + 过滤 + 转发 + KB 注入 + ctx 传播全集中，未来可拆分
- **无 auto-eviction**：workers 在内存中直到进程重启，长期运行可能内存累积——注释提到未来加 TTL sweeper
- **`filterToolsForAgentRole` 规则复杂**：白名单 + 黑名单 + coordinatorOnly + alwaysAvailable + 动态工具规则交叉，修改需小心
- **worker session row 持久化**：即使 worker 失败也保留 row，audit 可查；但 in-memory map 才是 live status 权威
- **`prologueKBLookup` 增加延迟**：worker 启动多一次 KB 查询，简单任务可能不必要
- **`workerToolForwarder` 前缀策略**：scopedWorkerToolCallID 加前缀，parent SPA 需理解前缀语义
- **background worker 无超时**：detached goroutine 跑到 terminal 才停，慢 LLM 可能长时间占用——未来可加 worker 级 deadline
- **`SendToWorker` 仅 Running 可继续**：terminal 状态 worker 不可续聊，UI 需引导用户新开
- **`StopWorker` 不等待**：调 cancel 后立即返回，worker 实际终止异步——CountWorkersByStatus 可能短暂仍见 Running
- **`newWorkerID` crypto/rand**：4 字节 hex，理论冲突概率极低但非零；未做冲突检测
- **coordinator-only stubs 不传 worker**：worker 拿不到 CoordinatorStubs，幻觉工具名会直接图崩溃——worker persona 需自身工具完整
