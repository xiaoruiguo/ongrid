# task_stop_tool.go

## 1. 概述

本文件实现 `TaskStop` 工具——取消 running worker。wire name "TaskStop" 匹配 claude-code 的 catalog，让 LLM 按名字选对工具。

是 coordinator-only 三件套（AgentTool / SendMessage / TaskStop）之一，用于 worker 跑偏时 mid-flight 取消。典型场景：worker 在同一失败工具上循环 / 追错假设 / 用时过多。

**后续**：stopped workers 在某些 flow 里仍可 SendMessage 继续，但典型 follow-up 是 fresh AgentTool spawn with corrected prompt。

## 2. 包信息

- **包名**：`tools`
- **路径**：`internal/manager/biz/aiops/tools/task_stop_tool.go`
- **导入**：
  - `basetool`
  - `log/slog` / `strings` / `errors`
- **Class**：`write`（side-effecting，改变 worker 状态）

## 3. 关键类型与接口

### `TaskStopTool`

```go
type TaskStopTool struct {
    spawner WorkerSpawner
    log     *slog.Logger
}
```

### `taskStopArgs`

```go
type taskStopArgs struct {
    TaskID string `json:"task_id"` // worker task_id（"agent-<8 hex>"）
}
```

### `taskStopResult`

```go
type taskStopResult struct {
    TaskID string `json:"task_id"`
    Status string `json:"status"`
}
```

## 4. 关键函数与流程

### `NewTaskStopTool(spawner, log)`

`log == nil` → `slog.Default()`。

### `TaskStopToolName = "TaskStop"`

wire name（PascalCase，与 AgentTool / SendMessage 一致，匹配 claude-code catalog）。

### `taskStopWhenToUse`（常量）

英文 LLM-facing 文案：

- 用途：Kill a running worker that's gone wrong / off-track
- 传 AgentTool 返回的 task_id
- 典型场景：mid-flight 意识到 approach 错了（worker 在同一失败工具上循环 / 追错假设 / 用时过多）
- 后续：stopped workers 在某些 flow 里仍可 SendMessage 继续，但典型 follow-up 是 fresh AgentTool spawn with corrected prompt

### `Info(_ context.Context)`

返回 `ToolInfo`：Name=`TaskStop`，Class=`write`，含 WhenToUse。

### `InvokableRun(ctx, argsJSON, opts ...)`

1. 校验 `spawner` 非 nil。
2. Unmarshal `taskStopArgs`。
3. `TaskID` trim 后空 → error。
4. `spawner.StopWorker(ctx, TaskID)`——取消 worker。
5. `spawner.GetWorker(TaskID)` 拿 worker 新状态。
   - **`!ok || w == nil`**：worker 已 gone——treat as success with synthetic status。Marshal `{task_id: TaskID, status: "killed"}` 返回。
6. Marshal `{task_id: w.ID, status: w.Status}` 返回。
7. `_ = opts`——opts 接受但忽略。

**Idempotent**：注释明示 "stopping an already-terminal worker is not an error"——多次 stop 同一 worker 不会报错。

## 5. 依赖关系

| 方向 | 依赖 | 说明 |
|------|------|------|
| 上游 | `basetool.BaseTool` / `ToolInfo` | 实现 BaseTool 接口 |
| 下游 | `WorkerSpawner.StopWorker` / `GetWorker` | worker 状态管理 |
| 共享 | `WorkerSpawner` 接口（与 AgentTool / SendMessage 共享） | coordinator-only 三件套 |

## 6. 并发与资源管理

- **无 per-call timeout**：依赖外层 ctx——`StopWorker` 通常快（cancel signal）。
- 无共享状态，并发安全（依赖 `WorkerSpawner` 实现的线程安全）。
- **Idempotent**：多次 stop 同一 worker 不报错。

## 7. 设计模式与亮点

- **wire name 匹配 claude-code catalog**：`TaskStop`（PascalCase）让 LLM 按名字选对工具，与 AgentTool / SendMessage 一致。
- **Idempotent 语义**：注释明示 "stopping an already-terminal worker is not an error"——LLM 可以放心 stop，不必先查询状态。
- **"worker gone" synthetic status**：`GetWorker` 失败时返回 `{status: "killed"}` 而非 error——worker 在 stop 后被 GC 是正常场景，LLM 看到 killed 状态即可。
- **"mid-flight 取消" 语义**：WhenToUse 明示用途是"意识到 approach 错了"——引导 LLM 在 worker 跑偏时主动取消，而不是等 timeout。
- **后续引导**：明示 "typical follow-up is a fresh AgentTool spawn with corrected prompt"——引导 LLM stop 后 spawn 新 worker 而非重试 stop。
- **`_ = opts` 显式忽略**：与 SendMessage 同模式，opts 是给装饰器链用的。
- **Class="write"**：改变 worker 状态（cancel），是 side-effecting——走 ReviewGate decorator，但 reviewer 通常直接 approve（stop worker 风险低）。

## 8. 注意事项

- **无 per-call timeout**：`StopWorker` 通常快（cancel signal），但若 worker 在 unkillable 状态（如 stuck syscall）可能阻塞。
- **`TaskID` 是 task_id**：LLM 必须从 prior AgentTool 调用拿 task_id，若 LLM hallucinate task_id 会 `StopWorker` 失败（但 idempotent 语义下不报错）。
- **Idempotent 可能掩盖错误**：stop 不存在的 worker 不报错——LLM 误以为成功，但实际 worker 可能从未存在。审计时要注意。
- **`GetWorker` 失败的 synthetic status**：worker gone 时返回 `{status: "killed"}`，LLM 无法区分"刚 stop 成功"和"worker 早已 gone"——这种语义模糊在某些场景可能误导 LLM。
- **Class="write" 走 ReviewGate**：stop worker 风险低，reviewer 通常 approve；但若 reviewer 配置过严会影响 LLM 自我纠错能力。
- **与 SendMessage 的 task_id 一致性**：`TaskID` 必须匹配 AgentTool 返回的 task_id 格式（"agent-<8 hex>"）。
- **无 batch 协议**：一次 stop 一个 worker，多 worker stop 要 LLM 多次调用。
- **"某些 flow 里仍可 SendMessage 继续"**：注释明示这是 flow-dependent，并非所有 stopped worker 都能 continue——LLM 不应假设 stop 后一定能 continue。
