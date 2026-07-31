# `http.go` 技术实现文档

> 源文件：`internal/manager/server/device/http.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/server/device`

## 1. 概述

本文件是 manager/device 子域（May 2026 entity split 后从 edge 拆出）的 HTTP 路由层：暴露 `/v1/devices` 的 list / get / patch / delete / edges 端点。设计要点：device 承载 host facts（hostname、OS、CPU/mem/disk 容量与实时使用率）+ operator 分配的 roles 位集；roles filter 支持 `unknown`（未分类）与具名 role 的互斥过滤。关键红线：`roleAdmin` 常量本地定义（镜像 `iam/model.RoleAdmin`，arch-lint 禁止 manager → iam import）；写操作（PATCH/DELETE）走 `requireAdmin` 中间件。

## 2. 包信息

- **包名**：`device`
- **所属模块**：`internal/manager/server/`（HTTP handler 层）
- **依赖方向**：被上层 router 装配调用 `NewHandler` + `Register`；依赖 `biz/device`（`Usecase`、`ListFilter`、`Repo`）、`model/device`（`Device`、roles 编解码）、`pkg/errs`、`pkg/tenantctx`

## 3. 关键类型与接口

```go
const roleAdmin = "admin" // 镜像 iam/model.RoleAdmin，避免跨界 import

type Handler struct {
    uc *devicebiz.Usecase
}

// DTO：device 列表/详情项
type deviceItem struct {
    ID, Name, Description, Hostname, OS, OSVersion, Arch, KernelVersion, IPAddress string/int/...
    CPUCount int
    MemTotalBytes, DiskTotalBytes uint64
    CPUUsagePct, MemUsagePct, DiskUsagePct float32
    Roles []string
    Online bool
    LastSeenAt *time.Time
    CreatedAt time.Time
    NodeID *uint64 // 拓扑 nodes 表外键，可空（backfill 未跑）
}

type listResp struct { Items []deviceItem; Total int }
type updateReq struct { Name *string; Description *string }       // 可选字段 patch
type updateRolesReq struct { Roles []string }
type edgeLinkRow struct { EdgeID, DeviceID uint64; Type string; CreatedAt time.Time } // host | discovered
```

## 4. 关键函数与流程

### `NewHandler` / `Register`
- **签名**：`func NewHandler(uc *devicebiz.Usecase) *Handler` + `func (h *Handler) Register(r chi.Router)`
- **职责**：挂载 6 个路由
- **流程**：
  - `GET /v1/devices`、`GET /v1/devices/{id}`、`GET /v1/devices/{id}/edges` —— 任意已认证
  - `PATCH /v1/devices/{id}`、`PATCH /v1/devices/{id}/roles`、`DELETE /v1/devices/{id}` —— `r.With(h.requireAdmin)` 包裹

### `requireAdmin`（中间件）
- **签名**：`func (h *Handler) requireAdmin(next http.Handler) http.Handler`
- **职责**：403 非 admin；401 无 tenant
- **流程**：`tenantctx.From` 失败 → 401；`t.Role != roleAdmin` → 403；否则 next

### `list`
- **职责**：`GET /v1/devices?hostname=&name=&roles=&online=&limit=&offset=`
- **流程**：
  1. tenantctx 校验
  2. 构造 `ListFilter`：Hostname/Name
  3. **roles filter 解析**：逗号分隔；遇 `unknown` → `unknownOnly=true`；具名 role → `IsValidRoleName` 校验 + `EncodeRoles` 累加 mask；`unknown` 与具名混用 → 400「cannot combine 'unknown' with named roles」
  4. online filter：`true`/`1`/`false`/`0`
  5. limit/offset 解析
  6. `h.uc.List(ctx, f)` → 翻译成 `deviceItem` → 200 `{items, total: len(items)}`
- **错误处理**：未知 role 名 → 400；roles 混用 → 400

### `get`
- **职责**：`GET /v1/devices/{id}`
- **流程**：tenantctx + parseID → `h.uc.Get` → `devToItem` → 200

### `update`
- **职责**：`PATCH /v1/devices/{id}` —— name / description 可选 patch
- **流程**：requireAdmin（中间件已过）→ parseID → decode `updateReq` → `h.uc.Get` 取当前 → 用 `in.Name`/`in.Description` 覆盖（nil 表示不改）→ `h.uc.UpdateNameDescription` → 204
- **关键**：先 Get 再 Update，为了支持部分字段 patch（nil 字段保留原值）

### `updateRoles`
- **职责**：`PATCH /v1/devices/{id}/roles` —— 整体替换 roles
- **流程**：requireAdmin → parseID → decode `{roles: [...]}` → `h.uc.UpdateRoles(ctx, id, req.Roles)` → 204

### `delete`
- **职责**：`DELETE /v1/devices/{id}`
- **流程**：requireAdmin → parseID → `h.uc.Delete` → 204

### `listEdges`
- **职责**：`GET /v1/devices/{id}/edges` —— 该 device 关联的 edge 连接
- **流程**：tenantctx + parseID → `h.uc.Links()` 取 link repo（可能 nil）→ `links.ListEdgesForDevice(ctx, id)` → 翻译成 `edgeLinkRow`（Type: host/discovered）→ 200 `{items: [...]}`
- **错误处理**：`h.uc.Links() == nil` → 返空列表（兼容未 wire link 的部署）

### helpers
- `devToItem`：model → DTO，含 `DecodeRoles` 把位集还原成 role 名切片
- `relType`：`EdgeDeviceRelationHost` → "host"，`Discovered` → "discovered"，其它 → "unknown"
- `parseID`：chi URLParam → uint64
- `writeJSON` / `writeErr` / `errCode`：标准 helper
- `var _ = context.Background`：编译期 guard，保 context import 不被 lint 删

## 5. 依赖关系

- **内部包**：`biz/device`（`Usecase`、`ListFilter`）、`model/device`（`Device`、`RoleUnknown`、`IsValidRoleName`、`EncodeRoles`、`DecodeRoles`、`EdgeDeviceRelation*`）、`pkg/errs`、`pkg/tenantctx`
- **外部库**：`github.com/go-chi/chi/v5`
- **被调用方**：上层 router 装配代码

## 6. 并发与资源管理

- **无共享状态**：Handler 仅持有 `uc` 指针
- **ctx 透传**：所有 usecase 调用透传 `r.Context()`
- **无缓存**：每次查询都打 DB

## 7. 设计模式与亮点

- **`roleAdmin` 本地常量**：注释明示「mirrors iam/model.RoleAdmin without crossing the BC boundary (arch-lint forbids manager -> iam imports)」——若 iam 改值需同步
- **roles filter 互斥语义**：`unknown`（未分类）与具名 role 不能混用，返 400 明确提示；这是 UX 友好的输入校验
- **部分字段 patch**：`updateReq` 用 `*string` 指针，nil 表示不改；先 Get 再 Update 保留未传字段
- **`listEdges` nil-safe**：`h.uc.Links()` 返 nil 时返空列表，兼容未 wire link repo 的部署
- **`NodeID` 可空**：注释明示「Nullable until topology.Migrate has run its backfill」——SPA 需处理 nil
- **`requireAdmin` 作为 chi middleware**：用 `r.With(h.requireAdmin).Patch(...)` 装配，比每个 handler 内部调 `requireAdmin` 函数更声明式

## 8. 注意事项

- **`roleAdmin` 字面量与 iam/model 耦合**：若 iam 改 admin 角色名需同步改此处常量
- **`list` 的 total 是 `len(items)`**：未调 CountIncidents 类的真实总数；若 sidebar badge 依赖真实总数需补
- **`update` 先 Get 再 Update**：存在 TOCTOU race（Get 后别人改了，Update 覆盖）；当前未加乐观锁
- **`updateRoles` 整体替换**：不是增量，client 需传完整 roles 列表
- **`listEdges` Type "unknown"**：`relType` default 返 "unknown"，新增 relation 类型需扩展
- **`var _ = context.Background`**：编译期 guard，保 context import——若未来真用到 context 可删
- **roles 编解码在 model/device**：`EncodeRoles`/`DecodeRoles` 是位集操作，新增 role 需扩展位定义
