# `orgs.go` 技术实现文档

> 源文件：`internal/iam/server/orgs.go`
> 包路径：`github.com/ongridio/ongrid/internal/iam/server`

## 1. 概述

本文件是 IAM BC HTTP 层的 Phase-1 企业功能扩展，承载 org CRUD、成员管理、富化 `/v1/me`、用户创建（含自动加入默认组织）与用户字段更新。所有 handler 方法 receiver 同 `http.go` 的 `Handler`，拆文件仅为 diff 友好。包含大量 DTO 定义与「未装配时 503 NotWiredYet」的降级处理。

## 2. 包信息

- **包名**：`server`（与 `http.go` 同包）
- **所属模块**：`internal/iam/server` —— HTTP 层企业端点
- **依赖方向**：被 `cmd/ongrid` 经 `http.go` 的 `RegisterProtected` 路由调用；依赖 `internal/iam/biz/user`、`biz/org`、`iam/model`、`pkg/errs`、`pkg/tenantctx`

## 3. 关键类型与接口

DTO 集合：
- `orgDTO` / `orgListResp`：org 列表与单项响应。
- `membershipDTO` / `membershipListResp`：成员列表响应。
- `orgMembershipForUserDTO`：用户视角的成员关系（org_id + org_name + role）。
- `meDTO`：富化的自查响应，含 memberships 数组。
- `fullUserDTO`：完整用户信息（含 status / timestamps）。
- `createOrgReq` / `updateOrgReq` / `addMemberReq` / `updateMemberReq` / `createUserReq` / `updateUserReq` / `resetPasswordReq`：各端点请求体。

```go
type updateOrgReq struct {
	Name         string  `json:"name"`
	Description  string  `json:"description"`
	ParentIDSet  bool    `json:"parent_id_set,omitempty"`
	ParentID     *uint64 `json:"parent_id,omitempty"`
}
```

- `updateOrgReq.ParentIDSet`：前端「确实想改 parent」的显式信号，解决 JSON null 与「字段未传」的 zero-value 歧义。

## 4. 关键函数与流程

### `me`
- **签名**：`func (h *Handler) me(w, r)`
- **职责**：富化自查，含用户基本信息 + 所属 org 列表。
- **流程**：从 `tenantctx` 取 userID → `svc.GetByID` → 组装 meDTO（Memberships 初始化为空切片避免 null）→ `svc.MembershipsByUser` 取成员关系（失败则忽略，仅返回用户）→ 写响应。

### org CRUD：`listOrgs` / `createOrg` / `updateOrg` / `deleteOrg`
- **流程**：均先 `requireAdmin`（list 仅需认证）→ `requireOrgsService`（nil 则 503）→ 调 svc → 写响应。
- **`createOrg`**：将 `createOrgReq` 映射为 `org.CreateInput` 调 svc.Create。
- **`updateOrg`**：将 `updateOrgReq` 映射为 `org.UpdateInput`（`ParentIDSet` → `SetParent`）。
- **`deleteOrg`**：直接调 svc.Delete。

### 成员管理：`listOrgMembers` / `addOrgMember` / `updateOrgMember` / `removeOrgMember`
- **流程**：均先 `requireAdmin`（list 仅需认证）→ 取 `svc.Memberships()`（nil 则 503）→ parseID + parseUserParam → 调 svc → 写响应。
- **`addOrgMember`**：校验 `UserID != 0` 后调 `AddOrUpdate`，返回 201 + 创建的成员关系。
- **`updateOrgMember`**：复用 `AddOrUpdate` 实现「改 role = upsert」，返回 204。
- **`removeOrgMember`**：调 `Remove`，返回 204。

### 用户扩展：`createUser` / `updateUser` / `resetPassword`
- **`createUser`**：
  1. `requireAdmin` + decode `createUserReq`。
  2. 映射为 `user.CreateInput` 调 `svc.User().Create`。
  3. 若 `!SkipDefaultOrg`：取 `svc.Orgs()` → `EnsureSeed("默认组织", "")` → `svc.Memberships().AddOrUpdate(u.ID, seed.ID, "member")`，失败仅 warn 不阻断用户创建。
  4. 返回 fullUserDTO。
- **`updateUser`**：先 `GetByID` 取当前值 → 按 `updateUserReq` 的指针字段决定是否更新 profile/status → 再次 GetByID 返回最新值。
- **`resetPassword`**：调 `svc.User().ResetPassword`，返回 204。

### 辅助函数
- **`toOrgDTO` / `toFullUserDTO`**：model → DTO 转换。
- **`parseUserParam`**：从 chi URL param `user_id` 解析 uint64。
- **`requireOrgsService`**：从 svc 取 Orgs，nil 则写 503 并返回 nil。

## 5. 依赖关系

- **内部包**：`internal/iam/biz/user`、`internal/iam/biz/org`、`internal/iam/model`、`internal/pkg/errs`、`internal/pkg/tenantctx`
- **外部库**：`github.com/go-chi/chi/v5`、标准库 `errors` / `net/http` / `strconv` / `time`
- **被调用方**：`http.go` 的 `RegisterProtected` 注册路由后被 chi 调度

## 6. 并发与资源管理

无显式锁；Handler 共享自 `http.go`，限流器仅在登录路径用。所有方法首参 `context.Context` 透传至 svc。

## 7. 设计模式与亮点

- **DTO 与 model 解耦**：所有出参经 DTO 转换，避免泄漏 `PassHash` 等敏感字段（纵深防御，biz 层已清空）。
- **`ParentIDSet` 消歧义**：JSON null 与字段未传在 zero-value 上无法区分，显式 bool 信号解决 PATCH 语义。
- **`requireOrgsService` 降级**：Phase-1 前的旧部署未装配 orgs 服务时返回 503 而非 panic，保证向后兼容。
- **`meDTO.Memberships` 初始化为空切片**：避免 JSON null 在前端引发空指针。
- **`createUser` 自动加入默认 org**：新用户默认加入「默认组织」为 member，避免无 membership 用户被 casbin 全拒；`SkipDefaultOrg` 提供退出选项。
- **`updateUser` 按指针字段部分更新**：`*string` nil 表示不改，非 nil 表示更新，符合 PATCH 语义。
- **`updateOrgMember` 复用 AddOrUpdate**：改 role = upsert，避免单独实现 update 路径。

## 8. 注意事项

- `createUser` 中自动加入默认 org 的失败仅 warn 不阻断用户创建，可能产生「无成员关系的孤儿用户」，需运营监控告警。
- `me` 中 `MembershipsByUser` 失败被静默忽略，用户将看到空 memberships；需评估是否应返回错误。
- `createUser` 多步操作（创建用户 + 加入默认 org）非事务，部分失败会留下不一致状态。
- `updateUser` 先读后写，存在 TOCTOU 竞态；并发同用户更新可能丢失字段，依赖前端避免并发提交。
- `updateOrgReq.ParentIDSet` 是前端层面的信号约定，若调用方不遵守会导致 parent 误改。
- `requireOrgsService` / `Memberships()` nil 检查散落各 handler，若未来服务装配稳定可考虑收敛到中间件。
- DTO 中 `display_name,omitempty` 在空字符串时省略字段，前端需处理 undefined。
- 审计事件未在本文件设置（仅 http.go 的 register/delete/setRole 设置），org/member 操作的审计依赖调用方或后续补充。
