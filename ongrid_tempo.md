# OnGrid × Tempo 集成技术实现文档

> 本文档深入分析 OnGrid 系统与 Grafana Tempo 分布式链路追踪后端的完整集成实现，覆盖数据平面（OTLP 推送）、控制平面（查询代理）、AI 工具、告警评估、健康探测、边缘 agent 转发、部署配置等全部代码路径。所有引用均锚定到具体文件路径与行号。

---

## 目录

1. [集成总览](#1-集成总览)
2. [分层文件索引](#2-分层文件索引)
3. [数据平面：OTel SDK 导出](#3-数据平面otel-sdk-导出)
4. [数据平面：边缘 Agent 转发](#4-数据平面边缘-agent-转发)
5. [控制平面：查询代理 HTTP Handler](#5-控制平面查询代理-http-handler)
6. [查询客户端 tracequery.Client](#6-查询客户端-tracequeryclient)
7. [配置层：TempoResolver 与 system_settings](#7-配置层temporesolver-与-system_settings)
8. [first-boot seed 与 env 配置](#8-first-boot-seed-与-env-配置)
9. [URL 探测 TempoURLProbe](#9-url-探测-tempourlprobe)
10. [系统健康检查 checkTempo](#10-系统健康检查-checktempo)
11. [AI 工具 query_traceql](#11-ai-工具-query_traceql)
12. [AI 工具 correlate_incident 的 trace panel](#12-ai-工具-correlate_incident-的-trace-panel)
13. [NL→TraceQL 翻译](#13-nltraceql-翻译)
14. [告警评估 trace_latency / trace_error_rate](#14-告警评估-trace_latency--trace_error_rate)
15. [插件端点解析 pluginEndpointResolver](#15-插件端点解析-pluginendpointresolver)
16. [Tempo 后端配置 tempo-config.yaml](#16-tempo-后端配置-tempo-configyaml)
17. [docker-compose 部署](#17-docker-compose-部署)
18. [内置知识库 tempo.md](#18-内置知识库-tempomd)
19. [并发与资源管理](#19-并发与资源管理)
20. [架构红线与设计要点](#20-架构红线与设计要点)
21. [附录：完整调用链](#21-附录完整调用链)

---

## 1. 集成总览

### 1.1 Tempo 在 OnGrid 中的角色

OnGrid 与 Grafana Tempo 的集成是**双向**的：

```
┌─────────────────────────────────────────────────────────────────┐
│                      OnGrid × Tempo 集成全景                      │
├─────────────────────────────────────────────────────────────────┤
│                                                                  │
│  数据平面（OTLP push）                                            │
│  ┌────────────┐   OTLP/HTTP    ┌─────────┐   OTLP/gRPC+HTTP      │
│  │ Manager    │ ─────────────► │         │ ◄──────────────────┐  │
│  │ tracing.Init│                │  Tempo  │                    │  │
│  └────────────┘                │ :4317   │                    │  │
│                                │ :4318   │                    │  │
│  ┌────────────┐  OTLP/HTTP     │         │                    │  │
│  │ Edge Agent │ ─────────────► │         │ ◄──────────────────┐  │
│  │ otelcol    │  via manager   │         │                    │  │
│  │  -contrib  │   nginx        └─────────┘                    │  │
│  └────────────┘                  ▲   ▲                         │  │
│                                  │   │                         │  │
│  控制平面（query proxy）          │   │                         │  │
│  ┌────────────┐  /api/search     │   │  /api/traces/<id>       │  │
│  │ SPA Traces │ ──────────────┐  │   │                         │  │
│  │   page     │               │  │   │                         │  │
│  └────────────┘               ▼  ▼   ▼                         │  │
│  ┌──────────────────────────────────────────┐                  │  │
│  │ Manager traces.Handler                   │                  │  │
│  │ /v1/traces/search                        │                  │  │
│  │ /v1/traces/{trace_id}                    │                  │  │
│  │ /v1/traces/tags/{tag}/values             │                  │  │
│  └──────────────────────────────────────────┘                  │  │
│                                                                  │
│  AI 工具                                                          │
│  ┌──────────────────────────────────────────┐                  │
│  │ query_traceql tool                       │                  │
│  │ correlate_incident tool (trace panel)    │                  │
│  └──────────────────────────────────────────┘                  │  │
│                                                                  │
│  告警评估（spanmetrics）                                          │
│  ┌──────────────────────────────────────────┐                  │
│  │ Tempo metrics-generator                  │                  │
│  │   → Prom remote_write                    │                  │
│  │   → traces_spanmetrics_latency_*         │                  │
│  │   → alert evaluators trace_latency /     │                  │
│  │     trace_error_rate                     │                  │
│  └──────────────────────────────────────────┘                  │  │
└─────────────────────────────────────────────────────────────────┘
```

### 1.2 三个关键端口

| 端口 | 协议 | 用途 | 暴露方式 |
|---|---|---|---|
| `4317` | OTLP gRPC | 应用推送 traces | 仅容器内 |
| `4318` | OTLP HTTP | 应用推送 traces（含 `/v1/traces`） | nginx 反代 + auth_request |
| `3200` | HTTP | Tempo API（`/api/search` / `/api/traces/<id>` / `/ready`） | 仅容器内（manager 代理） |

源码：[docker-compose.yml#L273-L289](file:///d:/claude/ongrid/deploy/docker-compose.yml#L273-L289) 注释明确"OTLP gRPC :4317 和 HTTP :4318 故意不发布——公共访问通过 nginx /v1/traces + auth_request → manager edgeauth"。

### 1.3 数据流三层

1. **推送层**：Manager 通过 OTel SDK 导出 + Edge Agent 通过 otelcol-contrib 转发
2. **查询层**：SPA → Manager `traces.Handler` → `tracequery.Client` → Tempo `/api/*`
3. **派生层**：Tempo `metrics-generator` 派生 `traces_spanmetrics_*` → Prom → alert evaluators

---

## 2. 分层文件索引

### 2.1 数据平面（推送）

| 文件 | 行号 | 作用 |
|---|---|---|
| [internal/pkg/tracing/tracing.go](file:///d:/claude/ongrid/internal/pkg/tracing/tracing.go) | L1-L106 | OTel SDK 初始化 + OTLP HTTP exporter |
| [cmd/ongrid/main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) | L217-L235 | tracing.Init 装配 |
| [cmd/ongrid/main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) | L2188-L2201 | otelhttp 中间件 |
| [internal/edgeagent/plugins/traces/plugin.go](file:///d:/claude/ongrid/internal/edgeagent/plugins/traces/plugin.go) | L1-L47 | Edge traces plugin（otelcol-contrib 子进程） |
| [internal/edgeagent/plugins/traces/render.go](file:///d:/claude/ongrid/internal/edgeagent/plugins/traces/render.go) | L1-L478 | otelcol.yaml 模板渲染 |

### 2.2 控制平面（查询）

| 文件 | 行号 | 作用 |
|---|---|---|
| [internal/pkg/tracequery/client.go](file:///d:/claude/ongrid/internal/pkg/tracequery/client.go) | L1-L252 | Tempo HTTP 查询客户端 |
| [internal/pkg/tracequery/doc.go](file:///d:/claude/ongrid/internal/pkg/tracequery/doc.go) | L1-L12 | 包注释（backend-decoupled 命名） |
| [internal/manager/server/traces/http.go](file:///d:/claude/ongrid/internal/manager/server/traces/http.go) | L1-L228 | 查询代理 HTTP handler |
| [cmd/ongrid/main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) | L1020-L1033 | tracesHandler 装配 |

### 2.3 配置与探测

| 文件 | 行号 | 作用 |
|---|---|---|
| [internal/pkg/config/config.go](file:///d:/claude/ongrid/internal/pkg/config/config.go) | L96-L102 | TracesConfig（URL 字段） |
| [internal/manager/biz/setting/telemetry.go](file:///d:/claude/ongrid/internal/manager/biz/setting/telemetry.go) | L72-L122 | TempoResolver（URL/Auth/TLS） |
| [internal/manager/biz/setting/probe.go](file:///d:/claude/ongrid/internal/manager/biz/setting/probe.go) | L51-L80 | TempoURLProbe（/ready 探测） |
| [internal/manager/model/setting/model.go](file:///d:/claude/ongrid/internal/manager/model/setting/model.go) | L179-L186 | CategoryTempo + KeyTempo* 常量 |
| [cmd/ongrid/main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) | L455-L479 | first-boot seed + Resolver 装配 |

### 2.4 AI 工具

| 文件 | 行号 | 作用 |
|---|---|---|
| [internal/manager/biz/aiops/tools/query_traceql.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/query_traceql.go) | L1-L189 | query_traceql schema + executor |
| [internal/manager/biz/aiops/tools/query_traceql_basetool.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/query_traceql_basetool.go) | L1-L138 | BaseTool 形式 |
| [internal/manager/biz/aiops/tools/correlate_incident.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/correlate_incident.go) | L263-L281 | trace panel 拉取 |
| [internal/manager/server/aiops/query_translate.go](file:///d:/claude/ongrid/internal/manager/server/aiops/query_translate.go) | L77-L85 | NL→TraceQL 翻译 |

### 2.5 告警与健康

| 文件 | 行号 | 作用 |
|---|---|---|
| [internal/manager/biz/alert/evaluators_phaseB.go](file:///d:/claude/ongrid/internal/manager/biz/alert/evaluators_phaseB.go) | L165-L278 | trace_latency / trace_error_rate evaluators |
| [internal/manager/service/systemhealth/service.go](file:///d:/claude/ongrid/internal/manager/service/systemhealth/service.go) | L226-L236 | checkTempo |
| [cmd/ongrid/main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) | L1725-L1737 | systemhealth 装配 |

### 2.6 边缘端点解析

| 文件 | 行号 | 作用 |
|---|---|---|
| [cmd/ongrid/main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) | L2610-L2713 | pluginEndpointResolver（traces 分支） |

### 2.7 部署与文档

| 文件 | 行号 | 作用 |
|---|---|---|
| [deploy/docker-compose.yml](file:///d:/claude/ongrid/deploy/docker-compose.yml) | L273-L289 | tempo service |
| [deploy/install/tempo-config.yaml](file:///d:/claude/ongrid/deploy/install/tempo-config.yaml) | L1-L76 | Tempo 单二进制配置 |
| [internal/manager/biz/knowledge/builtin_vault/systems/observability-stack/tempo.md](file:///d:/claude/ongrid/internal/manager/biz/knowledge/builtin_vault/systems/observability-stack/tempo.md) | L1-L103 | 内置知识库（RAG） |

---

## 3. 数据平面：OTel SDK 导出

### 3.1 tracing.Init 入口

源码：[internal/pkg/tracing/tracing.go#L60-L106](file:///d:/claude/ongrid/internal/pkg/tracing/tracing.go#L60-L106)

```go
func Init(ctx context.Context, cfg Config) (Shutdown, error) {
    if strings.TrimSpace(cfg.Endpoint) == "" {
        return func(context.Context) error { return nil }, nil
    }
    if cfg.SamplingRatio <= 0 || cfg.SamplingRatio > 1 {
        cfg.SamplingRatio = 1.0
    }

    opts := []otlptracehttp.Option{
        otlptracehttp.WithEndpoint(cfg.Endpoint),
    }
    if cfg.Insecure {
        opts = append(opts, otlptracehttp.WithInsecure())
    }

    exporter, err := otlptracehttp.New(ctx, opts...)
    if err != nil {
        return nil, fmt.Errorf("tracing: build exporter: %w", err)
    }

    res := resource.NewWithAttributes(
        semconv.SchemaURL,
        semconv.ServiceName(cfg.ServiceName),
    )

    tp := sdktrace.NewTracerProvider(
        sdktrace.WithBatcher(exporter,
            sdktrace.WithBatchTimeout(2*time.Second),
        ),
        sdktrace.WithResource(res),
        sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SamplingRatio))),
    )
    otel.SetTracerProvider(tp)
    otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
        propagation.TraceContext{},
        propagation.Baggage{},
    ))
    return tp.Shutdown, nil
}
```

### 3.2 Config 字段

源码：[tracing.go#L36-L52](file:///d:/claude/ongrid/internal/pkg/tracing/tracing.go#L36-L52)

```go
type Config struct {
    ServiceName   string  // resource.service.name，spanmetrics 按此切分
    Endpoint      string  // OTLP HTTP collector，host:port
    Insecure      bool    // http vs https
    SamplingRatio float64 // 0..1，根 span 采样率
}
```

### 3.3 关键设计

1. **空 Endpoint 返回 no-op shutdown**（[L61-L63](file:///d:/claude/ongrid/internal/pkg/tracing/tracing.go#L61-L63)）：调用方可以无条件 `defer shutdown()`，禁用 tracing 时零成本
2. **2s batch timeout**（[L95](file:///d:/claude/ongrid/internal/pkg/tracing/tracing.go#L95)）：事故 spans 不会被困在 batch 里一分钟才出现在 Tempo
3. **ParentBased(TraceIDRatioBased)**（[L98](file:///d:/claude/ongrid/internal/pkg/tracing/tracing.go#L98)）：根 span 采样，子 span 跟随根决策，保证 trace 完整性
4. **不调用 resource.Default()**（[L80-L84](file:///d:/claude/ongrid/internal/pkg/tracing/tracing.go#L80-L84)）：注释明确"skip resource.Default() merge to avoid schema-URL version conflicts between different otel module versions in the dep tree"

### 3.4 main.go 装配

源码：[cmd/ongrid/main.go#L217-L239](file:///d:/claude/ongrid/cmd/ongrid/main.go#L217-L239)

```go
// Initialise OpenTelemetry tracing. The Tempo OTLP HTTP receiver
// lives at tempo:4318 inside the docker network; spanmetrics
// generator on Tempo derives traces_spanmetrics_*_total which the
// trace_latency / trace_error_rate evaluators query.
otelEndpoint := os.Getenv("ONGRID_OTEL_ENDPOINT")
if otelEndpoint == "" {
    otelEndpoint = "tempo:4318"
}
otelShutdown, err := tracing.Init(rootCtx, tracing.Config{
    ServiceName:   "ongrid-manager",
    Endpoint:      otelEndpoint,
    Insecure:      true,
    SamplingRatio: 1.0,
})
if err != nil {
    log.Warn("tracing: init failed (continuing without OTel)", slog.Any("err", err))
}
defer func() {
    shutCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
    defer c()
    _ = otelShutdown(shutCtx)
}()
```

**关键点**：
- 默认 endpoint `tempo:4318`（容器内 DNS）
- `ONGRID_OTEL_ENDPOINT` 空字符串可禁用 tracing
- 初始化失败只 warn 不 fatal（manager 可继续运行，spanmetrics evaluators 会读到空矩阵）

### 3.5 otelhttp 中间件

源码：[cmd/ongrid/main.go#L2188-L2201](file:///d:/claude/ongrid/cmd/ongrid/main.go#L2188-L2201)

```go
otelhttpmw := func(next http.Handler) http.Handler {
    return otelhttp.NewHandler(next, "ongrid-manager",
        otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
            if route := chi.RouteContext(r.Context()).RoutePattern(); route != "" {
                return r.Method + " " + route
            }
            return r.Method + " " + r.URL.Path
        }),
    )
}
```

**Span 命名格式**：`{METHOD} {ROUTE_PATTERN}`（如 `GET /v1/traces/search`），Tempo spanmetrics generator 按此派生 `traces_spanmetrics_latency_bucket` per route。注释明确这一意图（[L2206-L2208](file:///d:/claude/ongrid/cmd/ongrid/main.go#L2206-L2208)）。

---

## 4. 数据平面：边缘 Agent 转发

### 4.1 traces plugin 定位

源码：[internal/edgeagent/plugins/traces/plugin.go#L1-L47](file:///d:/claude/ongrid/internal/edgeagent/plugins/traces/plugin.go#L1-L47)

```go
// Package traces is the edge-side `traces` plugin.
// It wraps an OpenTelemetry Collector (otelcol-contrib) subprocess:
// ongrid-edge writes an otelcol config derived from the manager-pushed
// PluginConfig, spawns otelcol-contrib, and lets it accept OTLP gRPC/HTTP
// from local applications and push directly to manager nginx /v1/traces.
const Name = "traces"

func New(binDir, workDir string, log *slog.Logger) plugins.Plugin {
    return plugins.NewSubprocess(plugins.SubprocessOpts{
        Name:         Name,
        Binary:       filepath.Join(binDir, "otelcol-contrib"),
        WorkDir:      filepath.Join(workDir, Name),
        ConfigFile:   filepath.Join(workDir, Name, "otelcol.yaml"),
        ConfigRender: render,
        Args: func(_ plugins.PluginConfig, configFile string) []string {
            return []string{"--config=" + configFile}
        },
        Log: log,
    })
}
```

**设计**：
- 边缘节点运行独立的 `otelcol-contrib` 子进程
- ongrid-edge 不触碰 trace 字节流，只渲染配置 + 管理子进程
- Plugin name `traces`（复数）匹配 OTLP endpoint path `/v1/traces`

### 4.2 otelcol.yaml 模板

源码：[internal/edgeagent/plugins/traces/render.go#L31-L255](file:///d:/claude/ongrid/internal/edgeagent/plugins/traces/render.go#L31-L255)

模板核心结构：

```yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: {{ .GRPCEndpoint }}    # 默认 127.0.0.1:4317
      http:
        endpoint: {{ .HTTPEndpoint }}    # 默认 127.0.0.1:4318

processors:
  memory_limiter:                         # 可选，bounded_pipelines=true 时启用
  k8sattributes:                          # 可选，K8s gateway 模式
  resource/device:                        # 注入 device_id + ongrid_source
    attributes:
      - key: device_id
        value: "{{ .EdgeID }}"
        action: upsert
      - key: ongrid_source
        value: "otlp"
        action: upsert
  batch:                                  # 8192 / 5s / 16384

exporters:
  otlphttp/manager:
    traces_endpoint: {{ .Endpoint }}      # manager /v1/traces
    headers:
      Authorization: "{{ .AuthHeader }}"  # Basic 或 Bearer
    tls:
      insecure_skip_verify: true          # 默认跳过自签证书
    compression: gzip
    timeout: 30s
    sending_queue:
      enabled: true
      num_consumers: 4
      queue_size: {{ .QueueSize }}
    retry_on_failure:
      enabled: true
      initial_interval: 1s
      max_interval: 30s
      max_elapsed_time: 5m

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [memory_limiter?, k8sattributes?, resource/device, batch]
      exporters: [otlphttp/manager]
```

### 4.3 render 函数关键逻辑

源码：[render.go#L279-L403](file:///d:/claude/ongrid/internal/edgeagent/plugins/traces/render.go#L279-L403)

1. **Endpoint 必填**（[L280-L282](file:///d:/claude/ongrid/internal/edgeagent/plugins/traces/render.go#L280-L282)）：`cfg.Endpoint == ""` 返回错误
2. **device_id 必填**（[L284-L286](file:///d:/claude/ongrid/internal/edgeagent/plugins/traces/render.go#L284-L286)）：`EdgeID == 0 && !omitDeviceID` 返回错误
3. **traces_endpoint 而非 endpoint**（[L136](file:///d:/claude/ongrid/internal/edgeagent/plugins/traces/render.go#L136)）：otelcol `otlphttp.endpoint` 是 base URL，collector 会自动追加 `/v1/traces`
4. **TLS 默认 skip-verify**（[L327-L332](file:///d:/claude/ongrid/internal/edgeagent/plugins/traces/render.go#L327-L332)）：注释明确"standard install ships a self-signed manager cert"
5. **Auth 双模式**（[L334-L341](file:///d:/claude/ongrid/internal/edgeagent/plugins/traces/render.go#L334-L341)）：AuthUser+AuthPass=Basic，仅 AuthPass=Bearer
6. **bounded_pipelines 校验**（[L313-L321](file:///d:/claude/ongrid/internal/edgeagent/plugins/traces/render.go#L313-L321)）：memory limiter 需要 `0 < spike < limit`，batch 需要 `0 < send <= max <= 4096`

### 4.4 不在边缘做 tail_sampling

源码注释（[render.go#L27-L30](file:///d:/claude/ongrid/internal/edgeagent/plugins/traces/render.go#L27-L30)）：

```go
// Pipeline: receivers -> resourcedetection (light) -> resource (inject
// device_id) -> batch -> exporter. We deliberately keep tail_sampling out of
// the edge — it stays a manager-side concern so
// edges remain stateless about cross-span decisions.
```

**设计意图**：tail_sampling 需要看完整 trace 树决策，边缘节点无状态、不持有跨 span 决策。

---

## 5. 控制平面：查询代理 HTTP Handler

### 5.1 路由注册

源码：[internal/manager/server/traces/http.go#L52-L57](file:///d:/claude/ongrid/internal/manager/server/traces/http.go#L52-L57)

```go
func (h *Handler) Register(r chi.Router) {
    r.Get("/v1/traces/search", h.search)
    r.Get("/v1/traces/tags/{tag}/values", h.tagValues)
    // The trace_id route comes last so it doesn't shadow /tags/...
    r.Get("/v1/traces/{trace_id}", h.getTrace)
}
```

**三条路由**：
- `GET /v1/traces/search` — TraceQL / facet 搜索
- `GET /v1/traces/tags/{tag}/values` — tag 值列表（下拉填充）
- `GET /v1/traces/{trace_id}` — 单 trace 详情

**注意**：`{trace_id}` 路由必须最后注册，否则会 shadow `/tags/...` 路由。

### 5.2 Querier 接口

源码：[http.go#L29-L35](file:///d:/claude/ongrid/internal/manager/server/traces/http.go#L29-L35)

```go
type Querier interface {
    SearchTraces(ctx context.Context, opts tracequery.SearchOptions) (*tracequery.SearchResult, error)
    GetTrace(ctx context.Context, traceID string) (*tracequery.TraceResult, error)
    TagValues(ctx context.Context, tag string) ([]string, error)
}
```

`*tracequery.Client` 满足此接口；测试可注入 fake。

### 5.3 search handler

源码：[http.go#L66-L152](file:///d:/claude/ongrid/internal/manager/server/traces/http.go#L66-L152)

```go
func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
    if h.q == nil {
        writeErr(w, http.StatusServiceUnavailable, "traces backend disabled")
        return
    }
    // 解析 start/end（RFC3339 或 unix 秒/毫秒/纳秒）
    // 解析 limit（1..1000，默认 100）
    // 解析 minDuration / maxDuration
    // 构建 SearchOptions
    // 当 q 为空时，从 service+operation 构建 Tags map（legacy tag 模式）
    ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
    defer cancel()
    out, err := h.q.SearchTraces(ctx, opts)
    // ...
}
```

**关键点**：
- `h.q == nil` 时返回 503（[L67-L70](file:///d:/claude/ongrid/internal/manager/server/traces/http.go#L67-L70)）：Tempo 禁用时 SPA 显示"traces disabled"而非静默失败
- 30s 超时（[L139](file:///d:/claude/ongrid/internal/manager/server/traces/http.go#L139)）：与 tracequery.Client 的 defaultTimeout 对称
- **facet 模式 fallback**（[L126-L137](file:///d:/claude/ongrid/internal/manager/server/traces/http.go#L126-L137)）：`q` 为空时用 `service.name` / `name` tags 走 legacy 搜索

### 5.4 getTrace handler

源码：[http.go#L154-L176](file:///d:/claude/ongrid/internal/manager/server/traces/http.go#L154-L176)

```go
func (h *Handler) getTrace(w http.ResponseWriter, r *http.Request) {
    // ...
    out, err := h.q.GetTrace(ctx, id)
    // Tempo returns OTLP-shaped JSON; pass it through verbatim so the SPA
    // can walk resourceSpans / scopeSpans / spans without a re-encode.
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusOK)
    _, _ = w.Write(out.Body)
}
```

**关键**：直接透传 Tempo 的 OTLP-shaped JSON，不重新编码。SPA 直接遍历 `resourceSpans` / `scopeSpans` / `spans`。

### 5.5 parseTime 灵活解析

源码：[http.go#L198-L218](file:///d:/claude/ongrid/internal/manager/server/traces/http.go#L198-L218)

```go
func parseTime(s string) (time.Time, error) {
    // RFC3339 优先
    // 否则按数值大小判断：>1e15 纳秒、>1e12 毫秒、否则秒
    switch {
    case n > 1e15:
        return time.Unix(0, n), nil
    case n > 1e12:
        return time.UnixMilli(n), nil
    default:
        return time.Unix(n, 0), nil
    }
}
```

**设计**：便于 curl 测试（`start=1700000000` 比 RFC3339 简洁）。

### 5.6 main.go 装配

源码：[cmd/ongrid/main.go#L1020-L1033](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1020-L1033)

```go
var tracesHandler *managerservertraces.Handler
if cfg.Traces.URL != "" {
    tracesHandler = managerservertraces.NewHandler(
        pkgtracequery.New(cfg.Traces.URL, log.With(slog.String("comp", "tracequery"))),
    )
} else {
    // Tempo disabled — handler installs but every route returns 503.
    tracesHandler = managerservertraces.NewHandler(nil)
}
```

**关键**：`cfg.Traces.URL == ""` 时传入 `nil` Querier，handler 仍注册但所有路由返回 503。

---

## 6. 查询客户端 tracequery.Client

### 6.1 包设计哲学

源码：[internal/pkg/tracequery/doc.go#L1-L12](file:///d:/claude/ongrid/internal/pkg/tracequery/doc.go#L1-L12)

```go
// Package tracequery is a tiny client for Tempo's HTTP query API
// (/api/search and /api/traces/<id>). It is consumed by the manager AI
// tool registry; the response is passed back to the LLM verbatim, so we
// preserve the raw JSON shape.
//
// Backend-decoupled name: the package is `tracequery`, not `tempoquery`,
// so that swapping the backend (e.g. to VictoriaTraces) is a single
// import-site change rather than a rename ripple.
```

**关键**：包名 `tracequery` 而非 `tempoquery`，为未来换 backend（如 VictoriaTraces）保留单点替换空间。

### 6.2 Client 结构

源码：[client.go#L41-L49](file:///d:/claude/ongrid/internal/pkg/tracequery/client.go#L41-L49)

```go
type Client struct {
    base       BaseURLResolver
    httpClient *http.Client
    log        *slog.Logger
}

const defaultTimeout = 30 * time.Second
```

### 6.3 BaseURLResolver 动态解析

源码：[client.go#L32-L37](file:///d:/claude/ongrid/internal/pkg/tracequery/client.go#L32-L37)

```go
// BaseURLResolver returns the current Tempo API root. It's invoked once
// per call so admin UI edits propagate without restart.
type BaseURLResolver interface {
    ResolveBaseURL(ctx context.Context) (string, error)
}
```

**关键**：每次调用都解析，admin UI 修改无需重启 manager。镜像 `promquery.BaseURLResolver`。

### 6.4 三个构造函数

源码：[client.go#L64-L87](file:///d:/claude/ongrid/internal/pkg/tracequery/client.go#L64-L87)

| 构造函数 | 用途 |
|---|---|
| `New(baseURL, log)` | 静态 baseURL，用默认 http.Client |
| `NewWithHTTPClient(baseURL, hc, log)` | 静态 baseURL + 自定义 http.Client（测试 seam） |
| `NewWithResolverAndHTTPClient(r, hc, log)` | 动态 resolver（生产用） |

### 6.5 SearchTraces

源码：[client.go#L112-L157](file:///d:/claude/ongrid/internal/pkg/tracequery/client.go#L112-L157)

```go
func (c *Client) SearchTraces(ctx context.Context, opts SearchOptions) (*SearchResult, error) {
    q := url.Values{}
    if opts.Query != "" {
        q.Set("q", opts.Query)              // TraceQL 优先
    } else if len(opts.Tags) > 0 {
        // legacy tags=key1=v1 key2=v2 form
        q.Set("tags", strings.Join(parts, " "))
    }
    limit := opts.Limit
    if limit <= 0 {
        limit = 100                          // 默认 100（Tempo 默认 20）
    }
    // start / end / minDuration / maxDuration
    body, err := c.do(ctx, "/api/search", q)
    // ...
}
```

**SearchOptions 字段**（[L93-L109](file:///d:/claude/ongrid/internal/pkg/tracequery/client.go#L93-L109)）：

| 字段 | 用途 |
|---|---|
| `Query` | TraceQL 表达式（优先） |
| `Tags` | legacy key/value 匹配（Query 为空时用） |
| `Limit` | 结果数上限（默认 100） |
| `Start` / `End` | 时间窗口 |
| `MinDuration` / `MaxDuration` | span duration 过滤 |

### 6.6 TagValues

源码：[client.go#L169-L185](file:///d:/claude/ongrid/internal/pkg/tracequery/client.go#L169-L185)

```go
func (c *Client) TagValues(ctx context.Context, tag string) ([]string, error) {
    path := "/api/search/tag/" + url.PathEscape(tag) + "/values"
    body, err := c.do(ctx, path, nil)
    // ...
    var env struct {
        TagValues []string `json:"tagValues"`
    }
    // ...
}
```

**用途**：SPA Traces 页面填充 service / operation 下拉。注释明确"v1 endpoint because that's what Tempo 2.x stable serves"。

### 6.7 GetTrace

源码：[client.go#L188-L201](file:///d:/claude/ongrid/internal/pkg/tracequery/client.go#L188-L201)

```go
func (c *Client) GetTrace(ctx context.Context, traceID string) (*TraceResult, error) {
    path := "/api/traces/" + url.PathEscape(traceID)
    body, err := c.do(ctx, path, nil)
    // ...
    return &TraceResult{Body: body}, nil
}
```

**注意**：不校验 traceID 格式，让 Tempo 的 4xx 错误原样返回给调用方。

### 6.8 do 函数与防御

源码：[client.go#L206-L243](file:///d:/claude/ongrid/internal/pkg/tracequery/client.go#L206-L243)

```go
func (c *Client) do(ctx context.Context, path string, q url.Values) ([]byte, error) {
    base, rerr := c.base.ResolveBaseURL(ctx)
    // ...
    req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
    req.Header.Set("Accept", "application/json")
    req.Header.Set("User-Agent", "ongrid-tracequery/0.1")
    resp, err := c.httpClient.Do(req)
    body, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024)) // 16 MiB cap
    if resp.StatusCode == http.StatusNotFound {
        return nil, fmt.Errorf("tracequery: %s: not found", path)
    }
    if resp.StatusCode != http.StatusOK {
        return nil, fmt.Errorf("tracequery: %s returned %d: %s", path, resp.StatusCode, truncate(string(body), 512))
    }
    return body, nil
}
```

**防御点**：
1. **16 MiB body cap**（[L228](file:///d:/claude/ongrid/internal/pkg/tracequery/client.go#L228)）：单 trace 可能很大
2. **错误消息截断 512 字节**（[L240](file:///d:/claude/ongrid/internal/pkg/tracequery/client.go#L240)）：避免多 MB Tempo 错误页膨胀日志/chat context
3. **User-Agent**（[L220](file:///d:/claude/ongrid/internal/pkg/tracequery/client.go#L220)）：`ongrid-tracequery/0.1`，便于 Tempo 端识别

---

## 7. 配置层：TempoResolver 与 system_settings

### 7.1 TempoResolver 结构

源码：[internal/manager/biz/setting/telemetry.go#L72-L106](file:///d:/claude/ongrid/internal/manager/biz/setting/telemetry.go#L72-L106)

```go
// TempoResolver mirrors LokiResolver for the trace signal. The URL it
// resolves is the OTLP HTTP push endpoint the edge's traces plugin
// targets (otelcol exporters.otlphttp.endpoint).
type TempoResolver struct {
    svc         *Service
    fallbackURL string
}

func (r *TempoResolver) URL(ctx context.Context) string {
    if v := r.get(ctx, model.KeyTempoURL); v != "" {
        return strings.TrimRight(v, "/")
    }
    return r.fallbackURL
}

func (r *TempoResolver) Auth(ctx context.Context) (basicUser, basicPassword string) {
    return r.get(ctx, model.KeyTempoBasicUser), r.get(ctx, model.KeyTempoBasicPassword)
}

func (r *TempoResolver) TLSInsecure(ctx context.Context) bool {
    return strings.EqualFold(r.get(ctx, model.KeyTempoTLSInsecure), "true")
}
```

### 7.2 双层解析

| 层 | 来源 | 用途 |
|---|---|---|
| 1（优先） | `system_settings.tempo.url` | admin UI 修改的外部 Tempo |
| 2（fallback） | `cfg.Traces.URL`（env seed） | 内嵌 Tempo（`http://tempo:3200`） |

**关键**：`svc.Get` 走 60s cache（注释 [main.go#L476-L477](file:///d:/claude/ongrid/cmd/ongrid/main.go#L476-L477)），admin UI 修改在边缘下次 reload 时生效。

### 7.3 system_settings 键

源码：[internal/manager/model/setting/model.go#L179-L186](file:///d:/claude/ongrid/internal/manager/model/setting/model.go#L179-L186)

```go
// Well-known keys under CategoryTempo. Mirrors CategoryLoki; URL is the
// OTLP HTTP push endpoint (e.g. https://tempo.customer.com/v1/traces).
const (
    KeyTempoURL          = "url"
    KeyTempoBasicUser    = "basic_user"
    KeyTempoBasicPassword = "basic_password" // sensitive
    KeyTempoTLSInsecure  = "tls_insecure"
)
```

`CategoryTempo` 定义于 [L42](file:///d:/claude/ongrid/internal/manager/model/setting/model.go#L42)：

```go
CategoryTempo = "tempo" // external Tempo OTLP HTTP endpoint + auth
```

---

## 8. first-boot seed 与 env 配置

### 8.1 first-boot seed

源码：[cmd/ongrid/main.go#L455-L465](file:///d:/claude/ongrid/cmd/ongrid/main.go#L455-L465)

```go
// Loki / Tempo seeds. Mirrors the Prom seed pattern — first-boot
// only, admin edits in UI persist across restarts. The URL is the
// only field we seed; auth and TLS stay blank by default since the
// embedded loki/tempo containers don't authenticate.
if err := settingSvc.SetIfAbsent(rootCtx, settingmodel.CategoryTempo, settingmodel.KeyTempoURL, cfg.Traces.URL, false); err != nil {
    log.Warn("seed tempo url", slog.Any("err", err))
}
```

**关键**：`SetIfAbsent` 仅首次启动写入，admin UI 修改持久化跨重启。

### 8.2 Resolver 装配

源码：[cmd/ongrid/main.go#L478-L479](file:///d:/claude/ongrid/cmd/ongrid/main.go#L478-L479)

```go
lokiResolver := managerbizsetting.NewLokiResolver(settingSvc, cfg.Logs.URL)
tempoResolver := managerbizsetting.NewTempoResolver(settingSvc, cfg.Traces.URL)
```

### 8.3 env 配置

源码：[internal/pkg/config/config.go#L96-L102](file:///d:/claude/ongrid/internal/pkg/config/config.go#L96-L102)

```go
type TracesConfig struct {
    URL string
}

Traces: TracesConfig{
    URL: getEnv("ONGRID_TRACES_URL", "http://tempo:3200"),
},
```

- env：`ONGRID_TRACES_URL`
- 默认：`http://tempo:3200`（容器内 DNS）

### 8.4 OTel endpoint env

源码：[cmd/ongrid/main.go#L223-L225](file:///d:/claude/ongrid/cmd/ongrid/main.go#L223-L225)

```go
otelEndpoint := os.Getenv("ONGRID_OTEL_ENDPOINT")
if otelEndpoint == "" {
    otelEndpoint = "tempo:4318"
}
```

- env：`ONGRID_OTEL_ENDPOINT`
- 默认：`tempo:4318`（OTLP HTTP receiver）
- 空：禁用 tracing

**注意**：`ONGRID_TRACES_URL`（查询，:3200）与 `ONGRID_OTEL_ENDPOINT`（推送，:4318）是两个独立配置。

---

## 9. URL 探测 TempoURLProbe

### 9.1 结构

源码：[internal/manager/biz/setting/probe.go#L51-L80](file:///d:/claude/ongrid/internal/manager/biz/setting/probe.go#L51-L80)

```go
// TempoURLProbe is the trace-side counterpart. Tempo also exposes
// /ready returning 200 once compactors and ingesters have replayed.
type TempoURLProbe struct {
    resolver *TempoResolver
    timeout  time.Duration
}

func NewTempoURLProbe(r *TempoResolver) *TempoURLProbe {
    return &TempoURLProbe{resolver: r, timeout: 5 * time.Second}
}

func (p *TempoURLProbe) Probe(ctx context.Context) error {
    if p == nil || p.resolver == nil {
        return fmt.Errorf("tempo probe not wired")
    }
    u := p.resolver.URL(ctx)
    if u == "" {
        return fmt.Errorf("tempo url is empty")
    }
    // Tempo's /ready lives at the API root; the OTLP push URL on
    // otelcol-style endpoints is /v1/traces. Strip if present.
    base := strings.TrimSuffix(u, "/v1/traces")
    user, pass := p.resolver.Auth(ctx)
    tlsInsecure := p.resolver.TLSInsecure(ctx)
    return probeReadyEndpoint(ctx, base+"/ready", user, pass, tlsInsecure, p.timeout)
}
```

### 9.2 /ready 端点

Tempo 的 `/ready` 返回 200 当 compactors 和 ingesters 完成 replay。OnGrid 利用此端点做"测试连接"探测。

### 9.3 /v1/traces 后缀剥离

**关键**（[L76](file:///d:/claude/ongrid/internal/manager/biz/setting/probe.go#L76)）：admin 可能把 OTLP push URL（`https://tempo.example.com/v1/traces`）填入 `tempo.url`，探测时剥离 `/v1/traces` 后缀，让 `/ready` 落在 API root。

### 9.4 main.go 装配

源码：[cmd/ongrid/main.go#L985-L992](file:///d:/claude/ongrid/cmd/ongrid/main.go#L985-L992)

```go
// Loki / Tempo URL probes — back the Integrations "测试连接" buttons.
lokiProbe := managerbizsetting.NewLokiURLProbe(lokiResolver)
tempoProbe := managerbizsetting.NewTempoURLProbe(tempoResolver)
// ...
integrationHandler = managerserverintegration.NewHandler(grafanaSvc, promTester, lokiProbe, tempoProbe, webSearchProbe)
```

---

## 10. 系统健康检查 checkTempo

### 10.1 checkTempo 实现

源码：[internal/manager/service/systemhealth/service.go#L226-L236](file:///d:/claude/ongrid/internal/manager/service/systemhealth/service.go#L226-L236)

```go
func (s *Service) checkTempo(ctx context.Context) Check {
    return s.probe(ctx, "tempo", "observability", "Tempo", func(ctx context.Context) (Status, string, map[string]any) {
        if !s.cfg.TracesEnabled || s.deps.Tempo == nil {
            return StatusDegraded, "Tempo is disabled or not wired", map[string]any{"enabled": s.cfg.TracesEnabled}
        }
        if err := s.deps.Tempo.Probe(ctx); err != nil {
            return StatusFailed, "Tempo readiness probe failed: " + err.Error(), nil
        }
        return StatusOK, "Tempo readiness probe succeeded", nil
    })
}
```

### 10.2 三态判定

| Status | 条件 |
|---|---|
| `StatusOK` | `/ready` 返回 200 |
| `StatusDegraded` | `TracesEnabled=false` 或 `Tempo == nil` |
| `StatusFailed` | `/ready` 探测失败 |

### 10.3 装配

源码：[cmd/ongrid/main.go#L1725-L1737](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1725-L1737)

```go
systemHealthSvc := managersvcsystemhealth.New(managersvcsystemhealth.Config{
    // ...
    TracesEnabled:       cfg.Traces.URL != "",
    // ...
}, managersvcsystemhealth.Dependencies{
    // ...
    Tempo:     tempoProbe,
    // ...
})
```

**关键**：`TracesEnabled` 由 `cfg.Traces.URL != ""` 决定，与 system_settings 中的 admin 编辑独立。

---

## 11. AI 工具 query_traceql

### 11.1 工具元数据

源码：[internal/manager/biz/aiops/tools/query_traceql.go#L13-L65](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/query_traceql.go#L13-L65)

```go
const ToolNameQueryTraceQL = "query_traceql"

const QueryTraceQLDescription = "Run a TraceQL search against Tempo. " +
    "Use this to find traces by service / operation / latency / status. " +
    "Returns trace summaries (id, service, root span name, duration, span count)."

var QueryTraceQLSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": { "type": "string", "description": "TraceQL expression..." },
    "service": { "type": "string" },
    "operation": { "type": "string" },
    "start": { "type": "string", "description": "RFC3339. Defaults to now-1h." },
    "end": { "type": "string", "description": "RFC3339. Defaults to now." },
    "limit": { "type": "integer", "minimum": 1, "maximum": 1000 },
    "min_duration": { "type": "string" },
    "max_duration": { "type": "string" }
  }
}`)
```

### 11.2 反向 guard

源码：[query_traceql_basetool.go#L32-L36](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/query_traceql_basetool.go#L32-L36)

```go
const queryTraceQLWhenToUse = "When the user wants TRACES — span chains across services, latency outliers, " +
    "specific trace IDs, or 'which call took 5 seconds'. " +
    "NOT for log lines (use query_logql), NOT for metric trends (use query_promql), " +
    "NOT for live host stats (use get_host_load). " +
    "At least one filter (query / service / operation / duration) is required — Tempo unfiltered search is too expensive."
```

**关键**：明确告诉 LLM 何时不该用此工具，避免误用。

### 11.3 必填 filter guard

源码：[query_traceql.go#L99-L105](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/query_traceql.go#L99-L105)

```go
// Require *some* filter — either a TraceQL query, a tag (service /
// operation), or a duration bound. An unfiltered Tempo search is too
// expensive to dump on the LLM by accident.
if strings.TrimSpace(in.Query) == "" &&
    strings.TrimSpace(in.Service) == "" &&
    strings.TrimSpace(in.Operation) == "" &&
    strings.TrimSpace(in.MinDuration) == "" &&
    strings.TrimSpace(in.MaxDuration) == "" {
    return ExecuteResult{}, fmt.Errorf("query_traceql: at least one of query/service/operation/min_duration/max_duration required")
}
```

### 11.4 默认时间窗口

源码：[query_traceql.go#L107-L124](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/query_traceql.go#L107-L124)

```go
end := time.Now()
start := end.Add(-time.Hour)         // 默认最近 1 小时
if in.End != "" {
    t, _ := time.Parse(time.RFC3339, in.End)
    end = t
}
if in.Start != "" {
    t, _ := time.Parse(time.RFC3339, in.Start)
    start = t
} else if in.End != "" {
    start = end.Add(-time.Hour)       // 仅指定 end 时，start = end - 1h
}
```

### 11.5 30s 调用超时

源码：[query_traceql.go#L79-L81](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/query_traceql.go#L79-L81)

```go
const queryTraceqlCallTimeout = 30 * time.Second
```

镜像 `query_promql`，跨信号类型对称。

### 11.6 Tags 构造

源码：[query_traceql.go#L147-L158](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/query_traceql.go#L147-L158)

```go
tags := map[string]string{}
if in.Service != "" {
    tags["service.name"] = in.Service
}
if in.Operation != "" {
    tags["name"] = in.Operation
}
if len(tags) == 0 {
    tags = nil
}
```

**注意**：`SearchTraces` 在 `Query` 非空时忽略 `Tags`，这是期望的优先级。

### 11.7 装配

源码：[cmd/ongrid/main.go#L1170-L1174](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1170-L1174)

```go
var traceQuerier aiopstools.TraceQuerier
if cfg.Traces.URL != "" {
    traceQuerier = pkgtracequery.New(cfg.Traces.URL, log.With(slog.String("comp", "aiops-tracequery")))
}
toolsReg := aiopstools.NewRegistry(fbClient, edgeUC, deviceUC, promQuerier, logQuerier, traceQuerier, alertUC, log)
```

**关键**：`cfg.Traces.URL == ""` 时 `traceQuerier = nil`，`query_traceql` 工具不注册。

---

## 12. AI 工具 correlate_incident 的 trace panel

### 12.1 trace panel 拉取

源码：[internal/manager/biz/aiops/tools/correlate_incident.go#L263-L281](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/correlate_incident.go#L263-L281)

```go
// Trace panel — needs Tempo + a service label on the incident.
if r.traceQuery != nil {
    service := strings.TrimSpace(labels["service"])
    if service == "" {
        service = strings.TrimSpace(annotations["service"])
    }
    if service != "" {
        entries, err := r.queryTracePanel(callCtx, service, wStart, wEnd)
        if err != nil {
            bundle.Skipped["trace_panel"] = "tempo query failed: " + err.Error()
        } else {
            bundle.TracePanel = entries
        }
    } else {
        bundle.Skipped["trace_panel"] = "no service label on incident"
    }
} else {
    bundle.Skipped["trace_panel"] = "trace query client not configured"
}
```

### 12.2 service 标签解析

**关键**：trace panel 需要 incident 上的 `service` 标签（从 `labels` 或 `annotations` 解析）。无 service 标签时 skip，不阻塞其他 panel。

### 12.3 traceEntry 结构

源码：[correlate_incident.go#L134-L140](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/correlate_incident.go#L134-L140)

```go
type traceEntry struct {
    TraceID    string `json:"trace_id"`
    Service    string `json:"service,omitempty"`
    RootName   string `json:"root_name,omitempty"`
    DurationMS int64  `json:"duration_ms,omitempty"`
    StartTime  string `json:"start_time,omitempty"`
}
```

### 12.4 100KB bundle 上限

源码：[correlate_incident.go#L67-L68](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/correlate_incident.go#L67-L68)

```go
const correlateMaxResponseBytes = 100 * 1024 // 100 KB
```

超限时按噪音顺序裁剪（logs → traces），避免 LLM context 爆炸。

---

## 13. NL→TraceQL 翻译

### 13.1 query_translate 服务

源码：[internal/manager/server/aiops/query_translate.go#L2-L85](file:///d:/claude/ongrid/internal/manager/server/aiops/query_translate.go#L2-L85)

```go
// natural-language → LogQL/TraceQL/PromQL helper.

// Dialect: "logql" | "traceql" | "promql".

"traceql": `TraceQL（Tempo 查询语言）。规则：
...
"出错的 trace" → {status=error}
...`,
```

**用途**：SPA 输入自然语言查询时，LLM 按此 dialect 翻译为 TraceQL 表达式。

### 13.2 chatruntime intent 路由

源码：[internal/manager/biz/aiops/chatruntime/runtime.go#L1155](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go#L1155)

```go
traceIntent := strings.Contains(low, "trace") || strings.Contains(low, "traceql") || strings.Contains(low, "span") || strings.Contains(userText, "链路")
```

源码：[runtime.go#L1246-L1247](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go#L1246-L1247)

```go
if traceIntent && !logIntent && !topologyIntent && !hostIntent {
    if info.Name == "query_traceql" {
        // 优先选择 query_traceql 工具
    }
}
```

**关键**：用户输入"链路"、"trace"、"span"等关键词时，chatruntime 优先路由到 `query_traceql` 工具。

---

## 14. 告警评估 trace_latency / trace_error_rate

### 14.1 评估器定位

源码：[internal/manager/biz/alert/evaluators_phaseB.go#L1-L10](file:///d:/claude/ongrid/internal/manager/biz/alert/evaluators_phaseB.go#L1-L10)

```go
// evaluators_phaseB.go contains the Phase-B evaluators —
// log_match / log_volume against Loki, trace_latency / trace_error_rate
// against Prom (spanmetrics).
```

**关键**：trace_latency / trace_error_rate **不直接查 Tempo**，而是查 Prometheus 的 spanmetrics（由 Tempo metrics-generator 派生）。

### 14.2 evaluateTraceLatency

源码：[evaluators_phaseB.go#L165-L222](file:///d:/claude/ongrid/internal/manager/biz/alert/evaluators_phaseB.go#L165-L222)

```go
func (e *PipelineEvaluator) evaluateTraceLatency(ctx context.Context, now time.Time) {
    rules := e.rules.TraceLatencyRules()
    for _, rule := range rules {
        // rule.Expr 是预构建的 histogram_quantile() Prom 表达式
        // 包含 > threshold_ms 比较，Prom 只返回违例 series
        entries, err := runPromInstant(ctx, e.prom, rule.Expr, now)
        // ...
        for _, ent := range entries {
            dedupeKey := fmt.Sprintf("pipeline:%s:%s", rule.RuleKey, labelSetKey(ent.Labels))
            summary := fmt.Sprintf("%s: %s %s 延迟 %.1fms > %gms",
                rule.RuleKey, rule.Spec.Service, rule.Spec.Quantile, ent.Value, rule.Spec.ThresholdMs)
            // RecordFiring + notify
        }
        e.sweepRecovery(ctx, rule.RuleKey, fired, "trace_latency condition cleared", now)
    }
}
```

### 14.3 evaluateTraceErrorRate

源码：[evaluators_phaseB.go#L224-L278](file:///d:/claude/ongrid/internal/manager/biz/alert/evaluators_phaseB.go#L224-L278)

```go
func (e *PipelineEvaluator) evaluateTraceErrorRate(ctx context.Context, now time.Time) {
    rules := e.rules.TraceErrorRateRules()
    for _, rule := range rules {
        entries, err := runPromInstant(ctx, e.prom, rule.Expr, now)
        for _, ent := range entries {
            summary := fmt.Sprintf("%s: %s 错误率 %.2f%% %s %g%%",
                rule.RuleKey, rule.Spec.Service, ent.Value, rule.Spec.Operator, rule.Spec.ThresholdPct)
            // RecordFiring + notify
        }
        e.sweepRecovery(ctx, rule.RuleKey, fired, "trace_error_rate condition cleared", now)
    }
}
```

### 14.4 metric_raw 恢复模式

源码：[evaluators_phaseB.go#L280-L300](file:///d:/claude/ongrid/internal/manager/biz/alert/evaluators_phaseB.go#L280-L300)

```go
func (e *PipelineEvaluator) sweepRecovery(ctx context.Context, ruleKey string, fired map[string]struct{}, reason string, now time.Time) {
    prev := e.firingSnapshot[ruleKey]
    for prevKey := range prev {
        if _, stillFiring := fired[prevKey]; stillFiring {
            continue
        }
        e.uc.SystemResolveIncident(ctx, prevKey, reason, now)
    }
    e.firingSnapshot[ruleKey] = fired
}
```

**关键**：上一 tick 违例但本 tick 消失的 series 自动 resolve。`firingSnapshot` 跨 tick 持久化。

### 14.5 依赖链

```
Tempo metrics-generator
    ↓ remote_write
Prometheus traces_spanmetrics_latency_bucket / traces_spanmetrics_calls_total
    ↓ PromQL
alert evaluators (trace_latency / trace_error_rate)
    ↓ RecordFiring
alert incidents
```

**关键**：若 `tracing.Init` 未初始化，Tempo receiver 收不到 spans，metrics-generator 派生空矩阵，evaluators 永远读到空。注释明确这一依赖（[main.go#L217-L221](file:///d:/claude/ongrid/cmd/ongrid/main.go#L217-L221)）。

---

## 15. 插件端点解析 pluginEndpointResolver

### 15.1 traces 分支

源码：[cmd/ongrid/main.go#L2668-L2695](file:///d:/claude/ongrid/cmd/ongrid/main.go#L2668-L2695)

```go
case "traces":
    if r.tempo != nil {
        if u := edgeReachableTempoURL(r.tempo.URL(ctx)); u != "" {
            // If the admin URL already includes /v1/traces (some
            // OTLP endpoints publish the path inline), respect it.
            endpoint := u
            if !strings.HasSuffix(endpoint, "/v1/traces") {
                endpoint += "/v1/traces"
            }
            user, password := r.tempo.Auth(ctx)
            return managerbizk8s.TelemetryTarget{
                Endpoint:      endpoint,
                BasicUser:     user,
                BasicPassword: password,
                TLSInsecure:   r.tempo.TLSInsecure(ctx),
            }, nil
        }
    }
    if r.publicURL == "" {
        return managerbizk8s.TelemetryTarget{}, nil
    }
    return managerbizk8s.TelemetryTarget{
        Endpoint:               strings.TrimRight(r.publicURL, "/") + "/v1/traces",
        UseTelemetryCredential: true,
    }, nil
```

### 15.2 两层解析

| 层 | 条件 | Endpoint |
|---|---|---|
| 1（admin 自定义） | `tempo.url` 是 edge 可达的公网 URL | `u + /v1/traces` |
| 2（fallback） | `tempo.url` 是 docker-internal seed | `publicURL + /v1/traces` |

### 15.3 edgeReachableTempoURL

源码：[cmd/ongrid/main.go#L2708-L2713](file:///d:/claude/ongrid/cmd/ongrid/main.go#L2708-L2713)

```go
func edgeReachableTempoURL(u string) string {
    if !isEdgeReachableURL(u) {
        return ""
    }
    return strings.TrimRight(u, "/")
}
```

`isEdgeReachableURL` 判断 URL 是否是公网可达（非 docker-internal hostname 如 `tempo`）。

### 15.4 /v1/traces 后缀智能处理

**关键**（[L2671-L2676](file:///d:/claude/ongrid/cmd/ongrid/main.go#L2671-L2676)）：某些 OTLP endpoint 内联 `/v1/traces` 路径，resolver 检查后缀避免重复追加。

### 15.5 UseTelemetryCredential fallback

fallback 层使用 `UseTelemetryCredential: true`，表示边缘用 telemetry 凭证（cluster-scoped write-only identity）认证，而非 Basic auth。详见 [telemetry_auth.go](file:///d:/claude/ongrid/internal/manager/biz/k8s/telemetry_auth.go)（30s cache + 1024 entries 上限）。

---

## 16. Tempo 后端配置 tempo-config.yaml

源码：[deploy/install/tempo-config.yaml](file:///d:/claude/ongrid/deploy/install/tempo-config.yaml)

### 16.1 关键配置

```yaml
server:
  http_listen_port: 3200     # API
  grpc_listen_port: 9095

distributor:
  receivers:
    otlp:
      protocols:
        grpc:
          endpoint: 0.0.0.0:4317
        http:
          endpoint: 0.0.0.0:4318

ingester:
  trace_idle_period: 10s
  max_block_duration: 5m

compactor:
  compaction:
    block_retention: 168h            # 7 days (ADR-013 §决策第 4 条)
    max_block_bytes: 100_000_000     # ~100 MB blocks

metrics_generator:
  storage:
    remote_write:
      - url: http://prometheus:9090/prometheus/api/v1/write
        send_exemplars: true
  processor:
    span_metrics:
      dimensions:
        - service.name
        - span.kind
        - status.code
    service_graphs:
      max_items: 10000

storage:
  trace:
    backend: local
    local:
      path: /var/tempo/blocks

overrides:
  defaults:
    metrics_generator:
      processors:
        - service-graphs
        - span-metrics
    ingestion:
      rate_limit_bytes: 20_000_000     # 20 MB/s per tenant
      burst_size_bytes: 30_000_000
    global:
      max_bytes_per_trace: 50_000_000  # 50 MB cap per trace
```

### 16.2 关键设计

1. **filesystem backend**（[L54-L56](file:///d:/claude/ongrid/deploy/install/tempo-config.yaml#L54-L56)）：`backend: local`，无对象存储依赖
2. **7 天保留**（[L25](file:///d:/claude/ongrid/deploy/install/tempo-config.yaml#L25)）：`block_retention: 168h`（ADR-013 §决策第 4 条）
3. **spanmetrics + service-graphs 双 processor**（[L66-L68](file:///d:/claude/ongrid/deploy/install/tempo-config.yaml#L66-L68)）：派生 RED metrics + service map
4. **send_exemplars: true**（[L42](file:///d:/claude/ongrid/deploy/install/tempo-config.yaml#L42)）：允许 Prometheus exemplar 关联 trace_id
5. **20 MB/s per-tenant 速率限制**（[L70](file:///d:/claude/ongrid/deploy/install/tempo-config.yaml#L70)）：防止单租户压垮集群
6. **50 MB per-trace 上限**（[L73](file:///d:/claude/ongrid/deploy/install/tempo-config.yaml#L73)）：防止超大 trace 压垮 ingester

### 16.3 remote_write 目标

```yaml
remote_write:
  - url: http://prometheus:9090/prometheus/api/v1/write
```

注释明确（[L31-L33](file:///d:/claude/ongrid/deploy/install/tempo-config.yaml#L31-L33)）："remote_write target is the docker-internal Prom which has `--web.enable-remote-write-receiver` enabled per ADR-009"。

---

## 17. docker-compose 部署

### 17.1 tempo service

源码：[deploy/docker-compose.yml#L273-L289](file:///d:/claude/ongrid/deploy/docker-compose.yml#L273-L289)

```yaml
# Tempo (ADR-013). Single-binary trace backend; OTLP gRPC :4317 and
# HTTP :4318 are intentionally NOT published — public access flows
# through nginx /v1/traces with auth_request → manager edgeauth
# (ADR-014 §决策第 1 条).
tempo:
  image: docker.cnb.cool/ongridio/ongrid/tempo:2.10.0
  container_name: ongrid-tempo
  restart: unless-stopped
  command:
    - -config.file=/etc/tempo/tempo.yaml
  volumes:
    - tempo_data:/var/tempo
    - ./install/tempo-config.yaml:/etc/tempo/tempo.yaml:ro
  depends_on:
    - prometheus
  networks:
    - ongrid_net
```

### 17.2 端口不发布

**关键**：4317 / 4318 / 3200 都不发布到主机。所有公共访问通过 nginx：
- 推送：`nginx /v1/traces` + `auth_request → manager edgeauth`
- 查询：`manager /v1/traces/*`（认证后代理）

### 17.3 Grafana datasource 注入

源码：[docker-compose.yml#L352-L356](file:///d:/claude/ongrid/deploy/docker-compose.yml#L352-L356)

```yaml
grafana:
  environment:
    ONGRID_LOG_URL: ${ONGRID_LOG_URL:-http://loki:3100}
    ONGRID_TRACE_QUERY_URL: ${ONGRID_TRACE_QUERY_URL:-http://tempo:3200}
```

Grafana provisioning YAML 使用 `${ONGRID_TRACE_QUERY_URL}` 配置 Tempo datasource。

### 17.4 volume

源码：[docker-compose.yml#L416](file:///d:/claude/ongrid/deploy/docker-compose.yml#L416)

```yaml
tempo_data:
```

docker named volume（dev compose）；生产 install 使用 host bind-mount（`ONGRID_DATA_DIR=/var/lib/ongrid`）。

---

## 18. 内置知识库 tempo.md

源码：[internal/manager/biz/knowledge/builtin_vault/systems/observability-stack/tempo.md](file:///d:/claude/ongrid/internal/manager/biz/knowledge/builtin_vault/systems/observability-stack/tempo.md)

### 18.1 文档定位

OnGrid 内置知识库（HLD-017 凭证保险库 boot sync）中的 Tempo 主题文档，供 RAG 检索。LLM agent 在调查 trace 相关问题时可检索此文档。

### 18.2 核心内容

1. **lookup-vs-search 区别**：
   - `trace_id` lookup：O(1) 直接 block 读
   - TraceQL attribute search：O(N) 扫描 block，昂贵

2. **canonical 事故调查流程**：
   ```
   alert → metric exemplar → trace ID → Tempo lookup → drill into spans
   ```
   而非"browsing Tempo for slow requests"。

3. **metrics-generator 派生**：
   - `traces_spanmetrics_*` counters
   - `traces_service_graph_request_*`（service map）

4. **sampling 策略**：
   - Head sampling：根 span 决策，便宜但丢失 tail 兴趣
   - Tail sampling：buffer 完整 trace 后决策（errors, latency），更贵但更有用
   - Tail sampling 在 OTel Collector，不在 Tempo

5. **TraceQL 语法示例**：
   ```traceql
   { resource.service.name="api" && duration > 500ms }
   { status = error }
   { span.http.status_code = 500 }
   ```

6. **operational signals**：
   - `tempo_distributor_spans_received_total`
   - `tempo_ingester_blocks_flushed_total`
   - `tempo_request_duration_seconds` p99
   - `tempo_metrics_generator_processor_*`（常见静默失败点）

### 18.3 correlation glue

文档强调 Tempo 的价值在于三向关联：
- **metric exemplars**：Prometheus sample 标记 trace_id
- **log labels**：每条日志含 trace_id，Loki 查询直接 pivot 到 Tempo
- **service map**：generator 派生 `traces_service_graph_request_*`，Grafana 渲染依赖图

---

## 19. 并发与资源管理

### 19.1 tracequery.Client 并发安全

源码：[client.go#L39-L40](file:///d:/claude/ongrid/internal/pkg/tracequery/client.go#L39-L40)

```go
// Client wraps Tempo's /api/search + /api/traces/<id>. Safe for concurrent
// use.
type Client struct {
    base       BaseURLResolver
    httpClient *http.Client
    log        *slog.Logger
}
```

`http.Client` 本身并发安全，`BaseURLResolver` 实现需保证并发安全（`TempoResolver` 通过 `setting.Service` 的 60s cache + mutex 保证）。

### 19.2 16 MiB body cap

源码：[client.go#L228](file:///d:/claude/ongrid/internal/pkg/tracequery/client.go#L228)

```go
body, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024)) // 16 MiB cap
```

防止单个大 trace 压垮 manager 内存。

### 19.3 超时分层

| 层 | 超时 | 来源 |
|---|---|---|
| `tracequery.Client` default | 30s | [client.go#L49](file:///d:/claude/ongrid/internal/pkg/tracequery/client.go#L49) |
| `traces.Handler` search/getTrace | 30s | [http.go#L139](file:///d:/claude/ongrid/internal/manager/server/traces/http.go#L139), [L164](file:///d:/claude/ongrid/internal/manager/server/traces/http.go#L164) |
| `traces.Handler` tagValues | 15s | [http.go#L188](file:///d:/claude/ongrid/internal/manager/server/traces/http.go#L188) |
| `query_traceql` tool | 30s | [query_traceql.go#L81](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/query_traceql.go#L81) |
| `correlate_incident` tool | 60s（umbrella） | [correlate_incident.go#L63](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/correlate_incident.go#L63) |
| `TempoURLProbe` | 5s | [probe.go#L60](file:///d:/claude/ongrid/internal/manager/biz/setting/probe.go#L60) |
| `systemhealth.checkTempo` | `ProbeTimeout`（默认 3s） | [service.go#L120-L121](file:///d:/claude/ongrid/internal/manager/service/systemhealth/service.go#L120-L121) |

### 19.4 OTel SDK batch

源码：[tracing.go#L92-L96](file:///d:/claude/ongrid/internal/pkg/tracing/tracing.go#L92-L96)

```go
sdktrace.WithBatcher(exporter,
    sdktrace.WithBatchTimeout(2*time.Second),
),
```

2s batch flush，事故 spans 快速出现在 Tempo。

### 19.5 otelcol sending_queue

源码：[render.go#L151-L159](file:///d:/claude/ongrid/internal/edgeagent/plugins/traces/render.go#L151-L159)

```yaml
sending_queue:
  enabled: true
  num_consumers: 4
  queue_size: {{ .QueueSize }}
retry_on_failure:
  enabled: true
  initial_interval: 1s
  max_interval: 30s
  max_elapsed_time: 5m
```

边缘 otelcol 4 consumer 并发推送，5m 重试窗口，防 manager 短暂不可用时丢 spans。

### 19.6 telemetry auth cache

源码：[internal/manager/biz/k8s/telemetry_auth.go#L14-L15](file:///d:/claude/ongrid/internal/manager/biz/k8s/telemetry_auth.go#L14-L15)

```go
telemetryAuthCacheTTL        = 30 * time.Second
maxTelemetryAuthCacheEntries = 1024
```

边缘 telemetry 凭证认证 30s cache，1024 entries 上限，防 LRU 爆炸。

---

## 20. 架构红线与设计要点

### 20.1 红线

1. **OTLP 端口不发布**：4317 / 4318 / 3200 仅容器内，公共访问通过 nginx + auth_request（[docker-compose.yml#L273-L276](file:///d:/claude/ongrid/deploy/docker-compose.yml#L273-L276)）
2. **查询代理必须认证**：`traces.Handler.Register` 注释明确"Caller must wrap r in the auth middleware before calling"（[http.go#L49-L51](file:///d:/claude/ongrid/internal/manager/server/traces/http.go#L49-L51)）
3. **trace_id 路由最后注册**：避免 shadow `/tags/...` 路由（[http.go#L55-L56](file:///d:/claude/ongrid/internal/manager/server/traces/http.go#L55-L56)）
4. **query_traceql 必须有 filter**：Tempo 无过滤搜索太昂贵，guard 拒绝（[query_traceql.go#L99-L105](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/query_traceql.go#L99-L105)）
5. **tracing.Init 不 fatal**：初始化失败只 warn，manager 继续运行（spanmetrics evaluators 读空矩阵）（[main.go#L233-L235](file:///d:/claude/ongrid/cmd/ongrid/main.go#L233-L235)）
6. **traces.Handler nil-safe**：`cfg.Traces.URL == ""` 时传入 nil Querier，handler 注册但返回 503（[main.go#L1030-L1033](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1030-L1033)）
7. **不在边缘做 tail_sampling**：tail_sampling 是 manager 侧关注点，边缘保持无状态（[render.go#L27-L30](file:///d:/claude/ongrid/internal/edgeagent/plugins/traces/render.go#L27-L30)）
8. **包名 backend-decoupled**：`tracequery` 而非 `tempoquery`，为换 backend 保留单点替换空间（[doc.go#L6-L8](file:///d:/claude/ongrid/internal/pkg/tracequery/doc.go#L6-L8)）
9. **first-boot seed 仅 URL**：auth 和 TLS 留空（内嵌 Tempo 不认证），admin 通过 UI 配置外部 Tempo（[main.go#L457-L459](file:///d:/claude/ongrid/cmd/ongrid/main.go#L457-L459)）
10. **错误消息截断 512 字节**：避免多 MB Tempo 错误页膨胀日志/chat context（[client.go#L240](file:///d:/claude/ongrid/internal/pkg/tracequery/client.go#L240)）

### 20.2 设计要点

1. **双向集成**：推送（OTel SDK + otelcol-contrib）+ 查询（tracequery.Client + traces.Handler）
2. **三层派生**：Tempo metrics-generator → Prom remote_write → alert evaluators（不直接查 Tempo）
3. **动态 BaseURLResolver**：每次调用解析 baseURL，admin UI 修改无需重启
4. **OTLP-shaped JSON 透传**：`GetTrace` 直接返回 Tempo 原始 JSON，SPA 遍历 `resourceSpans` 无需 re-encode（[http.go#L171-L175](file:///d:/claude/ongrid/internal/manager/server/traces/http.go#L171-L175)）
5. **ParentBased 采样**：根 span 决策，子 span 跟随，保证 trace 完整性（[tracing.go#L98](file:///d:/claude/ongrid/internal/pkg/tracing/tracing.go#L98)）
6. **2s batch flush**：事故 spans 快速可见（[tracing.go#L95](file:///d:/claude/ongrid/internal/pkg/tracing/tracing.go#L95)）
7. **otelhttp span 命名**：`{METHOD} {ROUTE_PATTERN}`，spanmetrics generator 按路由切分（[main.go#L2194-L2199](file:///d:/claude/ongrid/cmd/ongrid/main.go#L2194-L2199)）
8. **pluginEndpointResolver /v1/traces 智能后缀**：admin URL 可能内联 `/v1/traces`，resolver 检查后缀避免重复（[main.go#L2671-L2676](file:///d:/claude/ongrid/cmd/ongrid/main.go#L2671-L2676)）
9. **TempoURLProbe /v1/traces 剥离**：探测 `/ready` 时剥离 `/v1/traces` 后缀（[probe.go#L76](file:///d:/claude/ongrid/internal/manager/biz/setting/probe.go#L76)）
10. **correlate_incident trace panel 需 service 标签**：无 service 标签时 skip，不阻塞其他 panel（[correlate_incident.go#L265-L278](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/correlate_incident.go#L265-L278)）

---

## 21. 附录：完整调用链

### 21.1 数据推送链（Manager）

```
HTTP request
    ↓ otelhttp middleware（[main.go#L2192-L2201](file:///d:/claude/ongrid/cmd/ongrid/main.go#L2192-L2201)）
    ↓ 创建 span（命名 {METHOD} {ROUTE}）
    ↓ handler 处理
    ↓ span 结束
    ↓ OTel SDK batcher（2s flush）
    ↓ OTLP HTTP exporter
    ↓ POST http://tempo:4318/v1/traces
    ↓ Tempo distributor → ingester → block → compactor
    ↓ metrics-generator 派生 traces_spanmetrics_*
    ↓ remote_write → http://prometheus:9090/prometheus/api/v1/write
    ↓ alert evaluators（trace_latency / trace_error_rate）
```

### 21.2 数据推送链（Edge Agent）

```
Local app OTLP push
    ↓ otelcol-contrib receiver（grpc :4317 / http :4318）
    ↓ memory_limiter → k8sattributes → resource/device（注入 device_id）
    ↓ batch（8192 / 5s）
    ↓ otlphttp/manager exporter
    ↓ POST {manager_public_url}/v1/traces
    ↓ nginx auth_request → manager edgeauth
    ↓ proxy_pass → http://tempo:4318/v1/traces
```

### 21.3 查询链（SPA Traces 页面）

```
SPA fetch /v1/traces/search?q=...
    ↓ manager auth middleware
    ↓ traces.Handler.search（[http.go#L66](file:///d:/claude/ongrid/internal/manager/server/traces/http.go#L66)）
    ↓ tracequery.Client.SearchTraces（[client.go#L112](file:///d:/claude/ongrid/internal/pkg/tracequery/client.go#L112)）
    ↓ BaseURLResolver.ResolveBaseURL（动态）
    ↓ GET {tempo_url}/api/search?q=...&limit=100&start=...&end=...
    ↓ Tempo querier → query-frontend（TraceQL 分片）
    ↓ 返回 {traces: [...], metrics: {...}}
    ↓ 透传 JSON 给 SPA
```

### 21.4 AI 工具链（query_traceql）

```
用户："查最近出错的 trace"
    ↓ chatruntime intent 路由（[runtime.go#L1155](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go#L1155)）
    ↓ 识别 traceIntent
    ↓ 选择 query_traceql 工具（[runtime.go#L1246-L1247](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go#L1246-L1247)）
    ↓ LLM 生成 args（query="{status=error}", start=now-1h, end=now）
    ↓ executeQueryTraceQL（[query_traceql.go#L85](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/query_traceql.go#L85)）
    ↓ filter guard（必填 filter）
    ↓ 30s 超时 context
    ↓ tracequery.Client.SearchTraces
    ↓ GET {tempo_url}/api/search?q={status=error}&limit=50&start=...&end=...
    ↓ 返回 ResultJSON 给 LLM
    ↓ LLM 推理 + 自然语言回复
```

### 21.5 告警评估链（trace_latency）

```
alert evaluator tick（每 5m）
    ↓ evaluateTraceLatency（[evaluators_phaseB.go#L169](file:///d:/claude/ongrid/internal/manager/biz/alert/evaluators_phaseB.go#L169)）
    ↓ runPromInstant（rule.Expr，histogram_quantile + > threshold_ms）
    ↓ promquery.Client.Query
    ↓ GET {prom_url}/api/v1/query?query=histogram_quantile(0.95,...)>200
    ↓ 解析 vector entries
    ↓ 每个 entry：RecordFiring + notify
    ↓ sweepRecovery（上一 tick 违例但本 tick 消失的 series 自动 resolve）
```

### 21.6 健康检查链

```
SPA /api/v1/systemhealth
    ↓ systemhealth.Service.Check（[service.go#L132](file:///d:/claude/ongrid/internal/manager/service/systemhealth/service.go#L132)）
    ↓ checkTempo（[service.go#L226](file:///d:/claude/ongrid/internal/manager/service/systemhealth/service.go#L226)）
    ↓ TempoURLProbe.Probe（[probe.go#L66](file:///d:/claude/ongrid/internal/manager/biz/setting/probe.go#L66)）
    ↓ TempoResolver.URL（动态解析 system_settings）
    ↓ 剥离 /v1/traces 后缀
    ↓ GET {tempo_base}/ready（5s 超时）
    ↓ 200 = OK，非 200 = Failed，未配置 = Degraded
```

---

## 22. 交叉引用

- [ongrid_configs.md](file:///d:/claude/ongrid/ongrid_configs.md)：完整配置说明（`ONGRID_TRACES_URL` / `ONGRID_OTEL_ENDPOINT`）
- [ongrid_integration.md](file:///d:/claude/ongrid/ongrid_integration.md)：8 个外部系统集成（含 Tempo）
- [ongrid_LLM.md](file:///d:/claude/ongrid/ongrid_LLM.md)：AIOps 编排（query_traceql 工具上下文）
- [ongrid_api.md](file:///d:/claude/ongrid/ongrid_api.md)：26 个业务域 API（含 /v1/traces/*）
- [ongrid_frontier.md](file:///d:/claude/ongrid/ongrid_frontier.md)：Frontier 集成（edge agent 通信隧道）
- [ongrid_grafana.md](file:///d:/claude/ongrid/ongrid_grafana.md)：Grafana 集成（Tempo datasource provisioning）
- [ongrid_architecture.md](file:///d:/claude/ongrid/ongrid_architecture.md)：架构总览

---

**文档版本**：v1.0
**生成时间**：2026-07-31
**覆盖源码版本**：v0.7.113
**Tempo 版本**：2.10.0
