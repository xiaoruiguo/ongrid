# telemetry.go 技术实现文档

## 1. 概述

`telemetry.go` 定义 `LokiResolver` 与 `TempoResolver`，分别从 `system_settings` 读取 Loki（日志）与 Tempo（追踪）的连接配置：URL、可选 Basic Auth、TLS insecure 开关。两者镜像相同的结构——单 `Service` 依赖、nil-safe 的 Get 路径、env-seeded fallback URL。它们被 `PluginConfigUC.FetchForEdge` 用于决定 edge agent 的 logs/traces 插件推送目标，也被 HTTP handler 的"测试连接"探针调用。

## 2. 包信息

- 包名：`setting`
- 路径：`internal/manager/biz/setting/telemetry.go`
- 导入依赖：
  - 标准库：`context` / `strings`
  - 内部包：`github.com/ongridio/ongrid/internal/manager/model/setting`

## 3. 关键类型与接口

### `LokiResolver`

```go
type LokiResolver struct {
    svc         *Service
    fallbackURL string
}
```

读取 `loki.url` + 可选 basic auth。`fallbackURL` 是 env-seeded 默认值（`cfg.Logs.URL`）。当 DB 行缺失或为空时回落，保证全新安装、无管理员编辑时仍能解析到内嵌 Loki。

### `TempoResolver`

```go
type TempoResolver struct {
    svc         *Service
    fallbackURL string
}
```

Tempo 侧的对应实现。解析的 URL 是 edge agent 的 traces 插件（otelcol `exporters.otlphttp.endpoint`）所推送的 OTLP HTTP endpoint。

## 4. 关键函数与流程

### `NewLokiResolver` / `NewTempoResolver`

构造时对 `fallbackURL` 调用 `strings.TrimRight(..., "/")` 去除尾部斜杠，规范化 env 输入。

### `get`（私有 helper，两者结构一致）

```go
func (r *LokiResolver) get(ctx context.Context, key string) string {
    if r == nil || r.svc == nil { return "" }
    v, _, err := r.svc.Get(ctx, model.CategoryLoki, key)
    if err != nil { return "" }
    return strings.TrimSpace(v)
}
```

统一容错：receiver nil、svc nil、出错均返回空串。`found` 标志被丢弃——缺失与出错对调用方等价（都是"无值"）。

### `URL(ctx)` —— Loki

```go
if v := r.get(ctx, model.KeyLokiURL); v != "" {
    return strings.TrimRight(v, "/")
}
return r.fallbackURL
```

DB 行优先，再次 `TrimRight` 防止管理员填入带尾斜杠的值。回落到 env-seeded URL。

### `Auth(ctx)` —— Loki

```go
return r.get(ctx, model.KeyLokiBasicUser), r.get(ctx, model.KeyLokiBasicPassword)
```

返回 `(basicUser, basicPassword)`。空 user 即无 auth。

### `TLSInsecure(ctx)` —— Loki

```go
return strings.EqualFold(r.get(ctx, model.KeyLokiTLSInsecure), "true")
```

存储值是 `"true"` 或 `"false"`（HTTP handler 默认标记 non-sensitive）。任何非 `"true"` 值返回 `false`——fail-safe，避免误启用 insecure TLS。

### `TempoResolver` 方法

`URL` / `Auth` / `TLSInsecure` 与 Loki 镜像，仅 category 与 key 不同（`CategoryTempo` / `KeyTempoURL` / `KeyTempoBasicUser` / `KeyTempoBasicPassword` / `KeyTempoTLSInsecure`）。

## 5. 依赖关系

- **`*Service`**：唯一运行时依赖，提供 `system_settings` 带缓存读取
- **`model`**：常量 `CategoryLoki` / `CategoryTempo` 及对应 key
- **被依赖方**：
  - `PluginConfigUC.FetchForEdge`（决定 edge 插件推送目标）
  - HTTP handler 的"测试连接"探针
  - `probe.go` 的 `LokiURLProbe` / `TempoURLProbe` 直接持有 resolver

## 6. 并发与资源管理

- 两个 resolver 构造后完全只读，无字段变更
- `Service.Get` 内部 `sync.RWMutex` 保护 cache，并发安全
- 无 goroutine、无 IO 资源持有
- 所有方法均 nil-safe：`if r == nil { return "" }` / `return "", ""` / `return false`

## 7. 设计模式与亮点

### 镜像结构

`LokiResolver` 与 `TempoResolver` 在结构、方法签名、容错策略上完全对称。这种"复制而非抽象"是刻意的——两者读取的 category 不同，强行抽象成 `genericTelemetryResolver` 会牺牲类型清晰度，且 telemetry 信号类型有限（logs/traces/metrics），无未来扩张压力。

### env-seeded fallback

`fallbackURL` 让全新部署在 DB 全空时仍能解析到内嵌服务。这是 env-seeded defaults 模式的一贯体现——与 `LLMSettingsResolver` / `PromResolver` 一致。

### 尾部斜杠双重规范化

构造时 `TrimRight(fallbackURL, "/")`，`URL()` 方法再次 `TrimRight(v, "/")`。前者规范化 env 输入，后者防御管理员在 UI 填入带斜杠的值。这种"防御性双重规范化"在拼接 `/ready` 等路径时避免双斜杠。

### TLSInsecure 的 fail-safe

`strings.EqualFold(..., "true")` 而非 `strconv.ParseBool`——任何意外值（空串、`"yes"`、`"1"`）均回落为 `false`。这是安全敏感开关的正确默认。

### nil-safe 的全覆盖

每个方法首行检查 `r == nil`。这让上层 wiring 在部分装配阶段（如测试）能安全调用而不会 panic。

## 8. 注意事项

- **`get` 丢弃 `found`**：与 `Service.Get` 的 `(value, found, error)` 语义不同，resolver 无法区分"行存在但值为空"与"行不存在"。对 URL/Auth 来说这无差异（都是"无配置"），但若有调用方依赖"显式空值覆盖 fallback"的语义，此处会失效
- **fallback URL 假设 env 已规范化**：构造时 `TrimRight` 处理尾斜杠，但不处理其他畸形（如缺 schema）。env 配置错误会导致 edge 推送失败，需在部署期保证
- **`TempoResolver.URL` 不剥离 `/v1/traces`**：与 `TempoURLProbe.Probe` 不同——resolver 返回管理员填入的原始值，由消费方（如 probe）自行适配。这避免 resolver 越权改写输入
- **多租户**：cache key 当前是 `(category, key)`，多租户后需加 `org_id` 前缀
- **TLS insecure 的 `//nolint`**：本文件未直接出现，但 `probeReadyEndpoint` 用 `InsecureSkipVerify: insecure` 时标注了 `//nolint:gosec // operator opt-in`——resolver 只提供开关值，安全责任在调用方
