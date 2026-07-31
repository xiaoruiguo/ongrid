# `service.go` 技术实现文档

> 源文件：`internal/manager/service/aiops/service.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/service/aiops`

## 1. 概述

本文件是 AIOps 领域的应用服务层入口，承担 HTTP handler 与 biz 层之间的桥接职责，统一封装 session 仓储、agent loop 调用与（PR-9 引入的）graph kernel 切换。核心红线有三：(1) 单一会话拥有权 —— 非所有者拿到 `ErrNotFound` 而非 `ErrForbidden` 以避免泄露会话存在性；(2) HTTP 请求 ctx 与 turn 生命周期解耦（HLD-021），浏览器刷新/SSE 断开不会杀死进行中的 turn，但用户显式 Esc 仍可中断；(3) 两套 kernel 的 SSE 帧名字节相等，HTTP 层无需感知差异。

## 2. 包信息

- **包名**：`aiops`
- **所属模块**：`internal/manager/service/`
- **依赖方向**：被 HTTP handler 调用；依赖 `biz/aiops`（含 `agent`、`chatruntime`）、`model/aiops`、`internal/pkg/errs`、`internal/pkg/tenantctx`

## 3. 关键类型与接口

```go
type Kernel string  // "legacy" | "graph"
const (
    KernelLegacy Kernel = "legacy"
    KernelGraph  Kernel = "graph"
)

const (
    RoleAdmin  = "admin"
    RoleViewer = "viewer"
)

type Service struct {
    legacyAgent *agent.Agent
    runtime     RuntimeHandler
    kernel      Kernel
    sessions    biz.SessionRepo
    proposals   biz.MutatingProposalRepo
    usage       *biz.UsageUsecase
    log         *slog.Logger

    cancelMu sync.Mutex
    cancels  map[string]context.CancelFunc  // session_id -> in-flight turn cancel
}

type RuntimeHandler interface {
    Handle(ctx context.Context, req *chatruntime.Request) (*chatruntime.Reply, error)
}

type Caller struct {
    UserID uint64
    Role   string
}

type CreateSessionInput struct {
    Title             string
    Scope             []string
    RelatedIncidentID *uint64
    AgentID           string  // 绑定 chatruntime persona；空 = 用全局默认
}
```

`Caller.IsAdmin()` / `Caller.IsViewer()` 用于权限判定。`ParseKernel` 把字符串 env 值规范化为 `Kernel`，未识别值默认 `KernelLegacy`。

## 4. 关键函数与流程

### 构造函数

- **`New(...)`**：兼容入口，委托给 `NewWithKernel(..., KernelLegacy, nil, ...)`。
- **`NewWithKernel(...)`**：kernel 感知构造器；kernel=graph 但 runtime=nil 时，会在首个 PostMessage 处 Warn 后回退 legacy。
- **`SetMutatingProposalRepo(repo)`**：可选后置注入 ReviewGate 仓储；未注入时 HTTP 端返回 `ErrNotWiredYet`。

### 会话生命周期

- **`CreateSession(ctx, caller, in)`**：
  1. TrimSpace title，空则 `"Untitled"`；时间戳 UTC。
  2. `AgentID` 非空 → 写入 `sess.AgentID`（指针，便于区分"未设置"与"空字符串"）。
  3. `Scope` 非空 → `json.Marshal` 进 `ScopeJSON`。
  4. 调 `sessions.CreateSession`，失败 `%w`。
- **`GetSession(ctx, caller, sessionID)`**：先取 session；非 admin 且 `sess.UserID != caller.UserID` → `ErrNotFound`（隐藏存在性）。
- **`ListSessions` / `ListMessages` / `CloseSession` / `DeleteSession` / `RenameSession`**：均先 `GetSession` 校验所有权；RenameSession 拒绝空标题、截断 256 字符。

### `ListMutatingProposals`

- **签名**：`(ctx, caller Caller, f biz.MutatingProposalFilter) ([]*model.MutatingProposal, int64, error)`
- **职责**：admin-only 全局 ReviewGate 提案审计查询。
- **流程**：非 admin → `ErrForbidden`；proposals 未注入 → `ErrNotWiredYet`；trim `ToolName` / `Decision`；`Decision` 非空时白名单校验；limit 兜底 50（max 200），offset 兜底 0；分别调 `ListMutatingProposals` 与 `CountMutatingProposals`（后者把 limit/offset 清零）。

### PostMessage 家族（核心）

- **`PostMessage` / `PostMessageWithOpts` / `PostMessageStream` / `PostMessageStreamWithOpts`** 全部委托 `runWithKernel`，仅参数（`emit`、`opts`）不同。
- **`runWithKernel(ctx, caller, sessionID, content, emit, opts)`** 是 kernel 派发的单一咽喉：
  1. TrimSpace content；空 → `ErrInvalid`。
  2. `GetSession` 校验所有权。
  3. **HLD-021**：`ctx = context.WithoutCancel(ctx)` —— 把 turn 从 HTTP 请求生命周期剥离，避免浏览器刷新/SSE 断开杀死进行中的 turn（cloud_bash 等待人工审批时尤为关键）。
  4. 再 `context.WithCancel(ctx)` 套一层显式可取消 ctx；`registerCancel(sess.ID, cancel)` 注册到 `cancels` map。
  5. `defer unregisterCancel(sess.ID, cancel)` —— 仅当 map 中仍是自己的 cancel 时才删除（防止 newer turn 已替换 entry）。
  6. kernel=graph 且 runtime!=nil → `runGraph`；kernel=graph 但 runtime==nil → Warn 一次后回退 legacy；legacy 也 nil → `ErrNotWiredYet`。

### `StopSession`（显式中断）

- **签名**：`(ctx, caller Caller, sessionID string) (bool, error)`
- **职责**：响应用户 Esc 操作。
- **流程**：`GetSession` 校验所有权 → 加锁取出 `cancels[sessionID]` 并删除 → 调用 `cancel()`；未在运行返回 `(false, nil)`。
- **错误处理**：所有权失败直接返回 err；运行中 turn 被 cancel 后 graph 软失败、partial state 仍持久化。

### `sameCancel(a, b)`

- `context.CancelFunc` 不可 `==` 比较；用 `reflect.ValueOf(...).Pointer()` 比较函数指针身份。

### `runGraph(ctx, sess, content, emit, opts)`

- **职责**：把请求翻译到 `chatruntime.Request`、调 `runtime.Handle`、把 `chatruntime.Reply` 翻译回 `agent.Reply`，保持 HTTP DTO 不感知 kernel。
- **流程**：
  1. 若 `emit != nil`，包装一个 `graphEmit` 闭包，把 `chatruntime.Event` 经 `translateRuntimeEvent` 转成 `agent.Event` 再回调。
  2. `translateMentionsToRuntime(opts.Mentions)` —— 形状镜像，per-turn 一次分配。
  3. 从 `tenantctx.From(ctx)` 取 `role`（背景调度无 JWT 时为空，runtime 视为非 viewer）。
  4. 构造 `chatruntime.Request`（SessionID / UserID / Role / UserText / Mentions / Provider / Model / WebSearchEnabled / Locale / Emit）。
  5. `runtime.Handle(ctx, req)` 失败直接返回；成功经 `runtimeReplyToAgentReply` 翻译。

### 事件翻译函数

- **`translateRuntimeEvent(ev)`**：逐字段把 `chatruntime.Event` 镜像到 `agent.Event`（Assistant / Tool / Done / Notification / Approval）；SSE 帧名字节相等。
- **`translateMentionsToRuntime(in)`**：空切片返回 nil；否则一次 alloc 拷贝。
- **`runtimeReplyToAgentReply(r)`**：nil 安全；拷贝 Message/Usage/Iterations/ToolCalls。

### `UsageToday(ctx)`

- 返回 `usage.Today(ctx)` 的全局日 token 汇总；任何已认证 caller 可调用（权限由 HTTP handler 上游把关）。

## 5. 依赖关系

- **内部包**：`biz/aiops`、`biz/aiops/agent`、`biz/aiops/chatruntime`、`model/aiops`、`internal/pkg/errs`、`internal/pkg/tenantctx`
- **外部库**：`log/slog`、`sync`、`context`、`reflect`、`time`、`encoding/json`、`strings`
- **被调用方**：HTTP handler（`http.go`）、`cmd/ongrid/main.go`（构造 + 启动日志）

## 6. 并发与资源管理

- **`cancelMu`（sync.Mutex）**：保护 `cancels` map。每个 turn 进入时 `registerCancel`，若同 session 已有旧 turn 仍在册（不应发生 —— SPA 串行 turn），先 `prev()` 取消再覆盖。
- **turn ctx 解耦**：`context.WithoutCancel` + `context.WithCancel` 双层 —— 既不被 HTTP ctx 取消，又支持显式 stop。
- **unregister 防误删**：仅当 `sameCancel(cur, cancel)` 为真才 `delete`，避免 newer turn 或 StopSession 已替换/clear 后被旧 turn 的 defer 覆盖。
- **defer 资源释放**：`unregisterCancel` 在 `runWithKernel` return 前必执行。

## 7. 设计模式与亮点

- **双 kernel 并存**：legacy（agent.Agent for-loop）与 graph（chatruntime.Runtime）共存；`ONGRID_AGENT_KERNEL` opt-in，默认 legacy，灰度切换安全。
- **kernel-agnostic DTO**：两套 kernel 都产 `agent.Reply`，HTTP 层 `toPostMessageResp` 不感知来源。
- **HLD-021 turn 解耦**：用 `WithoutCancel` 解决"审批等待几分钟 + 浏览器刷新"的矛盾；显式 stop 通过 `cancels` map 单独提供。
- **存在性隐藏**：非所有者拿 `ErrNotFound` 而非 `ErrForbidden` —— 避免 IDOR 式会话枚举。
- **admin bypass 但用 owner 身份跑 agent**：service 层 re-read session 取 owning `user_id` 传给 agent，handler 不感知。
- **`SetMutatingProposalRepo` 后置注入**：解循环依赖 —— repo 在 cmd 构造晚于 service，可选注入 + `ErrNotWiredYet` fail-closed。
- **reflect 比较 CancelFunc**：因 Go 闭包不可 `==`，用 reflect pointer 身份比较。

## 8. 注意事项

- **默认 kernel = legacy**：生产切换到 graph 需显式 `ONGRID_AGENT_KERNEL=graph`；kernel=graph 但 runtime=nil 会 Warn 一次后回退，属 ops 误配信号。
- **turn 不会被 HTTP ctx 取消**：注释明示 per-tool timeout + eino max-steps 仍 bound 工作；SSE 写到死连接由 `writeSSE` 吞掉。
- **StopSession 仅 cancel 当前 turn**：不保证 turn 干净回滚 —— graph 软失败，partial state 仍持久化。
- **`registerCancel` 会取消同 session 旧 turn**：理论上 SPA 串行 turn 不会触发；防御性写法。
- **`ListMutatingProposals` 是 admin-only**：因包含全用户原始 tool arguments。
- **CreateSession 的 AgentID 是指针**：区分"未设置"与"空字符串 persona"语义。
- **Scope 写入 ScopeJSON**：注意 `json.Marshal` 失败返回 `ErrInvalid` 包装，不会丢错误。
- **`Kernel()` 方法暴露**：仅用于 `cmd/ongrid/main.go` 启动日志记录解析值。
