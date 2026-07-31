# ongrid 系统技术架构文档

> 版本：基于源码 2026-07-31 快照
> 模块：`github.com/ongridio/ongrid`
> Go 版本：1.25.0

---

## 目录

1. [系统概述](#1-系统概述)
2. [整体架构](#2-整体架构)
3. [分层架构与依赖约束](#3-分层架构与依赖约束)
4. [核心子系统](#4-核心子系统)
5. [Edge Agent 架构](#5-edge-agent-架构)
6. [IAM 认证授权架构](#6-iam-认证授权架构)
7. [公共包（internal/pkg）](#7-公共包internalpkg)
8. [可观测性栈](#8-可观测性栈)
9. [部署架构](#9-部署架构)
10. [前端架构](#10-前端架构)
11. [关键架构决策与红线](#11-关键架构决策与红线)

---

## 1. 系统概述

### 1.1 定位

ongrid 是一个云边协同的 AIOps 运维平台，核心能力包括：

- **AIOps 助手**：基于 LLM + ReAct graph + 工具调用的智能运维助手，支持自然语言查询指标 / 日志 / 追踪 / 拓扑 / 告警，并能执行运维动作（重启服务、K8s 操作、bash 执行）
- **告警全链路**：基于 PromQL / LogQL / TraceQL 的多源告警，四级通知闸门（silence / cooldown / dampening / inhibition），自动根因分析
- **Edge Agent**：部署在被管主机 / K8s 集群的边端代理，负责数据采集（metrics / logs / traces）、命令执行、inventory 上报、升级
- **工作流编排**：DAG 流程引擎，支持确定性骨架 + LLM 节点混合编排
- **IM 桥接**：飞书 / 钉钉 / Telegram / Slack 多平台集成，渐进式消息编辑
- **知识库 RAG**：基于本地 BGE 嵌入 + Qdrant 向量库的检索增强

### 1.2 技术栈概览

| 维度 | 选型 |
|------|------|
| 语言 | Go 1.25（后端）+ TypeScript（前端） |
| Web 框架 | go-chi/chi v5 |
| ORM | GORM（MySQL 生产 / SQLite dev） |
| 鉴权 | JWT（HMAC-SHA256）+ Casbin RBAC |
| LLM | OpenAI 兼容（多 provider 路由）+ eino graph |
| Tunnel | singchia/geminio + singchia/frontier |
| 可观测性 | OpenTelemetry + Prometheus + Loki + Tempo + Grafana |
| 向量库 | Qdrant + fastembed-go（本地 ONNX 嵌入） |
| 前端 | React 18 + Vite + TailwindCSS + Zustand |
| 容器 | 多阶段构建 + distroless / debian-slim nonroot |

---

## 2. 整体架构

### 2.1 顶层目录结构

```
ongrid/
├── cmd/                      # 命令入口
│   ├── ongrid/               # 云端 manager 主程序（约 5000 行装配）
│   └── ongrid-edge/          # 边端 agent 主程序 + K8s 子命令
├── api/                      # protobuf 定义（iam / manager / tunnel）
├── internal/                 # 业务实现（monorepo）
│   ├── edgeagent/            # 边端 agent
│   ├── iam/                  # 身份与访问管理
│   ├── manager/              # 云端 manager 业务核心
│   │   ├── biz/              # 业务用例层
│   │   ├── data/             # 数据落地层
│   │   ├── model/            # 数据模型层
│   │   ├── server/           # HTTP handler 层
│   │   └── service/          # 应用服务层
│   ├── pkg/                  # 共享工具包（27 个子包）
│   └── skill/                # 内置技能 + 技能框架
├── deploy/                   # 部署文件（docker-compose / Dockerfile / K8s chart）
├── web/                      # 前端 SPA
├── skills/                   # 技能 SKILL.md 定义
├── agents/                   # Agent 定义
├── docs/                     # 文档
└── tests/e2e/                # E2E 测试
```

### 2.2 云边拓扑

```
┌──────────────────────── Cloud（管理中心）────────────────────────┐
│                                                                  │
│  ┌─────────┐    ┌─────────────┐    ┌──────────────────────────┐ │
│  │ Web SPA │───▶│  nginx (443)│───▶│  manager (8080)          │ │
│  │ (React) │    │  /api 反代  │    │  - HTTP API              │ │
│  └─────────┘    │  /grafana   │    │  - AIOps chat            │ │
│                 │  /prometheus│    │  - Alert pipeline        │ │
│                 └──────┬──────┘    │  - Flow engine           │ │
│                        │           │  - IM bridge             │ │
│       ┌────────────────┼───────────┤  - Report generator      │ │
│       ▼                ▼           └──────────┬───────────────┘ │
│  ┌─────────┐    ┌─────────────┐    ┌──────────▼───────────────┐ │
│  │ Grafana │    │ Prometheus  │    │  frontier broker         │ │
│  │ (3000)  │    │ (9090)      │    │  (40011 service-bound)   │ │
│  └─────────┘    └─────────────┘    │  (40012 edge-bound)      │ │
│  ┌─────────┐    ┌─────────────┐    └──────────┬───────────────┘ │
│  │  Loki   │    │   Tempo     │               │                 │
│  │ (3100)  │    │  (4317/4318)│               │                 │
│  └─────────┘    └─────────────┘               │                 │
│  ┌─────────┐    ┌─────────────┐               │                 │
│  │ Qdrant  │    │   MySQL     │               │                 │
│  │ (6333)  │    │   (3306)    │               │                 │
│  └─────────┘    └─────────────┘               │                 │
└────────────────────────────────────────────────┼─────────────────┘
                                                 │ Tunnel (geminio)
                                                 │ 反向拨号
                    ┌────────────────────────────┼────────────────┐
                    │ Edge Sites                  │                │
                    │                             ▼                │
                    │  ┌──────────────────────────────────────┐   │
                    │  │ ongrid-edge agent                    │   │
                    │  │  - biz.Run (heartbeat/metrics/upgrade)│   │
                    │  │  - collector (composite/scrape)      │   │
                    │  │  - plugins (metrics/logs/traces/...) │   │
                    │  │  - k8s (inventory/metrics/actions)   │   │
                    │  │  - bash / webshell / host_files      │   │
                    │  │  - skill dispatcher                  │   │
                    │  └──────────────────────────────────────┘   │
                    │  主机 / K8s 集群                              │
                    └─────────────────────────────────────────────┘
```

### 2.3 数据平面与控制平面分离（ADR-014）

关键设计：edge 采集的 telemetry 数据**不经 manager Go 字节流**，而是经 nginx `auth_request` 鉴权后直达 Loki / Tempo / Prometheus：

```
edge agent (promtail/otelcol/node_exporter)
   │  basic auth (edge credential)
   ▼
nginx (443) ── auth_request ──► manager (/internal/auth/dataplane-verify)
   │
   ├── /loki/api/v1/push      ──► Loki
   ├── /v1/traces             ──► Tempo
   └── /prometheus/api/v1/write ──► Prometheus
```

manager 仅做认证决策，不在数据通路上，避免成为吞吐瓶颈。

---

## 3. 分层架构与依赖约束

### 3.1 gospec 分层约束

严格遵循 `cmd → web → controlplane → repo → model` 单向依赖：

```
cmd/ongrid (装配层)
    │
    ▼
internal/manager/server   (HTTP handler，chi router)
    │
    ▼
internal/manager/service  (应用服务，DTO 翻译)
    │
    ▼
internal/manager/biz      (业务用例，核心逻辑)
    │
    ▼
internal/manager/data     (GORM 实现)
    │
    ▼
internal/manager/model    (纯 GORM struct)
```

**红线**：
- 禁止跨层调用（如 server 直接 import data）
- `internal/<domain>` 之间禁止直接 import，必须通过 API / 事件 / `internal/shared/`
- 接口在消费方定义，禁止循环依赖
- `utils/`、`lerrors/` 不依赖任何业务包
- 依赖通过构造函数注入，不使用全局变量

### 3.2 manager 五层分包

以 `alert` 域为例：

| 层 | 包路径 | 职责 |
|----|--------|------|
| model | `internal/manager/model/alert` | `Incident` / `Rule` / `Channel` / `Event` 等 GORM struct + 常量 |
| data | `internal/manager/data/alert/store` | `Repo`（GORM 实现）+ `migrate.go` + `seed.go` + `provider.go` |
| biz | `internal/manager/biz/alert` | `Usecase`（incident 状态机）+ `PipelineEvaluator` + `Router` + `Investigator` |
| service | `internal/manager/service/alert` | `Service`（HTTP DTO ↔ biz input 翻译）|
| server | `internal/manager/server/alert` | `http.go`（chi handler + 路由注册）|

### 3.3 关键设计模式

- **接口在消费方定义**：`Repo` / `Writer` / `NodeMirror` 等窄接口在 biz 层定义，data 层实现
- **Narrow interface + 可选注入**：`Set*` / `With*` 注入可选能力，nil 时走 graceful degradation
- **post-construction setter**：解决循环依赖 + 避免 constructor signature 膨胀
- **双路径并存**：双 kernel（legacy/graph）、双工具路径（闭包/BaseTool）——灰度切换安全
- **存在性隐藏**：非所有者拿 `ErrNotFound` 而非 `ErrForbidden`，防 IDOR 枚举

---

## 4. 核心子系统

### 4.1 AIOps 子系统

AIOps 是 ongrid 最复杂的子系统，承载 LLM 驱动的运维助手。

#### 4.1.1 双 Kernel 派发

```
service/aiops/service.go
    │
    ├── KernelLegacy（默认）── agent.Agent 660 行 for-loop
    │
    └── KernelGraph（ONGRID_AGENT_KERNEL=graph）
            └── chatruntime.Runtime.Handle（1778 行，10 步主流程）
                    └── graph.BuildReActGraph（eino ReAct 图）
```

- `ONGRID_AGENT_KERNEL` env 切换，灰度安全
- graph 失败自动回退 legacy
- `runWithKernel` 单一咽喉：所有权校验 → `context.WithoutCancel(ctx)` 解耦 HTTP ctx

#### 4.1.2 HLD-021 turn 生命周期解耦

`context.WithoutCancel(ctx)` 把 turn 从 HTTP 请求生命周期剥离：
- 浏览器刷新 / SSE 断开不会杀死进行中的 turn
- cloud_bash 等待人工审批时尤为关键
- 显式 Esc 通过 `StopSession` 调 `cancels[sessionID]` cancel

#### 4.1.3 chatruntime.Runtime 10 步主流程

1. ownership check（非 owner 返 `ErrNotFound` 防指纹探测）
2. mention inline（`MentionResolver.Resolve` → bullets）
3. persist user msg（先落盘再调 graph）
4. load history
5. `tryApplyConfirmedConfigDraft` 快捷路径
6. resolve skills + system prompt（persona 解析、viewer 降级、coordinator stub overlay、意图过滤）
7. `buildEinoHistory`（toolreplay 集成，tool response hoist 防 DeepSeek 400）
8. build graph（`graph.BuildReActGraph`）
9. wire callbacks（SSE 适配）
10. invoke（`calcDynamicHints` 注入 `<system-reminder>` + `g.Invoke`）

#### 4.1.4 工具注册表

`biz/aiops/tools/registry.go` 注册 30+ 工具：

| 类别 | 工具 |
|------|------|
| 主机查询 | `get_host_load` / `get_process_list` / `host_files` |
| 指标查询 | `query_promql` / `metric_catalog` |
| 日志查询 | `query_logql` / `query_k8s_logs` |
| 追踪查询 | `query_traceql` |
| 拓扑 | `get_topology` / `expand_topology` / `find_topology_node` |
| 设备 | `query_edges` / `get_edge_summary` / `rank_edges` / `find_outlier_edges` |
| 告警 | `query_incidents` / `get_incident_detail` / `query_alert_rules` / `correlate_incident` |
| K8s | `query_k8s_snapshot` / `describe_k8s_resource` / `execute_k8s_action` |
| 执行 | `bash` / `cloud_bash` / `restart_service` / `install_skill` |
| 知识 | `query_knowledge` |
| 通信 | `send_im_message` / `serve_page` / `send_message` |
| 元工具 | `tool_search` / `task_stop` / `agent_tool` |

**nil-gating 注册**：每个工具注册前检查依赖，nil 时不注册——graceful degradation。

#### 4.1.5 LLM 客户端层

`internal/pkg/llm` 提供多 provider 路由：

```
MultiClient（router.go）
    ├── openaiClient（OpenAI）
    ├── openaiClient（Anthropic，OpenAI-compatible 端点）
    ├── openaiClient（Zhipu，自动 JWT transport）
    ├── openaiClient（DeepSeek）
    ├── openaiClient（Kimi）
    └── openaiClient（Gemini）
```

关键特性：
- per-day token 预算门控（`BudgetChecker`）
- Prometheus 监控（label 严格限制：model/kind/result，禁 user_id/org_id）
- 动态 Resolver（admin 设置 60s TTL 热更新）
- reasoning model 采样参数响应式自愈
- 永不记录用户 content

### 4.2 Alert 子系统

#### 4.2.1 Pipeline 评估

`biz/alert/pipeline.go` tick 驱动（默认 5min）：

1. `refreshDeviceStalenessGauge`（host offline 检测延迟 ≤ 60s）
2. `evaluatePromQuery`（metric_raw，Phase-3 collapse 后统一路径）
3. `evaluateMetricAnomaly` / `evaluateMetricForecast` / `evaluateMetricBurnRate`
4. `evaluateTraceLatency` / `evaluateTraceErrorRate`（查 Prom spanmetrics）
5. `evaluateLogMatch` / `evaluateLogVolume`

关键设计：
- **expr 即谓词**：metric_raw 的 `Expr` 本身含 PromQL 比较运算符
- **per-label-set dedupe**：`pipeline:<rule_key>:<labelSetKey>`
- **Recovery sweep**：上 tick fired 但本 tick 缺失 → 自动 resolve

#### 4.2.2 Incident 生命周期

`biz/alert/usecase.go` `RecordFiring` 流程：

1. `validateFiring` 校验
2. 查 existing incident
3. existing == nil → `CreateIncident`（race recovery：unique 冲突 → re-fetch + bump）
4. existing != nil → `ReopenIncident` 或 `BumpIncidentFiring`（原子自增 event_count）
5. 写 firing 事件
6. `matchSilence`
7. isNew && investigator != nil → `InvestigateAsync`（仅新建，避免 reopen flap 烧 LLM）
8. (isNew || isReopen) && workflowDispatcher != nil → `OnAlertFired`

#### 4.2.3 四级通知闸门

顺序：**silence → cooldown → dampening → inhibition → 投递**

- silence：最高优先级
- cooldown：`LastNotifiedAt` 间隔检查（默认 10min）
- dampening：`NotifyWindowSeconds` + `NotifyMinFires` 窗口内 ≥N 次才通知
- inhibition：`Inhibitor.Suppress` 命中写 `EventTypeInhibited`
- 投递：per-channel 串行，`BuildSenderFromChannel` 构造 typed sender（Slack/Feishu/DingTalk/WeCom/Telegram/Webhook）

### 4.3 Flow 子系统

`biz/flow/engine.go` DAG 执行器，**确定性骨架 + 概率节点内部**：

- 从 trigger 节点启动
- **fan-out 并发上限 4**（buffered chan 信号量，防 wide graph 无限 spawn）
- **OR-join + execute-once**：节点首次被任一入边激活时运行
- 节点 error 走 "error" port（已连则分支处理，未连则 run 失败）
- 双层 panic recover（node panic 不 crash manager）

### 4.4 Edge 设备管理

#### 4.4.1 frontierbound.Client

封装 `singchia/frontier` SDK，维持 geminio 长连接：

- **transport ID ↔ canonical edgeID 双向映射**（四个 map + RWMutex）
- **canonicalizeEdgeID 在 binding 未建立时返回 0**，防止 raw transport ID 泄露为 Prom label（v0.7.39 fix）
- **unbindEdgeTransport 三重校验**防止 frontier 投递旧连接 offline 事件误删新 binding

#### 4.4.2 edge/device 拆分（post-split）

- `Edge`：仅 tunnel 身份（access_key / secret_key / tunnel session）
- `Device`：host 事实（硬件指纹 / host facts / roles）
- `EdgeDevice`：junction 表
- **`resolveDeviceID` 永不回退到 edge_id**——二者是独立 auto-increment 序列，回退会污染不可变 Prom TSDB

#### 4.4.3 设备指纹 v3

`HardwareFingerprint`（MAC|CPU|disk hash）优先于 legacy HostID：
- hypervisor 重生成 NIC MAC 让克隆 VM 保持独立
- v3 迁移 in-place 保留 device.ID 和历史
- hash+prefix 统一列形状：`"fp_" + hex(sha256(seed)[:16])`

### 4.5 IM Bridge

多 IM 平台桥接（飞书 / 钉钉 / Telegram / Slack）：

- **session 规则**：一个 IM chat 一个 ongrid session；`/new` 显式重置
- **mark-on-entry dedup**：容量 2048 两代 map，key = `provider:appID:eventID`
- **渐进编辑**：Feishu/Telegram/Slack 实现 `MessageEditor`；DingTalk 一次性发送
- **locale 处理**：`localeDirective` 附加到 user content 末尾
- **并发要求**：webhook handler 必须 `go b.HandleInbound(...)` 后立即返回 200

### 4.6 Report 子系统

- **Generator seam**：PR-1 nop → PR-2 workerGenerator 渐进
- **Dedup-protected**：`(schedule_id, period_start)` unique key，duplicate 仍 re-arm schedule
- **异步 Generate**：`context.WithoutCancel(ctx)` 让请求取消后生成仍继续
- **defaultLocale 双轨**：scheduled 用 `ONGRID_DEFAULT_LOCALE`，manual 用 Accept-Language

---

## 5. Edge Agent 架构

### 5.1 目录结构

```
internal/edgeagent/
├── biz/                # Agent 主循环 + 升级流程
│   └── collector/      # 内嵌 CPU/Mem/Net 采集器
├── service/            # 轻量 handler 注册助手
├── collector/          # 数据采集层
│   ├── composite.go    # 组合 collector（embedded + scrape）
│   ├── scrape.go       # 多目标 HTTP scraper
│   ├── mapper.go       # counter delta 缓存
│   └── embedded.go     # gopsutil 内嵌采集
├── plugins/            # 插件框架
│   ├── supervisor.go   # reconcile loop 管理
│   ├── subprocess.go   # 子进程插件底座
│   ├── metrics/        # in-process 指标采集
│   ├── logs/           # promtail 子进程
│   ├── traces/         # otelcol-contrib 子进程
│   ├── hostmetrics/    # 主机指标
│   ├── procmetrics/    # 进程指标
│   ├── custommetrics/  # 自定义多目标
│   └── databasemetrics/# 数据库指标
├── k8s/                # K8s 适配层
│   ├── inventory.go    # full sync + watch delta
│   ├── metrics.go      # kube-state-metrics + OTLP gateway + app pod
│   ├── actions.go      # 7 种 K8s action
│   └── upgrade_prepare.go
├── bash/               # MethodBashExec handler
├── webshell/           # TCP 流转发器
├── host_files/         # find_large_files / du_summary / stat_file
├── skill/              # execute_skill dispatcher
├── cmdpolicy/          # 命令策略层 + Sandbox
├── restart_service/    # restart_service handler
└── model/              # edgeagent 域模型
```

### 5.2 Agent 主循环（biz/agent.go）

`Agent.Run(ctx)` 流程：

1. `registerHandlers` 注册 RPC handler
2. `OnReconnect` 回调（重连后重新 register_edge）
3. `Dial` 建立到 manager 的长连接
4. `registerEdge`（`registerMu` 串行化；注入 k8s-node 指纹）
5. errgroup 起三个 goroutine：
   - `heartbeatLoop`（30s 周期；连续 5 次失败触发 `errTunnelStuck`）
   - `metricsLoop`（10s 周期；`collector.CollectAll` → `pushOne`）
   - `upgradeRequested` 监视（收到升级信号后 cancel rootCtx）

**升级流程**：校验 SHA256 → 45 分钟下载 → 流式 hash → 写 `pending.sha256` → `os.Rename` → 非阻塞信号 → errgroup 取消 → systemd 视为干净退出 → ExecStartPre 回滚

### 5.3 数据采集架构

#### CompositeCollector（auto 模式）

回退优先级链：
1. host-role scrape 目标成功 → 取代 embedded host 快路径
2. 否则回退到 embedded 基线（gopsutil）
3. component-role scrape 只贡献 Prom samples

#### Scraper

多目标 HTTP scraper，每目标一个 goroutine：
- `GET /metrics` + `expfmt.TextParser` 解析
- `BearerTokenFile` 每次读（支持轮换）
- per-target `Mapper`（counter delta 缓存独立）
- host-role 额外 `MapToHostPoint`

### 5.4 插件系统

#### Plugin 接口

```go
type Plugin interface {
    Name() string
    Configure(cfg PluginConfig) error
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
    HealthSnapshot() PluginHealth
}
```

#### Supervisor（reconcile loop）

类 K8s controller 模式：
- `reloadSignal`（容量 1，coalesce）触发 reconcile
- 三份 map：`plugins`（注册）/ `current`（当前配置）/ `running`（运行时状态）
- 单插件失败不影响其他
- `configEqual` 比较 Enabled/EdgeID/Endpoint/AuthUser/AuthPass + Spec

#### 插件分类

| 插件 | 类型 | 职责 |
|------|------|------|
| metrics | in-process | 抓取本地 `/metrics` → tunnel push |
| logs | subprocess | promtail 日志采集 |
| traces | subprocess | otelcol-contrib 追踪采集 |
| hostmetrics | in-process | 主机指标 |
| procmetrics | in-process | 进程指标 |
| custommetrics | in-process | 自定义多目标 |
| databasemetrics | in-process | 数据库指标 |

### 5.5 K8s 集成

#### InventoryPusher

- 仅 controller role 运行
- 首次 full sync → `sync.Once` 启动 watch goroutine
- watch 事件经 2s 防抖 → `pushDelta`
- 错误处理：forbidden/notfound 永久禁用；resourceExpired 清 RV 发 fullResync；其他指数退避
- list 阶段就调 `k8sredact` 对 labels/annotations/events 脱敏

#### MetricsPusher

三个来源：
1. kube-state-metrics
2. OTLP gateway
3. 注解发现的应用 Pod（`prometheus.io/scrape=true`）

所有 target 配置 `LabelDrop` 移除高基数 label（uid/pod_uid/container_id）。

#### Actions（7 种）

`rollout_restart` / `scale` / `delete_pod` / `evict_pod` / `cordon` / `uncordon` / `drain`

- `ExpectedResourceVersion` 前置条件防冲突
- `drainNode` 类 kubectl drain：`drainDecision` 纯函数决策
- `withDryRun` URL 追加 `dryRun=All`

### 5.6 命令执行架构

#### cmdpolicy 策略层

v1 只读策略基线 + YAML 覆盖：

| 类别 | 示例 |
|------|------|
| ClassReadFS (16) | cat/head/tail/ls/find/du/stat/grep/awk(拒 system)/sed(拒 -i) |
| ClassReadSystem (17) | ps/top/uptime/free/df/iostat/ss/dmesg/journalctl(拒 --rotate) |
| ClassMixed (7) | iptables/systemctl/ip/mount（ReadOnlyMatchers / WriteMatchers） |
| ClassNetwork (7) | nc(仅 -z)/curl(仅 --head)/wget(仅 --spider)/dig/ping/traceroute |
| ClassDenied (~25) | bash/sh/rm/mv/chmod/shutdown/passwd/iptables-restore |

`Policy.Decide(cmd)`：`SplitPipes` 切管道分段 → 每段 `decideSegment` → 任一段拒绝立即返回。

#### bash handler

`req.Unrestricted` 分流：
- true（admin write gate）→ `sandbox.ExecRaw`，WARN 记录审计
- false → `sandbox.Exec`

#### webshell handler

**极简 TCP 转发器**——SSH 完全在 manager 侧，edge 只做 TCP 字节转发：
- `allowedTargets` 硬编码 `127.0.0.1:22` / `localhost:22`（防 pivot）
- 两个 goroutine `io.Copy` 双向转发
- 首个 error 关闭两端

### 5.7 Skill 系统

#### Registry（进程级单例）

- `globalRegistry` 包级单例
- `Register` nil → panic；重复 Key panic（作者期错误应炸响）
- `All()` 读锁 + 拷贝 + 字典序排序
- `AllByClass(classes...)` 按 `EffectiveClass()` 过滤

#### Loader（外部 skill pack 加载）

- `SkillManifest`：Name / Description / Schema / Entry / EnvAllow / Timeout / Class
- Allowlist + `EvalSymlinks` 双层防逃逸
- 强制 `Scope=ScopeManager`（外部二进制不能跑 edge）
- fail-soft 装载；defer/recover 防御 Register panic

#### SubprocessSkill

- deny-by-default env（连 `PATH` 都需显式 opt-in）
- `MaxSubprocessStdout=16MiB` / `MaxSubprocessStderrTail=4KiB`
- 平台特定进程组杀死（unix SIGKILL 进程组）
- `cmd.WaitDelay=1s` cancel 后 1s 强收尸

#### 内置 skill

`blank import` 触发 `init()` 自注册：
`restart_service` / `host_netns_inspect` / `probe_dns` / `probe_http` / `probe_tcp` / `read_journal` / `tail_file` / `web_search`

---

## 6. IAM 认证授权架构

### 6.1 分层结构

```
internal/iam/
├── biz/
│   ├── authz/         # casbin 授权
│   ├── membership/    # 成员关系
│   ├── org/           # 组织
│   └── user/          # 用户（hash.go / repo.go / usecase.go）
├── data/
│   ├── membership/store/
│   ├── org/store/
│   └── user/sqlite/
├── model/model.go     # User / Org / OrgMembership
├── server/
│   ├── http.go        # chi 路由 + 登录限流 + JWT
│   └── orgs.go        # org CRUD + 成员管理 + /v1/me
└── service/service.go # 聚合 4 个 biz 子服务
```

### 6.2 认证机制

- **密码存储**：argon2id（`internal/pkg/passwd`）
- **JWT**：HMAC-SHA256，access / refresh / 自定义 TTL 三套签发
- **Verify 不查 DB**：仅签名 + 过期校验（信任边界）
- **keyFunc 强制 `SigningMethodHMAC`**：防 `alg=none` 攻击
- **登录限流**：双键滑动窗口（IP 20次/5min，email 6次/15min）

### 6.3 授权机制（Casbin）

- `SyncedEnforcer`（线程安全）
- **双写一致性**：`OrgMembership`（HR 真值表）与 `casbin_rule`（策略表）每次 mutation 经 `SyncMembership` / `RevokeMembership` 双写
- 三角色 + superuser 兜底：`org_admin` / `member` / `viewer` / `superuser`
- `Allow` 出错返回 false（fail-closed）
- superuser 短路 casbin 检查，但保留兜底 `p` 行 defense in depth

### 6.4 角色体系

| 维度 | 角色 | 存储 |
|------|------|------|
| 系统级 | admin / user / viewer | `User.Role` 列 |
| 成员级 | org_admin / member / viewer | `OrgMembership.Role` 列 |
| 超级管理员 | superuser | `User.IsSuperuser` 列（独立于 membership）|

May 2026 pivot：admin ⇔ superuser 由 `SetRole` / `Create` 双写保持同步。

### 6.5 Phase-1 灰度装配

```go
svc := iamservice.New(userUC, log)  // 仅注入必填 user
svc.SetOrgs(orgService)              // 后置注入，nil 时 503
svc.SetMemberships(membershipSvc)    // 后置注入
svc.SetAuthz(enforcer)               // 后置注入
```

---

## 7. 公共包（internal/pkg）

共 27 个公共包，按用途分类：

### 7.1 LLM / AI

| 包 | 职责 |
|----|------|
| `llm` | OpenAI 兼容客户端 + 多 provider 路由 + 预算门控 + 智谱 JWT transport + reasoning model 自愈 |
| `mcpclient` | MCP（Model Context Protocol）Streamable HTTP 客户端 |
| `embedding` | 嵌入抽象 + OpenAI 兼容 + 本地 ONNX（fastembed-go）|
| `qdrantx` | Qdrant 向量库 REST 封装 |
| `docextract` | 文档抽取（知识库 chunk 化前置）|

### 7.2 Tunnel / 运行时

| 包 | 职责 |
|----|------|
| `tunnel` | edge ↔ manager 双向 RPC（geminio）；代际连接管理解决竞态 |
| `runner` | 子进程 skill 执行器 |
| `workspace` | per-session cwd（HLD-019 agent 工作区持久化）|

### 7.3 认证授权 / 多租户 / 加密

| 包 | 职责 |
|----|------|
| `auth` | JWT 签发/验证 + chi 中间件（双重 context 写入）|
| `authzmw` | 授权中间件 |
| `zhipuauth` | 智谱 JWT 签名 |
| `tenantctx` | context 承载调用方身份（可变 slot 模式）|
| `secretbox` | AES-256-GCM at-rest 加密（`ONGRID_SECRET_KEY`）|
| `passwd` | argon2id 密码哈希 |
| `k8sredact` | K8s secret 字段脱敏 |
| `credinject` | skill/MCP 声明式凭证注入 |

### 7.4 可观测性客户端

| 包 | 职责 |
|----|------|
| `prom` | 进程级 Prometheus registry（非全局避免测试污染）|
| `promquery` | Prom HTTP `/api/v1/query` 轻量客户端（raw JSON 透传 LLM）|
| `promwrite` | Prom remote_write 客户端（snappy+protobuf）|
| `promauth` | 带 auth 的 http.Client 构造 |
| `tracequery` | Tempo HTTP API 客户端（TraceQL + raw JSON）|
| `logquery` | Loki LogQL 查询客户端 |
| `grafana` | Grafana HTTP API 客户端 |
| `tracing` | OpenTelemetry 接入（OTLP HTTP exporter）|
| `logger` | slog JSON to stderr |

### 7.5 基础设施

| 包 | 职责 |
|----|------|
| `dbx` | GORM 入口（MySQL / SQLite dialect 切换）|
| `config` | env 驱动配置统一入口（零依赖策略，不引入 viper）|
| `httpserver` | net/http.Server 包装 + 优雅关停 |
| `notify` | 归一化 Message 路由到多 channel |
| `errs` | 错误工具 |

---

## 8. 可观测性栈

### 8.1 整体架构

```
edge agent (promtail/node_exporter/process_exporter/otelcol-contrib)
   │  basic auth (edge credential)
   ▼
nginx (443) ── auth_request ──► manager (/internal/auth/dataplane-verify)
   │
   ├── /loki/api/v1/push      ──► Loki (3100)
   ├── /v1/traces             ──► Tempo (4318 OTLP HTTP)
   └── /prometheus/api/v1/write ──► Prometheus (9090 remote_write receiver)

manager Go ── OTLP ──► Tempo (4317 gRPC，内部)
   │  prometheus scrape (ongrid:9100)
   ▼
Prometheus ── spanmetrics remote_write ──► 自身
   │
Grafana (3000) ◄── provisioning ──► 数据源 loki.yml / prometheus.yml / tempo.yml
```

### 8.2 日志（Loki）

- 后端：Loki 3.4.0，filesystem storage
- retention：720h（30d）
- cardinality 守门：`max_global_streams_per_user: 10000`
- 限流：nginx `limit_req_zone` 按 edge access key 隔离，2000r/m

### 8.3 指标（Prometheus）

- 后端：Prometheus v2.54.0，`--web.enable-remote-write-receiver`
- retention：90d / 20GB
- scrape：self-scrape + `ongrid-manager:9100`（15s interval）
- 查询代理：nginx `/prometheus/` → auth_request → `prometheus:9090`

### 8.4 追踪（Tempo）

- 后端：Tempo 2.10.0，local filesystem
- retention：168h（7d）
- receivers：OTLP gRPC 4317 / HTTP 4318（均不发布到 host）
- spanmetrics：remote_write 到 Prometheus，派生 `traces_spanmetrics_latency_bucket`
- manager 接入：OTLP HTTP exporter，2s batch timeout

### 8.5 面板（Grafana）

- 后端：Grafana OSS 11.1.4
- embedding：`GF_SECURITY_ALLOW_EMBEDDING: true`（iframe 嵌入 ongrid）
- provisioning：`deploy/grafana/provisioning/`（dashboards + datasources）
- bootstrap：manager 首启创建 Service Account + token 存 `system_settings.grafana.sa_token`

### 8.6 Prometheus label 红线

`internal/pkg/prom` 包注释强制：
- 禁 org_id / user_id / edge_id / URL full-path 作 label
- 仅允许 method / code / status / model / direction / result / plan_bucket

### 8.7 HTTP 中间件

#### audit.go（HLD-010）

- **只记录显式标注的用户动作**（handler 调 `SetAuditEvent`）
- **指针槽位跨 ctx 重包装**：ctx value 是 `*auditSlot` 而非 `Event` 值
- **403 独立 `StatusDenied`**
- **UserAgent 截断 512**
- **uc == nil graceful degradation**

#### metrics.go（ADR-026 self-obs）

- **route pattern 而非 path 作 label**：用 chi 编译期 route 模板
- **404 归桶 "unknown"**
- 4 个 label 维度：method + route + status + duration
- 单文件 33 行，无配置项

---

## 9. 部署架构

### 9.1 docker-compose（dev）

| 服务 | 镜像 | 端口 | 角色 |
|------|------|------|------|
| mysql | mysql:8.0 | 3306 | 主数据库 |
| ongrid | 本地构建 | 8080 / 9100 | manager |
| nginx | 本地构建 | 443 / 80 | TLS + SPA + /api 反代 |
| frontier | frontier:v1.2.4 | 40012 / 40011 | tunnel broker |
| prometheus | prometheus:v2.54.0 | 9090 | 云侧 Prom |
| loki | loki:3.4.0 | 3100 | 日志后端 |
| tempo | tempo:2.10.0 | 4317 / 4318 | 追踪后端 |
| qdrant | qdrant:v1.11.3 | 6333 | 向量库 |
| grafana | grafana-oss:11.1.4 | 3000 | 可视化 |
| searxng | searxng:latest | 8080 | web_search 后端 |
| ollama | ollama:latest | 11434 | 本地 LLM（可选）|

### 9.2 Dockerfile 构建策略

#### Dockerfile.ongrid（manager）

- 多阶段：`golang:1.25-bookworm` builder → `debian:bookworm-slim` runtime
- **CGO_ENABLED=1**（fastembed-go / onnxruntime 需要）
- ONNX Runtime v1.20.1 按 arch 下载
- **nonroot uid 65532**；`HOME=/var/lib/ongrid`
- EXPOSE 8080（API）+ 9100（metrics）

#### Dockerfile.ongrid-edge（edge agent）

- 6 阶段：builder + promtail + node_exporter + process_exporter + otelcol-contrib + distroless runtime
- **CGO_ENABLED=0**（纯 Go，无 cgo 依赖）
- **gcr.io/distroless/base-debian12:nonroot**
- 每个 plugin 独立阶段下载 + sha256sum 校验
- EXPOSE 9101（仅本地 /metrics）

#### Dockerfile.web（nginx + SPA）

- `node:20-alpine` builder → `nginx:1.27-alpine` runtime
- npm ci + vite build

### 9.3 K8s 部署（Helm chart）

`deploy/kubernetes/ongrid-edge/`——**仅 edge 侧 K8s 集成**：

- `daemonset.yaml`：edge agent DaemonSet（full-node 模式）
- `deployment.yaml`：controller Deployment（replicas: 1）
- `telemetry-gateway-deployment.yaml`：集群级 telemetry 网关
- `metrics-scraper-deployment.yaml`：metrics scraper
- `values.yaml`：mode full-node；node collectorMode off（避免 duplicate node_*）

### 9.4 镜像仓库

- Manager：`docker.cnb.cool/ongridio/ongrid:$(VERSION)`
- Web：`docker.cnb.cool/ongridio/ongrid/ongrid-web:$(VERSION)`
- Edge：`docker.cnb.cool/ongridio/ongrid-edge:$(VERSION)`
- Helm：`oci://helm.cnb.cool/ongridio/ongrid-edge`

### 9.5 Edge 插件版本

| 插件 | 版本 |
|------|------|
| promtail | 3.4.0 |
| otelcol-contrib | 0.118.0 |
| node_exporter | 1.8.2 |
| process-exporter | 0.8.4 |
| mysqld_exporter | 0.19.0 |
| postgres_exporter | 0.19.1 |
| redis_exporter | 1.86.0 |
| mongodb_exporter | 0.51.0 |

---

## 10. 前端架构

### 10.1 技术栈

| 维度 | 选型 |
|------|------|
| 框架 | React 18.3 + react-router-dom 6.27 |
| 构建 | Vite 5.4 + TypeScript 5.6 |
| 状态 | Zustand 5.0 |
| UI | TailwindCSS 3.4 + 自建 `components/ui/` |
| 图表 | Recharts 2.13 |
| 流程图 | @xyflow/react 12.10 + @dagrejs/dagre 3.0 |
| 终端 | xterm 5.3 + xterm-addon-fit + xterm-addon-web-links |
| Markdown | react-markdown 9 + remark-gfm 4 |
| 测试 | Vitest 1.6 + Playwright 1.61 + MSW 2.14 |

### 10.2 页面结构

`web/src/pages/`：Dashboard / Alerts / AlertRules / Agents / Approvals / ChatThread / DeviceShell / Edges / EdgeDetail / Flows / FlowEditor / Home / IncidentDetail / Knowledge / KnowledgeRepos / Kubernetes / Logs / Login / Mcp / Monitor / Pages / ReportDetail / Skills / SkillRun / Tasks / Topology / Traces + `settings/` 子目录

### 10.3 UI 设计约束

- **配色**：中性骨架用 zinc；主操作用 indigo；语义状态 emerald/amber/red/sky 走 Chip tone
- **克制**：满屏正常态用「小圆点 + 灰字」，只让异常跳出
- **light/dark**：light 靠 `html.light` 覆盖 zinc 类
- **i18n**：文案走 `tr('中文','English')` 跟随 locale
- **复用组件**：优先用 `web/src/components/ui/`（Card / Chip / Button / PageHeader / EmptyState）

---

## 11. 关键架构决策与红线

### 11.1 架构决策（ADR 摘要）

| ADR | 决策 |
|-----|------|
| ADR-007 | Tunnel 拓扑：edges dial 上游 frontier broker |
| ADR-008 | nginx TLS 终止 + auth_request 鉴权 |
| ADR-009 | 云侧 Prometheus（remote_write receiver）|
| ADR-012 | Loki 日志后端 |
| ADR-013 | Tempo 追踪后端 |
| ADR-014 | 数据平面与控制平面分离（edge telemetry 直达后端）|
| ADR-016 | Flow DAG 引擎：确定性骨架 + 概率节点内部 |
| ADR-017 | 凭据 vault AES-256-GCM |
| ADR-019 | Agent 工作区持久化 |
| ADR-021 | Turn 生命周期解耦（WithoutCancel）|
| ADR-024 | Edge 升级 bundle |
| ADR-026 | Self-obs HTTP metrics 中间件 |

### 11.2 核心红线

#### 架构
- 单服务：`cmd → web → controlplane → repo → model`，禁止跨层调用
- monorepo：`internal/<domain>` 之间禁止直接 import
- 接口在消费方定义，禁止循环依赖
- 依赖通过构造函数注入，不使用全局变量

#### 编码
- 禁止 `_ = fn()` 忽略错误
- 共享状态必须加锁，测试必须带 `-race`
- 错误用 `%w` 包装；不重复记录
- 所有涉及 IO 的函数第一个参数为 `context.Context`
- `init()` 仅允许做注册，禁止做 IO 或可能 panic
- 禁止全局可变变量

#### 安全
- 密码必须用 bcrypt / argon2id
- SQL 全部参数化
- 密钥禁止进代码仓库 / 镜像 / 日志
- 容器以非 root 用户运行
- 多租户接口强制 `tenant_id` 过滤

#### 可观测性
- 所有对外服务必须暴露 `/healthz` / `/readyz` / `/metrics`
- 日志结构化 + `trace_id`
- 高基数字段禁止作为 Prometheus label
- 敏感字段禁止明文入日志

#### 数据存储
- MySQL：生产 schema 变更走 migration 文件；大表用在线 DDL
- Redis：所有 key 必须设 TTL；禁止大 key
- ClickHouse：必须 Replicated engine；写入必须批量
- PII 字段加密存储

### 11.3 关键设计模式总结

1. **双路径并存**：双 kernel（legacy/graph）、双工具路径（闭包/BaseTool）——灰度切换安全
2. **HLD-021 turn 解耦**：`context.WithoutCancel` 把 turn 从 HTTP 请求生命周期剥离
3. **数据平面与控制平面分离**（ADR-014）：edge telemetry 经 nginx auth_request 直达后端
4. **post-split edge/device 拆分**：edge 仅 tunnel 身份，host facts 在 Device
5. **reconcile loop**：Supervisor / k8s InventoryPusher 类 K8s controller 模式
6. **deny-by-default**：subprocess env、cmdpolicy 策略
7. **fail-closed**：authz.Allow 出错返回 false
8. **graceful degradation**：nil-gating 注册、`Set*` 可选注入、noopClient
9. **存在性隐藏**：非所有者拿 `ErrNotFound` 防 IDOR
10. **partial failure 是常态**：批量操作永不返单 500；per-id 结果信封

---

## 附录：关键外部依赖

| 类别 | 依赖 |
|------|------|
| Web framework | `github.com/go-chi/chi/v5` |
| WebSocket | `github.com/gorilla/websocket` |
| ORM | `gorm.io/gorm` + `gorm.io/driver/mysql` + `github.com/glebarez/sqlite` |
| 鉴权 | `github.com/casbin/casbin/v2` + `github.com/golang-jwt/jwt/v5` |
| 可观测性 | `go.opentelemetry.io/otel` + `github.com/prometheus/client_golang` |
| LLM | `github.com/sashabaranov/go-openai` + `github.com/cloudwego/eino` |
| Tunnel | `github.com/singchia/geminio` + `github.com/singchia/frontier` |
| 嵌入 | `github.com/anush008/fastembed-go` + `github.com/yalue/onnxruntime_go` |
| IM | `github.com/larksuite/oapi-sdk-go/v3` + `github.com/open-dingtalk/dingtalk-stream-sdk-go` |
| 主机指标 | `github.com/shirou/gopsutil/v3` |
| 定时任务 | `github.com/robfig/cron/v3` |
| Markdown | `github.com/yuin/goldmark` |

---

*本文档基于 ongrid 源码 2026-07-31 快照生成，反映了系统的整体架构、核心子系统、部署拓扑与关键设计决策。*
