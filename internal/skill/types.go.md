# `types.go` 技术实现文档

> 源文件：`internal/skill/types.go`
> 包路径：`github.com/ongridio/ongrid/internal/skill`

## 1. 概述

`types.go` 是 ongrid L2 设备能力框架的核心类型定义文件，定义了 `Executor` 接口、`Metadata` 元数据、`Param`/`ParamSchema` 参数表、`Class`/`Scope` 分类枚举等所有 skill 实现与调度的基础类型。整个 skill 框架的"权限分级 + 自动派生 LLM 工具/HTTP/UI"设计哲学在此文件中定型。

## 2. 包信息

- **包名**：`skill`
- **所属模块**：`internal/skill`（框架核心层）
- **依赖方向**：被所有 builtin skill 实现依赖；被 `internal/manager`、`internal/edgeagent`、`cmd/*` 调度层依赖；本文件不依赖任何业务包

## 3. 关键类型与接口

```go
// 权限分级，零值为 safe（最宽松）
type Class string
const (
    ClassSafe      Class = "safe"      // 只读无副作用
    ClassMutating  Class = "mutating"  // 修改 edge 状态但可逆
    ClassDangerous Class = "dangerous" // 不可逆/集群影响
)

// 执行位置，零值为 ScopeHost（历史默认）
type Scope string
const (
    ScopeHost    Scope = "host"    // edge 执行，需 edge_id
    ScopeManager Scope = "manager" // manager 进程内执行
)

// 单个参数声明
type Param struct {
    Type     string   // string|int|float|bool|duration|enum|array
    Required bool
    Default  any
    Desc     string
    Enum     []string
    ItemType string   // Type=array 时的元素类型
}

// 有序参数表（slice 而非 map，保留 UI 渲染顺序）
type ParamSchema []ParamDef

type ParamDef struct {
    Name string
    Param
}

// 框架自动装配所需的全部元数据
type Metadata struct {
    Key           string      // 稳定 ID，lower_snake
    Name          string      // UI 标签
    Description   string      // 人 + LLM 共用的描述
    Class         Class
    Scope         Scope
    Category      string      // UI 分组（network/filesystem/process/...）
    Params        ParamSchema
    ResultPreview string      // 结果形状提示
}

// 可选接口：作者自带原始 JSON Schema 时实现
type RawSchemaProvider interface {
    JSONSchema() json.RawMessage
}

// skill 实现契约
type Executor interface {
    Metadata() Metadata
    Execute(ctx context.Context, params json.RawMessage) (json.RawMessage, error)
}
```

## 4. 关键函数与流程

### `Metadata.Validate`
- **签名**：`func (m Metadata) Validate() error`
- **职责**：校验元数据内部一致性，在 `Register` 时调用，让作者期错误在 boot 暴露。
- **流程**：
  1. `Key` 非空 + `validKey`（lower_snake `[a-z0-9_]`）；
  2. `Name`、`Description` 非空；
  3. `Class` ∈ `{""，ClassSafe，ClassMutating，ClassDangerous}`；
  4. `Scope` ∈ `{""，ScopeHost，ScopeManager}`；
  5. 遍历 `Params`：
     - `Name` 非空；
     - `Type` 合法；`Type=array` 时 `ItemType` 必填且合法（禁止嵌套数组）；
     - `Required && Default != nil` 互斥；
  6. 任一失败返回带上下文的 `error`。
- **错误处理**：返回 `error`，由 `Register` 决定是否 panic。

### `Metadata.EffectiveClass`
- **签名**：`func (m Metadata) EffectiveClass() Class`
- **职责**：返回 Class，零值降级为 `ClassSafe`。
- **设计意图**：作者忘填 Class 时默认安全（最宽松），但框架在注册时会 log 警告，不让此降级静默。

### `Metadata.EffectiveScope`
- **签名**：`func (m Metadata) EffectiveScope() Scope`
- **职责**：返回 Scope，零值降级为 `ScopeHost`。
- **设计意图**：保留历史兼容——Scope 字段出现前所有 skill 默认跑 edge。

### `validKey`
- **签名**：`func validKey(s string) bool`
- **职责**：校验字符串是否为合法的 lower_snake key（`[a-z0-9_]`，非空）。
- **流程**：遍历字符，仅允许 `a-z`、`0-9`、`_`，其他返回 false。

## 5. 依赖关系

- **内部包**：无（仅标准库）
- **外部库**：`context`、`encoding/json`、`errors`
- **被调用方**：所有 `internal/skill/builtin/*` 子包；`registry.go`（Register/AllByClass）；`schema.go`（BuildSchema/ToJSONSchema）；`loader.go`（buildSubprocessSkill）；`subprocess.go`（Metadata/Validate）

## 6. 并发与资源管理

无并发控制。本文件仅定义类型与纯校验函数，无共享可变状态。`Metadata` 是值类型，复制传递天然安全。

## 7. 设计模式与亮点

- **声明式元数据驱动**：作者只需写 `Metadata` + `Execute`，框架自动派生 LLM 工具注册、HTTP API、UI 表单、权限门、审计日志——"一个文件 + 一次 Register"即可上线新 skill。
- **三层权限分级（"代差"设计）**：safe / mutating / dangerous 对应"AI 直调 / 人工审批 / RSA SOP + 双签"，框架只标记 Class，具体策略由 manager 服务层实现，解耦清晰。
- **Scope 二分**：host（edge 执行）vs manager（进程内执行），让 web_search 这类云侧工具与 probe_tcp 这类设备侧工具共用同一框架。
- **零值降级到最安全/最历史**：Class 零值=safe，Scope 零值=host，兼顾"作者忘填时安全"与"旧 skill 兼容"。
- **ParamSchema 用 slice 保序**：UI 渲染顺序与作者声明顺序一致，比 `map[string]Param` 更友好。
- **RawSchemaProvider 扩展点**：声明式 ParamSchema 表达不了的复杂 schema（嵌套对象、oneOf）通过可选接口实现，简单场景不污染主路径。

## 8. 注意事项

- **`Class` 零值降级 safe 是双刃剑**：作者忘填时默认安全，但若实际有副作用的 skill 忘填 Class，会被 LLM 误判为可直调——`Register` 时的 warning log 是唯一防线，运维应监控该日志。
- **`Scope` 零值降级 host**：新写的 manager 侧 skill 必须显式设 `ScopeManager`，否则框架会要求 `edge_id` 并通过 tunnel 派发，导致调度失败。
- **`Param.Default` 为 `any`**：JSON 编码时需保证是 JSON 可序列化类型；`ToJSONSchema` 透传到 schema 的 `default` 字段，LLM 可见。
- **`ParamSchema` 禁止嵌套数组**：`ItemType` 仅支持标量，复杂结构需实现 `RawSchemaProvider`。
- **`Execute` 必须防御性 unmarshal**：文档明确"params 已被调用方按 schema 校验，但 Execute 仍需视为不可信"——schema 只校验形状不校验语义（如 host 是否可路由）。
- **`Metadata.Key` 是稳定 ID**：用于 audit log、HTTP 路由、LLM tool name、dedupe key，一旦发布不应变更，否则破坏兼容。
- **`validKey` 限制为 lower_snake**：与 LLM tool name 命名规范（OpenAI function-calling 推荐 snake_case）对齐。
