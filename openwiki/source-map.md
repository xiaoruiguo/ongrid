# Source Map

> 关键文件索引：按功能定位源码。

## 入口

| 功能 | 文件 | 行号 |
|------|------|------|
| 云端主入口 | `cmd/ongrid/main.go` | — |
| 边端主入口 | `cmd/ongrid-edge/main.go` | — |
| 前端入口 | `web/src/main.tsx` | — |
| 前端路由 | `web/src/App.tsx` | L98 (chat route) |
| Makefile | `Makefile` | — |

## 中间件链

| 中间件 | 文件 | 行号 |
|--------|------|------|
| OTel HTTP | `cmd/ongrid/main.go` | L2706-2715 |
| Metrics | `internal/manager/server/middleware/metrics.go` | L22 |
| Audit | `internal/manager/server/middleware/audit.go` | L72 |
| Auth (JWT) | `internal/pkg/auth/middleware.go` | L21-53 |
| Authz (Casbin) | `internal/pkg/authzmw/middleware.go` | L70-96 |
| tenantctx | `internal/pkg/tenantctx/tenantctx.go` | L33-86 |
| edgeauth | `internal/manager/server/edgeauth/http.go` | L67 |
| requireAdmin | `internal/manager/server/edge/http.go` | L315 |

详见 [ongrid_middleware.md](../ongrid_middleware.md)。

## AI 对话（AIOps）

| 功能 | 文件 | 行号 |
|------|------|------|
| SSE handler | `internal/manager/server/aiops/http.go` | — |
| 双内核分发 | `internal/manager/service/aiops/service.go` | L334-376 |
| 10 步编排 | `internal/manager/biz/aiops/chatruntime/runtime.go` | L468-851 |
| ReAct 图 | `internal/manager/biz/aiops/graph/react.go` | L78-188 |
| Callback 链 | `internal/manager/biz/aiops/graph/callbacks/chain.go` | L88-118 |
| LLM 路由 | `internal/pkg/llm/eino_routing.go` | — |
| MultiClient | `internal/pkg/llm/router.go` | L67-87 |
| Session 模型 | `internal/manager/model/aiops/model.go` | L49-73 |
| Session repo | `internal/manager/data/aiops/store/session.go` | L31 |

## 认证授权（IAM）

| 功能 | 文件 | 行号 |
|------|------|------|
| JWT 签发/验证 | `internal/pkg/auth/jwt.go` | L24-30, L84-99 |
| 密码哈希 (argon2id) | `internal/iam/biz/user/hash.go` | — |
| Casbin Enforcer | `internal/iam/biz/authz/authz.go` | — |
| User usecase | `internal/iam/biz/user/usecase.go` | — |
| Org usecase | `internal/iam/biz/org/usecase.go` | — |
| IAM HTTP | `internal/iam/server/http.go` | — |
| User model | `internal/iam/model/model.go` | — |

## 告警

| 功能 | 文件 |
|------|------|
| 告警管道 | `internal/manager/biz/alert/pipeline.go` |
| 告警路由 | `internal/manager/biz/alert/router.go` |
| 告警抑制 | `internal/manager/biz/alert/inhibit.go` |
| 告警规则 | `internal/manager/biz/alert/rules.go` |
| 告警重试 | `internal/manager/biz/alert/retry.go` |
| 告警模型 | `internal/manager/model/alert/model.go` |
| 告警 HTTP | `internal/manager/server/alert/http.go` |

## 边端

| 功能 | 文件 |
|------|------|
| Agent 生命周期 | `internal/edgeagent/biz/agent.go` |
| 指标采集 | `internal/edgeagent/collector/scrape.go` |
| K8s 资源 | `internal/edgeagent/k8s/inventory.go` |
| K8s 指标 | `internal/edgeagent/k8s/metrics.go` |
| K8s 只读 | `internal/edgeagent/k8s/readonly.go` |
| 反向隧道 | `internal/pkg/tunnel/client.go` |
| Webshell | `internal/edgeagent/webshell/handler.go` |
| 技能分发 | `internal/edgeagent/skill/dispatcher.go` |
| 内置技能 | `internal/skill/builtin/` |

## 前端

| 功能 | 文件 |
|------|------|
| 路由声明 | `web/src/App.tsx` |
| 首页（会话创建） | `web/src/pages/Home.tsx` |
| 对话页 | `web/src/pages/ChatThread.tsx` |
| API 客户端 | `web/src/api/client.ts` |
| Chat API | `web/src/api/chat.ts` |
| Auth store | `web/src/store/auth.ts` |
| Model 选择 | `web/src/store/modelSelection.ts` |
| 聊天输入 | `web/src/components/ChatInput.tsx` |
| 消息渲染 | `web/src/components/MessageBubble.tsx` |
| 布局 | `web/src/components/Layout.tsx` |
| 侧边栏 | `web/src/components/Sidebar.tsx` |

## 前端 API 层

`web/src/api/` 目录下按业务域拆分：`agents.ts`、`aiops.ts`、`alerts.ts`、`auth.ts`、`chat.ts`、`client.ts`、`devices.ts`、`edges.ts`、`flows.ts`、`grafana.ts`、`knowledge.ts`、`kubernetes.ts`、`logs.ts`、`mcp.ts`、`monitorPanels.ts`、`orgs.ts`、`pages.ts`、`prom.ts`、`reports.ts`、`secrets.ts`、`settings.ts`、`skills.ts`、`tasks.ts`、`topology.ts`、`traces.ts`、`users.ts`、`webshell.ts`

## 配置与部署

| 功能 | 文件 |
|------|------|
| Docker Compose | `deploy/docker-compose.yml` |
| 环境变量模板 | `deploy/.env.example` |
| nginx 配置 | `deploy/nginx.conf` |
| Prometheus | `deploy/prometheus.yml` |
| 告警规则 | `deploy/prometheus-rules.yml` |
| Loki | `deploy/loki-config.yaml` |
| Tempo | `deploy/tempo-config.yaml` |
| Frontier | `deploy/frontier.yaml` |
| SearXNG | `deploy/searxng/settings.yml` |
| K8s Helm chart | `deploy/kubernetes/ongrid-edge/` |
| Dockerfile (manager) | `deploy/Dockerfile.ongrid` |
| Dockerfile (edge) | `deploy/Dockerfile.ongrid-edge` |
| Dockerfile (web) | `deploy/Dockerfile.web` |
| Dockerfile (frontier) | `deploy/Dockerfile.frontier` |
| 安装脚本 | `deploy/install/install.sh` |

## 已有分析文档

仓库根目录下的分析文档（非 openwiki，但可参考）：

| 文档 | 主题 |
|------|------|
| `ongrid_middleware.md` | 中间件技术实现 |
| `ongrid_route_chat.md` | /chat/:sessionId 全链路 |
| `ongrid_startsession.md` | startSession 全链路 |
| `ongrid_LLM.md` | LLM 路由实现 |
| `ongrid_iam.md` | IAM 模块 |
| `ongrid_architecture.md` | 架构总览 |
| `ongrid_sse.md` | SSE 流式 |
| `ongrid_eino.md` | Eino 框架 |
| `ongrid_frontier.md` | Frontier broker |
| `ongrid_make_targets.md` | Makefile 全目标 |
