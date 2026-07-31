# systemupgrade/http.go 技术实现文档

## 1. 概述

`http.go` 是 ongrid manager 平台升级检查子域（`/v1/system/upgrade`）的 HTTP 路由层。提供 2 个端点（GET + POST 同一 handler）：检查平台是否有新版本可升级。admin-only。包含 godoc Swagger 注释。

## 2. 包信息

- **包名**：`systemupgrade`
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/server/systemupgrade`
- **路由前缀**：`/v1/system/upgrade`（由 `cmd/ongrid` 挂载，auth 中间件由上游注入）
- **文件定位**：HTTP 适配层（薄 handler —— admin 校验 + delegate to biz Service）

## 3. 关键类型与接口

### UpgradeService —— 窄接口

```go
type UpgradeService interface {
    Check(ctx context.Context) (*upgradesvc.Info, error)
}
```

**关键**：`Check` 不接收 caller——升级检查不依赖 caller 信息（admin 已在上游校验）。

### 类型别名

```go
type Info = upgradesvc.Info
```

把 svc 层的 `Info` 类型别名到本包，便于 godoc Swagger 注释引用 `systemupgrade.Info`。

### Handler

```go
type Handler struct {
    svc UpgradeService
}
```

## 4. 关键函数与流程

### Register —— 路由表

```go
func (h *Handler) Register(r chi.Router)
```

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/v1/system/upgrade` | admin | 检查升级信息 |
| POST | `/v1/system/upgrade/check` | admin | 同上（POST 显式触发） |

两个路由共用同一 `check` handler——与 systemhealth 同模式。

### check —— 含 godoc 注释

```go
// check godoc
// @Summary Check platform upgrade
// @Router /v1/system/upgrade [get]
// @Router /v1/system/upgrade/check [post]
// @Success 200 {object} systemupgrade.Info
func (h *Handler) check(w http.ResponseWriter, r *http.Request)
```

1. `requireAdmin` —— admin-only，返 bool（不返 caller，因为 svc.Check 不需要）
2. `h.svc == nil` → `errs.ErrNotWiredYet`（503）
3. `h.svc.Check(ctx)` → 200 + `*upgradesvc.Info`

### helpers

- `requireAdmin(w, r)` —— `tenantctx.From` + `Role == "admin"`，返 bool（不返 caller）
- `writeJSON` / `writeErr` —— 标准 errs 映射

**注意**：与 systemhealth 一样，`writeErr` default 是 `http.StatusBadGateway` + `upstream` slug——升级检查失败通常是上游（升级源）不可达。

## 5. 依赖关系

**外部**：
- `chi` —— 路由
- `net/http`、`encoding/json`、`errors`、`context`

**内部**：
- `internal/manager/service/systemupgrade`（Info 类型）
- `internal/pkg/errs`
- `internal/pkg/tenantctx`

## 6. 并发与资源管理

- **无共享可变状态**：`Handler.svc` 启动时装配
- **请求级隔离**：每请求独立 ctx
- **`h.svc == nil` graceful degradation**：未 wire 时返 503
- **无 goroutine 启动**：同步调 svc.Check
- **`writeJSON` swallow encode 错误**：响应已开始无法回传

## 7. 设计模式与亮点

1. **GET + POST 共用 handler**：与 systemhealth 同模式，前端可用任一方法
2. **godoc Swagger 注释**：`@Summary` / `@Router` / `@Success` 齐全——符合 ongrid API 规范（Handler 必须有 Swagger 注释）
3. **`type Info = upgradesvc.Info` 别名**：让 Swagger 注释引用 `systemupgrade.Info` 而非 svc 层路径，保持包内自包含
4. **`requireAdmin` 返 bool 不返 caller**：因为 svc.Check 不需要 caller，简化 helper 签名
5. **`h.svc == nil` → 503**：graceful degradation，未 wire 时明确告知前端
6. **default 502 `upstream` slug**：升级检查失败通常是升级源不可达，502 比 500 更准确
7. **极简实现**：单文件 100 行，无 DTO 定义（直接返 svc.Info）

## 8. 注意事项

1. **`roleAdmin` 字符串硬编码**：`t.Role != "admin"` 直接比较，避免跨 BC import
2. **`writeErr` default 502**：与 systemhealth 同模式，假设错误来自上游；本服务 internal error 也会被误判为 502
3. **无显式 timeout**：升级检查可能涉及网络请求（检查升级源），建议加 timeout
4. **`Check` 不接收 caller**：与 systemhealth 不同（health 接收 caller）；如需按 caller 决定检查范围需扩接口
5. **`Info` 类型别名**：让 Swagger 注释引用本包类型；如 svc 层 Info 字段变化需同步
6. **无审计**：升级检查是只读操作，不写审计
7. **GET 与 POST 语义等价**：未区分查询 vs 触发；如需区分（如 POST 强制刷新缓存）需拆 handler
8. **`errs.ErrNotWiredYet` 503**：与 prometheus/systemhealth 同模式
9. **无 body**：GET/POST 都不接 body，纯触发式端点
10. **`Info` schema 由 svc 层决定**：handler 透传，前端按 svc 层定义解析（通常含 current_version / latest_version / upgrade_available 等）
