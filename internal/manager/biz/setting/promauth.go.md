# promauth.go 技术实现文档

## 1. 概述

`promauth.go` 定义 `PromResolver`，一个针对 `system_settings` 表的多接口 resolver。它同时实现三个下游契约：`promauth.Resolver`（Bearer/Basic）、`promwrite.EndpointResolver`（remote_write 完整 URL + 回退）、`promquery.BaseURLResolver`（PromQL API root + 回退）。所有读取走 `Service.Get`，后者内置 60s 缓存；round-tripper 在其之上再叠加 5s TTL，因此管理员 UI 编辑后约 5 秒内传播，无需重启 manager。

## 2. 包信息

- 包名：`setting`
- 路径：`internal/manager/biz/setting/promauth.go`
- 导入依赖：
  - 标准库：`context` / `strings`
  - 内部包：`github.com/ongridio/ongrid/internal/manager/model/setting`、`github.com/ongridio/ongrid/internal/pkg/promauth`

## 3. 关键类型与接口

### `PromResolver`

```go
type PromResolver struct {
    svc               *Service
    fallbackQueryURL  string
    fallbackWriteURL  string
}
```

- `svc` 是 `system_settings` 服务的引用
- `fallbackQueryURL` / `fallbackWriteURL` 是 env 派生的引导值（来自 `cfg.Prom.URL` / `cfg.Prom.RemoteWriteURL`）。当对应 `system_settings` 行缺失或为空时使用——保证全新安装、DB 全空时仍能与内嵌 Prometheus 通信

## 4. 关键函数与流程

### `NewPromResolver`

```go
func NewPromResolver(svc *Service, fallbackQueryURL, fallbackWriteURL string) *PromResolver
```

构造时对 `fallbackQueryURL` 调用 `strings.TrimRight(..., "/")` 去除尾部斜杠，保证后续拼接路径时不会出现双斜杠。`fallbackWriteURL` 原样保留，因为它可能是完整的 remote_write endpoint。

### `get`（私有 helper）

```go
func (r *PromResolver) get(ctx context.Context, key string) string {
    if r.svc == nil { return "" }
    v, _, err := r.svc.Get(ctx, model.CategoryProm, key)
    if err != nil { return "" }
    return strings.TrimSpace(v)
}
```

统一的容错读取：`svc` 为 nil、读出错、行缺失均返回空串。这种"缺失即无值"的语义符合 Prom auth 的设计——没有配置行就是没有 auth header。

### `Resolve`（实现 `promauth.Resolver`）

```go
func (r *PromResolver) Resolve(ctx context.Context) (promauth.Config, error) {
    return promauth.Config{
        BearerToken:   r.get(ctx, model.KeyPromBearerToken),
        BasicUser:     r.get(ctx, model.KeyPromBasicUser),
        BasicPassword: r.get(ctx, model.KeyPromBasicPassword),
    }, nil
}
```

三个值并行读取，缺失行折叠为空串。`promauth.Config` 的下游消费者将空 Bearer/Basic 视为"无 auth header"。

### `ResolveBaseURL`（实现 `promquery.BaseURLResolver`）

```go
if v := r.get(ctx, model.KeyPromQueryURL); v != "" {
    return strings.TrimRight(v, "/"), nil
}
return r.fallbackQueryURL, nil
```

DB 行优先，回退到 env-seeded URL。同样去除尾部斜杠。

### `ResolveWriteURL`（实现 `promwrite.EndpointResolver`）

三层回退逻辑：

1. 若 `model.KeyPromRemoteWriteURL` 显式配置 → 直接返回
2. 否则若 `fallbackWriteURL` 非空 → 返回（env 引导值）
3. 否则从 `ResolveBaseURL` 派生：`base + "/api/v1/write"`

第三步复刻了原 `New()` 的语义——管理员只配 query URL 时，remote_write 自动落在 `<query_url>/api/v1/write`。

## 5. 依赖关系

- **`*Service`**：唯一的运行时依赖，提供带缓存的 `system_settings` 读取
- **`model`**：常量 `CategoryProm` / `KeyPromBearerToken` / `KeyPromBasicUser` / `KeyPromBasicPassword` / `KeyPromQueryURL` / `KeyPromRemoteWriteURL`
- **`promauth.Config`**：Bearer/Basic 三元组的 wire shape
- **三个下游接口**（`promauth.Resolver` / `promwrite.EndpointResolver` / `promquery.BaseURLResolver`）：本类型实现它们，但本包不导入这些接口——接口在消费方定义，避免循环依赖

## 6. 并发与资源管理

- `PromResolver` 构造后完全只读，无字段变更
- `Service.Get` 内部用 `sync.RWMutex` 保护 cache，并发安全
- 无 goroutine、无 IO 资源持有

## 7. 设计模式与亮点

### 三接口共用一 resolver

`PromResolver` 同时满足三个下游契约。这避免了为 promauth / promwrite / promquery 分别造三个 resolver，因为它们读取的是同一组 `system_settings` 行——合并后管理员编辑只需一次缓存失效即三个 surface 一致生效。

### 缺失即空 vs 缺失即 fallback

`Resolve`（auth）将缺失折叠为空串，`ResolveBaseURL` / `ResolveWriteURL` 将缺失折叠为 env fallback。这种差异是刻意的：

- Auth 缺失 = 无 auth header（合理默认）
- URL 缺失 = 用 env-seeded 值（保证开箱即用）

### 尾部斜杠规范化

所有返回 URL 都通过 `strings.TrimRight(..., "/")`，避免下游拼接 `base + "/api/v1/write"` 时出现 `//api/v1/write`。

### env fallback 双层

`ResolveWriteURL` 的三层回退（DB → env fallback → 派生）保证了：

- 管理员显式配置时尊重其选择
- env-seeded 部署无需 DB 配置即可工作
- 极简部署（只配 query URL）也能自动派生 remote_write

## 8. 注意事项

- **5s 传播延迟**：注释明确指出 round-tripper 在 Service 60s cache 之上叠加 5s TTL，因此 UI 编辑后 ~5s 生效——若管理员反馈"改了没反应"，应先确认是否在 5s 窗口内
- **`get` 静默吞错**：任何错误都返回空串。这意味着 DB 短暂故障期间 auth/URL 会"消失"，下游可能因 401 失败——但这是比"DB 故障导致整个 Prom 查询失败"更温和的降级
- **fallbackWriteURL 不去尾斜杠**：与 `fallbackQueryURL` 不一致；如果 env 配置带尾斜杠，可能产生 `//api/v1/write`。当前实现假设 env 值已规范化
- **多租户**：注释未提及，但 `Service.Get` 当前 cache key 是 `(category, key)`，未来多租户需加 `org_id` 前缀
