# `config_confirm.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/chatruntime/config_confirm.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/chatruntime`

## 1. 概述

本文件实现 `tryApplyConfirmedConfigDraft` —— 用户在 chat 中确认应用告警规则草案的快捷路径。当用户输入"ok/确认/apply"等确认词时，绕过 LLM ReAct 循环，直接调用 `apply_config_change` 工具应用最近的 config_draft。流程：(1) 解析确认消息提取 draft_hash + payload；(2) 失败则回退到历史消息查找最近 config_draft；(3) 调用 apply_config_change 工具；(4) 持久化 + SSE 推送结果。包含确认词识别、JSON payload 提取（fenced code block / balanced brace）、config_draft 历史扫描等辅助。

## 2. 包信息

- **包名**：`chatruntime`
- **所属模块**：`internal/manager/biz/aiops/chatruntime`
- **依赖方向**：被 `runtime.go` 的 `Handle` 主流程调用；依赖 `tools/basetool`（`WithUserID`/`WithUserText`）、`aiopsmodel`（`Session`/`Message`/`ToolCall`/`RoleAssistant`/`RoleUser`）

## 3. 关键类型与接口

```go
const applyConfigChangeToolName = "apply_config_change"

var confirmDraftHashRE = regexp.MustCompile(
    `(?i)\bdraft_hash\b\s*[:=]\s*"?\s*(sha256:[a-f0-9]{64})\s*"?`,
)
```

复用 `runtime.go` 的 `Runtime`/`Request`/`Reply`/`Emit`/`Event`/`EventToolStart`/`EventToolEnd`/`EventAssistant`/`EventDone`/`ToolEvent`/`AssistantEvent`。

## 4. 关键函数与流程

### `tryApplyConfirmedConfigDraft`
- **签名**：`func (rt *Runtime) tryApplyConfirmedConfigDraft(ctx, req, sess, history, emit) (*Reply, bool)`
- **职责**：尝试将用户确认消息转为直接 apply_config_change 调用
- **流程**：
  1. req/sess nil → (nil, false)
  2. `parseConfirmedConfigDraftApplyArgs(req.UserText)` 提取 args：
     - 失败但 `looksLikeConfigDraftConfirmation` true → `latestConfigDraftApplyArgs` 从历史找
     - 仍失败 → (nil, false)（不走快捷路径，交给 LLM）
  3. parseErr 非空 → `persistAndEmitDirectAssistant` 报错
  4. `findToolByName(ctx, rt.cfg.ToolBag, "apply_config_change")` → 未找到 → 报错
  5. 生成 callID（`direct_<uuid>`）+ emit EventToolStart
  6. `tool.InvokableRun(ctx, argsJSON, WithUserID, WithUserText)`
  7. emit EventToolEnd（success/error）
  8. runErr 非空 → 报错
  9. `formatConfigApplyResultMessage(result)` 格式化成功消息 → persistAndEmitDirectAssistant
  10. 返回 (Reply, true)（已处理）

### `persistAndEmitDirectAssistant`
- **签名**：`func (rt *Runtime) persistAndEmitDirectAssistant(ctx, sessionID, emit, content, toolCalls...) *Reply`
- **职责**：持久化 assistant 消息 + SSE 推送（Assistant + Done）
- **流程**：
  1. emit nil → no-op emit
  2. 构造 Message{Role: Assistant, Content: &content}
  3. `rt.cfg.Sessions.AppendMessage(ctx, msg)` 持久化；失败 Warn（不 fail）
  4. emit EventAssistant
  5. 构造 Reply{Message, Iterations: 1, ToolCalls}
  6. emit EventDone
  7. 返回 Reply

### `parseConfirmedConfigDraftApplyArgs`
- **签名**：`func parseConfirmedConfigDraftApplyArgs(text string) (string, error, bool)`
- **职责**：从确认消息提取 apply_config_change 参数
- **流程**：
  1. lower 含 "apply_config_change" + "draft_hash" + "payload" → 继续；否则 (nil, nil, false)
  2. `confirmDraftHashRE` 匹配 draft_hash（`sha256:<64 hex>`）；失败 → error "缺少有效 draft_hash"
  3. `extractPayloadJSON` 提取 payload JSON
  4. 构造 args struct{Domain: alert_rule, Action: create, Confirmed: true, DraftHash, Payload, ConfirmationText}
  5. json.Marshal → 返回

### `latestConfigDraftApplyArgs`
- **签名**：`func latestConfigDraftApplyArgs(history, confirmationText) (string, error, bool)`
- **职责**：从历史消息扫描最近的 config_draft
- **流程**：
  1. 从后往前遍历 history
  2. 跳过当前用户消息（第一个遇到的 RoleUser）
  3. 遇到第二个 RoleUser → 停止（跨对话回合）
  4. 对 assistant 消息：
     - Content 非空 → `configDraftApplyArgsFromToolResult`
     - 遍历 ToolCalls（从后往前）→ ResultJSON → `configDraftApplyArgsFromToolResult`
     - 遇到 `isConfigApplyResult` → 停止（已 apply 过）

### `configDraftApplyArgsFromToolResult`
- **签名**：`func configDraftApplyArgsFromToolResult(result, confirmationText) (string, error, bool)`
- **职责**：从 tool result JSON 提取 config_draft 参数
- **流程**：
  1. 非 `{` 开头 → false
  2. unmarshal struct{Kind, Domain, Action, DraftHash, Payload}
  3. Kind != "config_draft" → false
  4. DraftHash 空 → error "缺少 draft_hash"
  5. Payload 无效 → error "payload 无效"
  6. 构造 args（Domain 空 → alert_rule，Action 空 → create）

### `looksLikeConfigDraftConfirmation`
- **签名**：`func looksLikeConfigDraftConfirmation(text string) bool`
- **职责**：识别确认词
- **流程**：
  1. lowercase + trim + 去标点（。.!！?？）
  2. 空 或 >40 rune → false
  3. 含"取消/不要/别/no/cancel" → false
  4. 精确匹配：ok/okay/yes/y/confirm/confirmed/approve/approved/apply/好/好的/可以/行/嗯/是/确认/确认创建/确认应用/应用/创建/同意/通过 → true
  5. 含"确认/同意" + 含"创建/应用/生效" → true
  6. default → false

### `extractPayloadJSON`
- **签名**：`func extractPayloadJSON(text string) (json.RawMessage, error)`
- **职责**：从确认消息提取 payload JSON
- **流程**：
  1. 找 "payload" 位置
  2. 优先 `extractFencedJSON`（```json ... ```）
  3. 否则找首个 `{` → `extractBalancedJSONObject`

### `extractFencedJSON`
- **职责**：提取 ```json ... ``` 代码块内容

### `extractBalancedJSONObject`
- **职责**：提取平衡 `{...}` JSON 对象（处理字符串/转义/嵌套）

### `validateRawJSONObject`
- **职责**：验证是有效 JSON 对象 + 非空

### `findToolByName`
- **签名**：`func findToolByName(ctx, tools, name) (basetool.BaseTool, error)`
- **职责**：按 name 查找 tool

### `formatConfigApplyResultMessage`
- **签名**：`func formatConfigApplyResultMessage(result string) string`
- **职责**：格式化 apply 结果为人类可读消息
- **流程**：unmarshal struct{Kind, Status, ResourceID, Resource{Name,Type}, Message}；ResourceID>0 → "已确认并创建告警规则：<name>（ID: <id>, 状态: <status>）"；否则 "已确认并创建告警规则：<name>（状态: <status>）"

### `isConfigApplyResult`
- **职责**：判断 result 是否是 config_apply_result（已 apply 过的标记）

## 5. 依赖关系

- **内部包**：
  - `internal/manager/biz/aiops/tools/basetool`（`WithUserID`、`WithUserText`、`BaseTool`）
  - `internal/manager/model/aiops`（`Session`、`Message`、`ToolCall`、`RoleAssistant`、`RoleUser`）
- **外部库**：`github.com/google/uuid`、标准库 `context`、`encoding/json`、`fmt`、`log/slog`、`regexp`、`strings`、`time`

## 6. 并发与资源管理

- **无锁**：本文件方法无共享状态（Runtime 的锁由 runtime.go 管理）
- **ctx 透传**：InvokableRun 接受 ctx
- **UUID 生成**：`uuid.NewString()` 生成 callID，线程安全
- **正则包级常量**：`confirmDraftHashRE` 编译一次

## 7. 设计模式与亮点

- **快捷路径绕过 LLM**：用户确认词直接触发 apply_config_change，省一轮 LLM ReAct 循环，降低延迟和 token 消耗
- **双路径参数提取**：先从确认消息提取（draft_hash + payload 内联）；失败则回退历史扫描最近 config_draft
- **确认词多语言识别**：中英文 + 标点 trim + 否定词排除（"取消/不要/别/no"）
- **fenced JSON 优先**：payload 提取优先识别 ```json 代码块，其次平衡括号
- **平衡括号提取**：`extractBalancedJSONObject` 正确处理字符串/转义/嵌套
- **历史扫描防重复 apply**：遇到 `isConfigApplyResult` 停止，避免对已 apply 的 draft 再次操作
- **SSE 事件完整**：ToolStart + ToolEnd + Assistant + Done，UI 可实时反馈
- **持久化失败不 fail**：AppendMessage 失败仅 Warn，仍 emit + 返回 Reply（用户优先于审计）
- **callID 前缀 `direct_`**：标识这是快捷路径直接调用，非 LLM tool_call

## 8. 注意事项

- **`looksLikeConfigDraftConfirmation` 长度限制 40 rune**：过长文本不视为确认，避免误触发
- **`confirmDraftHashRE` 严格要求 `sha256:` 前缀**：draft_hash 必须是 `sha256:<64hex>` 格式
- **`latestConfigDraftApplyArgs` 跨回合停止**：遇到第二个 RoleUser 停止，避免跨对话回合误 apply
- **`extractBalancedJSONObject` 不处理注释**：JSON 标准不支持注释，payload 不应有注释
- **`formatConfigApplyResultMessage` unmarshal 失败回退默认**：返回 "已确认并应用配置变更。"
- **`tryApplyConfirmedConfigDraft` 返回 bool**：true 表示已处理（Handle 不再走 LLM）；false 表示未处理（Handle 继续 LLM 循环）
- **`persistAndEmitDirectAssistant` 持久化失败仅 Warn**：不 fail 请求，但消息可能丢失；高可靠性场景需重试机制
- **`findToolByName` 线性扫描**：tool 数量通常 <50，无需索引
