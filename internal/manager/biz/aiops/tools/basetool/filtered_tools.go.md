# `filtered_tools.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/basetool/filtered_tools.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools/basetool`

## 1. 概述

本文件实现 persona 白名单过滤后的工具视图的 ctx 传播。`ToolSearch` 从 ctx 读取该切片，只返回当前 persona（coordinator 或 worker）被允许看到的工具，无需在 ToolBag 上维护可变共享状态——后者会在并发 worker 间产生竞态。

## 2. 包信息

- **包名**：`basetool`
- **所属模块**：叶子包
- **依赖方向**：被 chatruntime（runtime 在 `graph.Invoke` 前 set）、`tools/tool_search_tool`（在 graph 内 read）调用；依赖标准库 `context`

## 3. 关键类型与接口

```go
type filteredToolsCtxKeyT struct{}
var filteredToolsCtxKey = filteredToolsCtxKeyT{}
```

## 4. 关键函数与流程

### `WithFilteredTools`
- **签名**：`func WithFilteredTools(ctx context.Context, tools []BaseTool) context.Context`
- **职责**：attach persona 过滤后的工具切片
- **流程**：`len(tools) == 0` → no-op；否则 `context.WithValue` attach
- **错误处理**：无

### `FilteredToolsFromContext`
- **签名**：`func FilteredToolsFromContext(ctx context.Context) []BaseTool`
- **职责**：取出过滤切片，无则返回 nil
- **流程**：类型断言 `ctx.Value(key).([]BaseTool)`，失败返回 nil；caller 应回退到完整工具宇宙

## 5. 依赖关系

- **内部包**：无（同包 `BaseTool` 接口）
- **外部库**：标准库 `context`
- **被调用方**：`chatruntime`（set）、`tools/tool_search_tool.go`（read）

## 6. 并发与资源管理

- 切片 immutable 共享；不同 worker 的 persona 过滤结果通过各自 ctx 隔离，避免 ToolBag 全局可变状态
- 无锁、无 channel

## 7. 设计模式与亮点

- **ctx 隔离 vs 全局状态**：通过 ctx 携带 per-request 工具视图，避免 ToolBag 上 per-call mutate 带来的并发竞态
- **nil = 全量回退**：caller 取到 nil 应回退完整工具宇宙，符合"忘记过滤 = 不限制"的宽松语义

## 8. 注意事项

- **空切片 no-op**：caller 传空切片等同不调用，ctx 不被 attach
- **过滤时机**：runtime 必须在 `graph.Invoke` 之前 set，否则 ToolSearch 在 graph 内拿不到过滤视图
- **切片不可变性**：attach 后底层切片不应被修改
