# monitor/http.go 技术实现文档

## 1. 概述

`http.go` 是 ongrid manager 用户自管理 Monitor 页面面板（`/v1/monitor/panels`）的 HTTP 路由层。提供 panel 的 CRUD：list 给所有认证用户（让所有运维都能看仪表板），create/update/delete 需 admin（与 settings/integration 子域的变更权限对齐）。共 4 个端点。

## 2. 包信息

- **包名**：`monitor`
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/server/monitor`
- **路由前缀**：`/v1/monitor/panels`（由 `cmd/ongrid` 挂载，auth 中间件由上游注入）
- **文件定位**：HTTP 适配层（薄 handler —— auth + JSON decode + delegate to biz）

## 3. 关键类型与接口

### PanelService —— 窄接口

```go
type PanelService interface {
    List(ctx context.Context) ([]*model.Panel, error)
    Get(ctx context.Context, id uint64) (*model.Panel, error)
    Create(ctx context.Context, in biz.CreateInput) (*model.Panel, error)
    Update(ctx context.Context, id uint64, in biz.UpdateInput) (*model.Panel, error)
    Delete(ctx context.Context, id uint64) error
}
```

由 `*biz/monitor.Service` 满足。

### Handler

```go
type Handler struct {
    svc PanelService
}
```

### 响应 DTO

```go
type listResp struct {
    Panels []*model.Panel `json:"panels"`
}
```

## 4. 关键函数与流程

### Register —— 路由表

```go
func (h *Handler) Register(r chi.Router)
```

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/v1/monitor/panels` | any auth | 列所有 panel |
| POST | `/v1/monitor/panels` | admin | 创建 panel |
| PATCH | `/v1/monitor/panels/{id}` | admin | 部分更新 |
| DELETE | `/v1/monitor/panels/{id}` | admin | 删除 |

### list

```go
func (h *Handler) list(w http.ResponseWriter, r *http.Request)
```

`requireUser` → `svc.List` → 200 + `{panels}`。任何已认证用户可读——让所有运维的仪表板都能渲染。

### create / update

`requireAdmin` + `json.Decode(&biz.CreateInput | biz.UpdateInput)` → `svc.Create/Update` → 200 + 保存后的 panel。

### delete

`requireAdmin` + `parseID` + `svc.Delete` → 200 + `{"status": "ok"}`。

### helpers

- `parseID(r)` —— `ParseUint` + `id == 0` 拒绝
- `requireAdmin(w, r)` —— `tenantctx.From` + `Role == "admin"` 检查，返 bool
- `requireUser(w, r)` —— `tenantctx.From` 存在性检查，返 bool
- `writeJSON` / `writeErr` / `errCode` —— 标准 errs 映射

## 5. 依赖关系

**外部**：
- `chi` —— URL 参数
- `net/http`、`encoding/json`、`strconv`、`errors`

**内部**：
- `internal/manager/biz/monitor`（CreateInput/UpdateInput）
- `internal/manager/model/monitor`（Panel）
- `internal/pkg/errs`
- `internal/pkg/tenantctx`

## 6. 并发与资源管理

- **无共享可变状态**：`Handler.svc` 启动时装配
- **请求级隔离**：每请求独立 ctx
- **无 goroutine 启动**：同步调 svc
- **`writeJSON` swallow encode 错误**：响应已开始无法回传

## 7. 设计模式与亮点

1. **权限二分：admin vs any auth**：list 给所有认证用户（让仪表板全员可见），create/update/delete 需 admin（与 settings/integration 子域对齐）
2. **`requireAdmin` / `requireUser` 返 bool 而非 (caller, bool)**：本子域不需要 caller 信息，简化 helper 签名
3. **`biz.CreateInput` / `UpdateInput` 直接作 decode target**：handler 不定义自己的 input DTO，biz 层类型直接复用——减少重复，但耦合 biz ↔ handler
4. **PATCH 而非 PUT**：update 用 PATCH 语义（部分更新），与 RESTful 约定一致
5. **delete 返 200 + `{"status":"ok"}`**：而非 204 No Content，前端按 status 字段判断
6. **`errs.HTTPStatus` 集中映射**：用 errs 包提供的函数，避免本文件重复 if-else

## 8. 注意事项

1. **`roleAdmin` 字符串硬编码**：`t.Role != "admin"` 直接比较，未引入 iam 包常量，避免跨 BC import
2. **`biz.CreateInput` 直接 decode**：handler 与 biz 类型耦合；如需在 handler 层做额外校验需另定义 DTO
3. **`delete` 返 200 而非 204**：与 RESTful 约定不同（其它子域多用 204）；前端按 body.status 判断
4. **无 body 大小限制**：`json.Decode(r.Body)` 未套 `MaxBytesReader`，理论上可传超大 body；panel 配置通常不大但严格防御应加上
5. **无审计**：本文件未调 `SetAuditEvent`，panel 变更不写审计；如需追溯需补 hook
6. **`requireUser` 仅检查存在性**：不检查 role，任何已登录用户（包括 viewer）都可 list
7. **`parseID` 拒绝 0**：避免 0 作为合法 ID 误命中
8. **list 不分页**：`svc.List` 返所有 panel，未做 limit/offset；如 panel 数量很大需评估
