# `resolve.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/toolreplay/resolve.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/toolreplay`

## 1. 概述

本文件解决历史回放时 assistant tool_calls 与 role=tool 消息的 LLM call id 配对问题：`Resolve` 为每个 assistant 消息产出 `{id, name, args}` 三元组（3 级优先级：llm_call_id 列 > pair-by-order > 丢弃）；`IndexToolMessagesByCallID` 建 tool_call_id → index 索引供 HOIST 重排；`MarkDependentToolsForSkip` / `MarkAllFollowingToolsForSkip` 标记跳过避免 orphan tool 污染。是 strict providers（DeepSeek v4+）"tool must follow tool_calls" 400 错误的根因修复层。

## 2. 包信息

- **包名**：`toolreplay`
- **所属模块**：`internal/manager/biz/aiops/toolreplay`
- **依赖方向**：被 chatruntime.buildEinoHistory / legacy agent.buildMessages 调用；仅依赖 aiopsmodel.Message

## 3. 关键类型与接口

```go
type ResolvedToolCall struct {
    ID       string  // LLM-assigned tool_call_id
    Name     string  // 工具名
    ArgsJSON string  // 原始 JSON（不 re-marshal）
}
```

无接口；全部是纯函数。

## 4. 关键函数与流程

### `Resolve`（核心）
- **签名**：`func Resolve(history []*aiopsmodel.Message) map[string][]ResolvedToolCall`
- **职责**：为每个 assistant 消息产出应 emit 的 tool_calls 三元组列表
- **3 级优先级**：
  1. **`Message.ToolCalls[i].LLMCallID` 非空**（post-fix 行，chat_tool_calls.llm_call_id 列已写）→ 直接用
  2. **pair-by-order**（back-compat，pre-fix 行 llm_call_id 为 NULL）→ 与后续 role=tool 消息按顺序配对，用 tool 行的 tool_call_id
  3. **omit**（保守 MVP）→ 不写入 result map，caller 丢弃 assistant + dependent tools
- **pair-by-order 流程**：
  1. 标记 `needsFallback = true`
  2. 从 `i+1` 起扫，遇到 role=tool 则与 `calls[toolIdx]` 配对
  3. **mismatched tool name 中止**：`*next.ToolName != calls[toolIdx].Name` → `ok = false`，整体丢弃（宁可丢两边也不发错配 tool 结果）
  4. 遇 role=assistant / role=user → break（跨 turn 边界）
  5. 配对完成后任一 call.ID 仍空 → `ok = false`，整体丢弃
- **返回**：`map[assistantMsgID][]ResolvedToolCall`

### `MarkDependentToolsForSkip`
- **签名**：`func MarkDependentToolsForSkip(history, assistantIdx, n int, skip map[int]bool)`
- **职责**：标记 assistantIdx 之后的 n 个 role=tool 消息为跳过
- **流程**：
  1. 从 `assistantIdx + 1` 起扫
  2. role=tool → `skip[j] = true` + count++
  3. 遇 role=assistant / role=user → return（跨 turn 边界）
  4. count == n → return
- **用途**：assistant 被丢弃时，其 dependent tools 也必须跳过，避免 orphan tool 污染 LLM

### `IndexToolMessagesByCallID`
- **签名**：`func IndexToolMessagesByCallID(history) map[string]int`
- **职责**：建 tool_call_id → history index 索引，供 HOIST 重排
- **流程**：
  1. 遍历 history
  2. role=tool + `ToolCallID` 非空 → `out[*ToolCallID] = i`
  3. **first-write-wins**：重复 id 防御性跳过
- **用途**：buildEinoHistory 中 `emitToolByCallID(callID)` 用此索引 O(1) 查 tool 行，HOIST 到 parent assistant 之后——防 created_at 顺序错乱（长运行 AgentTool 子智能体后完成）导致 DeepSeek 400

### `MarkAllFollowingToolsForSkip`
- **签名**：`func MarkAllFollowingToolsForSkip(history, assistantIdx int, skip map[int]bool)`
- **职责**：标记 assistantIdx 之后所有连续 role=tool 消息为跳过（污染数据兜底）
- **流程**：
  1. 从 `assistantIdx + 1` 起扫
  2. role=tool → `skip[j] = true`
  3. 遇 role=assistant / role=user / role=system → return
- **用途**：assistant content=NULL 且无 hydrated ToolCalls（chat_tool_calls 从未写入）时，丢弃整个 tool-call 组——避免发 dangling tool 消息被 strict providers 拒绝

## 5. 依赖关系

- **内部包**：`manager/model/aiops`（Message / RoleAssistant / RoleTool / RoleUser / RoleSystem）
- **被调用方**：chatruntime.buildEinoHistory（Resolve + IndexToolMessagesByCallID + MarkDependentToolsForSkip + MarkAllFollowingToolsForSkip）、legacy agent.buildMessages（Resolve）

## 6. 并发与资源管理

- **无并发状态**：全部纯函数，无共享可变状态
- **输入只读**：history 切片与 skip map 都不修改 history 本身；skip map 由 caller 持有并传入
- **map 分配**：Resolve 返回新 map，IndexToolMessagesByCallID 返回新 map——caller 各自持有独立副本

## 7. 设计模式与亮点

- **3 级优先级降级**：llm_call_id 列 > pair-by-order > 丢弃——post-fix 行用列，pre-fix 行 back-compat 配对，无法配对的保守丢弃
- **pair-by-order mismatch 中止**：tool name 不匹配立即整体丢弃——注释明示"宁可丢两边也不发错配 tool 结果"
- **first-write-wins 防重复 id**：IndexToolMessagesByCallID 对重复 tool_call_id 防御性跳过——防数据异常
- **HOIST 索引**：IndexToolMessagesByCallID 让 buildEinoHistory O(1) 查 tool 行，实现"tool response 立即 hoist 到 parent assistant 之后"——created_at 顺序错乱的根因修复
- **MarkDependentToolsForSkip 跨 turn 边界**：遇 role=assistant/user 立即 return——不误标记下一 turn 的 tool
- **MarkAllFollowingToolsForSkip 污染数据兜底**：content=NULL 且无 ToolCalls 的 assistant 整组丢弃——防 dangling tool
- **ArgsJSON 原始 JSON**：注释明示"emit as raw JSON, not re-marshalled, so float precision / key order match"——保 provider repeat-content safety
- **保守 MVP**：无法配对的 assistant + tools 全丢，不发半截 envelope——strict providers 不会 400
- **纯函数**：无副作用，可独立测试
- **双 caller 共享**：legacy agent.buildMessages（返回 llm.Message）+ graph chatruntime.buildEinoHistory（返回 schema.Message）消费同一 map——契约层统一

## 8. 注意事项

- **`Resolve` 返回 map key 是 assistant m.ID**：caller 用 `callIDs[m.ID]` 查找，m.ID 必须非空
- **pair-by-order 跨 turn 边界**：遇 role=assistant/user break，不跨 turn 配对——若 tool 响应跨 turn 则无法配对
- **`MarkDependentToolsForSkip` 修改 caller 的 skip map**：caller 传入 map，函数写入——非线程安全（但 buildEinoHistory 单线程调用）
- **`IndexToolMessagesByCallID` orphan-pattern 不索引**：`ToolCallID == nil` 的 tool 行不进 map——这些是 orphan，HOIST 不到
- **`MarkAllFollowingToolsForSkip` 含 role=system 边界**：遇 system 也 return，防跨 system 块误标记
- **ArgsJSON 不 re-marshal**：若原始 JSON 有格式问题（如重复 key），replay 时原样发出——provider 可能拒绝，但保持原貌
- **无 LLM 调用**：纯本地解析，无网络/IO
- **依赖 aiopsmodel.Message 字段**：`ToolCalls[i].LLMCallID`（*string）、`ToolCallID`（*string）、`ToolName`（*string）——repo 层必须正确 hydrate 这些字段
- **pre-fix 行 back-compat**：llm_call_id 列为 NULL 的旧行靠 pair-by-order，但若 history 顺序被打乱（如并行写入）pair-by-order 可能错配——mismatch 检测是最后防线
- **HOIST 场景**：长运行 AgentTool 子智能体后完成，其 tool response 在 created_at 顺序上排在后续 assistant 之后——IndexToolMessagesByCallID + HOIST 把它提到 parent 之后，满足 strict provider "tool must immediately follow tool_calls"
