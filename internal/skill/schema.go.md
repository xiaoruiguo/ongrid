# `schema.go` 技术实现文档

> 源文件：`internal/skill/schema.go`
> 包路径：`github.com/ongridio/ongrid/internal/skill`

## 1. 概述

`schema.go` 负责把 skill 的声明式参数 schema（`ParamSchema`）转换为 OpenAI function-calling 兼容的 JSON Schema。同时提供 `BuildSchema` 入口，优先使用 skill 自带的原始 schema（`RawSchemaProvider`），否则按 `Metadata.Params` 自动派生。这是 LLM 工具注册、HTTP API 校验、UI 表单生成的共同底座。

## 2. 包信息

- **包名**：`skill`
- **所属模块**：`internal/skill`（框架核心层）
- **依赖方向**：被 `internal/manager`（LLM 工具注册表）、`internal/edgeagent`（HTTP 路由）等调用 `BuildSchema`；依赖同包 `Executor`、`Metadata`、`ParamSchema`、`RawSchemaProvider`（types.go）

## 3. 关键类型与接口

本文件未定义新类型，仅使用 `types.go` 中已有的：

```go
type ParamSchema []ParamDef   // 有序参数表
type RawSchemaProvider interface { JSONSchema() json.RawMessage } // 可选扩展接口
```

## 4. 关键函数与流程

### `BuildSchema`
- **签名**：`func BuildSchema(e Executor) (json.RawMessage, error)`
- **职责**：返回某个 Executor 的参数 JSON Schema。
- **流程**：
  1. 类型断言 `e` 是否实现 `RawSchemaProvider`；若实现且返回非空 raw schema，直接返回（作者自带 schema 优先）；
  2. 否则调用 `e.Metadata().Params.ToJSONSchema()` 派生，并 `json.Marshal` 为 `json.RawMessage`。
- **错误处理**：仅在 `json.Marshal` 派生 schema 时可能返回编码错误；RawSchemaProvider 路径不产生 error。
- **设计意图**：调用方拿到 raw bytes 后可 `json.Unmarshal` 进 map 注入额外字段（例如 `skill_bridge` 给 edge-scoped skill 注入 `device_id`）。

### `ParamSchema.ToJSONSchema`
- **签名**：`func (s ParamSchema) ToJSONSchema() map[string]any`
- **职责**：把声明式参数表转换为 OpenAI function-calling 期望的 JSON Schema 对象（`{type:"object", properties:{...}, required:[...]}`）。
- **流程**：遍历 `s`，对每个 `ParamDef`：
  - 构造 entry，先放 `description`；
  - 按 `p.Type` 映射 JSON Schema `type`：
    - `string`/`duration` → `"string"`（duration 由调用方用 `Duration.String()` 序列化）
    - `int` → `"integer"`、`float` → `"number"`、`bool` → `"boolean"`
    - `enum` → `"string"` 并附 `enum` 数组
    - `array` → `"array"`，按 `p.ItemType` 递归映射 `items`（不允许嵌套数组）
    - 默认 → `"string"`
  - `p.Default != nil` 时附 `default`；
  - `p.Required` 时把 name 加入 `required` 列表。
- **输出**：`{type:"object", properties:props, required?:required}`，`required` 仅在非空时出现。
- **错误处理**：无显式 error；非法 Type 已由 `Metadata.Validate()` 在注册阶段拦截，此处走 default 兜底为 string。

## 5. 依赖关系

- **内部包**：同包 `Executor`、`Metadata`、`ParamSchema`、`RawSchemaProvider`
- **外部库**：`encoding/json`
- **被调用方**：`internal/manager/biz/aiops`（LLM 工具注册）、`internal/edgeagent`（HTTP API schema 暴露）、`internal/skill` 自身（subprocess skill 的 Schema 字段流转）

## 6. 并发与资源管理

无并发控制。`ToJSONSchema` 是纯函数式转换，`BuildSchema` 仅读取 `Metadata()` 与 `JSONSchema()`，无共享可变状态。

## 7. 设计模式与亮点

- **优先级覆盖模式**：`BuildSchema` 用类型断言实现"作者自带 schema 优先，否则框架派生"，让复杂 schema（嵌套对象、oneOf、数组 of 对象）能通过实现 `RawSchemaProvider` 表达，简单场景仍走声明式 `ParamSchema`。
- **OpenAI function-calling 兼容**：输出形状（`type/properties/required`）与 `internal/pkg/llm.ToolSchema` 严格对齐，是 LLM 工具链的契约边界。
- **duration → string 的隐式约定**：调用方需用 `time.Duration.String()` 序列化，文档显式说明，避免反序列化歧义。
- **array items 递归但禁止嵌套**：`ItemType` 只支持标量，覆盖 N+15 批量场景（`device_ids:[1,2]`、`paths:["/var"]`），同时避免 LLM 误用嵌套数组。

## 8. 注意事项

- **`BuildSchema` 不校验 raw schema 形状**：`RawSchemaProvider` 返回的 JSON 必须是合法的 JSON Schema 对象（top-level `type:"object"` + `properties` + `required`），框架不二次校验，作者自负其责。
- **`ToJSONSchema` 默认兜底 string**：若 `Metadata.Validate()` 未拦截到非法 Type（未来新增 Type 时遗漏），会静默降级为 string，可能掩盖 bug。
- **`required` 仅在非空时写入**：调用方不应假设返回 map 必含 `required` 键。
- **`default` 字段直接透传 `any`**：JSON 编码时需保证 `Default` 是 JSON 可序列化类型（string/int/float/bool），传 Go 函数/通道会编码失败。
- **输出 `map[string]any` 而非具体 struct**：便于调用方注入额外字段，但牺牲了部分类型安全；JSON 编码时 key 顺序不保证（Go map 迭代无序）。
