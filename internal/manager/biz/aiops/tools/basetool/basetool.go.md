# `basetool.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/basetool/basetool.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools/basetool`

## 1. 概述

本文件定义 ongrid 的 eino-aligned 工具表面（tool surface）：`BaseTool` 接口及其元数据类型 `ToolInfo`、`InvokeOption` 函数式选项及 `Resolved` 解析值。设计目标是对齐 cloudwego/eino 的 `tool.BaseTool` + `tool.InvokableTool`，以便未来切换为 eino 时只需一行 type alias 改动。关键红线：本地接口镜像 eino 但暂不直接依赖 eino（PR 顺序约束）；闭包式 `Tool`（registry.go）与本接口共存。

## 2. 包信息

- **包名**：`basetool`
- **所属模块**：`internal/manager/biz/aiops/tools/basetool`（叶子包，无下游业务依赖）
- **依赖方向**：被 `decorators` 包、各 tool 实现、`chatruntime`、`cmd/main` 装配点调用；仅依赖标准库 `context`、`encoding/json`

## 3. 关键类型与接口

```go
type BaseTool interface {
    Info(ctx context.Context) (*ToolInfo, error)
    InvokableRun(ctx context.Context, argsJSON string, opts ...InvokeOption) (string, error)
}

type ToolInfo struct {
    Name        string          // 稳定的 wire name，snake_case 无空格
    Description string          // LLM 可见的功能一句话
    WhenToUse   string          // 与兄弟工具的区分提示
    Parameters  json.RawMessage // 参数 JSON Schema
    Class       string          // "read" / "write" / "destructive"
    Origin      string          // "" builtin / "mcp" / "skill"
}

type InvokeOption func(*invokeConfig)

type invokeConfig struct {
    Tenant   string
    UserID   uint64
    DeviceID *uint64
    UserText string
}

type Resolved struct {
    Tenant   string
    UserID   uint64
    DeviceID *uint64
    UserText string
}
```

常量：

```go
const (
    OriginBuiltin = ""
    OriginMCP     = "mcp"
    OriginSkill   = "skill"
)
```

方法：`(i ToolInfo) IsDynamic() bool` —— `Origin != OriginBuiltin`（运行时发现的工具，需路由到 specialist）。

## 4. 关键函数与流程

### `BaseTool.Info`
- **签名**：`Info(ctx context.Context) (*ToolInfo, error)`
- **职责**：返回工具元数据；纯只读，禁止触碰外部系统
- **流程**：实现方直接构造 `ToolInfo` 返回

### `BaseTool.InvokableRun`
- **签名**：`InvokableRun(ctx context.Context, argsJSON string, opts ...InvokeOption) (string, error)`
- **职责**：执行工具；`argsJSON` 是 LLM 产出的原始 JSON；返回值是回灌给 LLM 的 JSON 字符串
- **流程**：由具体实现解析 args → 执行 → 错误向上传播由 agent loop 写入 `chat_tool_calls.status`
- **opts 用途**：`opts` 承载 tenant/user/device 等上下文，由装饰器链设置；opts 通过 `ResolveOptions` 解析后由跨包装饰器读取

### `WithTenant / WithUserID / WithDeviceID / WithUserText`
- **签名**：`func WithXxx(...) InvokeOption`
- **职责**：函数式选项构造器，写入 `invokeConfig` 对应字段
- **流程**：返回闭包修改 `*invokeConfig`

### `ResolveOptions`
- **签名**：`func ResolveOptions(opts []InvokeOption) Resolved`
- **职责**：将 `[]InvokeOption` 应用到 fresh `invokeConfig`，返回导出的 `Resolved`
- **流程**：遍历 opts 跳过 nil；填入 `Resolved` 结构；该间接层让跨包装饰器能读 resolved 值而不暴露 `invokeConfig`
- **错误处理**：无错误返回

## 5. 依赖关系

- **内部包**：无
- **外部库**：仅标准库 `context`、`encoding/json`
- **被调用方**：`decorators/*`（读取 Resolved 注入 tenant_bind/audit/ratelimit）、各 `*_basetool.go`、`chatruntime`、`cmd/ongrid/main.go`

## 6. 并发与资源管理

- `ToolInfo` 字段全部值类型，可并发读
- `InvokeOption` 是无状态函数；`invokeConfig` 是 per-call 临时对象，无需加锁
- `Resolved` 也为值类型，跨 goroutine 传递安全

## 7. 设计模式与亮点

- **eino 形状镜像**：接口/字段名与 `cloudwego/eino` 的 `tool.BaseTool`/`schema.ToolInfo` 对齐，未来替换为 type alias 即可，callsite 不变
- **Description/WhenToUse 分离**：路由提示独立于功能描述，便于 system prompt 在不同位置渲染、skill manifest 覆盖
- **Class 字段**：`read`/`write`/`destructive` 三档，供 agent loop / SOP gating 决定是否双签
- **Origin + IsDynamic**：动态来源（MCP/skill）通过非空 Origin 自动识别，无需 name-prefix 字符串匹配表，新来源零成本扩展
- **`DeviceID *uint64`**：指针类型，区分 "无 device"（集群级工具如 query_promql）与 "device 0"
- **`ResolveOptions` 间接层**：跨包装饰器读 resolved 值而不暴露 `invokeConfig`，保持封装

## 8. 注意事项

- **PR 顺序约束**：当前不能 import eino（PR-1 才拥有 eino 依赖）；本文件预留了切换点
- **闭包路径共存**：`registry.go` 的闭包式 `Tool` 不变，两种路径在本 PR 并存
- **`opts` 透传**：tool 实现通常忽略 opts，只有装饰器链读取；这是 eino 的约定
- **空 Origin = builtin**：新动态来源必须显式 stamp 非空 Origin，否则被当 builtin 处理
