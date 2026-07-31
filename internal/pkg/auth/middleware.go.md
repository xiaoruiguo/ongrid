# `middleware.go` 技术实现文档

> 源文件：`internal/pkg/auth/middleware.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/auth`

## 1. 概述

该文件实现 ongrid 的 HTTP 认证中间件：从请求头或查询参数中提取 JWT，校验后将 `tenantctx.Tenant` 写入请求上下文（context）。下游 handler 通过 `tenantctx.From(ctx)` 获取调用者身份。中间件不做任何 DB lookup，也不做按路由的角色判断——后者由 iam/manager 的具体 handler 负责。

## 2. 包信息

- **包名**：`auth`
- **所属模块**：`internal/pkg/`（基础设施层）
- **依赖方向**：被 `cmd/ongrid` / `cmd/ongrid-edge` 在 chi 路由装配时调用；依赖 `internal/pkg/tenantctx`。

## 3. 关键类型与接口

无显著类型定义（仅有 `Middleware` 工厂函数）。

## 4. 关键函数与流程

### `Middleware`
- **签名**：`func Middleware(signer *Signer) func(http.Handler) http.Handler`
- **职责**：返回 chi 兼容的认证中间件。
- **流程**：
  1. `extractBearer(r)` 提取 token；为空 → 401 `missing bearer token`，**不调用 next**。
  2. `signer.Verify(tok)` 验签；失败 → 401 `invalid token`。
  3. 计算 `isSuper`：`claims.IsSuperuser || claims.Role == "admin"`（兼容旧 token）。
  4. 构造 `tenantctx.Tenant{UserID, Email, Role, IsSuperuser}`。
  5. **双重写入**：`tenantctx.SetOnSlot(r.Context(), t)` 把租户写入外层可变 slot（供更外层的 audit middleware 读取，因为它持有的 `r` 没有本中间件加深的 context）；`tenantctx.With(r.Context(), t)` 再写入内层不可变 context。
  6. `next.ServeHTTP(w, r.WithContext(ctx))`。
- **错误处理**：所有失败统一 401，文本简短不泄露细节（`missing bearer token` / `invalid token`）。

### `extractBearer`
- **签名**：`func extractBearer(r *http.Request) string`
- **职责**：从 `Authorization: Bearer <tok>` 提取；缺失时回退到 `?token=<tok>` 查询参数。
- **流程**：先 `strings.HasPrefix(h, "Bearer ")` 检测并 `TrimPrefix`；否则查 `URL.Query().Get("token")`；都无则空串。
- **设计理由**：浏览器原生 WebSocket 构造器无法设置请求头，故保留 query 兜底。

## 5. 依赖关系

- **内部包**：`github.com/ongridio/ongrid/internal/pkg/tenantctx`。
- **外部库**：仅标准库 `net/http`、`strings`。
- **被调用方**：`cmd/ongrid` / `cmd/ongrid-edge` 路由装配，与 `authzmw` 配合形成认证 → 授权链路。

## 6. 并发与资源管理

无并发控制。中间件闭包捕获 `signer` 指针，但 `Signer` 字段不可变，多 goroutine 共享安全。

## 7. 设计模式与亮点

- **双重 context 写入**：`SetOnSlot` + `With` 同时写外层可变 slot 与内层不可变 context——解决 audit middleware 在更外层但需要看到更内层身份信息的问题，是 ongrid 的特殊设计。
- **WebSocket 兜底**：query 参数 token 兼容浏览器原生 WebSocket 无法设头的限制。
- **职责剥离**：中间件只做认证（who are you），不做授权（what can you do），授权下沉到 `authzmw` + casbin。
- **向后兼容**：`Role=="admin"` 旧 token 通过 `isSuper` 升级为超管，升级后新签发的 token 携带显式 `IsSuperuser` 字段。

## 8. 注意事项

- **不查 DB 的代价**：用户被禁用 / 删除后，已签发的 token 在 TTL 内仍可用；需要短期 access TTL（默认 15min）+ refresh 轮换机制兜底。
- **query token 暴露风险**：URL 中的 token 会进 access log、浏览器历史、Referer；仅应保留给 WebSocket upgrade 路径，不应推广到普通 API。
- **`SetOnSlot` 必须配套**：若上游未安装 audit middleware 提供外层 slot，`SetOnSlot` 可能空操作；具体语义需对照 `tenantctx` 实现。
- **401 文本固定**：失败文本简短，便于客户端区分但不含调试信息；如需更细粒度（过期 vs 签名错），需要扩展 Verify 的错误类型。
