# `restart_service.go` 技术实现文档

> 源文件：`internal/pkg/tunnel/restart_service.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/tunnel`

## 1. 概述

本文件定义第一个"变更型"edge skill 的 wire 协议：`restart_service.restart`，通过 `systemctl restart <unit>` 重启白名单内的 systemd 服务。Wire shape 极简（unit 名 + 可选 reason），镜像 `host_files.go` 风格：一个 method 常量 + Request/Response 结构对。当前 PR-7 阶段 edge handler 是 mock（无真实 systemctl shell-out），通过 `Mocked` 字段明示姿态。

## 2. 包信息

- **包名**：`tunnel`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被 manager 侧 `internal/manager/biz/aiops/tools/restart_service_basetool.go` 调用；被 edge 侧 `internal/edgeagent/restart_service/handlers.go` 处理；本文件仅依赖标准库 `time`

## 3. 关键类型与接口

```go
const MethodRestartService = "restart_service.restart"

type RestartServiceRequest struct {
    Service string // 短 systemd unit 名（无 .service 后缀，无全路径）
    Reason  string // 操作者理由，原文进 audit log
}

type RestartServiceResponse struct {
    Service   string    // 回显解析后的值
    Restarted bool      // mock 或真实重启成功
    Mocked    bool      // PR-7 stub 阶段为 true
    StartedAt time.Time // edge 时钟
    EndedAt   time.Time
    Error     string    // 失败时填
}
```

## 4. 关键函数与流程

无函数定义（纯 wire 协议常量与结构）。

文档要点：
- **`MethodRestartService`** = `"restart_service.restart"`：cloud→edge RPC method 名
- **`RestartServiceRequest`**：
  - `Service`：短 systemd unit 名（如 "nginx"、"redis"）；无 `.service` 后缀；无全路径。edge sandbox 权威声明白名单（`internal/edgeagent/restart_service/handlers.go::DefaultSandbox`），manager BaseTool 也预过滤同一集合，让 LLM 在调用离开 cloud 前就拿到清晰错误
  - `Reason`：操作者理由，原文进 edge audit log，让 post-mortem 记录"WHY restarted"而非仅"WHAT"
- **`RestartServiceResponse`**：
  - `Mocked=true`：当前 PR-7 stub 阶段，edge handler 不调真实 systemctl；测试断言此字段让 audit 姿态在两种模式下都明确
  - `StartedAt`/`EndedAt`：mock 重启的 edge 时钟区间
  - `Error`：mock systemctl 失败消息，仅 `Restarted=false` 时填

## 5. 依赖关系

- **内部包**：无
- **外部库**：无（仅 `time`）
- **被调用方**：
  - manager：`internal/manager/biz/aiops/tools/restart_service_basetool.go`（BaseTool 填充请求，经 ReviewGate decorator）
  - edge：`internal/edgeagent/restart_service/handlers.go`（DefaultSandbox 白名单 + mock 实现）

## 6. 并发与资源管理

无并发控制（纯类型定义）。Wire 结构是值类型，可安全并发传递。

## 7. 设计模式与亮点

- **SOP 双重签名验证**：注释详述 manager 侧 ReviewGate decorator 拦截：coordinator LLM 发 tool_call → decorator spawn reviewer worker → reviewer 读 SOP + edge state → 仅 "Decision: approve" 才 marshal wire 请求。是"变更型"skill 的端到端 SOP 验证模式
- **白名单双重过滤**：manager BaseTool 预过滤 + edge sandbox 权威过滤，让 LLM 在调用离开 cloud 前就拿到清晰错误
- **`Mocked` 字段显式姿态**：当前 PR-7 stub 阶段 `Mocked=true`；测试断言此字段让 audit 姿态在 mock 与真实模式下都明确，避免误把 mock 当真实重启
- **`Reason` 进 audit**：注释明示"records WHY the restart fired, not just WHAT"，让 post-mortem 有理由可查
- **`Service` 回显**：响应回显解析后的值（含未来 edge 侧 canonicalization）
- **wire shape 镜像 host_files.go**：一个 method 常量 + Request/Response 对，保持包内一致性

## 8. 注意事项

- **当前是 mock**：`Mocked=true` 时 `Restarted` 反映 mock 结果；真实 systemctl shell-out 上线后翻 `Mocked=false`。caller 不应假设 `Restarted=true` 等于真实重启
- **白名单权威在 edge**：注释明示"edge sandbox declares the allow-list authoritatively"，manager 侧预过滤仅为 LLM 体验；caller 不能依赖 manager 侧过滤为安全边界
- **`Service` 短名约定**：无 `.service` 后缀、无全路径；caller 需规范化输入
- **ReviewGate 是 manager 侧装饰器**：本 wire 不感知 review 流程；review 在 marshal 前拦截
- **未来真实 systemctl 风险**：注释提到"Real systemctl shell-out is deferred"，真实实现需考虑 systemctl 权限（root 或 polkit）、超时、并发重启冲突
- **无 timeout 字段**：与 `BashExecRequest` 不同，本 wire 无 per-call timeout；edge handler 内部决定
