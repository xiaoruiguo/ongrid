# middleware/audit.go 技术实现文档

## 1. 概述

`audit.go` 实现了 ongrid manager 的 HTTP 审计中间件（HLD-010）。它**只记录显式标注的用户动作**（handler 调 `SetAuditEvent` 写入），不再为未标注的变更请求生成 `http_<method>_<resource>` fallback（运维反馈该类条目无用）。中间件在请求 ctx 中安装一个可变 `*auditSlot` 指针，让 handler 链中任何位置（即便经过 auth/otel 等 `r.WithContext` 重包装）都能写入事件。

## 2. 包信息

- **包名**：`middleware`（与 `metrics.go` 同包）
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/server/middleware`
- **文件定位**：HTTP 中间件 + ctx 槽位工具
- **使用方**：`cmd/ongrid` 在 chi router 装配时挂载；handler 调 `SetAuditEvent` 写入

## 3. 关键类型与接口

### auditContextKey + auditSlot —— ctx 槽位

```go
type auditContextKey struct{}

type auditSlot struct {
    ev  audit.Event
    set bool
}
```

**关键设计**：ctx value 是 `*auditSlot` 指针而非 `Event` 值——指针在 `r.WithContext` 重包装后仍指向同一个 slot，handler 写入对中间件可见。早期实现（直接 set Event 到 `*r`）在任何中间件调 `r.WithContext` 后失效。

### 公开 API

```go
// handler 调用：写入待审计事件
func SetAuditEvent(r *http.Request, ev audit.Event)

// 任意位置读取：取已写入的事件
func GetAuditEvent(ctx context.Context) (audit.Event, bool)

// 中间件构造器
func AuditMiddleware(uc *audit.Usecase) func(http.Handler) http.Handler
```

`SetAuditEvent` 在 slot 未安装时 no-op，安全可在外部链路调用。

## 4. 关键函数与流程

### AuditMiddleware —— 主中间件

```go
func AuditMiddleware(uc *audit.Usecase) func(http.Handler) http.Handler
```

执行流程：
1. `chimw.NewWrapResponseWriter(w, r.ProtoMajor)` 包装 w 以捕获 status
2. **安装 auditSlot**：`slot := &auditSlot{}` + `ctx = context.WithValue(r.Context(), auditContextKey{}, slot)`
3. **安装 tenantctx slot**：`ctx = tenantctx.WithSlot(ctx)`——让链中更深处的 `auth.Middleware` 能写入 tenant，本中间件 post-handler 时可见
4. `next.ServeHTTP(ww, r.WithContext(ctx))` 执行内层链
5. post-handler 检查：
   - `uc == nil`（审计未启用）或 `!slot.set`（handler 未标注）→ 不记录
   - 否则 `enrichFromRequest(&slot.ev, r, ctx, ww.Status())` 补全字段 + `uc.Emit(ctx, slot.ev)` 发送

### enrichFromRequest —— 字段补全

```go
func enrichFromRequest(ev *audit.Event, r *http.Request, ctx context.Context, status int)
```

按"已设则不覆盖"原则补全：
- `tenantctx.From(ctx)` 取 tenant，补 `UserID` / `UserEmail` / `Role`
- `clientIP(r)` 补 `IP`
- `truncate(r.UserAgent(), 512)` 补 `UserAgent`（截断 512）
- `chimw.GetReqID(r.Context())` 补 `RequestID`
- `statusBucket(status)` 补 `Status`

**关键**：传入的是携带 slot 的 `ctx`（不是 `r.Context()`），因为外层 `r.Context()` 没有 tenant slot。

### statusBucket —— 状态分桶

```go
func statusBucket(status int) string
```

- `>= 500` → `StatusFailure`
- `== 403` → `StatusDenied`（鉴权拒绝，独立于普通失败）
- `>= 400` → `StatusFailure`
- 其它 → `StatusSuccess`

### clientIP —— 客户端 IP 提取

```go
func clientIP(r *http.Request) string
```

`X-Forwarded-For` 第一跳（nginx 已剥离/替换，ADR-008），否则 `RemoteAddr` 的 host 部分。

### truncate —— 字符串截断

```go
func truncate(s string, n int) string
```

简单字节截断，用于 UserAgent 等长字段防数据库溢出。

## 5. 依赖关系

**外部**：
- `chi/middleware` —— `NewWrapResponseWriter`、`GetReqID`
- `net/http`、`context`、`net`、`strings`

**内部**：
- `internal/manager/biz/audit`（Usecase + Event 类型）
- `internal/manager/model/audit`（Status 常量）
- `internal/pkg/tenantctx`（tenant 槽位 + 读取）

## 6. 并发与资源管理

- **每请求独立 slot**：`slot := &auditSlot{}` 在中间件入口创建，请求结束随 ctx GC
- **指针共享跨重包装**：slot 是指针，所有 `r.WithContext` 重包装后仍指向同一 slot
- **`uc.Emit` 同步执行**：在 next.ServeHTTP 返回后调用，会阻塞响应发送——若 Emit 慢（如 DB 写入）会增加响应延迟。考虑改为异步队列（如 biz 层已有缓冲则 OK）
- **无 goroutine 启动**：本中间件本身无 goroutine

## 7. 设计模式与亮点

1. **指针槽位跨 ctx 重包装**：ctx value 是 `*auditSlot` 而非 `Event`，解决"任何中间件 `r.WithContext` 后 set 失效"的早期 bug。这是经过 bug 才加的设计
2. **双重 slot 安装**：同时安装 auditSlot 和 tenantctx slot——auditSlot 让 handler 写事件，tenantctx slot 让 auth 中间件写 tenant，post-handler 时 audit 中间件可见
3. **显式标注才审计**：`SetAuditEvent` 是 opt-in，未标注的请求不记录——`audit_logs` 是用户有意义动作的策展轨迹，不是访问日志
4. **`enrichFromRequest` 已设则不覆盖**：handler 显式 set 的字段（如 `auth_login` 的 user_id）优先，中间件只补缺失
5. **403 独立 StatusDenied**：鉴权拒绝与普通 4xx 失败分离，便于安全分析
6. **XFF 第一跳**：信任 nginx 已剥离/替换 XFF（ADR-008），取第一跳作为真实 client IP
7. **UserAgent 截断 512**：防长 UA 撑爆数据库字段
8. **`uc == nil` graceful degradation**：审计未配置时中间件仍可挂载，仅跳过 Emit
9. **`GetAuditEvent` 公开**：测试或其它中间件可读取已写入事件

## 8. 注意事项

1. **`uc.Emit` 同步阻塞**：在响应发送前调用，若 Emit 慢会增加延迟；高 QPS 场景需确认 biz 层是否有缓冲/异步
2. **`SetAuditEvent` 在 slot 未安装时 no-op**：handler 在测试或非审计链路调用是安全的，但意味着审计可能静默丢失——若关键操作未生效需检查中间件是否挂载
3. **`tenantctx.WithSlot` 必须在 audit 中间件安装**：auth.Middleware（更深链）依赖该 slot 写入 tenant；若顺序错误 audit 行会缺 user_id/email
4. **`clientIP` 信任 XFF**：依赖 nginx 剥离/替换（ADR-008）；若部署在非 nginx 后面需重新评估
5. **`statusBucket` 403 独立**：与 401（unauthorized）和其它 4xx 区分；如需 401 也独立需扩 case
6. **`truncate` 字节截断**：非 rune 截断，多字节 UTF-8 UA 可能被截断在中途产生无效 UTF-8；如需严格应改 rune 截断
7. **审计不覆盖所有变更**：handler 必须显式调 `SetAuditEvent`，遗漏则不记录；新增 handler 时需检查是否需要审计
8. **已 wired 的 Action 列表见注释**：`auth_login` / `audit_view` / alert/rule/channel/knowledge/user/settings CRUD——新增 Action 应在注释中补充便于检索
9. **`uc == nil || !slot.set` 提前返回**：跳过 enrichFromRequest，避免无意义字段补全
10. **post-handler 才 Emit**：事件在响应发送前构造但 Emit 在 next 返回后，确保 status 已知
