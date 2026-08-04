# OnGrid Makefile 目标分析

> 分析 `d:\claude\ongrid\Makefile` 的全部 make target，列出每个目标涉及的源码文件清单和目标结果说明。
> Makefile 是 OnGrid 项目唯一构建/测试/部署入口（gospec 红线），所有 CI / Dockerfile / README 都应只调 `make target`。

---

## 目录

1. [全局变量与环境](#1-全局变量与环境)
2. [help — 帮助](#2-help--帮助)
3. [build — 构建](#3-build--构建)
4. [test — 测试](#4-test--测试)
5. [lint — 代码检查](#5-lint--代码检查)
6. [proto — 协议生成](#6-proto--协议生成)
7. [migrate — 数据库迁移](#7-migrate--数据库迁移)
8. [docker — 镜像构建](#8-docker--镜像构建)
9. [compose — 本地编排](#9-compose--本地编排)
10. [run — 本地运行](#10-run--本地运行)
11. [release — 交叉编译](#11-release--交叉编译)
12. [release — 前端构建](#12-release--前端构建)
13. [release — Docker 镜像发布](#13-release--docker-镜像发布)
14. [release — Kubernetes Edge 镜像](#14-release--kubernetes-edge-镜像)
15. [release — 镜像校验与测试](#15-release--镜像校验与测试)
16. [release — Frontier Broker 构建](#16-release--frontier-broker-构建)
17. [release — 边端插件下载](#17-release--边端插件下载)
18. [release — Edge 升级包](#18-release--edge-升级包)
19. [release — Kubernetes Helm Chart](#19-release--kubernetes-helm-chart)
20. [release — 嵌入模型](#20-release--嵌入模型)
21. [release — 打包发布](#21-release--打包发布)
22. [release — 版本与清理](#22-release--版本与清理)
23. [clean — 清理](#23-clean--清理)
24. [目标依赖关系总览](#24-目标依赖关系总览)

---

## 1. 全局变量与环境

**文件**: [Makefile](file:///d:/claude/ongrid/Makefile) L4-52

| 变量 | 默认值 | 作用 |
|------|--------|------|
| `MODULE` | `github.com/ongridio/ongrid` | Go 模块路径 |
| `BIN_DIR` | `bin` | 本地构建产物目录 |
| `VERSION` | `cat VERSION` → `git describe` → `v0.0.0-dev` | 版本号三级回退 |
| `LDFLAGS` | `-X main.version=$(VERSION)` | 链接期注入版本号 |
| `GO_BUILD` | `go build -trimpath -ldflags '$(LDFLAGS)'` | 标准构建命令 |
| `TARGET_OS` / `TARGET_ARCH` | `linux` / `amd64` | 交叉编译目标 |
| `EDGE_PLUGIN_ARCHES` | `linux-amd64` | 边端插件架构（otelcol ~290M/arch） |
| `STAGE` | `dist/stage/ongrid-$(VERSION)-$(PACKAGE_TARGET)` | 打包暂存目录 |
| `OUT` | `dist/out` | 最终产物输出目录 |
| `CLOUD_IMAGE_PLATFORMS` | `linux/amd64,linux/arm64` | 多架构镜像平台 |
| `CLOUD_IMAGE_REPO` | `docker.cnb.cool/ongridio/ongrid` | CNB 镜像仓库 |
| `FRONTIER_VERSION` | `v1.2.4` | Frontier broker 版本 |
| `K8S_EDGE_IMAGE_REPO` | `docker.cnb.cool/ongridio/ongrid-edge` | K8s Edge 镜像仓库 |
| `DB_DSN` | `root:root@tcp(127.0.0.1:3306)/ongrid...` | 数据库连接串 |
| `MIGRATIONS` | `db/migrations` | 迁移文件路径 |

**版本来源文件**: [VERSION](file:///d:/claude/ongrid/VERSION)（当前 `v0.10.2`）

---

## 2. help — 帮助

### `help`

**行号**: L60-63

**涉及文件**:
- [Makefile](file:///d:/claude/ongrid/Makefile) — awk 脚本解析自身

**目标结果**: 默认目标（`.DEFAULT_GOAL := help`）。用 awk 扫描 Makefile 中所有 `target: ## 说明` 格式的行，打印彩色 target 列表。无需记忆 target 名，`make` 或 `make help` 即可查看全部可用目标。

---

## 3. build — 构建

### `build`（聚合）

**行号**: L70

**依赖**: `build-ongrid` + `build-ongrid-edge`

**目标结果**: 同时构建云端和边端两个二进制。

---

### `build-ongrid`

**行号**: L72-74

**涉及源码**:
- [cmd/ongrid/main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) — 云端主入口（中间件挂载、路由注册、服务编排）
- [cmd/ongrid/](file:///d:/claude/ongrid/cmd/ongrid/) — 含 `cloud_bash_path_test.go`、`coordinator_tools_test.go`、`llm_default_test.go`、`main_kernel_test.go`
- [internal/](file:///d:/claude/ongrid/internal/) — 全部业务代码（iam / manager / edgeagent / pkg）
- [go.mod](file:///d:/claude/ongrid/go.mod) / [go.sum](file:///d:/claude/ongrid/go.sum) — 依赖锁

**目标结果**: 产出 `bin/ongrid` 二进制（当前平台），通过 `-trimpath` 去除路径信息、`-ldflags` 注入版本号。

---

### `build-ongrid-edge`

**行号**: L76-78

**涉及源码**:
- [cmd/ongrid-edge/main.go](file:///d:/claude/ongrid/cmd/ongrid-edge/main.go) — 边端主入口
- [cmd/ongrid-edge/](file:///d:/claude/ongrid/cmd/ongrid-edge/) — 含 `k8s_credentials.go`、`k8s_data_plane.go`、`k8s_host_runtime.go`、`k8s_upgrade.go` 及对应测试
- [internal/edgeagent/](file:///d:/claude/ongrid/internal/edgeagent/) — 边端 agent 业务（bash / biz / collector / k8s / plugins / skill / webshell）

**目标结果**: 产出 `bin/ongrid-edge` 二进制（当前平台）。

---

## 4. test — 测试

### `test`

**行号**: L85-86

**涉及源码**:
- 全仓库 `./...` — 所有 `*_test.go` 文件

**目标结果**: 运行全部单元测试（`go test ./...`），不含 race 检测。

---

### `test-race`

**行号**: L88-89

**涉及源码**: 同 `test`

**目标结果**: 运行单元测试 + 竞态检测（`-race`），满足 AGENTS.md 红线"共享状态必须加锁，测试必须带 `-race`"。

---

### `test-integration`

**行号**: L91-92

**涉及源码**:
- 带 `//go:build integration` 标签的测试文件

**目标结果**: 运行集成测试（`-tags=integration`），需要外部依赖（DB / Redis 等）。

---

### `test-e2e`

**行号**: L94-95

**涉及源码**:
- [tests/e2e/](file:///d:/claude/ongrid/tests/e2e/) — 全部 E2E 测试文件：
  - `alert_evaluator_test.go` — 告警评估
  - `auth_login_test.go` / `auth_rbac_test.go` / `auth_refresh_test.go` — 认证授权
  - `credentials_test.go` — 凭证
  - `flow_mcp_node_test.go` / `flow_serve_page_test.go` — 流程
  - `mcp_test.go` — MCP
  - `notify_channel_crud_test.go` / `notify_signed_test.go` / `notify_slack_test.go` — 通知
  - `rca_pipeline_test.go` — RCA 管道
  - `settings_reveal_test.go` — 设置
  - `skills_registry_test.go` — 技能
  - `tasks_test.go` — 任务
  - `workflow_generate_test.go` / `workflow_test.go` — 工作流
  - `main_test.go` — E2E 入口
  - `testenv/` — 测试环境（`env.go` / `fakes.go` / `http.go` / `secrets.go`）
- [docs/test/e2e-catalog.md](file:///d:/claude/ongrid/docs/test/e2e-catalog.md) — E2E 目录

**目标结果**: 运行 E2E 测试（`-tags=e2e`），默认使用 fakes 无外部凭证，`-count=1` 禁用缓存。

---

### `test-e2e-live`

**行号**: L97-98

**涉及源码**:
- 同 `test-e2e`
- [tests/e2e/secrets.example.env](file:///d:/claude/ongrid/tests/e2e/secrets.example.env) — 真实凭证模板

**目标结果**: E2E live 模式（`E2E_LIVE_ALL=1`），使用 `tests/e2e/secrets.local.env` 打通真实外部服务，超时 15 分钟。

---

## 5. lint — 代码检查

### `lint`

**行号**: L105-106

**涉及源码**:
- [.golangci.yml](file:///d:/claude/ongrid/.golangci.yml) — golangci-lint 配置（启用 gofmt / goimports / govet / staticcheck / errcheck / revive 等）
- 全仓库 Go 源码

**目标结果**: 运行 `golangci-lint run`，执行静态代码检查。

---

### `arch-lint`

**行号**: L108-110

**涉及源码**:
- [.go-arch-lint.yml](file:///d:/claude/ongrid/.go-arch-lint.yml) — BC 边界校验配置
  - 强制 iam / manager / edgeagent 三大 BC 两两禁止互 import
  - `internal/pkg/**` 不得 import 任何 BC
  - 同一 BC 内 service 层禁止越过 biz 直接 import data 层

**目标结果**: 运行 `go-arch-lint check`，校验架构分层边界。未安装时跳过（exit 0）。

---

## 6. proto — 协议生成

### `proto`

**行号**: L117-132

**涉及源码**:
- [api/buf.yaml](file:///d:/claude/ongrid/api/buf.yaml) — buf 配置
- [api/buf.gen.yaml](file:///d:/claude/ongrid/api/buf.gen.yaml) — buf 代码生成配置
- Proto 定义文件：
  - [api/iam/v1/iam.proto](file:///d:/claude/ongrid/api/iam/v1/iam.proto)
  - [api/manager/aiops/v1/aiops.proto](file:///d:/claude/ongrid/api/manager/aiops/v1/aiops.proto)
  - [api/manager/alert/v1/alert.proto](file:///d:/claude/ongrid/api/manager/alert/v1/alert.proto)
  - [api/manager/edge/v1/edge.proto](file:///d:/claude/ongrid/api/manager/edge/v1/edge.proto)
  - [api/manager/k8s/v1/k8s.proto](file:///d:/claude/ongrid/api/manager/k8s/v1/k8s.proto)
  - [api/manager/metric/v1/metric.proto](file:///d:/claude/ongrid/api/manager/metric/v1/metric.proto)
  - [api/manager/notification/v1/notification.proto](file:///d:/claude/ongrid/api/manager/notification/v1/notification.proto)
  - [api/manager/setting/v1/setting.proto](file:///d:/claude/ongrid/api/manager/setting/v1/setting.proto)
  - [api/tunnel/v1/tunnel.proto](file:///d:/claude/ongrid/api/tunnel/v1/tunnel.proto)

**目标结果**: 重新生成 proto 的 Go 代码。优先使用 `buf generate`，回退到 `protoc + protoc-gen-go + protoc-gen-go-grpc`。产出写入 `api/gen/`。

---

## 7. migrate — 数据库迁移

### `migrate-up`

**行号**: L139-140

**涉及源码**:
- `db/migrations/` — 迁移 SQL 文件目录（由 `MIGRATIONS` 变量指定）
- `golang-migrate` CLI 工具

**目标结果**: 执行数据库迁移 up（全部未应用的迁移），连接串由 `DB_DSN` 变量提供。

---

### `migrate-down`

**行号**: L142-143

**涉及源码**: 同 `migrate-up`

**目标结果**: 回滚 1 步迁移（`down 1`）。

---

## 8. docker — 镜像构建

### `docker`（聚合）

**行号**: L150

**依赖**: `docker-ongrid` + `docker-ongrid-edge`

**目标结果**: 构建全部两个 Docker 镜像。

---

### `docker-ongrid`

**行号**: L152-153

**涉及源码**:
- [deploy/Dockerfile.ongrid](file:///d:/claude/ongrid/deploy/Dockerfile.ongrid) — 云端镜像构建文件
- [cmd/ongrid/](file:///d:/claude/ongrid/cmd/ongrid/) — 编译入口
- [internal/](file:///d:/claude/ongrid/internal/) — 全部业务代码
- [go.mod](file:///d:/claude/ongrid/go.mod) / [go.sum](file:///d:/claude/ongrid/go.sum)

**目标结果**: 构建 `ongrid:$(VERSION)` 镜像，传入 `VERSION` build-arg。

---

### `docker-ongrid-edge`

**行号**: L155-163

**涉及源码**:
- [deploy/Dockerfile.ongrid-edge](file:///d:/claude/ongrid/deploy/Dockerfile.ongrid-edge) — 边端镜像构建文件
- [cmd/ongrid-edge/](file:///d:/claude/ongrid/cmd/ongrid-edge/) — 编译入口
- [internal/edgeagent/](file:///d:/claude/ongrid/internal/edgeagent/) — 边端业务代码

**目标结果**: 构建 `ongrid-edge:$(VERSION)` 镜像，传入 `VERSION` + 4 个边端插件版本 build-arg（PROMTAIL / NODE_EXPORTER / PROCESS_EXPORTER / OTELCOL）。

---

## 9. compose — 本地编排

### `compose-up`

**行号**: L170-171

**涉及源码**:
- [deploy/docker-compose.yml](file:///d:/claude/ongrid/deploy/docker-compose.yml) — Compose 编排文件
- [deploy/.env.example](file:///d:/claude/ongrid/deploy/.env.example) — 环境变量模板
- [deploy/nginx.conf](file:///d:/claude/ongrid/deploy/nginx.conf) — nginx 配置
- [deploy/prometheus.yml](file:///d:/claude/ongrid/deploy/prometheus.yml) — Prometheus 配置
- [deploy/prometheus-rules.yml](file:///d:/claude/ongrid/deploy/prometheus-rules.yml) — 告警规则
- [deploy/loki-config.yaml](file:///d:/claude/ongrid/deploy/loki-config.yaml) — Loki 配置
- [deploy/tempo-config.yaml](file:///d:/claude/ongrid/deploy/tempo-config.yaml) — Tempo 配置

**目标结果**: 本地 `docker compose up -d` 启动全套服务（manager / edge / frontier / nginx / prometheus / loki / tempo / grafana 等）。

---

### `compose-down`

**行号**: L173-174

**涉及源码**: 同 `compose-up`

**目标结果**: 停止并移除本地 Compose 容器。

---

## 10. run — 本地运行

### `run-ongrid`

**行号**: L181-182

**涉及源码**:
- [cmd/ongrid/main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) — 入口

**目标结果**: `go run ./cmd/ongrid`，本地直接运行云端服务（不走 Docker），便于调试。

---

### `run-ongrid-edge`

**行号**: L184-185

**涉及源码**:
- [cmd/ongrid-edge/main.go](file:///d:/claude/ongrid/cmd/ongrid-edge/main.go) — 入口

**目标结果**: `go run ./cmd/ongrid-edge`，本地直接运行边端服务。

---

## 11. release — 交叉编译

### `build-linux`

**行号**: L202-207

**涉及源码**:
- [cmd/ongrid/main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go)

**目标结果**: 交叉编译 `bin/linux-amd64/ongrid`，`CGO_ENABLED=0` 纯静态，`-s -w` 去除调试符号减小体积。

---

### `build-edge-all`（聚合）

**行号**: L210-211

**依赖**: 4 个子目标

**目标结果**: 交叉编译 ongrid-edge 全部 4 个目标（linux/darwin × amd64/arm64）。

---

### `build-edge-linux-amd64`

**行号**: L214-218

**涉及源码**: [cmd/ongrid-edge/main.go](file:///d:/claude/ongrid/cmd/ongrid-edge/main.go)

**目标结果**: 产出 `bin/linux-amd64/ongrid-edge`。

---

### `build-edge-linux-arm64`

**行号**: L221-225

**涉及源码**: 同上

**目标结果**: 产出 `bin/linux-arm64/ongrid-edge`。

---

### `build-edge-darwin-amd64`

**行号**: L228-232

**涉及源码**: 同上

**目标结果**: 产出 `bin/darwin-amd64/ongrid-edge`（macOS Intel）。

---

### `build-edge-darwin-arm64`

**行号**: L235-239

**涉及源码**: 同上

**目标结果**: 产出 `bin/darwin-arm64/ongrid-edge`（macOS Apple Silicon）。

---

### `docker-build`

**行号**: L242-249

**涉及源码**:
- [deploy/Dockerfile.ongrid](file:///d:/claude/ongrid/deploy/Dockerfile.ongrid)

**目标结果**: 使用 `docker buildx build` 构建 `ongrid:$(VERSION)` 镜像，默认 `linux/amd64`，可用 `PLATFORM=linux/arm64` 覆盖，`--load` 加载到本地 Docker。

---

## 12. release — 前端构建

### `build-web`

**行号**: L254-255

**涉及源码**:
- [web/package.json](file:///d:/claude/ongrid/web/package.json) — npm 依赖与脚本
- [web/package-lock.json](file:///d:/claude/ongrid/web/package-lock.json) — 锁文件
- [web/vite.config.ts](file:///d:/claude/ongrid/web/vite.config.ts) — Vite 构建配置
- [web/tsconfig.json](file:///d:/claude/ongrid/web/tsconfig.json) — TypeScript 配置
- [web/tailwind.config.ts](file:///d:/claude/ongrid/web/tailwind.config.ts) — Tailwind 配置
- [web/src/](file:///d:/claude/ongrid/web/src/) — 全部前端源码（App.tsx / pages / components / api / store / lib）
- [web/index.html](file:///d:/claude/ongrid/web/index.html) — HTML 入口

**目标结果**: `npm ci && npm run build`，编译前端 SPA 到 `web/dist/`（`tsc -b && vite build`）。

---

### `docker-build-web`

**行号**: L257-265

**涉及源码**:
- [deploy/Dockerfile.web](file:///d:/claude/ongrid/deploy/Dockerfile.web) — 前端镜像构建文件（SPA + nginx，ADR-008）
- [web/](file:///d:/claude/ongrid/web/) — 前端源码（Dockerfile 内自行 `npm ci && npm run build`）

**目标结果**: 构建 `ongrid-web:$(VERSION)` 镜像，内含 nginx + 编译后的 SPA 静态文件。nginx.conf 和 TLS 证书在运行时 bind-mount。

---

## 13. release — Docker 镜像发布

### `docker-push-cloud-images`

**行号**: L268-288

**涉及源码**:
- [scripts/publish-release-image.sh](file:///d:/claude/ongrid/scripts/publish-release-image.sh) — 镜像发布脚本（幂等）
- [scripts/release-manifest-platforms.jq](file:///d:/claude/ongrid/scripts/release-manifest-platforms.jq) — jq 架构过滤器
- [deploy/Dockerfile.ongrid](file:///d:/claude/ongrid/deploy/Dockerfile.ongrid) — manager 镜像
- [deploy/Dockerfile.web](file:///d:/claude/ongrid/deploy/Dockerfile.web) — web 镜像

**目标结果**: 发布两个多架构镜像（`linux/amd64,linux/arm64`）到 CNB：
1. `docker.cnb.cool/ongridio/ongrid:$(VERSION)` — manager
2. `docker.cnb.cool/ongridio/ongrid/ongrid-web:$(VERSION)` — web

发布前通过 jq 过滤器校验 manifest 包含 amd64 + arm64，幂等（已存在则跳过）。

---

### `docker-push-release-images`（聚合）

**行号**: L322

**依赖**: `docker-push-cloud-images` + `docker-push-k8s-edge`

**目标结果**: 发布全部项目自身多架构镜像（manager + web + edge）。

---

### `release-image-refs`

**行号**: L324-325

**目标结果**: 打印本次发布的 3 个镜像引用（manager / web / edge），CI 消费用。

---

## 14. release — Kubernetes Edge 镜像

### `docker-build-k8s-edge`

**行号**: L291-302

**涉及源码**:
- [deploy/Dockerfile.ongrid-edge](file:///d:/claude/ongrid/deploy/Dockerfile.ongrid-edge)
- [cmd/ongrid-edge/](file:///d:/claude/ongrid/cmd/ongrid-edge/)

**目标结果**: 构建本地 K8s ongrid-edge 镜像（默认 `linux/amd64`），同时打两个标签：`ongrid-edge:$(VERSION)` 和 `$(K8S_EDGE_IMAGE_REF)`，`--load` 到本地 Docker。

---

### `docker-push-k8s-edge`

**行号**: L304-317

**涉及源码**:
- [scripts/publish-release-image.sh](file:///d:/claude/ongrid/scripts/publish-release-image.sh)
- [scripts/release-manifest-platforms.jq](file:///d:/claude/ongrid/scripts/release-manifest-platforms.jq)
- [deploy/Dockerfile.ongrid-edge](file:///d:/claude/ongrid/deploy/Dockerfile.ongrid-edge)

**目标结果**: 发布 K8s ongrid-edge 多架构镜像（`linux/amd64,linux/arm64`）到 `docker.cnb.cool/ongridio/ongrid-edge:$(VERSION)`，幂等发布。

---

### `k8s-edge-image-ref`

**行号**: L319-320

**目标结果**: 打印 K8s Edge 镜像引用 `$(K8S_EDGE_IMAGE_REF)`，CI 消费用。

---

## 15. release — 镜像校验与测试

### `verify-release-images`

**行号**: L327-333

**涉及源码**:
- [scripts/verify-release-images.sh](file:///d:/claude/ongrid/scripts/verify-release-images.sh) — 校验脚本
- [scripts/release-manifest-platforms.jq](file:///d:/claude/ongrid/scripts/release-manifest-platforms.jq) — jq 过滤器

**目标结果**: 校验 3 个项目镜像（manager / web / edge）均包含 amd64 + arm64 manifest。

---

### `test-release-manifest-filter`

**行号**: L335-340

**涉及源码**:
- [scripts/release-manifest-platforms.jq](file:///d:/claude/ongrid/scripts/release-manifest-platforms.jq)

**目标结果**: 单元测试 jq 架构过滤器——验证包含双架构的 manifest 通过、单架构的 manifest 被拒绝。

---

### `test-release-image-publish`

**行号**: L342-343

**依赖**: `test-release-manifest-filter`

**涉及源码**:
- [scripts/test-publish-release-image.sh](file:///d:/claude/ongrid/scripts/test-publish-release-image.sh)

**目标结果**: 校验 release 镜像幂等发布逻辑。

---

### `verify-compose-images`

**行号**: L346-347

**涉及源码**:
- [scripts/verify-cnb-compose-images.sh](file:///d:/claude/ongrid/scripts/verify-cnb-compose-images.sh)
- [deploy/docker-compose.yml](file:///d:/claude/ongrid/deploy/docker-compose.yml)

**目标结果**: 渲染并校验 Compose 运行镜像全部按预期指向 CNB 镜像仓库。

---

## 16. release — Frontier Broker 构建

### `docker-build-broker`

**行号**: L355-367

**涉及源码**:
- [deploy/Dockerfile.frontier](file:///d:/claude/ongrid/deploy/Dockerfile.frontier) — Frontier 构建文件（多阶段：golang:1.24-alpine builder → alpine:3.20 运行时）
- `$(FRONTIER_SRC)` = `$(HOME)/frontier` — 上游 frontier 源码目录

**目标结果**: 从上游源码本地构建 `singchia/frontier:$(FRONTIER_VERSION)` 镜像。幂等——如果本地已有匹配平台的镜像且 `FRONTIER_BUILD_FORCE != 1` 则跳过。Release 包和 Compose 部署使用 CNB 镜像，此 target 仅供本地开发。

---

## 17. release — 边端插件下载

### `fetch-promtail`

**行号**: L375-392

**涉及源码**:
- 外部下载：`https://github.com/grafana/loki/releases/download/v$(PROMTAIL_VERSION)/promtail-$(os)-$(arch).zip`
- 默认版本：`PROMTAIL_VERSION=3.4.0`

**目标结果**: 下载 promtail 到 `bin/<os>-<arch>/promtail`（ADR-012 / ADR-015 日志插件）。幂等缓存。Grafana 只发 linux 版本，darwin 不可用。

---

### `fetch-otelcol`

**行号**: L402-420

**涉及源码**:
- 外部下载：`https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/v$(OTELCOL_VERSION)/otelcol-contrib_*.tar.gz`
- 默认版本：`OTELCOL_VERSION=0.118.0`

**目标结果**: 下载 otelcol-contrib 到 `bin/<os>-<arch>/otelcol-contrib`（ADR-013 / ADR-015 追踪插件）。~200MB/arch，linux-only。

---

### `fetch-node-exporter`

**行号**: L442-460

**涉及源码**:
- 外部下载：`https://github.com/prometheus/node_exporter/releases/download/v$(NODE_EXPORTER_VERSION)/node_exporter-*.tar.gz`
- 默认版本：`NODE_EXPORTER_VERSION=1.8.2`

**目标结果**: 下载 node_exporter 到 `bin/<os>-<arch>/node_exporter`（主机指标源：CPU / 内存 / 磁盘 / 网络 / 负载）。linux-only。

---

### `fetch-process-exporter`

**行号**: L463-482

**涉及源码**:
- 外部下载：`https://github.com/ncabatoff/process-exporter/releases/download/v$(PROCESS_EXPORTER_VERSION)/process-exporter-*.tar.gz`
- 默认版本：`PROCESS_EXPORTER_VERSION=0.8.4`

**目标结果**: 下载 process-exporter 到 `bin/<os>-<arch>/process_exporter`（进程级指标，支撑 Top N 进程时间线面板）。linux-only。

---

### `fetch-db-exporters`（聚合）

**行号**: L485

**依赖**: `fetch-mysqld-exporter` + `fetch-postgres-exporter` + `fetch-redis-exporter` + `fetch-mongodb-exporter`

**目标结果**: 下载全部 4 个数据库 exporter。

---

### `fetch-mysqld-exporter`

**行号**: L487-503

**涉及源码**: 外部下载 `https://github.com/prometheus/mysqld_exporter/releases/...`，版本 `MYSQLD_EXPORTER_VERSION=0.19.0`

**目标结果**: 下载 mysqld_exporter 到 `bin/<os>-<arch>/mysqld_exporter`。

---

### `fetch-postgres-exporter`

**行号**: L505-521

**涉及源码**: 外部下载 `https://github.com/prometheus-community/postgres_exporter/releases/...`，版本 `POSTGRES_EXPORTER_VERSION=0.19.1`

**目标结果**: 下载 postgres_exporter 到 `bin/<os>-<arch>/postgres_exporter`。

---

### `fetch-redis-exporter`

**行号**: L523-539

**涉及源码**: 外部下载 `https://github.com/oliver006/redis_exporter/releases/...`，版本 `REDIS_EXPORTER_VERSION=1.86.0`

**目标结果**: 下载 redis_exporter 到 `bin/<os>-<arch>/redis_exporter`。

---

### `fetch-mongodb-exporter`

**行号**: L541-557

**涉及源码**: 外部下载 `https://github.com/percona/mongodb_exporter/releases/...`，版本 `MONGODB_EXPORTER_VERSION=0.51.0`

**目标结果**: 下载 mongodb_exporter 到 `bin/<os>-<arch>/mongodb_exporter`。

---

## 18. release — Edge 升级包

### `build-edge-bundle`

**行号**: L568-572

**涉及源码**:
- [dist/build-edge-bundle.sh](file:///d:/claude/ongrid/dist/build-edge-bundle.sh) — Edge 升级包构建脚本（ADR-024）
- `bin/<arch>/ongrid-edge` — 边端二进制（由 `build-edge-all` 产出）

**目标结果**: 为每个 `EDGE_PLUGIN_ARCHES` 架构打 Edge 升级包到 `dist/out/edge-bundles/`。

---

## 19. release — Kubernetes Helm Chart

### `package-k8s-chart`

**行号**: L575-578

**涉及源码**:
- [dist/package-k8s-chart.sh](file:///d:/claude/ongrid/dist/package-k8s-chart.sh) — Helm chart 打包脚本
- [deploy/kubernetes/ongrid-edge/](file:///d:/claude/ongrid/deploy/kubernetes/ongrid-edge/) — Helm chart 源：
  - `Chart.yaml` — chart 元数据
  - `values.yaml` — 默认值
  - `templates/` — 模板文件（daemonset / deployment / configmap / rbac / serviceaccount / telemetry-gateway / upgrade-preflight 等 14 个模板）
  - `_helpers.tpl` — 模板辅助函数

**目标结果**: 打包 K8s Helm chart 到 `bin/k8s/ongrid-edge.tgz`。

---

### `publish-k8s-chart`

**行号**: L580-587

**依赖**: `package-k8s-chart`

**涉及源码**:
- [scripts/publish-helm-chart.sh](file:///d:/claude/ongrid/scripts/publish-helm-chart.sh) — Helm chart 发布脚本

**目标结果**: 发布 Helm chart 到 CNB OCI 制品库 `oci://helm.cnb.cool/ongridio/ongrid-edge`。

---

### `test-k8s-chart`

**行号**: L589-590

**依赖**: `package-k8s-chart`

**涉及源码**:
- [scripts/test-k8s-chart.sh](file:///d:/claude/ongrid/scripts/test-k8s-chart.sh) — chart 校验脚本
- [deploy/kubernetes/ongrid-edge/](file:///d:/claude/ongrid/deploy/kubernetes/ongrid-edge/)

**目标结果**: 校验 K8s Helm Chart 的兼容性、拆分、暂停与非法配置。

---

### `test-publish-k8s-chart`

**行号**: L592-593

**涉及源码**:
- [scripts/test-publish-helm-chart.sh](file:///d:/claude/ongrid/scripts/test-publish-helm-chart.sh)

**目标结果**: 校验 Helm Chart 幂等发布逻辑。

---

## 20. release — 嵌入模型

### `fetch-embedding-model`

**行号**: L596-597

**涉及源码**:
- [dist/fetch-embedding-model.sh](file:///d:/claude/ongrid/dist/fetch-embedding-model.sh) — 模型下载脚本

**目标结果**: 预拉 BGE 离线嵌入模型到 `.cache/`，用于本地 RAG（`ONGRID_EMBEDDING_PROVIDER=local`）。幂等。`package` 不会自动依赖此目标（网络慢），需手动执行。

---

## 21. release — 打包发布

### `check-release-target`

**行号**: L600-609

**目标结果**: 校验 `PLATFORM` 与 `TARGET_OS/TARGET_ARCH` 一致，且 `PACKAGE_TARGET` 为 `linux-amd64` 或 `linux-arm64`。不一致则 exit 2。

---

### `package`

**行号**: L619-630

**依赖**: `check-release-target` → `fetch-promtail` → `fetch-otelcol` → `fetch-node-exporter` → `fetch-process-exporter` → `fetch-db-exporters` → `build-edge-all`

**涉及源码**:
- [dist/package.sh](file:///d:/claude/ongrid/dist/package.sh) — 打包主脚本
- [dist/build-edge-bundle.sh](file:///d:/claude/ongrid/dist/build-edge-bundle.sh) — Edge 升级包
- [deploy/docker-compose.yml](file:///d:/claude/ongrid/deploy/docker-compose.yml) — Compose 安装文件
- [deploy/install/](file:///d:/claude/ongrid/deploy/install/) — 安装脚本（`install.sh` / `uninstall.sh` / `upgrade.sh`）
- [deploy/install/edge/](file:///d:/claude/ongrid/deploy/install/edge/) — 边端安装脚本
- [deploy/.env.example](file:///d:/claude/ongrid/deploy/.env.example) — 环境变量模板
- [deploy/nginx.conf](file:///d:/claude/ongrid/deploy/nginx.conf) — nginx 配置
- [deploy/prometheus.yml](file:///d:/claude/ongrid/deploy/prometheus.yml) — Prometheus 配置
- [deploy/prometheus-rules.yml](file:///d:/claude/ongrid/deploy/prometheus-rules.yml) — 告警规则
- [deploy/loki-config.yaml](file:///d:/claude/ongrid/deploy/loki-config.yaml) — Loki 配置
- [deploy/tempo-config.yaml](file:///d:/claude/ongrid/deploy/tempo-config.yaml) — Tempo 配置
- [deploy/frontier.yaml](file:///d:/claude/ongrid/deploy/frontier.yaml) — Frontier 配置
- [deploy/searxng/settings.yml](file:///d:/claude/ongrid/deploy/searxng/settings.yml) — SearXNG 配置
- `bin/<os>-<arch>/` — 全部边端二进制和插件

**目标结果**: 打单架构 release tarball 到 `dist/out/ongrid-$(VERSION)-$(PACKAGE_TARGET).tar.xz` + `.sha256`。流程：
1. 下载全部边端插件（promtail / otelcol / node_exporter / process_exporter / db exporters）
2. 交叉编译全部 4 个 edge 目标
3. 清理 `dist/stage` + `dist/out`（`PACKAGE_CLEAN=1` 时）
4. 打 Edge 升级包
5. `dist/package.sh` 组装 Compose 安装文件 + Edge 二进制 → tar.xz + sha256

---

### `package-all`

**行号**: L632-642

**涉及源码**: 同 `package`

**目标结果**: 打 amd64 + arm64 两个生产安装包到 `dist/out/`，分别调用 `package TARGET_ARCH=amd64` 和 `package TARGET_ARCH=arm64`，`PACKAGE_CLEAN=0` 避免第二次清理第一次的产物。

---

### `test-release-package`

**行号**: L644-646

**涉及源码**:
- [scripts/test-public-url.sh](file:///d:/claude/ongrid/scripts/test-public-url.sh) — URL 校验
- `scripts/test-compose-release-package.sh` — Compose 包内容校验

**目标结果**: 校验安装 URL 与 Compose 发布包内容。

---

## 22. release — 版本与清理

### `dist-clean`

**行号**: L649-650

**目标结果**: 清理 release 产物：`dist/stage` / `dist/out` / `bin/linux-*` / `bin/darwin-*` / `bin/windows-*`。

---

### `version-print`

**行号**: L653-654

**目标结果**: 打印当前 `VERSION`，CI 消费。

---

## 23. clean — 清理

### `clean`

**行号**: L661-662

**目标结果**: 清理全部构建产物：`bin/` 目录 + `coverage.out` + `coverage.html`。

---

## 24. 目标依赖关系总览

```
make help                          # 默认目标，打印帮助

# ─── 构建 ───
make build
  ├── build-ongrid                 # bin/ongrid ← cmd/ongrid/
  └── build-ongrid-edge            # bin/ongrid-edge ← cmd/ongrid-edge/

# ─── 测试 ───
make test                          # go test ./...
make test-race                     # go test -race ./...
make test-integration              # go test -tags=integration ./...
make test-e2e                      # go test -tags=e2e ./tests/e2e/...
make test-e2e-live                 # E2E_LIVE_ALL=1 go test -tags=e2e ...

# ─── 检查 ───
make lint                          # golangci-lint run
make arch-lint                     # go-arch-lint check

# ─── 协议 ───
make proto                         # buf generate / protoc

# ─── 数据库 ───
make migrate-up                    # migrate up
make migrate-down                  # migrate down 1

# ─── Docker ───
make docker
  ├── docker-ongrid                # ongrid:VERSION ← Dockerfile.ongrid
  └── docker-ongrid-edge           # ongrid-edge:VERSION ← Dockerfile.ongrid-edge

# ─── Compose ───
make compose-up                    # docker compose up -d
make compose-down                  # docker compose down

# ─── 运行 ───
make run-ongrid                    # go run ./cmd/ongrid
make run-ongrid-edge               # go run ./cmd/ongrid-edge

# ─── Release 交叉编译 ───
make build-linux                   # bin/linux-amd64/ongrid
make build-edge-all
  ├── build-edge-linux-amd64       # bin/linux-amd64/ongrid-edge
  ├── build-edge-linux-arm64       # bin/linux-arm64/ongrid-edge
  ├── build-edge-darwin-amd64      # bin/darwin-amd64/ongrid-edge
  └── build-edge-darwin-arm64      # bin/darwin-arm64/ongrid-edge
make docker-build                  # ongrid:VERSION (buildx, --load)

# ─── Release 前端 ───
make build-web                     # web/dist/ ← npm ci && npm run build
make docker-build-web              # ongrid-web:VERSION ← Dockerfile.web

# ─── Release 镜像发布 ───
make docker-push-release-images
  ├── docker-push-cloud-images     # manager + web → CNB (multi-arch)
  └── docker-push-k8s-edge         # edge → CNB (multi-arch)
make release-image-refs            # 打印镜像引用
make verify-release-images         # 校验 manifest

# ─── Release 镜像测试 ───
make test-release-manifest-filter  # jq 过滤器单测
make test-release-image-publish    # 幂等发布测试
make verify-compose-images         # Compose 镜像引用校验

# ─── Release Frontier ───
make docker-build-broker           # singchia/frontier:VERSION (本地开发)

# ─── Release 边端插件 ───
make fetch-promtail                # bin/<arch>/promtail
make fetch-otelcol                 # bin/<arch>/otelcol-contrib
make fetch-node-exporter           # bin/<arch>/node_exporter
make fetch-process-exporter        # bin/<arch>/process_exporter
make fetch-db-exporters
  ├── fetch-mysqld-exporter        # bin/<arch>/mysqld_exporter
  ├── fetch-postgres-exporter      # bin/<arch>/postgres_exporter
  ├── fetch-redis-exporter         # bin/<arch>/redis_exporter
  └── fetch-mongodb-exporter       # bin/<arch>/mongodb_exporter

# ─── Release Edge 升级包 ───
make build-edge-bundle             # dist/out/edge-bundles/

# ─── Release Helm Chart ───
make package-k8s-chart             # bin/k8s/ongrid-edge.tgz
make publish-k8s-chart             # → CNB OCI
make test-k8s-chart                # chart 校验
make test-publish-k8s-chart        # 幂等发布测试

# ─── Release 嵌入模型 ───
make fetch-embedding-model         # .cache/ BGE 模型

# ─── Release 打包 ───
make check-release-target          # 校验平台参数
make package                       # dist/out/ongrid-VERSION-linux-ARCH.tar.xz
  ├── check-release-target
  ├── fetch-promtail
  ├── fetch-otelcol
  ├── fetch-node-exporter
  ├── fetch-process-exporter
  ├── fetch-db-exporters
  ├── build-edge-all
  └── build-edge-bundle
make package-all                   # amd64 + arm64 两个包
make test-release-package          # 包内容校验

# ─── 清理 ───
make dist-clean                    # dist/ + bin/<os>-*
make clean                         # bin/ + coverage
make version-print                 # 打印 VERSION
```

---

## 附录：关键文件索引

| 文件 | 关联目标 | 作用 |
|------|----------|------|
| [Makefile](file:///d:/claude/ongrid/Makefile) | 全部 | 唯一构建入口 |
| [VERSION](file:///d:/claude/ongrid/VERSION) | 全部 | 版本号来源（v0.10.2） |
| [go.mod](file:///d:/claude/ongrid/go.mod) / [go.sum](file:///d:/claude/ongrid/go.sum) | build / test / lint | Go 依赖锁 |
| [.golangci.yml](file:///d:/claude/ongrid/.golangci.yml) | lint | golangci-lint 配置 |
| [.go-arch-lint.yml](file:///d:/claude/ongrid/.go-arch-lint.yml) | arch-lint | BC 边界校验配置 |
| [cmd/ongrid/main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) | build-ongrid / run-ongrid / build-linux | 云端入口 |
| [cmd/ongrid-edge/main.go](file:///d:/claude/ongrid/cmd/ongrid-edge/main.go) | build-ongrid-edge / run-ongrid-edge / build-edge-* | 边端入口 |
| [deploy/Dockerfile.ongrid](file:///d:/claude/ongrid/deploy/Dockerfile.ongrid) | docker-ongrid / docker-build / docker-push-cloud-images | 云端镜像 |
| [deploy/Dockerfile.ongrid-edge](file:///d:/claude/ongrid/deploy/Dockerfile.ongrid-edge) | docker-ongrid-edge / docker-build-k8s-edge / docker-push-k8s-edge | 边端镜像 |
| [deploy/Dockerfile.web](file:///d:/claude/ongrid/deploy/Dockerfile.web) | docker-build-web / docker-push-cloud-images | 前端镜像 |
| [deploy/Dockerfile.frontier](file:///d:/claude/ongrid/deploy/Dockerfile.frontier) | docker-build-broker | Frontier broker 镜像 |
| [deploy/docker-compose.yml](file:///d:/claude/ongrid/deploy/docker-compose.yml) | compose-up / compose-down / package | Compose 编排 |
| [deploy/kubernetes/ongrid-edge/](file:///d:/claude/ongrid/deploy/kubernetes/ongrid-edge/) | package-k8s-chart / test-k8s-chart | Helm chart 源 |
| [api/](file:///d:/claude/ongrid/api/) (9 个 .proto + buf.yaml) | proto | gRPC 协议定义 |
| [web/](file:///d:/claude/ongrid/web/) | build-web / docker-build-web | 前端 SPA 源码 |
| [tests/e2e/](file:///d:/claude/ongrid/tests/e2e/) | test-e2e / test-e2e-live | E2E 测试 |
| [dist/package.sh](file:///d:/claude/ongrid/dist/package.sh) | package | release tarball 打包 |
| [dist/build-edge-bundle.sh](file:///d:/claude/ongrid/dist/build-edge-bundle.sh) | build-edge-bundle | Edge 升级包 |
| [dist/package-k8s-chart.sh](file:///d:/claude/ongrid/dist/package-k8s-chart.sh) | package-k8s-chart | Helm chart 打包 |
| [dist/fetch-embedding-model.sh](file:///d:/claude/ongrid/dist/fetch-embedding-model.sh) | fetch-embedding-model | BGE 模型下载 |
| [scripts/publish-release-image.sh](file:///d:/claude/ongrid/scripts/publish-release-image.sh) | docker-push-cloud-images / docker-push-k8s-edge | 镜像幂等发布 |
| [scripts/release-manifest-platforms.jq](file:///d:/claude/ongrid/scripts/release-manifest-platforms.jq) | docker-push-* / verify-release-images / test-release-manifest-filter | jq 架构过滤器 |
| [scripts/verify-release-images.sh](file:///d:/claude/ongrid/scripts/verify-release-images.sh) | verify-release-images | 镜像 manifest 校验 |
| [scripts/verify-cnb-compose-images.sh](file:///d:/claude/ongrid/scripts/verify-cnb-compose-images.sh) | verify-compose-images | Compose 镜像引用校验 |
| [scripts/publish-helm-chart.sh](file:///d:/claude/ongrid/scripts/publish-helm-chart.sh) | publish-k8s-chart | Helm chart 发布 |
| [scripts/test-k8s-chart.sh](file:///d:/claude/ongrid/scripts/test-k8s-chart.sh) | test-k8s-chart | chart 校验 |
| [scripts/test-publish-helm-chart.sh](file:///d:/claude/ongrid/scripts/test-publish-helm-chart.sh) | test-publish-k8s-chart | chart 幂等发布测试 |
| [scripts/test-publish-release-image.sh](file:///d:/claude/ongrid/scripts/test-publish-release-image.sh) | test-release-image-publish | 镜像幂等发布测试 |
| [scripts/test-public-url.sh](file:///d:/claude/ongrid/scripts/test-public-url.sh) | test-release-package | URL 校验 |
