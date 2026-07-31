# marketplace/http.go 技术实现文档

## 1. 概述

`http.go` 是 ongrid manager skill 市场（`/v1/marketplace/*`）的 HTTP 路由层。skill 是可热插拔的能力扩展单元（如 terraform 模块、监控检查脚本），市场负责 install/list/uninstall/setBindings/registries 共 6 个端点。权限模型：install/uninstall/setBindings 需 admin，list/registries 任何已认证用户可用。

## 2. 包信息

- **包名**：`marketplace`
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/server/marketplace`
- **路由前缀**：`/v1/marketplace`（由 `cmd/ongrid` 在 chi router 上挂载，auth 中间件由上游统一注入）
- **文件定位**：HTTP 适配层（薄包装，调 `svc.*`）

## 3. 关键类型与接口

### Service —— 窄接口

```go
type Service interface {
    Install(ctx context.Context, caller bizmp.Caller, src bizmp.Source) (*bizmp.InstallResult, error)
    List(ctx context.Context, caller bizmp.Caller) ([]*model.InstalledPack, error)
    Uninstall(ctx context.Context, caller bizmp.Caller, packID string) error
    SetBindings(ctx context.Context, caller bizmp.Caller, packID string, bindings map[string]string) error
    Registries(ctx context.Context, caller bizmp.Caller) bizmp.AllowedRegistries
}
```

由 `*bizmp.Usecase` 通过结构化类型满足。

### Handler

```go
type Handler struct {
    svc Service
}
```

### 响应/错误 DTO

```go
type listResp struct {
    Items []*model.InstalledPack `json:"items"`
    Total int                    `json:"total"`
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
| POST | `/v1/marketplace/install` | admin | 从 registry 或本地路径安装 pack |
| POST | `/v1/marketplace/upload` | admin | 从浏览器上传 zip/tar.gz 安装（实现在 `upload.go`） |
| GET | `/v1/marketplace/installed` | any auth | 列已安装 pack |
| DELETE | `/v1/marketplace/installed/{pack_id}` | admin | 卸载 pack |
| PUT | `/v1/marketplace/installed/{pack_id}/bindings` | admin | 设置 pack 绑定（如绑定到特定 edge） |
| GET | `/v1/marketplace/registries` | any auth | 列允许的 registry 配置 |

### install

```go
func (h *Handler) install(w http.ResponseWriter, r *http.Request)
```

`requireAdmin` → `json.Decode(&bizmp.Source)` → `svc.Install` → 200 + `InstallResult`。

### listInstalled

```go
func (h *Handler) listInstalled(w http.ResponseWriter, r *http.Request)
```

`callerFromRequest` → `svc.List` → 200 + `{items, total}`。任何已认证用户可调（用于 SPA 显示已装能力清单）。

### uninstall

```go
func (h *Handler) uninstall(w http.ResponseWriter, r *http.Request)
```

`requireAdmin` → `chi.URLParam(pack_id)` → `svc.Uninstall` → 204 No Content。

### setBindings

```go
func (h *Handler) setBindings(w http.ResponseWriter, r *http.Request)
```

**关键约束**：`http.MaxBytesReader(w, r.Body, 16<<10)`（16 KiB）——bindings 是简单的 `map[string]string`，16 KiB 足够，防恶意大 body。`requireAdmin` + `pack_id` URL 参数 + `bindings` JSON body → `svc.SetBindings` → 200 + `{"ok": true}`。

### registries

```go
func (h *Handler) registries(w http.ResponseWriter, r *http.Request)
```

返 `svc.Registries` 的 `AllowedRegistries` 结构（包含哪些 registry 配置可被当前 caller 使用）。

### helpers —— callerFromRequest / requireAdmin

```go
func callerFromRequest(r *http.Request) (bizmp.Caller, bool)
func requireAdmin(w http.ResponseWriter, r *http.Request) (bizmp.Caller, bool)
```

从 `tenantctx.From(r.Context())` 取 `tenant`，构造 `bizmp.Caller{UserID, Role}`。`requireAdmin` 在 `callerFromRequest` 之上检查 `Role == "admin"`，否则返 `errs.ErrForbidden`。

### writeJSON / writeErr / mapErr

```go
func writeJSON(w http.ResponseWriter, code int, body any)
func writeErr(w http.ResponseWriter, err error)
func mapErr(err error) (int, string)
```

`mapErr` 把 `errs` 哨兵映射到 HTTP status + slug：
- `ErrUnauthorized` → 401 `unauthorized`
- `ErrForbidden` → 403 `forbidden`
- `ErrNotFound` → 404 `not-found`
- `ErrConflict` → 409 `conflict`
- `ErrInvalid` → 400 `invalid-argument`
- 其它 → 500 `internal`

`writeErr` 返 `{error, code}` JSON envelope（不同于 logs/knowledge 的简化 schema，本子域走带 code slug 的版本，便于前端国际化和分支处理）。

## 5. 依赖关系

**外部**：
- `chi` —— URL 参数
- `net/http`、`encoding/json`、`errors`

**内部**：
- `internal/manager/biz/marketplace`（biz 类型：Caller/Source/InstallResult/AllowedRegistries）
- `internal/manager/model/marketplace`（model.InstalledPack）
- `internal/pkg/errs`（错误哨兵）
- `internal/pkg/tenantctx`（鉴权上下文）

## 6. 并发与资源管理

- **无共享可变状态**：`Handler.svc` 启动时装配，handler 内不修改；chi handler 并发安全。
- **`setBindings` body 上限 16 KiB**：`MaxBytesReader` 防恶意大 body，bindings 是小型 map 不需要更多。
- **`writeJSON` 中 `_ = json.NewEncoder(w).Encode(body)`**：swallow encode 错误——响应已开始写入（Header 已 sent），无法回传错误。
- **请求级隔离**：每请求独立 ctx。

## 7. 设计模式与亮点

1. **权限二分：admin vs any auth**：install/uninstall/setBindings 需 admin（变更系统状态），list/registries 任何已认证用户可查（决策辅助）。`requireAdmin` / `callerFromRequest` 双 helper 让权限校验显式且不重复。
2. **`mapErr` 集中错误映射**：所有 `errs` 哨兵到 HTTP status + slug 的映射集中在 `mapErr`，handler 只调 `writeErr(w, err)`，新增错误类型只需扩 map。
3. **`errorBody` 带 code slug**：`{error, code}` 双字段，前端可按 `code` 分支国际化，`error` 是 fallback 文案。
4. **`callerFromRequest` 从 `tenantctx` 派生**：handler 不直接依赖 `iam`/`session` 包，只从 `tenantctx` 取 `UserID/Role`，保持架构分层清晰。
5. **`setBindings` body 上限 16 KiB**：bindings 是 `map[string]string`，按内容大小预判合理上限，防恶意大 body 耗内存。
6. **结构化类型满足 Service 接口**：`*bizmp.Usecase` 鸭子类型满足 `Service`，无需显式声明，测试替身易写。

## 8. 注意事项

1. **`roleAdmin` 字符串硬编码**：`requireAdmin` 中 `c.Role != "admin"` 直接比较字符串"admin"，未引入 iam 包的 `roleAdmin` 常量，避免 manager→iam 跨 BC import（架构 lint 约束）。如 role 体系变更需同步本文件。
2. **`callerFromRequest` 失败时 caller 零值**：返 `bizmp.Caller{}, false`，调用方必须检查 `ok`，否则会传入零值 caller 导致 biz 层权限判定失效。
3. **`writeJSON` swallow encode 错误**：`_ = json.NewEncoder(w).Encode(body)`——响应已开始，无法回传错误，只能记录在 access log（如有）。
4. **`upload` 端点实现在 `upload.go`**：本文件 Register 注册 `/v1/marketplace/upload`，但 handler 方法 `upload` 在 `upload.go` 中实现（同包）。
5. **`install` 端点 body 无大小限制**：`json.Decode(r.Body)` 未套 `MaxBytesReader`，理论上可传超大 body——Source 结构通常很小，但如需严格防御应加上。
6. **`uninstall` 返 204 而非 200**：与 RESTful 约定一致（删除成功无 body），前端按 status 判断。
7. **`setBindings` 返 `{"ok": true}`**：简化的成功响应，无返回数据；如需返回更新后的 bindings 需扩 Service 接口。
