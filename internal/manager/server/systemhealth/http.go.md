# systemhealth/http.go 技术实现文档

## 1. 概述

`http.go` 是 ongrid manager 平台健康检查子域（`/v1/system/health`）的 HTTP 路由层。提供 2 个端点（GET + POST 同一 handler）：检查平台各组件（Loki / Prom / DB / 等）健康状态。admin-only。

## 2. 包信息

- **包名**：`systemhealth`
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/server/systemhealth`
- **路由前缀**：`/v1/system/health`（由 `cmd/ongrid` 挂载，auth 中间件由上游注入）
- **文件定位**：HTTP 适配层（薄 handler —— admin 校验 + delegate to biz Service）

## 3. 关键类型与接口

### HealthService —— 窄接口

```go
type HealthService interface {
    Check(ctx context.Context, caller alertsvc.Caller) (*healthsvc.Report, error)
}
```

**关键**：`Check` 接收 `alertsvc.Caller`（来自 `internal/manager/service/alert`），不是本子域自定义 caller——可能因为 health 检查复用 alert 服务的 caller 类型。

### Handler

```go
type Handler struct {
    svc HealthService
}
```

## 4. 关键函数与流程

### Register —— 路由表

```go
func (h *Handler) Register(r chi.Router)
```

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/v1/system/health` | admin | 检查平台健康 |
| POST | `/v1/system/health/check` | admin | 同上（POST 显式触发） |

两个路由共用同一 `check` handler——GET 用于查询，POST 用于显式触发检查（语义等价，让前端可用任一方法）。

### check

```go
func (h *Handler) check(w http.ResponseWriter, r *http.Request)
```

1. `requireAdmin` —— 取 `alertsvc.Caller`，admin-only
2. `h.svc == nil` → `errs.ErrNotWiredYet`（503）
3. `h.svc.Check(ctx, caller)` → 200 + `*healthsvc.Report`

### helpers

- `requireAdmin(w, r)` —— `tenantctx.From` + `Role == "admin"`，返 `(alertsvc.Caller, bool)`
- `writeJSON` / `writeErr` —— 标准 errs 映射

**注意**：`writeErr` 的 default 是 `http.StatusBadGateway` + `upstream` slug（不是 500 internal）——因为 health 检查失败通常是上游组件 down，而非本服务 internal error。

## 5. 依赖关系

**外部**：
- `chi` —— 路由
- `net/http`、`encoding/json`、`errors`、`context`

**内部**：
- `internal/manager/service/alert`（Caller 类型）
- `internal/manager/service/systemhealth`（Report 类型）
- `internal/pkg/errs`
- `internal/pkg/tenantctx`

## 6. 并发与资源管理

- **无共享可变状态**：`Handler.svc` 启动时装配
- **请求级隔离**：每请求独立 ctx
- **`h.svc == nil` graceful degradation**：未 wire 时返 503
- **无 goroutine 启动**：同步调 svc.Check
- **`writeJSON` swallow encode 错误**：响应已开始无法回传

## 7. 设计模式与亮点

1. **GET + POST 共用 handler**：语义等价，前端可用任一方法——GET 便于 curl 测试，POST 显式触发
2. **`h.svc == nil` → 503**：graceful degradation，未 wire 时明确告知前端
3. **default 502 `upstream` slug**：health 检查失败通常是上游组件 down，502 比 500 更准确
4. **`requireAdmin` 返 alertsvc.Caller**：复用 alert 服务 caller 类型，避免本子域自定义
5. **极简实现**：单文件 95 行，无 DTO 定义（直接返 svc.Report）

## 8. 注意事项

1. **`requireAdmin` 返 `alertsvc.Caller`**：与其它子域的本地 caller 类型不同；如 alert 服务 Caller 变化需同步
2. **`roleAdmin` 字符串硬编码**：`t.Role != "admin"` 直接比较，避免跨 BC import
3. **`writeErr` default 502**：与其它子域 default 500 不同——本子域假设错误来自上游；如本服务 internal error 也会被误判为 502
4. **无显式 timeout**：依赖 svc 层 / 上游 ctx 控制；health 检查可能慢（多组件探测），建议加 timeout
5. **`Check` 接收 caller**：让 svc 层可按 caller 决定检查范围（如 non-admin 只查部分组件）——但本层已 admin-only，caller 总是 admin
6. **无审计**：health 检查是只读操作，不写审计
7. **`Report` schema 由 svc 层决定**：handler 透传，前端按 svc 层定义解析
8. **GET 与 POST 语义等价**：未区分查询 vs 触发；如需区分（如 POST 强制刷新缓存）需拆 handler
9. **`errs.ErrNotWiredYet` 503**：与 prometheus 子域同模式，让前端统一处理"未 wire"状态
10. **无 body**：GET/POST 都不接 body，纯触发式端点
