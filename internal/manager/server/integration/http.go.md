# `http.go` 技术实现文档

> 源文件：`internal/manager/server/integration/http.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/server/integration`

## 1. 概述

本文件是第三方可观测性栈集成（Grafana / Prom / Loki / Tempo / WebSearch / LLM）的 HTTP 层：暴露 `/v1/integrations/*` 测试 + 同步端点（admin-only）+ `/v1/observability/dashboards/{uid}` 代理（任意已认证用户）。设计要点：所有 probe 接口可选注入，nil 时端点 503 而非崩溃；LLM 配置的 `validate-and-save` 在保存成功后调 `llmRouter.Invalidate()` 即时刷新路由缓存 + 写审计。关键红线：`decodeLLMConfigurationRequest` 用 `DisallowUnknownFields` + 二次 decode 防多对象；`fetchDashboard` 透传 Grafana JSON 不 reshape。

## 2. 包信息

- **包名**：`integration`
- **所属模块**：`internal/manager/server/`（HTTP handler 层）
- **依赖方向**：被上层 router 装配调用 `NewHandler` + `SetLLMRouter/SetLLMProbe` + `Register`；依赖 `biz/grafana`、`biz/setting`、`biz/audit`、`model/audit`、`server/middleware`（audit）、`pkg/grafana`、`pkg/errs`、`pkg/tenantctx`

## 3. 关键类型与接口

```go
type GrafanaService interface {
    Test(ctx) error
    Sync(ctx) (*bizgrafana.SyncResult, error)
    FetchDashboardJSON(ctx, uid string) ([]byte, error)
}

type PromQuerier interface {
    Query(ctx, expr string, ts time.Time) (any, error)
}

type URLProbe interface {
    Probe(ctx) error  // Loki /ready, Tempo /ready 或 /api/echo
}

type WebSearchProbe interface {
    Probe(ctx) (provider, sample string, err error)
}

type LLMRouterInvalidator interface {
    Invalidate()  // *llm.MultiClient 实现，刷 provider catalog 缓存
}

type LLMConfigProbe interface {
    Probe(ctx, in bizsetting.LLMProbeInput) (bizsetting.LLMProbeResult, error)
    Save(ctx, in bizsetting.LLMProbeInput) (bizsetting.LLMProbeResult, error)
}

// 适配器：把 *promquery.Client 包成 PromQuerier，不泄漏具体返回类型
type promQueryAdapter struct { q func(...) error }
func AdaptPromQuerier(q func(...) error) PromQuerier

type Handler struct {
    grafana   GrafanaService
    prom      PromQuerier      // 可空
    loki      URLProbe         // 可空
    tempo     URLProbe         // 可空
    webSearch WebSearchProbe   // 可空
    llmRouter LLMRouterInvalidator  // 可空
    llmProbe  LLMConfigProbe   // 可空
}

type errorBody struct {
    Error string `json:"error"`
    Code  string `json:"code"`
}
```

## 4. 关键函数与流程

### `NewHandler` + `SetLLMRouter` / `SetLLMProbe`
- **职责**：构造 + 可选注入；probe 接口 nil 时端点 503
- **流程**：`SetLLMRouter` / `SetLLMProbe` post-construction 注入，不 widening `NewHandler` 现有签名

### `Register`
- **职责**：挂载路由
- **流程**：
  - admin-only：`POST /v1/integrations/grafana/test|sync`、`/prom/test`、`/loki/test`、`/tempo/test`、`/websearch/test`、`/llm/test`、`/llm/validate-and-save`、`/llm/invalidate`
  - 任意已认证：`GET /v1/observability/dashboards/{uid}`（注释明示该路径是 query 语义不是 admin action）

### `testGrafana` / `syncGrafana`
- **职责**：Grafana 连通性测试 + 推送 folder/datasource/dashboards
- **流程**：requireAdmin → `h.grafana.Test/Sync` → 200

### `testProm` / `testLoki` / `testTempo` / `testWebSearch`
- **职责**：各数据源 probe
- **流程**：requireAdmin → probe nil 时 503 + `{code: "<provider>-disabled"}` → 否则 `Probe(ctx)` → 200
- **关键**：testProm 跑 `up` PromQL；testLoki/testTempo 跑 `/ready`；testWebSearch 返 provider + sample title

### `testLLMConfiguration`
- **职责**：`POST /v1/integrations/llm/test` —— 验证未保存的 provider draft
- **流程**：requireAdmin → `h.llmProbe == nil` → 503 → `decodeLLMConfigurationRequest` → `h.llmProbe.Probe(ctx, in)` → 200
- **Swagger**：有 `@Summary` / `@Router` / `@Success` / `@Failure` 注释

### `validateAndSaveLLMConfiguration`
- **职责**：`POST /v1/integrations/llm/validate-and-save` —— 校验每个 model + 原子保存
- **流程**：requireAdmin → nil probe 503 → decode → `h.llmProbe.Save(ctx, in)` →
  - `result.Saved` 时：`h.llmRouter.Invalidate()`（若已 wire）+ `auditmw.SetAuditEvent`（Action=SettingUpdate, ResourceID=`llm/<provider>`）
  - 200 返 result
- **关键**：保存成功后即时刷 LLM 路由缓存，避免等 60s TTL

### `decodeLLMConfigurationRequest`
- **签名**：`func decodeLLMConfigurationRequest(w, r) (bizsetting.LLMProbeInput, bool)`
- **职责**：严格解码 LLM 配置请求
- **流程**：
  1. `MaxBytesReader(32KB)`
  2. `DisallowUnknownFields()`
  3. decode → `LLMProbeInput`
  4. **二次 decode** 到 `struct{}{}`，必须 `io.EOF`（防多个 JSON 对象）
- **错误处理**：任一步失败 → 400 join ErrInvalid

### `invalidateLLM`
- **职责**：`POST /v1/integrations/llm/invalidate` —— 手动刷 LLM 路由缓存
- **流程**：requireAdmin → `h.llmRouter == nil` → 503 → `h.llmRouter.Invalidate()` → 200
- **关键**：注释明示 setting.Service.Set 已刷自身 per-row cache，此 hook 是 nudge router

### `fetchDashboard`
- **职责**：`GET /v1/observability/dashboards/{uid}` —— 代理 Grafana dashboard JSON
- **流程**：requireUser（任意已认证）→ `chi.URLParam(uid)` 空 → 400 → `h.grafana.FetchDashboardJSON(ctx, uid)` → 透传 verbatim Grafana envelope
- **关键**：注释明示「We don't reshape — Monitor.tsx walks panels[] and Grafana's full schema is richer than we want to model in Go」；browser 永不见 Grafana credential

### `requireAdmin` / `requireUser`
- **签名**：`func (h *Handler) requireAdmin(w, r) bool` + `requireUser`
- **职责**：admin gating / 任意已认证 gating（defence-in-depth，auth middleware 已 reject missing-bearer）

### `writeErr`
- **职责**：sentinel → HTTP code + slug
- **流程**：`ErrUnauthorized`→401、`ErrForbidden`→403、`ErrInvalid`→400、`ErrNotFound`/`pkggrafana.ErrDashboardNotFound`→404、**default→502 upstream**（连接失败、Grafana auth、dashboard parse 错误都 502）

### `AdaptPromQuerier`
- **职责**：把 `func(ctx, expr, ts) error` 包成 `PromQuerier`
- **流程**：返回 `promQueryAdapter{q: q}`，`Query` 方法调 `a.q` 返 `(nil, err)`

## 5. 依赖关系

- **内部包**：
  - `biz/grafana`（`SyncResult`）、`biz/setting`（`LLMProbeInput`/`Result`）、`biz/audit`（`Event`）
  - `model/audit`（Action/Resource/Status 常量）
  - `server/middleware`（`auditmw.SetAuditEvent`）
  - `pkg/grafana`（`ErrDashboardNotFound`）、`pkg/errs`、`pkg/tenantctx`
- **外部库**：`github.com/go-chi/chi/v5`
- **被调用方**：上层 router 装配代码（`cmd/ongrid/main.go`）

## 6. 并发与资源管理

- **无共享状态**：Handler 字段启动期设定后只读
- **ctx 透传**：所有 service 调用透传 `r.Context()`
- **`MaxBytesReader(32KB)` for LLM 配置**：防超大 body
- **无锁**：只读字段

## 7. 设计模式与亮点

- **Probe 接口可选注入**：prom/loki/tempo/webSearch/llmProbe 全可空，nil 时端点 503 + `{code: "<provider>-disabled"}`——部署裁剪友好
- **`AdaptPromQuerier` 函数适配器**：把 `*promquery.Client` 包成 `PromQuerier` 而不泄漏具体返回类型——handler 包不 import promquery
- **`validateAndSave` 即时刷缓存**：保存成功后调 `llmRouter.Invalidate()`，admin 编辑秒级生效，不等 60s TTL
- **`validateAndSave` 写审计**：`Action=SettingUpdate, ResourceID=llm/<provider>, Payload={provider, disabled, model_count}`——LLM 配置变更可追溯
- **`decodeLLMConfigurationRequest` 严格解码**：`DisallowUnknownFields` + 二次 decode 防多对象——配置类输入需严格
- **`fetchDashboard` 不 reshape**：注释明示 Grafana schema 比 Go model 丰富，透传 verbatim JSON 让前端处理
- **`fetchDashboard` 路径语义**：注释明示「lives under /v1/observability rather than /v1/integrations because it's a read path... semantically a query, not an admin action」
- **502 upstream for connection failures**：连接失败、Grafana auth、parse 错误都归 502，body 带 operator-readable reason
- **Swagger 注释**：LLM 端点有完整 `@Summary`/`@Router`/`@Success`/`@Failure`

## 8. 注意事项

- **`requireUser` 是 defence-in-depth**：auth middleware 已 reject missing-bearer，此处仅 tenantctx 存在性检查
- **`writeErr` default 502**：未知错误归 502 upstream（非 500），因为 integration 错误多与上游连接相关
- **`decodeLLMConfigurationRequest` 32KB 上限**：LLM 配置含多 model，32KB 足够；超限 400
- **`fetchDashboard` 透传 verbatim**：不 reshape，Grafana envelope `{dashboard, meta}` 直接传给前端
- **`invalidateLLM` 手动刷**：通常 `validateAndSave` 已自动刷，此端点是手动兜底
- **probe nil 时 slug**：`<provider>-disabled`（如 `prom-disabled`/`loki-disabled`），前端据此显示「未启用」
- **`AdaptPromQuerier` 返 `(nil, err)`**：`Query` 返回 `any`，adapter 永远返 nil data——handler 不关心 PromQL 结果，只关心 err
- **`SetLLMRouter`/`SetLLMProbe` 分离**：不 widening `NewHandler` 签名，保持向后兼容
