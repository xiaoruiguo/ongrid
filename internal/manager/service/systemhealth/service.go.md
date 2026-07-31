# `service.go` 技术实现文档

> 源文件：`internal/manager/service/systemhealth/service.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/service/systemhealth`

## 1. 概述

本文件是平台自检聚合服务，为 manager UI 的"系统健康"页生成跨组件健康报告。每个 check 在独立 timeout ctx 内运行，结果含 status / message / details / duration_ms。核心红线：(1) 所有外部依赖（DB / Prom / Grafana / Loki / Tempo / Qdrant / Frontier / alert / edges / LLM / embedding）通过窄接口注入，nil 降级为 `StatusDegraded` 而非失败；(2) probe timeout 默认 3s，pctx deadline exceeded 时强制覆盖 OK 为 Failed（防 fn 误报 OK）；(3) overall status 优先级 Failed > Degraded > Unknown > OK。

## 2. 包信息

- **包名**：`systemhealth`
- **所属模块**：`internal/manager/service/systemhealth`
- **依赖方向**：被 HTTP handler 调用；依赖 `biz/edge`、`model/edge`、`service/alert`（Caller / Rule / IncidentFilter 类型）、`internal/pkg/llm`

## 3. 关键类型与接口

```go
type Status string
const (
    StatusOK       Status = "ok"
    StatusDegraded Status = "degraded"
    StatusFailed   Status = "failed"
    StatusUnknown  Status = "unknown"
)

type Check struct {
    ID, Group, Label string
    Status           Status
    Message          string
    Details          map[string]any
    DurationMS       int64
}

type Summary struct { OK, Degraded, Failed, Unknown int }

type Report struct {
    Status    Status
    CheckedAt time.Time
    Summary   Summary
    Checks    []Check
}

// 依赖接口（消费方定义）
type DBPinger interface { PingContext(ctx) error }
type PromQuerier interface { Query(ctx, expr string, ts time.Time) (any, error) }
type URLProbe interface { Probe(ctx) error }
type GrafanaTester interface { Test(ctx) error }
type RuleLister interface { ListRules(ctx, caller alertsvc.Caller, scopeType string) ([]*alertsvc.Rule, error) }
type IncidentCounter interface { CountIncidents(ctx, caller alertsvc.Caller, in alertsvc.IncidentFilter) (int64, error) }
type EdgeLister interface { List(ctx, f edgebiz.ListFilter) ([]*edgemodel.Edge, error) }
type LLMProviderResolver interface { ResolveProviders(ctx) ([]llm.ProviderConfig, string, error) }

type Dependencies struct {
    DB        DBPinger
    Prom      PromQuerier
    Grafana   GrafanaTester
    Loki      URLProbe
    Tempo     URLProbe
    Rules     RuleLister
    Incidents IncidentCounter
    Edges     EdgeLister
    LLM       LLMProviderResolver
    HTTP      *http.Client
}

type Config struct {
    Version             string
    ProbeTimeout        time.Duration
    PromEnabled         bool
    LogsEnabled         bool
    TracesEnabled       bool
    AlertEnabled        bool
    EvaluatorInterval   time.Duration
    NotifyCooldown      time.Duration
    FrontierAddr        string
    FrontierDisabled    bool
    LLMConfigured       bool
    EmbeddingConfigured bool
    QdrantURL           string
    QdrantCollection    string
}

type Service struct { cfg Config; deps Dependencies }
```

## 4. 关键函数与流程

### `New(cfg, deps)`

- ProbeTimeout<=0 → 3s；QdrantCollection 空 → "ongrid_knowledge"；HTTP nil → `&http.Client{Timeout: ProbeTimeout}`。

### `Check(ctx, caller) (*Report, error)`

- **职责**：运行 12 个 check 并汇总。
- **流程**：构造 checks 切片（顺序：manager / database / prometheus / grafana / loki / tempo / qdrant / frontier / alerts / edges / llm / embedding）；`summarize` + `overall`；返回 Report（CheckedAt=now UTC）。
- **错误处理**：Check 永不返回 error —— 单个 check 失败封装为 StatusFailed。

### 各 check 函数

均委托 `s.probe(ctx, id, group, label, fn)`：

- **`checkManager`**：OK + details{version}（health endpoint 已达即 OK）。
- **`checkDatabase`**：DB nil → Failed "not wired"；PingContext err → Failed；否则 OK。
- **`checkPrometheus`**：!PromEnabled || Prom nil → Degraded；Query("up", now) err → Failed；否则 OK。
- **`checkGrafana`**：Grafana nil → Degraded；Test err：若 `isGrafanaConfigMissing`（root_url empty / sa_token empty）→ Degraded（配置缺失非故障）；否则 Failed。
- **`checkLoki`**：!LogsEnabled || Loki nil → Degraded；Probe err → Failed。
- **`checkTempo`**：!TracesEnabled || Tempo nil → Degraded；Probe err → Failed。
- **`checkQdrant`**：QdrantURL 空 → Degraded；GET `/collections/<collection>`；2xx → OK；否则 Failed + body 前 256 字节。
- **`checkFrontier`**：FrontierDisabled → Degraded + details{addr}；否则 OK（edge online 状态另查）。
- **`checkAlerts`**：!AlertEnabled → Degraded；Rules/Incidents nil → Degraded "not fully wired"；ListRules → count enabled；CountIncidents(status=open)；details{rules, enabled_rules, open_incidents, evaluator_interval_seconds, notify_cooldown_seconds}；len(rules)==0 → Degraded "no rules"；open>0 → Degraded "N open incident(s)"；否则 OK。
- **`checkEdges`**：Edges nil → Degraded；List(Limit:1000)；统计 online/offline；len==0 → OK "no edge registered"；online==0 → Failed "all offline"；offline>0 → Degraded "N offline"；否则 OK。
- **`checkLLM`**：LLM 非 nil → ResolveProviders；err → Failed；len(providers)==0 → Degraded；否则 OK + details{providers, default_provider}。LLM nil 但 !LLMConfigured → Degraded；LLMConfigured → OK。
- **`checkEmbedding`**：!EmbeddingConfigured → Degraded；否则 OK。

### `probe(ctx, id, group, label, fn) Check`

- **流程**：start := now；`pctx, cancel := context.WithTimeout(ctx, ProbeTimeout)`；defer cancel；`fn(pctx)` 返回 (status, message, details)；**关键**：若 `pctx.Err()==DeadlineExceeded && status==OK` → 强制覆盖为 Failed + "probe timed out"（防 fn 误报 OK）；返回 Check{DurationMS: time.Since(start).Milliseconds()}。

### `summarize(checks) Summary` / `overall(s) Status`

- summarize：按 status 计数。
- overall：Failed>0 → Failed；Degraded>0 → Degraded；Unknown>0 → Unknown；否则 OK。

### `isGrafanaConfigMissing(err)`

- nil → false；msg 含 "root_url is empty" 或 "sa_token / api_key empty" → true（配置缺失非故障，降级而非失败）。

## 5. 依赖关系

- **内部包**：`biz/edge`、`model/edge`、`service/alert`（Caller / Rule / IncidentFilter 类型引用）、`internal/pkg/llm`
- **外部库**：`net/http`、`net/url`、`io`、`time`、`strings`、`fmt`、`context`
- **被调用方**：HTTP handler（/v1/system/health 等）

## 6. 并发与资源管理

- **无共享可变状态**：Service 字段在 New 后只读。
- **每个 check 独立 timeout ctx**：pctx 不影响其他 check；defer cancel 防泄漏。
- **HTTP client 共享**：deps.HTTP 供 checkQdrant 使用；Timeout=ProbeTimeout。
- **无并发执行**：checks 顺序执行（非 parallel）；总耗时 ≈ sum(各 check)；ProbeTimeout 3s × 12 = 最坏 36s。

## 7. 设计模式与亮点

- **窄接口注入**：每个外部依赖一个独立小接口（DBPinger / PromQuerier / URLProbe / GrafanaTester / RuleLister / IncidentCounter / EdgeLister / LLMProviderResolver）；nil 降级为 Degraded 而非 Failed。
- **timeout 强制覆盖 OK**：防 fn 在 deadline 后误报 OK；注释明示这是防 fn bug 的兜底。
- **Grafana 配置缺失区分**：`isGrafanaConfigMissing` 把"未配置"降级为 Degraded，避免全新部署 Grafana 显示 Failed。
- **overall 优先级**：Failed > Degraded > Unknown > OK —— 一个组件 Failed 整体即 Failed。
- **details 丰富**：alerts check 含 rules/enabled_rules/open_incidents/interval/cooldown；edges check 含 sampled/online/offline/limit；UI 可渲染详情。
- **Qdrant body 截断 256 字符**：防错误响应过大污染 UI。
- **Frontier check 仅查 enabled 状态**：注释明示"edge online state is checked separately" —— Frontier 自身可达性由 edge check 间接反映。

## 8. 注意事项

- **checks 顺序执行**：12 个 check 串行；最坏 36s；UI 应设置足够 timeout。未并行化是为简化错误归因（避免并发日志交错）。
- **ProbeTimeout 默认 3s**：caller 可覆盖；Qdrant 在远端网络下可能需更长。
- **edges check Limit:1000**：超过 1000 个 edge 的部署只采样前 1000；details.limit 字段提示 UI。
- **checkManager 永远 OK**：health endpoint 已达即说明 manager API 通；不实际再探活。
- **checkFrontier 不探活**：FrontierDisabled=true → Degraded；否则 OK（不实际 dial frontier，依赖 edge online 间接反映）。
- **checkLLM 的 LLM nil fallback**：LLM 接口未注入时用 cfg.LLMConfigured 布尔判断；兼容未注入 LLMProviderResolver 的部署。
- **QdrantCollection 默认 "ongrid_knowledge"**：与 vector store 默认集合名一致。
- **caller 仅用于 alerts check**：ListRules / CountIncidents 需要 caller；其他 check 不需要权限。
- **HTTP client Timeout=ProbeTimeout**：Qdrant 探测受 ProbeTimeout 约束；若 Qdrant 在远端，需调大 ProbeTimeout。
