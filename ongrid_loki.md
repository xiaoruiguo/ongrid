# OnGrid × Loki 集成技术实现文档

> 本文档深入分析 OnGrid 系统与 Grafana Loki 日志聚合后端的完整集成实现，覆盖数据平面（Promtail 推送）、控制平面（查询代理）、AI 工具、告警评估、健康探测、边缘 agent 转发、部署配置、内置知识库等全部代码路径。所有引用均锚定到具体文件路径与行号。

---

## 目录

1. [集成总览](#1-集成总览)
2. [分层文件索引](#2-分层文件索引)
3. [数据平面：Promtail 子进程插件](#3-数据平面promtail-子进程插件)
4. [数据平面：promtail.yaml 模板渲染](#4-数据平面promtailyaml-模板渲染)
5. [数据平面：nginx auth_request 网关](#5-数据平面nginx-auth_request-网关)
6. [控制平面：查询代理 HTTP Handler](#6-控制平面查询代理-http-handler)
7. [查询客户端 logquery.Client](#7-查询客户端-logqueryclient)
8. [配置层：LokiResolver 与 system_settings](#8-配置层lokiresolver-与-system_settings)
9. [first-boot seed 与 env 配置](#9-first-boot-seed-与-env-配置)
10. [URL 探测 LokiURLProbe](#10-url-探测-lokiurlprobe)
11. [系统健康检查 checkLoki](#11-系统健康检查-checkloki)
12. [AI 工具 query_logql](#12-ai-工具-query_logql)
13. [AI 工具 correlate_incident 的 log panel](#13-ai-工具-correlate_incident-的-log-panel)
14. [NL→LogQL 翻译](#14-nllogql-翻译)
15. [告警评估 log_match / log_volume](#15-告警评估-log_match--log_volume)
16. [告警预览 PreviewDeps.Log](#16-告警预览-previewdepslog)
17. [插件端点解析 pluginEndpointResolver](#17-插件端点解析-pluginendpointresolver)
18. [Mention 搜索 mentionLogClient](#18-mention-搜索-mentionlogclient)
19. [Loki 后端配置 loki-config.yaml](#19-loki-后端配置-loki-configyaml)
20. [docker-compose 部署](#20-docker-compose-部署)
21. [内置知识库 loki.md](#21-内置知识库-lokimd)
22. [并发与资源管理](#22-并发与资源管理)
23. [架构红线与设计要点](#23-架构红线与设计要点)
24. [附录：完整调用链](#24-附录完整调用链)

---

## 1. 集成总览

### 1.1 Loki 在 OnGrid 中的角色

OnGrid 与 Grafana Loki 的集成是**双向**的：

```
┌─────────────────────────────────────────────────────────────────┐
│                      OnGrid × Loki 集成全景                       │
├─────────────────────────────────────────────────────────────────┤
│                                                                  │
│  数据平面（push）                                                 │
│  ┌────────────┐  /loki/api/v1/push  ┌─────────┐                  │
│  │ Edge Agent │ ──────────────────► │         │                  │
│  │ Promtail   │   via manager nginx │  Loki   │                  │
│  │  subprocess│   auth_request      │ :3100   │                  │
│  └────────────┘                     │         │                  │
│                                     │  /ready │                  │
│  控制平面（query proxy）            │  /loki/ │                  │
│  ┌────────────┐  /v1/logs/query     │  api/*  │                  │
│  │ SPA Logs   │  _range             │         │                  │
│  │   page     │ ──────────────────┐ │         │                  │
│  └────────────┘                   ▼ ▼         ▼                  │
│  ┌──────────────────────────────────────────┐                    │
│  │ Manager logs.Handler                     │                    │
│  │ /v1/logs/query_range                     │                    │
│  │ /v1/logs/labels                          │                    │
│  │ /v1/logs/labels/{name}/values            │                    │
│  └──────────────────────────────────────────┘                    │
│                                                                  │
│  AI 工具                                                          │
│  ┌──────────────────────────────────────────┐                    │
│  │ query_logql tool                         │                    │
│  │ correlate_incident tool (log panel)      │                    │
│  └──────────────────────────────────────────┘                    │
│                                                                  │
│  告警评估（LogQL metric query）                                   │
│  ┌──────────────────────────────────────────┐                    │
│  │ log_match: count_over_time(stream [w])   │                    │
│  │ log_volume: 同上 + 阈值比较              │                    │
│  └──────────────────────────────────────────┘                    │
└─────────────────────────────────────────────────────────────────┘
```

### 1.2 两个关键端口

| 端口 | 用途 | 暴露方式 |
|---|---|---|
| `3100` | Loki API（`/ready` / `/loki/api/v1/*`） | 仅容器内（manager 代理 + nginx auth_request） |

源码：[docker-compose.yml#L258-L269](file:///d:/claude/ongrid/deploy/docker-compose.yml#L258-L269) 注释明确"only docker-internal port 3100 — public access flows through nginx /loki/api/v1/push with auth_request → manager edgeauth"。

### 1.3 数据流三层

1. **推送层**：Edge Agent Promtail 子进程 → nginx `/loki/api/v1/push` → `auth_request` → Loki
2. **查询层**：SPA → Manager `logs.Handler` → `logquery.Client` → Loki `/loki/api/v1/*`
3. **派生层**：Loki LogQL metric query（`count_over_time`）→ alert evaluators（log_match / log_volume）

### 1.4 三信号对称设计

OnGrid 的可观测性三信号（metrics / logs / traces）采用对称设计：

| 信号 | 客户端包 | HTTP Handler | Resolver | Probe | AI 工具 |
|---|---|---|---|---|---|
| Metrics | `promquery` | `metric.Handler` | `PromResolver` | `PromTester` | `query_promql` |
| Logs | `logquery` | `logs.Handler` | `LokiResolver` | `LokiURLProbe` | `query_logql` |
| Traces | `tracequery` | `traces.Handler` | `TempoResolver` | `TempoURLProbe` | `query_traceql` |

Loki 集成完全镜像 Prom 模式，便于维护和理解。

---

## 2. 分层文件索引

### 2.1 数据平面（推送）

| 文件 | 行号 | 作用 |
|---|---|---|
| [internal/edgeagent/plugins/logs/plugin.go](file:///d:/claude/ongrid/internal/edgeagent/plugins/logs/plugin.go) | L1-L47 | Edge logs plugin（Promtail 子进程） |
| [internal/edgeagent/plugins/logs/render.go](file:///d:/claude/ongrid/internal/edgeagent/plugins/logs/render.go) | L1-L344 | promtail.yaml 模板渲染 |
| [internal/manager/server/edgeauth/http.go](file:///d:/claude/ongrid/internal/manager/server/edgeauth/http.go) | L1-L129 | nginx auth_request 验证端点 |

### 2.2 控制平面（查询）

| 文件 | 行号 | 作用 |
|---|---|---|
| [internal/pkg/logquery/client.go](file:///d:/claude/ongrid/internal/pkg/logquery/client.go) | L1-L279 | Loki HTTP 查询客户端 |
| [internal/manager/server/logs/http.go](file:///d:/claude/ongrid/internal/manager/server/logs/http.go) | L1-L199 | 查询代理 HTTP handler |
| [cmd/ongrid/main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) | L1010-L1018 | logsHandler 装配 |

### 2.3 配置与探测

| 文件 | 行号 | 作用 |
|---|---|---|
| [internal/pkg/config/config.go](file:///d:/claude/ongrid/internal/pkg/config/config.go) | L84-L89 | LogsConfig（URL 字段） |
| [internal/manager/biz/setting/telemetry.go](file:///d:/claude/ongrid/internal/manager/biz/setting/telemetry.go) | L10-L70 | LokiResolver（URL/Auth/TLS） |
| [internal/manager/biz/setting/probe.go](file:///d:/claude/ongrid/internal/manager/biz/setting/probe.go) | L17-L49 | LokiURLProbe（/ready 探测） |
| [internal/manager/model/setting/model.go](file:///d:/claude/ongrid/internal/manager/model/setting/model.go) | L41, L155-L162 | CategoryLoki + KeyLoki* 常量 |
| [cmd/ongrid/main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) | L455-L479 | first-boot seed + Resolver 装配 |

### 2.4 AI 工具

| 文件 | 行号 | 作用 |
|---|---|---|
| [internal/manager/biz/aiops/tools/query_logql.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/query_logql.go) | L1-L162 | query_logql executor |
| [internal/manager/biz/aiops/tools/query_logql_basetool.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/query_logql_basetool.go) | - | BaseTool 形式 |
| [internal/manager/biz/aiops/tools/correlate_incident.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/correlate_incident.go) | L247-L261, L434-L488 | log panel 拉取 |
| [internal/manager/server/aiops/query_translate.go](file:///d:/claude/ongrid/internal/manager/server/aiops/query_translate.go) | L65 | NL→LogQL 翻译 |

### 2.5 告警与健康

| 文件 | 行号 | 作用 |
|---|---|---|
| [internal/manager/biz/alert/evaluators_phaseB.go](file:///d:/claude/ongrid/internal/manager/biz/alert/evaluators_phaseB.go) | L31-L163 | log_match / log_volume evaluators |
| [internal/manager/biz/alert/preview.go](file:///d:/claude/ongrid/internal/manager/biz/alert/preview.go) | - | PreviewDeps.Log |
| [internal/manager/service/systemhealth/service.go](file:///d:/claude/ongrid/internal/manager/service/systemhealth/service.go) | - | checkLoki |
| [cmd/ongrid/main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) | L1725-L1737 | systemhealth 装配 |

### 2.6 部署与文档

| 文件 | 行号 | 作用 |
|---|---|---|
| [deploy/docker-compose.yml](file:///d:/claude/ongrid/deploy/docker-compose.yml) | L258-L269 | loki service |
| [deploy/install/loki-config.yaml](file:///d:/claude/ongrid/deploy/install/loki-config.yaml) | - | Loki 单二进制配置 |
| [internal/manager/biz/knowledge/builtin_vault/systems/observability-stack/loki.md](file:///d:/claude/ongrid/internal/manager/biz/knowledge/builtin_vault/systems/observability-stack/loki.md) | L1-L132 | 内置知识库（RAG） |

---

## 3. 数据平面：Promtail 子进程插件

### 3.1 logs plugin 定位

源码：[internal/edgeagent/plugins/logs/plugin.go#L1-L47](file:///d:/claude/ongrid/internal/edgeagent/plugins/logs/plugin.go#L1-L47)

```go
// Package logs is the edge-side `logs` plugin.
//
// It wraps a Promtail subprocess: ongrid-edge writes a Promtail config
// derived from the manager-pushed PluginConfig, spawns promtail, and
// lets Promtail push directly to manager nginx /loki/api/v1/push.
// ongrid-edge does not touch the log byte stream.
package logs

const Name = "logs"

func New(binDir, workDir string, log *slog.Logger) plugins.Plugin {
    return plugins.NewSubprocess(plugins.SubprocessOpts{
        Name:         Name,
        Binary:       filepath.Join(binDir, "promtail"),
        WorkDir:      filepath.Join(workDir, Name),
        ConfigFile:   filepath.Join(workDir, Name, "promtail.yaml"),
        ConfigRender: render,
        Args: func(_ plugins.PluginConfig, configFile string) []string {
            return []string{
                "-config.file=" + configFile,
                "-positions.file=" + filepath.Join(filepath.Dir(configFile), "positions.yaml"),
            }
        },
        Log: log,
    })
}
```

**设计**：
- 边缘节点运行独立的 `promtail` 子进程
- ongrid-edge 不触碰 log 字节流，只渲染配置 + 管理子进程
- Plugin name `logs`（复数）匹配 Loki push endpoint path `/loki/api/v1/push`
- `positions.yaml` 存在 config 旁，re-create workdir 不丢失 journald cursor

### 3.2 与 traces plugin 的对称设计

| 维度 | logs plugin | traces plugin |
|---|---|---|
| 子进程 | `promtail` | `otelcol-contrib` |
| 接收协议 | journald / file | OTLP gRPC/HTTP |
| 推送端点 | `/loki/api/v1/push` | `/v1/traces` |
| 配置文件 | `promtail.yaml` | `otelcol.yaml` |
| 认证 | Basic / Bearer | Basic / Bearer |

---

## 4. 数据平面：promtail.yaml 模板渲染

### 4.1 模板结构

源码：[internal/edgeagent/plugins/logs/render.go#L30-L120](file:///d:/claude/ongrid/internal/edgeagent/plugins/logs/render.go#L30-L120)

```yaml
server:
  disable: true   # 无 HTTP API，只推送

clients:
  - url: {{ .Endpoint }}              # manager /loki/api/v1/push
    {{- if .AuthUser }}
    basic_auth:
      username: {{ .AuthUser }}
      password: {{ .AuthPass }}
    {{- else if .AuthPass }}
    bearer_token: {{ .AuthPass }}
    {{- end }}
    {{- if .TLSInsecureSkipVerify }}
    tls_config:
      insecure_skip_verify: true
    {{- end }}
    tenant_id: ongrid                 # X-Scope-OrgID
    backoff_config:
      min_period: 500ms
      max_period: 1m
      max_retries: 10
    batchsize: 1048576                # 1 MiB
    batchwait: 1s
    external_labels:
      device_id: "{{ .EdgeID }}"
      {{- if .KubernetesMode }}
      cluster_id: "{{ .ClusterID }}"
      {{- if .NodeName }}
      node: "{{ .NodeName }}"
      {{- end }}
      {{- end }}
      {{- range $k, $v := .ExtraLabels }}
      {{ $k }}: "{{ $v }}"
      {{- end }}

scrape_configs:
{{- if .KubernetesMode }}
  - job_name: kubernetes-pods
    pipeline_stages:
      - cri: {}
      - regex:
          source: filename
          expression: '^/var/log/pods/(?P<namespace>[^_]+)_(?P<pod>[^_]+)_(?P<pod_uid>[^/]+)/(?P<container>[^/]+)/[^/]+\.log$'
      - labels:
          namespace:
          pod:
          container:
    static_configs:
      - targets: [localhost]
        labels:
          ongrid_source: "kubernetes:pod"
          job: "kubernetes-pods"
          __path__: "{{ .PodLogPath }}"
{{- else }}
{{- if .EnableJournald }}
  - job_name: journald
    journal:
      max_age: 12h
      labels:
        ongrid_source: "journald"
    relabel_configs:
      - source_labels: ['__journal__systemd_unit']
        target_label:  'unit'
      - source_labels: ['__journal_syslog_identifier']
        target_label:  'identifier'
      {{- if .JournaldUnits }}
      - source_labels: ['__journal__systemd_unit']
        regex:         '{{ .JournaldUnitsRegex }}'
        action:        'keep'
      {{- end }}
      - source_labels: ['__journal_priority']
        target_label:  'level'
{{- end }}
{{- range .FilePaths }}
  - job_name: 'file-{{ . | jobNameSafe }}'
    static_configs:
      - targets: [localhost]
        labels:
          ongrid_source: 'file:{{ . }}'
          job:           'file'
          __path__:      '{{ . }}'
{{- end }}
{{- end }}
```

### 4.2 render 函数关键逻辑

源码：[render.go#L137-L228](file:///d:/claude/ongrid/internal/edgeagent/plugins/logs/render.go#L137-L228)

1. **Endpoint 必填**（[L138-L140](file:///d:/claude/ongrid/internal/edgeagent/plugins/logs/render.go#L138-L140)）：`cfg.Endpoint == ""` 返回错误
2. **device_id 必填**（[L141-L143](file:///d:/claude/ongrid/internal/edgeagent/plugins/logs/render.go#L141-L143)）：`cfg.EdgeID == 0` 返回错误（需设 `ONGRID_EDGE_ID`）
3. **journald 默认开**（[L168-L173](file:///d:/claude/ongrid/internal/edgeagent/plugins/logs/render.go#L168-L173)）：systemd 主机默认抓 journald
4. **Kubernetes 模式覆盖**（[L194-L197](file:///d:/claude/ongrid/internal/edgeagent/plugins/logs/render.go#L194-L197)）：`mode=kubernetes` 时清空 journald + file_paths，只抓 CRI pod 日志
5. **TLS 默认 skip-verify**（[L177-L182](file:///d:/claude/ongrid/internal/edgeagent/plugins/logs/render.go#L177-L182)）：注释明确"standard install ships a self-signed manager cert"
6. **Auth 双模式**（模板 L38-L44）：AuthUser+AuthPass=Basic，仅 AuthPass=Bearer
7. **fallback syslog**（[L191-L193](file:///d:/claude/ongrid/internal/edgeagent/plugins/logs/render.go#L191-L193)）：journald 关闭且无 file_paths 时 tail `/var/log/syslog` + `/var/log/messages`

### 4.3 journald 优先的设计

源码注释（[render.go#L161-L167](file:///d:/claude/ongrid/internal/edgeagent/plugins/logs/render.go#L161-L167)）：

```go
// Journald is the universal default: systemd-journald is always running
// on systemd hosts, whereas rsyslog / /var/log/syslog is NOT guaranteed
// (absent on Arch, Alpine, minimal cloud images, containers). It
// self-rotates (journald.conf SystemMaxUse) and tags every entry with
// its systemd unit, so services are cleanly separable by the `unit`
// label.
```

**关键**：journald 在 systemd 主机上普遍存在且自轮转，比 rsyslog 更可靠。

### 4.4 cardinality-safe 标签集

模板注释（[render.go#L17-L19](file:///d:/claude/ongrid/internal/edgeagent/plugins/logs/render.go#L17-L19)）：

```go
// attach the cardinality-safe label set (device_id +
// ongrid_source + optional service/host)
```

**关键**：只 attach 低基数标签（`device_id` / `ongrid_source` / `cluster_id` / `node`），避免 Loki 索引爆炸。高基数字段（如 `request_id`）绝不入 label。

### 4.5 Kubernetes CRI 解析

模板 L75-L82：

```yaml
pipeline_stages:
  - cri: {}
  - regex:
      source: filename
      expression: '^/var/log/pods/(?P<namespace>[^_]+)_(?P<pod>[^_]+)_(?P<pod_uid>[^/]+)/(?P<container>[^/]+)/[^/]+\.log$'
  - labels:
      namespace:
      pod:
      container:
```

从 CRI 文件名解析 `namespace` / `pod` / `container` 并提升为 label，便于 Loki 按 K8s 维度查询。

---

## 5. 数据平面：nginx auth_request 网关

### 5.1 edgeauth.Handler 定位

源码：[internal/manager/server/edgeauth/http.go#L1-L13](file:///d:/claude/ongrid/internal/manager/server/edgeauth/http.go#L1-L13)

```go
// Package edgeauth exposes a tiny internal HTTP endpoint that nginx's
// `auth_request` module calls before proxying telemetry data plane
// requests to downstream backends (Loki, Tempo, ...).
//
// The endpoint validates the Authorization header (Basic auth) through a
// wiring-provided authenticator. The manager exposes separate edge-only,
// telemetry-only, and compatibility endpoints so nginx can enforce the
// credential scope required by each exact data-plane route.
//
// This endpoint is mounted on the public mux (no JWT auth) because nginx
// itself is the only legitimate caller, and it lives behind the local
// docker network. nginx must NOT proxy_pass external traffic to it.
```

### 5.2 verify 端点

源码：[edgeauth/http.go#L67-L97](file:///d:/claude/ongrid/internal/manager/server/edgeauth/http.go#L67-L97)

```go
func (h *Handler) verify(w http.ResponseWriter, r *http.Request) {
    user, pass, ok := parseBasicAuth(r.Header.Get("Authorization"))
    if !ok {
        w.Header().Set("WWW-Authenticate", `Basic realm="ongrid-data-plane"`)
        http.Error(w, "missing or malformed Authorization header", http.StatusUnauthorized)
        return
    }

    identity, err := h.authn.AuthenticateDataPlane(r.Context(), user, pass)
    if err != nil {
        if errors.Is(err, errs.ErrUnauthorized) {
            http.Error(w, "unauthorized", http.StatusUnauthorized)
            return
        }
        http.Error(w, "auth backend error", http.StatusInternalServerError)
        return
    }

    // Surface edge_id back to nginx so it can pass through to downstream
    // (e.g. inject as a forced label header into Loki). nginx reads via
    // `auth_request_set $edge_id $upstream_http_x_edge_id;`.
    if identity.EdgeID != 0 {
        w.Header().Set("X-Edge-Id", uintToA(identity.EdgeID))
    }
    if identity.ClusterID != 0 {
        w.Header().Set("X-Cluster-Id", uintToA(identity.ClusterID))
    }
    w.WriteHeader(http.StatusOK)
}
```

### 5.3 X-Edge-Id 注入

**关键**（[L87-L92](file:///d:/claude/ongrid/internal/manager/server/edgeauth/http.go#L87-L92)）：验证成功后，通过 `X-Edge-Id` 响应头把 edge_id 回传给 nginx。nginx 通过 `auth_request_set` 捕获并注入到下游 Loki 请求头中，实现"边缘身份强制标签"。

### 5.4 nginx 路由模式

| nginx 路由 | auth_request 端点 | proxy_pass 目标 | 用途 |
|---|---|---|---|
| `/loki/api/v1/push` | `/internal/auth/dataplane-verify` | `http://loki:3100` | Promtail 推送 |
| `/v1/traces` | `/internal/auth/dataplane-verify` | `http://tempo:4318` | OTLP 推送 |

**关键**：公共访问只通过 nginx 这两条 push 路由，读路径（query）全部经 manager 代理。

---

## 6. 控制平面：查询代理 HTTP Handler

### 6.1 路由注册

源码：[internal/manager/server/logs/http.go#L51-L55](file:///d:/claude/ongrid/internal/manager/server/logs/http.go#L51-L55)

```go
func (h *Handler) Register(r chi.Router) {
    r.Get("/v1/logs/query_range", h.queryRange)
    r.Get("/v1/logs/labels", h.labels)
    r.Get("/v1/logs/labels/{name}/values", h.labelValues)
}
```

**三条路由**：
- `GET /v1/logs/query_range` — LogQL range 查询
- `GET /v1/logs/labels` — label 名列表（下拉填充）
- `GET /v1/logs/labels/{name}/values` — 单 label 值列表

### 6.2 Querier 接口

源码：[logs/http.go#L30-L34](file:///d:/claude/ongrid/internal/manager/server/logs/http.go#L30-L34)

```go
type Querier interface {
    QueryRange(ctx context.Context, opts logquery.QueryRangeOptions) (*logquery.QueryRangeResult, error)
    LabelNames(ctx context.Context, start, end time.Time) ([]string, error)
    LabelValues(ctx context.Context, name string, start, end time.Time) ([]string, error)
}
```

`*logquery.Client` 满足此接口；测试可注入 fake。

### 6.3 queryRange handler

源码：[logs/http.go#L64-L124](file:///d:/claude/ongrid/internal/manager/server/logs/http.go#L64-L124)

```go
func (h *Handler) queryRange(w http.ResponseWriter, r *http.Request) {
    if h.q == nil {
        writeErr(w, http.StatusServiceUnavailable, "logs backend disabled")
        return
    }
    q := r.URL.Query()
    from, err := parseTime(q.Get("start"))
    // ...
    limit := 1000
    if s := q.Get("limit"); s != "" {
        n, perr := strconv.Atoi(s)
        if perr != nil || n <= 0 || n > 5000 {
            writeErr(w, http.StatusBadRequest, "limit must be 1..5000")
            return
        }
        limit = n
    }
    // step / direction 解析
    ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
    defer cancel()
    out, err := h.q.QueryRange(ctx, logquery.QueryRangeOptions{
        Query:     q.Get("query"),
        Start:     from,
        End:       to,
        Limit:     limit,
        Step:      step,
        Direction: dir,
    })
    // ...
}
```

**关键点**：
- `h.q == nil` 时返回 503（[L65-L68](file:///d:/claude/ongrid/internal/manager/server/logs/http.go#L65-L68)）：Loki 禁用时 SPA 显示"logs disabled"
- 30s 超时（[L104](file:///d:/claude/ongrid/internal/manager/server/logs/http.go#L104)）：与 logquery.Client 的 defaultTimeout 对称
- limit 上限 5000（[L83](file:///d:/claude/ongrid/internal/manager/server/logs/http.go#L83)）：防止单查询拉爆内存

### 6.4 labels / labelValues handler

源码：[logs/http.go#L126-L167](file:///d:/claude/ongrid/internal/manager/server/logs/http.go#L126-L167)

```go
func (h *Handler) labels(w http.ResponseWriter, r *http.Request) {
    // ...
    ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
    defer cancel()
    out, err := h.q.LabelNames(ctx, from, to)
    // ...
    writeJSON(w, http.StatusOK, map[string]any{"labels": out})
}

func (h *Handler) labelValues(w http.ResponseWriter, r *http.Request) {
    name := chi.URLParam(r, "name")
    // ...
    out, err := h.q.LabelValues(ctx, name, from, to)
    writeJSON(w, http.StatusOK, map[string]any{"values": out})
}
```

**注意**：labels / labelValues 用 15s 超时（比 queryRange 的 30s 短），因为元数据查询通常更快。

### 6.5 parseTime 灵活解析

源码：[logs/http.go#L169-L189](file:///d:/claude/ongrid/internal/manager/server/logs/http.go#L169-L189)

```go
func parseTime(s string) (time.Time, error) {
    if s == "" {
        return time.Time{}, errors.New("missing")
    }
    if t, err := time.Parse(time.RFC3339, s); err == nil {
        return t, nil
    }
    if n, err := strconv.ParseInt(s, 10, 64); err == nil {
        switch {
        case n > 1e15:
            return time.Unix(0, n), nil    // 纳秒
        case n > 1e12:
            return time.UnixMilli(n), nil  // 毫秒
        default:
            return time.Unix(n, 0), nil    // 秒
        }
    }
    return time.Time{}, errs.ErrInvalid
}
```

**设计**：便于 curl 测试（`start=1700000000` 比 RFC3339 简洁）。

### 6.6 main.go 装配

源码：[cmd/ongrid/main.go#L1005-L1018](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1005-L1018)

```go
// Loki query proxy. Enables the in-product Logs page to
// run LogQL without exposing /loki/* read paths through nginx. The
// data plane /loki/api/v1/push route stays auth_request-gated for
// ingest only.
var logsHandler *managerserverlogs.Handler
if cfg.Logs.URL != "" {
    logsHandler = managerserverlogs.NewHandler(
        pkglogquery.New(cfg.Logs.URL, log.With(slog.String("comp", "logquery"))),
    )
} else {
    // Loki disabled — handler installs but every route returns 503.
    logsHandler = managerserverlogs.NewHandler(nil)
}
```

**关键**：`cfg.Logs.URL == ""` 时传入 `nil` Querier，handler 仍注册但所有路由返回 503。

---

## 7. 查询客户端 logquery.Client

### 7.1 包设计哲学

源码：[internal/pkg/logquery/client.go#L1-L13](file:///d:/claude/ongrid/internal/pkg/logquery/client.go#L1-L13)

```go
// Package logquery is the manager-side Loki query client.
//
// Mirrors internal/pkg/promquery and internal/pkg/tracequery — same
// shape, separate package so the three signal types stay independently
// swappable. Backend-decoupled name (logquery, not lokiquery) per
// — when Loki gets swapped for VictoriaLogs the
// package name and all import paths stay valid.
```

**关键**：包名 `logquery` 而非 `lokiquery`，为未来换 backend（如 VictoriaLogs）保留单点替换空间。

### 7.2 Client 结构

源码：[client.go#L53-L64](file:///d:/claude/ongrid/internal/pkg/logquery/client.go#L53-L64)

```go
// Client wraps Loki's /loki/api/v1/query_range + /label/<name>/values.
// Safe for concurrent use.
type Client struct {
    base       BaseURLResolver
    httpClient *http.Client
    log        *slog.Logger
}

const defaultTimeout = 30 * time.Second
```

### 7.3 BaseURLResolver 动态解析

源码：[client.go#L46-L51](file:///d:/claude/ongrid/internal/pkg/logquery/client.go#L46-L51)

```go
// BaseURLResolver returns the current Loki API root. Invoked once per
// call so admin-side URL changes propagate without a manager restart
// (mirrors promquery / tracequery patterns).
type BaseURLResolver interface {
    ResolveBaseURL(ctx context.Context) (string, error)
}
```

**关键**：每次调用都解析，admin UI 修改无需重启 manager。

### 7.4 三个构造函数

源码：[client.go#L75-L100](file:///d:/claude/ongrid/internal/pkg/logquery/client.go#L75-L100)

| 构造函数 | 用途 |
|---|---|
| `New(baseURL, log)` | 静态 baseURL，用默认 http.Client |
| `NewWithHTTPClient(baseURL, hc, log)` | 静态 baseURL + 自定义 http.Client（测试 seam） |
| `NewWithResolverAndHTTPClient(r, hc, log)` | 动态 resolver（生产用） |

### 7.5 QueryRange

源码：[client.go#L124-L171](file:///d:/claude/ongrid/internal/pkg/logquery/client.go#L124-L171)

```go
func (c *Client) QueryRange(ctx context.Context, opts QueryRangeOptions) (*QueryRangeResult, error) {
    if strings.TrimSpace(opts.Query) == "" {
        return nil, errors.New("logquery: query is empty")
    }
    if opts.Start.IsZero() || opts.End.IsZero() {
        return nil, errors.New("logquery: start/end required")
    }
    if !opts.End.After(opts.Start) {
        return nil, errors.New("logquery: end must be after start")
    }

    q := url.Values{}
    q.Set("query", opts.Query)
    q.Set("start", strconv.FormatInt(opts.Start.UnixNano(), 10))   // 纳秒
    q.Set("end", strconv.FormatInt(opts.End.UnixNano(), 10))
    limit := opts.Limit
    if limit <= 0 {
        limit = 1000
    }
    q.Set("limit", strconv.Itoa(limit))
    if opts.Step > 0 {
        q.Set("step", opts.Step.String())
    }
    dir := opts.Direction
    if dir == "" {
        dir = "backward"
    }
    q.Set("direction", dir)

    body, err := c.do(ctx, "/loki/api/v1/query_range", q)
    // ...
    var env struct {
        Status string           `json:"status"`
        Data   QueryRangeResult `json:"data"`
        Error  string           `json:"error"`
    }
    // ...
}
```

**QueryRangeOptions 字段**（[L103-L118](file:///d:/claude/ongrid/internal/pkg/logquery/client.go#L103-L118)）：

| 字段 | 用途 |
|---|---|
| `Query` | LogQL 表达式（必填） |
| `Start` / `End` | 时间窗口（必填，纳秒精度） |
| `Limit` | 结果行上限（默认 1000） |
| `Step` | metric query 步长（stream query 忽略） |
| `Direction` | `forward` / `backward`（默认 `backward`） |

### 7.6 X-Scope-OrgID 硬编码

源码：[client.go#L247-L249](file:///d:/claude/ongrid/internal/pkg/logquery/client.go#L247-L249)

```go
// Single-tenant install — nginx injects X-Scope-OrgID: ongrid on
// the ingest side, so use the same on read.
req.Header.Set("X-Scope-OrgID", "ongrid")
```

**关键**：单租户部署硬编码 `X-Scope-OrgID: ongrid`，与 nginx ingest 路径注入的值一致。

### 7.7 LabelNames / LabelValues

源码：[client.go#L173-L230](file:///d:/claude/ongrid/internal/pkg/logquery/client.go#L173-L230)

```go
func (c *Client) LabelNames(ctx context.Context, start, end time.Time) ([]string, error) {
    q := url.Values{}
    if !start.IsZero() {
        q.Set("start", strconv.FormatInt(start.UnixNano(), 10))
    }
    if !end.IsZero() {
        q.Set("end", strconv.FormatInt(end.UnixNano(), 10))
    }
    body, err := c.do(ctx, "/loki/api/v1/labels", q)
    // ...
}

func (c *Client) LabelValues(ctx context.Context, name string, start, end time.Time) ([]string, error) {
    // ...
    path := "/loki/api/v1/label/" + url.PathEscape(name) + "/values"
    body, err := c.do(ctx, path, q)
    // ...
}
```

**用途**：SPA Logs 页面填充 label 选择器自动补全。

### 7.8 do 函数与防御

源码：[client.go#L232-L272](file:///d:/claude/ongrid/internal/pkg/logquery/client.go#L232-L272)

```go
func (c *Client) do(ctx context.Context, path string, q url.Values) ([]byte, error) {
    base, rerr := c.base.ResolveBaseURL(ctx)
    // ...
    req.Header.Set("Accept", "application/json")
    req.Header.Set("User-Agent", "ongrid-logquery/0.1")
    req.Header.Set("X-Scope-OrgID", "ongrid")

    resp, err := c.httpClient.Do(req)
    // ...
    // 8 MiB cap
    body, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
    if resp.StatusCode != http.StatusOK {
        c.log.Warn("logquery: non-200",
            slog.Int("status", resp.StatusCode),
            slog.String("path", path),
        )
        return nil, fmt.Errorf("logquery: %s returned %d: %s", path, resp.StatusCode, truncate(string(body), 512))
    }
    return body, nil
}
```

**防御点**：
1. **8 MiB body cap**（[L260](file:///d:/claude/ongrid/internal/pkg/logquery/client.go#L260)）：比 tracequery 的 16 MiB 小，因为 log 行通常更小
2. **错误消息截断 512 字节**（[L269](file:///d:/claude/ongrid/internal/pkg/logquery/client.go#L269)）：避免多 MB Loki 错误页膨胀日志/chat context
3. **User-Agent**（[L246](file:///d:/claude/ongrid/internal/pkg/logquery/client.go#L246)）：`ongrid-logquery/0.1`，便于 Loki 端识别
4. **X-Scope-OrgID**（[L249](file:///d:/claude/ongrid/internal/pkg/logquery/client.go#L249)）：单租户标识

### 7.9 QueryRangeResult 双形态

源码：[client.go#L29-L37](file:///d:/claude/ongrid/internal/pkg/logquery/client.go#L29-L37)

```go
// QueryRangeResult is the unmarshalled `data` field from
// /loki/api/v1/query_range. Result holds either a streams response (log
// lines) or a matrix response (metric_over_time aggregations) — kept as
// raw JSON so the SPA can switch on `ResultType` ("streams" | "matrix")
// itself, mirroring promquery's pattern.
type QueryRangeResult struct {
    ResultType string          `json:"resultType"`
    Result     json.RawMessage `json:"result"`
}
```

**关键**：`Result` 是 `json.RawMessage`，由调用方根据 `ResultType` 决定如何解码（streams vs matrix）。

---

## 8. 配置层：LokiResolver 与 system_settings

### 8.1 LokiResolver 结构

源码：[internal/manager/biz/setting/telemetry.go#L10-L70](file:///d:/claude/ongrid/internal/manager/biz/setting/telemetry.go#L10-L70)

```go
// LokiResolver reads loki.url + optional basic auth from system_settings.
// Used by:
//   - PluginConfigUC.FetchForEdge to decide where the edge's logs plugin
//     pushes (customer Loki vs manager nginx fall-through to the
//     docker-internal Loki).
//   - HTTP handler for "test connection" probes from the Integrations UI.
//
// fallbackURL is the env-seeded default (cfg.Logs.URL); when the DB row
// is missing or empty we fall back to that so a fresh install with no
// admin edits still resolves to the embedded Loki.
type LokiResolver struct {
    svc         *Service
    fallbackURL string
}

func (r *LokiResolver) URL(ctx context.Context) string {
    if r == nil {
        return ""
    }
    if v := r.get(ctx, model.KeyLokiURL); v != "" {
        return strings.TrimRight(v, "/")
    }
    return r.fallbackURL
}

func (r *LokiResolver) Auth(ctx context.Context) (basicUser, basicPassword string) {
    // ...
    return r.get(ctx, model.KeyLokiBasicUser), r.get(ctx, model.KeyLokiBasicPassword)
}

func (r *LokiResolver) TLSInsecure(ctx context.Context) bool {
    // ...
    return strings.EqualFold(r.get(ctx, model.KeyLokiTLSInsecure), "true")
}
```

### 8.2 双层解析

| 层 | 来源 | 用途 |
|---|---|---|
| 1（优先） | `system_settings.loki.url` | admin UI 修改的外部 Loki |
| 2（fallback） | `cfg.Logs.URL`（env seed） | 内嵌 Loki（`http://loki:3100`） |

**关键**：`svc.Get` 走 60s cache（注释 [main.go#L476-L477](file:///d:/claude/ongrid/cmd/ongrid/main.go#L476-L477)），admin UI 修改在边缘下次 reload 时生效。

### 8.3 system_settings 键

源码：[internal/manager/model/setting/model.go#L41](file:///d:/claude/ongrid/internal/manager/model/setting/model.go#L41), [L155-L162](file:///d:/claude/ongrid/internal/manager/model/setting/model.go#L155-L162)

```go
CategoryLoki = "loki" // external Loki / VictoriaLogs URL + auth

const (
    KeyLokiURL          = "url"
    KeyLokiBasicUser    = "basic_user"
    KeyLokiBasicPassword = "basic_password" // sensitive
    KeyLokiTLSInsecure  = "tls_insecure"
)
```

### 8.4 TempoResolver 镜像

源码注释明确（[telemetry.go#L72-L74](file:///d:/claude/ongrid/internal/manager/biz/setting/telemetry.go#L72-L74)）：

```go
// TempoResolver mirrors LokiResolver for the trace signal.
```

两个 Resolver 完全对称，便于维护。

---

## 9. first-boot seed 与 env 配置

### 9.1 first-boot seed

源码：[cmd/ongrid/main.go#L456-L462](file:///d:/claude/ongrid/cmd/ongrid/main.go#L456-L462)

```go
// Loki / Tempo seeds. Mirrors the Prom seed pattern — first-boot
// only, admin edits in UI persist across restarts. The URL is the
// only field we seed; auth and TLS stay blank by default since the
// embedded loki/tempo containers don't authenticate.
if err := settingSvc.SetIfAbsent(rootCtx, settingmodel.CategoryLoki, settingmodel.KeyLokiURL, cfg.Logs.URL, false); err != nil {
    log.Warn("seed loki url", slog.Any("err", err))
}
```

**关键**：`SetIfAbsent` 仅首次启动写入，admin UI 修改持久化跨重启。auth 和 TLS 留空（内嵌 Loki 不认证）。

### 9.2 Resolver 装配

源码：[cmd/ongrid/main.go#L478](file:///d:/claude/ongrid/cmd/ongrid/main.go#L478)

```go
lokiResolver := managerbizsetting.NewLokiResolver(settingSvc, cfg.Logs.URL)
```

### 9.3 env 配置

源码：[internal/pkg/config/config.go#L84-L89](file:///d:/claude/ongrid/internal/pkg/config/config.go#L84-L89)

```go
type LogsConfig struct {
    URL string
}

Logs: LogsConfig{
    URL: getEnv("ONGRID_LOGS_URL", "http://loki:3100"),
},
```

- env：`ONGRID_LOGS_URL`
- 默认：`http://loki:3100`（容器内 DNS）

---

## 10. URL 探测 LokiURLProbe

### 10.1 结构

源码：[internal/manager/biz/setting/probe.go#L17-L49](file:///d:/claude/ongrid/internal/manager/biz/setting/probe.go#L17-L49)

```go
// LokiURLProbe is a URLProbe implementation that hits the configured
// Loki's /ready endpoint. Used by the Integrations card's "测试连接"
// button. Returns nil iff the URL is reachable, the auth header (if
// supplied) is accepted, and Loki returns 200/2xx.
//
// The 5s deadline is intentionally tight — operators expect the probe
// to either succeed quickly or fail with a clear "timed out" message;
// no point waiting for slow networks here when the real ingest path
// has its own retry budget.
type LokiURLProbe struct {
    resolver *LokiResolver
    timeout  time.Duration
}

func NewLokiURLProbe(r *LokiResolver) *LokiURLProbe {
    return &LokiURLProbe{resolver: r, timeout: 5 * time.Second}
}

func (p *LokiURLProbe) Probe(ctx context.Context) error {
    if p == nil || p.resolver == nil {
        return fmt.Errorf("loki probe not wired")
    }
    u := p.resolver.URL(ctx)
    if u == "" {
        return fmt.Errorf("loki url is empty")
    }
    user, pass := p.resolver.Auth(ctx)
    tlsInsecure := p.resolver.TLSInsecure(ctx)
    return probeReadyEndpoint(ctx, u+"/ready", user, pass, tlsInsecure, p.timeout)
}
```

### 10.2 /ready 端点

Loki 的 `/ready` 返回 200 当所有组件就绪。OnGrid 利用此端点做"测试连接"探测。

### 10.3 probeReadyEndpoint 共享函数

源码：[probe.go#L266-L296](file:///d:/claude/ongrid/internal/manager/biz/setting/probe.go#L266-L296)

```go
func probeReadyEndpoint(ctx context.Context, fullURL, user, pass string, insecure bool, timeout time.Duration) error {
    cctx, cancel := context.WithTimeout(ctx, timeout)
    defer cancel()
    req, err := http.NewRequestWithContext(cctx, http.MethodGet, fullURL, nil)
    // ...
    if user != "" {
        req.SetBasicAuth(user, pass)
    }
    client := &http.Client{
        Timeout: timeout,
        Transport: &http.Transport{
            TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}, //nolint:gosec // operator opt-in
        },
    }
    resp, err := client.Do(req)
    // ...
    if resp.StatusCode >= 200 && resp.StatusCode < 300 {
        return nil
    }
    body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
    return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}
```

**关键**：
- 错误 body 截断 200 字节（[L294](file:///d:/claude/ongrid/internal/manager/biz/setting/probe.go#L294)）：避免多 MB 错误页膨胀 UI
- Loki / Tempo / WebSearch 共用此函数

### 10.4 main.go 装配

源码：[cmd/ongrid/main.go#L985-L992](file:///d:/claude/ongrid/cmd/ongrid/main.go#L985-L992)

```go
// Loki / Tempo URL probes — back the Integrations "测试连接" buttons.
lokiProbe := managerbizsetting.NewLokiURLProbe(lokiResolver)
tempoProbe := managerbizsetting.NewTempoURLProbe(tempoResolver)
webSearchProbe := managerbizsetting.NewWebSearchProbe(managerbizsetting.NewWebSearchResolver(settingSvc))
integrationHandler = managerserverintegration.NewHandler(grafanaSvc, promTester, lokiProbe, tempoProbe, webSearchProbe)
```

---

## 11. 系统健康检查 checkLoki

### 11.1 checkLoki 实现

源码：[internal/manager/service/systemhealth/service.go](file:///d:/claude/ongrid/internal/manager/service/systemhealth/service.go)

`checkLoki` 与 `checkTempo` 对称实现，三态判定：

| Status | 条件 |
|---|---|
| `StatusOK` | `/ready` 返回 200 |
| `StatusDegraded` | `LogsEnabled=false` 或 `Loki == nil` |
| `StatusFailed` | `/ready` 探测失败 |

### 11.2 装配

源码：[cmd/ongrid/main.go#L1725-L1737](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1725-L1737)

```go
systemHealthSvc := managersvcsystemhealth.New(managersvcsystemhealth.Config{
    // ...
}, managersvcsystemhealth.Dependencies{
    // ...
    Loki:      lokiProbe,
    Tempo:     tempoProbe,
    // ...
})
```

---

## 12. AI 工具 query_logql

### 12.1 工具元数据

源码：[internal/manager/biz/aiops/tools/query_logql.go#L13-L21](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/query_logql.go#L13-L21)

```go
const ToolNameQueryLogQL = "query_logql"

const QueryLogQLDescription = "Run a LogQL range query against Loki. " +
    "Use this to investigate log patterns, error counts, or pipe into per-edge filters. " +
    "Returns the raw Loki response (streams or matrix)."
```

### 12.2 Schema

源码：[query_logql.go#L45-L74](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/query_logql.go#L45-L74)

```json
{
  "type": "object",
  "properties": {
    "query": { "type": "string", "description": "LogQL expression. Example: \"{edge_id=\\\"1\\\"} |= \\\"error\\\"\"." },
    "start": { "type": "string", "description": "RFC3339 start time. Defaults to now-1h." },
    "end": { "type": "string", "description": "RFC3339 end time. Defaults to now." },
    "limit": { "type": "integer", "minimum": 1, "maximum": 5000 },
    "direction": { "type": "string", "enum": ["backward", "forward"] }
  },
  "required": ["query"]
}
```

### 12.3 parseLogQLTime 灵活时间

源码：[query_logql.go#L23-L43](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/query_logql.go#L23-L43)

```go
func parseLogQLTime(value string, fallback time.Time) (time.Time, error) {
    value = strings.TrimSpace(value)
    if value == "" || strings.EqualFold(value, "now") {
        return fallback, nil
    }
    if rest, ok := strings.CutPrefix(strings.ToLower(value), "now-"); ok {
        d, err := time.ParseDuration(rest)
        // ...
        return time.Now().Add(-d), nil
    }
    if rest, ok := strings.CutPrefix(strings.ToLower(value), "now+"); ok {
        // ...
    }
    return time.Parse(time.RFC3339, value)
}
```

**关键**：支持 `now` / `now-1h` / `now+30m` / RFC3339 四种格式，便于 LLM 生成。

### 12.4 默认时间窗口

源码：[query_logql.go#L105-L124](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/query_logql.go#L105-L124)

```go
end := time.Now()
start := end.Add(-time.Hour)         // 默认最近 1 小时
if in.End != "" {
    t, err := parseLogQLTime(in.End, end)
    // ...
    end = t
}
if in.Start != "" {
    t, err := parseLogQLTime(in.Start, start)
    // ...
    start = t
} else if in.End != "" {
    // User pinned end but not start — keep the 1h window relative to
    // the supplied end so the call is still bounded.
    start = end.Add(-time.Hour)
}
```

### 12.5 30s 调用超时

源码：[query_logql.go#L87](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/query_logql.go#L87)

```go
const queryLogqlCallTimeout = 30 * time.Second
```

镜像 `query_promql` / `query_traceql`，跨信号类型对称。

### 12.6 默认 limit=200

源码：[query_logql.go#L126-L129](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/query_logql.go#L126-L129)

```go
limit := in.Limit
if limit <= 0 {
    limit = 200
}
```

**关键**：AI 工具默认 limit=200（比 HTTP handler 的 1000 小），避免 LLM context 爆炸。

### 12.7 装配

源码：[cmd/ongrid/main.go#L1166-L1174](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1166-L1174)

```go
var logQuerier aiopstools.LogQuerier
if cfg.Logs.URL != "" {
    logQuerier = pkglogquery.New(cfg.Logs.URL, log.With(slog.String("comp", "aiops-logquery")))
}
toolsReg := aiopstools.NewRegistry(fbClient, edgeUC, deviceUC, promQuerier, logQuerier, traceQuerier, alertUC, log)
```

**关键**：`cfg.Logs.URL == ""` 时 `logQuerier = nil`，`query_logql` 工具不注册。

---

## 13. AI 工具 correlate_incident 的 log panel

### 13.1 log panel 拉取

源码：[internal/manager/biz/aiops/tools/correlate_incident.go#L247-L261](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/correlate_incident.go#L247-L261)

```go
// Log panel — needs Loki + edge_id (so we can scope the query).
if r.logQuery != nil {
    if inc.DeviceID != nil {
        entries, err := r.queryLogPanel(callCtx, *inc.DeviceID, wStart, wEnd)
        if err != nil {
            bundle.Skipped["log_panel"] = "loki query failed: " + err.Error()
        } else {
            bundle.LogPanel = entries
        }
    } else {
        bundle.Skipped["log_panel"] = "incident has no edge_id"
    }
} else {
    bundle.Skipped["log_panel"] = "log query client not configured"
}
```

### 13.2 queryLogPanel 实现

源码：[correlate_incident.go#L434-L488](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/correlate_incident.go#L434-L488)

```go
func (r *Registry) queryLogPanel(ctx context.Context, edgeID uint64, start, end time.Time) ([]logEntry, error) {
    q := fmt.Sprintf(`{edge_id="%d"} |~ "(?i)error|panic|oom|fatal|fail"`, edgeID)
    res, err := r.logQuery.QueryRange(ctx, logquery.QueryRangeOptions{
        Query:     q,
        Start:     start,
        End:       end,
        Limit:     50,
        Direction: "backward",
    })
    // ...
    var raw []struct {
        Stream map[string]string `json:"stream"`
        Values [][2]string       `json:"values"`
    }
    // ...
    entries := make([]logEntry, 0, 64)
    for _, st := range raw {
        for _, v := range st.Values {
            ts := parseLokiNanoTimestamp(v[0])
            line := truncateLine(v[1], 200)
            entries = append(entries, logEntry{Timestamp: ts, Line: line, Labels: st.Stream})
        }
    }
    sort.SliceStable(entries, func(i, j int) bool {
        return entries[i].Timestamp.After(entries[j].Timestamp)
    })
    if len(entries) > 50 {
        entries = entries[:50]
    }
    return entries, nil
}
```

### 13.3 错误关键词正则

**关键**（[L435](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/correlate_incident.go#L435)）：查询使用 `|~ "(?i)error|panic|oom|fatal|fail"` 正则匹配错误关键词，case-insensitive。

### 13.4 行截断 200 字符

源码：[correlate_incident.go#L483-L488](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/correlate_incident.go#L483-L488)

```go
func truncateLine(s string, n int) string {
    if len(s) <= n {
        return s
    }
    return s[:n] + "…"
}
```

**关键**：每行截断 200 字符，避免长 stack trace 膨胀 LLM context。

### 13.5 50 条上限 + 倒序

- `Limit: 50`：最多 50 条
- `Direction: "backward"`：最新优先
- 二次排序保证（[L466-L468](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/correlate_incident.go#L466-L468)）：Loki 返回可能跨 stream，需重新按时间排序

### 13.6 100KB bundle 上限

源码：[correlate_incident.go#L67-L68](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/correlate_incident.go#L67-L68)

```go
const correlateMaxResponseBytes = 100 * 1024 // 100 KB
```

源码：[correlate_incident.go#L649-L694](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/correlate_incident.go#L649-L694)

超限时按噪音顺序裁剪：
1. logs → 10 条（[L661-L665](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/correlate_incident.go#L661-L665)）
2. traces → 5 条（[L674-L678](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/correlate_incident.go#L674-L678)）
3. metric values → 丢弃（[L687-L692](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/correlate_incident.go#L687-L692)）

---

## 14. NL→LogQL 翻译

### 14.1 query_translate 服务

源码：[internal/manager/server/aiops/query_translate.go#L2-L65](file:///d:/claude/ongrid/internal/manager/server/aiops/query_translate.go#L2-L65)

```go
// natural-language → LogQL/TraceQL/PromQL helper.

// Dialect: "logql" | "traceql" | "promql".

"logql": `LogQL（Loki 查询语言）。规则：
...
```

**用途**：SPA 输入自然语言查询时，LLM 按此 dialect 翻译为 LogQL 表达式。

### 14.2 chatruntime intent 路由

源码：[internal/manager/biz/aiops/chatruntime/runtime.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go)

chatruntime 通过关键词识别 logIntent（如"日志"、"log"、"error"），优先路由到 `query_logql` 工具。

---

## 15. 告警评估 log_match / log_volume

### 15.1 评估器定位

源码：[internal/manager/biz/alert/evaluators_phaseB.go#L1-L10](file:///d:/claude/ongrid/internal/manager/biz/alert/evaluators_phaseB.go#L1-L10)

```go
// evaluators_phaseB.go contains the Phase-B evaluators —
// log_match / log_volume against Loki, trace_latency / trace_error_rate
// against Prom (spanmetrics).
```

**关键**：log_match / log_volume **直接查 Loki**（与 trace_* 查 Prom 不同）。

### 15.2 evaluateLogMatch

源码：[evaluators_phaseB.go#L31-L93](file:///d:/claude/ongrid/internal/manager/biz/alert/evaluators_phaseB.go#L31-L93)

```go
func (e *PipelineEvaluator) evaluateLogMatch(ctx context.Context, now time.Time) {
    rules := e.rules.LogMatchRules()
    for _, rule := range rules {
        expr := buildLogMatchQuery(rule.StreamSelector, rule.LineFilter, rule.Window)
        entries, err := runLokiInstant(ctx, e.logq, expr, now)
        // ...
        for _, ent := range entries {
            if !compareFloat(ent.Value, rule.Operator, rule.Threshold) {
                continue
            }
            dedupeKey := fmt.Sprintf("pipeline:%s:%s", rule.RuleKey, labelSetKey(ent.Labels))
            fired[dedupeKey] = struct{}{}
            summary := fmt.Sprintf("%s: log_match %g %s %g (labels=%s)",
                rule.RuleKey, ent.Value, rule.Operator, rule.Threshold, labelSetKey(ent.Labels))
            // RecordFiring + notify
        }
        e.sweepRecovery(ctx, rule.RuleKey, fired, "log_match condition cleared", now)
    }
}
```

### 15.3 buildLogMatchQuery

源码：[evaluators_phaseB.go#L403-L414](file:///d:/claude/ongrid/internal/manager/biz/alert/evaluators_phaseB.go#L403-L414)

```go
func buildLogMatchQuery(stream, filter, window string) string {
    if window == "" {
        window = "5m"
    }
    if filter == "" {
        return fmt.Sprintf("count_over_time(%s [%s])", stream, window)
    }
    return fmt.Sprintf("count_over_time(%s |~ %q [%s])", stream, filter, window)
}
```

**LogQL 形式**：
- 无 filter：`count_over_time({job="api"} [5m])`
- 有 filter：`count_over_time({job="api"} |~ "error" [5m])`

### 15.4 runLokiInstant

源码：[evaluators_phaseB.go#L347-L389](file:///d:/claude/ongrid/internal/manager/biz/alert/evaluators_phaseB.go#L347-L389)

```go
// runLokiInstant queries Loki via QueryRange over a tight 60s window
// ending at `now` with a 30s step, then takes the latest sample of
// each matrix series — the closest LogQL approximation to "evaluate
// count_over_time as of now".
func runLokiInstant(ctx context.Context, l LogQuerier, expr string, now time.Time) ([]vectorEntry, error) {
    if l == nil {
        return nil, nil
    }
    res, err := l.QueryRange(ctx, logquery.QueryRangeOptions{
        Query: expr,
        Start: now.Add(-60 * time.Second),
        End:   now,
        Step:  30 * time.Second,
        Limit: 1000,
    })
    // ...
    // 取每个 series 的最后一个 sample（最新值）
    last := s.Values[len(s.Values)-1]
    // ...
}
```

**关键**：Loki 无 Prom 的 `query`（instant）端点，通过 60s 窗口 + 30s step + 取最新 sample 近似"as of now"。

### 15.5 evaluateLogVolume

源码：[evaluators_phaseB.go#L101-L163](file:///d:/claude/ongrid/internal/manager/biz/alert/evaluators_phaseB.go#L101-L163)

```go
// evaluateLogVolume — v1 implementation reuses the log_match shape
// (current-window count vs absolute threshold). The "ratio vs previous
// window" semantics in the original spec is left for a future pass; the
// current shape already covers "log volume crossed N" alerts which is
// the common ask.
func (e *PipelineEvaluator) evaluateLogVolume(ctx context.Context, now time.Time) {
    // 与 evaluateLogMatch 几乎相同，只是 rule kind 不同
}
```

**关键**：log_volume v1 复用 log_match 形状（当前窗口 count vs 绝对阈值），"与前窗口比率"语义留待未来实现。

### 15.6 sweepRecovery 自动恢复

源码：[evaluators_phaseB.go#L283-L300](file:///d:/claude/ongrid/internal/manager/biz/alert/evaluators_phaseB.go#L283-L300)

```go
func (e *PipelineEvaluator) sweepRecovery(ctx context.Context, ruleKey string, fired map[string]struct{}, reason string, now time.Time) {
    if e.firingSnapshot == nil {
        e.firingSnapshot = map[string]map[string]struct{}{}
    }
    prev := e.firingSnapshot[ruleKey]
    for prevKey := range prev {
        if _, stillFiring := fired[prevKey]; stillFiring {
            continue
        }
        if _, err := e.uc.SystemResolveIncident(ctx, prevKey, reason, now); err != nil {
            // ...
        }
    }
    e.firingSnapshot[ruleKey] = fired
}
```

**关键**：上一 tick 违例但本 tick 消失的 series 自动 resolve。`firingSnapshot` 跨 tick 持久化。

### 15.7 装配

源码：[cmd/ongrid/main.go#L2485-L2493](file:///d:/claude/ongrid/cmd/ongrid/main.go#L2485-L2493)

```go
if cfg.Alert.Enabled {
    // ...
    var alertLogQuerier managerbizalert.LogQuerier
    if cfg.Logs.URL != "" {
        alertLogQuerier = pkglogquery.New(cfg.Logs.URL, log.With(slog.String("comp", "alert-logquery")))
    }
    pipelineEval := managerbizalert.NewPipelineEvaluator(managerbizalert.PipelineEvaluatorOpts{
        // ...
    })
}
```

**关键**：`cfg.Logs.URL == ""` 时 `alertLogQuerier = nil`，log_match / log_volume 规则静默跳过（仍出现在 UI 列表，但不评估）。

---

## 16. 告警预览 PreviewDeps.Log

### 16.1 PreviewDeps 装配

源码：[cmd/ongrid/main.go#L1124-L1137](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1124-L1137)

```go
// Wire the read-only preview clients (Prom range + Loki range). Each
// is optional — when nil, the corresponding kind returns skipped_reason
// instead of a hard error.
{
    previewDeps := managerbizalert.PreviewDeps{}
    if promQueryClient != nil {
        previewDeps.Prom = promQueryClient
    }
    if cfg.Logs.URL != "" {
        previewDeps.Log = pkglogquery.New(cfg.Logs.URL, log.With(slog.String("comp", "alert-preview-log")))
    }
    alertSvc.SetPreviewDeps(previewDeps)
}
```

**用途**：告警规则编辑页面的"预览"功能，实时验证 LogQL 表达式能否返回数据。

---

## 17. 插件端点解析 pluginEndpointResolver

### 17.1 logs 分支

源码：[cmd/ongrid/main.go#L2647-L2667](file:///d:/claude/ongrid/cmd/ongrid/main.go#L2647-L2667)

```go
case "logs":
    if r.loki != nil {
        if u := edgeReachableLokiURL(r.loki.URL(ctx)); u != "" {
            user, password := r.loki.Auth(ctx)
            return managerbizk8s.TelemetryTarget{
                Endpoint:      u + "/loki/api/v1/push",
                BasicUser:     user,
                BasicPassword: password,
                TLSInsecure:   r.loki.TLSInsecure(ctx),
            }, nil
        }
    }
    if r.publicURL == "" {
        return managerbizk8s.TelemetryTarget{}, nil
    }
    return managerbizk8s.TelemetryTarget{
        Endpoint:               strings.TrimRight(r.publicURL, "/") + "/loki/api/v1/push",
        UseTelemetryCredential: true,
    }, nil
```

### 17.2 两层解析

| 层 | 条件 | Endpoint |
|---|---|---|
| 1（admin 自定义） | `loki.url` 是 edge 可达的公网 URL | `u + /loki/api/v1/push` |
| 2（fallback） | `loki.url` 是 docker-internal seed | `publicURL + /loki/api/v1/push` |

### 17.3 edgeReachableLokiURL

源码：[cmd/ongrid/main.go#L2697-L2706](file:///d:/claude/ongrid/cmd/ongrid/main.go#L2697-L2706)

```go
// edgeReachableLokiURL returns the URL when it looks like an
// admin-configured external endpoint (a public hostname or IP), and ""
// when it's the docker-internal seed which the edge can't reach. The
// caller falls back to the manager's PublicURL in the latter case.
func edgeReachableLokiURL(u string) string {
    if !isEdgeReachableURL(u) {
        return ""
    }
    return strings.TrimRight(u, "/")
}
```

`isEdgeReachableURL` 判断 URL 是否是公网可达（非 docker-internal hostname 如 `loki`）。

### 17.4 UseTelemetryCredential fallback

fallback 层使用 `UseTelemetryCredential: true`，表示边缘用 telemetry 凭证（cluster-scoped write-only identity）认证，而非 Basic auth。

### 17.5 docker-internal seed 判定

源码注释（[main.go#L2617-L2626](file:///d:/claude/ongrid/cmd/ongrid/main.go#L2617-L2626)）：

```go
// We treat any URL whose hostname looks like the docker-internal
// service name (loki, tempo, prometheus, grafana) — i.e. has no dot
// and no port-without-host — as a marker that the admin hasn't
// overridden the seed and we should fall through to PublicURL.
```

**关键**：URL hostname 无点（如 `loki`）视为 docker-internal seed，fallback 到 PublicURL。

---

## 18. Mention 搜索 mentionLogClient

### 18.1 mentionLogClient 装配

源码：[cmd/ongrid/main.go#L1454-L1459](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1454-L1459)

```go
// device + alert biz + Loki client. Any nil dep just means that
// type returns no results — graceful for deployments without Loki
var mentionLogClient *pkglogquery.Client
if cfg.Logs.URL != "" {
    mentionLogClient = pkglogquery.New(cfg.Logs.URL, log.With(slog.String("comp", "mention-logquery")))
}
```

**用途**：chat 中 `@mention` 搜索时，从 Loki 拉取相关日志行作为上下文。

---

## 19. Loki 后端配置 loki-config.yaml

源码：[deploy/install/loki-config.yaml](file:///d:/claude/ongrid/deploy/install/loki-config.yaml)

### 19.1 关键配置

```yaml
auth_enabled: false         # 单租户模式

server:
  http_listen_port: 3100

ingester:
  max_idle_period: 5m
  max_chunk_age: 1h
  chunk_target_size: 1048576
  chunk_idle_period: 30m

limits_config:
  ingestion_rate_mb: 10             # per-tenant MB/s
  ingestion_burst_size_mb: 20
  max_streams_per_user: 100000      # stream cardinality cap
  max_query_length: 720h            # 30 days max query window
  max_query_parallelism: 32
  retention_period: 168h            # 7 days

compactor:
  working_directory: /loki/compactor
  shared_store: filesystem
  retention_enabled: true
  retention_delete_delay: 2h

storage_config:
  boltdb_shipper:
    active_index_directory: /loki/index
    cache_location: /loki/boltdb-cache
    shared_store: filesystem
  filesystem:
    directory: /loki/chunks

schema_config:
  configs:
    - from: 2024-01-01
      store: tsdb
      object_store: filesystem
      schema: v13
      index:
        prefix: index_
        period: 24h
```

### 19.2 关键设计

1. **单租户模式**（`auth_enabled: false`）：与 logquery.Client 硬编码 `X-Scope-OrgID: ongrid` 配合
2. **filesystem backend**：无对象存储依赖
3. **7 天保留**（`retention_period: 168h`）：与 Tempo 一致
4. **100k streams cap**（`max_streams_per_user: 100000`）：防止高基数标签爆炸
5. **30 天查询窗口**（`max_query_length: 720h`）：防止超大查询
6. **TSDB schema v13**：现代 Loki 推荐的 index 格式

---

## 20. docker-compose 部署

### 20.1 loki service

源码：[deploy/docker-compose.yml#L258-L269](file:///d:/claude/ongrid/deploy/docker-compose.yml#L258-L269)

```yaml
# Loki (ADR-012). Single-binary log backend, only docker-internal port
# 3100 — public access flows through nginx /loki/api/v1/push with
# auth_request → manager edgeauth
loki:
  image: docker.cnb.cool/ongridio/ongrid/loki:3.4.0
  container_name: ongrid-loki
  restart: unless-stopped
  command:
    - -config.file=/etc/loki/local-config.yaml
  volumes:
    - loki_data:/loki
    - ./install/loki-config.yaml:/etc/loki/local-config.yaml:ro
  networks:
    - ongrid_net
```

### 20.2 端口不发布

**关键**：3100 端口不发布到主机。所有公共访问通过 nginx：
- 推送：`nginx /loki/api/v1/push` + `auth_request → manager edgeauth`
- 查询：`manager /v1/logs/*`（认证后代理）

### 20.3 Grafana datasource 注入

源码：[docker-compose.yml#L352-L356](file:///d:/claude/ongrid/deploy/docker-compose.yml#L352-L356)

```yaml
grafana:
  environment:
    ONGRID_LOG_URL: ${ONGRID_LOG_URL:-http://loki:3100}
    ONGRID_TRACE_QUERY_URL: ${ONGRID_TRACE_QUERY_URL:-http://tempo:3200}
```

Grafana provisioning YAML 使用 `${ONGRID_LOG_URL}` 配置 Loki datasource。

### 20.4 volume

源码：[docker-compose.yml#L415](file:///d:/claude/ongrid/deploy/docker-compose.yml#L415)

```yaml
loki_data:
```

docker named volume（dev compose）；生产 install 使用 host bind-mount（`ONGRID_DATA_DIR=/var/lib/ongrid`）。

### 20.5 Promtail binary 分发

Promtail binary 通过 edge bundle 分发到边缘节点（`ONGRID_EDGE_BUNDLE_DIR=/usr/share/ongrid/edge-bundles`）。详见 [ongrid_configs.md](file:///d:/claude/ongrid/ongrid_configs.md)。

---

## 21. 内置知识库 loki.md

源码：[internal/manager/biz/knowledge/builtin_vault/systems/observability-stack/loki.md](file:///d:/claude/ongrid/internal/manager/biz/knowledge/builtin_vault/systems/observability-stack/loki.md)

### 21.1 文档定位

OnGrid 内置知识库（HLD-017 凭证保险库 boot sync）中的 Loki 主题文档，供 RAG 检索。LLM agent 在调查 log 相关问题时可检索此文档。

### 21.2 核心内容

1. **"Prometheus for logs" 定位**：只索引 label，不索引 log 内容，存储便宜
2. **stream 是工作单元**：相同 label set 的 log 行集合
3. **cardinality 陷阱**：
   - 新 label → 新 stream → 新 chunk lineage
   - 高基数 label（如 `request_id`）→ 索引爆炸
4. **LogQL 两半**：
   - Log query（返回 log 行）：`{job="api"} |= "error"`
   - Metric query（返回数字序列）：`rate({job="api"} |= "error" [5m])`
5. **pipeline stage 顺序**：`|=` 比 `| json |` 便宜，先过滤再解析
6. **成本模型**：ingest bandwidth + storage + query CPU/mem + index size
7. **关键配置**：
   - `ingestion_rate_mb: 10`
   - `max_streams_per_user: 100000`
   - `max_query_length: 720h`
   - `retention_period: 168h`
8. **operational signals**：
   - `loki_distributor_lines_received_total`
   - `loki_ingester_streams_created_total` rate（stream churn 早期警告）
   - `loki_request_duration_seconds` p99

### 21.3 OnGrid edge 集成说明

源码：[loki.md#L121-L122](file:///d:/claude/ongrid/internal/manager/biz/knowledge/builtin_vault/systems/observability-stack/loki.md#L121-L122)

```markdown
Ongrid edge: subprocess promtail bundled as a plugin (see ADR-015 /
PR-C1). Data plane goes nginx → loki direct, not through the tunnel.
```

**关键**：数据平面走 nginx → loki 直连，**不通过 tunnel**（与控制信令分离）。

---

## 22. 并发与资源管理

### 22.1 logquery.Client 并发安全

源码：[client.go#L53-L59](file:///d:/claude/ongrid/internal/pkg/logquery/client.go#L53-L59)

```go
// Client wraps Loki's /loki/api/v1/query_range + /label/<name>/values.
// Safe for concurrent use.
type Client struct {
    base       BaseURLResolver
    httpClient *http.Client
    log        *slog.Logger
}
```

`http.Client` 本身并发安全，`BaseURLResolver` 实现需保证并发安全（`LokiResolver` 通过 `setting.Service` 的 60s cache + mutex 保证）。

### 22.2 8 MiB body cap

源码：[client.go#L260](file:///d:/claude/ongrid/internal/pkg/logquery/client.go#L260)

```go
body, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
```

防止单个大查询压垮 manager 内存。比 tracequery 的 16 MiB 小，因为 log 行通常更小。

### 22.3 超时分层

| 层 | 超时 | 来源 |
|---|---|---|
| `logquery.Client` default | 30s | [client.go#L64](file:///d:/claude/ongrid/internal/pkg/logquery/client.go#L64) |
| `logs.Handler` queryRange | 30s | [logs/http.go#L104](file:///d:/claude/ongrid/internal/manager/server/logs/http.go#L104) |
| `logs.Handler` labels / labelValues | 15s | [logs/http.go#L135](file:///d:/claude/ongrid/internal/manager/server/logs/http.go#L135), [L159](file:///d:/claude/ongrid/internal/manager/server/logs/http.go#L159) |
| `query_logql` tool | 30s | [query_logql.go#L87](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/query_logql.go#L87) |
| `correlate_incident` tool | 60s（umbrella） | [correlate_incident.go#L63](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/correlate_incident.go#L63) |
| `LokiURLProbe` | 5s | [probe.go#L34](file:///d:/claude/ongrid/internal/manager/biz/setting/probe.go#L34) |
| `runLokiInstant`（alert） | 30s（继承 Client） | [evaluators_phaseB.go#L351-L357](file:///d:/claude/ongrid/internal/manager/biz/alert/evaluators_phaseB.go#L351-L357) |

### 22.4 Promtail backoff + retry

源码：[render.go#L53-L58](file:///d:/claude/ongrid/internal/edgeagent/plugins/logs/render.go#L53-L58)

```yaml
backoff_config:
  min_period: 500ms
  max_period: 1m
  max_retries: 10
batchsize: 1048576    # 1 MiB
batchwait: 1s
```

Promtail 内置退避重试，10 次重试，最大间隔 1 分钟。批量推送 1 MiB / 1s。

### 22.5 多 Client 实例隔离

main.go 为不同用途创建独立 Client（独立 comp 标签便于日志追踪）：

| Client | comp 标签 | 用途 |
|---|---|---|
| `pkglogquery.New(cfg.Logs.URL, "logquery")` | `logquery` | SPA Logs 页面查询 |
| `pkglogquery.New(cfg.Logs.URL, "aiops-logquery")` | `aiops-logquery` | query_logql AI 工具 |
| `pkglogquery.New(cfg.Logs.URL, "alert-logquery")` | `alert-logquery` | 告警评估 |
| `pkglogquery.New(cfg.Logs.URL, "alert-preview-log")` | `alert-preview-log` | 告警规则预览 |
| `pkglogquery.New(cfg.Logs.URL, "mention-logquery")` | `mention-logquery` | chat mention 搜索 |

**关键**：每个 Client 独立 http.Client + 30s timeout，互不影响。

---

## 23. 架构红线与设计要点

### 23.1 红线

1. **3100 端口不发布**：仅容器内，公共访问通过 nginx + auth_request（[docker-compose.yml#L258-L260](file:///d:/claude/ongrid/deploy/docker-compose.yml#L258-L260)）
2. **查询代理必须认证**：`logs.Handler.Register` 注释明确"Caller must wrap r in the auth middleware before calling"（[logs/http.go#L48-L50](file:///d:/claude/ongrid/internal/manager/server/logs/http.go#L48-L50)）
3. **数据平面 vs 控制平面分离**：push 走 nginx auth_request，query 走 manager 代理（[logs/http.go#L1-L5](file:///d:/claude/ongrid/internal/manager/server/logs/http.go#L1-L5)）
4. **logs.Handler nil-safe**：`cfg.Logs.URL == ""` 时传入 nil Querier，handler 注册但返回 503（[main.go#L1015-L1017](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1015-L1017)）
5. **X-Scope-OrgID 硬编码 ongrid**：单租户部署与 nginx ingest 路径一致（[client.go#L247-L249](file:///d:/claude/ongrid/internal/pkg/logquery/client.go#L247-L249)）
6. **first-boot seed 仅 URL**：auth 和 TLS 留空（内嵌 Loki 不认证），admin 通过 UI 配置外部 Loki（[main.go#L457-L459](file:///d:/claude/ongrid/cmd/ongrid/main.go#L457-L459)）
7. **cardinality-safe 标签集**：只 attach 低基数标签（`device_id` / `ongrid_source` / `cluster_id` / `node`），高基数字段绝不入 label（[render.go#L17-L19](file:///d:/claude/ongrid/internal/edgeagent/plugins/logs/render.go#L17-L19)）
8. **错误消息截断 512 字节**：避免多 MB Loki 错误页膨胀日志/chat context（[client.go#L269](file:///d:/claude/ongrid/internal/pkg/logquery/client.go#L269)）
9. **包名 backend-decoupled**：`logquery` 而非 `lokiquery`，为换 backend 保留单点替换空间（[client.go#L5-L7](file:///d:/claude/ongrid/internal/pkg/logquery/client.go#L5-L7)）
10. **journald 默认开**：systemd 主机普遍存在且自轮转，比 rsyslog 可靠（[render.go#L161-L167](file:///d:/claude/ongrid/internal/edgeagent/plugins/logs/render.go#L161-L167)）

### 23.2 设计要点

1. **三信号对称设计**：Loki / Tempo / Prom 三个 backend 采用相同的 Resolver / Probe / Client / Handler / AI 工具模式
2. **动态 BaseURLResolver**：每次调用解析 baseURL，admin UI 修改无需重启
3. **QueryRangeResult 双形态**：`Result` 是 `json.RawMessage`，由调用方根据 `ResultType` 决定如何解码（streams vs matrix）
4. **runLokiInstant 近似 instant**：Loki 无 Prom 的 `query` 端点，通过 60s 窗口 + 30s step + 取最新 sample 近似
5. **correlate_incident 错误关键词正则**：`|~ "(?i)error|panic|oom|fatal|fail"` case-insensitive 匹配
6. **100KB bundle 上限 + 噪音顺序裁剪**：logs → traces → metric values
7. **多 Client 实例隔离**：不同用途独立 Client + comp 标签
8. **Promtail positions.yaml 持久化**：re-create workdir 不丢失 journald cursor（[plugin.go#L39](file:///d:/claude/ongrid/internal/edgeagent/plugins/logs/plugin.go#L39)）
9. **Kubernetes CRI 解析**：从文件名解析 namespace/pod/container 提升为 label
10. **fallback syslog**：journald 关闭且无 file_paths 时 tail `/var/log/syslog` + `/var/log/messages`，避免零 job 静默失败

---

## 24. 附录：完整调用链

### 24.1 数据推送链（Edge Agent）

```
Local log source (journald / file / k8s pod)
    ↓ Promtail scrape
    ↓ pipeline_stages（cri / regex / labels）
    ↓ attach external_labels（device_id / ongrid_source）
    ↓ batch（1 MiB / 1s）
    ↓ Promtail client push
    ↓ POST {manager_public_url}/loki/api/v1/push
    ↓ nginx auth_request → manager edgeauth /internal/auth/dataplane-verify
    ↓ Basic auth 校验 + X-Edge-Id 注入
    ↓ nginx proxy_pass → http://loki:3100/loki/api/v1/push
    ↓ Loki distributor → ingester → chunk → object store
```

### 24.2 查询链（SPA Logs 页面）

```
SPA fetch /v1/logs/query_range?query=...&start=...&end=...
    ↓ manager auth middleware
    ↓ logs.Handler.queryRange（[logs/http.go#L64](file:///d:/claude/ongrid/internal/manager/server/logs/http.go#L64)）
    ↓ logquery.Client.QueryRange（[client.go#L124](file:///d:/claude/ongrid/internal/pkg/logquery/client.go#L124)）
    ↓ BaseURLResolver.ResolveBaseURL（动态）
    ↓ GET {loki_url}/loki/api/v1/query_range?query=...&start=...&end=...
    ↓ Header: X-Scope-OrgID: ongrid
    ↓ Loki querier → query-frontend（LogQL 解析 + 分片）
    ↓ 返回 {status:success, data:{resultType, result}}
    ↓ 透传 JSON 给 SPA
```

### 24.3 AI 工具链（query_logql）

```
用户："查最近 1 小时的错误日志"
    ↓ chatruntime intent 路由
    ↓ 识别 logIntent
    ↓ 选择 query_logql 工具
    ↓ LLM 生成 args（query='{job="api"} |= "error"', start=now-1h, end=now）
    ↓ executeQueryLogQL（[query_logql.go#L91](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/query_logql.go#L91)）
    ↓ parseLogQLTime 解析时间
    ↓ 30s 超时 context
    ↓ logquery.Client.QueryRange
    ↓ GET {loki_url}/loki/api/v1/query_range?query=...
    ↓ 返回 ResultJSON 给 LLM
    ↓ LLM 推理 + 自然语言回复
```

### 24.4 告警评估链（log_match）

```
alert evaluator tick（每 5m）
    ↓ evaluateLogMatch（[evaluators_phaseB.go#L31](file:///d:/claude/ongrid/internal/manager/biz/alert/evaluators_phaseB.go#L31)）
    ↓ buildLogMatchQuery（stream + filter + window）
    ↓ runLokiInstant（60s 窗口 + 30s step）
    ↓ logquery.Client.QueryRange
    ↓ GET {loki_url}/loki/api/v1/query_range?query=count_over_time(...)
    ↓ 解析 matrix，取每个 series 最新 sample
    ↓ compareFloat(value, operator, threshold)
    ↓ 违例：RecordFiring + notify
    ↓ sweepRecovery（上一 tick 违例但本 tick 消失的 series 自动 resolve）
```

### 24.5 健康检查链

```
SPA /api/v1/systemhealth
    ↓ systemhealth.Service.Check
    ↓ checkLoki
    ↓ LokiURLProbe.Probe（[probe.go#L38](file:///d:/claude/ongrid/internal/manager/biz/setting/probe.go#L38)）
    ↓ LokiResolver.URL（动态解析 system_settings）
    ↓ GET {loki_base}/ready（5s 超时）
    ↓ 200 = OK，非 200 = Failed，未配置 = Degraded
```

### 24.6 correlate_incident log panel 链

```
LLM 调用 correlate_incident(incident_id=123)
    ↓ executeCorrelateIncident（[correlate_incident.go#L159](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/correlate_incident.go#L159)）
    ↓ GetIncident → 获取 edge_id
    ↓ queryLogPanel（[correlate_incident.go#L434](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/correlate_incident.go#L434)）
    ↓ LogQL: {edge_id="123"} |~ "(?i)error|panic|oom|fatal|fail"
    ↓ logquery.Client.QueryRange（limit=50, backward）
    ↓ 解析 streams → logEntry
    ↓ 截断 200 字符 + 倒序排序
    ↓ bundle.LogPanel = entries
    ↓ marshalBundleWithCap（100KB 上限，超限裁剪 logs → 10）
    ↓ 返回 ResultJSON 给 LLM
```

---

## 25. 交叉引用

- [ongrid_configs.md](file:///d:/claude/ongrid/ongrid_configs.md)：完整配置说明（`ONGRID_LOGS_URL`）
- [ongrid_tempo.md](file:///d:/claude/ongrid/ongrid_tempo.md)：Tempo 集成（三信号对称设计）
- [ongrid_integration.md](file:///d:/claude/ongrid/ongrid_integration.md)：8 个外部系统集成（含 Loki）
- [ongrid_LLM.md](file:///d:/claude/ongrid/ongrid_LLM.md)：AIOps 编排（query_logql 工具上下文）
- [ongrid_api.md](file:///d:/claude/ongrid/ongrid_api.md)：26 个业务域 API（含 /v1/logs/*）
- [ongrid_frontier.md](file:///d:/claude/ongrid/ongrid_frontier.md)：Frontier 集成（edge agent 通信隧道）
- [ongrid_grafana.md](file:///d:/claude/ongrid/ongrid_grafana.md)：Grafana 集成（Loki datasource provisioning）
- [ongrid_architecture.md](file:///d:/claude/ongrid/ongrid_architecture.md)：架构总览

---

**文档版本**：v1.0
**生成时间**：2026-07-31
**覆盖源码版本**：v0.7.113
**Loki 版本**：3.4.0
