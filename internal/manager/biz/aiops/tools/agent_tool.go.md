# `agent_tool.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/agent_tool.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件实现 `AgentTool`：coordinator-only 工具，spawn specialized sub-agent worker 处理可委派任务。同步语义（background 字段已移除——弱 coordinator 模型乱用 background=true 把 pending task_id 当最终答复）。关键设计：dedupe 缓存（sha256(subagent_type + prompt)）90s TTL，拦截"AgentTool → host_bash×N → AgentTool 同任务"循环；LLM-facing name 为 PascalCase "AgentTool" 对齐 claude-code 工具目录以保 tool selection 质量。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 coordinator agent loop 调用；依赖 `basetool`、标准库

## 3. 关键类型与接口

```go
type WorkerSpawner interface {
    SpawnWorker(ctx, req SpawnWorkerRequest) (*WorkerHandle, error)
    SendToWorker(ctx, workerID, message string) error
    StopWorker(ctx, workerID string) error
    GetWorker(workerID string) (*WorkerHandle, bool)
}

type SpawnWorkerRequest struct {
    AgentName, Prompt string
    Background        bool
    ParentSession     string
    Locale, Provider, Model string
}

type WorkerHandle struct {
    ID, AgentName, Status, Result, Err string
    Background bool
    DurationMs int64
}

type SubagentRegistry interface {
    HasAgent(name string) bool
}

type AgentTool struct {
    spawner  WorkerSpawner
    registry SubagentRegistry
    log      *slog.Logger
    dedupe   sync.Map // map[string]*dedupeEntry
}

type dedupeEntry struct {
    result *agentToolResult
    expiry time.Time
}

const (
    AgentToolName = "AgentTool"
    dedupeTTL     = 90 * time.Second
)
```

## 4. 关键函数与流程

### `NewAgentTool`
- **签名**：`func NewAgentTool(spawner WorkerSpawner, registry SubagentRegistry, log *slog.Logger) *AgentTool`
- **职责**：构造 AgentTool
- **流程**：log nil → slog.Default()；返回 `&AgentTool{spawner, registry, log}`

### `dedupeKey`
- **签名**：`func dedupeKey(subagentType, prompt string) string`
- **职责**：hash (subagentType, prompt) 成稳定短 key
- **流程**：`TrimSpace(subagentType) + "|" + Join(Fields(prompt), " ")`（whitespace 归一化），sha256 hex

### `AgentTool.Info`
- **签名**：`func (t *AgentTool) Info(_) (*basetool.ToolInfo, error)`
- **职责**：返回元数据
- **流程**：返回 `ToolInfo{Name: AgentToolName, Description, WhenToUse: agentToolWhenToUse, Parameters: agentToolSchema, Class: "write"}`

### `AgentTool.InvokableRun`
- **签名**：`func (t *AgentTool) InvokableRun(ctx, argsJSON string, opts ...basetool.InvokeOption) (string, error)`
- **职责**：parse args → dedupe 检查 → spawn worker → 返回结果
- **流程**：
  1. spawner nil → `errors.New("AgentTool: runtime not wired")`
  2. unmarshal args；err → `fmt.Errorf("AgentTool: parse args: %w")`
  3. SubagentType 空 / Prompt 空 → error
  4. registry 非 nil 且 `!HasAgent(SubagentType)` → `fmt.Errorf("AgentTool: unknown subagent_type %q")`
  5. **dedupe 检查**：`dedupeKey` → `dedupe.Load(dKey)`；命中且未过期且 result 非 nil → 拷贝 cached，Hint 改写为"重复派活拦截...立即基于它给用户写最终答复；不要再调用任何工具"，marshal 返回（log "dedup hit"）
  6. `spawner.SpawnWorker(ctx, SpawnWorkerRequest{AgentName, Prompt, ParentSession: SessionIDFromContext(ctx), Locale, Provider, Model})`（同步，Background=false）
  7. err → `fmt.Errorf("AgentTool: spawn: %w")`
  8. 构造 `agentToolResult{TaskID, Status, Result, Err}`；Err 非空 → Hint "已返回错误...直接回答用户"；否则 Hint "已返回最终结论...不要再调用任何工具"
  9. store 拷贝（Hint 清空，留给未来 cache hit 改写）
  10. `dedupe.Store(dKey, &dedupeEntry{result: &storeRes, expiry: now.Add(dedupeTTL)})`
  11. marshal res 返回
- **错误处理**：spawner/registry/parse 错误 fail-fast；spawn 错误 wrap 返回

## 5. 依赖关系

- **内部包**：`basetool`（SessionID/Locale/LLMChoice FromContext）
- **外部库**：标准库 `crypto/sha256`、`encoding/hex`、`encoding/json`、`errors`、`fmt`、`log/slog`、`strings`、`sync`、`time`
- **被调用方**：coordinator agent loop；wiring 在 cmd/main.go（chatruntime.Runtime 包成 WorkerSpawner shim）

## 6. 并发与资源管理

- `dedupe sync.Map`：并发安全，多 coordinator turn 共享
- `dedupeEntry` 是 `*agentToolResult` 指针；cache hit 时拷贝 struct 再改 Hint，避免改到缓存内容
- spawner 调用同步阻塞，无新 goroutine
- ctx 透传：ParentSession/Locale/Provider/Model 从 ctx 解析传入 SpawnWorkerRequest

## 7. 设计模式与亮点

- **PascalCase wire name**：对齐 claude-code 工具目录，SOTA 模型已学此名，切 snake_case 会降 tool selection 质量
- **dedupe 反循环**：90s TTL 拦截"重复派同 specialist 同任务"循环（E2E eval D1：5 redundant AgentTool calls）；Hint 改写让 LLM 不观察短路（结果像正常 reply 含更强"停止 loop"提示）
- **store 不带 Hint**：cache hit 时动态改写 Hint，fresh call 拿常规 Hint
- **同步语义**：background 字段移除——弱 coordinator 模型乱用 background=true 把 pending task_id 当最终答复（D1: 122 tool calls in 240s）；future async 走独立 tool name
- **Provider/Model 透传**：从 ctx 拿 coordinator 的 LLM choice 传入 sub-agent，避免 RoutingChatModel fallback 默认 "openai" 让无 OpenAI key install 的 specialist 报错
- **Locale 透传**：英文问题不交给默认中文 GLM specialist（feedback_ai_output_locale.md regression 2026-06-02）
- **registry 可选**：nil 时跳过本地校验，runtime 自身仍会 "agent not found" 报错
- **窄 seam 接口**：WorkerSpawner/SubagentRegistry 在本文件定义而非 import chatruntime，避免 tools → chatruntime 循环

## 8. 注意事项

- **dedupeTTL 90s**：覆盖多 turn 答复组成的秒级间隔；不能太长否则用户 follow-up 相似问题拿 stale 结果
- **Class="write"**：spawn sub-agent 是 mutating 操作（worker 可能写状态），走 review_gate 装饰器
- **Hint 是中文**：直接给 LLM 的指令，coordinator 模型需支持中文；当前面向中文 SRE 场景
- **subagent_type 校验**：registry nil 时无本地校验，依赖 runtime 报错
- **background 字段不再暴露**：schema 注释明示；future async 走独立 tool name 让语义显式
- **store result 不含 Hint**：避免 cache hit 拿到"重复派活"提示而 fresh call 拿不到常规 Hint 的反转
