# send_message_tool.go

## 1. 概述

本文件实现 `SendMessage` 工具——通过发送 follow-up message 继续 worker。wire name "SendMessage" 匹配 claude-code 的 catalog，让 LLM 按名字选对工具。

是 coordinator-only 三件套（AgentTool / SendMessage / TaskStop）之一，用于 worker 继续 running or completed 状态。典型场景：初始结果接近但需要 refinement（"focus on the last 30 min"、"also check disk"）。

**NOT for**：fresh tasks（用 AgentTool）/ killed workers（spawn 新的）。

## 2. 包信息

- **包名**：`tools`
- **路径**：`internal/manager/biz/aiops/tools/send_message_tool.go`
- **导入**：
  - `basetool`
  - `log/slog` / `strings` / `errors`
- **Class**：`write`（side-effecting，改变 worker 状态）

## 3. 关键类型与接口

### `SendMessageTool`

```go
type SendMessageTool struct {
    spawner WorkerSpawner
    log     *slog.Logger
}
```

### `sendMessageArgs`

```go
type sendMessageArgs struct {
    To      string `json:"to"`      // worker task_id（"agent-<8 hex>"）
    Message string `json:"message"` // follow-up body，作为 worker 的新 user turn
}
```

### `sendMessageResult`

```go
type sendMessageResult struct {
    TaskID string `json:"task_id"`
    Status string `json:"status"`
    Result string `json:"result,omitempty"`
    Err    string `json:"error,omitempty"`
}
```

## 4. 关键函数与流程

### `NewSendMessageTool(spawner, log)`

`log == nil` → `slog.Default()`。

### `SendMessageToolName = "SendMessage"`

wire name（PascalCase，与 AgentTool / TaskStop 一致，匹配 claude-code catalog）。

### `sendMessageWhenToUse`（常量）

英文 LLM-facing 文案：

- 用途：Continue a running or completed worker by sending a follow-up message
- `to` = AgentTool 返回的 task_id
- 典型场景：initial result close but needs refinement（"focus on the last 30 min"、"also check disk"）
- **NOT for**：fresh tasks（用 AgentTool）/ killed workers（spawn 新的）

### `Info(_ context.Context)`

返回 `ToolInfo`：Name=`SendMessage`，Class=`write`，含 WhenToUse。

### `InvokableRun(ctx, argsJSON, opts ...)`

1. 校验 `spawner` 非 nil。
2. Unmarshal `sendMessageArgs`。
3. `To` trim 后空 → error。`Message` trim 后空 → error。
4. `spawner.SendToWorker(ctx, To, Message)`——发 follow-up。
5. `spawner.GetWorker(To)` 拿 worker 新状态。
   - `!ok || w == nil` → error "worker %q vanished after send"
6. 构造 `sendMessageResult{TaskID: w.ID, Status: w.Status, Result: w.Result, Err: w.Err}`，Marshal 返回。
7. `_ = opts`——opts 接受但忽略（与 QueryPromQLTool 同模式）。

## 5. 依赖关系

| 方向 | 依赖 | 说明 |
|------|------|------|
| 上游 | `basetool.BaseTool` / `ToolInfo` | 实现 BaseTool 接口 |
| 下游 | `WorkerSpawner.SendToWorker` / `GetWorker` | worker 状态管理 |
| 共享 | `WorkerSpawner` 接口（与 AgentTool / TaskStop 共享） | coordinator-only 三件套 |

## 6. 并发与资源管理

- **无 per-call timeout**：依赖外层 ctx——`SendToWorker` 可能阻塞等 worker 处理。
- 无共享状态，并发安全（依赖 `WorkerSpawner` 实现的线程安全）。
- `_ = opts` 显式忽略 InvokeOption。

## 7. 设计模式与亮点

- **wire name 匹配 claude-code catalog**：`SendMessage`（PascalCase）而非 `send_message`（snake_case）——让 LLM 按名字选对工具，匹配训练时记忆。与 AgentTool / TaskStop 一致。
- **"continue 而非 spawn" 语义**：WhenToUse 明示 NOT for fresh tasks / killed workers——引导 LLM 在 worker 还活着时用 SendMessage 继续，而不是无脑 spawn 新 worker。
- **返回 worker 新状态**：`sendMessageResult` 包含 `Status / Result / Err`，让 LLM 看到 follow-up 后的 terminal state——不必再调 GetWorker 查询。
- **"worker vanished after send" 防御**：`GetWorker` 失败时 error——worker 在 send 后被 GC 或 crash 的边缘场景。
- **`_ = opts` 显式忽略**：与 QueryPromQLTool 同模式，opts 是给装饰器链用的，工具本身不需要。
- **Class="write"**：改变 worker 状态（继续 running），是 side-effecting——走 ReviewGate decorator，但 reviewer 通常直接 approve（continue worker 风险低）。

## 8. 注意事项

- **无 per-call timeout**：`SendToWorker` 可能阻塞等 worker 处理 follow-up；若 worker 卡住，caller 无 timeout 会一直等。
- **`To` 是 task_id**：LLM 必须从 prior AgentTool 调用拿 task_id，若 LLM hallucinate task_id 会 `SendToWorker` 失败。
- **"worker vanished after send" 是边缘场景**：worker 在 send 后被 GC 或 crash——LLM 看到 error 应该 spawn 新 worker 而非重试。
- **Class="write" 走 ReviewGate**：continue worker 风险低，reviewer 通常 approve；但若 reviewer 配置过严会影响 LLM refinement 能力。
- **无 batch 协议**：一次 continue 一个 worker，多 worker continue 要 LLM 多次调用。
- **`Result` / `Err` 字段**：worker 可能还在 running（Status != terminal），此时 `Result` / `Err` 为空——LLM 要根据 Status 判断是否等结果。
- **与 AgentTool 的 task_id 一致性**：`To` 必须匹配 AgentTool 返回的 task_id 格式（"agent-<8 hex>"），否则 `SendToWorker` 找不到 worker。
