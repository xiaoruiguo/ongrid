# Architecture

> 系统架构与模块边界。OnGrid 遵循 gospec 分层规范，单服务 `cmd → web → controlplane → repo → model`，禁止跨层调用。

## 云边架构

```
┌─────────────────────────────────────────────────┐
│                   Cloud (Manager)                │
│  ┌─────────┐  ┌──────────┐  ┌─────────────────┐ │
│  │  Web SPA │  │  IAM BC  │  │  Manager BC     │ │
│  │ (React)  │  │ (auth/z) │  │ (aiops/alert/   │ │
│  │          │  │          │  │  edge/flow/...) │ │
│  └────┬─────┘  └────┬─────┘  └────────┬────────┘ │
│       │             │                  │          │
│  ─────┴─────────────┴──────────────────┴──────── │
│              chi router + middleware              │
│  (OTel → Metrics → Audit → Auth → Authz)         │
│                                                   │
│  ┌─────────────────────────────────────────────┐ │
│  │           Frontier (gRPC broker)            │ │
│  └──────────────────┬──────────────────────────┘ │
└─────────────────────┼───────────────────────────┘
                      │ 反向隧道 (边端拨出)
┌─────────────────────┼───────────────────────────┐
│              Edge (ongrid-edge)                   │
│  ┌──────────┐  ┌───────────┐  ┌───────────────┐  │
│  │ collector │  │  plugins  │  │  k8s agent    │  │
│  │ (cpu/mem/ │  │ (promtail/│  │ (inventory/   │  │
│  │  net/disk)│  │  otelcol) │  │  metrics/     │  │
│  │           │  │           │  │  readonly)    │  │
│  └──────────┘  └───────────┘  └───────────────┘  │
└───────────────────────────────────────────────────┘
```

## 代码分层

### `cmd/` — 入口

| 路径 | 作用 |
|------|------|
| `cmd/ongrid/main.go` | 云端主入口：中间件挂载、路由注册、服务编排 |
| `cmd/ongrid-edge/main.go` | 边端主入口：agent 生命周期、插件加载 |

### `internal/` — 业务代码（三大 BC 禁止互 import）

**IAM BC** (`internal/iam/`) — 独立认证授权 BC

| 层 | 路径 | 职责 |
|----|------|------|
| server | `internal/iam/server/` | HTTP handler (login/refresh/users/orgs) |
| service | `internal/iam/service/` | 业务逻辑编排 |
| biz | `internal/iam/biz/` | authz Enforcer (Casbin)、user/org/membership usecase |
| data | `internal/iam/data/` | SQLite user store、org repo |
| model | `internal/iam/model/` | User/Org/Membership GORM 模型 |

**Manager BC** (`internal/manager/`) — 核心业务 BC

| 层 | 路径 | 职责 |
|----|------|------|
| server | `internal/manager/server/` | HTTP handler (aiops/alert/edge/flow/k8s/...) |
| service | `internal/manager/service/` | 服务编排（双内核：graph + legacy） |
| biz | `internal/manager/biz/` | 业务逻辑（aiops chatruntime、alert pipeline、audit、edge authn） |
| data | `internal/manager/data/` | GORM repo 实现 |
| model | `internal/manager/model/` | GORM 模型 (Session/Alert/Edge/AuditLog/...) |

**EdgeAgent BC** (`internal/edgeagent/`) — 边端 agent

| 层 | 路径 | 职责 |
|----|------|------|
| biz | `internal/edgeagent/biz/` | collector (cpu/mem/net)、agent 生命周期、升级 |
| k8s | `internal/edgeagent/k8s/` | K8s inventory/metrics/readonly |
| collector | `internal/edgeagent/collector/` | 指标采集映射 |
| plugins | `internal/edgeagent/plugins/` | 插件系统 |
| skill | `internal/edgeagent/skill/` | 内置技能（probe_dns/http/tcp、tail_file、web_search） |
| webshell | `internal/edgeagent/webshell/` | 浏览器 SSH |

### `internal/pkg/` — 共享库（不依赖任何 BC）

关键包：`auth` (JWT)、`authzmw` (Casbin middleware)、`tenantctx` (双层 context)、`tracing` (OTel)、`prom` (metrics)、`llm` (多 LLM 路由)、`tunnel` (反向隧道)、`dbx` (GORM 辅助)、`notify` (IM 通知)。

### `web/` — 前端 SPA

| 路径 | 职责 |
|------|------|
| `web/src/App.tsx` | 路由声明（React Router） |
| `web/src/pages/` | 页面组件 (Home/ChatThread/Alerts/Edges/...) |
| `web/src/components/` | 复用组件 (ChatInput/MessageBubble/Layout/Sidebar) |
| `web/src/api/` | API 层 (client.ts 请求封装 + 各业务 API) |
| `web/src/store/` | Zustand 状态 (auth/chatSessions/modelSelection/theme) |
| `web/src/lib/` | 工具函数 (promql/format/frontmatter/routes) |

### `api/` — Proto 定义

`api/iam/v1/`、`api/manager/{aiops,alert,edge,k8s,metric,notification,setting}/v1/`、`api/tunnel/v1/`。使用 buf 生成。

## 中间件链

请求经过 4 层全局中间件 + 1 层认证中间件 + 路由级授权：

```
HTTP → OTel HTTP → Metrics → Audit → [public/protected 分流]
                                    → protected: Auth(JWT) → [handler + Authz(路由级)]
```

详见 [source-map.md](source-map.md) 中间件部分，或完整的 [ongrid_middleware.md](../ongrid_middleware.md)。

## 双内核架构

aiops 服务采用双内核设计（`internal/manager/service/aiops/service.go`）：

- **Graph 内核**：基于 CloudWeGo Eino 的 ReAct 图，支持工具调用
- **Legacy 内核**：直连 LLM 的简单对话模式
- `runWithKernel()` 根据配置分发，`assistantIDRelay` 跨 handler 传递

## BC 边界强制

`make arch-lint` 通过 [.go-arch-lint.yml](../.go-arch-lint.yml) 校验：
- iam / manager / edgeagent 三大 BC 两两禁止互 import
- `internal/pkg/**` 不得 import 任何 BC
- 同一 BC 内 service 层禁止越过 biz 直接 import data 层
