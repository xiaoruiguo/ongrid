# mcp/http.go 技术实现文档

## 1. 概述

`http.go` 是 ongrid manager MCP（Model Context Protocol）外部服务器注册子域（`/v1/mcp/servers`）的 HTTP 路由层（HLD-018）。所有路由 **admin-only**，提供 MCP server 的 CRUD + 连接探测端点。共 6 个端点：create / list / get / update / delete / test（initialize → tools/list）。

## 2. 包信息

- **包名**：`mcp`
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/server/mcp`
- **路由前缀**：`/v1/mcp/servers`（由 `cmd/ongrid` 挂载，auth 中间件由上游注入）
- **文件定位**：HTTP 适配层（薄包装，调 `svc.*`）

## 3. 关键类型与接口

### Service —— 窄接口

```go
type Service interface {
    Create(ctx context.Context, s *model.Server) (*model.Server, error)
    Update(ctx context.Context, id uint64, patch *model.Server) error
    Delete(ctx context.Context, id uint64) error
    Get(ctx context.Context, id uint64) (*model.Server, error)
    List(ctx context.Context) ([]*model.Server, error)
    TestConnection(ctx context.Context, id uint64) ([]mcpclient.Tool, error)
}
```

由 `*bizmcp.Usecase` 通过结构化类型满足。`TestConnection` 返 `[]mcpclient.Tool`——MCP server initialize + tools/list 后的可用工具列表。

### Handler

```go
type Handler struct {
    svc Service
}
```

### serverInput —— 编辑视图

```go
type serverInput struct {
    Name               string `json:"name"`
    Transport          string `json:"transport"`
    Endpoint           string `json:"endpoint"`
    Command            string `json:"command"`
    ArgsJSON           string `json:"args_json"`
    Credential         string `json:"credential"`
    HeaderTemplateJSON string `json:"header_template_json"`
    Trusted            bool   `json:"trusted"`
    Enabled            bool   `json:"enabled"`
}
```

**关键约束**：只暴露用户可编辑字段，`Status` / `tools cache` / 时间戳由服务端管理。`toModel()` 方法把 input 转 `model.Server`。

### 响应/错误 DTO

```go
type listResp struct {
    Items []*model.Server `json:"items"`
    Total int             `json:"total"`
}

type testResp struct {
    Tools []mcpclient.Tool `json:"tools"`
    Count int              `json:"count"`
}

type errorBody struct {
    Error string `json:"error"`
    Code  string `json:"code"`
}
```

## 4. 关键函数与流程

### Register —— 路由表

```go
func (h *Handler) Register(r chi.Router)
```

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| POST | `/v1/mcp/servers` | admin | 创建 MCP server 注册 |
| GET | `/v1/mcp/servers` | admin | 列出所有注册 |
| GET | `/v1/mcp/servers/{id}` | admin | 单个 server |
| PUT | `/v1/mcp/servers/{id}` | admin | 更新（全量替换字段） |
| DELETE | `/v1/mcp/servers/{id}` | admin | 删除注册 |
| POST | `/v1/mcp/servers/{id}/test` | admin | 探测：initialize + tools/list |

### create

```go
func (h *Handler) create(w http.ResponseWriter, r *http.Request)
```

`requireAdmin` → `MaxBytesReader(64 KiB)` + `json.Decode(&serverInput)` → `in.toModel()` → 设置 `CreatedBy = caller.UserID` → `svc.Create` → 200 + `model.Server`。

### update

`requireAdmin` + `parseID` + `MaxBytesReader(64 KiB)` + `json.Decode` → `svc.Update(id, in.toModel())` → 200 + `{"ok": true}`。**全量替换语义**，未传字段会被置零——前端需先 GET 再 PUT。

### delete

返 `204 No Content`。

### test —— 连接探测

```go
func (h *Handler) test(w http.ResponseWriter, r *http.Request)
```

调 `svc.TestConnection(ctx, id)`，返 200 + `{tools, count}`。biz 层负责 MCP initialize + tools/list 流程；失败会返错误。

### helpers

- `parseID(r)` —— `chi.URLParam("id")` + `ParseUint`，`id == 0` 也返 `errs.ErrInvalid`
- `callerFromRequest(r)` —— 从 `tenantctx.From(ctx)` 取 `Tenant`
- `requireAdmin` —— caller 必须存在且 `Role == "admin"`，否则 401/403
- `writeJSON` / `writeErr` / `mapErr` —— 同 marketplace 的标准模式

## 5. 依赖关系

**外部**：
- `chi` —— URL 参数
- `net/http`、`encoding/json`、`strconv`、`errors`

**内部**：
- `internal/manager/model/mcp`（model.Server）
- `internal/pkg/errs`（错误哨兵）
- `internal/pkg/mcpclient`（Tool 类型）
- `internal/pkg/tenantctx`（鉴权上下文）

## 6. 并发与资源管理

- **无共享可变状态**：`Handler.svc` 启动时装配；chi handler 并发安全
- **`create`/`update` body 上限 64 KiB**：`MaxBytesReader` 防 malicious 大 body；MCP server 配置（含 ArgsJSON/HeaderTemplateJSON）通常远小于此
- **请求级隔离**：每请求独立 ctx
- **`writeJSON` swallow encode 错误**：响应已开始，无法回传

## 7. 设计模式与亮点

1. **全 admin-only**：MCP server 注册涉及外部进程/端点信任边界，所有路由走 `requireAdmin`，无任何 any-auth 降级
2. **`serverInput` 编辑视图分离**：DTO 只含可编辑字段，`Status`/`tools cache`/时间戳由服务端管理，避免前端误改服务端字段
3. **`toModel()` 转换方法**：DTO 自带 model 转换，保持 handler 简洁
4. **`test` 端点返 tools 列表**：让管理员在保存前预览 MCP server 提供的工具，决定是否 `Trusted`
5. **`mapErr` 集中错误映射**：与 marketplace 同模式，新增错误类型只扩 map
6. **`errorBody` 带 code slug**：便于前端按 code 分支国际化
7. **`parseID` 拒绝 0**：避免 0 作为合法 ID 误命中

## 8. 注意事项

1. **`update` 是全量替换**：未传字段会置零；前端应先 GET 再 PUT，或考虑改 PATCH 部分更新
2. **`roleAdmin` 字符串硬编码**：`c.Role != "admin"` 直接比较字符串，未引入 iam 包常量，避免跨 BC import
3. **`Credential` 字段安全**：`serverInput.Credential` 含敏感信息（API key/token），biz 层应加密存储；本层不感知
4. **`TestConnection` 可能慢**：MCP initialize + tools/list 是网络往返，前端应有 loading 状态；本层无 timeout，由 biz 层控制
5. **`create` 返 200 而非 201**：与 RESTful 约定不同（其它子域多用 201）；如需统一需评估前端兼容
6. **`update` 返 `{"ok": true}`**：不返更新后的资源，前端如需显示需另发 GET
7. **无审计**：本文件未调 `SetAuditEvent`，MCP server 变更是否需审计待确认；如需需在 handler 中显式调用
8. **`serverInput` 含 `HeaderTemplateJSON` 字符串字段**：JSON-as-string 设计，biz 层负责解析校验；handler 不验证 JSON 合法性
