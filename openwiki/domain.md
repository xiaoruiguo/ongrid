# Domain Concepts

> 领域概念与术语表。

## 核心概念

| 术语 | 含义 | 源码位置 |
|------|------|----------|
| **Manager** | 云端控制平面，承载全部业务逻辑和 AI Agent | `cmd/ongrid/` |
| **Edge** | 边端数据平面，采集指标/日志/追踪 | `cmd/ongrid-edge/` |
| **Frontier** | gRPC broker，边端通过反向隧道连接 | `deploy/Dockerfile.frontier` |
| **BC** | Bounded Context，限界上下文（iam / manager / edgeagent） | `.go-arch-lint.yml` |
| **Tenant** | 租户上下文（UserID/Email/Role/IsSuperuser） | `internal/pkg/tenantctx/tenantctx.go` |
| **Session** | AI 对话会话 | `internal/manager/model/aiops/model.go` L49 |
| **RCA** | Root Cause Analysis，根因分析 | `agents/incident-investigator.md` |
| **AIOps** | AI 运维，OnGrid 的核心能力 | `internal/manager/biz/aiops/` |

## AI/Agent 领域

| 术语 | 含义 |
|------|------|
| **ReAct Graph** | Reasoning + Acting 图，AI Agent 的推理执行循环 |
| **Dual Kernel** | 双内核：Graph 内核（Eino ReAct）+ Legacy 内核（直连 LLM） |
| **Callback Chain** | 6-handler 回调链：AlertDraftGuard → Persistence → SSE → Audit → Metrics → Budget |
| **Pseudo-streaming** | clientChatModel.Stream() 将 Generate() 包装为单 chunk StreamReader |
| **HOIST** | 将 chat turn 从 HTTP 请求生命周期分离的机制 |
| **assistantIDRelay** | atomic.Pointer[string]，跨 handler 传递 assistant ID |
| **MultiClient** | 多 LLM 客户端管理，60s TTL 缓存 |

## 认证授权

| 术语 | 含义 |
|------|------|
| **JWT Dual Token** | access token (15m) + refresh token (720h)，Claims 相同仅 TTL 不同 |
| **Casbin RBAC** | 四元组 (sub, dom, obj, act)，3 角色 (org_admin/member/viewer) |
| **Superuser Bypass** | IsSuperuser 绕过 Casbin 检查，防止策略锁死管理员 |
| **auditSlot** | *pointer 类型，审计中间件跨 r.WithContext() 存活 |
| **tenantctx slot** | *pointer 类型，外层中间件读取内层中间件写入的租户信息 |
| **edgeauth** | 数据平面认证（Basic Auth + nginx auth_request），与 JWT 分离 |

## 告警

| 术语 | 含义 |
|------|------|
| **Alert Rule** | 告警规则，PromQL 表达式 + 通知渠道 |
| **Incident** | 告警触发的 incident，可 ack/resolve/silence |
| **Inhibit** | 告警抑制（同类告警聚合） |
| **Pipeline** | 告警评估管道（评估 → 路由 → 通知） |

## 边端

| 术语 | 含义 |
|------|------|
| **Reverse Tunnel** | 反向隧道，边端主动拨出连接云端，零入站端口 |
| **Plugin** | 边端插件（promtail/otelcol/node_exporter/process_exporter） |
| **Skill** | 内置技能（probe_dns/http/tcp、tail_file、web_search、read_journal） |
| **Webshell** | 浏览器 SSH，通过反向隧道，无需跳板机 |

## 部署

| 术语 | 含义 |
|------|------|
| **Compose Install** | Docker Compose 部署（`deploy/docker-compose.yml`） |
| **K8s Edge** | Kubernetes 边端部署（`deploy/kubernetes/ongrid-edge/` Helm chart） |
| **CNB** | CNB 镜像仓库（`docker.cnb.cool/ongridio/`） |
| **Edge Bundle** | ADR-024 边端升级包（`dist/build-edge-bundle.sh`） |
