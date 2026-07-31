# `main.go`（ongrid manager）技术实现文档

> 源文件：`cmd/ongrid/main.go`
> 包路径：`github.com/ongridio/ongrid/cmd/ongrid`

## 1. 概述

本文件是 `ongrid` 云端 manager 二进制的入口，是整个项目体量最大、依赖最重的启动装配文件（约 5000 行）。它负责：加载配置；初始化 OpenTelemetry、DB、IAM（用户/组织/成员/Casbin 鉴权）；装配 manager 边端管理、设备、K8s、拓扑、告警、AIOps（含 LLM 多 provider 路由 + chatruntime 图内核 + 工具注册表 + 知识库 RAG + IM 桥接 + 报告 + Flow 编排）、市场/技能/MCP/审批等子系统；通过 frontierbound SDK 连接上游 broker 接收 edge 反向调用；暴露 HTTP API + Prometheus metrics；用 errgroup 编排所有后台 goroutine（API 服务、metrics 服务、DB 池采样、告警评估、设备在线状态对账、RCA 调查、报告调度、Flow 调度等）。

## 2. 包信息

- **包名**：`main`
- **所属模块**：`cmd/ongrid`（命令入口层，cmd → web/server → controlplane/biz → repo/data → model 分层最顶层）
- **依赖方向**：被操作系统启动；调用 `internal/iam/*`、`internal/manager/*`（biz/data/server/service/model）、`internal/pkg/*`、`internal/skill/*`；不直接依赖 `internal/edgeagent/*`（edge 侧包）

## 3. 关键类型与接口

本文件定义了大量适配器（adapter/shim）类型，用于在不同子包的同形接口间桥接，避免循环依赖。以下按职责分组列出。

### LLM / AIOps 适配器

```go
// 把 biz/setting.Service 适配为 llm.Resolver（避免 pkg/llm → manager/biz/setting 反向依赖）
type llmResolverFunc struct { svc *managerbizsetting.Service }

// 在 ChatReq 上盖固定 Provider id，用于 RoutingChatModel 的 per-provider inner
type providerInjectingClient struct {
    inner    llm.Client
    provider string
}

// chatruntime.Runtime 适配为 aiopstools.WorkerSpawner（避免 tools → chatruntime 循环）
type chatruntimeSpawnerShim struct { rt *aiopschatruntime.Runtime }

// 同上的可变版本，用于 ReviewGate 延迟绑定 runtime
type chatruntimeReviewSpawner struct {
    mu sync.RWMutex
    rt *aiopschatruntime.Runtime
}

// aiops data repo 适配为 ReviewGate 的 audit seam
type mutatingProposalSink struct { repo mutatingProposalRepo }

// 把 mentions.Searcher 适配为 agent.Mention 解析器（两个包的 Mention 类型不同）
type mentionResolverAdapter struct { inner *managerbizaiopsmentions.Searcher }
type chatruntimeMentionAdapter struct { inner *managerbizaiopsmentions.Searcher }

// chatruntime.AgentRegistry 适配为 tools.SubagentRegistry（仅 HasAgent 判断）
type agentRegistryShim struct { inner *aiopschatruntime.AgentRegistry }
```

### K8s / 鉴权适配器

```go
// managersvcedge.Service 适配为 k8s biz 的 EdgeIdentityIssuer
type k8sEdgeIdentityIssuer struct { svc *managersvcedge.Service }

// data plane auth 适配器：edge + telemetry 双源认证
type dataPlaneAuthAdapter struct {
    edge      *managerbizedge.AccessKeyAuthenticator
    telemetry *managerbizk8s.TelemetryAuthenticator
}
type edgeOnlyAuthAdapter struct { authn *managerbizedge.AccessKeyAuthenticator }
type telemetryOnlyAuthAdapter struct { authn *managerbizk8s.TelemetryAuthenticator }

// K8s remote_write 目标解析器（嵌入式 Prom → publicURL；外部 Prom → 直连 + auth）
type k8sRemoteWriteResolver struct {
    resolver  *managerbizsetting.PromResolver
    prom      config.PromConfig
    publicURL string
}

// 插件 endpoint 解析器：loki/tempo URL 不可达时回退到 manager publicURL
type pluginEndpointResolver struct {
    publicURL string
    loki      telemetryBackendResolver
    tempo     telemetryBackendResolver
}
```

### WebSSH / Flow / 审批适配器

```go
// fbClient 适配为 webshell Streamer（io.ReadWriteCloser）
type webshellStreamerAdapter struct { c *managersvcfb.Client }
type webshellAuditAdapter struct { repo *managerwebshelldata.Repo }
type hostDeviceResolverAdapter struct { repo *managerdevicedata.EdgeDeviceRepo }

// Flow 工具调用器：把 aiops 工具注册表适配为 flow 引擎的 ToolInvoker
type flowToolInvoker struct {
    reg   *aiopstools.Registry
    deps  aiopstoolsdec.Deps
    tools map[string]aiopstoolsbase.BaseTool
    mcp   *flowMCPSource // 后注入
}

// Flow 工具目录（palette）
type flowToolCatalog struct {
    reg *aiopstools.Registry
    mcp *flowMCPSource
}

// Flow 的 agent/llm/notify 执行器
type flowAgentRunner struct { rt *aiopschatruntime.Runtime }
type flowLLMRunner struct { client llm.Client }
type flowNotifierShim struct {
    channels *manageralertdata.Repo
    router   *notify.Router
}

// MCP live source（每次调用实时查询服务器工具列表）
type flowMCPSource struct {
    uc  *managerbizmcp.Usecase
    log *slog.Logger
}

// 审批 proposer 适配器（cloud_bash / host_bash / install_skill / mcp_call）
type cloudBashProposerShim struct{ uc *managerbizapproval.Usecase }
type hostBashProposerShim struct{ uc *managerbizapproval.Usecase }
type installSkillProposerShim struct{ uc *managerbizapproval.Usecase }
type mcpCallerShim struct{ uc *managerbizmcp.Usecase }
type mcpProposerShim struct{ uc *managerbizapproval.Usecase }

// 报告投递适配器
type reportDelivererShim struct {
    channels *manageralertdata.Repo
    router   *notify.Router
}

// IM 发送适配器（send_im_message 工具）
type imSenderShim struct {
    channels *manageralertdata.Repo
    router   *notify.Router
}

// 文件型页面存储（serve_page 工具）
type filePageStore struct {
    dir string
    log *slog.Logger
}
```

### 审批 payload 类型

```go
type cloudBashPayload struct {
    Command     string
    Credentials []string
    Credential  string // legacy
    SessionID   string
}
type hostBashPayload struct {
    DeviceIDs      []uint64
    Command        string
    TimeoutSeconds int
}
type installSkillPayload struct {
    URL    string
    Type   string
    Ref    string
    UserID uint64
}
type mcpCallPayload struct {
    Server    string
    Tool      string
    Arguments map[string]any
    Command   string
}
```

### 其他

```go
// 多 Investigator 链式调用
type investigatorChain []managerbizalert.Investigator

// 页面元信息
type pageMeta struct {
    ID        string
    Title     string
    CreatedAt string
    URL       string
    SizeBytes int64
    Source    string
}

// MCP 工具条目
type mcpEntry struct {
    wire   string
    server string
    bare   string
    desc   string
    schema json.RawMessage
}
```

## 4. 关键函数与流程

### `main`（核心入口，约 2400 行）

按启动顺序分组说明关键装配步骤：

#### 4.1 基础设施初始化
1. `config.Load()` 加载配置；JWT secret 仍为默认值则拒绝启动（安全红线）
2. `logger.WithService` 构造 slog logger
3. `signal.NotifyContext(SIGINT, SIGTERM)` 根 context
4. `tracing.Init` 初始化 OpenTelemetry（端点 `tempo:4318`，可 env 覆盖；失败仅 Warn 不阻断）
5. `dbx.Open` + `dbx.RunMigrations` 按依赖顺序执行所有 data 包的 Migrate

#### 4.2 IAM 装配
1. `iamdatauser.NewRepo` + `auth.NewSigner` + `iambizuser.NewUsecase` + `BootstrapAdmin`
2. `EnsureSuperuser` 把现有 admin 标记为 is_superuser
3. `iambizauthz.New` 构造 Casbin Enforcer + `SeedRolePolicies`
4. 构造 org/membership service，`HydrateMemberships` 从 DB 灌 casbin g 规则
5. `EnsureSeed` 创建"默认组织"并把现有用户回填为成员；reparent 散落的顶级 org
6. `authzmw.New` 构造 RBAC 中间件

#### 4.3 系统设置 + LLM 装配
1. `managersettingdata.NewRepo` + `managerbizsetting.New`
2. 用 `SetIfAbsent` 把 env 中的 LLM/Prom/Grafana/Loki/Tempo/WebSearch 配置种子化到 DB（首次启动；后续 admin 编辑持久化）
3. 构造 `llmResolver`（读 system_settings）+ `llm.NewWithResolver`
4. 构造多 provider 配置列表（OpenAI/Anthropic/Zhipu/Gemini/DeepSeek/Kimi），每个 provider 的 APIKey 为空则不加入
5. `llm.NewMultiClient` 多 provider 路由器 + `llmSettingsResolver` 动态 catalog
6. 把 `llmRouter` 作为 `llm.Client` 供下游使用

#### 4.4 manager 核心装配
1. edge/device biz + repo 构造
2. **Boot backfill**：把 stale online edge 改为 offline（manager 崩溃修复）+ FailOrphaned 孤儿调查报告
3. K8s biz + remote_write resolver + telemetry target resolver
4. topology biz + 把 edge/device/k8s UC 的 topology mirror 钩子设上 + boot reconcile
5. data plane auth handler（edge + telemetry 双源）
6. alert biz + 通知渠道种子化 + 内置规则种子化 + CachedRulesProvider
7. cloud Prom（可选）：`promwriteClient` / `promQueryClient` / `promwriteIngester`
8. integration handler（Grafana/Prom/Loki/Tempo/WebSearch probe）
9. metric/logs/traces query handler（Prom-backed，替代 legacy MySQL 路径）

#### 4.5 frontierbound + WebSSH
1. `managersvcfb.New` 或 `NewDisabled`（`ONGRID_FRONTIER_DISABLED=true`）
2. `managersvcfb.Install` 注册 edge lifecycle callback + reverse-call handler（edge auth、edge UC、metric ingester、prom ingester、plugin config UC、webshell router、device resolver、k8s registry）
3. `pluginConfigUC.SetNotifier(fbClient)` 回填 notifier（之前为 nil）
4. WebSSH handler 构造（fbClient.OpenStream + router + audit）

#### 4.6 AIOps 装配
1. `aiopstools.NewRegistry` + 各 Set* 钩子（plugin config lister、config manager、k8s snapshot、audit lister、topology info、topology graph）
2. `aiopsagent.New` legacy agent
3. **chatruntime graph kernel**（`ONGRID_AGENT_KERNEL=graph`）：
   - `loadBootstrapRegistries` 加载 ./skills + ./agents + marketplace skills
   - `buildAIOpsRuntime`：RoutingChatModel（per-provider inner）+ ToolBag（BuildBaseTools + AppendHostFilesTools + decorator wrap）+ 默认 "default" persona + GraphCfg + CallbackDeps（含 BudgetChecker）+ CoordinatorStubs
   - 注入 AgentTool/SendMessage/TaskStop 三件套（coordinator-only）到 toolbag
4. `managersvcaiops.NewWithKernel` 双内核服务（legacy + graph）
5. **Knowledge base RAG**：embedding + qdrant + `managerbizknowledge.New`；可选 builtin vault seed（goroutine 后台同步）
6. **IM bridge**：adapter + bridge + stream supervisor（注册 feishu/dingtalk/telegram/slack factory）+ `go imbridgeStreamSupervisor.Run`
7. **Mention searcher** + 注入 agent 与 chatruntime
8. **Investigators**：legacy aiopsinvestigator（可选）+ structured RCA investigator（`ONGRID_INVESTIGATOR_ENABLED=true`，goroutine backfill unstarted incidents）
9. `chainInvestigators` 串联

#### 4.7 报告 + Flow + 技能/市场/MCP/审批
1. **Report**：`managerbizreport.NewWorkerGenerator`（chatruntime-based）或 `NewUnavailableGenerator`；scheduler 启动
2. **Flow**：`newFlowToolInvoker` + `flowExec`（Tools/Notify/LLM/Agent）+ `managerbizflow.NewUsecase` + `HealStaleRuns` + dispatcher + scheduler
3. **Skill**：`managerbizskill.New` + `WithExtraSkills`（chatruntime SKILL.md）
4. **Marketplace**：`managerbizmarketplace.NewUsecase` + `managerservermarketplace.NewHandler`
5. **Secret vault**：`managerbizsecret.NewUsecase`
6. **MCP**：`managerbizmcp.NewUsecase` + flow MCP source 接入
7. **Approval inbox**：`managerbizapproval.NewUsecase` + 注册 cloud_bash/host_bash/mcp_call/install_skill executor
8. **cloud_bash 配套**：`runner.NewShellRunner` + workspace manager + PATH/PYTHONUSERBASE 环境注入
9. **chatRT.AppendToolBag**：cloud_bash/host_bash/install_skill/serve_page/send_im_message + MCP 工具（connect enabled servers + ListTools + Wrap）

#### 4.8 HTTP 路由
1. chi router + OTel middleware + Metrics middleware + Audit middleware
2. `/healthz` `/readyz` 公开端点
3. `/internal/auth/*` 数据平面鉴权（nginx auth_request 用）
4. `/api` 路由组：public（login/refresh/im webhook/serve_page share）+ protected（JWT + 所有 BC handler Register）
5. `/r/{token}` 公开报告分享
6. `httpserver.New` × 2（api + metrics）

#### 4.9 errgroup 后台 goroutine
- `apiServer.Start` / `metricsServer.Start`
- DB pool sampler（10s ticker，更新 `prom.DBPool*` gauge）
- `auditUC.RunRetention`（每日 03:00 清理）
- `runK8sEventRetention` / `runK8sTopologyReconcile`
- chatruntime worker session sampler（15s，更新 `prom.SetWorkerSessions`）
- RCA investigator inflight gauge（15s）
- `deviceUC.ReconcilePresence`（boot + 60s ticker，修复孤儿设备）
- `alertRules.Loop` / `pipelineEval.Loop` / `retryWorker.Loop`（告警评估 + 重试）

#### 4.10 退出
- `eg.Wait()`；非 `context.Canceled` 错误 `os.Exit(1)`
- `defer otelShutdown`（5s 超时）+ `defer fbClient.Close`

### `buildAIOpsRuntime`
- **签名**：`func buildAIOpsRuntime(ctx, cfg, llmClient, llmRouter, toolsReg, sessions, mutatingProposals, fbClient, edgeUC, deviceUC, reg, log, skillReg, agentReg, resolver) (*aiopschatruntime.Runtime, error)`
- **职责**：构造 graph 内核 chatruntime（可选内核，失败回退 legacy）
- **流程**：
  1. **RoutingChatModel**：从 `resolver.ResolveProviders` 获取已配置 provider 列表，每个 provider 用 `providerInjectingClient` + `llm.NewClientChatModel` 构造 inner ChatModel；未配置 provider 则用 env 兜底；最后为所有已知 provider id 预注册 inner（即使未配置），使 UI 后续添加 provider 时无需重启
  2. **ToolBag**：`toolsReg.BuildBaseTools()` + `AppendHostFilesTools` + `aiopstoolsdec.Wrap` 装饰器链（timeout/ratelimit/metric/review）；`chatruntimeReviewSpawner` + `mutatingProposalSink` 用于 ReviewGate
  3. **默认 persona**：`agentReg.Add(&Agent{Name: "default", Tools: buildCoordinatorToolNames(bag.AllTools()), MaxTurns: 30})`
  4. **CallbackDeps**：Persistence/Audit/Metrics + 可选 BudgetChecker（`cfg.LLM.DailyTokenLimit > 0`）
  5. **CoordinatorStubs**：`aiopstools.CoordinatorRedirectStubs()` 包装后加入（捕获 LLM 幻觉工具名）
  6. `aiopschatruntime.NewRuntime` 构造；`reviewSpawner.SetRuntime` 回填；`rt.SetToolBag(bag)`
- **错误处理**：任何步骤失败返回 `(nil, err)`，`main` 据 Warn 回退 legacy kernel

### `loadBootstrapRegistries`
- **职责**：从 `ONGRID_BUILTIN_SKILLS_ROOT`（默认 ./skills）+ `ONGRID_BUILTIN_AGENTS_ROOT`（默认 ./agents）+ `ONGRID_SKILLS_ROOT`（默认 /var/lib/ongrid/skills）加载技能与 persona
- **亮点**：无论 kernel 是否为 graph 都加载，确保 `/v1/agents` 在无 LLM provider 时也能列出 persona

### 审批 proposer 适配器
- `cloudBashProposerShim.ProposeAndAwait`：`uc.Propose` 入队 → 通过 `aiopschatruntime.EmitFromContext` 推送 `EventApprovalPending` 卡片到 SSE → `awaitDecision` 轮询（1.5s 间隔，30 分钟超时）
- `awaitDecision`：轮询 approval 状态；executed/failed/rejected 返回对应结果 JSON；ctx 取消返回 cancelled；超时返回 timeout
- 同模式用于 host_bash / install_skill / mcp_call

### `filePageStore`
- **SavePage**：随机 12 字节 hex 作为 id + capability；`pages/<id>/index.html` + `meta.json` sidecar
- **readPageHTML**：兼容目录布局与 legacy 平铺文件
- **List**：扫描目录，读 meta.json 或从 HTML 提取 title，按 CreatedAt 降序
- **Delete**：`isHexToken` 校验 id 防路径穿越 + `os.RemoveAll`
- **share token**：`mintPageShareToken` / `verifyPageShareToken` 用 HMAC-SHA256 签名 `pageID|expUnix`，30 天 TTL

### `flowToolInvoker`
- **mergeBag**：把 ToolBag 中新工具（未在 map 中）Wrap 后加入；跳过 chat-only review 工具；避免重复 Wrap 导致 metric 重复注册
- **InvokeTool**：MCP 工具（`mcp__` 前缀）走 live source；其他工具从 map 取出，`coerceArgsToSchema` 做类型强转（scalar→array、numeric-string→number 等）后调用

### `coerceArgsToSchema` / `coerceValue`
- **职责**：Flow 编排中 `{{ref}}` 解析后的值类型可能与 schema 不符，做 best-effort 强转避免 unmarshal 失败
- **覆盖**：array（scalar 包裹 / JSON 字符串解析）、number/integer（字符串解析）、boolean（"true"/"false"）、object（JSON 字符串解析）

### `runK8sEventRetention` / `runK8sTopologyReconcile`
- **Event retention**：boot 立即跑一次 + 按 `EventCleanupInterval` ticker 循环；统计 deleted_by_ttl + deleted_by_cluster_limit
- **Topology reconcile**：30s ticker 调 `uc.ReconcileTopology`

## 5. 依赖关系

- **内部包**（按分层）：
  - `internal/iam/*`：user/org/membership/authz biz + data + server + service + model
  - `internal/manager/biz/*`：device/edge/k8s/metric/promwrite/topology/alert/aiops（含 agent/chatruntime/graph/investigator/mentions/tools）/approval/audit/flow/grafana/imbridge/knowledge/marketplace/mcp/monitor/report/secret/setting/skill/webshell
  - `internal/manager/data/*`：各 BC 的 store
  - `internal/manager/server/*`：各 BC 的 HTTP handler + middleware
  - `internal/manager/service/*`：aiops/alert/edge/frontierbound/k8s/metric/prometheus/systemhealth/systemupgrade
  - `internal/manager/model/*`：alert/aiops/setting/webshell
  - `internal/pkg/*`：auth/authzmw/config/dbx/errs/httpserver/llm/logger/runner/secretbox/workspace/embedding/qdrantx/tracing/logquery/notify/prom/promauth/promquery/promwrite/tracequery/mcpclient
  - `internal/skill/*`：core + builtin（_ import 触发 init 注册）
- **外部库**：
  - `github.com/cloudwego/eino`：AIOps graph 框架（ChatModel 接口）
  - `github.com/go-chi/chi/v5`：HTTP router
  - `github.com/google/uuid`：报告 ID 生成
  - `github.com/prometheus/client_golang`：metrics registry
  - `golang.org/x/sync/errgroup`：goroutine 编排
  - `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp`：OTel HTTP middleware
  - 标准库：context/crypto/hmac/crypto/rand/crypto/sha256/encoding/json/encoding/hex/errors/fmt/io/log/slog/net/http/net/url/os/os/signal/path/filepath/sort/strconv/strings/sync/syscall/time
- **被调用方**：操作系统启动；docker-compose；systemd

## 6. 并发与资源管理

- **errgroup 编排**：所有后台 goroutine 通过 `errgroup.WithContext(rootCtx)` 管理；任一失败取消 egCtx，触发兄弟退出
- **根 context**：`signal.NotifyContext(SIGINT, SIGTERM)` 支持 SIGINT/SIGTERM 优雅关闭
- **后台 goroutine**（启动顺序）：
  - Grafana SA bootstrap（10s 延迟 + 30s sync 超时）
  - builtin vault seed sync（5 分钟超时）
  - IM bridge stream supervisor（`go imbridgeStreamSupervisor.Run`）
  - RCA investigator backfill（30s 超时，boot 后跑一次）
  - errgroup 内：apiServer/metricsServer/DB pool sampler/audit retention/k8s event retention/k8s topology reconcile/worker session sampler/investigator inflight gauge/device presence reconcile/alert rules loop/pipeline evaluator/retry worker
- **互斥锁**：
  - `chatruntimeReviewSpawner`：`sync.RWMutex` 保护 rt 字段延迟绑定
  - `flowToolInvoker`：无显式锁，但 `tools map` 在构造后只读（mergeBag 在启动阶段完成）
- **资源释放**：
  - `defer otelShutdown`（5s 超时 context）
  - `defer fbClient.Close`
  - `defer legacy.Close()`（legacy investigator）
  - HTTP server / DB pool / ticker 由各自包或 `defer t.Stop()` 管理
- **typed-nil 防护**：多处用 `if client != nil { var iface SomeInterface = client }` 模式避免 typed-nil 接口非 nil 陷阱（promQuerier/promTester/promWiring 等）

## 7. 设计模式与亮点

- **适配器模式（大量）**：本文件定义了 20+ 个 adapter/shim 类型，全部用于解决"两个子包有同形接口但不应直接 import"的循环依赖问题。这是 Go 大型 monorepo 装配层的典型手法——把桥接逻辑集中在 cmd 层，保持子包依赖单向
- **双内核模式**：AIOps 支持 legacy 与 graph 两种内核，`ONGRID_AGENT_KERNEL` 环境变量切换；graph 构造失败自动回退 legacy——渐进式迁移
- **propose-confirm 审批模式**：cloud_bash/host_bash/install_skill/mcp_call 都走 `ProposeAndAwait` 同步阻塞模型（HLD-021），工具在 SSE 推审批卡片后轮询 decision，30 分钟超时——把人类审批嵌入 ReAct 循环
- **配置种子化 + 持久化**：`SetIfAbsent` 模式确保首次启动从 env 种子化 DB，后续 admin 编辑持久化跨重启——LLM/Prom/Grafana/Loki/Tempo/WebSearch 全走此模式
- **typed-nil 防护**：多处显式 `if client != nil` 把 typed-nil 转为 untyped nil，避免接口非 nil 但底层 nil 的陷阱
- **boot backfill 模式**：启动时修复 stale online edge / 孤儿调查报告 / 孤儿设备 / 散落顶级 org——把"前次崩溃残留"作为启动必做项
- **延迟绑定**：`pluginConfigUC` 先用 nil notifier 构造，fbClient 存在后再 `SetNotifier`；`chatruntimeReviewSpawner` 用 RWMutex 延迟绑定 runtime——解决鸡生蛋问题
- **协调器工具白名单**：`buildCoordinatorToolNames` 把核心只读工具 + 显式 policy 例外列入 coordinator 可用集，其余走 AgentTool 派发——避免 coordinator 变成全功能 deep-dive worker
- **coerceArgsToSchema**：Flow 编排的 `{{ref}}` 解析可能产生类型不匹配，做 best-effort 强转而非硬失败——借鉴 n8n/Dify 的容错思路
- **HMAC 无状态 share token**：`mintPageShareToken` / `verifyPageShareToken` 用 HMAC 签名 `pageID|exp`，无需 DB 存储 token——文件型资源的无状态授权
- **i18n 工具名映射**：`flowToolLabelZhMap` / `flowToolDescZhMap` 在 cmd 层集中维护工具中文名，作为 SPA 的单一真相源
- **ongridBasePrompt**：硬编码的 AIOps 协调器 system prompt，强制 tool_call 前 content 必须说明目的、3 数据点后给阶段结论、禁止同工具同参数重复——针对观察到的 LLM self-loop 失败模式

## 8. 注意事项

- **文件体量**：本文件约 5000 行，是项目最复杂的启动装配；任何修改应先确认对应 BC 的独立单元测试覆盖，避免在 main 中调试
- **JWT secret 安全红线**：`insecureJWTSecret` 检测确保生产环境必须设置强随机 secret，否则拒绝启动——这是安全合规要求
- **frontierbound 可禁用**：`ONGRID_FRONTIER_DISABLED=true` 使 edge-tunnel 功能在调用点报错，但 HTTP/DB 不受影响——用于 e2e 测试环境
- **typed-nil 陷阱**：新增可选依赖（如 promQueryClient）时必须用 `if x != nil { var iface = x }` 模式，否则 nil 检查失效
- **审批超时**：`approvalWaitTimeout = 30 分钟`，cbDeps.Timeout 设为 31 分钟确保工具自身超时先于装饰器超时——顺序错了用户会看到晦涩的 ErrToolTimeout
- **cloud_bash 工具延迟注入**：cloud_bash proposer 在 `buildAIOpsRuntime` 之后才构造，所以 startup toolbag 不含 cloud_bash；必须用 `chatRT.AppendToolBag` 后置注入——注释明确说明否则 LLM 调用 cloud_bash 会 "tool not found"
- **MCP 工具热加载**：boot 时连接 enabled servers + ListTools + 注入；运行时 server 变更需重启 manager（或通过 flowMCPSource live 查询，但 chat 路径是 boot 快照）
- **secretbox.KeyIsWeak**：`ONGRID_SECRET_KEY` 未设置时仅 Warn 不阻断，但凭证加密用不安全内置 key——生产环境必须设置
- **市场签名校验**：`mpRequireSigned` 默认要求 `ongrid-official` 源签名；`mpDevMode` 默认 true（dev 友好），生产应显式设 `ONGRID_MARKETPLACE_DEVMODE=false`
- **Grafana bootstrap 异步**：Grafana 容器可能未就绪，bootstrap 在 goroutine 中延迟 10s 执行，失败非致命——admin 可手动配置
- **backfill goroutine 超时**：RCA backfill 用 30s 超时 + detached ctx，避免阻塞启动；但 100 条上限可能在大量历史 incident 时漏处理
- **ongridBasePrompt 修改**：该 prompt 直接影响 LLM 行为，修改应基于观察到的失败模式并做 A/B 验证；prompt 中的工具名引用（如 `query_promql`）必须与注册名一致
- **adapter 维护成本**：新增 BC 时若接口与现有 adapter 同形，考虑在 cmd 层加 adapter；但过多 adapter 是代码异味，长期应考虑抽取到独立的 `internal/wiring` 包
- **测试策略**：main.go 本身难以单元测试（装配逻辑），应通过 e2e 测试覆盖关键启动路径；各 adapter 应有独立单元测试
