# OnGrid API 接口使用说明文档

> 本文档系统说明 OnGrid 平台对外暴露的全部 API 接口，覆盖：认证机制、统一响应格式、错误码、IAM（登录/用户/组织）、Edge 设备管理、K8s 集群、AIOps 对话、告警、指标、监控、知识库、报表、Flow、WebShell、IM Bridge、Marketplace、Secret、MCP、Skill、Approval、Audit、Topology、Integrations、SystemHealth、SystemUpgrade、Prometheus 代理、内部 endpoint（edge tunnel / K8s enroll）共 26 个域。

---

## 目录

1. [总览与架构](#1-总览与架构)
2. [认证与授权](#2-认证与授权)
3. [统一响应格式与错误码](#3-统一响应格式与错误码)
4. [IAM 域：认证 / 用户 / 组织](#4-iam-域认证--用户--组织)
5. [Edge 设备管理](#5-edge-设备管理)
6. [Kubernetes 集群管理](#6-kubernetes-集群管理)
7. [AIOps 对话与会话](#7-aiops-对话与会话)
8. [告警 Incidents 与规则](#8-告警-incidents-与规则)
9. [通知渠道（Notification Channels）](#9-通知渠道notification-channels)
10. [指标查询（Host Metrics）](#10-指标查询host-metrics)
11. [监控面板（Monitor Panels）](#11-监控面板monitor-panels)
12. [知识库（Knowledge）](#12-知识库knowledge)
13. [报表与定时任务（Reports）](#13-报表与定时任务reports)
14. [Flow 编排](#14-flow-编排)
15. [WebShell（WebSSH）](#15-webshellwebssh)
16. [IM Bridge（飞书/Slack/钉钉/Telegram）](#16-im-bridge飞书slack钉钉telegram)
17. [Marketplace（技能/插件市场）](#17-marketplace技能插件市场)
18. [Secret 凭据管理](#18-secret-凭据管理)
19. [MCP Server 管理](#19-mcp-server-管理)
20. [Skill 执行](#20-skill-执行)
21. [Approval 审批](#21-approval-审批)
22. [Audit 审计日志](#22-audit-审计日志)
23. [Topology 拓扑](#23-topology-拓扑)
24. [Integrations 集成测试](#24-integrations-集成测试)
25. [SystemHealth 与 SystemUpgrade](#25-systemhealth-与-systemupgrade)
26. [Prometheus 代理与票据](#26-prometheus-代理与票据)
27. [内部 endpoint（edge tunnel / K8s enroll）](#27-internal-endpointedge-tunnel--k8s-enroll)
28. [middleware 链与审计](#28-middleware-链与审计)
29. [架构红线](#29-架构红线)
30. [关键文件索引](#30-关键文件索引)

---

## 1. 总览与架构

OnGrid 后端拆分为两个 bounded context（BC），各自有独立 HTTP server：

| BC | 监听 | 路由前缀 | 职责 |
|----|------|----------|------|
| **IAM** | `:9091`（默认） | `/v1/auth/*`、`/v1/users`、`/v1/orgs`、`/v1/self`、`/v1/me` | 登录/JWT/用户/组织/成员 |
| **Manager** | `:9090`（默认） | `/v1/*` 大部分业务 | edge/k8s/aiops/alert/metric/report/... |

**Router 选型**：`go-chi/chi/v5`（轻量、中间件友好、URL param 支持）。

**装配入口**：`cmd/ongrid/main.go` 把每个 `*Handler.Register(r)` 挂到 `protected`（鉴权）或 `public`（无需鉴权）或 `internal`（edge/tunnel 专用）router 上。

**Proto 仅作规范**：`api/**/*.proto` 是 message shape 的规范文档，**不生成 Go 代码**；handler 用手写 DTO + JSON tag 镜像 proto 形状（与 tunnel 包同样的 philosophy，便于切 protobuf binary）。

---

## 2. 认证与授权

### 2.1 JWT 双 Token

| Token | 用途 | TTL | 端点 |
|-------|------|-----|------|
| Access Token | 业务请求鉴权 | 短（默认 30min） | `POST /v1/auth/login` 返回 |
| Refresh Token | 续期 access token | 长（默认 7d） | 同上 + `POST /v1/auth/refresh` |

**Header 格式**：

```
Authorization: Bearer <access_token>
```

### 2.2 Claims 内容（ADR-003）

```json
{
  "user_id": 42,
  "email": "alice@org.com",
  "role": "admin",
  "org_id": 7,
  "exp": <unix>,
  "iat": <unix>
}
```

`org_id` / `user_id` 不在请求体中由用户提供，由 JWT claims + URL path + `internal/pkg/tenantctx` middleware 注入 context。

### 2.3 角色与权限（Casbin RBAC）

| Role | 含义 | 典型权限 |
|------|------|----------|
| `admin` | 系统管理员 | 全部 CRUD + 用户管理 |
| `owner` | 组织拥有者 | 同 admin（组织级） |
| `member` | 普通成员 | 业务读写 |
| `readonly` / `viewer` | 只读 | 仅 GET，不能写/创建 |

`internal/iam/biz/authz`（Casbin）+ handler 内 `requireAdmin` / `caller.IsViewer()` 双层校验。WebShell 等高危操作用 `h.authz.Require("device:shell", "exec/read/manage")` 精细控制。

### 2.4 登录限流（loginThrottle）

- **IP 级**：20 次/5min（NAT 容忍）
- **Email 级**：6 次/15min（防 password spray）
- 成功登录清 email 计数（IP 保留，可能正在攻击其他用户）
- 重启清空计数（单 manager MVP 不用 Redis）

### 2.5 Anonymous / Public 端点

只有少数 endpoint 不需要 JWT：

- `POST /v1/auth/login`
- `POST /v1/auth/refresh`
- `GET /r/{token}`（报表共享链接）
- `POST /v1/im/feishu/events`（飞书事件回调，签名校验）
- `GET /v1/prometheus/auth`（Prometheus 代理票据校验，nginx auth_request 调用）

---

## 3. 统一响应格式与错误码

### 3.1 成功响应

直接返回业务对象，**不包裹** `{code, message, data}` 外壳（注释明示：handler 直接 `writeJSON` 业务对象；proto 文档说 `{code, message, data}` 是规范目标，但当前实现是裸 JSON）。

```http
HTTP/1.1 200 OK
Content-Type: application/json

{"id": 1, "email": "alice@org.com", "role": "admin"}
```

### 3.2 错误响应

```http
HTTP/1.1 404 Not Found
Content-Type: application/json

{
  "error": "not found",
  "code": "not-found"
}
```

### 3.3 错误码映射表（`internal/pkg/errs`）

| Sentinel | HTTP | code 字符串 | 含义 |
|----------|------|------------|------|
| `ErrNotFound` | 404 | `not-found` | 资源不存在 |
| `ErrUnauthorized` | 401 | `unauthorized` | 未认证 / token 失效 |
| `ErrForbidden` | 403 | `forbidden` | 无权限 |
| `ErrTenantMismatch` | 403 | `forbidden` | 租户不匹配 |
| `ErrConflict` | 409 | `conflict` | 冲突（重名/状态冲突） |
| `ErrInvalid` | 400 | `invalid` | 参数错误 |
| `ErrBudgetExceeded` | 429 | `budget-exceeded` | LLM 预算超限 |
| `ErrTooManyAttempts` | 429 | — | 登录限流 |
| `ErrEdgeOffline` | 503 | `edge-offline` | edge 离线 |
| `ErrNotWiredYet` | 501 | `not-wired-yet` | 功能未装配（如 legacy kernel 缺 userAgents） |
| 其他 | 500 | `internal` | 内部错误 |

### 3.4 所有权隔离（避免存在性泄露）

非 owner / 非 admin 调用别人的 session → **404 而非 403**，避免泄露 session 是否存在（见 aiops/http.go 注释）。

---

## 4. IAM 域：认证 / 用户 / 组织

源文件：`internal/iam/server/http.go`、`orgs.go`

### 4.1 认证

#### 登录

```
POST /v1/auth/login
Content-Type: application/json

{"email": "alice@org.com", "password": "secret"}
```

**响应 200**：

```json
{
  "access_token": "eyJ...",
  "refresh_token": "eyJ...",
  "expires_in": 1800,
  "role": "admin"
}
```

失败 → 401 + 审计 `auth_login_failed`。

#### 刷新

```
POST /v1/auth/refresh

{"refresh_token": "eyJ..."}
```

返回新的 access + refresh pair。

### 4.2 用户管理（admin only）

| Method | Path | 说明 |
|--------|------|------|
| POST | `/v1/auth/register` | 注册新用户（admin） |
| GET | `/v1/self` | 当前用户简版 |
| GET | `/v1/me` | 当前用户 + memberships |
| GET | `/v1/users` | 用户列表（admin） |
| POST | `/v1/users` | 同 register（admin） |
| PATCH | `/v1/users/{id}` | 更新用户 |
| PATCH | `/v1/users/{id}/role` | 改角色（admin） |
| PATCH | `/v1/users/{id}/password` | 重置密码 |
| DELETE | `/v1/users/{id}` | 删除用户（admin） |

**注意**：`PATCH /v1/users/{id}/superuser` 已退役（2026-05），系统单一特权层 `role=admin`。

### 4.3 组织与成员

| Method | Path | 说明 |
|--------|------|------|
| GET | `/v1/orgs` | 我所属的 org 列表 |
| POST | `/v1/orgs` | 创建 org（自己成 owner） |
| PATCH | `/v1/orgs/{id}` | 更新 org |
| DELETE | `/v1/orgs/{id}` | 删除 org |
| GET | `/v1/orgs/{id}/members` | 成员列表 |
| POST | `/v1/orgs/{id}/members` | 邀请成员 |
| PATCH | `/v1/orgs/{id}/members/{user_id}` | 改成员角色 |
| DELETE | `/v1/orgs/{id}/members/{user_id}` | 移除成员 |

---

## 5. Edge 设备管理

源文件：`internal/manager/server/edge/http.go`

| Method | Path | 说明 |
|--------|------|------|
| GET | `/v1/edges` | 列表（支持 status/name 过滤 + 分页） |
| GET | `/v1/edges/{id}` | 详情 |
| GET | `/v1/edges/{id}/processes` | 实时进程列表（tunnel RPC 到 edge） |
| GET | `/v1/edges/{id}/plugins` | 插件健康 |
| GET | `/v1/integrations/plugin-counts` | 插件统计 |

**列表过滤参数**：`status_filter=online|offline|all`、`name=<模糊>`、`page`、`page_size`。

**Edge 创建**：通过 `/v1/edges` POST 创建（返回 `access_key` + `secret_key`，secret 仅此一次返回，后端存 Argon2id hash）。

**Rotate Secret**：`POST /v1/edges/{id}/rotate-secret`（旧 secret 立即失效）。

---

## 6. Kubernetes 集群管理

源文件：`internal/manager/server/k8s/http.go`

### 6.1 用户面（RegisterProtected，需 JWT）

| Method | Path | 说明 |
|--------|------|------|
| GET | `/v1/k8s/clusters` | 集群列表（status/mode/name 过滤） |
| GET | `/v1/k8s/edge-attachments` | edge↔cluster 关联 |
| GET | `/v1/k8s/clusters/{cluster_id}` | 集群详情 |
| GET | `/v1/k8s/clusters/{cluster_id}/health` | 健康汇总（degraded_workloads/pending_pods/crashloop/oom/imagepull/notready_nodes） |
| GET | `/v1/k8s/clusters/{cluster_id}/nodes` | 节点列表 |
| GET | `/v1/k8s/clusters/{cluster_id}/workloads` | 工作负载 |
| GET | `/v1/k8s/clusters/{cluster_id}/pods` | Pod 列表 |
| GET | `/v1/k8s/clusters/{cluster_id}/events` | K8s 事件 |

**通用过滤参数**：`namespace`、`kind`、`phase`、`reason`、`q=<模糊>`、`issue_only=true`（只看异常）、`limit`、`offset`。

### 6.2 创建集群

```
POST /v1/k8s/clusters
{"name": "prod-cluster", "uid": "abc-123", "mode": "full_node"}
```

返回 `cluster` + `bootstrap_token` + `install_command` + `node_bootstrap_token`。

### 6.3 内部面（RegisterInternal，bootstrap token 鉴权）

| Method | Path | 鉴权 | 说明 |
|--------|------|------|------|
| POST | `/internal/k8s/enroll` | Bootstrap Token | edge agent 注册 |
| POST | `/internal/k8s/telemetry-config` | X-Edge-Id | 刷新遥测配置 |

`Enroll` 返回 `edge_id` + `access_key` + `secret_key` + `cloud_addr` + `manager_public_url`，edge agent 用这些连 frontier。

---

## 7. AIOps 对话与会话

源文件：`internal/manager/server/aiops/http.go`

### 7.1 Session 管理

| Method | Path | 说明 |
|--------|------|------|
| POST | `/v1/chat/sessions` | 创建会话 |
| GET | `/v1/chat/sessions` | 列表（支持 `related_incident_id` 过滤） |
| GET | `/v1/chat/sessions/{id}/messages` | 完整历史 |
| DELETE | `/v1/chat/sessions/{id}` | 硬删除（rows + messages + tool_calls） |
| PATCH | `/v1/chat/sessions/{id}` | 重命名（`{"title": "..."}`） |
| POST | `/v1/chat/sessions/{id}/stop` | 中断当前 turn（Esc） |

**CreateSession Body**：

```json
{
  "title": "诊断 CPU 飙高",
  "scope": ["edge_1", "edge_2"],
  "related_incident_id": 42,
  "agent_id": "sre-diagnostic"
}
```

### 7.2 发送消息

#### 阻塞模式

```
POST /v1/chat/sessions/{id}/messages

{
  "content": "查看 edge-1 的 CPU 情况",
  "provider": "deepseek",
  "model": "deepseek-chat",
  "mentions": [{"type": "edge", "id": "1", "label": "edge-1"}],
  "web_search_enabled": true,
  "locale": "zh-CN"
}
```

**响应**：

```json
{
  "session_id": "abc",
  "assistant_message": {"id": "msg-2", "content": "...", "created_at": "..."},
  "tool_calls": [
    {"name": "query_promql", "edge_id": 1, "status": "success", "duration_ms": 320}
  ],
  "usage": {"prompt_tokens": 800, "completion_tokens": 200, "total_tokens": 1000},
  "iterations": 3
}
```

#### 流式模式（SSE）

```
POST /v1/chat/sessions/{id}/messages/stream
```

**响应**：`Content-Type: text/event-stream`，每帧 `event: <type>\ndata: <json>\n\n`。

**Frame 类型**：

| event | 含义 |
|-------|------|
| `assistant` | 一轮 assistant 消息已持久化（含 content / pending_tool_calls） |
| `tool_start` | 工具开始执行 |
| `tool_end` | 工具完成（status: success/error/timeout + result） |
| `approval_pending` | 高危操作待审批 |
| `task_notification` | 子任务通知 |
| `done` | 终答（含 usage/iterations） |
| `summary` | 兜底 summary（agent 未发 done 时） |
| `error` | 流中错误（不换状态码，headers 已发） |

**关键 header**：
- `X-Accel-Buffering: no`（禁 nginx buffering）
- 首帧 `: ok\n\n` 立即 hint 连接存活

### 7.3 辅助 endpoint

| Method | Path | 说明 |
|--------|------|------|
| GET | `/v1/usage/today` | 当日 token 用量汇总 |
| GET | `/v1/aiops/mutating-proposals` | ReviewGate 提案审计（filter: `tool_name`/`decision`） |
| GET | `/v1/aiops/mentions/search` | @-mention 搜索（`q`/`type`/`limit`） |
| GET | `/v1/aiops/models` | LLM provider catalog + default |
| POST | `/v1/aiops/query-translate` | 自然语言 → LogQL/TraceQL/PromQL |

### 7.4 Agent Inventory（Phase 1+3）

| Method | Path | 说明 |
|--------|------|------|
| GET | `/v1/agents` | 所有 persona |
| GET | `/v1/agents/{name}` | 单个 persona |
| POST | `/v1/agents/custom` | 创建用户 persona（非 viewer） |
| PATCH | `/v1/agents/custom/{name}` | 更新 |
| DELETE | `/v1/agents/custom/{name}` | 删除用户 persona |
| DELETE | `/v1/agents/{name}` | 通用删除（builtin 拒绝；disk 是 session-scoped；user 走 DB） |

`Source` 字段：`builtin` / `disk` / `user`，决定 SPA 是否显示编辑/删除按钮。

---

## 8. 告警 Incidents 与规则

源文件：`internal/manager/server/alert/http.go`

### 8.1 Incidents

| Method | Path | 说明 |
|--------|------|------|
| GET | `/v1/alerts/incidents` | 列表（`status_filter`/`severity_filter`/`query`/分页） |
| GET | `/v1/alerts/incidents/{id}` | 详情 |
| GET | `/v1/alerts/incidents/{id}/events` | 事件流 |
| GET | `/v1/alerts/incidents/{id}/investigation` | 查看调查 |
| POST | `/v1/alerts/incidents/{id}/investigation` | 触发 AI 调查 |
| POST | `/v1/alerts/incidents/{id}/ack` | 确认 |
| POST | `/v1/alerts/incidents/{id}/resolve` | 解决 |
| POST | `/v1/alerts/incidents/{id}/silence` | 静音 |
| GET | `/v1/alerts/runtime-info` | 运行时信息 |

### 8.2 Alert Rules（admin only）

| Method | Path | 说明 |
|--------|------|------|
| GET | `/v1/alert-rules` | 列表 |
| GET | `/v1/alert-rules/{id}` | 详情 |
| POST | `/v1/alert-rules` | 创建 |
| POST | `/v1/alert-rules/preview` | 预览（不落库） |
| PUT | `/v1/alert-rules/{id}` | 更新 |
| POST | `/v1/alert-rules/{id}/enabled` | 启用/禁用 |
| DELETE | `/v1/alert-rules/{id}` | 删除 |

---

## 9. 通知渠道（Notification Channels）

源文件：`internal/manager/server/alert/http.go`（同包）

| Method | Path | 说明 |
|--------|------|------|
| GET | `/v1/notification-channels` | 列表 |
| GET | `/v1/notification-channels/{id}` | 详情 |
| POST | `/v1/notification-channels` | 创建 |
| PUT | `/v1/notification-channels/{id}` | 更新 |
| DELETE | `/v1/notification-channels/{id}` | 删除 |
| POST | `/v1/notification-channels/{id}/test` | 测试发送 |

**Channel Type**：`WEBHOOK` / `SLACK` / `FEISHU` / `DINGTALK`。

`endpoint_masked` 字段：脱敏后的 endpoint（如 `https://hooks.slack.com/services/T.../B.../***`），完整 endpoint 不回显。

---

## 10. 指标查询（Host Metrics）

源文件：`internal/manager/server/metric/http.go`、`prom_handler.go`

| Method | Path | 说明 |
|--------|------|------|
| GET | `/v1/edges/{id}/metrics` | 时序点查询 |

**Query 参数**：`from`、`to`（RFC3339）、`resolution=AUTO|RAW|M5|H1`。

**Resolution 选择规则（ADR-004）**：
- AUTO + ≤6h → RAW（10s）
- AUTO + ≤7d → M5（5min 聚合）
- AUTO + >7d → H1（1h 聚合）

**响应**：`{edge_id, resolution, points: [{ts, cpu_pct, mem_pct, load1, load5, load15, net_rx_bps, net_tx_bps, disk_used_pct}]}`。

`prom_handler.go` 还提供 `/api/v1/query` 等 Prom 兼容端点供 Grafana datasource 用。

---

## 11. 监控面板（Monitor Panels）

源文件：`internal/manager/server/monitor/http.go`

| Method | Path | 说明 |
|--------|------|------|
| GET | `/v1/monitor/panels` | 面板列表 |
| POST | `/v1/monitor/panels` | 创建 |
| PATCH | `/v1/monitor/panels/{id}` | 更新 |
| DELETE | `/v1/monitor/panels/{id}` | 删除 |

用户自定义面板会通过 `grafana.SyncMonitorPanels` 镜像到 Grafana `ongrid-monitor` dashboard（与 `coreMonitorPanels` 合并）。

---

## 12. 知识库（Knowledge）

源文件：`internal/manager/server/knowledge/http.go`

| Method | Path | 说明 |
|--------|------|------|
| GET | `/v1/knowledge/docs` | 文档列表 |
| GET | `/v1/knowledge/docs/{id}` | 文档详情 |
| GET | `/v1/knowledge/search` | RAG 搜索（`q` + 过滤） |
| GET | `/v1/knowledge/paths` | 路径列表 |
| GET | `/v1/knowledge/repos` | 代码仓库列表 |
| GET | `/v1/knowledge/ssh-identities` | SSH 身份列表 |

**Search 响应**：top-k 命中（score + payload），payload 含 `source_type` / `title` / `content` / `url` / `repo_id`。

---

## 13. 报表与定时任务（Reports）

源文件：`internal/manager/server/report/http.go`

### 13.1 Protected

| Method | Path | 说明 |
|--------|------|------|
| GET | `/v1/reports` | 报表列表 |
| GET | `/v1/reports/{id}` | 详情 |
| GET | `/v1/report-schedules` | 定时计划列表 |
| GET | `/v1/report-schedules/{id}` | 计划详情 |
| GET | `/v1/tasks` | 任务列表 |
| GET | `/v1/tasks/{id}` | 任务详情 |

### 13.2 Public（共享链接）

| Method | Path | 说明 |
|--------|------|------|
| GET | `/r/{token}` | token 共享报表（无需 JWT） |

报表通过 `delivery.go` 投递到 IM/邮件，token 链接让外部用户也能查看。

---

## 14. Flow 编排

源文件：`internal/manager/server/flow/http.go`

| Method | Path | 说明 |
|--------|------|------|
| GET | `/v1/flows` | Flow 列表 |
| GET | `/v1/flows/{id}` | 详情 |
| GET | `/v1/flows/{id}/runs` | 运行历史 |
| GET | `/v1/flow-runs/{run_id}` | 单次运行详情 |
| GET | `/v1/flow-tools` | 可用工具 |
| GET | `/v1/flow-node-types` | 节点类型 |

Flow 可由 LLM 自然语言生成（`biz/flow/generate.go`），也可手动编辑。

---

## 15. WebShell（WebSSH）

源文件：`internal/manager/server/webshell/http.go`

| Method | Path | 鉴权 | 说明 |
|--------|------|------|------|
| GET | `/v1/devices/{device_id}/shell` | `device:shell:exec` + WebSocket upgrade | 打开 shell |
| GET | `/v1/webshell/sessions` | `device:shell:read` | 活跃会话列表 |
| DELETE | `/v1/webshell/sessions/{id}` | `device:shell:manage` | 强制结束会话 |

### 15.1 WebSocket 协议

- **Subprotocol**：`ongrid.shell.v1`
- **首帧**：客户端发 `{"type":"open","cols":80,"rows":24,"term":"xterm-256color","ssh_user":"root","ssh_pass":"..."}`
- **后续控制帧**：`{"type":"resize","cols":120,"rows":40}` / `{"type":"close"}`
- **二进制帧**：stdin 字节
- **服务端文本帧**：`{"type":"stdout","data":"<base64>"}` / `{"type":"exit","code":0}`

### 15.2 限制

- `MaxSessionsPerUser = 5`
- `MaxSessionsPerDevice = 5`
- `IdleTimeout = 15min`（无输入自动关闭）
- `SSHPass` one-shot，dial 后立即 wipe

### 15.3 路径

```
浏览器 WS → manager openShell
   ↓ frontier OpenStream(edgeID)
   ↓ ssh.NewClientConn(stream, "127.0.0.1:22", cfg)
   ↓ sess.Shell() + io.Copy 双向
edge agent AcceptStream → dial 127.0.0.1:22 → sshd
```

---

## 16. IM Bridge（飞书/Slack/钉钉/Telegram）

源文件：`internal/manager/server/imbridge/http.go`

### 16.1 Protected（管理 IM App）

| Method | Path | 说明 |
|--------|------|------|
| GET | `/v1/im/apps` | IM 应用列表 |
| POST | `/v1/im/apps` | 创建 |
| GET | `/v1/im/apps/{id}` | 详情 |
| PUT | `/v1/im/apps/{id}` | 更新 |
| DELETE | `/v1/im/apps/{id}` | 删除 |
| POST | `/v1/im/apps/{id}/reveal` | 显示明文 secret |

### 16.2 Public（事件回调）

| Method | Path | 鉴权 | 说明 |
|--------|------|------|------|
| POST | `/v1/im/feishu/events` | 签名校验（`X-Lark-Signature`） | 飞书事件流 |

飞书签名校验用 `X-Lark-Request-Timestamp` + `X-Lark-Request-Nonce` + `X-Lark-Signature` + app secret 计算。

IM 消息进来 → bridge → 转 chat request → AIOps kernel；LLM 响应 → `send_im_message_basetool` 推回 IM。

---

## 17. Marketplace（技能/插件市场）

源文件：`internal/manager/server/marketplace/http.go`

| Method | Path | 说明 |
|--------|------|------|
| POST | `/v1/marketplace/install` | 从 registry 安装 pack |
| POST | `/v1/marketplace/upload` | 上传 tar.gz pack |
| GET | `/v1/marketplace/installed` | 已安装列表 |
| DELETE | `/v1/marketplace/installed/{pack_id}` | 卸载 |
| PUT | `/v1/marketplace/installed/{pack_id}/bindings` | 设置绑定（哪些 edge 启用） |
| GET | `/v1/marketplace/registries` | 可用 registry 列表 |

Pack 含 skill / agent / plugin / claude command 等多种类型，签名校验在 `biz/marketplace/signature.go`。

---

## 18. Secret 凭据管理

源文件：`internal/manager/server/secret/http.go`

| Method | Path | 说明 |
|--------|------|------|
| GET | `/v1/secrets` | 凭据列表（不返回明文） |
| POST | `/v1/secrets` | 创建 |
| PUT | `/v1/secrets/{id}` | 更新 |
| DELETE | `/v1/secrets/{id}` | 删除 |
| GET | `/v1/credential-types` | 凭据类型 catalog |

`credential-types` 在 `biz/secret/credtype.go` 定义，如 `database-metrics`（DB exporter 凭据）、`ssh-identity`、`webhook-url` 等。明文加密存储（PII 字段加密），列表只回显 masked。

---

## 19. MCP Server 管理

源文件：`internal/manager/server/mcp/http.go`

| Method | Path | 说明 |
|--------|------|------|
| POST | `/v1/mcp/servers` | 注册 MCP server |
| GET | `/v1/mcp/servers` | 列表 |
| GET | `/v1/mcp/servers/{id}` | 详情 |
| PUT | `/v1/mcp/servers/{id}` | 更新 |
| DELETE | `/v1/mcp/servers/{id}` | 删除 |
| POST | `/v1/mcp/servers/{id}/test` | 测试连接 |

注册后通过 `tools/mcp_basetool.go` 暴露给 LLM，LLM 可调用外部 MCP server 的工具。

---

## 20. Skill 执行

源文件：`internal/manager/server/skill/http.go`

| Method | Path | 说明 |
|--------|------|------|
| GET | `/v1/skills` | Skill 列表 |
| GET | `/v1/skills/{key}` | 详情 |
| POST | `/v1/skills/{key}/execute` | 直接执行 skill |

`/v1/skills/{key}/execute` 用于 SPA 显式触发 skill（绕过 chat），如 web_search、code_browse 等。

---

## 21. Approval 审批

源文件：`internal/manager/server/approval/http.go`

| Method | Path | 说明 |
|--------|------|------|
| GET | `/v1/approvals` | 待审批列表 |
| GET | `/v1/approvals/count` | 计数 |
| GET | `/v1/approvals/{id}` | 详情 |
| POST | `/v1/approvals/{id}/approve` | 批准 |
| POST | `/v1/approvals/{id}/reject` | 拒绝 |

AIOps 中 `review_gate` decorator 拦截高危操作时创建 approval，用户通过此 endpoint 批准/拒绝。

---

## 22. Audit 审计日志

源文件：`internal/manager/server/audit/http.go`

| Method | Path | 说明 |
|--------|------|------|
| GET | `/v1/admin/audit-logs` | 审计日志列表（admin only） |

**审计策略**（HLD-010）：只记录 handler 显式 `SetAuditEvent` 标注的用户动作，不是 access log。已接入的动作：`auth_login_failed`、`user_create/delete`、`alert/rule/channel/knowledge/settings CRUD`、`audit_view` 等。

---

## 23. Topology 拓扑

源文件：`internal/manager/server/topology/http.go`

| Method | Path | 说明 |
|--------|------|------|
| GET | `/v1/topology/nodes` | 节点列表 |
| GET | `/v1/topology/nodes/{id}` | 节点详情 |
| GET | `/v1/topology/relations` | 关系列表 |
| GET | `/v1/topology/relations/{id}` | 关系详情 |
| GET | `/v1/topology/relation-types` | 关系类型 |
| GET | `/v1/topology/relation-types/{name}` | 单个关系类型 |
| GET | `/v1/topology/node-types` | 节点类型 |
| GET | `/v1/topology/node-types/{name}` | 单个节点类型 |

拓扑数据由 edge 上报 + K8s inventory 自动构建，AIOps `get_topology` / `expand_topology` 工具读取。

---

## 24. Integrations 集成测试

源文件：`internal/manager/server/integration/http.go`

| Method | Path | 说明 |
|--------|------|------|
| POST | `/v1/integrations/grafana/test` | 测试 Grafana |
| POST | `/v1/integrations/grafana/sync` | 同步 datasource + dashboards |
| POST | `/v1/integrations/prom/test` | 测试 Prom |
| POST | `/v1/integrations/loki/test` | 测试 Loki |
| POST | `/v1/integrations/tempo/test` | 测试 Tempo |
| POST | `/v1/integrations/websearch/test` | 测试 Web Search |
| POST | `/v1/integrations/llm/test` | 测试 LLM 单 provider（不落库） |
| POST | `/v1/integrations/llm/validate-and-save` | 校验 + 原子保存 LLM 配置 |
| POST | `/v1/integrations/llm/invalidate` | 立即刷新 LLM 缓存 |
| GET | `/v1/observability/dashboards/{uid}` | 代理查 Grafana dashboard JSON |

### 24.1 LLM test 请求

```json
{
  "provider": "deepseek",
  "api_key": "sk-...",
  "base_url": "",
  "default_model": "deepseek-chat",
  "models": ["deepseek-chat", "deepseek-reasoner"]
}
```

### 24.2 LLM test 响应

```json
{
  "valid": true,
  "code": "ok",
  "provider": "deepseek",
  "model": "deepseek-chat",
  "detail": "200 OK",
  "latency_ms": 1200
}
```

**安全**：`api_key` 仅用于本次探测，不写 DB 不写日志；空 `api_key` 表示显式禁用该 provider。

---

## 25. SystemHealth 与 SystemUpgrade

### 25.1 SystemHealth

源文件：`internal/manager/server/systemhealth/http.go`

| Method | Path | 说明 |
|--------|------|------|
| GET | `/v1/system/health` | 健康汇总（12 个 check） |
| POST | `/v1/system/health/check` | 触发主动检查 |

**响应**：`{checks: [{id, group, status, detail}], overall: OK|DEGRADED|FAILED}`。

Check 项：db / prom / loki / tempo / grafana / frontier / qdrant / llm / embedding / edges / rules / incidents。

### 25.2 SystemUpgrade

源文件：`internal/manager/server/systemupgrade/http.go`

| Method | Path | 说明 |
|--------|------|------|
| GET | `/v1/system/upgrade` | 检查升级 |
| POST | `/v1/system/upgrade/check` | 触发检查 |

---

## 26. Prometheus 代理与票据

源文件：`internal/manager/server/prometheus/http.go`

### 26.1 Protected（用户面）

| Method | Path | 说明 |
|--------|------|------|
| POST | `/v1/prometheus/launch` | 生成 30min 票据 + 跳转 URL |
| POST | `/v1/prometheus/query_range` | 代理 range query |

### 26.2 Public（nginx auth_request）

| Method | Path | 说明 |
|--------|------|------|
| GET | `/v1/prometheus/auth` | 票据校验（nginx auth_request 调用） |

### 26.3 流程

```
浏览器 → POST /v1/prometheus/launch
   ↓ 拿 ticket cookie + /prometheus/graph?g0.expr=...
浏览器 → GET /prometheus/*
   ↓
nginx auth_request → GET /v1/prometheus/auth（验票）
   ↓ 200 OK + Set-Cookie 新 ticket（滑动续期）
nginx proxy_pass → Prometheus
```

**票据**：JWT Subject=`prometheus-proxy`，TTL 30min。Subject 校验防其他 JWT 滥用。

---

## 27. Internal endpoint（edge tunnel / K8s enroll）

### 27.1 edge tunnel（frontierbound 反向 RPC）

源文件：`internal/manager/service/frontierbound/handlers.go`

**不在 HTTP server 上**，而是通过 frontier broker 的 geminio RPC 注册：

| Method（geminio） | 方向 | 说明 |
|------------------|------|------|
| `register_edge` | edge→cloud | 注册 + K8s controller 分支 |
| `heartbeat` | edge→cloud | 心跳 + 插件健康 piggyback |
| `push_host_metrics` | edge→cloud | 推主机指标 |
| `push_prom_samples` | edge→cloud | 推 Prom 样本（host/k8s 分流） |
| `push_k8s_inventory` | edge→cloud | 推 K8s 快照 |
| `get_plugin_configs` | edge→cloud | 取插件配置 |
| `shell_output` / `shell_exit` | edge→cloud | WebSSH 输出 |

详见 `ongrid_rpc_singchia_geminio.md`。

### 27.2 K8s enroll（HTTP internal）

`POST /internal/k8s/enroll`：用 bootstrap token 鉴权（非 JWT），edge agent 注册集群时调用。

`POST /internal/k8s/telemetry-config`：用 `X-Edge-Id` header 鉴权，刷新遥测配置。

### 27.3 数据面 auth

`dataPlaneAuthHandler.Register(mux)`：edge 数据面（nginx auth_request）调用的鉴权 endpoint。

---

## 28. middleware 链与审计

### 28.1 middleware 顺序

```
请求 → Recover → RequestID → Logger → CORS → Timeout
     → AuditMiddleware（注入 auditSlot）
     → AuthMiddleware（验 JWT → tenantctx）
     → OTelMiddleware（注入 trace）
     → CasbinMiddleware（按需）
     → Handler
```

### 28.2 AuditMiddleware（HLD-010）

源文件：`internal/manager/server/middleware/audit.go`

- **安装 `*auditSlot` 到 context**：handler 链下游通过 `SetAuditEvent(r, ev)` 写入
- **不自动派生 action**：早期版本会生成 `http_<method>_<resource>` 兜底，2026-05 移除（operator 反馈噪音大）
- **显式标注才审计**：handler 必须调用 `SetAuditEvent`，否则不记
- **slot 指针存活 r.WithContext**：中间件 re-wrap request 时不丢

### 28.3 tenantctx

`internal/pkg/tenantctx`：AuthMiddleware 注入 `Tenant{UserID, Role, OrgID}` 到 context，handler 通过 `tenantctx.From(ctx)` 取。

### 28.4 Metrics middleware

`middleware/metrics.go`：Prometheus `ongrid_http_requests_total{method,path,status}` + `ongrid_http_request_seconds{method,path}`。

**Label 限制**：`path` 用 chi route pattern（如 `/v1/edges/{id}`）而非真实 URL，避免高基数。

---

## 29. 架构红线

1. **org_id / user_id 不在 body 中由用户提供** —— 来自 JWT claims + URL path + tenantctx middleware（ADR-003）
2. **Secret 仅此一次明文返回** —— edge 创建 / rotate 时返回明文 secret_key，后端存 Argon2id hash
3. **endpoint_masked 不回显完整 endpoint** —— NotificationChannel / Secret / IM App 列表都返回脱敏
4. **所有权隔离用 404 而非 403** —— 防止泄露资源存在性
5. **Handler 必须有 Swagger 注释** —— `@Summary` / `@Router` / `@Success`（AGENTS.md 要求，目前部分文件已加）
6. **响应格式统一**：成功裸业务对象；错误 `{error, code}` + HTTP status
7. **破坏性变更走新版本** —— API 变更先更新 `.proto`，原版本只允许加非破坏性内容
8. **接口在消费方定义** —— `AIOpsService` / `MentionSearcher` / `ModelCatalog` / `AgentLister` / `Streamer` 等接口在 handler 包定义
9. **所有 IO 函数第一参数 context.Context** —— handler 透传 `r.Context()`
10. **错误 `%w` 包装** —— sentinel + 上下文（method/edgeID/transportID）
11. **High-cardinality 字段禁止做 Prom label** —— path 用 route pattern
12. **敏感字段禁止明文入日志** —— api_key / ssh_pass / secret_key 不入 log
13. **Audit 只记显式标注动作** —— 不是 access log
14. **Login 限流** —— IP 20/5min + email 6/15min
15. **WebShell 限制** —— 5 sessions/user、5 sessions/device、15min idle
16. **Prom ticket Subject 校验** —— 防 JWT 滥用
17. **Proto 仅作规范** —— 不生成 Go 代码，handler 手写 DTO 镜像

---

## 30. 关键文件索引

### 30.1 Proto 规范

| 文件 | 域 |
|------|----|
| `api/iam/v1/iam.proto` | IAM |
| `api/manager/aiops/v1/aiops.proto` | AIOps |
| `api/manager/alert/v1/alert.proto` | Alert |
| `api/manager/edge/v1/edge.proto` | Edge |
| `api/manager/k8s/v1/k8s.proto` | K8s |
| `api/manager/metric/v1/metric.proto` | Metric |
| `api/manager/setting/v1/setting.proto` | Setting |
| `api/manager/notification/v1/notification.proto` | Notification |
| `api/tunnel/v1/tunnel.proto` | Tunnel（wire 协议） |

### 30.2 HTTP handler

| 文件 | 路由域 |
|------|--------|
| `internal/iam/server/http.go` + `orgs.go` | IAM |
| `internal/manager/server/aiops/http.go` | AIOps |
| `internal/manager/server/alert/http.go` | Alert + Notification |
| `internal/manager/server/edge/http.go` | Edge |
| `internal/manager/server/k8s/http.go` | K8s |
| `internal/manager/server/metric/http.go` + `prom_handler.go` | Metric |
| `internal/manager/server/monitor/http.go` | Monitor |
| `internal/manager/server/knowledge/http.go` | Knowledge |
| `internal/manager/server/report/http.go` | Report |
| `internal/manager/server/flow/http.go` | Flow |
| `internal/manager/server/webshell/http.go` | WebShell |
| `internal/manager/server/imbridge/http.go` | IM Bridge |
| `internal/manager/server/marketplace/http.go` | Marketplace |
| `internal/manager/server/secret/http.go` | Secret |
| `internal/manager/server/mcp/http.go` | MCP |
| `internal/manager/server/skill/http.go` | Skill |
| `internal/manager/server/approval/http.go` | Approval |
| `internal/manager/server/audit/http.go` | Audit |
| `internal/manager/server/topology/http.go` | Topology |
| `internal/manager/server/integration/http.go` | Integrations |
| `internal/manager/server/systemhealth/http.go` | SystemHealth |
| `internal/manager/server/systemupgrade/http.go` | SystemUpgrade |
| `internal/manager/server/prometheus/http.go` | Prometheus 代理 |
| `internal/manager/server/setting/http.go` | Setting |
| `internal/manager/server/logs/http.go` | Logs（Loki 代理） |
| `internal/manager/server/traces/http.go` | Traces（Tempo 代理） |
| `internal/manager/server/device/http.go` | Device |

### 30.3 装配与公共

| 文件 | 职责 |
|------|------|
| `cmd/ongrid/main.go` | 装配所有 handler 到 router |
| `internal/pkg/errs/errs.go` | 错误 sentinel + HTTPStatus 映射 |
| `internal/pkg/tenantctx/` | 租户 context |
| `internal/manager/server/middleware/audit.go` | 审计 middleware |
| `internal/manager/server/middleware/metrics.go` | Prometheus middleware |
| `internal/manager/service/frontierbound/handlers.go` | edge tunnel 反向 RPC（非 HTTP） |

---

> 本文档覆盖 OnGrid 全部对外 API（HTTP + 内部 geminio RPC + internal HTTP）。如需深入某域，参考 §30 文件索引定位源文件。RPC 协议细节见 `ongrid_rpc_singchia_geminio.md`，外部系统集成见 `ongrid_integration.md`，LLM 子系统见 `ongrid_LLM.md`。
