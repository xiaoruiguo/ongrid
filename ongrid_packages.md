# OnGrid 包分析文档

> 本文档分析 OnGrid 项目中每个包的用途、代码实现和包含的函数功能。
> 生成时间：2026-08-03

---

## 目录

1. [cmd/ — 二进制入口](#1-cmd--二进制入口)
2. [internal/pkg/ — 公共基础包](#2-internalpkg--公共基础包)
3. [internal/iam/ — IAM 身份与访问管理](#3-internaliam--iam-身份与访问管理)
4. [internal/manager/biz/ — 云端业务逻辑层](#4-internalmanagerbiz--云端业务逻辑层)
5. [internal/manager/server/ — HTTP 路由层](#5-internalmanagerserver--http-路由层)
6. [internal/manager/service/ — 服务门面层](#6-internalmanagerservice--服务门面层)
7. [internal/manager/data/ — 数据持久化层](#7-internalmanagerdata--数据持久化层)
8. [internal/manager/model/ — 领域模型层](#8-internalmanagermodel--领域模型层)
9. [internal/edgeagent/ — 边端 Agent](#9-internaledgeagent--边端-agent)
10. [internal/skill/ — Skill 框架](#10-internalskill--skill-框架)
11. [web/src/ — 前端](#11-websrc--前端)

---

## 1. cmd/ — 二进制入口

### 1.1 cmd/ongrid — 云端管理器主程序

**文件**: `cmd/ongrid/main.go` (~5530 行)

**用途**: OnGrid 云端管理器主二进制，包含所有依赖注入、适配器组装和启动逻辑。

**核心类型**:

| 类型 | 行号 | 说明 |
|------|------|------|
| `Config` | ~L80 | 全局配置结构体，包含数据库、Redis、JWT、Ollama、Grafana 等所有配置项 |
| `app` | ~L150 | 应用容器，持有所有 service/repo/biz 实例 |
| `sqliteSessionRepo` | ~L200 | chat_sessions/messages/tool_calls 的 SQLite 持久化 |
| `sqliteAlertRepo` | ~L300 | alert 规则与状态的 SQLite 持久化 |
| `sqliteDeviceRepo` | ~L400 | device 注册表的 SQLite 持久化 |
| `sqliteEdgeRepo` | ~L500 | edge 节点的 SQLite 持久化 |
| `sqliteFlowRepo` | ~L600 | flow DAG 定义的 SQLite 持久化 |
| `sqliteApprovalRepo` | ~L700 | approval 审批流的 SQLite 持久化 |
| `sqliteK8sRepo` | ~L800 | K8s 集群注册表的 SQLite 持久化 |
| `sqliteSecretRepo` | ~L900 | 密钥保险库的 SQLite 持久化 |
| `sqliteSettingRepo` | ~L1000 | 系统设置的 SQLite 持久化 |
| `sqliteReportRepo` | ~L1100 | 定时报告的 SQLite 持久化 |
| `sqliteAuditRepo` | ~L1200 | 审计日志的 SQLite 持久化 |
| `sqliteKnowledgeRepo` | ~L1300 | 知识库的 SQLite 持久化 |
| `sqliteMCPRepo` | ~L1400 | MCP 服务器注册表的 SQLite 持久化 |
| `sqliteMarketplaceRepo` | ~L1500 | 技能市场的 SQLite 持久化 |
| `sqliteMonitorRepo` | ~L1600 | 监控面板的 SQLite 持久化 |
| `sqliteTopologyRepo` | ~L1700 | 拓扑图的 SQLite 持久化 |
| `sqliteMetricRepo` | ~L1800 | 指标查询的 SQLite 持久化 |
| `sqliteIMBridgeRepo` | ~L1900 | IM 桥接的 SQLite 持久化 |
| `sqliteMutatingProposalRepo` | ~L2000 | 变更提案审计的 SQLite 持久化 |
| `sqliteGrafanaRepo` | ~L2100 | Grafana 配置的 SQLite 持久化 |
| `sqliteWebshellRepo` | ~L2200 | WebShell 会话的 SQLite 持久化 |
| `sqlitePromWriteRepo` | ~L2300 | Prometheus Remote-Write 的 SQLite 持久化 |

**核心函数**:

| 函数 | 行号 | 签名 | 说明 |
|------|------|------|------|
| `main` | ~L1 | `func main()` | 入口，调用 `run()` |
| `run` | ~L20 | `func run() error` | 初始化配置、数据库、IAM、Manager、HTTP 服务器 |
| `buildAIOpsRuntime` | ~L3917 | `func buildAIOpsRuntime(...) (*chatruntime.Runtime, error)` | 组装 eino 图内核：ChatModel + ToolBag + Callback 链 |
| `newMultiClient` | ~L4100 | `func newMultiClient(cfg Config, resolver *llm.Resolver) *llm.MultiClient` | 构建 LLM 多供应商客户端 |
| `newRoutingChatModel` | ~L4200 | `func newRoutingChatModel(mc *llm.MultiClient) *llm.RoutingChatModel` | 构建 eino ChatModel 路由器 |
| `buildToolBag` | ~L4300 | `func buildToolBag(...) *tools.ToolBag` | 组装 AI 工具集（内置 + Skill + HostFiles） |
| `buildCallbackChain` | ~L4400 | `func buildCallbackChain(...) []callbacks.Handler` | 组装回调链（AlertDraftGuard → Persistence → SSE → Audit → Metrics → Budget） |
| `newGrafanaService` | ~L4500 | `func newGrafanaService(...) *grafana.Service` | 构建 Grafana 自动配置服务 |
| `newIMBridgeUsecase` | ~L4600 | `func newIMBridgeUsecase(...) *imbridge.Usecase` | 构建 IM 桥接用例 |
| `newFlowEngine` | ~L4700 | `func newFlowEngine(...) *flow.Engine` | 构建 DAG 流程引擎 |
| `newReportScheduler` | ~L4800 | `func newReportScheduler(...) *report.Scheduler` | 构建定时报告调度器 |
| `newAlertPipeline` | ~L4900 | `func newAlertPipeline(...) *alert.Pipeline` | 构建告警流水线 |
| `newSettingService` | ~L5000 | `func newSettingService(...) *setting.Service` | 构建设置服务 |
| `newEdgeUsecase` | ~L5100 | `func newEdgeUsecase(...) *edge.Usecase` | 构建边端管理用例 |
| `newK8sUsecase` | ~L5200 | `func newK8sUsecase(...) *k8s.Usecase` | 构建 K8s 管理用例 |
| `newMarketplaceUsecase` | ~L5300 | `func newMarketplaceUsecase(...) *marketplace.Usecase` | 构建技能市场用例 |
| `newKnowledgeUsecase` | ~L5400 | `func newKnowledgeUsecase(...) *knowledge.Usecase` | 构建知识库用例 |

### 1.2 cmd/ongrid-edge — 边端 Agent 主程序

**文件**: `cmd/ongrid-edge/main.go` (862 行)

**用途**: OnGrid 边端 Agent 二进制，负责 K8s 注册、隧道连接、采集器、插件管理。

**核心类型**:

| 类型 | 行号 | 说明 |
|------|------|------|
| `Config` | L25 | 边端配置：云端地址、隧道 Token、采集模式、插件列表 |
| `edgeApp` | L80 | 边端应用容器 |

**核心函数**:

| 函数 | 行号 | 签名 | 说明 |
|------|------|------|------|
| `main` | L1 | `func main()` | 入口 |
| `run` | L15 | `func run() error` | 初始化配置、隧道、采集器、插件、K8s 集成 |
| `newCollector` | L200 | `func newCollector(cfg Config, pushFn func(...)) collector.Collector` | 构建双模式采集器（嵌入式 + 抓取式） |
| `newPluginSupervisor` | L300 | `func newPluginSupervisor(cfg Config, tunnel *tunnel.Client) (*plugins.Supervisor, error)` | 构建插件监控器 |
| `registerTunnelHandlers` | L400 | `func registerTunnelHandlers(app *edgeApp)` | 注册隧道消息处理器（bash、文件、服务重启等） |
| `startK8sIntegration` | L500 | `func startK8sIntegration(app *edgeApp)` | 启动 K8s 集成（身份注册、指标网关、升级准备） |

**辅助文件**:

| 文件 | 行数 | 说明 |
|------|------|------|
| `k8s_credentials.go` | 516 | K8s Secret 凭证管理：读取/写入/轮换 Secret |
| `k8s_data_plane.go` | 514 | K8s 遥测网关：指标抓取模式、推送模式 |
| `k8s_host_runtime.go` | 235 | K8s 宿主命名空间入口：unshare/chroot 进入宿主 |
| `k8s_upgrade.go` | 44 | K8s 升级准备：下载新版本、准备替换 |

### 1.3 cmd/mytest — 测试工具

**文件**: `cmd/mytest/main.go` (60 行)

**用途**: Ollama 流式输出测试工具。

**核心函数**:

| 函数 | 行号 | 签名 | 说明 |
|------|------|------|------|
| `main` | L1 | `func main()` | 连接 Ollama，发送流式请求，打印每个 token |

### 1.4 cmd/ollama — Ollama SSE 聊天服务

**文件**: `cmd/ollama/main.go` (151 行)

**用途**: 独立的 Ollama SSE 聊天 Web 服务，带 React 前端。

**核心函数**:

| 函数 | 行号 | 签名 | 说明 |
|------|------|------|------|
| `main` | L1 | `func main()` | 启动 HTTP 服务，提供 SSE 聊天接口 |
| `handleChat` | L50 | `func handleChat(w http.ResponseWriter, r *http.Request)` | SSE 聊天处理器，流式返回 Ollama 响应 |
| `handleIndex` | L130 | `func handleIndex(w http.ResponseWriter, r *http.Request)` | 返回内嵌的 React 前端页面 |

---

## 2. internal/pkg/ — 公共基础包

### 2.1 auth — JWT 认证

**文件**: `internal/pkg/auth/jwt.go`, `internal/pkg/auth/middleware.go`

**用途**: JWT 签发/验证 + HTTP 认证中间件。

**核心类型**:

| 类型 | 文件 | 说明 |
|------|------|------|
| `Signer` | jwt.go | JWT 签发器，持有 secret 和 TTL 配置 |
| `Claims` | jwt.go | JWT 载荷：UserID, Role, OrgID, RegisteredClaims |

**核心函数**:

| 函数 | 文件 | 签名 | 说明 |
|------|------|------|------|
| `NewSigner` | jwt.go | `func NewSigner(secret string, accessTTL, refreshTTL time.Duration) *Signer` | 构建签发器 |
| `Sign` | jwt.go | `func (s *Signer) Sign(claims Claims) (string, error)` | 签发 access token |
| `SignRefresh` | jwt.go | `func (s *Signer) SignRefresh(claims Claims) (string, error)` | 签发 refresh token |
| `Verify` | jwt.go | `func (s *Signer) Verify(tokenStr string) (*Claims, error)` | 验证 token |
| `NewMiddleware` | middleware.go | `func NewMiddleware(signer *Signer) func(http.Handler) http.Handler` | 构建认证中间件 |

### 2.2 authzmw — Casbin 授权中间件

**文件**: `internal/pkg/authzmw/middleware.go`

**用途**: HTTP 授权中间件，基于 Casbin RBAC with domains。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `NewMiddleware` | `func NewMiddleware(enforcer *casbin.Enforcer) func(http.Handler) http.Handler` | 构建授权中间件，从 context 提取四元组 (sub, dom, obj, act) |

### 2.3 config — 环境变量配置

**文件**: `internal/pkg/config/config.go`

**用途**: MVP 风格配置，`os.Getenv` + 默认值，无 YAML/Viper 依赖。

**核心类型**:

| 类型 | 说明 |
|------|------|
| `Config` | 全局配置结构体，包含 DB/Redis/JWT/SMTP/Ollama/Grafana/Telemetry 等所有配置项 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `Load` | `func Load() *Config` | 从环境变量加载配置，缺失 JWT_SECRET 致命退出 |

### 2.4 creddinject — 凭证注入模板解析

**文件**: `internal/pkg/creddinject/creddinject.go`

**用途**: 将模板字符串中的 `${VAULT:credential_name}` 占位符替换为保险库中的实际值。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `Inject` | `func Inject(ctx context.Context, tpl string, resolver func(context.Context, string) (string, error)) (string, error)` | 解析模板并注入凭证值 |

### 2.5 dbx — 数据库辅助

**文件**: `internal/pkg/dbx/dbx.go`, `internal/pkg/dbx/migrate.go`, `internal/pkg/dbx/soft_delete.go`

**用途**: 数据库连接管理、迁移、软删除支持。

**核心函数**:

| 函数 | 文件 | 签名 | 说明 |
|------|------|------|------|
| `OpenSQLite` | dbx.go | `func OpenSQLite(dsn string) (*gorm.DB, error)` | 打开 SQLite 连接（WAL 模式） |
| `OpenMySQL` | dbx.go | `func OpenMySQL(dsn string) (*gorm.DB, error)` | 打开 MySQL 连接 |
| `RunMigrations` | migrate.go | `func RunMigrations(db *gorm.DB, fs embed.FS, dir string) error` | 运行嵌入式迁移文件 |
| `SoftDelete` | soft_delete.go | `func SoftDelete(db *gorm.DB, model interface{}, id uint64) error` | 软删除（设置 deleted_at） |

### 2.6 docextract — 文档文本提取

**文件**: `internal/pkg/docextract/extract.go`

**用途**: 从 Markdown/PDF/DOCX 文件中提取纯文本，用于知识库 RAG。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `Extract` | `func Extract(ctx context.Context, r io.Reader, filename string) (string, error)` | 根据文件扩展名选择提取器 |
| `ExtractPDF` | `func ExtractPDF(ctx context.Context, r io.Reader) (string, error)` | PDF 文本提取 |
| `ExtractDOCX` | `func ExtractDOCX(ctx context.Context, r io.Reader) (string, error)` | DOCX 文本提取 |

### 2.7 embedding — 向量嵌入

**文件**: `internal/pkg/embedding/embedding.go`, `internal/pkg/embedding/local.go`

**用途**: 文本向量嵌入，支持 OpenAI API 和本地 ONNX 模型。

**核心类型**:

| 类型 | 文件 | 说明 |
|------|------|------|
| `OpenAIEmbedding` | embedding.go | OpenAI Embedding API 客户端 |
| `LocalEmbedding` | local.go | 本地 ONNX Embedding 模型 |

**核心函数**:

| 函数 | 文件 | 签名 | 说明 |
|------|------|------|------|
| `NewOpenAIEmbedding` | embedding.go | `func NewOpenAIEmbedding(apiKey, model, baseURL string) *OpenAIEmbedding` | 构建 OpenAI 嵌入客户端 |
| `Embed` | embedding.go | `func (e *OpenAIEmbedding) Embed(ctx context.Context, texts []string) ([][]float32, error)` | 批量嵌入 |
| `NewLocalEmbedding` | local.go | `func NewLocalEmbedding(modelPath string) (*LocalEmbedding, error)` | 构建本地 ONNX 嵌入模型 |
| `Embed` | local.go | `func (e *LocalEmbedding) Embed(ctx context.Context, texts []string) ([][]float32, error)` | 批量嵌入 |

### 2.8 errs — 哨兵错误

**文件**: `internal/pkg/errs/errs.go`

**用途**: 统一哨兵错误定义 + HTTP 状态码映射。

**核心变量**:

| 变量 | 说明 |
|------|------|
| `ErrNotFound` | 资源不存在 → 404 |
| `ErrForbidden` | 权限不足 → 403 |
| `ErrUnauthorized` | 未认证 → 401 |
| `ErrInvalid` | 参数无效 → 400 |
| `ErrConflict` | 冲突 → 409 |
| `ErrNotWiredYet` | 功能未启用 → 503 |
| `ErrBudgetExceeded` | 预算超限 → 429 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `HTTPStatus` | `func HTTPStatus(err error) int` | 从错误链提取 HTTP 状态码 |

### 2.9 grafana — Grafana 管理 API 客户端

**文件**: `internal/pkg/grafana/client.go`

**用途**: Grafana 管理 API 客户端，用于自动创建数据源、仪表盘、用户。

**核心类型**:

| 类型 | 说明 |
|------|------|
| `Client` | Grafana Admin API 客户端 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `NewClient` | `func NewClient(baseURL, adminUser, adminPass string) *Client` | 构建客户端 |
| `CreateDataSource` | `func (c *Client) CreateDataSource(ctx context.Context, ds DataSource) error` | 创建数据源 |
| `CreateOrgUser` | `func (c *Client) CreateOrgUser(ctx context.Context, orgID int, user GrafanaUser) error` | 创建组织用户 |
| `CreateDashboard` | `func (c *Client) CreateDashboard(ctx context.Context, orgID int, dashboard json.RawMessage) error` | 创建仪表盘 |

### 2.10 httpserver — 优雅关闭 HTTP 服务器

**文件**: `internal/pkg/httpserver/server.go`

**用途**: 支持 graceful shutdown 的 HTTP 服务器封装。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `ListenAndServe` | `func ListenAndServe(srv *http.Server, logger *slog.Logger) error` | 启动服务，监听 SIGINT/SIGTERM 优雅关闭 |

### 2.11 k8sredact — K8s 凭证脱敏

**文件**: `internal/pkg/k8sredact/redact.go`

**用途**: K8s Secret 值的脱敏处理，防止敏感信息泄露到日志。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `Redact` | `func Redact(data map[string][]byte) map[string]string` | 将 Secret 值替换为 `***REDACTED***` |

### 2.12 llm — LLM 多供应商客户端

**文件**: `internal/pkg/llm/client.go`, `router.go`, `eino_routing.go`, `budget.go`, `budget_callback.go`, `metrics.go`, `noop.go`, `probe.go`

**用途**: LLM 多供应商路由、eino ChatModel 适配、预算门控、探测。

**核心类型**:

| 类型 | 文件 | 说明 |
|------|------|------|
| `MultiClient` | client.go | 多供应商 LLM 客户端，7 种供应商（OpenAI/Azure/DeepSeek/Ollama/Zhipu/Moonshot/SiliconFlow） |
| `Resolver` | client.go | 三级凭证缓存（运行时可变 → 启动固定 → 首次引导种子），60s TTL |
| `RoutingChatModel` | eino_routing.go | eino `model.ToolCallingChatModel` 接口实现，按 provider 路由到内部 ChatModel |
| `clientChatModel` | eino_routing.go | 单供应商 eino ChatModel 适配器 |
| `budgetStopModel` | budget.go | 预算门控装饰器，超预算时返回停止信号 |
| `BudgetCallback` | budget_callback.go | eino callback 预算追踪 |
| `Metrics` | metrics.go | LLM 调用指标（延迟/Token 用量/错误率） |
| `NoopChatModel` | noop.go | 空操作 ChatModel（测试用） |

**核心函数**:

| 函数 | 文件 | 签名 | 说明 |
|------|------|------|------|
| `NewMultiClient` | client.go | `func NewMultiClient(resolver *Resolver) *MultiClient` | 构建多供应商客户端 |
| `Generate` | client.go | `func (mc *MultiClient) Generate(ctx context.Context, provider, model string, msgs []*schema.Message, opts ...model.Option) (*schema.Message, error)` | 非流式生成 |
| `Stream` | client.go | `func (mc *MultiClient) Stream(ctx context.Context, provider, model string, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error)` | 流式生成 |
| `NewResolver` | client.go | `func NewResolver(db *gorm.DB, envConfig Config) *Resolver` | 构建凭证解析器 |
| `Resolve` | client.go | `func (r *Resolver) Resolve(ctx context.Context, provider string) (*ProviderConfig, error)` | 解析供应商凭证 |
| `NewRoutingChatModel` | eino_routing.go | `func NewRoutingChatModel(mc *MultiClient) *RoutingChatModel` | 构建 eino 路由 ChatModel |
| `Probe` | probe.go | `func Probe(ctx context.Context, provider, baseURL, apiKey, model string) error` | 探测供应商可达性（20s 超时） |
| `normalizeOpenAIBaseURL` | client.go | `func normalizeOpenAIBaseURL(u string) string` | 自动追加 `/v1` 到裸地址 |

### 2.13 logger — 结构化日志

**文件**: `internal/pkg/logger/logger.go`

**用途**: 结构化 JSON 日志（slog），支持 trace_id 注入。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `New` | `func New(level string) *slog.Logger` | 构建 JSON 日志器 |
| `WithTraceID` | `func WithTraceID(ctx context.Context, logger *slog.Logger) *slog.Logger` | 注入 trace_id |

### 2.14 logquery — Loki 查询客户端

**文件**: `internal/pkg/logquery/client.go`

**用途**: Grafana Loki LogQL 查询客户端。

**核心类型**:

| 类型 | 说明 |
|------|------|
| `Client` | Loki HTTP API 客户端 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `NewClient` | `func NewClient(baseURL string, opts ...Option) *Client` | 构建客户端 |
| `Query` | `func (c *Client) Query(ctx context.Context, query string, start, end time.Time, limit int) ([]LogEntry, error)` | 执行 LogQL 查询 |

### 2.15 mcpclient — MCP 客户端

**文件**: `internal/pkg/mcpclient/client.go`

**用途**: MCP (Model Context Protocol) Streamable-HTTP 客户端。

**核心类型**:

| 类型 | 说明 |
|------|------|
| `Client` | MCP Streamable-HTTP 客户端 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `NewClient` | `func NewClient(baseURL string, opts ...Option) *Client` | 构建客户端 |
| `ListTools` | `func (c *Client) ListTools(ctx context.Context) ([]Tool, error)` | 列出可用工具 |
| `CallTool` | `func (c *Client) CallTool(ctx context.Context, name string, args map[string]interface{}) (*ToolResult, error)` | 调用工具 |

### 2.16 notify — 多通道通知

**文件**: `internal/pkg/notify/notify.go`, `internal/pkg/notify/webhook.go`

**用途**: 多通道通知发送（Webhook/Slack/飞书/钉钉/企微/Telegram）。

**核心类型**:

| 类型 | 说明 |
|------|------|
| `Notifier` | 通知器接口 |
| `WebhookNotifier` | webhook.go | 通用 Webhook 通知器 |
| `SlackNotifier` | notify.go | Slack 通知器 |
| `FeishuNotifier` | notify.go | 飞书通知器 |
| `DingTalkNotifier` | notify.go | 钉钉通知器 |
| `WeComNotifier` | notify.go | 企微通知器 |
| `TelegramNotifier` | notify.go | Telegram 通知器 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `NewNotifier` | `func NewNotifier(channel string, cfg ChannelConfig) (Notifier, error)` | 按通道类型构建通知器 |
| `Send` | `func (n Notifier) Send(ctx context.Context, title, body string) error` | 发送通知 |

### 2.17 passwd — Argon2id 密码哈希

**文件**: `internal/pkg/passwd/argon2.go`

**用途**: Argon2id 密码哈希与验证，PHC 自描述编码。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `Hash` | `func Hash(password string) (string, error)` | Argon2id 哈希（64 MiB 内存，PHC 编码） |
| `Verify` | `func Verify(password, encoded string) (bool, error)` | 验证密码（`subtle.ConstantTimeCompare` 防时序攻击） |

### 2.18 prom — Prometheus 注册表

**文件**: `internal/pkg/prom/prom.go`, `internal/pkg/prom/manager_metrics.go`

**用途**: Prometheus 指标注册表 + Manager 自观测指标。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `NewRegistry` | `func NewRegistry() *prometheus.Registry` | 构建独立注册表 |
| `NewManagerMetrics` | `func NewManagerMetrics(reg *prometheus.Registry) *ManagerMetrics` | 构建 Manager 指标集 |

### 2.19 promauth — TLS + 动态认证 HTTP 客户端

**文件**: `internal/pkg/promauth/client.go`

**用途**: 构建 Prometheus 远端认证 HTTP 客户端（TLS + Bearer Token / Basic Auth）。

**核心类型**:

| 类型 | 说明 |
|------|------|
| `ClientBuilder` | HTTP 客户端构建器 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `NewBuilder` | `func NewBuilder() *ClientBuilder` | 构建器 |
| `WithTLS` | `func (b *ClientBuilder) WithTLS(caCert, clientCert, clientKey []byte) *ClientBuilder` | 配置 TLS |
| `WithBearer` | `func (b *ClientBuilder) WithBearer(token string) *ClientBuilder` | 配置 Bearer Token |
| `Build` | `func (b *ClientBuilder) Build() (*http.Client, error)` | 构建 HTTP 客户端 |

### 2.20 promquery — Prometheus 查询客户端

**文件**: `internal/pkg/promquery/client.go`

**用途**: Prometheus HTTP API 查询客户端（instant + range）。

**核心类型**:

| 类型 | 说明 |
|------|------|
| `Client` | Prometheus API 客户端 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `NewClient` | `func NewClient(baseURL string, opts ...Option) *Client` | 构建客户端 |
| `Query` | `func (c *Client) Query(ctx context.Context, query string, ts time.Time) (model.Value, error)` | 即时查询 |
| `QueryRange` | `func (c *Client) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) (model.Value, error)` | 范围查询 |

### 2.21 promwrite — Prometheus Remote-Write 客户端

**文件**: `internal/pkg/promwrite/client.go`, `internal/pkg/promwrite/proto.go`

**用途**: Prometheus remote_write 客户端，手写 protobuf + snappy 压缩。

**核心类型**:

| 类型 | 说明 |
|------|------|
| `Client` | Remote-Write 客户端 |
| `TimeSeries` | proto.go | 时序数据 protobuf 编码 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `NewClient` | `func NewClient(endpoint string, opts ...Option) *Client` | 构建客户端 |
| `Write` | `func (c *Client) Write(ctx context.Context, series []TimeSeries) error` | 写入时序数据（protobuf + snappy） |

### 2.22 qdrantx — Qdrant 向量数据库客户端

**文件**: `internal/pkg/qdrantx/client.go`

**用途**: Qdrant REST API 客户端，用于 RAG 向量检索。

**核心类型**:

| 类型 | 说明 |
|------|------|
| `Client` | Qdrant REST API 客户端 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `NewClient` | `func NewClient(baseURL string, opts ...Option) *Client` | 构建客户端 |
| `Upsert` | `func (c *Client) Upsert(ctx context.Context, collection string, points []Point) error` | 插入/更新向量 |
| `Search` | `func (c *Client) Search(ctx context.Context, collection string, vector []float32, limit int) ([]ScoredPoint, error)` | 向量相似搜索 |

### 2.23 runner — 执行沙箱

**文件**: `internal/pkg/runner/runner.go`, `internal/pkg/runner/shell.go`

**用途**: 命令执行沙箱抽象，支持超时、工作目录、环境变量注入。

**核心类型**:

| 类型 | 说明 |
|------|------|
| `Runner` | 命令执行器 |
| `Result` | 执行结果（Stdout/Stderr/ExitCode/Error） |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `New` | `func New(opts ...Option) *Runner` | 构建执行器 |
| `Run` | `func (r *Runner) Run(ctx context.Context, cmd string, args ...string) (*Result, error)` | 执行命令 |
| `RunShell` | shell.go | `func (r *Runner) RunShell(ctx context.Context, script string) (*Result, error)` | 执行 Shell 脚本 |

### 2.24 secretbox — AES-256-GCM 加密

**文件**: `internal/pkg/secretbox/secretbox.go`

**用途**: AES-256-GCM 对称加密，用于密钥保险库存储。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `Seal` | `func Seal(key, plaintext []byte) ([]byte, error)` | 加密 |
| `Open` | `func Open(key, ciphertext []byte) ([]byte, error)` | 解密 |
| `GenerateKey` | `func GenerateKey() ([]byte, error)` | 生成 32 字节密钥 |

### 2.25 tenantctx — 请求租户上下文

**文件**: `internal/pkg/tenantctx/tenantctx.go`

**用途**: 在 context 中存取请求调用方身份（UserID/Role/OrgID）。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `WithCaller` | `func WithCaller(ctx context.Context, userID uint64, role, orgID string) context.Context` | 注入调用方身份 |
| `CallerFrom` | `func CallerFrom(ctx context.Context) (uint64, string, string)` | 提取调用方身份 |

### 2.26 tracequery — Tempo 追踪查询客户端

**文件**: `internal/pkg/tracequery/client.go`

**用途**: Grafana Tempo TraceQL 查询客户端。

**核心类型**:

| 类型 | 说明 |
|------|------|
| `Client` | Tempo HTTP API 客户端 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `NewClient` | `func NewClient(baseURL string, opts ...Option) *Client` | 构建客户端 |
| `Query` | `func (c *Client) Query(ctx context.Context, query string, start, end time.Time, limit int) ([]Trace, error)` | 执行 TraceQL 查询 |

### 2.27 tracing — OpenTelemetry 初始化

**文件**: `internal/pkg/tracing/tracing.go`

**用途**: OpenTelemetry Tracer 初始化（OTLP 导出）。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `InitTracer` | `func InitTracer(ctx context.Context, endpoint string) (func(context.Context) error, error)` | 初始化 Tracer，返回 shutdown 函数 |

### 2.28 tunnel — 边云隧道

**文件**: `internal/pkg/tunnel/client.go`, `messages.go`, `bash.go`, `host_files.go`, `restart_service.go`, `types.go`

**用途**: 基于 geminio 的边云隧道，支持 bash 执行、文件传输、服务重启。

**核心类型**:

| 类型 | 文件 | 说明 |
|------|------|------|
| `Client` | client.go | 隧道客户端（云端侧），管理到边端的连接 |
| `Message` | messages.go | 隧道消息封装 |
| `BashRequest` | bash.go | bash 执行请求 |
| `BashResponse` | bash.go | bash 执行响应 |
| `HostFilesRequest` | host_files.go | 文件传输请求 |
| `RestartServiceRequest` | restart_service.go | 服务重启请求 |

**核心函数**:

| 函数 | 文件 | 签名 | 说明 |
|------|------|------|------|
| `NewClient` | client.go | `func NewClient(addr string, opts ...Option) *Client` | 构建隧道客户端 |
| `Connect` | client.go | `func (c *Client) Connect(ctx context.Context) error` | 连接到边端 |
| `ExecBash` | bash.go | `func (c *Client) ExecBash(ctx context.Context, req BashRequest) (*BashResponse, error)` | 远程执行 bash |
| `ReadFile` | host_files.go | `func (c *Client) ReadFile(ctx context.Context, req HostFilesRequest) ([]byte, error)` | 读取远程文件 |
| `RestartService` | restart_service.go | `func (c *Client) RestartService(ctx context.Context, req RestartServiceRequest) error` | 重启远程服务 |

### 2.29 workspace — Agent 工作目录管理

**文件**: `internal/pkg/workspace/workspace.go`

**用途**: Agent 工作目录生命周期管理（创建/清理/隔离）。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `New` | `func New(baseDir string) *Workspace` | 构建工作目录管理器 |
| `Create` | `func (w *Workspace) Create(ctx context.Context, sessionID string) (string, error)` | 创建会话工作目录 |
| `Cleanup` | `func (w *Workspace) Cleanup(ctx context.Context, sessionID string) error` | 清理会话工作目录 |

### 2.30 zhipuauth — 智谱 JWT 签名

**文件**: `internal/pkg/zhipuauth/zhipuauth.go`

**用途**: 智谱 AI JWT 签名（HMAC-SHA256），用于 API 认证。

**核心类型**:

| 类型 | 说明 |
|------|------|
| `zhipuJWTTransport` | HTTP RoundTripper，自动注入 JWT Authorization 头 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `NewTransport` | `func NewTransport(apiKey string) http.RoundTripper` | 构建 JWT 签名 Transport |
| `generateToken` | `func generateToken(apiKey string, expSeconds int64) (string, error)` | 生成 JWT token |

---

## 3. internal/iam/ — IAM 身份与访问管理

### 3.1 biz/authz — Casbin 授权引擎

**文件**: `internal/iam/biz/authz/authz.go`

**用途**: Casbin enforcer 封装，RBAC with domains 四元组 (sub, dom, obj, act)。

**核心类型**:

| 类型 | 说明 |
|------|------|
| `Enforcer` | Casbin enforcer 封装 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `NewEnforcer` | `func NewEnforcer(db *gorm.DB) (*Enforcer, error)` | 构建 enforcer（从 DB 加载策略） |
| `Enforce` | `func (e *Enforcer) Enforce(sub, dom, obj, act string) (bool, error)` | 四元组授权检查 |
| `AddRoleForUser` | `func (e *Enforcer) AddRoleForUser(user, role, dom string) (bool, error)` | 为用户添加角色 |
| `DeleteRoleForUser` | `func (e *Enforcer) DeleteRoleForUser(user, role, dom string) (bool, error)` | 删除用户角色 |

### 3.2 biz/membership — 组织成员管理

**文件**: `internal/iam/biz/membership/usecase.go`

**用途**: 组织成员 CRUD + Casbin 策略同步。

**核心类型**:

| 类型 | 说明 |
|------|------|
| `Usecase` | 成员管理用例 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `AddMember` | `func (u *Usecase) AddMember(ctx context.Context, orgID, userID uint64, role string) error` | 添加成员（同步 Casbin 策略） |
| `RemoveMember` | `func (u *Usecase) RemoveMember(ctx context.Context, orgID, userID uint64) error` | 移除成员 |
| `UpdateRole` | `func (u *Usecase) UpdateRole(ctx context.Context, orgID, userID uint64, role string) error` | 更新成员角色 |
| `ListMembers` | `func (u *Usecase) ListMembers(ctx context.Context, orgID uint64) ([]*model.OrgMembership, error)` | 列出组织成员 |

### 3.3 biz/org — 组织管理

**文件**: `internal/iam/biz/org/usecase.go`

**用途**: 扁平组织 CRUD + 种子组织创建。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `CreateOrg` | `func (u *Usecase) CreateOrg(ctx context.Context, name string) (*model.Org, error)` | 创建组织 |
| `SeedOrg` | `func (u *Usecase) SeedOrg(ctx context.Context, name, adminEmail, adminPass string) (*model.Org, error)` | 创建种子组织（含管理员） |
| `ListOrgs` | `func (u *Usecase) ListOrgs(ctx context.Context) ([]*model.Org, error)` | 列出所有组织 |

### 3.4 biz/user — 用户管理

**文件**: `internal/iam/biz/user/usecase.go`, `hash.go`, `repo.go`

**用途**: 用户账号管理（注册/登录/密码重置）。

**核心类型**:

| 类型 | 文件 | 说明 |
|------|------|------|
| `Usecase` | usecase.go | 用户管理用例 |
| `Repo` | repo.go | 用户存储接口 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `Register` | `func (u *Usecase) Register(ctx context.Context, email, password, displayName string) (*model.User, error)` | 注册用户 |
| `Login` | `func (u *Usecase) Login(ctx context.Context, email, password string) (*model.User, error)` | 登录验证 |
| `ChangePassword` | `func (u *Usecase) ChangePassword(ctx context.Context, userID uint64, oldPass, newPass string) error` | 修改密码 |
| `hashPassword` | hash.go | `func hashPassword(password string) (string, error)` | Argon2id 哈希 |

### 3.5 data/org — 组织数据层

**文件**: `internal/iam/data/org/store/repo.go`, `migrate.go`

**用途**: 组织 GORM 持久化。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `Create` | `func (r *Repo) Create(ctx context.Context, org *model.Org) error` | 创建组织 |
| `GetByID` | `func (r *Repo) GetByID(ctx context.Context, id uint64) (*model.Org, error)` | 按 ID 查询 |
| `List` | `func (r *Repo) List(ctx context.Context) ([]*model.Org, error)` | 列出所有组织 |

### 3.6 data/user — 用户数据层

**文件**: `internal/iam/data/user/sqlite/user.go`, `migrate.go`

**用途**: 用户 GORM 持久化。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `Create` | `func (r *Repo) Create(ctx context.Context, user *model.User) error` | 创建用户 |
| `GetByEmail` | `func (r *Repo) GetByEmail(ctx context.Context, email string) (*model.User, error)` | 按邮箱查询 |
| `GetByID` | `func (r *Repo) GetByID(ctx context.Context, id uint64) (*model.User, error)` | 按 ID 查询 |
| `UpdatePassword` | `func (r *Repo) UpdatePassword(ctx context.Context, id uint64, hash string) error` | 更新密码哈希 |

### 3.7 data/membership — 成员数据层

**文件**: `internal/iam/data/membership/store/repo.go`, `migrate.go`

**用途**: 组织成员 GORM 持久化。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `Create` | `func (r *Repo) Create(ctx context.Context, m *model.OrgMembership) error` | 创建成员关系 |
| `ListByOrg` | `func (r *Repo) ListByOrg(ctx context.Context, orgID uint64) ([]*model.OrgMembership, error)` | 按组织列出成员 |
| `Delete` | `func (r *Repo) Delete(ctx context.Context, orgID, userID uint64) error` | 删除成员关系 |

### 3.8 model — IAM 实体

**文件**: `internal/iam/model/model.go`

**用途**: IAM 领域模型定义。

**核心类型**:

| 类型 | 说明 |
|------|------|
| `User` | 用户实体（ID/Email/PasswordHash/DisplayName/Role/CreatedAt） |
| `Org` | 组织实体（ID/Name/CreatedAt） |
| `OrgMembership` | 组织成员关系（OrgID/UserID/Role/CreatedAt） |

### 3.9 server — IAM HTTP 路由

**文件**: `internal/iam/server/http.go`, `orgs.go`

**用途**: IAM HTTP 路由 + 登录限流。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `NewHTTPServer` | `func NewHTTPServer(svc *service.Service, signer *auth.Signer) *HTTPServer` | 构建 HTTP 服务器 |
| `RegisterRoutes` | `func (s *HTTPServer) RegisterRoutes(mux *http.ServeMux)` | 注册路由（/api/v1/auth/*, /api/v1/orgs/*） |
| `handleLogin` | http.go | `func (s *HTTPServer) handleLogin(w http.ResponseWriter, r *http.Request)` | 登录（5 次/分钟限流） |
| `handleRegister` | http.go | `func (s *HTTPServer) handleRegister(w http.ResponseWriter, r *http.Request)` | 注册 |

### 3.10 service — IAM 服务门面

**文件**: `internal/iam/service/service.go`

**用途**: IAM 服务门面，组合所有 biz 用例。

**核心类型**:

| 类型 | 说明 |
|------|------|
| `Service` | IAM 服务门面 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `New` | `func New(userUC *user.Usecase, orgUC *org.Usecase, memberUC *membership.Usecase, authzUC *authz.Enforcer) *Service` | 构建服务 |

---

## 4. internal/manager/biz/ — 云端业务逻辑层

### 4.1 aiops — AI 运维核心

**文件**: `internal/manager/biz/aiops/repo.go`, `usage.go`

**用途**: 会话持久化接口 + Token 用量统计。

**核心接口**:

| 接口 | 文件 | 说明 |
|------|------|------|
| `SessionRepo` | repo.go | 会话存储接口（CRUD + 消息 + 工具调用） |
| `MutatingProposalRepo` | repo.go | 变更提案审计存储接口 |

**核心类型**:

| 类型 | 文件 | 说明 |
|------|------|------|
| `UsageUsecase` | usage.go | Token 用量统计用例 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `GetUsage` | `func (u *UsageUsecase) GetUsage(ctx context.Context, userID uint64, period string) (*UsageSummary, error)` | 获取用量统计 |

### 4.2 aiops/agent — 旧版 Agent 循环

**文件**: `internal/manager/biz/aiops/agent/agent.go`

**用途**: 旧版 Agent for 循环内核（ONGRID_AGENT_KERNEL=legacy 时使用）。

**核心类型**:

| 类型 | 说明 |
|------|------|
| `Agent` | 旧版 Agent 循环 |
| `Reply` | Agent 回复 |
| `RunOptions` | 运行选项（Provider/Model/Mentions） |
| `Emit` | `func(Event)` 流式事件回调 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `New` | `func New(cfg Config) *Agent` | 构建 Agent |
| `Run` | `func (a *Agent) Run(ctx context.Context, opts RunOptions) (*Reply, error)` | 运行一轮对话 |
| `RunStream` | `func (a *Agent) RunStream(ctx context.Context, opts RunOptions, emit Emit) (*Reply, error)` | 流式运行 |

### 4.3 aiops/chatruntime — 新版 eino 图内核

**文件**: `internal/manager/biz/aiops/chatruntime/runtime.go`, `types.go`

**用途**: 新版 eino 图内核（ONGRID_AGENT_KERNEL=graph 时使用），基于 compose.Graph 构建 ReAct 拓扑。

**核心类型**:

| 类型 | 文件 | 说明 |
|------|------|------|
| `Runtime` | runtime.go | 图内核运行时 |
| `Config` | runtime.go | 运行时配置（SkillRegistry/AgentRegistry/ChatModel/ToolBag/CallbackDeps） |
| `Request` | runtime.go | 每次请求输入（SessionID/UserID/Role/UserText/Provider/Model/Mentions/Locale/Emit） |
| `Reply` | runtime.go | 终端结果（Message/Usage/Iterations/ToolCalls） |
| `Event` | runtime.go | 流式事件信封（Type=assistant/tool_start/tool_end/done/error） |
| `Mention` | types.go | @-提及（Type/ID/Label） |
| `MentionResolver` | types.go | @-提及解析接口 |
| `SkillRegistry` | types.go | 技能注册表 |
| `AgentRegistry` | types.go | Agent 人设注册表 |
| `Worker` | types.go | 子 Agent 工作者 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `NewRuntime` | `func NewRuntime(cfg Config) (*Runtime, error)` | 构建运行时 |
| `Handle` | `func (rt *Runtime) Handle(ctx context.Context, req *Request) (*Reply, error)` | 处理一轮对话 |
| `SetMentionResolver` | `func (rt *Runtime) SetMentionResolver(r MentionResolver)` | 后置注入 @-提及解析器 |
| `SetCredentialBinder` | `func (rt *Runtime) SetCredentialBinder(b CredentialBinder)` | 后置注入凭证绑定器 |
| `SetAgentWriteEnabledProvider` | `func (rt *Runtime) SetAgentWriteEnabledProvider(fn func(ctx context.Context) bool)` | 后置注入写操作门控 |
| `ComposeSystemPrompt` | `func ComposeSystemPrompt(base string, skills []*SkillMeta, agent *AgentMeta, locale string) string` | 组装系统提示词 |

### 4.4 aiops/graph — ReAct 图

**文件**: `internal/manager/biz/aiops/graph/react.go`, `tool_adapter.go`, `types.go`, `budget_stop_model.go`

**用途**: eino compose.Graph ReAct 拓扑（START → MessageAssembler → ReActSubgraph → OutputProjector → END）。

**核心类型**:

| 类型 | 文件 | 说明 |
|------|------|------|
| `Config` | types.go | 图配置（MaxIterations/Temperature/ToolTimeout） |
| `Input` | types.go | 图输入（Messages/SystemPrompt/ToolBag/Locale） |
| `Output` | types.go | 图输出（Message/Usage/Iterations/ToolCalls） |
| `BudgetStopModel` | budget_stop_model.go | 预算停止装饰器 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `NewReActGraph` | `func NewReActGraph(cfg Config) (*compose.Graph, error)` | 构建 ReAct 图 |
| `NewToolAdapter` | `func NewToolAdapter(tools []basetool.BaseTool) *ToolAdapter` | 构建工具适配器 |

### 4.5 aiops/graph/callbacks — 回调处理器链

**文件**: `chain.go`, `sse.go`, `persistence.go`, `audit.go`, `metrics.go`, `alert_draft_guard.go`

**用途**: eino callback 处理器链，按序执行：AlertDraftGuard → Persistence → SSE → Audit → Metrics → Budget。

**核心类型**:

| 类型 | 文件 | 说明 |
|------|------|------|
| `Deps` | chain.go | 回调依赖（Persistence/Audit/Metrics/Budget/SSE） |
| `PersistenceHandler` | persistence.go | 消息持久化回调（assistant + tool_call 行） |
| `SSEHandler` | sse.go | SSE 流式推送回调 |
| `AuditHandler` | audit.go | 审计日志回调 |
| `MetricsHandler` | metrics.go | LLM 调用指标回调 |
| `AlertDraftGuardHandler` | alert_draft_guard.go | 告警草案守卫回调（拦截 LLM 自由文本草案） |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `NewDefaultHandlers` | chain.go | `func NewDefaultHandlers(deps Deps) []einocallbacks.Handler` | 构建默认回调链 |
| `NewPersistenceHandler` | persistence.go | `func NewPersistenceHandler(deps PersistenceDeps) *PersistenceHandler` | 构建持久化回调 |
| `NewSSEHandler` | sse.go | `func NewSSEHandler(emit chatruntime.Emit) *SSEHandler` | 构建 SSE 回调 |
| `NewAuditHandler` | audit.go | `func NewAuditHandler(sink AuditSink) *AuditHandler` | 构建审计回调 |
| `NewMetricsHandler` | metrics.go | `func NewMetricsHandler(reg prometheus.Registerer) *MetricsHandler` | 构建指标回调 |
| `NewAlertDraftGuardHandler` | alert_draft_guard.go | `func NewAlertDraftGuardHandler(deps AlertDraftGuardDeps) *AlertDraftGuardHandler` | 构建告警守卫回调 |

### 4.6 aiops/tools — 工具注册表

**文件**: `internal/manager/biz/aiops/tools/registry.go`, `toolbag.go`, `agent_tool.go`

**用途**: AI 工具注册表 + 工具包装器。

**核心类型**:

| 类型 | 文件 | 说明 |
|------|------|------|
| `Registry` | registry.go | 工具注册表（内置 + Skill + HostFiles） |
| `ToolBag` | toolbag.go | 工具集容器（支持延迟加载 + 过滤） |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `NewRegistry` | `func NewRegistry(deps RegistryDeps) *Registry` | 构建注册表 |
| `BuildBaseTools` | `func (r *Registry) BuildBaseTools(ctx context.Context) ([]basetool.BaseTool, error)` | 构建基础工具列表 |
| `NewToolBag` | `func NewToolBag(tools []basetool.BaseTool) *ToolBag` | 构建工具集 |

### 4.7 aiops/mentions — @-提及搜索

**文件**: `internal/manager/biz/aiops/mentions/search.go`

**用途**: @-提及搜索（设备/告警/K8s 集群等实体）。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `Search` | `func Search(ctx context.Context, query string, limit int) ([]MentionResult, error)` | 搜索提及实体 |

### 4.8 aiops/alertdraft — 告警草案编译器

**文件**: `internal/manager/biz/aiops/alertdraft/types.go`, `promql.go`, `regex.go`, `scope.go`

**用途**: 告警规则草案编译器（PromQL 验证 + 正则校验 + 范围限定）。

**核心类型**:

| 类型 | 文件 | 说明 |
|------|------|------|
| `Draft` | types.go | 告警草案（Kind/Spec/DraftHash） |
| `ConfigDraft` | types.go | 配置草案 |
| `PromQLValidator` | promql.go | PromQL 语法验证器 |
| `RegexValidator` | regex.go | 正则表达式验证器 |
| `ScopeResolver` | scope.go | 范围解析器 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `ValidatePromQL` | `func ValidatePromQL(expr string) error` | 验证 PromQL 语法 |
| `ValidateRegex` | `func ValidateRegex(pattern string) error` | 验证正则表达式 |
| `ResolveScope` | `func ResolveScope(ctx context.Context, scope []string) (string, error)` | 解析告警范围 |

### 4.9 alert — 告警管理

**文件**: `internal/manager/biz/alert/usecase.go`, `pipeline.go`, `rules.go`, `inhibit.go`, `retry.go`, `router.go`, `preview.go`, `burn_rate_sli.go`

**用途**: 告警全生命周期管理（规则评估 → 抑制 → 路由 → 通知 → 确认）。

**核心类型**:

| 类型 | 文件 | 说明 |
|------|------|------|
| `Usecase` | usecase.go | 告警管理用例 |
| `Pipeline` | pipeline.go | 告警流水线（评估 → 抑制 → 路由 → 通知） |
| `Router` | router.go | 告警路由器（按标签匹配通知通道） |
| `Inhibitor` | inhibit.go | 告警抑止器（级联告警抑制） |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `NewPipeline` | `func NewPipeline(deps PipelineDeps) *Pipeline` | 构建告警流水线 |
| `Evaluate` | `func (p *Pipeline) Evaluate(ctx context.Context, rule AlertRule) ([]Alert, error)` | 评估告警规则 |
| `Preview` | `func Preview(ctx context.Context, expr string, duration time.Duration) ([]PreviewPoint, error)` | 预览告警规则 |
| `BurnRateSLI` | `func BurnRateSLI(ctx context.Context, expr string, window time.Duration) (float64, error)` | 计算 Burn Rate SLI |

### 4.10 approval — 审批收件箱

**文件**: `internal/manager/biz/approval/usecase.go`

**用途**: 审批流程管理（待审批列表/批准/拒绝）。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `ListPending` | `func (u *Usecase) ListPending(ctx context.Context, userID uint64) ([]Approval, error)` | 列出待审批项 |
| `Approve` | `func (u *Usecase) Approve(ctx context.Context, id uint64, userID uint64) error` | 批准 |
| `Reject` | `func (u *Usecase) Reject(ctx context.Context, id uint64, userID uint64, reason string) error` | 拒绝 |

### 4.11 audit — 审计日志

**文件**: `internal/manager/biz/audit/usecase.go`

**用途**: 审计日志记录与查询。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `Record` | `func (u *Usecase) Record(ctx context.Context, entry AuditEntry) error` | 记录审计条目 |
| `Query` | `func (u *Usecase) Query(ctx context.Context, filter AuditFilter) ([]AuditEntry, int64, error)` | 查询审计日志 |

### 4.12 device — 设备管理

**文件**: `internal/manager/biz/device/usecase.go`, `repo.go`, `edge_device.go`

**用途**: 设备注册表管理（注册/注销/心跳/标签）。

**核心类型**:

| 类型 | 文件 | 说明 |
|------|------|------|
| `Usecase` | usecase.go | 设备管理用例 |
| `Repo` | repo.go | 设备存储接口 |
| `EdgeDevice` | edge_device.go | 边端设备适配器 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `Register` | `func (u *Usecase) Register(ctx context.Context, d Device) (*Device, error)` | 注册设备 |
| `Heartbeat` | `func (u *Usecase) Heartbeat(ctx context.Context, id uint64) error` | 设备心跳 |
| `List` | `func (u *Usecase) List(ctx context.Context, filter DeviceFilter) ([]*Device, int64, error)` | 列出设备 |

### 4.13 edge — 边端节点管理

**文件**: `internal/manager/biz/edge/usecase.go`, `authn.go`, `authcache.go`, `plugin_config.go`, `plugin_health.go`

**用途**: 边端节点管理（认证/插件配置/健康检查）。

**核心类型**:

| 类型 | 文件 | 说明 |
|------|------|------|
| `Usecase` | usecase.go | 边端管理用例 |
| `AuthCache` | authcache.go | 边端认证缓存 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `Authenticate` | `func (u *Usecase) Authenticate(ctx context.Context, token string) (*EdgeNode, error)` | 边端认证 |
| `GetPluginConfig` | `func (u *Usecase) GetPluginConfig(ctx context.Context, nodeID uint64, plugin string) ([]byte, error)` | 获取插件配置 |
| `ReportHealth` | `func (u *Usecase) ReportHealth(ctx context.Context, nodeID uint64, health PluginHealth) error` | 上报插件健康 |

### 4.14 flow — DAG 流程引擎

**文件**: `internal/manager/biz/flow/engine.go`, `graph.go`, `nodes.go`, `dispatcher.go`, `scheduler.go`, `catalog.go`, `generate.go`, `expr.go`, `http_node.go`, `noderegistry.go`

**用途**: DAG 执行引擎（确定性骨架 + 概率性节点内部）。

**核心类型**:

| 类型 | 文件 | 说明 |
|------|------|------|
| `Engine` | engine.go | DAG 引擎 |
| `Graph` | graph.go | DAG 图定义 |
| `Node` | nodes.go | DAG 节点（HTTP/Condition/LLM/Alert/Script） |
| `Dispatcher` | dispatcher.go | 任务分发器 |
| `Scheduler` | scheduler.go | 定时调度器 |
| `Catalog` | catalog.go | 节点类型目录 |
| `NodeRegistry` | noderegistry.go | 节点注册表 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `NewEngine` | `func NewEngine(deps EngineDeps) *Engine` | 构建引擎 |
| `Execute` | `func (e *Engine) Execute(ctx context.Context, flowID string, input map[string]interface{}) (*FlowResult, error)` | 执行流程 |
| `NewGraph` | `func NewGraph(def GraphDef) (*Graph, error)` | 构建图 |
| `GenerateFlow` | `func GenerateFlow(ctx context.Context, prompt string) (*GraphDef, error)` | LLM 生成流程定义 |

### 4.15 grafana — Grafana 自动配置

**文件**: `internal/manager/biz/grafana/service.go`

**用途**: Grafana 自动配置（数据源/仪表盘/用户同步）。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `AutoConfigure` | `func (s *Service) AutoConfigure(ctx context.Context, orgID int) error` | 自动配置组织 |
| `SyncDatasources` | `func (s *Service) SyncDatasources(ctx context.Context, orgID int) error` | 同步数据源 |
| `SyncDashboard` | `func (s *Service) SyncDashboard(ctx context.Context, orgID int, dashboard json.RawMessage) error` | 同步仪表盘 |

### 4.16 imbridge — IM 桥接

**文件**: `internal/manager/biz/imbridge/usecase.go`, `bridge.go`, `dedup.go`, `sender.go`, `adapter.go`

**用途**: IM 消息桥接（告警 → Slack/飞书/钉钉/企微/Telegram）。

**核心类型**:

| 类型 | 文件 | 说明 |
|------|------|------|
| `Usecase` | usecase.go | IM 桥接用例 |
| `Bridge` | bridge.go | 消息桥接器 |
| `Dedup` | dedup.go | 消息去重器 |
| `Sender` | sender.go | 消息发送器 |
| `Adapter` | adapter.go | IM 平台适配器接口 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `SendAlert` | `func (u *Usecase) SendAlert(ctx context.Context, alert Alert, channels []string) error` | 发送告警到 IM |
| `Deduplicate` | `func (d *Dedup) Deduplicate(key string) bool` | 消息去重 |

### 4.17 k8s — K8s 集群管理

**文件**: `internal/manager/biz/k8s/usecase.go`, `status.go`, `inventory_upload.go`, `telemetry_auth.go`

**用途**: K8s 集群注册表管理（注册/状态/清单/遥测认证）。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `Register` | `func (u *Usecase) Register(ctx context.Context, cluster K8sCluster) (*K8sCluster, error)` | 注册集群 |
| `GetStatus` | `func (u *Usecase) GetStatus(ctx context.Context, id uint64) (*ClusterStatus, error)` | 获取集群状态 |
| `UploadInventory` | `func (u *Usecase) UploadInventory(ctx context.Context, id uint64, inventory []byte) error` | 上传集群清单 |
| `TelemetryAuth` | `func (u *Usecase) TelemetryAuth(ctx context.Context, id uint64) (string, error)` | 遥测认证令牌 |

### 4.18 knowledge — 知识库 + RAG

**文件**: `internal/manager/biz/knowledge/usecase.go`

**用途**: 知识库管理 + RAG 检索增强生成。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `CreateKnowledge` | `func (u *Usecase) CreateKnowledge(ctx context.Context, k Knowledge) (*Knowledge, error)` | 创建知识库 |
| `IngestDocument` | `func (u *Usecase) IngestDocument(ctx context.Context, id uint64, r io.Reader, filename string) error` | 摄入文档（提取+分块+嵌入+存储） |
| `Search` | `func (u *Usecase) Search(ctx context.Context, id uint64, query string, topK int) ([]Chunk, error)` | 向量相似搜索 |

### 4.19 marketplace — 技能/Agent 市场

**文件**: `internal/manager/biz/marketplace/usecase.go`, `source.go`, `signature.go`, `targz.go`

**用途**: 技能/Agent 市场管理（安装/卸载/签名验证/tar.gz 解包）。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `Install` | `func (u *Usecase) Install(ctx context.Context, packURL string) (*InstalledPack, error)` | 安装技能包 |
| `Uninstall` | `func (u *Usecase) Uninstall(ctx context.Context, packID string) error` | 卸载技能包 |
| `VerifySignature` | `func VerifySignature(data []byte, sig string, pubKey *rsa.PublicKey) error` | 验证签名 |

### 4.20 mcp — MCP 服务器管理

**文件**: `internal/manager/biz/mcp/usecase.go`

**用途**: MCP (Model Context Protocol) 服务器注册表管理。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `Register` | `func (u *Usecase) Register(ctx context.Context, s MCPServer) (*MCPServer, error)` | 注册 MCP 服务器 |
| `ListTools` | `func (u *Usecase) ListTools(ctx context.Context, id uint64) ([]mcpclient.Tool, error)` | 列出工具 |
| `CallTool` | `func (u *Usecase) CallTool(ctx context.Context, id uint64, name string, args map[string]interface{}) (*mcpclient.ToolResult, error)` | 调用工具 |

### 4.21 metric — 指标查询

**文件**: `internal/manager/biz/metric/query.go`, `ingester.go`, `downsample.go`, `retention.go`

**用途**: 指标查询/写入/降采样/保留策略。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `Query` | `func (u *Usecase) Query(ctx context.Context, expr string, start, end time.Time, step time.Duration) (model.Value, error)` | 查询指标 |
| `Ingest` | `func (u *Usecase) Ingest(ctx context.Context, series []promwrite.TimeSeries) error` | 写入指标 |
| `Downsample` | `func (u *Usecase) Downsample(ctx context.Context, expr string, from, to time.Duration) error` | 降采样 |

### 4.22 monitor — 监控面板

**文件**: `internal/manager/biz/monitor/service.go`

**用途**: 监控面板管理（CRUD + 嵌入式 Grafana）。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `CreatePanel` | `func (s *Service) CreatePanel(ctx context.Context, p Panel) (*Panel, error)` | 创建面板 |
| `ListPanels` | `func (s *Service) ListPanels(ctx context.Context) ([]*Panel, error)` | 列出面板 |

### 4.23 promwrite — Prometheus Remote-Write 桥接

**文件**: `internal/manager/biz/promwrite/ingester.go`

**用途**: Prometheus remote_write 接收桥接。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `Ingest` | `func (i *Ingester) Ingest(ctx context.Context, series []promwrite.TimeSeries) error` | 接收并转发时序数据 |

### 4.24 report — 定时报告

**文件**: `internal/manager/biz/report/usecase.go`, `content.go`, `cron.go`, `delivery.go`, `facts.go`, `generator.go`, `period.go`, `query.go`, `scheduler.go`, `task.go`

**用途**: 定时报告生成与投递（Cron 调度 + LLM 生成 + 多通道投递）。

**核心类型**:

| 类型 | 文件 | 说明 |
|------|------|------|
| `Usecase` | usecase.go | 报告管理用例 |
| `Scheduler` | scheduler.go | Cron 调度器 |
| `Generator` | generator.go | LLM 报告生成器 |
| `Delivery` | delivery.go | 多通道投递器 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `CreateReport` | `func (u *Usecase) CreateReport(ctx context.Context, r Report) (*Report, error)` | 创建报告 |
| `Generate` | `func (g *Generator) Generate(ctx context.Context, report *Report) (string, error)` | LLM 生成报告内容 |
| `Deliver` | `func (d *Delivery) Deliver(ctx context.Context, report *Report, content string) error` | 投递报告 |

### 4.25 secret — 密钥保险库

**文件**: `internal/manager/biz/secret/usecase.go`, `credtype.go`

**用途**: 密钥保险库管理（AES-256-GCM 加密存储）。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `Create` | `func (u *Usecase) Create(ctx context.Context, s Secret) (*Secret, error)` | 创建密钥 |
| `Get` | `func (u *Usecase) Get(ctx context.Context, id uint64) (*Secret, error)` | 获取密钥（解密） |
| `List` | `func (u *Usecase) List(ctx context.Context, filter SecretFilter) ([]*Secret, error)` | 列出密钥 |

### 4.26 setting — 系统设置

**文件**: `internal/manager/biz/setting/service.go`, `llm.go`, `llm_probe.go`, `agent.go`, `probe.go`, `promauth.go`, `telemetry.go`, `websearch.go`

**用途**: 系统设置管理（LLM 配置/探测/Agent 配置/遥测/Web 搜索）。

**核心函数**:

| 函数 | 文件 | 签名 | 说明 |
|------|------|------|------|
| `Get` | service.go | `func (s *Service) Get(ctx context.Context, key string) (string, error)` | 获取设置 |
| `Set` | service.go | `func (s *Service) Set(ctx context.Context, key, value string) error` | 设置值 |
| `ProbeLLM` | llm_probe.go | `func (s *Service) ProbeLLM(ctx context.Context, provider, baseURL, apiKey, model string) error` | 探测 LLM 供应商 |
| `ResolveLLMConfig` | llm.go | `func (s *Service) ResolveLLMConfig(ctx context.Context, provider string) (*llm.ProviderConfig, error)` | 解析 LLM 配置 |

### 4.27 skill — Skill 执行

**文件**: `internal/manager/biz/skill/service.go`, `audit.go`

**用途**: Skill 执行服务（安全级别分类 + 审计）。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `Execute` | `func (s *Service) Execute(ctx context.Context, skillName string, args map[string]interface{}) (interface{}, error)` | 执行技能 |
| `Classify` | `func (s *Service) Classify(skillName string) SkillClass` | 分类安全级别（safe/mutating/dangerous） |

### 4.28 topology — 拓扑图

**文件**: `internal/manager/biz/topology/usecase.go`, `repo.go`

**用途**: 拓扑图管理（服务依赖关系图）。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `GetTopology` | `func (u *Usecase) GetTopology(ctx context.Context) (*Topology, error)` | 获取拓扑图 |
| `Refresh` | `func (u *Usecase) Refresh(ctx context.Context) error` | 刷新拓扑 |

### 4.29 webshell — WebShell 会话路由

**文件**: `internal/manager/biz/webshell/router.go`

**用途**: WebSSH 会话路由（映射会话到边端隧道）。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `Route` | `func (r *Router) Route(ctx context.Context, sessionID string) (string, error)` | 路由到目标边端 |

---

## 5. internal/manager/server/ — HTTP 路由层

### 5.1 aiops — AI 运维 HTTP 处理器

**文件**: `internal/manager/server/aiops/handler.go`

**用途**: AI 运维 HTTP 处理器（会话 CRUD + 消息发送 + SSE 流式）。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `NewHandler` | `func NewHandler(svc *aiopssvc.Service) *Handler` | 构建处理器 |
| `RegisterRoutes` | `func (h *Handler) RegisterRoutes(mux *http.ServeMux)` | 注册路由 |
| `handleCreateSession` | `func (h *Handler) handleCreateSession(w, r)` | POST /v1/chat/sessions |
| `handlePostMessageStream` | `func (h *Handler) handlePostMessageStream(w, r)` | POST /v1/chat/sessions/{id}/messages/stream |

### 5.2 alert — 告警 HTTP 处理器

**文件**: `internal/manager/server/alert/handler.go`

**用途**: 告警 HTTP 处理器（规则 CRUD + 确认/静默）。

### 5.3 device — 设备 HTTP 处理器

**文件**: `internal/manager/server/device/handler.go`

**用途**: 设备 HTTP 处理器（列表/详情/标签）。

### 5.4 edge — 边端 HTTP 处理器

**文件**: `internal/manager/server/edge/handler.go`

**用途**: 边端 HTTP 处理器（注册/心跳/插件配置）。

### 5.5 flow — 流程 HTTP 处理器

**文件**: `internal/manager/server/flow/handler.go`

**用途**: 流程 HTTP 处理器（CRUD + 执行 + 定时）。

### 5.6 k8s — K8s HTTP 处理器

**文件**: `internal/manager/server/k8s/handler.go`

**用途**: K8s HTTP 处理器（集群注册/状态/清单）。

### 5.7 knowledge — 知识库 HTTP 处理器

**文件**: `internal/manager/server/knowledge/handler.go`

**用途**: 知识库 HTTP 处理器（CRUD + 文档上传 + 搜索）。

### 5.8 marketplace — 市场 HTTP 处理器

**文件**: `internal/manager/server/marketplace/handler.go`

**用途**: 市场 HTTP 处理器（安装/卸载/列表）。

### 5.9 mcp — MCP HTTP 处理器

**文件**: `internal/manager/server/mcp/handler.go`

**用途**: MCP HTTP 处理器（注册/列表/调用）。

### 5.10 metric — 指标 HTTP 处理器

**文件**: `internal/manager/server/metric/handler.go`

**用途**: 指标 HTTP 处理器（查询/写入）。

### 5.11 monitor — 监控 HTTP 处理器

**文件**: `internal/manager/server/monitor/handler.go`

**用途**: 监控 HTTP 处理器（面板 CRUD）。

### 5.12 report — 报告 HTTP 处理器

**文件**: `internal/manager/server/report/handler.go`

**用途**: 报告 HTTP 处理器（CRUD + 手动触发）。

### 5.13 secret — 密钥 HTTP 处理器

**文件**: `internal/manager/server/secret/handler.go`

**用途**: 密钥 HTTP 处理器（CRUD）。

### 5.14 setting — 设置 HTTP 处理器

**文件**: `internal/manager/server/setting/handler.go`

**用途**: 设置 HTTP 处理器（获取/更新/探测）。

### 5.15 topology — 拓扑 HTTP 处理器

**文件**: `internal/manager/server/topology/handler.go`

**用途**: 拓扑 HTTP 处理器（获取/刷新）。

### 5.16 webshell — WebShell HTTP 处理器

**文件**: `internal/manager/server/webshell/handler.go`

**用途**: WebShell HTTP 处理器（WebSocket 升级 + 隧道代理）。

### 5.17 approval — 审批 HTTP 处理器

**文件**: `internal/manager/server/approval/handler.go`

**用途**: 审批 HTTP 处理器（列表/批准/拒绝）。

### 5.18 audit — 审计 HTTP 处理器

**文件**: `internal/manager/server/audit/handler.go`

**用途**: 审计 HTTP 处理器（查询）。

---

## 6. internal/manager/service/ — 服务门面层

### 6.1 aiops — AI 运维服务

**文件**: `internal/manager/service/aiops/service.go`

**用途**: AI 运维服务门面，持有 legacyAgent + runtime 双内核，按 ONGRID_AGENT_KERNEL 切换。

**核心类型**:

| 类型 | 说明 |
|------|------|
| `Service` | AI 运维服务（legacyAgent + runtime + kernel + sessions + proposals + usage） |
| `Kernel` | 内核枚举（legacy/graph） |
| `Caller` | 调用方身份（UserID/Role） |
| `RuntimeHandler` | 图内核接口（`Handle(ctx, *chatruntime.Request) (*chatruntime.Reply, error)`） |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `NewWithKernel` | `func NewWithKernel(a *agent.Agent, runtime RuntimeHandler, kernel Kernel, ...) *Service` | 内核感知构造器 |
| `CreateSession` | `func (s *Service) CreateSession(ctx, caller, CreateSessionInput) (*model.Session, error)` | 创建会话 |
| `PostMessageStreamWithOpts` | `func (s *Service) PostMessageStreamWithOpts(ctx, caller, sessionID, content, emit, opts) (*agent.Reply, error)` | 流式发送消息 |
| `CancelTurn` | `func (s *Service) CancelTurn(sessionID string)` | 取消进行中的对话轮次（Esc 停止） |
| `ListMutatingProposals` | `func (s *Service) ListMutatingProposals(ctx, caller, filter) ([]*model.MutatingProposal, int64, error)` | 列出变更提案 |

### 6.2 alert — 告警服务

**文件**: `internal/manager/service/alert/service.go`

**用途**: 告警服务门面，组合 alert.Usecase + alert.Pipeline。

### 6.3 device — 设备服务

**文件**: `internal/manager/service/device/service.go`

**用途**: 设备服务门面。

### 6.4 edge — 边端服务

**文件**: `internal/manager/service/edge/service.go`

**用途**: 边端服务门面。

### 6.5 flow — 流程服务

**文件**: `internal/manager/service/flow/service.go`

**用途**: 流程服务门面。

### 6.6 k8s — K8s 服务

**文件**: `internal/manager/service/k8s/service.go`

**用途**: K8s 服务门面。

### 6.7 knowledge — 知识库服务

**文件**: `internal/manager/service/knowledge/service.go`

**用途**: 知识库服务门面。

### 6.8 marketplace — 市场服务

**文件**: `internal/manager/service/marketplace/service.go`

**用途**: 市场服务门面。

### 6.9 mcp — MCP 服务

**文件**: `internal/manager/service/mcp/service.go`

**用途**: MCP 服务门面。

### 6.10 metric — 指标服务

**文件**: `internal/manager/service/metric/service.go`

**用途**: 指标服务门面。

### 6.11 monitor — 监控服务

**文件**: `internal/manager/service/monitor/service.go`

**用途**: 监控服务门面。

### 6.12 report — 报告服务

**文件**: `internal/manager/service/report/service.go`

**用途**: 报告服务门面。

### 6.13 secret — 密钥服务

**文件**: `internal/manager/service/secret/service.go`

**用途**: 密钥服务门面。

### 6.14 setting — 设置服务

**文件**: `internal/manager/service/setting/service.go`

**用途**: 设置服务门面。

### 6.15 topology — 拓扑服务

**文件**: `internal/manager/service/topology/service.go`

**用途**: 拓扑服务门面。

### 6.16 webshell — WebShell 服务

**文件**: `internal/manager/service/webshell/service.go`

**用途**: WebShell 服务门面。

### 6.17 approval — 审批服务

**文件**: `internal/manager/service/approval/service.go`

**用途**: 审批服务门面。

### 6.18 audit — 审计服务

**文件**: `internal/manager/service/audit/service.go`

**用途**: 审计服务门面。

---

## 7. internal/manager/data/ — 数据持久化层

### 7.1 aiops — AI 运维数据层

**文件**: `internal/manager/data/aiops/store/repo.go`

**用途**: chat_sessions / chat_messages / chat_tool_calls 的 GORM 持久化。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `CreateSession` | `func (r *Repo) CreateSession(ctx context.Context, s *model.Session) error` | 创建会话 |
| `CreateMessage` | `func (r *Repo) CreateMessage(ctx context.Context, m *model.Message) error` | 创建消息 |
| `CreateToolCall` | `func (r *Repo) CreateToolCall(ctx context.Context, tc *model.ToolCall) error` | 创建工具调用 |
| `UpdateToolCall` | `func (r *Repo) UpdateToolCall(ctx context.Context, tc *model.ToolCall) error` | 更新工具调用 |

### 7.2 alert — 告警数据层

**文件**: `internal/manager/data/alert/store/repo.go`

**用途**: 告警规则与状态的 GORM 持久化。

### 7.3 device — 设备数据层

**文件**: `internal/manager/data/device/store/repo.go`

**用途**: 设备注册表的 GORM 持久化。

### 7.4 edge — 边端数据层

**文件**: `internal/manager/data/edge/store/repo.go`

**用途**: 边端节点的 GORM 持久化。

### 7.5 flow — 流程数据层

**文件**: `internal/manager/data/flow/store/repo.go`

**用途**: 流程 DAG 定义的 GORM 持久化。

### 7.6 k8s — K8s 数据层

**文件**: `internal/manager/data/k8s/store/repo.go`

**用途**: K8s 集群注册表的 GORM 持久化。

### 7.7 knowledge — 知识库数据层

**文件**: `internal/manager/data/knowledge/store/repo.go`

**用途**: 知识库与文档块的 GORM 持久化。

### 7.8 marketplace — 市场数据层

**文件**: `internal/manager/data/marketplace/store/repo.go`

**用途**: 已安装技能包的 GORM 持久化。

### 7.9 mcp — MCP 数据层

**文件**: `internal/manager/data/mcp/store/repo.go`

**用途**: MCP 服务器注册表的 GORM 持久化。

### 7.10 metric — 指标数据层

**文件**: `internal/manager/data/metric/store/repo.go`

**用途**: 指标查询配置的 GORM 持久化。

### 7.11 monitor — 监控数据层

**文件**: `internal/manager/data/monitor/store/repo.go`

**用途**: 监控面板的 GORM 持久化。

### 7.12 report — 报告数据层

**文件**: `internal/manager/data/report/store/repo.go`

**用途**: 定时报告的 GORM 持久化。

### 7.13 secret — 密钥数据层

**文件**: `internal/manager/data/secret/store/repo.go`

**用途**: 密钥保险库的 GORM 持久化。

### 7.14 setting — 设置数据层

**文件**: `internal/manager/data/setting/store/repo.go`

**用途**: 系统设置的 GORM 持久化（key-value）。

### 7.15 topology — 拓扑数据层

**文件**: `internal/manager/data/topology/store/repo.go`

**用途**: 拓扑图的 GORM 持久化。

### 7.16 approval — 审批数据层

**文件**: `internal/manager/data/approval/store/repo.go`

**用途**: 审批流的 GORM 持久化。

### 7.17 audit — 审计数据层

**文件**: `internal/manager/data/audit/store/repo.go`

**用途**: 审计日志的 GORM 持久化。

### 7.18 webshell — WebShell 数据层

**文件**: `internal/manager/data/webshell/store/repo.go`

**用途**: WebShell 会话的 GORM 持久化。

### 7.19 promwrite — Remote-Write 数据层

**文件**: `internal/manager/data/promwrite/store/repo.go`

**用途**: Prometheus Remote-Write 配置的 GORM 持久化。

### 7.20 imbridge — IM 桥接数据层

**文件**: `internal/manager/data/imbridge/store/repo.go`

**用途**: IM 通道配置的 GORM 持久化。

### 7.21 grafana — Grafana 数据层

**文件**: `internal/manager/data/grafana/store/repo.go`

**用途**: Grafana 配置的 GORM 持久化。

---

## 8. internal/manager/model/ — 领域模型层

### 8.1 aiops — AI 运维模型

**文件**: `internal/manager/model/aiops/model.go`

**核心类型**:

| 类型 | 说明 |
|------|------|
| `Session` | 会话（ID/UserID/Title/AgentID/ScopeJSON/CreatedAt/UpdatedAt/ClosedAt） |
| `Message` | 消息（ID/SessionID/Role/Content/Tokens/CreatedAt） |
| `ToolCall` | 工具调用（ID/MessageID/Name/ArgsJSON/ResultJSON/Status/StartedAt/EndedAt） |
| `MutatingProposal` | 变更提案（ID/SessionID/ToolName/ArgsJSON/Decision/ReviewerID/ReviewedAt） |

### 8.2 alert — 告警模型

**文件**: `internal/manager/model/alert/model.go`

**核心类型**: `AlertRule`, `Alert`, `AlertState`, `Silence`, `InhibitRule`

### 8.3 device — 设备模型

**文件**: `internal/manager/model/device/model.go`

**核心类型**: `Device`, `DeviceLabel`

### 8.4 edge — 边端模型

**文件**: `internal/manager/model/edge/model.go`

**核心类型**: `EdgeNode`, `PluginConfig`, `PluginHealth`

### 8.5 flow — 流程模型

**文件**: `internal/manager/model/flow/model.go`

**核心类型**: `Flow`, `FlowRun`, `NodeDef`, `EdgeDef`

### 8.6 k8s — K8s 模型

**文件**: `internal/manager/model/k8s/model.go`

**核心类型**: `K8sCluster`, `ClusterStatus`

### 8.7 knowledge — 知识库模型

**文件**: `internal/manager/model/knowledge/model.go`

**核心类型**: `Knowledge`, `Document`, `Chunk`

### 8.8 marketplace — 市场模型

**文件**: `internal/manager/model/marketplace/model.go`

**核心类型**: `InstalledPack`, `PackMeta`

### 8.9 mcp — MCP 模型

**文件**: `internal/manager/model/mcp/model.go`

**核心类型**: `MCPServer`

### 8.10 metric — 指标模型

**文件**: `internal/manager/model/metric/model.go`

**核心类型**: `MetricQuery`

### 8.11 monitor — 监控模型

**文件**: `internal/manager/model/monitor/model.go`

**核心类型**: `Panel`

### 8.12 report — 报告模型

**文件**: `internal/manager/model/report/model.go`

**核心类型**: `Report`, `ReportTask`

### 8.13 secret — 密钥模型

**文件**: `internal/manager/model/secret/model.go`

**核心类型**: `Secret`（ID/Name/Type/EncryptedValue/CreatedAt）

### 8.14 setting — 设置模型

**文件**: `internal/manager/model/setting/model.go`

**核心类型**: `Setting`（Key/Value/UpdatedAt）

### 8.15 topology — 拓扑模型

**文件**: `internal/manager/model/topology/model.go`

**核心类型**: `Topology`, `ServiceNode`, `Dependency`

### 8.16 approval — 审批模型

**文件**: `internal/manager/model/approval/model.go`

**核心类型**: `Approval`

### 8.17 audit — 审计模型

**文件**: `internal/manager/model/audit/model.go`

**核心类型**: `AuditEntry`（ID/UserID/Action/Resource/Detail/CreatedAt）

### 8.18 webshell — WebShell 模型

**文件**: `internal/manager/model/webshell/model.go`

**核心类型**: `WebshellSession`

### 8.19 promwrite — Remote-Write 模型

**文件**: `internal/manager/model/promwrite/model.go`

**核心类型**: `PromWriteConfig`

### 8.20 imbridge — IM 桥接模型

**文件**: `internal/manager/model/imbridge/model.go`

**核心类型**: `IMChannel`

### 8.21 grafana — Grafana 模型

**文件**: `internal/manager/model/grafana/model.go`

**核心类型**: `GrafanaConfig`

---

## 9. internal/edgeagent/ — 边端 Agent

### 9.1 bash — bash 隧道处理器

**文件**: `internal/edgeagent/bash/handlers.go`

**用途**: bash.exec 隧道消息处理器，接收云端 bash 执行请求。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `HandleBashExec` | `func HandleBashExec(ctx context.Context, req tunnel.BashRequest) (*tunnel.BashResponse, error)` | 处理 bash 执行请求 |

### 9.2 biz — Agent 核心循环

**文件**: `internal/edgeagent/biz/agent.go`, `upgrade.go`, `upgrade_package.go`, `json.go`

**用途**: 边端 Agent 核心循环（心跳/升级/包管理）。

**核心类型**:

| 类型 | 文件 | 说明 |
|------|------|------|
| `Agent` | agent.go | 边端 Agent 核心 |
| `UpgradeManager` | upgrade.go | 升级管理器 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `New` | `func New(cfg Config) *Agent` | 构建 Agent |
| `Run` | `func (a *Agent) Run(ctx context.Context) error` | 启动核心循环 |
| `Upgrade` | `func (u *UpgradeManager) Upgrade(ctx context.Context, version string) error` | 执行升级 |

### 9.3 biz/collector — Phase 1 占位采集器

**文件**: `internal/edgeagent/biz/collector/cpu.go`, `mem.go`, `net.go`

**用途**: Phase 1 占位采集器（CPU/内存/网络基础指标）。

### 9.4 cmdpolicy — 命令策略沙箱

**文件**: `internal/edgeagent/cmdpolicy/parse.go`, `policy.go`, `sandbox.go`, `types.go`

**用途**: 命令策略沙箱（白名单/黑名单 + 参数校验）。

**核心类型**:

| 类型 | 文件 | 说明 |
|------|------|------|
| `Policy` | policy.go | 命令策略（Allow/Deny 规则） |
| `Sandbox` | sandbox.go | 命令沙箱执行器 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `ParsePolicy` | `func ParsePolicy(data []byte) (*Policy, error)` | 解析策略文件 |
| `Execute` | `func (s *Sandbox) Execute(ctx context.Context, cmd string) (*ExecResult, error)` | 沙箱执行命令 |

### 9.5 collector — 双模式采集器

**文件**: `internal/edgeagent/collector/embedded.go`, `scrape.go`, `composite.go`, `mapper.go`, `noop_push.go`, `hwfingerprint.go`

**用途**: 双模式采集器（嵌入式进程内 + 外部抓取式）。

**核心类型**:

| 类型 | 文件 | 说明 |
|------|------|------|
| `Collector` | composite.go | 双模式采集器接口 |
| `EmbeddedCollector` | embedded.go | 嵌入式采集器（进程内 Prometheus 注册表） |
| `ScrapeCollector` | scrape.go | 抓取式采集器（拉取 /metrics 端点） |
| `CompositeCollector` | composite.go | 组合采集器（嵌入式 + 抓取式） |
| `Mapper` | mapper.go | 指标映射器（重标签） |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `NewComposite` | `func NewComposite(embedded *EmbeddedCollector, scrape *ScrapeCollector) *CompositeCollector` | 构建组合采集器 |
| `Collect` | `func (c *CompositeCollector) Collect(ctx context.Context) ([]TimeSeries, error)` | 采集指标 |
| `HardwareFingerprint` | `func HardwareFingerprint() (string, error)` | 硬件指纹 |

### 9.6 host_files — 文件检查处理器

**文件**: `internal/edgeagent/host_files/handlers.go`, `sandbox.go`

**用途**: 文件检查隧道处理器（读取/列出/搜索文件）。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `HandleReadFile` | `func HandleReadFile(ctx context.Context, req tunnel.HostFilesRequest) ([]byte, error)` | 读取文件 |
| `HandleListDir` | `func HandleListDir(ctx context.Context, req tunnel.HostFilesRequest) ([]FileInfo, error)` | 列出目录 |

### 9.7 k8s — K8s 集成

**文件**: `internal/edgeagent/k8s/actions.go`, `identity.go`, `inventory.go`, `metrics.go`, `readonly.go`, `upgrade_prepare.go`

**用途**: K8s 集成（身份注册/清单收集/指标网关/只读查询/升级准备）。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `RegisterIdentity` | `func RegisterIdentity(ctx context.Context, cfg K8sConfig) error` | 注册 K8s 身份 |
| `CollectInventory` | `func CollectInventory(ctx context.Context) ([]byte, error)` | 收集集群清单 |
| `StartMetricsGateway` | `func StartMetricsGateway(ctx context.Context, cfg MetricsGatewayConfig) error` | 启动指标网关 |
| `UpgradePrepare` | `func UpgradePrepare(ctx context.Context, version string) error` | 升级准备 |

### 9.8 model — 边端本地类型

**文件**: `internal/edgeagent/model/model.go`

**用途**: 边端 Agent 本地类型定义。

**核心类型**: `LocalConfig`, `PluginState`, `CollectorConfig`

### 9.9 plugins — 插件运行时框架

**文件**: `internal/edgeagent/plugins/plugin.go`, `supervisor.go`, `subprocess.go`, `config_env.go`, `config_tunnel.go`

**用途**: 插件运行时框架（子进程管理 + 配置注入 + 健康监控）。

**核心类型**:

| 类型 | 文件 | 说明 |
|------|------|------|
| `Supervisor` | supervisor.go | 插件监控器（启动/停止/重启/健康检查） |
| `Plugin` | plugin.go | 插件定义 |
| `Subprocess` | subprocess.go | 子进程管理器 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `NewSupervisor` | `func NewSupervisor(cfg SupervisorConfig) *Supervisor` | 构建监控器 |
| `Start` | `func (s *Supervisor) Start(ctx context.Context) error` | 启动所有插件 |
| `Stop` | `func (s *Supervisor) Stop(ctx context.Context) error` | 停止所有插件 |
| `Restart` | `func (s *Supervisor) Restart(ctx context.Context, name string) error` | 重启指定插件 |

### 9.10 plugins/logs — Promtail 日志插件

**文件**: `internal/edgeagent/plugins/logs/plugin.go`, `render.go`

**用途**: Promtail 日志采集插件（配置渲染 + 子进程管理）。

### 9.11 plugins/metrics — 指标采集插件

**文件**: `internal/edgeagent/plugins/metrics/plugin.go`, `scrape.go`

**用途**: 进程内指标采集插件（Prometheus 抓取）。

### 9.12 plugins/traces — 追踪采集插件

**文件**: `internal/edgeagent/plugins/traces/plugin.go`, `render.go`

**用途**: Otelcol-contrib 追踪采集插件（配置渲染 + 子进程管理）。

### 9.13 restart_service — 服务重启处理器

**文件**: `internal/edgeagent/restart_service/handlers.go`

**用途**: 服务重启隧道处理器。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `HandleRestartService` | `func HandleRestartService(ctx context.Context, req tunnel.RestartServiceRequest) error` | 重启服务 |

### 9.14 service — 主机负载处理器

**文件**: `internal/edgeagent/service/handlers.go`

**用途**: get_host_load / get_process_list 隧道处理器。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `HandleGetHostLoad` | `func HandleGetHostLoad(ctx context.Context) (*HostLoad, error)` | 获取主机负载 |
| `HandleGetProcessList` | `func HandleGetProcessList(ctx context.Context) ([]Process, error)` | 获取进程列表 |

### 9.15 skill — Skill 分发

**文件**: `internal/edgeagent/skill/dispatcher.go`

**用途**: Skill 分发器（按名称路由到已注册的 Skill 处理器）。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `Dispatch` | `func (d *Dispatcher) Dispatch(ctx context.Context, name string, args map[string]interface{}) (interface{}, error)` | 分发 Skill 调用 |

### 9.16 webshell — WebShell 处理器

**文件**: `internal/edgeagent/webshell/handler.go`

**用途**: TCP 流转发 SSH 处理器（WebSocket → SSH 隧道）。

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `HandleWebshell` | `func HandleWebshell(ctx context.Context, sessionID string, conn net.Conn) error` | 处理 WebShell 连接 |

---

## 10. internal/skill/ — Skill 框架

### 10.1 核心框架

**文件**: `internal/skill/types.go`, `registry.go`, `schema.go`, `loader.go`, `subprocess.go`

**用途**: 元数据驱动的 L2 设备能力框架，安全级别分类（safe/mutating/dangerous）。

**核心类型**:

| 类型 | 文件 | 说明 |
|------|------|------|
| `Skill` | types.go | 技能定义（Name/Description/Class/Parameters/Handler） |
| `Class` | types.go | 安全级别（Safe/Mutating/Dangerous） |
| `Registry` | registry.go | 技能注册表 |
| `Schema` | schema.go | 参数 JSON Schema |
| `Loader` | loader.go | SKILL.md 描述符加载器 |
| `SubprocessSkill` | subprocess.go | 子进程技能执行器 |

**核心函数**:

| 函数 | 签名 | 说明 |
|------|------|------|
| `NewRegistry` | `func NewRegistry() *Registry` | 构建注册表 |
| `Register` | `func (r *Registry) Register(s Skill) error` | 注册技能 |
| `Resolve` | `func (r *Registry) Resolve(query string) ([]Skill, error)` | 按查询匹配技能 |
| `Execute` | `func (r *Registry) Execute(ctx context.Context, name string, args map[string]interface{}) (interface{}, error)` | 执行技能 |
| `LoadFromDir` | `func LoadFromDir(dir string) ([]Skill, error)` | 从目录加载 SKILL.md |

### 10.2 builtin — 内置技能

**文件**: `internal/skill/builtin/probe_dns.go`, `probe_http.go`, `probe_tcp.go`, `read_journal.go`, `tail_file.go`, `web_search.go`

**用途**: 6 个内置技能（DNS 探测/HTTP 探测/TCP 探测/日志读取/文件追踪/Web 搜索）。

**核心函数**:

| 函数 | 文件 | 签名 | 说明 |
|------|------|------|------|
| `ProbeDNS` | probe_dns.go | `func ProbeDNS(ctx context.Context, host string) (*DNSResult, error)` | DNS 探测 |
| `ProbeHTTP` | probe_http.go | `func ProbeHTTP(ctx context.Context, url string, opts ...HTTPOption) (*HTTPResult, error)` | HTTP 探测 |
| `ProbeTCP` | probe_tcp.go | `func ProbeTCP(ctx context.Context, addr string, timeout time.Duration) error` | TCP 探测 |
| `ReadJournal` | read_journal.go | `func ReadJournal(ctx context.Context, unit string, lines int) ([]string, error)` | 读取 systemd 日志 |
| `TailFile` | tail_file.go | `func TailFile(ctx context.Context, path string, lines int) ([]string, error)` | 追踪文件尾部 |
| `WebSearch` | web_search.go | `func WebSearch(ctx context.Context, query string, limit int) ([]SearchResult, error)` | Web 搜索 |

---

## 11. web/src/ — 前端

### 11.1 api/ — API 客户端

**用途**: 30+ API 客户端文件，每个对应一个后端服务。

**核心文件**:

| 文件 | 说明 |
|------|------|
| `chat.ts` | AI 聊天会话 + 消息 API |
| `alerts.ts` | 告警规则 API |
| `devices.ts` | 设备 API |
| `edges.ts` | 边端节点 API |
| `flows.ts` | 流程 API |
| `k8s.ts` | K8s 集群 API |
| `knowledge.ts` | 知识库 API |
| `marketplace.ts` | 技能市场 API |
| `mcp.ts` | MCP 服务器 API |
| `metrics.ts` | 指标查询 API |
| `monitors.ts` | 监控面板 API |
| `reports.ts` | 定时报告 API |
| `secrets.ts` | 密钥保险库 API |
| `settings.ts` | 系统设置 API |
| `topology.ts` | 拓扑图 API |
| `auth.ts` | 认证 API |
| `audit.ts` | 审计日志 API |
| `approvals.ts` | 审批 API |

### 11.2 components/ — React 组件

**用途**: 20+ React 组件，遵循 zinc/indigo 配色规范。

**核心目录**:

| 目录 | 说明 |
|------|------|
| `ui/` | 基础 UI 组件（Card/Chip/Button/PageHeader/EmptyState/Modal/Table） |
| `chat/` | AI 聊天组件（ChatPanel/MessageBubble/ToolCallCard/MentionInput） |
| `alert/` | 告警组件（AlertRuleCard/AlertDetail/AlertDraftPreview） |
| `device/` | 设备组件（DeviceCard/DeviceDetail） |
| `topology/` | 拓扑组件（TopologyGraph/ServiceNode） |
| `flow/` | 流程组件（FlowEditor/NodeCard） |
| `monitor/` | 监控组件（PanelGrid/MetricChart） |

### 11.3 pages/ — 路由页面

**用途**: 30+ 路由页面。

**核心页面**:

| 页面 | 说明 |
|------|------|
| `ChatPage` | AI 聊天主页面 |
| `AlertsPage` | 告警规则列表 |
| `AlertDetailPage` | 告警详情 |
| `DevicesPage` | 设备列表 |
| `DeviceDetailPage` | 设备详情 |
| `MonitorPage` | 监控面板 |
| `TopologyPage` | 拓扑图 |
| `FlowsPage` | 流程列表 |
| `FlowEditorPage` | 流程编辑器 |
| `KnowledgePage` | 知识库管理 |
| `MarketplacePage` | 技能市场 |
| `SettingsPage` | 系统设置 |
| `ReportsPage` | 定时报告 |
| `AuditPage` | 审计日志 |
| `ApprovalsPage` | 审批收件箱 |

### 11.4 store/ — Zustand 状态管理

**用途**: 9 个 Zustand store。

**核心 Store**:

| Store | 说明 |
|-------|------|
| `useAuthStore` | 认证状态（token/user/role） |
| `useChatStore` | 聊天状态（sessions/messages/streaming） |
| `useAlertStore` | 告警状态 |
| `useDeviceStore` | 设备状态 |
| `useSettingStore` | 设置状态 |
| `useUIStore` | UI 状态（sidebar/theme/locale） |
| `useTopologyStore` | 拓扑状态 |
| `useFlowStore` | 流程状态 |
| `useMonitorStore` | 监控状态 |

### 11.5 lib/ — 工具库

**用途**: 15+ 工具文件。

**核心文件**:

| 文件 | 说明 |
|------|------|
| `api.ts` | API 客户端基础（fetch 封装 + 错误处理） |
| `sse.ts` | SSE 客户端（fetch + ReadableStream 手动帧解析） |
| `auth.ts` | 认证工具（token 存储/刷新） |
| `format.ts` | 格式化工具（时间/数字/字节） |
| `i18n.ts` | 国际化工具 |
| `theme.ts` | 主题工具（light/dark 切换） |
| `cn.ts` | CSS 类名合并工具 |

### 11.6 i18n/ — 国际化

**用途**: 多语言支持（中文/英文），使用 `tr('中文','English')` 模式。

**核心文件**:

| 文件 | 说明 |
|------|------|
| `zh-CN.ts` | 中文翻译 |
| `en-US.ts` | 英文翻译 |
| `index.ts` | 国际化初始化 |

---

## 附录：架构层次关系

```
cmd/ongrid/main.go          ← 入口：依赖注入 + 组装
├── internal/iam/            ← IAM BC（独立限界上下文）
│   ├── biz/                 ← 业务逻辑（authz/membership/org/user）
│   ├── data/                ← 数据持久化（GORM）
│   ├── model/               ← 领域模型
│   ├── server/              ← HTTP 路由
│   └── service/             ← 服务门面
├── internal/manager/        ← 云端管理器 BC
│   ├── biz/                 ← 业务逻辑（29 个子包）
│   │   ├── aiops/           ← AI 运维核心
│   │   │   ├── agent/       ← 旧版 Agent 循环
│   │   │   ├── chatruntime/ ← 新版 eino 图内核
│   │   │   ├── graph/       ← ReAct 图 + 回调链
│   │   │   ├── tools/       ← 工具注册表
│   │   │   ├── mentions/    ← @-提及搜索
│   │   │   └── alertdraft/  ← 告警草案编译器
│   │   ├── alert/           ← 告警管理
│   │   ├── flow/            ← DAG 流程引擎
│   │   ├── report/          ← 定时报告
│   │   └── ...              ← 其他 25 个业务子包
│   ├── data/                ← 数据持久化（21 个子包）
│   ├── model/               ← 领域模型（21 个子包）
│   ├── server/              ← HTTP 路由（18 个子包）
│   └── service/             ← 服务门面（18 个子包）
├── internal/edgeagent/      ← 边端 Agent
│   ├── bash/                ← bash 隧道处理器
│   ├── biz/                 ← Agent 核心循环
│   ├── cmdpolicy/           ← 命令策略沙箱
│   ├── collector/           ← 双模式采集器
│   ├── host_files/          ← 文件检查处理器
│   ├── k8s/                 ← K8s 集成
│   ├── plugins/             ← 插件运行时框架
│   ├── skill/               ← Skill 分发
│   └── webshell/            ← WebShell 处理器
├── internal/skill/          ← Skill 框架
│   ├── builtin/             ← 6 个内置技能
│   └── (核心框架)            ← Registry/Loader/Schema
├── internal/pkg/            ← 公共基础包（30 个）
│   ├── auth/                ← JWT 认证
│   ├── authzmw/             ← Casbin 授权中间件
│   ├── llm/                 ← LLM 多供应商客户端
│   ├── tunnel/              ← 边云隧道
│   ├── notify/              ← 多通道通知
│   └── ...                  ← 其他 25 个公共包
└── web/src/                 ← React 前端
    ├── api/                 ← 30+ API 客户端
    ├── components/          ← 20+ React 组件
    ├── pages/               ← 30+ 路由页面
    ├── store/               ← 9 个 Zustand store
    ├── lib/                 ← 15+ 工具文件
    └── i18n/                ← 国际化
```
