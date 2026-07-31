# `catalog.go` 技术实现文档

> 源文件：`internal/manager/biz/flow/catalog.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/flow`

## 1. 概述

本文件实现 tool-node 调色板源。flow engine 的 `tool` 节点可按名 dispatch 任何注册的 BaseTool；本 catalog 把这个 universe 暴露给 canvas，让每个 tool 成为拖拽式、表单驱动的节点而非手敲 tool 名。Catalog 是**只读元数据**（name/description/when-to-use/class/category/JSON-Schema params），在 `cmd/ongrid/main.go` 中由 live `tools.Registry` 产出，让 biz/flow 免于 aiops/tools 导入。

## 2. 包信息

- **包名**：`flow`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 HTTP handler（list-tools API）调用；依赖 `encoding/json`

## 3. 关键类型与接口

```go
type ToolMeta struct {
    Name          string          // wire name（进 tool node config.tool）
    DisplayZh     string          // 中文显示标签（fallback Name）
    Description   string          // 一行"做什么"（英文）
    DescriptionZh string          // 中文一行（fallback Description）
    WhenToUse     string          // 消歧提示
    Class         string          // read / write / destructive
    Category      string          // UI 分组（topology/host/observability/…）
    Parameters    json.RawMessage // args 对象的 JSON Schema（form 源）
}

type ToolCatalog interface {
    ListTools() []ToolMeta
}
```

## 4. 关键函数与流程

### `WithToolCatalog`
- **签名**：`func (u *Usecase) WithToolCatalog(c ToolCatalog) *Usecase`
- **职责**：注入 palette 源；返回 Usecase 供链式构造
- **流程**：`u.catalog = c; return u`

### `ListTools`
- **签名**：`func (u *Usecase) ListTools() []ToolMeta`
- **职责**：返回 tool-node palette；catalog 未接线返回 nil
- **流程**：
  1. catalog nil → nil（LLM/tools runtime 不可用时 canvas 仍工作，tool drawer 无 preset）
  2. `catalog.ListTools()`

### `ListNodeTypes`
- **签名**：`func (u *Usecase) ListNodeTypes() []*NodeSpec`
- **职责**：返回所有注册的 node-type spec —— 前端从这些渲染 palette + config drawer 而非硬编码
- **流程**：`return AllNodeSpecs()`（透传到 noderegistry.go）

## 5. 依赖关系

- **外部库**：`encoding/json`
- **桥接接口**：`ToolCatalog`（cmd/ongrid/main.go 实现，包装 `tools.Registry.BuildBaseTools().AllTools()`）
- **被调用方**：HTTP handler（list-tools / list-node-types API）、`generate.go`（genSystemPrompt 用 ListTools 拼 LLM prompt）

## 6. 并发与资源管理

- **无共享状态**：catalog 字段不可变；Usecase 持有 catalog 引用
- **无锁**：catalog.ListTools 由实现负责并发安全（通常读 snapshot）

## 7. 设计模式与亮点

- **ToolMeta vs BaseTool 解耦**：biz/flow 不 import aiops/tools；通过 ToolCatalog 接口 + ToolMeta DTO 隔离
- **same seam 模式**：注释明示"same seam pattern as AgentRunner / ToolInvoker / Notifier" —— biz/flow 通过窄接口接入 aiops 子系统
- **catalog nil-safe**：LLM/tools runtime 不可用时 canvas 仍工作；tool drawer 无 preset 但其他节点类型可用
- **Parameters 是 json.RawMessage**：直接传给前端 form 渲染；biz 不解码 schema
- **Class 三档**：read/write/destructive；testRunSideEffect 用此判断是否可 test-run（见 usecase.go）

## 8. 注意事项

- **catalog 可 nil**：LLM 未配置时；ListTools 返回 nil；canvas tool drawer 空
- **ToolMeta.Class 驱动 test-run 守护**：write/destructive tool 不能 test-run（防 side effect）
- **Parameters 是 JSON Schema**：前端 form 从此渲染；biz 不解码
- **ListNodeTypes 透传 AllNodeSpecs**：noderegistry.go 注册表的全集；新增 node 类型自动出现在 API
