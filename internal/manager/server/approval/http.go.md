# `http.go` 技术实现文档

> 源文件：`internal/manager/server/approval/http.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/server/approval`

## 1. 概述

本文件是 propose-confirm 收件箱（HLD-017）的 HTTP 层：暴露 `/v1/approvals` 的 list / count / get / approve / reject 路由。设计要点：**所有路由 admin-only**（读 + 决策都需 admin 角色）；strictly additive，不破坏既有路径。关键红线：`reject` 用 `http.MaxBytesReader` 限 8KB body 防滥用；`approve` 不带 body，仅用 URL 参数。

## 2. 包信息

- **包名**：`approval`
- **所属模块**：`internal/manager/server/`（HTTP handler 层）
- **依赖方向**：被上层 router 装配调用 `NewHandler` + `Register`；依赖 `biz/approval`（`Usecase`）、`pkg/errs`、`pkg/tenantctx`

## 3. 关键类型与接口

```go
type Handler struct{ uc *bizapproval.Usecase }

// 内部 caller 结构（与 server/secret 镜像，避免 import service 层）
type caller struct {
    UserID uint64
    Role   string
}

type errorBody struct {
    Error string `json:"error"`
    Code  string `json:"code"`
}
```

无 DTO 类型——直接返 usecase 返回值；`reject` 内联匿名 struct `{Reason string}`。

## 4. 关键函数与流程

### `NewHandler`
- **签名**：`func NewHandler(uc *bizapproval.Usecase) *Handler`
- **职责**：注入 usecase

### `Register`
- **签名**：`func (h *Handler) Register(r chi.Router)`
- **职责**：挂载 5 个路由：`GET /v1/approvals`、`GET /v1/approvals/count`、`GET /v1/approvals/{id}`、`POST /v1/approvals/{id}/approve`、`POST /v1/approvals/{id}/reject`
- **错误处理**：每个 handler 内部 `requireAdmin` gating

### `list`
- **职责**：`GET /v1/approvals?status=`
- **流程**：requireAdmin → `h.uc.List(ctx, status, 0)` → 200 `{items: [...]}`
- **错误处理**：non-admin → 403；usecase 错误透传

### `count`
- **职责**：`GET /v1/approvals/count` — pending 计数（badge 轮询用）
- **流程**：requireAdmin → `h.uc.CountPending(ctx)` → 200 `{pending: n}`

### `get`
- **职责**：`GET /v1/approvals/{id}` — 详情
- **流程**：requireAdmin → `h.uc.Get(ctx, chi.URLParam(r, "id"))` → 200

### `approve`
- **职责**：`POST /v1/approvals/{id}/approve` — 批准
- **流程**：requireAdmin → `h.uc.Approve(ctx, c.UserID, id)` → 200（返更新后的 approval）
- **关键**：不带 body；approver 由 caller.UserID 决定

### `reject`
- **职责**：`POST /v1/approvals/{id}/reject` — 拒绝
- **流程**：requireAdmin → `http.MaxBytesReader(w, r.Body, 8<<10)` 限 8KB → decode `{reason}` → `h.uc.Reject(ctx, c.UserID, id, in.Reason)` → 200 `{ok: true}`
- **错误处理**：decode 失败**故意忽略**（`_ =`）——reason 可空，reject 仍能进行

### `requireAdmin`
- **签名**：`func requireAdmin(w, r) (caller, bool)`
- **职责**：从 `tenantctx` 取 caller；role != "admin" → 403
- **流程**：`tenantctx.From` 失败 → 401；role 非 admin → 403；否则返 caller

### `writeJSON` / `writeErr`
- 标准 helper；`writeErr` 把 sentinel 映射到 HTTP code + slug（`unauthorized`/`forbidden`/`not_found`/`invalid`/`internal`）

## 5. 依赖关系

- **内部包**：`biz/approval`（`Usecase`）、`pkg/errs`、`pkg/tenantctx`
- **外部库**：`github.com/go-chi/chi/v5`
- **被调用方**：上层 router 装配代码

## 6. 并发与资源管理

- **无共享状态**：Handler 仅持有 `uc` 指针，无锁、无缓存
- **ctx 透传**：所有 usecase 调用透传 `r.Context()`
- **body 限制**：reject 用 `MaxBytesReader` 限 8KB，防超大 reason body 滥用

## 7. 设计模式与亮点

- **Admin-only 全局 gating**：读 + 写都 admin，简化权限模型；propose-confirm 是高敏感操作
- **`MaxBytesReader` 8KB 限 body**：reject reason 限长，防滥用
- **镜像 server/secret 的 helper**：注释明示「mirrors server/secret」——caller / errorBody / writeJSON / writeErr 复制而非共享，避免跨 BC import
- **approve 无 body**：approver 身份从 `tenantctx` 取，不信任 client 传的 user_id
- **reject 容错 decode**：reason 可空，decode 失败仍能 reject（语义：拒绝不需要理由）

## 8. 注意事项

- **`reject` decode 错误被忽略**：`_ = json.NewDecoder(...).Decode(&in)`——这是故意的，reason 非必填；若未来要求必填 reason 需改此处
- **`List` 第三参数固定 0**：`h.uc.List(r.Context(), status, 0)` 第三个参数（limit?）硬编码 0，分页能力未暴露
- **无审计写入**：本 handler 不调 `auditmw.SetAuditEvent`——审计由 usecase 层负责（approve/reject 是业务操作）
- **`errCode` slug 表有限**：仅 5 个 sentinel；新增 sentinel 需扩展
- **`caller` 类型本地定义**：不 import service 层，避免 BC 跨界；与 `server/secret` 镜像
