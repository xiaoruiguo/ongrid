# report/http.go 技术实现文档

## 1. 概述

`http.go` 是 ongrid manager 定时报告子域（HLD-014）的 HTTP 路由层。提供报告列表/详情/删除/分享/立即生成，schedule 的 CRUD + toggle + run-now，以及 HLD-022 Phase 2 的统一任务（task）surface。RBAC（ADR-022）：admin/user 可写，viewer 只读。公开分享路由 `/r/{token}` 单独挂载无 auth。

## 2. 包信息

- **包名**：`report`
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/server/report`
- **路由**：
  - `Register` —— `/v1/reports` + `/v1/report-schedules` + `/v1/tasks`（auth 中间件保护）
  - `RegisterPublic` —— `/r/{token}`（无 auth 中间件，公开分享）
- **文件定位**：HTTP 适配层（薄 handler —— auth + JSON decode + delegate to biz Usecase）

## 3. 关键类型与接口

### Handler

```go
type Handler struct {
    uc  *bizreport.Usecase
    now func() time.Time // 注入时钟，便于测试
}
```

**关键**：直接持有 `*bizreport.Usecase`（具体类型，非接口）——与其它子域的窄接口模式不同，因为 report 子域调用方法多且 biz 层稳定。`now func() time.Time` 注入便于测试时间相关逻辑。

### 请求 DTO

```go
type generateNowReq struct {
    Kind      string `json:"kind"`
    Timezone  string `json:"timezone"`
    ScopeJSON string `json:"scope_json"`
}

type scheduleReq struct { /* 见 dto.go */ }

type toggleReq struct {
    Enabled bool `json:"enabled"`
}
```

### 常量

```go
const roleViewer = "viewer"
```

本地常量镜像 RBAC 角色名，避免跨 BC import。

## 4. 关键函数与流程

### Register —— 路由表

```go
func (h *Handler) Register(r chi.Router)
```

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/v1/reports` | any auth | 列报告（filter by status/kind/schedule_id/task_id） |
| POST | `/v1/reports` | writer | 立即生成报告 |
| GET | `/v1/reports/{id}` | any auth | 报告详情 |
| DELETE | `/v1/reports/{id}` | writer | 删除报告 |
| POST | `/v1/reports/{id}/share` | writer | mint 分享 token |
| GET | `/v1/report-schedules` | any auth | 列 schedule |
| POST | `/v1/report-schedules` | writer | 创建 schedule |
| GET | `/v1/report-schedules/{id}` | any auth | schedule 详情 |
| PUT | `/v1/report-schedules/{id}` | writer | 更新 schedule |
| DELETE | `/v1/report-schedules/{id}` | writer | 删除 schedule |
| POST | `/v1/report-schedules/{id}/toggle` | writer | 启用/禁用 |
| POST | `/v1/report-schedules/{id}/run-now` | writer | 立即触发 |
| GET | `/v1/tasks` | any auth | 统一任务列表（HLD-022） |
| POST | `/v1/tasks/oneoff` | writer | 创建一次性任务 |
| GET | `/v1/tasks/{id}` | any auth | 任务详情 |
| POST | `/v1/tasks/{id}/run` | writer | 重跑 oneoff 任务 |
| DELETE | `/v1/tasks/{id}` | writer | 删除 oneoff 任务 |

### RegisterPublic —— 公开分享路由

```go
func (h *Handler) RegisterPublic(r chi.Router)
```

`GET /r/{token}` —— 无 auth 中间件，30d TTL，只读。

### requireWriter —— RBAC 中间件

```go
func (h *Handler) requireWriter(next http.Handler) http.Handler
```

拒绝 `role == "viewer"` 的 caller（ADR-022 只读层），admin/user 通过。

### listReports

```go
func (h *Handler) listReports(w http.ResponseWriter, r *http.Request)
```

支持 filter：`status` / `kind` / `limit`（默认 50）/ `offset`（默认 0）/ `schedule_id`（按 schedule 范围）/ `task_id`（HLD-022 规范 key，覆盖定时 + run-now）。

### generateNow —— 立即生成

```go
func (h *Handler) generateNow(w http.ResponseWriter, r *http.Request)
```

1. `tenantctx.From` 取 caller（含 user_id）
2. `json.Decode(&generateNowReq)`，默认 `Kind = weekly`、`Timezone = UTC`
3. `time.LoadLocation(timezone)` 校验时区
4. `bizreport.PeriodFor(kind, now, loc, zero)` 计算报告周期
5. `h.uc.GenerateNow(ctx, userID, kind, tz, scope, locale, "", period)` 异步生成
6. 202 Accepted + 报告详情（含 status=pending/running）

### shareReport / sharedReport

```go
func (h *Handler) shareReport(w http.ResponseWriter, r *http.Request)  // mint token
func (h *Handler) sharedReport(w http.ResponseWriter, r *http.Request) // 公开访问
```

分享返 `{share_token, path: "/r/" + token}`；公开访问用 `GetSharedReport` 验证 token + TTL。

### schedule CRUD

- `createSchedule` —— `req.toModel(userID)` 构造 + `uc.CreateSchedule(ctx, s, now)` → 201
- `updateSchedule` —— `uc.GetSchedule` 取 existing + `req.applyTo(existing)` mutate + `uc.UpdateSchedule` → 200
- `deleteSchedule` —— 204 No Content
- `toggleSchedule` —— `req.Enabled` bool + `uc.SetScheduleEnabled` → 200
- `runNow` —— `uc.RunNow(ctx, id, locale, now)` → 202 Accepted

### helpers

- `authed(w, r)` —— tenantctx 存在性检查，返 bool
- `pathID(r)` —— `ParseUint(chi.URLParam("id"))`
- `atoiDefault(s, def)` —— 带默认值的 Atoi
- `localeFromRequest(r)` —— 从 `Accept-Language` 头取 `en`/`zh`，未知返空（biz 层 DefaultLocale 兜底）
- `var _ = context.Background` —— 编译期 guard

## 5. 依赖关系

**外部**：
- `chi` —— URL 参数
- `net/http`、`encoding/json`、`strconv`、`strings`、`time`、`errors`、`context`

**内部**：
- `internal/manager/biz/report`（Usecase + ReportFilter + PeriodFor）
- `internal/manager/model/report`（Report / ReportSchedule / Kind 常量）
- `internal/pkg/errs`
- `internal/pkg/tenantctx`

## 6. 并发与资源管理

- **`now func() time.Time` 注入**：便于测试时间相关逻辑（PeriodFor / TTL）
- **`uc *bizreport.Usecase` 具体类型**：直接持有，无接口隔离——因为 report 子域调用方法多且 biz 层稳定
- **请求级隔离**：每请求独立 ctx
- **`writeJSON` swallow encode 错误**：响应已开始无法回传
- **异步生成**：`GenerateNow` / `RunNow` 返 202 Accepted，实际生成在 biz 层异步

## 7. 设计模式与亮点

1. **`requireWriter` RBAC 中间件**：拒绝 viewer 写操作，admin/user 通过——ADR-022 三层 RBAC
2. **`now func() time.Time` 注入**：可测试时钟，避免生产代码 `time.Now()` 散落
3. **`localeFromRequest` 从 Accept-Language 取语言**：报告内容按操作者 UI 语言生成（en/zh），与 alert/http.go 镜像
4. **`generateNow` 异步返 202**：报告生成耗时，立即返 pending 报告，前端轮询状态
5. **`task_id` 作为规范 filter key**（HLD-022）：覆盖定时 + run-now，是任务详情页统一入口
6. **`scheduleReq.applyTo` PATCH 语义**：详见 dto.go 文档
7. **公开分享路由单独挂载**：`RegisterPublic` 不带 auth 中间件，token 自带鉴权
8. **`roleViewer` 本地常量**：避免跨 BC import iam 包
9. **`var _ = context.Background` 编译期 guard**：防御性 import 保留
10. **`uc` 直接持有而非窄接口**：与其它子域不同，report 子域 biz 稳定且方法多，省接口分离开销

## 8. 注意事项

1. **`uc` 直接持有 `*bizreport.Usecase`**：测试时需替换整个 Usecase 或注入 fake——不如窄接口灵活
2. **`roleViewer` 字符串硬编码**：与其它子域的 `roleAdmin` 同模式，避免跨 BC import
3. **`generateNow` 默认 weekly/UTC**：前端不传时按周报 + UTC 生成；如需其它周期需显式传
4. **`time.LoadLocation` 校验时区**：无效时区返 400，避免后续 PeriodFor 报错
5. **`runNow` / `generateNow` 返 202**：前端需轮询报告状态（pending → running → success/failed）
6. **`sharedReport` 无 auth 中间件**：完全依赖 token 鉴权；token 泄露 = 任意人可读，TTL 30d 后失效
7. **`requireWriter` 不检查 admin**：admin/user 都通过，仅拒绝 viewer；如需 admin-only 需另加检查
8. **`listReports` 默认 limit 50**：大用户量需分页，offset + limit
9. **`localeFromRequest` 仅识别 en/zh**：其它语言返空，biz 层 DefaultLocale 兜底
10. **`createSchedule` 返 201**：与 RESTful 约定一致；`updateSchedule` 返 200 + 更新后的资源
11. **`deleteSchedule` 返 204**：与 `deleteReport` 返 204 一致；但 `deleteTask`（task.go）也返 204
12. **无审计**：本文件未调 `SetAuditEvent`，报告/schedule 变更不写审计；如需追溯需补 hook
