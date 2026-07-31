# `restart_service.go` 技术实现文档

> 源文件：`internal/skill/builtin/restart_service/restart_service.go`
> 包路径：`github.com/ongridio/ongrid/internal/skill/builtin/restart_service`

## 1. 概述

`restart_service.go` 是 ongrid 第一个 **mutating 类** skill 的注册 shim：在全局 Registry 中声明 `host_restart_service` 的元数据（Class=mutating，Scope=host），让框架能在 catalog/SPA/文档中渲染该 skill。**实际的重启副作用不在此处执行**——`Execute` 方法被锁定为返回 error，强制调用方走 manager 侧 BaseTool + ReviewGate 审批流。

## 2. 包信息

- **包名**：`restart_service`
- **所属模块**：`internal/skill/builtin/restart_service`（内置 mutating skill 子包）
- **依赖方向**：被 `builtin/doc.go` blank import 触发 `init()`；依赖 `internal/skill` 框架类型；实际执行在 `internal/edgeagent/restart_service/handlers.go`（通过 tunnel 派发，不经本 Executor）

## 3. 关键类型与接口

```go
// skill 实现，无状态（注册 shim）
type RestartService struct{}

// 稳定标识常量，与 manager 侧 ToolNameRestartService 相等
const Key = "host_restart_service"

// Execute 直接返回的错误
var errMutatingDirect = errors.New(
    "host_restart_service: mutating skills must be invoked via the manager BaseTool " +
        "(ReviewGate decorator). Direct skill.Execute is not supported.",
)
```

## 4. 关键函数与流程

### `init()`
- **签名**：`func init() { skill.Register(&RestartService{}) }`
- **职责**：自注册到全局 Registry，让框架感知该 skill 的存在与元数据。

### `RestartService.Metadata`
- **签名**：`func (RestartService) Metadata() skill.Metadata`
- **职责**：返回元数据。Key=`host_restart_service`，Class=`ClassMutating`，Scope=`ScopeHost`，Category=`process`。
- **参数**：`device_id`（必填 int，目标设备）、`service`（必填 string，systemd 短名）、`reason`（可选 string，写入审计）。
- **设计意图**：
  - `Class=ClassMutating` 是框架判定"禁止直调 + 需 reviewer 二审"的关键字段；
  - `Scope=ScopeHost` 表示实际副作用发生在 edge，manager 侧 BaseTool 通过 tunnel 派发；
  - `device_id` 参数满足框架对 edge-scoped skill 的 `edge_id` 要求（通过 edge_devices junction 查找）。

### `RestartService.Execute`
- **签名**：`func (RestartService) Execute(_ context.Context, _ json.RawMessage) (json.RawMessage, error)`
- **职责**：**永远是 error**，返回 `errMutatingDirect`。
- **设计意图**：
  - 旧版闭包式 agent kernel 与新版 graph kernel 都可能进入 `Execute`；
  - 旧版 kernel 没有 ReviewGate decorator，直接 `Execute` 会绕过审批；
  - 此处返回 error 确保旧 kernel 无法意外触发真实重启；
  - 新版 graph kernel 调用 manager BaseTool wrapper（被 ReviewGate 装饰），根本不会到达此 `Execute` 路径。
- **错误处理**：直接返回固定 error，不做任何实际工作。

## 5. 依赖关系

- **内部包**：`github.com/ongridio/ongrid/internal/skill`
- **外部库**：`context`、`encoding/json`、`errors`
- **被调用方**：
  - `internal/skill/builtin/doc.go` 通过 blank import 触发 `init()`；
  - `internal/manager/biz/aiops/tools/restart_service_basetool.go`（manager 侧 BaseTool，按 Key 路由）；
  - 实际 edge 执行在 `internal/edgeagent/restart_service/handlers.go`，通过 tunnel `MethodRestartService` 派发，**不经本 Execute**。

## 6. 并发与资源管理

无并发控制。`RestartService` 无状态，`Execute` 是固定 error 返回，无共享可变状态。

## 7. 设计模式与亮点

- **Shim 模式（注册占位）**：本 Executor 仅负责"教 Registry 这个 skill 存在 + 元数据正确"，实际逻辑分布在 manager BaseTool + edge handler 两端，本 Execute 是死路。
- **防御性 error 锁定**：`Execute` 显式返回 error，让任何"绕过 ReviewGate 直接调 skill"的路径立即失败，把保护机制可见化——开发者调试时会看到 error 并改走 BaseTool 路径。
- **Key 常量与 manager BaseTool 对齐**：`Key = "host_restart_service"` 与 manager 侧 `ToolNameRestartService` 相等，让 audit log 与 catalog 跨链接一致。
- **Class=ClassMutating 是框架契约**：框架的权限门检查此字段决定是否拒绝 LLM 直调；本 skill 的所有安全性都依赖这一字段被正确设置。
- **Scope=ScopeHost 的双层含义**：框架据此要求 `edge_id` 参数（通过 `device_id` 满足）；同时暗示"实际副作用在 edge"，调用方应通过 tunnel 派发而非本地执行。

## 8. 注意事项

- **`Execute` 永远返回 error 是有意为之**：这不是 bug，是安全设计；任何修改本 `Execute` 让其直接执行 `systemctl restart` 的尝试都会破坏审批流保护。
- **实际执行路径**：manager BaseTool（被 ReviewGate 装饰）→ reviewer agent 审批 → approve 后通过 `tunnel.MethodRestartService` 派发到 edge → `internal/edgeagent/restart_service/handlers.go` 执行 `systemctl restart`（PR-7 为 MOCK 实现，真实 systemctl 在后续 PR）。
- **`device_id` 与 `edge_id` 的关系**：框架对 edge-scoped skill 要求 `edge_id`，BaseTool 通过 `device_id` 在 `edge_devices` junction 查找对应 edge_id。
- **新增 mutating skill 流程**：建子目录 → 写 shim（参考本文件）→ 在 `builtin/doc.go` 加 blank import → 在 manager 侧写 BaseTool + ReviewGate 装饰 → 在 edge 侧写 handler。
- **`reason` 参数进审计**：调用方应填写有意义的重启理由，便于事后复盘；本 shim 不校验 reason 内容，由 BaseTool 层强制。
- **`service` 参数无白名单**：本 shim 不校验 service 名，由 edge handler 维护允许列表（仅允许重启特定 service）。
- **PR-7 状态**：注释明确"edge handler 是 MOCK 实现"，真实 `systemctl restart` shell-out 在后续 PR；SOP 审批端到端是 PR-7 的重点。
