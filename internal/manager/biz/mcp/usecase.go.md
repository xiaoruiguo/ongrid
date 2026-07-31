# usecase.go

## 1. 概述

`usecase.go` 是 mcp 包的唯一源文件，定义外部 MCP server 注册的 biz 层（HLD-018）。职责：
- CRUD 校验（`normalizeAndValidate`）
- 凭证解析：从 vault 取引用凭证的字段 map，展开 `{{field}}` 占位符到 HTTP header
- 连接探针：`initialize → tools/list`，结果（tools 快照 + status）回写到行

文件刻意小而薄：复杂协议逻辑在 `internal/pkg/mcpclient`，biz 层只做编排与持久化映射。

## 2. 包信息

- 包名：`mcp`
- 路径：`internal/manager/biz/mcp`
- 包注释：明确 HLD-018 + 三大职责（CRUD / 凭证解析 / 连接探针）

## 3. 关键类型与接口

### Transport 常量

```go
const (
    TransportHTTP  = "http"
    TransportStdio = "stdio"
)
```

### Repo 接口（持久化契约）

```go
type Repo interface {
    Create(ctx, *model.Server) error
    Get(ctx, id) (*model.Server, error)
    GetByName(ctx, name) (*model.Server, error)
    List(ctx) ([]*model.Server, error)
    Update(ctx, id, patch *model.Server) error
    Delete(ctx, id) error
    SetStatus(ctx, id, status, lastErr) error
    SetToolsCache(ctx, id, toolsJSON) error
}
```

### SecretResolver 接口

```go
type SecretResolver interface {
    ResolveFields(ctx, name string) (map[string]string, error)
}
```

`*bizsecret.Usecase` 结构性满足。注释：fields map 只在进程内，不持久化。

### Usecase

```go
type Usecase struct {
    repo    Repo
    secrets SecretResolver
    log     *slog.Logger
}
```

## 4. 关键函数与流程

### Create / Update / Delete / Get / List

标准 CRUD。`Create` 与 `Update` 调 `normalizeAndValidate` 后委派给 repo。`Delete` 直接委派。

### ListEnabled

返回 `Enabled == true` 的 server 子集。boot 时调，连这些 server 拉 tools 注册到 agent toolbag。

### CallTool

```go
func (u *Usecase) CallTool(ctx, serverName, toolName string, args map[string]any) (string, error)
```

trusted path（MCP tool adapter）+ post-approval executor 都调它。流程：
1. `repo.GetByName(serverName)`
2. `BuildClient(ctx, s)`
3. `cli.Initialize(ctx)`
4. `cli.CallTool(ctx, toolName, args)`
5. `res.IsError` → 返回 error 含 `TextContent()`
6. 否则返回 `res.TextContent()`（flatten 文本）

### BuildClient

```go
func (u *Usecase) BuildClient(ctx, s) (*mcpclient.Client, error)
```

1. transport 校验：`stdio` 拒绝（未实现）；`http`/空 fallthrough；其它 `ErrInvalid`
2. endpoint 必填（http）
3. 若 `s.Credential` 非空：`secrets.ResolveFields(ctx, s.Credential)` 取 fields map
4. `expandHeaders(s.HeaderTemplateJSON, fields)` 展开 `{{field}}` 占位
5. `mcpclient.NewHTTP(s.Endpoint, headers, 0)` 构造 client

### TestConnection

```go
func (u *Usecase) TestConnection(ctx, id) ([]mcpclient.Tool, error)
```

探针 + 缓存结果：
- 任一步失败 → `repo.SetStatus(id, "error", err.Error())` + 返回 err
- 成功 → `json.Marshal(tools)` → `repo.SetToolsCache(id, string(b))` → `repo.SetStatus(id, "ok", "")` → 返回 tools

### normalizeAndValidate

trim + 默认值 + 不变量：
- `Name` 非空
- `Transport` 默认 `http`，必须是 `http`/`stdio`
- `http` 必须有 endpoint

### expandHeaders

```go
func expandHeaders(templateJSON string, fields map[string]string) (map[string]string, error)
```

解析 header 模板（JSON `map[string]string`），每个 value 中所有 `{{field}}` 替换为 `fields[field]`。空模板返回 nil。

## 5. 依赖关系

### 外部包

- `encoding/json` / `strings` / `fmt` / `log/slog` / `context`

### 内部包

- `model "github.com/ongridio/ongrid/internal/manager/model/mcp"` —— `Server` 模型
- `github.com/ongridio/ongrid/internal/pkg/errs` —— 错误哨兵
- `github.com/ongridio/ongrid/internal/pkg/mcpclient` —— MCP 客户端协议实现

### 被谁调用

- HTTP handler 调 CRUD + `TestConnection`
- chatruntime 调 `ListEnabled`（boot）+ `CallTool`（运行时）

## 6. 并发与资源管理

- 无锁、无 goroutine，纯同步
- 无共享可变状态；并发安全由 repo 与 `SecretResolver` 实现保证
- `BuildClient` 不缓存 client，每次调用新建（连接池由 `mcpclient.Client` 内部管理）

## 7. 设计模式与亮点

### 窄接口 + 结构性满足

`SecretResolver` 只暴露 `ResolveFields`，`*bizsecret.Usecase` 不需 `impl` 声明就结构性满足。符合 gospec 红线。

### 凭证字段不持久化

`SecretResolver.ResolveFields` 返回进程内 map，`expandHeaders` 把字段塞进 header 后 map 即丢。凭证明文不进 DB、不进日志。

### stdio 显式拒绝

`BuildClient` 对 `stdio` 显式返回 "not supported yet" 而非 fallthrough 到默认。注释明确这是阶段限制。

### TestConnection 缓存回写

探针结果（tools 列表 + status）回写到行，让 SPA 不必每次重新探针就能渲染。错误也持久化，UI 能直接显示。

### header 模板用 JSON map

`HeaderTemplateJSON` 是 `map[string]string` 而非自由文本，强制结构化。`expandHeaders` 解析失败返回明确错误。

## 8. 注意事项

- **stdio 未实现**：`BuildClient` 对 stdio transport 直接 error。若未来支持，需在 `mcpclient` 加 stdio client + 这里分支
- **header 占位符未匹配时保留原样**：`expandHeaders` 用 `ReplaceAll`，若 `fields` 缺 key，`{{key}}` 保留在 value 中。这是有意的 —— 让缺字段显式暴露而非静默空值
- **`CallTool` 不缓存 client**：每次调用新建 client + Initialize。高频调用应考虑 client 池（当前 MCP 调用频率低，简单优先）
- **`TestConnection` 错误吞 SetStatus**：`SetStatus` 失败被 `_ =` 丢弃。gospec 红线"禁止 `_ = fn()` 忽略错误"—— 这里是有意的：探针已失败，再因 status 写入失败让调用方拿不到原 error 是反直觉的。应加注释说明
- **`Server.Credential` 是 name 不是 id**：`SecretResolver.ResolveFields` 按 name 解析。name 重复或缺失由 `bizsecret` 处理
- **transport 字面量硬编码**：`TransportHTTP` / `TransportStdio` 是字符串常量，与 DB 持久化值绑定。改值需 migration
- **`HeaderTemplateJSON` 空 → nil headers**：`mcpclient.NewHTTP` 接受 nil headers 表示无自定义头
