# OnGrid 技术组件协作机制

> 本文档分析 OnGrid 所有技术组件的协作机制，包含启动注入链、边云隧道、AI Agent 全链路、认证授权、告警流水线、IM 桥接、知识库 RAG 等完整协作序列。
> 生成时间：2026-08-03

---

## 目录

1. [云端启动与依赖注入链](#1-云端启动与依赖注入链)
2. [边云隧道协作机制](#2-边云隧道协作机制)
3. [AI Agent 全链路协作](#3-ai-agent-全链路协作)
4. [认证授权协作机制](#4-认证授权协作机制)
5. [告警流水线协作](#5-告警流水线协作)
6. [IM 桥接协作](#6-im-桥接协作)
7. [知识库 RAG 协作](#7-知识库-rag-协作)
8. [Flow 流程引擎协作](#8-flow-流程引擎协作)
9. [定时报告协作](#9-定时报告协作)
10. [后台并发任务协作](#10-后台并发任务协作)
11. [架构全景序列图](#11-架构全景序列图)

---

## 1. 云端启动与依赖注入链

### 1.1 启动总览

OnGrid 云端管理器的启动逻辑全部集中在 `cmd/ongrid/main.go` 的 `main()` 函数（L277-3066），采用"长 main + 辅助函数"风格，按 13 个阶段严格顺序完成组件构造和注入。

### 1.2 13 阶段启动链

```
阶段 1: 基础设施
  config.Load() → logger → signal.NotifyContext → tracing.Init
  → dbx.Open(DB) → RunMigrations(22个域)

阶段 2: IAM 限界上下文
  userRepo → signer(JWT) → userUC → BootstrapAdmin → iamSvc → iamHandler
  → authzEnf(Casbin) → SeedRolePolicies → orgRepo → membershipRepo
  → orgSvc → membershipSvc → HydrateMemberships → EnsureSeed("默认组织")
  → iamSvc.SetOrgs/SetMemberships/SetAuthz → authzMW

阶段 3: 系统设置与可观测性
  prom.NewRegistry() → RegisterManagerMetrics → notifyRouter
  → settingRepo → settingSvc → promResolver → auditRepo → auditUC
  → SetIfAbsent(LLM/Prom/Grafana/Loki/Tempo/WebSearch 初始配置)
  → lokiResolver → tempoResolver → grafanaSvc
  → monitorRepo → monitorSvc → monitorHandler

阶段 4: LLM 客户端链
  newLLMResolver(settingSvc)                          — DB→Resolver 适配器
  → llm.NewWithResolver(Config, resolver)             — OpenAI 兼容客户端
  → llm.NewMultiClient(providerCfgs, default, openai) — 多 Provider 路由器
  → llmSettingsResolver(settingSvc)                   — 动态 Provider 目录
  → llmRouter.SetProvidersResolver(llmSettingsResolver)
  → llmClient := llm.Client(llmRouter)                — 统一接口

阶段 5: Edge / K8s / 拓扑域
  edgeRepo → deviceRepo → edgeDeviceRepo → deviceUC → edgeUC
  → edgeAuthn → edgeSvc → k8sRepo → k8sUC
  → k8sUC.SetRemoteWriteResolver / SetTelemetryTargetResolver
  → k8sSvc → edgeSvc.SetManagedEdgeGuard(k8sSvc)
  → pluginConfigRepo → pluginConfigUC(notifier=nil,待回填)
  → edgeHandler → deviceHandler
  → topologyNodeRepo/RelationRepo → topologyUC → topologyHandler
  → edgeUC.SetNodeMirror(topologyUC) / deviceUC.SetTopologyMirror / k8sUC.SetTopologyMirror

阶段 6: 告警与遥测
  alertRepo → alertUC → SeedChannelsFromConfig / SeedBuiltinRules
  → alertRules(CachedRulesProvider) → alertResolver → alertInhibitor
  → metricIngestSvc → [可选] promwriteClient / promQueryClient
  → integrationHandler → metricHandler → logsHandler(Loki) → tracesHandler(Tempo)

阶段 7: Frontierbound 客户端
  fbClient := managersvcfb.New(Config{Addr, ServiceName})
  → managersvcfb.Install(rootCtx, fbClient, Wiring{
      EdgeAuthn, EdgeUC, MetricIngester, PromIngester,
      PluginConfigUC, WebshellRouter, DeviceResolver,
      K8sRegistry, K8sInventory
  })
  → 回填: edgeSvc.SetEdgeCaller(fbClient)        — edge 管理操作经隧道下发
  → 回填: pluginConfigUC.SetNotifier(fbClient)    — 配置变更推送
  → 回填: pluginConfigUC.SetDatabaseMetricsSecretWriter(fbClient)

阶段 8: AIOps 工具注册表与 Agent
  aiopsRepo → mutatingProposalRepo
  → [可选] promQuerier / logQuerier / traceQuerier
  → toolsReg := aiopstools.NewRegistry(fbClient, edgeUC, deviceUC, ...)
  → toolsReg.SetPluginConfigLister / SetConfigManager / SetK8sSnapshotReader / ...
  → aiopsAgent := aiopsagent.New(llmClient, toolsReg, aiopsRepo, Config{...})

阶段 9: 知识库与 Bootstrap 注册表
  knowledgeRepo → embedder(embedding.New) → qdrantClient
  → knowledgeUC := knowledge.New(...)
  → toolsReg.SetKnowledgeSearcher(knowledgeUC)
  → 后台 goroutine: SyncBuiltinVault
  → bootstrapSkillReg, bootstrapAgentReg := loadBootstrapRegistries(./skills, ./agents)

阶段 10: AIOps Runtime 构建
  buildAIOpsRuntime(ctx, cfg, llmClient, llmRouter, toolsReg, ...)
    → RoutingChatModel (7 个 provider 的 inner ChatModel)
    → ToolBag (BuildBaseTools + AppendHostFilesTools + Wrap 装饰器)
    → Agent Registry (虚拟 "default" persona)
    → Callback Deps (Persistence + Audit + Metrics + Budget)
    → Runtime (NewRuntime)
  → 回填: toolsReg.SetWorkerSpawner(chatruntimeSpawnerShim{rt})
  → 回填: rt.AppendToolBag(AgentTool + SendMessage + TaskStop)

阶段 11: 后续服务装配
  aiopsSvc (双内核: legacy + graph)
  → imbridgeSvc → mentionSearcher → legacyInv / rcaInv
  → reportRepo → reportGen → reportUC → reportHandler
  → flowRepo → flowInvoker → flowExec → flowUC → flowHandler
  → systemHealthSvc → skillSvc → mpUC(marketplace)
  → secretUC → mcpUC → approvalUC
  → cloudBashRunner + executor 注册
  → 回填: toolsReg.SetCloudBashProposer / SetHostBashProposer / SetIMSender / SetPageStore
  → 回填: chatRT.AppendToolBag(cloud_bash / host_bash / mcp / serve_page / send_im)

阶段 12: HTTP 服务器
  mux := chi.NewRouter()
  → 中间件: otelhttpmw → MetricsMiddleware → AuditMiddleware
  → 公开路由: /healthz, /readyz, /api/v1/auth/*, /api/v1/notifications/webhook/*
  → 保护路由: auth.Middleware(signer) → 28 个 Handler.Register(protected)
  → apiServer := httpserver.New(cfg.HTTPAddr, mux)
  → metricsServer := httpserver.New(cfg.MetricsAddr, metricsMux)

阶段 13: errgroup 并发 goroutine
  eg.Go(apiServer.Start)           — HTTP API 服务
  eg.Go(metricsServer.Start)       — Prometheus 指标服务
  eg.Go(DB pool sampler, 10s)      — 数据库连接池采样
  eg.Go(auditUC.RunRetention)      — 审计日志保留清理
  eg.Go(runK8sEventRetention)      — K8s 事件保留清理
  eg.Go(runK8sTopologyReconcile)   — K8s 拓扑对账
  eg.Go(chatruntime worker sampler, 15s) — Agent worker 状态采样
  eg.Go(RCA investigator inflight gauge) — 根因分析并发度采样
  eg.Go(device presence reconciler, 60s)  — 设备在线状态对账
  eg.Go(alertRules.Loop)           — 告警规则缓存刷新
  eg.Go(pipelineEval.Loop)         — 告警规则评估
  eg.Go(retryWorker.Loop)          — 通知投递重试
  eg.Wait()                        — 阻塞直到任一 goroutine 退出
```

### 1.3 核心设计模式

**"先构造后回填"模式**：因循环依赖无法一次性注入，先以 nil 构造，待依赖就绪后通过 Setter 回填：

| 组件 | 回填时机 | 回填内容 |
|------|---------|---------|
| `pluginConfigUC` | 阶段 7 fbClient 构造后 | `SetNotifier(fbClient)` |
| `edgeSvc` | 阶段 7 fbClient 构造后 | `SetEdgeCaller(fbClient)` |
| `reviewSpawner` | 阶段 10 Runtime 构造后 | `SetRuntime(rt)` |
| `toolsReg` | 阶段 11 各 executor 构造后 | `SetCloudBashProposer / SetHostBashProposer / ...` |
| `chatRT` | 阶段 11 后注册工具 | `AppendToolBag(cloud_bash / host_bash / ...)` |

### 1.4 buildAIOpsRuntime 内部 5 步

```
Step 1 — RoutingChatModel:
  每个 provider → providerInjectingClient{inner: llmClient, provider}
  → llm.NewClientChatModel(ClientChatModelConfig{Client, Model})
  → llm.NewRoutingChatModel(RoutingChatModelConfig{Inner, DefaultProvider, DefaultResolver})

Step 2 — ToolBag:
  bag := toolsReg.BuildBaseTools()
  → bag = AppendHostFilesTools(bag, fbClient, edgeUC, deviceUC)
  → baseTools := bag.SchemasForLLM()
  → wrapped := [装饰器链: audit / timeout / rate-limit / metric](每个 tool)

Step 3 — Agent Registry:
  注册虚拟 "default" persona (顶层 chat coordinator)
  → buildCoordinatorToolNames (核心只读工具 + 少量例外)

Step 4 — Callback Deps:
  cbDeps := Deps{
    Persistence: {Repo: sessions, Registerer: reg},
    Audit:       {Logger: log},
    Metrics:     {Registerer: reg},
  }
  → [条件] cbDeps.BudgetChecker = llm.NewInMemoryBudget(DailyTokenLimit)

Step 5 — 拼装 Runtime:
  rt := NewRuntime(Config{
    SkillRegistry, AgentRegistry, Sessions, ChatModel,
    ToolBag: wrapped, CoordinatorStubs,
    BasePrompt, HistoryLimit: 50,
    GraphCfg: {MaxIterations: 30, ToolTimeout: 15s},
    CallbackDeps: cbDeps,
  })
  → reviewSpawner.SetRuntime(rt)  — 解决先有鸡还是先有蛋
  → rt.SetToolBag(bag)            — 完整未脱敏工具袋
```

---

## 2. 边云隧道协作机制

### 2.1 三层架构

```
[Edge Agent]                    [Frontier Broker]                 [Cloud Manager]
  tunnel.Client                                                   frontierbound.Client
  (geminio RetryEnd)  ──TCP/TLS──>  (geminio broker)  <──TCP──    (fbsvc.Service)
      │                                                                  │
      ├─ RegisterHandler (cloud→edge RPC)                                ├─ Call(edgeID, method, body) (cloud→edge)
      ├─ Call(method, req) (edge→cloud RPC)                             ├─ Register(method, handler) (edge→cloud)
      └─ OnReconnect(callback)                                          └─ RegisterGetEdgeID/EdgeOnline/EdgeOffline
```

### 2.2 边缘注册完整序列

```
Edge Agent 启动:
  1. tunnel.NewClient(CloudAddr, AccessKey, SecretKey)
  2. RegisterHandler(get_host_load, get_process_list, bash.exec, host_files, restart_service, ...)
  3. OnReconnect(→ registerEdge)  — 重连后重新注册
  4. client.Dial(ctx)  — 指数退避(1s→60s)拨号
  5. registerEdge:
     a. collector.HostInfo(ctx) → hostname/os/cpu/mem/指纹
     b. [K8s] applyKubernetesHostIdentity → K8s node 信息覆盖
     c. client.Call(MethodRegisterEdge, RegisterEdgeRequest)

Frontier Broker:
  6. 收到连接 → 从 Meta blob 提取 {access_key, secret_key}
  7. 调用 GetEdgeID 回调 → 认证 → 返回规范 EdgeID

Cloud Manager frontierbound handler:
  8. bindEdgeTransport(canonicalEdgeID) — 建立传输ID↔规范ID 双向映射
  9. 分流:
     [K8s controller]: ClearHostDeviceLink → K8sRegistry.HandleRegister → setKubernetesController(true)
     [普通 host / K8s node]: EdgeUC.HandleRegister:
       → host fingerprint 查找/创建 Device 行
       → 更新主机事实(CPU/Mem/OS)
       → edge_devices(type=host) 关联 edge ↔ device
       → 状态翻转 online, lastSeen=now
  10. 返回 RegisterEdgeResponse{EdgeID, ServerTime}

Edge Agent:
  11. 存储 EdgeID → 启动 heartbeatLoop(30s) + metricsLoop(10s)
```

### 2.3 心跳与指标推送

```
heartbeatLoop (每 30s):
  1. [EdgeID==0] → 重试 registerEdge
  2. 收集插件健康: pluginHealthFn() → []PluginHealthWire
  3. client.Call(MethodHeartbeat, HeartbeatRequest{EdgeID, Ts, Plugins})
  4. [失败] → 重新 registerEdge 修复绑定
  5. [连续 5 次失败] → errTunnelStuck 终止 agent

Cloud heartbeat handler:
  6. canonicalizeEdgeID(edgeID) — 查绑定映射
  7. EdgeUC.HandleHeartbeat:
     → repo.UpdateStatus(edgeID, online, ts) — 更新 edges 表 last_seen_at
     → 同步刷新关联 Device 的 last_seen_at
  8. [K8s controller] → refreshKubernetesControllerHeartbeat
  9. RecordPluginHealth (best-effort)
  10. 返回空 HeartbeatResponse{}

metricsLoop (每 10s):
  1. collector.CollectAll(ctx) — 采集所有源的指标
  2. 对每个 CollectorOutput, pushOne 分两路:

  [快速路径] push_host_metrics (8 字段):
    client.Call(MethodPushHostMetrics, PushHostMetricsRequest{
      EdgeID, Points[]{Ts, CPUPct, MemPct, Load1/5/15, NetRxBps, NetTxBps, DiskUsedPct}
    })
    → 云端: canonicalize → resolveDeviceID → MetricIngester.Push(deviceID, points)

  [富路径] push_prom_samples (开放集):
    client.Call(MethodPushPromSamples, PushPromSamplesRequest{
      EdgeID, Source, Samples[]{Name, Labels, Value, TsMs}
    })
    → 云端: canonicalize → resolveDeviceID
    → [source 前缀 k8s:] → PushKubernetes(clusterID, ...)
    → [其他] → Push(deviceID, source, samples) → Prom remote_write
```

### 2.4 bash_exec 端到端序列

```
云端发起 (LLM 工具调用):
  1. LLM 生成 host_bash tool_call, 参数: device_ids[] + cmd
  2. BashTool.InvokableRun:
     → 解析参数, 验证 batch
     → [写命令 + 写门控开启] → ProposeAndAwait 审批流程
     → runBatch 并发(每 device_id):
       a. resolver.LookupHostEdge(deviceID) → edge_id 解析
       b. 构造 BashExecRequest{Cmd, Timeout, Unrestricted}
       c. fbClient.Call(edgeID, MethodBashExec, body)

Frontier Broker 路由:
  3. svc.Call(transportID, "bash.exec", req) → edge 的 geminio End

Edge Agent 执行:
  4. MethodBashExec handler 被触发
  5. 解析 BashExecRequest
  6. [Unrestricted=true] → sandbox.ExecRaw(cmd) — 绕过 cmdpolicy
     [否则] → sandbox.Exec(cmd):
       → 命令解析(支持管道, 拒绝 redirects/&&/||/$())
       → 二进制白名单检查
       → 路径白名单检查
       → 网络主机白名单检查
  7. 返回 BashExecResponse{Allowed, Stdout, Stderr, ExitCode, DurationMs}

响应回传:
  8. JSON 响应原路返回: edge → geminio → frontier → frontierbound → BashTool
  9. 组装 BashBatchResponse{Cmd, SuccessCount, ErrorCount, Results[]} 返回 LLM
```

### 2.5 重连恢复机制

```
隧道断开:
  1. geminio RetryEnd 自动重拨 (指数退避)
  2. retryDelegate.EndReOnline 触发:
     → promotePendingConnection() — 提升新连接
     → fireReconnectCallbacks() — 异步触发业务回调
  3. Agent 的 OnReconnect 回调:
     → registerEdge() — 让新 manager 服务端绑定同一个 canonical edge_id
  4. [registerEdge 失败] → heartbeatLoop 下次 tick 重试
  5. [连续 5 次失败] → errTunnelStuck 终止 agent

路由失效回收 (recycleBrokenRoute):
  当 register_edge 或 heartbeat 返回特定错误时:
  ("no such rpc" / "mismatch clientID" / "edge binding not ready")
  → 关闭当前活跃连接 → 触发 RetryEnd 单线程重连
  → 避免在旧传输还活着时创建第二个 RetryEnd 的竞态
```

---

## 3. AI Agent 全链路协作

### 3.1 七层协作链概览

```
HTTP Handler → Service Layer (kernel switch) → ChatRuntime.Handle
  → Graph.BuildReActGraph → eino ReAct Agent (ChatModel ↔ ToolsNode)
  → Callback Chain (AlertDraftGuard → Persistence → SSE → Audit → Metrics → Budget)
  → LLM RoutingChatModel → Provider Client → OpenAI-compatible API
```

### 3.2 SSE 流式对话完整序列

```
前端:
  POST /api/v1/chat/sessions/{id}/messages/stream
  Accept: text/event-stream
  Body: { content, provider?, model?, mentions?, web_search_enabled?, locale? }

HTTP Handler (internal/manager/server/aiops/http.go):
  1. callerFromCtx(r.Context()) — 提取调用方身份
  2. parseID(r) — 解析 session ID
  3. json.Decode(&postMessageReq) — 解析请求体
  4. 构造 agent.RunOptions{Provider, Model, Mentions, WebSearchEnabled, Locale}
  5. 检查 http.Flusher 支持
  6. 写 SSE 头: Content-Type: text/event-stream, Cache-Control: no-cache, X-Accel-Buffering: no
  7. 写心跳帧: ": ok\n\n" + Flush
  8. 构造 emit 闭包: func(e agent.Event) { writeSSE(w, flusher, eventName(e.Type), eventPayload(id, e)) }
  9. 调用: h.svc.PostMessageStreamWithOpts(ctx, caller, id, req.Content, emit, opts)

Service Layer (internal/manager/service/aiops/service.go):
  10. runWithKernel:
      a. 内容校验 + GetSession 所有权检查
      b. HLD-021: ctx = context.WithoutCancel(ctx) — 脱离 HTTP 生命周期
      c. ctx, cancel = context.WithCancel(ctx) — 注册显式取消
      d. registerCancel(sess.ID, cancel)
      e. [kernel==graph] → runGraph(ctx, sess, content, emit, opts)
      f. [kernel==legacy] → legacyAgent.RunStreamWithOpts(ctx, ...)

  11. runGraph:
      a. 构造 graphEmit 闭包: translateRuntimeEvent(ev) → emit(翻译后的事件)
      b. 构造 chatruntime.Request{SessionID, UserID, Role, UserText, Mentions, Provider, Model, ...}
      c. runtime.Handle(ctx, req)

ChatRuntime.Handle (internal/manager/biz/aiops/chatruntime/runtime.go):
  12. 所有权检查: GetSession → sess.UserID != req.UserID → ErrNotFound
  13. @-mention 渲染: MentionResolver.Resolve → markdown bullet 前言
  14. 用户消息持久化: AppendMessage(role=user) — 崩溃前落盘不变量
  15. 加载历史: ListMessages(sess.ID, HistoryLimit=50)
  16. 技能解析 + 系统提示组装:
      a. SkillRegistry.Resolve(userText) → 活跃技能
      b. CredentialBinder.BoundCredentialNamesForSkills → HLD-017 凭证绑定
      c. AgentRegistry.ByName(personaName) → persona 解析
      d. filterToolsForAgentRole — viewer 剥离 Class!="read" 工具
      e. filterCoordinatorToolsForIntent — 按意图精简工具集
      f. ComposeSystemPrompt(basePrompt, activeSkills, persona)
  17. 构建 eino 历史: buildEinoHistory(history) → schema.Message[]
  18. 构建 ReAct 图: graph.BuildReActGraph(ChatModel, toolBag, graphCfg)
  19. 装配回调链: callbacks.NewDefaultHandlers(deps)
  20. 计算动态提示: calcDynamicHints(history) — 连续失败/重复调用/未兑现承诺
  21. 调用图: g.Invoke(ctx, &graph.Input{...}, invokeOpts...)

eino ReAct Agent 内部循环:
  22. MessageAssembler: 组装 system + history + system-reminder + user text
  23. ReActSubgraph 循环 (最多 MaxIterations 轮):
      a. ChatModel.Generate(messages) → assistant message (可能含 tool_calls)
      b. [有 tool_calls] → ToolsNode 并行执行工具
      c. 工具结果回喂 → 下一轮 ChatModel
      d. [无 tool_calls] → 收敛, 输出最终 assistant message
  24. 每个节点执行触发 callback handler 链

Callback Handler 链 (6 个 handler 按序触发):
  25. AlertDraftGuard.OnEnd(ChatModel):
      → 检查是否在创建告警规则但模型没调 draft_config_change
      → [是] → 将内容替换为拦截消息
  26. Persistence.OnEnd(ChatModel):
      → INSERT chat_messages (role=assistant)
      → store MessageID 到 assistantIDRelay (atomic.Pointer)
  27. Persistence.OnStart(Tool):
      → INSERT chat_tool_calls (status=pending)
  28. SSE.OnEnd(ChatModel):
      → 从 relay 读取 MessageID
      → emit SSEEvent(assistant_end, content, MessageID)
  29. SSE.OnStart(Tool):
      → emit SSEEvent(tool_start)
  30. SSE.OnEnd(Tool):
      → emit SSEEvent(tool_end, result)
  31. Audit.OnEnd: 记录 duration + token usage + tool_calls 数
  32. Metrics.OnEnd: ongrid_graph_chat_turns_total / ongrid_tool_invocations_total
  33. Budget.OnEnd: BudgetChecker.Record(usage)

事件翻译链 (4 层):
  34. eino callback → SSEEvent (callbacks/sse.go)
  35. SSEEvent → chatruntime.Event (runtime.go toCallbackEmitter)
      → assistant_start: 丢弃 (legacy SPA 无此帧)
      → assistant_delta: 丢弃 (token 级流式待支持)
      → assistant_end → EventAssistant
      → tool_start/end → EventToolStart/ToolEnd
  36. chatruntime.Event → agent.Event (service.go translateRuntimeEvent)
  37. agent.Event → writeSSE (http.go emit 闭包)
  38. writeSSE: "event: <type>\ndata: <json>\n\n" + Flush

OutputProjector:
  39. 提取最终 assistant message → graph.Output
  40. 构造 chatruntime.Reply{Message, Usage, Iterations, ToolCalls}

ChatRuntime.Handle 收尾:
  41. [错误] → buildGraphErrorApology → 持久化道歉消息 + emit
  42. [成功] → emit EventDone(Reply)
  43. defer FinalizeBatches(WithoutCancel ctx) — autoheal 残留工具批

前端消费:
  44. fetch + ReadableStream → TextDecoder → 以 \n\n 拆分 SSE 帧
  45. dispatchFrame: 解析 event: + data: → JSON.parse → 按类型分发回调
  46. onAssistant → 更新消息列表
  47. onToolStart → 创建工具卡片
  48. onToolEnd → 更新工具卡片状态
  49. onDone → 完成
```

### 3.3 Esc 停止机制

```
用户按 Esc:
  SPA: POST /api/v1/chat/sessions/{id}/stop
  → HTTP Handler: stopSession
  → Service.StopSession:
    a. 所有权检查
    b. cancelMu.Lock → 取出 cancel func → delete map → Unlock
    c. [无在飞 turn] → 返回 {stopped: false} (幂等)
    d. cancel() — 触发 ctx.Done()

取消传播:
  context.WithCancel 的 ctx.Done() 关闭
  → chatruntime.Handle 的 ctx 被取消
  → graph.Invoke 内部所有 ctx 检查点返回 Canceled
  → ChatModel 调用中断 / Tool 执行中断
  → graph 返回 invokeErr (context canceled)
  → buildGraphErrorApology → "本次请求超时或被取消..."
  → 持久化道歉消息 + emit EventAssistant + emit EventDone
  → defer FinalizeBatches — autoheal 残留工具批
```

### 3.4 工具袋两级延迟加载

```
tierByName 分类:
  core (始终全 schema, 26 个):
    get_host_load, query_promql, AgentTool, draft_config_change, ...
  specialty (超阈值时 redacted, 10 个):
    host_bash, rank_edges, host_find_large_files, ...

SchemasForLLM():
  [deferral off] → 全 schema
  [deferral on] → core(全) + extras(ToolSearch 全) + specialty(redacted)
    → redactedTool: Info() 返回空 schema + "call ToolSearch with query='select:NAME'"

运行时过滤:
  filterToolsForAgentRole:
    [viewer / write-disabled] → 剥离 Class!="read" 工具
  filterCoordinatorToolsForIntent:
    [metric 意图] → 只保留 query_promql + list_metric_catalog
    [log 意图] → 只保留 query_logql
    [复杂意图(root cause)] → 只保留控制工具 (AgentTool/SendMessage/TaskStop/ToolSearch)
```

---

## 4. 认证授权协作机制

### 4.1 登录与 JWT 签发序列

```
用户登录:
  POST /api/v1/auth/login {email, password}

IAM Handler (internal/iam/server/http.go):
  1. loginThrottle.check(IP, email) — IP 限 20次/5min, email 限 6次/15min
  2. svc.Login(email, password)

User Usecase (internal/iam/biz/user/usecase.go):
  3. 查用户 (GetByEmail)
  4. 校验状态 active
  5. verifyPassword: passwd.Verify(password, user.PasswordHash)
     → Argon2id + subtle.ConstantTimeCompare 防时序攻击
  6. issuePair:
     → 构造 Claims{UserID, Email, Role, IsSuperuser, RegisteredClaims{ExpiresAt}}
     → signer.SignAccess(Claims) — HMAC, accessTTL=15min
     → signer.SignRefresh(Claims) — HMAC, refreshTTL=168h/720h
  7. 返回 TokenPair{AccessToken, RefreshToken, ExpiresIn, Role, UserID}

Token 刷新:
  POST /api/v1/auth/refresh {refresh_token}
  1. signer.Verify(refresh_token) — 仅签名+过期校验, 不查 DB
  2. 查用户确保仍 active
  3. issuePair 签发新对
```

### 4.2 受保护路由认证授权链

```
请求: GET /api/v1/alerts/incidents
      Authorization: Bearer <access_token>

auth.Middleware(signer):
  1. extractBearer: 优先 Authorization 头, 回退 ?token= (WebSocket)
  2. signer.Verify(token):
     → HMAC 签名验证
     → 过期检查
     → 返回 Claims (不查 DB)
  3. 构造 tenantctx.Tenant{UserID, Email, Role, IsSuperuser}
  4. tenantctx.SetOnSlot — 镜像到外层可变 slot (审计中间件可见)
  5. tenantctx.With — 注入 context value (下游 handler 可见)

authzmw.Require("alert:incident", "read"):
  6. tenantctx.From(ctx) — 提取调用方身份
  7. [IsSuperuser] → 直接放行 (短路 Casbin, 防止损坏策略锁死管理员)
  8. [Authorizer == nil] → 放行 (遗留/测试布线)
  9. Authorizer.AllowAnyOrg(userID, "alert:incident", "read"):
     → 遍历用户所属的所有 org
     → casbin Enforce(uid, dom, "alert:incident", "read") per org
     → 首个 allow 即返回 true
  10. [全部 deny] → 403

Handler:
  11. tenantctx.From(ctx) → 获取 UserID/Role
  12. 业务逻辑执行
```

### 4.3 Casbin RBAC with Domains

```
Casbin 模型 (model.conf):
  r = sub, dom, obj, act                    — 请求四元组
  p = sub, dom, obj, act                    — 策略四元组
  g = _, _, _                                — 三元组角色绑定 (subject, role, domain)
  m = g(r.sub, p.sub, r.dom)                — 角色匹配
      && (p.dom == "*" || r.dom == p.dom)   — 域匹配
      && (p.obj == "*" || keyMatch(r.obj, p.obj)) — 对象匹配(支持通配)
      && (p.act == "*" || r.act == p.act)   — 动作匹配

硬编码角色策略矩阵:
  org_admin: 域内全权限 + 成员管理
  member:    read + write + device:shell:exec
  viewer:    仅 read, 无 shell 访问
  superuser: 全通配 (防御性纵深)

成员管理同步:
  AddOrUpdate:
    1. repo.Upsert(OrgMembership) — 写 DB
    2. authz.SyncMembership(userID, orgID, role) — 同步 casbin g 规则
       → 先删除旧 g 规则, 再添加新规则 (casbin 不隐式替换)
  Remove:
    1. repo.Delete(OrgMembership)
    2. authz.RevokeMembership(userID, orgID)
```

### 4.4 Tenant Context 双层存储

```
问题: 中间件嵌套时, 内层 r.WithContext() 产生新 ctx,
      外层的 r 引用不携带内层写入的值。

解决方案: 双层存储
  1. 不可变 context value (With / From):
     - 标准 context.WithValue
     - 内层中间件写入, 内层 handler 读取
  2. 可变 slot (WithSlot / SetOnSlot):
     - *slot 指针存在外层 context 中
     - 内层写入 *slot, 外层可读取
     - From 优先读 slot, 保证外层中间件拾取内层写入的值

协作序列:
  [外层] AuditMiddleware: r = r.WithContext(tenantctx.WithSlot(ctx))
  [内层] auth.Middleware:  r = r.WithContext(tenantctx.With(innerCtx, tenant))
                           tenantctx.SetOnSlot(r.Context(), tenant) — 写入外层 slot
  [外层] AuditMiddleware: tenantctx.From(r.Context()) → 优先读 slot → 拾取 tenant
```

---

## 5. 告警流水线协作

### 5.1 规则评估完整序列

```
PipelineEvaluator.tick (每 5min):

  1. refreshDeviceStalenessGauge — 更新 device_last_seen_seconds_ago Prom gauge

  2. evaluatePromQuery (metric_raw 规则):
     a. CachedRulesProvider.MetricRawRules() → 规则快照 (atomic.Pointer)
     b. 对每条规则: prom.Query(rule.Expr, now)
        → 表达式即谓词: PromQL 比较运算符 (up == 0, cpu_pct > 90) 过滤不匹配 series
     c. 解析返回 vector, 对每个 series:
        → 构造 FiringInput{DedupeKey: "pipeline:<rule_key>:<labelSetKey>"}
        → RecordFiring(FiringInput)
     d. 恢复扫描: 上一 tick 的 dedupeKey 不在本次结果 → SystemResolveIncident

  3. evaluateMetricAnomaly (异常检测):
     → zscore / mad 基线检测

  4. evaluateMetricForecast (线性预测):
     → predict_linear 预测未来值

  5. evaluateMetricBurnRate (SLO 燃烧率):
     → 多窗口 (5m/1h/6h/1d) 燃烧率检查

  6-7. evaluateTraceLatency / evaluateTraceErrorRate:
     → histogram_quantile / 错误率查询 spanmetrics

  8-9. evaluateLogMatch / evaluateLogVolume:
     → Loki count_over_time 查询
```

### 5.2 Incident 生命周期

```
RecordFiring (upsert 逻辑):
  1. 计算 dedupeKey (默认 host:<device_id>:<rule>, 或 pipeline 传入)
  2. 按 dedupeKey 查现有 incident
  3. 不存在 → CreateIncident(status=open, EventCount=1)
     存在且 resolved → ReopenIncident(清 silenced_until/resolved_at)
     存在其他状态 → BumpIncidentFiring(更新 last_fired_at, summary, value)
  4. 写 firing event (isNew → EventTypeFiring; isReopen → EventTypeReopened)
  5. matchSilence 检查静默状态
  6. [isNew && investigator != nil] → InvestigateAsync (异步 AI 根因分析)
  7. [isNew || isReopen] && workflowDispatcher != nil → OnAlertFired (触发自动化工作流)
```

### 5.3 通知投递六层门控

```
MaybeNotify (通知编排):

  门控 1: Silenced → 跳过 (静默匹配)
  门控 2: CooldownExceeded → 否 → RecordRepeatSuppressed("cooldown") 跳过
  门控 3: Dampening → 规则配置 NotifyWindowSeconds + NotifyMinFires 时,
          统计窗口内 firing 次数 < 阈值 → RecordRepeatSuppressed("dampened") 跳过
  门控 4: Inhibitor.Suppress → 高优先级 incident 抑制 → 写 inhibited event 跳过
          内置抑制规则:
            - edge_offline 抑制同设备所有其他 host-scope 告警
            - prom_ingest_fail 抑制所有 scrape_down 告警
  门控 5: resolveChannels:
          DBChannelResolver.ChannelsFor:
            [规则级钉定优先] → NotifyChannelIDs 非空 → 只匹配那些 ID
            [全局过滤] → severity ≥ floor + scope_type 匹配
            [回退] → 合成通道 (env-config, ID=0)
  门控 6: 逐通道发送:
          持久化通道 (ID>0):
            RecordDelivery(pending) → BuildSenderFromChannel → SendVia → FinishDelivery
          合成通道 (ID==0):
            Notifier.Send
          任一成功 → MarkNotified

RetryWorker.tick (每 30s):
  ListRetriableDeliveries → 逐行:
    退避未到 (attempt_count * backoffPerAttempt) → skip
    incident resolved → 标记 success
    channel disabled → 烧预算停止重试
    notifier.Send → 更新 delivery → write event → MarkNotified
```

### 5.4 告警到 IM 通知的桥接

```
BuildSenderFromChannel (根据通道类型构造 Sender):
  ChannelTypeSlack    → notify.NewSlackSender(webhook_url)
  ChannelTypeFeishu   → notify.NewFeishuSender(webhook_url, secret)
  ChannelTypeDingTalk → notify.NewDingTalkSender(webhook_url, secret)
  ChannelTypeWeCom    → notify.NewWeComSender(webhook_url)
  ChannelTypeTelegram → notify.NewTelegramSender(bot_token, chat_id)
  ChannelTypeWebhook  → notify.NewGenericWebhookSender(url)

注意: 告警通知走 notify.Sender 接口 (单向推送, 发完即走)
      IM Bridge 走 imbridge.Sender 接口 (双向对话, 支持 SendText + EditText)
      两者是独立的通知路径, 不共享接口
```

---

## 6. IM 桥接协作

### 6.1 入站消息处理序列

```
用户在飞书群发消息 "CPU 为什么这么高？"

Provider (飞书/钉钉/Slack/Telegram):
  1. webhook 或 stream 模式接收事件
  2. 解密解码 → InboundMessage{Text, AppID, ChatID, ThreadID, UserID}

Bridge.HandleInbound:
  3. 空文本 → 忽略
  4. 事件去重: dedupSet.seenOrAdd(provider:app_id:event_id) → 重复跳过
     (双代 map, 容量 2048, 保留 2048-4096 个最近 key)
  5. GetAppByAppID → 检查 app.Enabled
  6. FindThread(app_id, chat_id, thread_id):
     不存在 → EnsureSession + CreateThread (首次消息)
     /new/新会话/新建会话/重新开始 → EnsureSession + RotateThreadSession
     其他 → TouchThread(更新 LastSeenAt)
  7. 斜杠命令短路 → 发送确认 "已开启新会话" → 返回
  8. [sender 实现 MessageEditor] → sender.SendText("思考中…") → placeholder messageID
  9. newStreamEditor(sender, chatID, placeholder, locale)
  10. 追加语言指令: localeDirective → "(LANGUAGE: Respond in English...)"
  11. agent.StreamMessage(sessionID, userContent, emit):

AiopsServiceAdapter.StreamMessage:
  12. svc.PostMessageStreamWithOpts(ctx, caller, sessionID, content, emit, runOptions)
      → runOptions.Provider/Model 从 LLMDefaultProvider 获取集群默认
  13. agent 循环 → EventAssistant (文本块):

streamEditor.OnEvent:
  14. EventAssistant → 累积文本到 buf
      → shouldFlush (800ms 或 200 字符) → EditText(messageID, buf)
  15. EventDone → 强制 flush (确保最后一块落地)
  16. EventToolStart/ToolEnd/TaskNotification → 丢弃 (IM 中太吵)

  17. editor.Flush() → 确保最后一块文本落地
```

### 6.2 会话映射规则

```
映射 key = (im_app_id, im_chat_id, thread_id)
  → 一个群一个会话, 群内所有用户共享
  → 会话稳定: 无空闲自动轮换, 同一映射行永远指向同一 ongrid 会话
  → 重置: 用户发送 /new → 分配新会话 + 轮换映射指针
```

---

## 7. 知识库 RAG 协作

### 7.1 文档摄入序列

```
仓库同步 (POST /api/v1/knowledge/repos/{id}/sync):

  1. 解析仓库注册行
  2. [内置 vault] → materializeBuiltinVault (本地解压)
     [外部仓库] → buildGitAuthEnv (SSH identity / anonymous)
                → syncFastPath (fetch + reset --hard)
                或 syncAtomicReplace (clone 到 tmp → rename)
  3. scanRepoFiles: 遍历目录树
     → 过滤 .md/.txt/.rst
     → 跳过 .git/vendor/node_modules
  4. DeleteByFilter(source_type=repo, repo_id=id) — 清除旧点集
  5. 逐文件 splitForChunks:
     → chunkChars=2500 runes (非字节, 正确处理 CJK)
     → chunkOverlap=250 runes (跨块边界保留上下文)
     → maxChunksPerFile=256 (防止病态大文件)
     → Chunk 0 前置标题 (embedding 捕获"这是什么文档"信号)
  6. 批量 embed (batch=32):
     [OpenAI 兼容] → POST /v1/embeddings
     [本地 ONNX] → BAAI BGE 模型 (bge-small-zh-v1.5, dim=512)
  7. qdrantx.Upsert:
     → 每 chunk 一个 point
     → payload: {source_type, repo_id, path, path_prefixes, tags, chunk_index, id_alias, parent_url}
     → ID: md5(scope||url||chunk_index) 取前 8 字节 → uint64
```

### 7.2 RAG 搜索序列

```
Agent 提问 "DNS 解析失败怎么排查？"

Usecase.Search(query, opts{PathPrefix: "网络"}):
  1. embed.Embed([query]) → 查询向量
  2. 构造 MustMatch 过滤器:
     [path] → 精确匹配
     [path_prefixes] → 累积前缀匹配 (denormalized 数组 + keyword 索引)
  3. qdrantx.Search(collection, vector, opts{Limit: overFetch, MustMatch}):
     overFetch = limit * 5 (上限 200) — 补偿同文档多 chunk 占据顶部
     → cosine 相似度 top-K 搜索 + 服务端过滤
  4. 按 parent_url / url 去重 — 同一文档只保留最高分 chunk
  5. 截断到 limit
  6. 返回 []SearchHit{Doc, Score}

Agent:
  7. 将 top-K 文档内容注入 prompt
  8. LLM 生成回答 (引用知识库内容)
```

---

## 8. Flow 流程引擎协作

### 8.1 DAG 执行序列

```
用户触发流程 (POST /api/v1/flows/{id}/execute):

  1. flowUC.Execute(flowID, input):
     a. flowRepo.GetFlow(flowID) → GraphDef (节点 + 边定义)
     b. flowExec.NewGraph(GraphDef) → DAG 图
     c. graph.Execute(ctx, input):
        → 拓扑排序节点
        → 按依赖关系并行执行

  2. 节点执行 (Node.Execute):
     HTTPNode:
       → 发送 HTTP 请求到外部端点
       → 结果注入下游节点
     ConditionNode:
       → 表达式求值 (expr.Evaluate)
       → 选择分支
     LLMNode:
       → llmClient.Chat(messages)
       → 结果注入下游节点
     AlertNode:
       → alertUC.RecordFiring(...)
       → 创建告警
     ScriptNode:
       → runner.RunShell(script)
       → 结果注入下游节点

  3. 结果汇总:
     → FlowResult{Outputs, Duration, Status}
     → flowRepo.CreateRun(flowID, result) — 持久化执行记录

  4. [定时触发] Scheduler.Cron:
     → 按 cron 表达式定时触发流程
```

### 8.2 LLM 生成流程

```
用户描述需求 "当 CPU 超过 90% 时自动排查并通知"

GenerateFlow(prompt):
  1. 构造 system prompt (Flow DSL 规范 + 节点类型目录)
  2. llmClient.Chat([{system, user}])
  3. 解析 LLM 输出为 GraphDef JSON
  4. 验证 DAG 合法性 (无环、节点类型合法)
  5. 返回 GraphDef
```

---

## 9. 定时报告协作

### 9.1 报告生成与投递序列

```
Scheduler.Cron (按报告配置的 cron 表达式触发):

  1. reportRepo.ListDueReports() → 到期报告列表

  2. 对每个报告 Report.Generate:
     a. period.CalcPeriod(report.Period) → [from, to] 时间范围
     b. facts.Collect(ctx, from, to):
        → 查告警: alertUC.ListIncidents(from, to)
        → 查指标: metricUC.Query(expr, from, to)
        → 查拓扑: topologyUC.GetTopology()
        → 查设备: deviceUC.List()
     c. content.Generate(ctx, report, facts):
        → 构造 system prompt (报告模板 + 角色)
        → llmClient.Chat([{system, user=facts}])
        → 生成 markdown 报告内容
     d. task.Save(report, content) — 持久化报告任务

  3. delivery.Deliver(ctx, report, content):
     → 逐通道发送:
       [邮件] → SMTP 发送
       [IM] → imbridge 发送
       [Webhook] → notify.Sender 发送
     → 记录投递状态

  4. [失败] → 重试 (按 retry 策略)
```

---

## 10. 后台并发任务协作

### 10.1 errgroup 并发模型

```
rootCtx (SIGINT/SIGTERM 触发取消)
  └─ eg, egCtx := errgroup.WithContext(rootCtx)

  eg.Go(apiServer.Start(egCtx))           — HTTP API 服务 (端口 8090)
  eg.Go(metricsServer.Start(egCtx))       — Prometheus 指标服务 (端口 9090)
  eg.Go(DB pool sampler, 10s)             — 数据库连接池采样
  eg.Go(auditUC.RunRetention(egCtx))      — 审计日志保留清理
  eg.Go(runK8sEventRetention(egCtx))      — K8s 事件保留清理
  eg.Go(runK8sTopologyReconcile(egCtx))   — K8s 拓扑对账
  eg.Go(chatruntime worker sampler, 15s)  — Agent worker 状态采样
  eg.Go(RCA investigator inflight gauge)  — 根因分析并发度采样
  eg.Go(device presence reconciler, 60s)  — 设备在线状态对账
  eg.Go(alertRules.Loop(egCtx))           — 告警规则缓存刷新
  eg.Go(pipelineEval.Loop(egCtx))         — 告警规则评估 (每 5min)
  eg.Go(retryWorker.Loop(egCtx))          — 通知投递重试 (每 30s)

  eg.Wait() — 阻塞直到任一 goroutine 退出 → 触发 egCtx 取消 → 全部优雅关闭
```

### 10.2 优雅关闭序列

```
SIGINT/SIGTERM 接收:
  1. rootCtx 取消 → egCtx 取消
  2. apiServer.Start 收到 ctx.Done():
     → http.Server.Shutdown(ctx) — 等待在飞请求完成
     → 拒绝新连接
  3. metricsServer.Start 收到 ctx.Done():
     → http.Server.Shutdown(ctx)
  4. 后台 goroutine 收到 ctx.Done():
     → 保存当前状态 → 退出
  5. eg.Wait() 返回 → main 退出
```

---

## 11. 架构全景序列图

### 11.1 完整请求-响应序列

```
┌──────────┐     ┌──────────────┐     ┌─────────────────┐     ┌───────────┐
│  浏览器   │     │  chi Router  │     │  Service Layer  │     │  Biz Layer │
│          │     │  + Middleware │     │                 │     │           │
└────┬─────┘     └──────┬───────┘     └────────┬────────┘     └─────┬─────┘
     │                  │                      │                    │
     │  HTTP Request    │                      │                    │
     ├─────────────────>│                      │                    │
     │                  │                      │                    │
     │                  │ auth.Middleware(JWT)  │                    │
     │                  │ authzmw.Require(Casbin)                   │
     │                  │ tenantctx.With       │                    │
     │                  ├─────────────────────>│                    │
     │                  │                      │                    │
     │                  │                      │ biz.UseCase调用    │
     │                  │                      ├───────────────────>│
     │                  │                      │                    │
     │                  │                      │                    │ ├─ [Edge] fbClient.Call(edgeID, method, body)
     │                  │                      │                    │ │   → frontier → edge agent → 执行 → 响应
     │                  │                      │                    │ │
     │                  │                      │                    │ ├─ [LLM] llmClient.Chat(messages)
     │                  │                      │                    │ │   → RoutingChatModel → provider → OpenAI API
     │                  │                      │                    │ │
     │                  │                      │                    │ ├─ [DB] repo.Query(ctx, ...)
     │                  │                      │                    │ │   → GORM → MySQL/SQLite
     │                  │                      │                    │ │
     │                  │                      │                    │ ├─ [Prom] promQueryClient.Query(expr, ts)
     │                  │                      │                    │ │   → Prometheus HTTP API
     │                  │                      │                    │ │
     │                  │                      │                    │ ├─ [Loki] logQueryClient.Query(query, range)
     │                  │                      │                    │ │   → Loki HTTP API
     │                  │                      │                    │ │
     │                  │                      │                    │ ├─ [Tempo] traceQueryClient.Query(query, range)
     │                  │                      │                    │ │   → Tempo HTTP API
     │                  │                      │                    │ │
     │                  │                      │                    │ ├─ [Qdrant] qdrantClient.Search(vector, filter)
     │                  │                      │                    │ │   → Qdrant REST API
     │                  │                      │                    │ │
     │                  │                      │                    │ ├─ [Grafana] grafanaClient.CreateDashboard(...)
     │                  │                      │                    │ │   → Grafana Admin API
     │                  │                      │                    │ │
     │                  │                      │                    │ └─ [Notify] notifyRouter.Send(msg, channels...)
     │                  │                      │                    │     → Slack/飞书/钉钉/企微/Telegram/Webhook
     │                  │                      │                    │
     │                  │                      │<───────────────────┤
     │                  │                      │                    │
     │                  │<─────────────────────┤                    │
     │                  │                      │                    │
     │  HTTP Response   │                      │                    │
     │<─────────────────┤                      │                    │
     │                  │                      │                    │
     │  [SSE] event: assistant\ndata: {...}\n\n                     │
     │  [SSE] event: tool_start\ndata: {...}\n\n                    │
     │  [SSE] event: tool_end\ndata: {...}\n\n                      │
     │  [SSE] event: done\ndata: {...}\n\n                          │
     │<─────────────────┤                      │                    │
     │                  │                      │                    │
```

### 11.2 AI Agent 全链路序列

```
浏览器          HTTP Handler      Service        ChatRuntime      eino Graph       Callback Chain
  │                 │                │                │               │                │
  │ POST stream     │                │                │               │                │
  ├────────────────>│                │                │               │                │
  │                 │ SSE 握手       │                │               │                │
  │←── ": ok\n\n" ─┤                │                │               │                │
  │                 │                │                │               │                │
  │                 │ PostMessageStreamWithOpts      │               │                │
  │                 ├───────────────>│                │               │                │
  │                 │                │ WithoutCancel  │               │                │
  │                 │                │ registerCancel │               │                │
  │                 │                │                │               │                │
  │                 │                │ Handle(req)    │               │                │
  │                 │                ├───────────────>│               │                │
  │                 │                │                │ 所有权检查     │                │
  │                 │                │                │ mention 渲染   │                │
  │                 │                │                │ 用户消息持久化  │                │
  │                 │                │                │ 历史加载       │                │
  │                 │                │                │ 技能+persona  │                │
  │                 │                │                │ 工具过滤       │                │
  │                 │                │                │ 图构建         │                │
  │                 │                │                │ 回调装配       │                │
  │                 │                │                │               │                │
  │                 │                │                │ g.Invoke()    │                │
  │                 │                │                ├──────────────>│                │
  │                 │                │                │               │                │
  │                 │                │                │               │ ChatModel.Generate
  │                 │                │                │               │────┐           │
  │                 │                │                │               │    │ OpenAI API│
  │                 │                │                │               │<───┘           │
  │                 │                │                │               │                │
  │                 │                │                │               │ OnEnd(ChatModel)│
  │                 │                │                │               ├───────────────>│
  │                 │                │                │               │                │ AlertDraftGuard
  │                 │                │                │               │                │ → Persistence(INSERT assistant)
  │                 │                │                │               │                │ → SSE(emit assistant_end)
  │                 │                │                │               │                │ → Audit(log)
  │                 │                │                │               │                │ → Metrics(counter)
  │                 │                │                │               │                │ → Budget(record)
  │                 │                │                │               │<───────────────┤
  │                 │                │                │               │                │
  │                 │                │                │               │ [有 tool_calls]│
  │                 │                │                │               │ ToolsNode执行  │
  │                 │                │                │               │────┐           │
  │                 │                │                │               │    │ fbClient   │
  │                 │                │                │               │    │ → edge     │
  │                 │                │                │               │<───┘           │
  │                 │                │                │               │                │
  │                 │                │                │               │ OnEnd(Tool)    │
  │                 │                │                │               ├───────────────>│
  │                 │                │                │               │                │ Persistence(UPDATE tool_call)
  │                 │                │                │               │                │ SSE(emit tool_end)
  │                 │                │                │               │                │ Audit(log)
  │                 │                │                │               │                │ Metrics(counter)
  │                 │                │                │               │<───────────────┤
  │                 │                │                │               │                │
  │                 │                │                │               │ [收敛] OutputProjector
  │                 │                │                │<──────────────┤                │
  │                 │                │                │ Reply         │                │
  │                 │                │                │ emit Done     │                │
  │                 │                │<───────────────┤               │                │
  │                 │                │                │               │                │
  │  event: assistant\ndata: {...}\n\n                │               │                │
  │←────────────────┤                │                │               │                │
  │  event: tool_start\ndata: {...}\n\n               │               │                │
  │←────────────────┤                │                │               │                │
  │  event: tool_end\ndata: {...}\n\n│                │               │                │
  │←────────────────┤                │                │               │                │
  │  event: done\ndata: {...}\n\n    │                │               │                │
  │←────────────────┤                │                │               │                │
  │                 │                │                │               │                │
  │  [Esc 停止]     │                │                │               │                │
  │  POST /stop     │                │                │               │                │
  ├────────────────>│ StopSession    │                │               │                │
  │                 ├───────────────>│ cancel()       │               │                │
  │                 │                ├───────────────>│ ctx.Done()    │                │
  │                 │                │                │ graph 中断    │                │
  │                 │                │                │ 道歉消息       │                │
  │                 │                │                │ emit Done     │                │
  │  event: assistant\ndata: {道歉}\n\n               │               │                │
  │←────────────────┤                │                │               │                │
  │  event: done    │                │                │               │                │
  │←────────────────┤                │                │               │                │
```

---

## 附录 A：技术组件索引

| 组件 | 文件路径 | 职责 |
|------|---------|------|
| **启动入口** | `cmd/ongrid/main.go` | 13 阶段依赖注入 + errgroup 并发 |
| **HTTP 服务器** | `internal/pkg/httpserver/server.go` | 优雅关闭 HTTP 服务 |
| **路由器** | chi router (第三方) | HTTP 路由 + 中间件链 |
| **JWT 认证** | `internal/pkg/auth/jwt.go`, `middleware.go` | JWT 签发/验证 + HTTP 中间件 |
| **Casbin 授权** | `internal/pkg/authzmw/middleware.go` | RBAC with domains 授权 |
| **IAM** | `internal/iam/` | 用户/组织/成员管理 |
| **LLM 客户端** | `internal/pkg/llm/` | 多 Provider 路由 + eino 适配 |
| **AI Agent** | `internal/manager/biz/aiops/` | 对话/工具/回调全链路 |
| **eino 框架** | cloudwego/eino (第三方) | ReAct Agent + compose.Graph |
| **边云隧道** | `internal/pkg/tunnel/`, `internal/manager/service/frontierbound/` | geminio 隧道 |
| **Edge Agent** | `cmd/ongrid-edge/`, `internal/edgeagent/` | 边端采集/执行/插件 |
| **告警** | `internal/manager/biz/alert/` | 规则评估/incident/通知 |
| **通知** | `internal/pkg/notify/` | 多通道通知 |
| **IM 桥接** | `internal/manager/biz/imbridge/` | 飞书/钉钉/Slack/Telegram |
| **知识库** | `internal/manager/biz/knowledge/` | RAG 文档摄入/搜索 |
| **向量嵌入** | `internal/pkg/embedding/` | OpenAI + 本地 ONNX |
| **向量数据库** | `internal/pkg/qdrantx/` | Qdrant REST 客户端 |
| **Flow 引擎** | `internal/manager/biz/flow/` | DAG 流程执行 |
| **定时报告** | `internal/manager/biz/report/` | LLM 报告生成 |
| **密钥保险库** | `internal/manager/biz/secret/` | AES-256-GCM 加密存储 |
| **Skill 框架** | `internal/skill/` | L2 设备能力框架 |
| **数据库** | `internal/pkg/dbx/` | MySQL/SQLite + GORM |
| **Prometheus** | `internal/pkg/prom/` | 指标注册表 + 自观测 |
| **日志** | `internal/pkg/logger/` | 结构化 JSON + trace_id |
| **追踪** | `internal/pkg/tracing/` | OpenTelemetry OTLP |
| **加密** | `internal/pkg/secretbox/` | AES-256-GCM |
| **密码** | `internal/pkg/passwd/` | Argon2id |
| **审计** | `internal/manager/biz/audit/` | 操作审计日志 |
| **审批** | `internal/manager/biz/approval/` | 变更审批流 |
| **MCP** | `internal/pkg/mcpclient/`, `internal/manager/biz/mcp/` | MCP 服务器管理 |
| **Grafana** | `internal/pkg/grafana/` | Grafana 自动配置 |
| **Prom 查询** | `internal/pkg/promquery/` | Prometheus HTTP API |
| **Prom 写入** | `internal/pkg/promwrite/` | Prometheus remote_write |
| **Loki 查询** | `internal/pkg/logquery/` | Loki LogQL 查询 |
| **Tempo 查询** | `internal/pkg/tracequery/` | Tempo TraceQL 查询 |
| **WebShell** | `internal/manager/biz/webshell/` | WebSocket SSH 终端 |
| **市场** | `internal/manager/biz/marketplace/` | 技能/Agent 市场 |
| **系统设置** | `internal/manager/biz/setting/` | LLM 配置/探测/遥测 |
| **设备管理** | `internal/manager/biz/device/` | 设备注册表 |
| **边端管理** | `internal/manager/biz/edge/` | 边端节点管理 |
| **K8s 管理** | `internal/manager/biz/k8s/` | K8s 集群管理 |
| **拓扑** | `internal/manager/biz/topology/` | 服务依赖关系图 |
| **监控面板** | `internal/manager/biz/monitor/` | 用户面板 |
| **指标** | `internal/manager/biz/metric/` | 指标查询/写入/降采样 |
