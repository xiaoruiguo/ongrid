# `mcp_basetool.go` 技术实现文档

> 源文件：`internal/manager\biz\aiops\tools\mcp_basetool.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件实现 HLD-018：把一个外部 MCP server 的一个 tool 适配成 ongrid BaseTool。Wire name 格式 `mcp__<server>__<tool>`。Runtime 在 boot 时连接每个 enabled server 后把这些 tool bolt 到 toolbag。**Trusted server** 的 tool 同步运行（`MCPCaller.CallMCPTool`）；否则排队到 human approval inbox（`MCPProposer.ProposeMCPCall`，同 `cloud_bash` 的 propose-confirm 模型）。`MCPToolClass` 按 tool 名动词推断风险类——pure-read（list/get/log/top/stats/view/...）为 "read"，mutating 动词或未知为 "destructive"（gated 到 full-flow / approval）。`Origin=OriginMCP` 让 runtime-discovered 工具路由到 specialists 而非 coordinator。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 runtime boot 注册调用；依赖 `basetool`。通过 `MCPCaller` / `MCPProposer` 窄接口解耦 MCP client 与 approval biz。

## 3. 关键类型与接口

```go
// 同步运行 MCP tool（trusted-server path）。
type MCPCaller interface {
    CallMCPTool(ctx, server, tool string, args map[string]any) (string, error)
}

// 排队 MCP call 等 human approval（default path），返回 approval id。
type MCPProposer interface {
    ProposeMCPCall(ctx, server, tool string, args map[string]any, sessionID string, userID uint64) (id string, err error)
}

type MCPTool struct {
    server, bareName, wireName string  // wireName = mcp__<server>__<tool>
    desc                       string
    schema                     json.RawMessage  // MCP tool 的 inputSchema，原样给 LLM
    trusted                    bool             // true = 同步, false = approval
    caller                     MCPCaller
    proposer                   MCPProposer
    log                        *slog.Logger
}

const MCPToolNamePrefix = "mcp__"
```

## 4. 关键函数与流程

```go
func NewMCPTool(server, bareName, desc string, schema json.RawMessage, trusted bool, caller MCPCaller, proposer MCPProposer, log *slog.Logger) *MCPTool
func MCPToolName(server, tool string) string  // mcp__<sanitize(server)>__<sanitize(tool)>
func sanitizeMCPSeg(s string) string           // trim + lower + [a-z0-9] 保留，其他 → _
func MCPToolClass(bareName string) string      // 动词嗅探 → "read" | "destructive"
func (t *MCPTool) Info(_ context.Context) (*basetool.ToolInfo, error)
func (t *MCPTool) InvokableRun(ctx, argsJSON string, opts ...InvokeOption) (string, error)
```

**`InvokableRun` 流程**：
1. Unmarshal argsJSON → `map[string]any`（空字符串跳过，空 args 即 `{}`）。
2. **Trusted path**：`t.trusted == true` → `caller.CallMCPTool(ctx, server, bareName, args)` 直接返回结果字符串。
3. **Approval path**（default）：`proposer.ProposeMCPCall(ctx, server, bareName, args, "", cfg.UserID)` 排队，返回 approval id。构造 `out = {status: "pending_approval", approval_id: id, message: "..."}`。`message` 是 LLM-facing instruction（同 `cloud_bash` 契约）：inline confirmation card 已渲染，回复一句短句说外部 MCP action 需用户确认，不要引导用户去页面/菜单，不要重述 call/approval_id/status table。

**`MCPToolName`**：`MCPToolNamePrefix + sanitizeMCPSeg(server) + "__" + sanitizeMCPSeg(tool)`。`sanitizeMCPSeg` 把 server/tool 名规范化为 `[a-z0-9_]`（大写转小写，其他字符转 `_`），确保 wire name 符合 LLM tool name 规范。

**`MCPToolClass`**：
- 遍历 mutating 动词列表（`delete`/`remove`/`create`/`apply`/`exec`/`scale`/`restart`/`patch`/`update`/`drain`/`cordon`/`rollout`/`evict`/`kill`/`destroy`/`stop`/`start`/`attach`/`write`）——`strings.Contains(n, v)` 命中 → `"destructive"`。
- 否则遍历 read 动词列表（`list`/`get`/`read`/`view`/`describe`/`log`/`top`/`stat`/`status`/`summary`/`event`/`watch`/`search`/`info`/`query`/`show`/`fetch`/`inspect`/`config`/`cat`/`tail`/`head`）——命中 → `"read"`。
- 都不命中 → `"destructive"`（保守默认，gated 到 approval）。

**`Info`**：
- `class := MCPToolClass(bareName)`。
- `desc` 空则 `"MCP tool <bareName> from server <server>"`。
- `schema` 空则 `{"type":"object"}`。
- `WhenToUse = "外部 MCP 服务「<server>」提供的能力。"`。
- `Origin = basetool.OriginMCP`（runtime-discovered → routed to specialists, not the coordinator）。

## 5. 依赖关系

- **basetool**：`ToolInfo`（`Class` 动态 / `Origin=OriginMCP`）、`ResolveOptions(opts)` 取 `UserID`。
- **MCPCaller / MCPProposer**：窄接口，runtime 实现。接口在消费方定义。
- 不依赖 alertbiz / devicebiz / edgebiz / prom / log / trace / tunnel。

## 6. 并发与资源管理

- **无锁、无共享可变状态**：`MCPTool` 仅持有不变依赖引用，多 goroutine 可并发调用。
- **无 goroutine**：单次 `CallMCPTool` 或 `ProposeMCPCall` 同步调用。超时由 `ctx` 控制（本工具不加独立超时）。
- **`trusted` 决定路径**：trusted 同步（可能阻塞，但 MCP server 应自带超时）；non-trusted 排队即返回 `pending_approval`，不阻塞。

## 7. 设计模式与亮点

- **HLD-018 MCP 适配**：把外部 MCP server 的 tool 适配成 ongrid BaseTool，让 LLM 像调原生工具一样调 MCP tool。Wire name `mcp__<server>__<tool>` 让 LLM 能识别这是 MCP 工具。
- **Trusted vs approval 双路径**：注释明示 "A trusted server's tools run synchronously; otherwise the call is queued to the human approval inbox (same propose-confirm model as cloud_bash)"。`trusted` flag 仅控制 sync-vs-approval run path，与 `Class` 解耦。
- **`MCPToolClass` 动词嗅探**：MCP server 很少设 `readOnlyHint` annotation，所以按 tool 名动词推断。mutating 动词或未知 → `destructive`（保守，gated 到 approval）。read 动词 → `read`（可单节点 test-run）。
- **`Origin=OriginMCP`**：runtime-discovered 工具路由到 specialists 而非 coordinator。这是 basetool 的 origin 机制——让 runtime 知道这是动态发现的工具，区别于内置工具。
- **`sanitizeMCPSeg` 防 wire name 异常**：MCP server/tool 名可能含大写 / 连字符 / 空格，sanitize 后仅 `[a-z0-9_]`，确保 wire name 符合 LLM tool name 规范。
- **`message` LLM-facing instruction**：与 `cloud_bash` / `install_skill` 同契约——inline card 已渲染，回复一句短句，不引导去页面，不重述 call/id。
- **schema 原样传递**：MCP tool 的 inputSchema 原样给 LLM，不做转换。空 schema 默认 `{"type":"object"}`。
- **Conservative default `destructive`**：未知动词归 destructive，确保新 MCP tool 默认走 approval 路径，安全第一。

## 8. 注意事项

- **`trusted` 与 `Class` 解耦**：trusted server 的 tool 仍可能是 `destructive` class（如 `delete_pod`）。`Class` 决定 Reviewer 审批装饰器是否触发，`trusted` 决定 sync-vs-approval。两层独立。
- **`MCPToolClass` 启发式不完美**：`restart_service` 含 `restart` → destructive（正确）；`describe_pod` 含 `describe` → read（正确）；但 `exec_container` 含 `exec` → destructive（正确）；`watch_events` 含 `watch` → read（可能误判，watch 可能长期占用）。注释明示 "MCP servers rarely set the readOnlyHint annotation, so we read the verb"。
- **`args` 是 `map[string]any`**：MCP tool args 是无类型 map，本工具不做 schema 校验（schema 传给 LLM 让 LLM 遵守）。MCP server 端应自行校验。
- **`sessionID` 硬编码空字符串**：`proposer.ProposeMCPCall(ctx, server, bareName, args, "", cfg.UserID)` 第二参数 sessionID 传空。与 `install_skill`（传 `basetool.SessionIDFromContext(ctx)`）不一致——可能是疏忽，approval inbox 可能需要 sessionID 关联 inline card。
- **无独立超时**：trusted path 依赖 MCP server 自带超时；approval path 排队即返回。若 MCP server hang，trusted path 会阻塞到 ctx 超时。
- **`Origin=OriginMCP` 路由影响**：runtime-discovered 工具路由到 specialists。LLM 调用时会看到 origin 标记，可能影响 routing 决策。
- **`WhenToUse` 简短**：仅 "外部 MCP 服务「<server>」提供的能力。"，缺乏反 guard。LLM 可能误用 MCP 工具做原生工具能做的事。未来可补充 "NOT for: 原生工具能做的事（用对应原生工具）"。
- **无 `ExecuteResult.DeviceID`**：BaseTool 返回纯字符串，无 device 维度。
