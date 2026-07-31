# secret/http.go 技术实现文档

## 1. 概述

`http.go` 是 ongrid manager 通用密钥保险库（HLD-017）的 HTTP 路由层。所有路由 **admin-only**；值是 **write-only**——list API 返回 redacted 视图（`has_value` 标志），绝不返回密钥材料本身。共 5 个端点：list / create / update / delete / credential-types。

## 2. 包信息

- **包名**：`secret`
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/server/secret`
- **路由前缀**：`/v1/secrets` + `/v1/credential-types`（由 `cmd/ongrid` 挂载，auth 中间件由上游注入）
- **文件定位**：HTTP 适配层（薄 handler —— admin 校验 + JSON decode + delegate to biz Usecase）

## 3. 关键类型与接口

### Handler

```go
type Handler struct {
    uc *bizsecret.Usecase
}
```

直接持有 `*bizsecret.Usecase` 具体类型，无窄接口。

### caller —— 本地 caller 类型

```go
type caller struct {
    UserID uint64
    Role   string
}
```

本子域自定义 caller 类型（与其它子域的 `biz*.Caller` 平行），从 `tenantctx` 构造。

### 内联请求 DTO

```go
// create
struct {
    Name        string            `json:"name"`
    Type        string            `json:"type"`
    Description string            `json:"description"`
    Fields      map[string]string `json:"fields"`
}

// update
struct {
    Description string            `json:"description"`
    Fields      map[string]string `json:"fields"`
}
```

`Fields map[string]string` 是 credential type 定义的键值对（如 `{api_key, secret}`）。

## 4. 关键函数与流程

### Register —— 路由表

```go
func (h *Handler) Register(r chi.Router)
```

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/v1/secrets` | admin | 列所有密钥（redacted） |
| POST | `/v1/secrets` | admin | 创建密钥 |
| PUT | `/v1/secrets/{id}` | admin | 更新密钥 |
| DELETE | `/v1/secrets/{id}` | admin | 删除密钥 |
| GET | `/v1/credential-types` | admin | 列可用 credential 类型 |

### types —— credential 类型清单

```go
func (h *Handler) types(w http.ResponseWriter, r *http.Request)
```

调 `bizsecret.AllCredTypes()` 返静态类型清单（含字段定义 + 注入方式），让前端 create-credential UI 渲染正确表单。

### list —— redacted 视图

```go
func (h *Handler) list(w http.ResponseWriter, r *http.Request)
```

调 `uc.List(ctx)` 返 redacted 视图（`has_value` 标志），**绝不返回密钥材料**。

### create

`requireAdmin` + `MaxBytesReader(64 KiB)` + decode → `uc.Create(ctx, name, type, desc, fields)` → 200 + 创建后的视图。

### update

`requireAdmin` + `ParseUint(id)` + `MaxBytesReader(64 KiB)` + decode → `uc.Update(ctx, id, desc, fields)` → 200 + `{"ok": true}`。

### del

`requireAdmin` + `ParseUint(id)` + `uc.Delete(ctx, id)` → 200 + `{"ok": true}`。

### helpers

- `requireAdmin(w, r)` —— `tenantctx.From` + `Role == "admin"`，返 `(caller, bool)`
- `writeJSON` / `writeErr` / `errCode` / `errSlug` —— 标准 errs 映射

**注意**：本文件 `errCode` 返 `int`（HTTP status），`errSlug` 返字符串 slug——与其它子域的 `mapErr` 单函数模式不同，拆为两个函数。

## 5. 依赖关系

**外部**：
- `chi` —— URL 参数
- `net/http`、`encoding/json`、`strconv`、`errors`

**内部**：
- `internal/manager/biz/secret`（Usecase + AllCredTypes）
- `internal/pkg/errs`
- `internal/pkg/tenantctx`

## 6. 并发与资源管理

- **无共享可变状态**：`Handler.uc` 启动时装配
- **请求级隔离**：每请求独立 ctx
- **`create`/`update` body 上限 64 KiB**：`MaxBytesReader` 防 malicious 大 body
- **`writeJSON` swallow encode 错误**：响应已开始无法回传

## 7. 设计模式与亮点

1. **write-only 设计**：list 返 redacted 视图（`has_value`），绝不返回密钥材料——即便管理员也只能写不能读
2. **`/v1/credential-types` 独立端点**：静态类型清单单独暴露，让前端 create-credential 表单动态渲染
3. **`Fields map[string]string` 灵活字段**：credential type 定义字段集，每个 secret 按类型存对应键值
4. **`caller` 本地类型**：避免依赖 biz 包的 Caller 类型，保持 handler 自包含
5. **`errCode` + `errSlug` 双函数**：分别返 HTTP status 和 slug，比单 `mapErr` 更灵活
6. **admin-only 全部端点**：密钥是高敏感资源，无 any-auth 降级
7. **`MaxBytesReader(64 KiB)`**：create/update body 上限，防恶意大 body

## 8. 注意事项

1. **write-only 不可读**：list/create/update 都不返密钥材料；如需验证值需 shell 访问 DB
2. **`roleAdmin` 字符串硬编码**：`t.Role != "admin"` 直接比较，避免跨 BC import
3. **`caller` 本地类型与 biz.Caller 平行**：如 biz 层 Caller 字段变化需同步本文件
4. **`errCode` / `errSlug` 拆分**：与其它子域的 `mapErr` 单函数模式不同，调用方需注意
5. **无审计**：本文件未调 `SetAuditEvent`，密钥变更不写审计；如需追溯需补 hook（密钥变更通常是高敏感操作，建议审计）
6. **`create` 返 200 而非 201**：与 RESTful 约定不同；如需统一需评估前端兼容
7. **`update` 返 `{"ok": true}`**：不返更新后的资源，前端如需显示需另发 GET
8. **`del` 返 200 而非 204**：与 RESTful 约定不同；前端按 body.ok 判断
9. **`credential-types` 是静态清单**：`AllCredTypes()` 返编译期固定类型，新增类型需改 biz 层代码
10. **`Fields` 不验证字段集**：本层不校验 fields 是否符合 credential type 定义，由 biz 层负责
