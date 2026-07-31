# `runtime.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/chatruntime/runtime.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/chatruntime`

## 1. 概述

本文件是 graph-based agent 路径的进程内编排入口（PR-9 cutover 层）：`Runtime.Handle` 串起 10 步主流程（ownership check → mention inline → user msg persist → history load → tryApplyConfirmedConfigDraft 快捷路径 → skill resolve + system prompt → buildEinoHistory → build graph → wire callbacks → invoke）。承载 @-mention、toolreplay 集成、coordinator 工具意图过滤、动态提示、图错误降级道歉、per-request model 选择等关键逻辑。是 chatruntime 包最大、最核心的文件（1778 行）。

## 2. 包信息

- **包名**：`chatruntime`
- **所属模块**：`internal/manager/biz/aiops/chatruntime`
- **依赖方向**：被 manager HTTP service 层调用；调用 graph / graph/callbacks / toolreplay / basetool / aiopsmodel / llm / errs

## 3. 关键类型与接口

```go
type Mention struct{ Type, ID, Label string }
type MentionResolver interface{ Resolve(ctx, []Mention) []string }

type EventType string
const (
    EventAssistant  EventType = "assistant"
    EventToolStart  EventType = "tool_start"
    EventToolEnd    EventType = "tool_end"
    EventDone       EventType = "done"
    EventError      EventType = "error"
)

type AssistantEvent struct{ Iteration, MessageID, Content, CreatedAt; PendingToolCalls int }
type ToolEvent struct{ ToolCallID, Name, DeviceID, Status, StartedAt, EndedAt, DurationMs, Error, ArgsJSON, ResultJSON }
type Event struct {
    Type         EventType
    Assistant    *AssistantEvent
    Tool         *ToolEvent
    Done         *Reply
    Error        string
    Notification *TaskNotification
    Approval     *ApprovalPending
}

type Emit func(Event)

type Request struct {
    SessionID, UserID, Role, UserText, Provider, Model, Locale string
    Mentions []Mention
    WebSearchEnabled bool
    Emit Emit
}

type Reply struct {
    Message    *aiopsmodel.Message
    Usage      llm.Usage
    Iterations int
    ToolCalls  []*aiopsmodel.ToolCall
}

type Config struct {
    SkillRegistry, AgentRegistry, Sessions, ChatModel, ToolBag, CoordinatorStubs
    MentionResolver, CredentialBinder, BasePrompt, HistoryLimit, GraphCfg, CallbackDeps
    AgentWriteEnabled func(ctx) bool
    Logger *slog.Logger
}

type ToolBagProvider interface{ DeferredTools(); AllTools() }
type CredentialBinder interface{ BoundCredentialNamesForSkills(ctx, []string) []string }

type Runtime struct {
    cfg Config
    log *slog.Logger
    workersMu sync.Mutex
    workers   map[string]*Worker
    bag       ToolBagProvider
}
```

`NewRuntime(cfg)` 校验 Sessions + ChatModel 必填，HistoryLimit 默认 50。

## 4. 关键函数与流程

### `Handle`（10 步主流程）
- **签名**：`func (rt *Runtime) Handle(ctx, req *Request) (*Reply, error)`
- **流程**：
  1. **ownership check**：`Sessions.GetSession` → `sess.UserID != req.UserID` 返回 `ErrNotFound`（注释明示非 ErrForbidden 防指纹探测）
  2. **mention inline**：`MentionResolver.Resolve` → bullets → 拼到 UserText 前作为 augmentedUserText
  3. **persist user msg**：`AppendMessage(role=user)`——"legacy invariant: survives a graph crash"
  4. **load history**：`ListMessages(sess.ID, HistoryLimit)`（含刚 append 的 user msg）
  5. **tryApplyConfirmedConfigDraft**：快捷路径，命中则直接返回 reply
  6. **resolve skills + system prompt**：
     - `SkillRegistry.Resolve(UserText, Policy{AllowedClasses: ["*"]})`
     - `CredentialBinder.BoundCredentialNamesForSkills` → `basetool.WithBoundCredentials(ctx, creds)`（HLD-017）
     - persona 解析：`isCoordinator = sess.AgentID == nil/""/"default"`
     - viewer 降级 / `AgentWriteEnabled` gate → `readOnly` → `filterToolsForAgentRole(..., readOnly=true)`
     - persona SystemPrompt 覆盖 basePrompt + persona.Tools 白名单过滤
     - coordinator stub overlay（防幻觉工具名"tool not found in toolsNode"）
     - `filterCoordinatorToolsForIntent` 意图过滤
     - coordinator append `buildAgentCatalog` + `buildToolCapabilityDigest`
     - `ComposeSystemPrompt(basePrompt, activeSkills, nil)`
  7. **buildEinoHistory**：toolreplay 集成
  8. **build graph**：persona MaxTurns 覆盖 GraphCfg.MaxIterations；`graph.BuildReActGraph(ChatModel, sessionToolBag, graphCfg)`
  9. **wire callbacks**：`deps.Persistence.SessionID/Model` 填充；`toCallbackEmitter` 适配 SSE；`NewDefaultHandlers`
  10. **invoke**：
      - `calcDynamicHints(history)` + `agentReminder`
      - ctx 注入：`WithFilteredTools` / `WithLocale` / `WithLLMChoice` / `WithSessionID` / `WithArtifactSource` / `WithHostWriteAllowed`
      - `compose.WithCallbacks(handlers)` + `WithChatModelOption(chatModelOpts(req)...)` + `WithToolsNodeOption(WithUserText)`
      - `defer FinalizeBatches(ctx, handlers)`（autoheal，`context.WithoutCancel` 防取消阻塞）
      - `g.Invoke(ctx, &graph.Input{...})`
  - **错误降级**：invokeErr 非 nil → `buildGraphErrorApology` + best-effort persist + emit assistant + emit done + 返回 reply, nil（不返回 err，保证 SPA 体验）
  - **成功路径**：`AlertDraftGuard.SanitizeAssistantContent` 脱敏 → emit EventDone → 返回 reply

### `buildEinoHistory`（toolreplay 集成）
- **签名**：`func buildEinoHistory(rows []*aiopsmodel.Message) []*schema.Message`
- **职责**：把持久化 rows 翻译为 eino schema.Message，处理 tool_call 配对
- **流程**：
  1. 去掉尾部 role=user 行（graph assembler 会单独 append）
  2. `toolreplay.Resolve(rows)` 全量解析 callIDs（含尾部 user 区，防 pair-by-order 错位）
  3. `toolreplay.IndexToolMessagesByCallID(rows)` 建 tool_call_id → index 索引
  4. 遍历 rows：
     - user/system → 直接 emit
     - assistant with ToolCalls：
       - callIDs 未命中 → `MarkDependentToolsForSkip` + 跳过
       - precheck 任一 tool_call 无 hoistable response → `MarkDependentToolsForSkip` + 跳过
       - emit assistant + `emitToolByCallID(tc.ID)` **立即 hoist** tool response 到 assistant 之后（防 created_at 顺序错乱导致 DeepSeek 400）
       - content="" + 无 ToolCalls → `MarkAllFollowingToolsForSkip` + 跳过（污染数据兜底）
     - tool：**仅通过 hoisting emit**，自然顺序到达的 role=tool 是 orphan → 跳过
- **关键不变量**：tool message 必须紧跟其 parent assistant 的 tool_calls——strict providers（DeepSeek v4+）会 400 拒绝 orphan

### `sanitizeToolReplayContent`
- **签名**：`func sanitizeToolReplayContent(content string) string`
- **职责**：重写过期的 per-turn tool budget envelope
- **流程**：unmarshal 为 `toolBudgetReplayEnvelope`；status == `call_budget_exceeded` → 改 scope=`expired_previous_turn` + instruction 提示可重调 → re-marshal
- **目的**：防止 LLM 把上一轮的 budget 超限当作本轮事实

### `chatModelOpts`
- **签名**：`func chatModelOpts(req *Request) []model.Option`
- **职责**：per-request model 选择（SPA picker）→ eino model options
- **流程**：`req.Provider` → `llm.WithProvider`；`req.Model` → `model.WithModel`；空则不加 option

### `filterCoordinatorToolsForIntent`
- **签名**：`func filterCoordinatorToolsForIntent(bag, userText, isCoordinator) []basetool.BaseTool`
- **职责**：按用户意图关键词过滤 coordinator 可见工具，防 LLM 在简单查询时误用重工具
- **意图识别**：knowledge / metric / metricCatalog / ranking / log / trace / sourceSearch / dbHealth / changeEvent / alertRules / incident / topology / host 多维度关键词（中英文）
- **过滤策略**：
  - knowledge intent + knowledgeLookupIntent → 仅 `query_knowledge`
  - topology intent + topologyFactsIntent → 仅 `get_topology`
  - metricCatalog intent → 仅 `list_metric_catalog`
  - complexHint 或 complexCoordinatorIntent（≥3 intents）→ 仅控制工具（AgentTool/SendMessage/TaskStop/ToolSearch）
  - alertRules intent + !complex → 仅 `query_alert_rules`
  - incident intent + incidentLookupIntent + !complex → 仅 `query_incidents`
  - 其他单 intent → 按意图保留对应工具，剥离无关（如 logIntent 剥 get_topology/host_bash）
- **否定词**："不要先查拓扑" 等关键词关闭 topologyIntent

### `calcDynamicHints`
- **签名**：`func (rt *Runtime) calcDynamicHints(history) []string`
- **职责**：从历史产出 per-turn 提示子弹，注入 `<system-reminder>` 块
- **启发式**：
  - (a) `consecutiveFailedTool(history, 2)` ≥ 2 → "X 已连续失败 N 次：换工具，或问用户澄清"
  - (b) `repeatedToolCall(history, 3)` ≥ 3 → "X 已重复调用 N 次：从已有数据下结论，不要再调用同款工具"
  - (c) `alertDraftGuardNeedsDraftRetry` → 告警草案被拦截后的重试引导
  - (d) `detectUnfollowedPromise` → "上一轮你说'让我...'但没真发 tool_call" 防计划句未执行
- **纯函数**：仅依赖 history，无 LLM/IO

### `buildGraphErrorApology`
- **签名**：`func buildGraphErrorApology(err) string`
- **职责**：把图级错误转为用户友好道歉 markdown，按错误类分类措辞
- **错误类**：
  - "not found in toolsnode" / "tool not found" → 引导用 AgentTool 派生
  - "exceeds max" / "exceeded max" / "max steps" / "maxstep" / "max iterations" → 收敛建议
  - "context canceled" / "context deadline" → 超时/取消提示
  - "budget" → 预算用完
  - "insufficient tool messages" / "tool_calls must be followed" → 脏会话提示新开
  - "429" / "余额不足" / "insufficient_quota" / "rate limit" / "quota" → provider 配额/限流
  - "llm: chat completion" / "openai api" / "api error" → provider 配置问题
  - default → 200 字摘要 + 截图反馈

### `toCallbackEmitter`
- **签名**：`func (rt *Runtime) toCallbackEmitter(emit Emit, sessionID string) callbacks.SSEEmitter`
- **职责**：把 chatruntime.Emit 适配为 callbacks.SSEEmitter
- **映射**：
  - `SSEEventAssistantEnd` → `EventAssistant`（携带完整 content + pending tool count）
  - `SSEEventAssistantStart` → 抑制（legacy SPA 无此 frame）
  - `SSEEventAssistantDelta` → 抑制（token-level streaming 待 SPA 支持）
  - `SSEEventToolStart/End` → `EventToolStart/End`
  - `SSEEventDone` → 抑制（Handle 直接 emit，防双 fire）
  - `SSEEventError` → `EventError`

## 5. 依赖关系

- **标准库**：`context`、`encoding/json`、`errors`、`fmt`、`log/slog`、`strings`、`sync`、`time`
- **外部库**：`github.com/cloudwego/eino/components/model`、`compose`、`schema`
- **内部包**：`biz/aiops`（SessionRepo）、`biz/aiops/graph`、`biz/aiops/graph/callbacks`、`biz/aiops/toolreplay`、`biz/aiops/tools/basetool`、`manager/model/aiops`、`pkg/errs`、`pkg/llm`
- **被调用方**：manager HTTP service 层（Handle）、worker.go（共享 Emit / Event / Request 等类型）

## 6. 并发与资源管理

- **`workersMu sync.Mutex`**：保护 `workers map[string]*Worker`（SpawnWorker/SendToWorker/StopWorker/CountWorkersByStatus 操作）
- **per-request 无共享状态**：Handle 每次调用构建独立 graph + handlers + ctx，无跨请求可变状态
- **`defer FinalizeBatches`**：用 `context.WithoutCancel(ctx)` 防请求 ctx 取消阻塞 autoheal stub 插入
- **emit nil-safe**：`req.Emit == nil` 时用空函数占位，后续逻辑无需 nil-check
- **ctx 透传**：WithEmit / WithFilteredTools / WithLocale / WithLLMChoice / WithSessionID / WithArtifactSource / WithHostWriteAllowed 全部注入 ctx

## 7. 设计模式与亮点

- **ownership check 返回 ErrNotFound**：注释明示"non-owners can't fingerprint sessions"——安全考虑优于明确的 ErrForbidden
- **persist before LLM**：user msg 先落盘再调 graph，graph crash 仍保留用户输入——legacy invariant
- **tryApplyConfirmedConfigDraft 快捷路径**：配置草案确认绕过完整 graph，省一轮 LLM 调用
- **viewer 降级 + AgentWriteEnabled gate**：双重 read-only 控制——viewer 角色 + admin 全局开关任一触发即剥离写工具
- **coordinator stub overlay**：防幻觉工具名导致"tool not found in toolsNode"图崩溃——stub 返回"用 AgentTool 派生"提示
- **filterCoordinatorToolsForIntent 意图过滤**：简单查询不暴露重工具，降低 LLM 误用概率；complexHint ≥3 intents 强制走 AgentTool
- **否定词识别**："不要先查拓扑" 关闭 topologyIntent——尊重用户显式意图
- **toolreplay HOIST**：tool response 立即 hoist 到 parent assistant 之后，防 created_at 顺序错乱——DeepSeek 400 根因修复
- **orphan tool 丢弃**：自然顺序到达的 role=tool（未被 hoist）是 orphan，丢弃防 400
- **precheck 完整性**：assistant 的所有 tool_calls 都必须有 hoistable response，否则整体丢弃——避免发半截 envelope
- **sanitizeToolReplayContent**：重写过期 budget envelope，防 LLM 误把上轮超限当本轮事实
- **错误降级道歉**：图级错误不返回 err，而是 persist + emit 道歉消息——SPA 体验一致
- **错误分类措辞**：不同错误类给不同用户提示（脏会话 vs 配额 vs 配置），可操作性强
- **calcDynamicHints 纯函数**：仅依赖 history，无副作用——可独立测试
- **detectUnfollowedPromise**：检测"让我..."计划句未执行，下轮 nudge——防中等 LLM 戛然而止
- **consecutiveFailedTool / repeatedToolCall**：防 LLM 卡在失败/重复循环
- **alertDraftGuardNeedsDraftRetry**：告警草案被拦截后的精确重试引导
- **toCallbackEmitter 抑制 AssistantStart/Delta**：legacy SPA 兼容，等 SPA 支持再开 token streaming
- **defer FinalizeBatches WithoutCancel**：请求 ctx 取消也能完成 autoheal stub 插入

## 8. 注意事项

- **1778 行大文件**：Handle 主流程 + toolreplay + 意图过滤 + 动态提示 + 错误降级全部集中，未来可考虑拆分
- **`filterCoordinatorToolsForIntent` 关键词硬编码**：中英文关键词列表手工维护，新增意图需改源码
- **`buildGraphErrorApology` 字符串匹配**：跨 gateway 错误消息形态不同，新增 gateway 需扩展匹配
- **`consecutiveFailedTool` 用 `"error"` 子串判定失败**：注释明示"loose parse"——未来工具若用更丰富 JSON 报 partial failure 仍能命中，但成功 payload 若含 "error" 字段会误判
- **`detectUnfollowedPromise` promiseMarkers 中文**：`"让我"/"我先"/"我来"` 等中文计划句标记，英文未覆盖
- **`tryApplyConfirmedConfigDraft` 在 history load 之后**：命中则跳过后续 graph，但 user msg 已 persist——若快捷路径失败需保证不重复 persist
- **`filterCoordinatorToolsForIntent` 仅 coordinator**：`!isCoordinator` 直接返回原 bag，worker 不走意图过滤
- **`buildEinoHistory` 去尾 user**：graph assembler 会 append UserText，不去尾会导致 LLM 看到同一 user turn 两次
- **`sanitizeToolReplayContent` 仅处理 budget envelope**：其他类型的过期内容不重写
- **`toCallbackEmitter` 抑制 Delta**：token-level streaming 已实现但 gate 在 SPA 支持，当前 drop
- **`Handle` 错误降级返回 nil err**：caller 拿到 reply 非 nil + err nil，需通过 reply.Message 判断是否降级路径
