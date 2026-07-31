# `http.go` 技术实现文档

> 源文件：`internal/manager/server/audit/http.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/server/audit`

## 1. 概述

本文件是 HLD-010 审计日志的 admin-only 只读 HTTP 端点：`GET /v1/admin/audit-logs`，支持分页 + 多维 filter（user_email / action / resource_type / status / 时间窗）。设计要点：**读路径不再自审计**（2026-05-21 operator 决定移除 `audit_view` action，因为每次刷新都产生行，淹没 create/update/delete 信号）；403 拒绝也不写审计（nginx/manager request log 已够）。关键红线：admin 或 superuser 才能访问；时间 filter 走 RFC3339 严格解析。

## 2. 包信息

- **包名**：`audit`
- **所属模块**：`internal/manager/server/`（HTTP handler 层）
- **依赖方向**：被上层 router 装配调用 `NewHandler` + `Register`；依赖 `biz/audit`（`Usecase`、`ListFilters`）、`model/audit`（`Log`）、`pkg/errs`、`pkg/tenantctx`

## 3. 关键类型与接口

```go
type Handler struct {
    uc *bizaudit.Usecase
}

// wire-level audit log（与 model/audit.Log 字段对应）
type wireLog struct {
    ID            uint64     `json:"id"`
    OccurredAt    time.Time  `json:"occurred_at"`
    UserID        *uint64    `json:"user_id,omitempty"`
    UserEmail     string     `json:"user_email"`
    Role          string     `json:"role"`
    IP            string     `json:"ip"`
    UserAgent     string     `json:"user_agent"`
    Action        string     `json:"action"`
    ResourceType  string     `json:"resource_type"`
    ResourceID    string     `json:"resource_id"`
    ResourceName  string     `json:"resource_name"`
    Status        string     `json:"status"`
    ErrorCode     string     `json:"error_code,omitempty"`
    ErrorMessage  string     `json:"error_message,omitempty"`
    PayloadJSON   string     `json:"payload_json,omitempty"`
    RequestID     string     `json:"request_id,omitempty"`
}

type listResp struct {
    Items []wireLog `json:"items"`
    Total int64     `json:"total"`
}
```

## 4. 关键函数与流程

### `NewHandler` / `Register`
- **签名**：`func NewHandler(uc *bizaudit.Usecase) *Handler` + `func (h *Handler) Register(r chi.Router)`
- **职责**：构造 + 挂载 `GET /v1/admin/audit-logs`
- **流程**：单一路由

### `list`
- **签名**：`func (h *Handler) list(w http.ResponseWriter, r *http.Request)`
- **职责**：分页 + filter 查询审计日志
- **流程**：
  1. `tenantctx.From(ctx)` 取 tenant；缺失 → 401
  2. `t.Role != "admin" && !t.IsSuperuser` → 403（**不写审计**——read-only 拒绝靠 nginx/manager request log 留痕）
  3. 解析 query：`user_email`/`action`/`resource_type`/`status`/`limit`(默认 50)/`offset`(默认 0)/`from`/`to`
  4. `from`/`to` 用 `time.Parse(time.RFC3339, ...)`；解析失败**静默忽略**（不报错，该 filter 不生效）
  5. `h.uc.List(ctx, f)` → rows + total
  6. **读路径不审计**（注释明示 2026-05-21 决定）
  7. `toWire` 逐行翻译 → 200 `{items, total}`
- **错误处理**：usecase 错误透传

### `toWire`
- **签名**：`func toWire(r auditmodel.Log) wireLog`
- **职责**：model → wire 字段对字段映射

### `parseInt`
- **签名**：`func parseInt(s string, def int) int`
- **职责**：query int 解析，失败返默认值

### `writeJSON` / `writeErr`
- 标准 helper；`writeErr` 内联 `errBody` 类型，slug 表覆盖 `unauthorized`/`forbidden`/`invalid`/`not_found`/`internal`

## 5. 依赖关系

- **内部包**：`biz/audit`（`Usecase`、`ListFilters`）、`model/audit`（`Log`）、`pkg/errs`、`pkg/tenantctx`
- **外部库**：`github.com/go-chi/chi/v5`
- **被调用方**：上层 router 装配代码

## 6. 并发与资源管理

- **无共享状态**：Handler 仅持有 `uc` 指针
- **ctx 透传**：`r.Context()` 透传给 usecase
- **无缓存**：每次查询都打 DB

## 7. 设计模式与亮点

- **读路径不自审计**：注释明示 2026-05-21 operator 决定移除 `audit_view`——每次刷新产生行淹没 create/update/delete 信号；这是审计层级的"信噪比"优化
- **403 不写审计**：注释明示「Denied access stays unaudited」——read-only 拒绝靠 nginx/manager request log 留痕足够
- **admin OR superuser**：`t.Role != "admin" && !t.IsSuperuser` ——superuser 角色也能访问（比纯 admin 更宽）
- **时间 filter RFC3339 严格解析**：失败静默忽略而非报错，UX 友好（用户输错格式不阻断查询）
- **`PayloadJSON` 透传字符串**：不解析为 JSON object，让前端按需 parse（避免双重 encode）

## 8. 注意事项

- **读路径不审计是历史决定**：2026-05-21 移除 `audit_view`；若未来需要审计读路径需重新评估
- **`from`/`to` 解析失败静默**：用户输错时间格式不会报错，filter 静默失效——前端应做格式校验
- **`limit` 默认 50**：未传 limit 时返 50 条；上限由 usecase 层控制
- **`superuser` 角色也能访问**：与纯 admin gating 不同；`t.IsSuperuser` 字段来自 tenantctx
- **`errBody` 内联在 `writeErr`**：与其它 handler 的 `errorBody` 重复定义，但避免跨包共享 helper
- **无审计写入 helper 调用**：本 handler 不调 `auditmw.SetAuditEvent`，因为读路径不审计
