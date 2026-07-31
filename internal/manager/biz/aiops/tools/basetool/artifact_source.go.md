# `artifact_source.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/basetool/artifact_source.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools/basetool`

## 1. 概述

本文件实现"artifact 来源"的 ctx 传播。`serve_page`（及未来任何产出 artifact 的工具）读取该值，让运维 UI 能展示"生成来源"列：本页是 chat 助手生成还是 workflow run 生成。chat runtime stamp "chat"，flow tool-invoker stamp "workflow"，缺失视为 unknown。

## 2. 包信息

- **包名**：`basetool`
- **所属模块**：叶子包，避免 chatruntime ↔ tools 循环依赖
- **依赖方向**：被 chatruntime（生产者）、`tools/serve_page` 经由 cmd/main PageStore（消费者）调用；仅依赖标准库 `context`

## 3. 关键类型与接口

```go
type artifactSourceCtxKeyT struct{}
var artifactSourceCtxKey = artifactSourceCtxKeyT{}

const (
    ArtifactSourceChat     = "chat"
    ArtifactSourceWorkflow = "workflow"
)
```

ctx key 使用空 struct 类型别名以避免与其他 ctx value 冲突。

## 4. 关键函数与流程

### `WithArtifactSource`
- **签名**：`func WithArtifactSource(ctx context.Context, src string) context.Context`
- **职责**：在 ctx 上 attach artifact 来源码
- **流程**：`src == ""` → 直接返回原 ctx（no-op）；否则 `context.WithValue` attach 字符串
- **错误处理**：无错误返回

### `ArtifactSourceFromContext`
- **签名**：`func ArtifactSourceFromContext(ctx context.Context) string`
- **职责**：取出来源码，无则返回 `""`
- **流程**：类型断言 `ctx.Value(key).(string)`，断言失败返回零值

## 5. 依赖关系

- **内部包**：无
- **外部库**：标准库 `context`
- **被调用方**：`chatruntime`、`cmd/main` flowToolInvoker、`tools/serve_page`

## 6. 并发与资源管理

- ctx value 是不可变字符串，天然并发安全
- 无锁、无 channel、无缓存

## 7. 设计模式与亮点

- **叶子包作为依赖倒置点**：chatruntime 和 tools 都依赖 basetool，避免 chatruntime → tools 的循环依赖
- **空值 no-op**：caller 可放心传 unknown source，不会污染 ctx
- **来源码字面量集中定义**：`ArtifactSourceChat`/`ArtifactSourceWorkflow` 集中常量，SPA 端做 i18n 本地化

## 8. 注意事项

- **存储原值**：来源码以原值存储，由 SPA 负责 localization
- **缺失 = unknown**：未 stamp 时返回 `""`，UI 应展示 unknown 而非默认 chat
- **新增来源码**：新增来源（如 "scheduled_job"）需在此常量块统一登记
