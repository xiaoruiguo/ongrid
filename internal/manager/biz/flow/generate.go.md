# `generate.go` 技术实现文档

> 源文件：`internal/manager/biz/flow/generate.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/flow`

## 1. 概述

本文件实现自然语言 → flow graph 生成器。把一行描述转成可运行工作流，使用 live tool catalog，让用户不必手放节点。`GenerateGraph` 调用 LLM 把 prompt 转成 `{name, description, graph}` JSON；best-effort 校验 graph 后返回 `CreateInput`。System prompt 含中文节点类型说明 + 可用工具列表 + 报告网页范式 + 示例。

## 2. 包信息

- **包名**：`flow`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 HTTP handler（generate-graph API）调用；依赖 `pkg/errs`、`encoding/json`、`strings`

## 3. 关键类型与接口

```go
type GenLLM interface {
    RunLLM(ctx, system, user string) (string, error)
}
```

## 4. 关键函数与流程

### `WithLLM`
- **签名**：`func (u *Usecase) WithLLM(l GenLLM) *Usecase`
- **职责**：注入生成 LLM；返回 Usecase 供链式构造
- **流程**：`u.llm = l; return u`

### `GenerateGraph`
- **签名**：`func (u *Usecase) GenerateGraph(ctx, prompt string) (CreateInput, error)`
- **职责**：把 prompt 转成 flow graph；返回 CreateInput ready for Create
- **流程**：
  1. llm nil → `ErrNotWiredYet "workflow generation not wired"`
  2. prompt TrimSpace；空 → `ErrInvalid "prompt required"`
  3. `llm.RunLLM(ctx, genSystemPrompt(u.ListTools()), prompt)` → out
  4. `stripCodeFences(out)` 去 ``` 围栏
  5. unmarshal `{name, description, graph}` → gen
  6. graph TrimSpace；空或 "null" → `ErrInvalid "model returned no graph"`
  7. `ParseGraph(graph)` 校验；失败 `ErrInvalid "generated graph invalid: %v"`
  8. name 空 → "AI 生成的工作流"
  9. 返回 `CreateInput{Name, Description, GraphJSON: graph}`
- **错误处理**：LLM 失败透传；JSON parse 失败 ErrInvalid；graph 校验失败 ErrInvalid

### `stripCodeFences`
- **签名**：`func stripCodeFences(s string) string`
- **职责**：去 leading/trailing ``` 围栏；trim 到 outermost JSON object
- **流程**：
  1. TrimSpace
  2. 前缀 "```" → 去第一行（含 ```）+ 去 trailing "```"
  3. TrimSpace
  4. 若 `{` 在 i>0 位置 → 截 `s[i : LastIndexByte('}')+1]`
- **关键设计**：模型可能加围栏/散文 despite 指令；robust 提取

### `genSystemPrompt`
- **签名**：`func genSystemPrompt(tools []ToolMeta) string`
- **职责**：构造 LLM system prompt
- **流程**：
  1. 写中文角色定义 + JSON 输出规则 + 图规则（nodes/edges 结构）
  2. 节点类型与 config 说明（trigger.manual/cron/alert_fired / tool / llm / agent / condition / notify / http_request / transform / set）
  3. **报告网页范式**：用户要"生成报告/网页/可视化"时用 serve_page 范式（llm 生成 HTML → tool serve_page）
  4. **可用工具列表**：遍历 tools 写 `- tool_name：desc（必填: ...）`
  5. 示例（"巡检设备1的负载，生成网页报告"）
  6. 收尾"只输出 JSON。工具名必须用上面列出的"
- **关键设计**：中文 prompt；含可用工具列表防模型编造不存在的 tool；含范式引导报告网页场景

### `requiredParams`
- **签名**：`func requiredParams(schema json.RawMessage) []string`
- **职责**：从 JSON Schema 提取 required 字段列表

### `oneLine`
- **签名**：`func oneLine(s string) string`
- **职责**：去换行；>80 rune 截断 + "…"

## 5. 依赖关系

- **内部包**：`pkg/errs`
- **外部库**：`context`、`encoding/json`、`fmt`、`strings`
- **桥接接口**：`GenLLM`（与 LLM 节点共用 LLMRunner；实际上 LLMRunner 也可满足 GenLLM，签名兼容）
- **被调用方**：HTTP handler（generate-graph API）
- **协作**：`Usecase.ListTools`（提供可用工具列表）

## 6. 并发与资源管理

- **无共享状态**：Usecase.llm 字段不可变
- **无锁**：LLM 调用无状态
- **ctx 透传**：RunLLM 接受 ctx

## 7. 设计模式与亮点

- **中文 system prompt**：模型按中文指令生成；节点 name/config 说明都是中文
- **可用工具列表防编造**：prompt 含 `- tool_name：desc（必填: ...）`；模型只能用列表中的 tool
- **报告网页范式**：用户要"生成报告/网页"时引导用 serve_page（llm 生成 HTML → tool serve_page）
- **stripCodeFences robust**：模型可能加围栏/散文；提取 outermost JSON object
- **ParseGraph 校验**：生成 graph 必须通过校验才返回；防模型产出非法结构
- **name 默认"AI 生成的工作流"**：模型未给 name 时兜底
- **示例引导**：prompt 含完整示例（"巡检设备1的负载，生成网页报告"）让模型模仿结构
- **oneLine 截断 80 rune**：工具描述过长时截断；保持 prompt 紧凑

## 8. 注意事项

- **LLM 必须接**：llm nil → ErrNotWiredYet；未配 LLM 时 generate-graph API 不可用
- **模型可能加围栏**：stripCodeFences 处理；但若模型输出纯散文无 `{` 则 unmarshal 失败 → ErrInvalid
- **graph 校验失败 → ErrInvalid**：模型产出非法结构（如 cycle / unknown type）会被 ParseGraph 拒绝
- **name 默认中文**：操作员可后续 UI 改
- **prompt 必须非空**：TrimSpace 后空 → ErrInvalid
- **可用工具列表来自 ListTools**：catalog 未接线时 tools 空；prompt 仍可用其他节点类型
- **system prompt 含示例**：模型可能过度模仿示例；操作员应写明确 prompt
- **oneLine 80 rune 截断**：工具描述被截断；模型可能看不到完整描述
