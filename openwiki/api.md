# API

> Proto 定义与 HTTP API 概览。

## Proto 定义

Proto 文件在 `api/` 目录，使用 buf 管理（`api/buf.yaml` + `api/buf.gen.yaml`）。

| Proto | 路径 |
|-------|------|
| IAM | `api/iam/v1/iam.proto` |
| AIOps | `api/manager/aiops/v1/aiops.proto` |
| Alert | `api/manager/alert/v1/alert.proto` |
| Edge | `api/manager/edge/v1/edge.proto` |
| K8s | `api/manager/k8s/v1/k8s.proto` |
| Metric | `api/manager/metric/v1/metric.proto` |
| Notification | `api/manager/notification/v1/notification.proto` |
| Setting | `api/manager/setting/v1/setting.proto` |
| Tunnel | `api/tunnel/v1/tunnel.proto` |

重新生成：`make proto`（优先 buf generate，回退 protoc + protoc-gen-go/grpc）。

## HTTP API 约定

- **路由器**：chi v5 (`github.com/go-chi/chi/v5`)
- **响应格式**：`{code, message, data}` 统一包装
- **Swagger**：Handler 必须有 `@Summary`、`@Router`、`@Success` 注释
- **破坏性变更**：走新版本，原版本只允许加非破坏性内容
- **所有 API 变更先更新 .proto**，禁止改生成代码

## 路由结构

```
mux (chi.NewRouter)
├── /healthz, /readyz              (公开, 无 auth)
├── /internal/auth/*               (edgeauth, 数据平面认证)
├── /api
│   ├── 公开路由
│   │   ├── iam login/refresh
│   │   ├── IM webhooks
│   │   └── pages
│   └── protected (auth.Middleware)
│       ├── /v1/chat/sessions      (aiops)
│       ├── /v1/edges              (edge CRUD, authz)
│       ├── /v1/knowledge/*        (knowledge, authz)
│       ├── /v1/alerts/*           (alert)
│       ├── /v1/settings/*         (setting)
│       ├── /v1/orgs               (iam)
│       ├── /v1/users              (iam)
│       └── ...
```

## 认证

- **控制平面**：JWT Bearer token（`Authorization: Bearer <jwt>`），WebSocket 降级 `?token=<jwt>`
- **数据平面**：Basic Auth + nginx auth_request（edgeauth，独立于 JWT）

## 授权对象命名（Casbin Phase 1）

| 对象 | 动作 | 说明 |
|------|------|------|
| `edge:*` | read/write/delete/manage | 边端 CRUD + 插件配置 |
| `knowledge:doc` | read/write/delete | 文档增删改 |
| `knowledge:repo` | read/write/delete | Git 仓库注册 |
| `alert:rule` | read/write/delete/manage | 告警规则 CRUD |
| `alert:incident` | read/write | incident ack/resolve/silence |
| `agent:custom` | read/write/delete | 自定义 agent CRUD |
| `monitor:panel` | read/write/delete | 监控面板 CRUD |
| `org:*` | manage | 组织管理 |
| `user:*` | manage | 用户管理 |
