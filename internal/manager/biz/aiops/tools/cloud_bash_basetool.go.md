# `cloud_bash_basetool.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/cloud_bash_basetool.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件实现 `cloud_bash` BaseTool：cloud-side（manager）命令工具，与 `host_bash`（在 edge 设备上跑）相对。在 manager-side Runner sandbox 跑命令并注入 bound credential 的 env——支持 terraform / 云厂商 CLI / kubectl 等操作云资源。关键红线：MVP 不直接执行——每次调用排队到 human approval inbox（biz/approval），用户在 chat 内 inline 卡片批准后才由注册的 executor 跑命令；LLM 永远无法未经 human-in-the-loop 跑带云凭据的 manager-side 命令。Class="destructive"。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 Registry 注册和 agent loop 调用；依赖 `basetool`、`cloudwego/eino/compose`

## 3. 关键类型与接口

```go
const ToolNameCloudBash = "cloud_bash"

type CloudBashProposer interface {
    ProposeAndAwait(ctx, command string, credentials []string,
        sessionID, toolCallID string, userID uint64) (result string, err error)
}

type CloudBashTool struct {
    proposer CloudBashProposer
    log      *slog.Logger
}

type cloudBashArgs struct {
    Command    string
    Credential string
}
```

## 4. 关键函数与流程

### `NewCloudBashTool`
- 构造 CloudBashTool；log nil → slog.Default()

### `Info`
- 返回 `ToolInfo{Name: ToolNameCloudBash, Description, WhenToUse: cloudBashWhenToUse, Parameters: CloudBashSchema, Class: "destructive"}`
- **Class=destructive**：cloud_bash 能用云凭据跑任意命令，永远走最高门控（human approval）

### `mergeCreds`
- **签名**：`func mergeCreds(bound []string, perCall string) []string`
- **职责**：合并 session 绑定凭据 + per-call 凭据，去重保序（bound 优先）
- **流程**：map[string]bool 去重；TrimSpace 跳过空；bound 先 add，perCall 后 add

### `InvokableRun`
- **签名**：`func (t *CloudBashTool) InvokableRun(ctx, argsJSON string, opts ...basetool.InvokeOption) (string, error)`
- **职责**：queue approval → block 等决策 → 返回 executor 真实输出
- **流程**：
  1. proposer nil → error "approval inbox not wired"
  2. unmarshal args；Command 空 → error
  3. `ResolveOptions(opts)` 取 UserID
  4. `mergeCreds(BoundCredentialsFromContext(ctx), in.Credential)` 合并凭据
  5. `compose.GetToolCallID(ctx)` 取 eino 权威 tool call id（关联 streaming card，避免重复渲染；legacy kernel 下为空，proposer 回退 standalone 卡片）
  6. `proposer.ProposeAndAwait(ctx, Command, creds, SessionIDFromContext(ctx), toolCallID, UserID)`
  7. err → wrap "cloud_bash: propose"
  8. 返回 result（approve 时是 executor 真实命令输出；reject/timeout 是 terminal status blob）
- **错误处理**：proposer nil / parse / Command 空 fail-fast；propose 错误 wrap

## 5. 依赖关系

- **内部包**：`basetool`（BoundCredentialsFromContext / SessionIDFromContext）
- **外部库**：`github.com/cloudwego/eino/compose`（GetToolCallID）、标准库 `context`、`encoding/json`、`fmt`、`log/slog`、`strings`
- **被调用方**：Registry 注册；生产 binding 由 cmd/main.go 把 biz/approval.Usecase 包成 CloudBashProposer

## 6. 并发与资源管理

- CloudBashTool immutable，多 goroutine 共享安全
- ProposeAndAwait 同步阻塞——等 human 决策或 timeout
- 无新 goroutine；ctx 透传给 proposer

## 7. 设计模式与亮点

- **MVP 永不直接执行**：每次都走 human approval；future 加 read-class auto-run allowlist
- **HLD-021 synchronous propose-confirm**：阻塞等 human 决策，approve 后返回 executor 真实输出，ReAct loop 自然继续
- **HLD-017 design-time credential binding**：凭据在 install time 决定（session 的 active-skill bound credentials），不由 LLM/user 在 run time 选；per-call credential 是补充
- **HLD-019 persistent workspace**：session id 透传给 approval，execute-on-approve hook 在 `<workspace>/sessions/<id>/` 跑命令，跨命令复用文件
- **凭据合并去重保序**：bound 优先，perCall 补充；TrimSpace 跳空
- **toolCallID 关联卡片**：避免重复渲染 approval 卡片；legacy kernel 兼容回退 standalone
- **Class=destructive**：走 review_gate 装饰器最高门控 + human approval 双层

## 8. 注意事项

- **proposer nil 报错**：approval inbox 必须 wire
- **Command 空报错**：必需参数
- **persistent cwd**：cwd 是 conversation 的持久工作目录，文件跨调用复用，用相对路径
- **credential 可选**：无需云认证的命令可省略
- **未来 read-class allowlist**：MVP 全部走 approval；未来 read 命令可 auto-run
- **Class=destructive 路径**：chain.Wrap 装上 ReviewGate 后，destructive 工具会触发 reviewer；但 cloud_bash 本身已是 human approval，需注意双层 gate 是否冗余
