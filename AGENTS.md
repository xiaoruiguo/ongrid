# AGENTS.md

> 本项目遵循 [gospec](https://github.com/singchia/gospec) — Go 后端项目 SDLC 全流程规范。
>
> 本文件由 `scripts/install.sh` 自动生成。完整规范见 gospec 仓库。

## Agent 必读

任何编码 / 设计 / API / 数据 / 测试 / CI / 部署 / 监控 / 安全 / 文档 任务，**先按 gospec 规范走**。

### 第一步：找到 gospec 任务路由表

按以下顺序查找 spec 入口：

1. `~/.claude/skills/gospec/spec/spec.md`（个人安装，推荐）
2. `.claude/skills/gospec/spec/spec.md`（项目级安装）
3. 上面都不存在 → 重新安装：
   ```bash
   git clone https://github.com/singchia/gospec ~/.claude/skills/gospec
   ```

### 第二步：路由 → 加载

读 `spec/spec.md` 顶部的"任务路由表"，找到当前任务对应的 1-3 个子文件，**只读必要文件**，不要顺序读完整个 spec。

### 第三步：实施 + 自查

按子文件指引实施，结束前对照文件末尾的"自查清单"逐项核对。

### 第四步：PR 前对照 review 清单

提交 PR 前对照 `spec/07-code-review.md` 自查清单。

---

## 核心约束（无需读 spec 也要遵守）

> 这些是任何任务都要守的红线。不论 agent 是否加载了完整 spec，都不能违反。

### 架构
- **单服务**：`cmd → web → controlplane → repo → model`，禁止跨层调用
- **monorepo**：`internal/<domain>` 之间禁止直接 import，必须通过 API / 事件 / `internal/shared/`
- 接口在消费方定义，禁止循环依赖
- `utils/`、`lerrors/` 不依赖任何业务包
- 依赖通过构造函数注入，不使用全局变量
- 每个目录都被 CODEOWNERS 覆盖

### 编码
- 禁止 `_ = fn()` 忽略错误（确实想丢弃必须注释说明）
- 共享状态必须加锁，测试必须带 `-race`
- 错误用 `%w` 包装；不重复记录（要么处理要么传播）
- 所有涉及 IO 的函数第一个参数为 `context.Context`
- `init()` 仅允许做注册（pprof / metrics collector / driver），禁止做 IO 或可能 panic
- 禁止全局可变变量（只读单例 / collector 除外）
- 避免 `any` / `interface{}` 出现在公共 API 边界（解码 / SDK 适配等不可避免时就近注释）

### 前端 UI
- **跟随大多数页面**（总纲）：约定没写明的，照现有**大多数页面**的做法来——页头、列表 / 表格结构、间距、空状态、加载态、交互，对齐主流惯例、不自创风格；和多数页面不一致时，要改的是你这页去迁就大盘，而不是反过来。动手前先翻几个成熟页（如 Alerts / Devices / Monitor）对一眼
- **复用组件**：优先用 `web/src/components/ui/`（`Card` / `Chip` / `Button` / `PageHeader` / `EmptyState`），不要手搓
- **配色**：中性骨架用 zinc（容器 `bg-zinc-900/40` + `border-zinc-800/60`，文字 `zinc-100` / `zinc-400` / `zinc-500`）；主操作 / 强调用 indigo（按钮 `indigo-600`）；语义状态只有 emerald(成功) / amber(降级) / red(异常) / sky(信息)，走 Chip `tone`，状态点用 `-500` 档（不用 -400，太亮）；品牌紫 `--accent` 只给 logo / 品牌面，不做大面积按钮
- **克制**（产品灵魂）：满屏正常态用「小圆点 + 灰字」，不给每个 OK 铺彩色底（满屏彩底很 low），只让异常跳出；一组数据用「一个 `Card` + `divide-y` 分行」而非每行一卡；禁止 `animate-pulse` / 发光阴影 / `hover:scale` / 花哨「刷新中」徽章
- **light/dark**：light 靠 `web/src/styles/index.css` 的 `html.light` 覆盖 zinc 类；坑——带透明度的 `bg-zinc-900/20` 不被纯 `.bg-zinc-900` 覆盖匹配，优先用纯 zinc 类，非用透明度变体不可时在 `index.css` 补 `html.light .bg-zinc-900\/NN`
- **i18n**：文案走 `tr('中文','English')` 跟随 locale，禁止同一字符串中英并排拼接
- **验证**：视觉改动必须 chrome headless 截图实看再提交；涉及主题的 light + dark 各截一张

### API
- 所有 API 变更先更新 `.proto`，禁止改生成代码
- Handler 必须有 Swagger 注释：`@Summary`、`@Router`、`@Success` 缺一不可
- 响应格式统一：`{code, message, data}`
- 破坏性变更走新版本，原版本只允许加非破坏性内容

### 测试
- 新功能必须有单元测试
- CI 强制启用 `-race`
- E2E 测试必须清理数据

### Git
- 提交格式：`<type>(<scope>): <desc>`（Conventional Commits）
- 禁止提交敏感信息（密码、密钥、token）
- 禁止 force push main/master

### 可观测性
- 所有对外服务必须暴露 `/healthz`、`/readyz`、`/metrics`
- 日志结构化（slog / zap）+ `trace_id`，ERROR 包含完整 error chain
- 高基数字段（user_id、email、url）禁止作为 Prometheus label
- 敏感字段禁止明文入日志

### 安全
- 密码必须用 bcrypt / argon2id，禁止 MD5 / SHA1
- SQL 全部参数化，禁止字符串拼接
- 密钥禁止进代码仓库 / 镜像 / 日志
- 容器以非 root 用户运行
- 多租户接口强制 `tenant_id` 过滤
- CI 必须包含 `govulncheck` + 依赖 / 镜像漏洞扫描

### 运维
- 任何变更必须有回滚方案
- 告警规则必须配 Runbook 链接
- 高风险变更走金丝雀或 feature flag
- P0 / P1 事故必须产出 blameless postmortem

### 数据存储
- **MySQL**：生产 schema 变更走 migration 文件；大表用在线 DDL 工具；变更兼容滚动发布（expand-contract）
- **Redis**：所有 key 必须设 TTL；禁止大 key（value > 10KB / 集合 > 5000）；分布式锁必须有 owner 校验
- **ClickHouse**：必须 Replicated engine；写入必须批量；ORDER BY 从低基数到高基数
- **InfluxDB**：tag 必须低基数（user_id / url 等禁止做 tag）；bucket 必须有 retention
- PII 字段加密存储，测试环境禁止生产数据明文

---

## 需求载体选择

不是所有变更都要写 PRD。按变更类型选载体（详见 `spec/01-requirement/`）：

| 变更类型 | 载体 |
|---------|------|
| Bug / 小改 / 配置 / 文档修复 | Issue（issue tracker） |
| 重构 / 升级依赖 / 性能优化（用户不感知） | RFC（`docs/rfc/RFC-XXX-*.md`） |
| 用户可感知的功能 / 业务变更 | PRD（`docs/requirements/PRD-XXX-*.md`） |
| 跨多个 PRD 的战略 | Epic（`docs/requirements/EPIC-XXX-*.md`） |

---

## 输出语言

默认中文（代码注释、文档、commit message）。

---

完整规范、所有子主题的具体细节、模板和自查清单见 `spec/spec.md` 的任务路由表。

---

## 技术栈（含版本）

> 项目版本：`v0.10.2`（见 `VERSION`）。版本号除特别说明外，均来自 `go.mod` / `web/package.json` / `cmd/ollama/frontend/package.json` / `deploy/docker-compose.yml` / `Makefile` / `.tool-versions`。

### 后端（Go）

- **语言 / 工具链**：Go `1.25.0`（`go.mod` 声明）；`.tool-versions` 指定 `golang 1.25.11`。Module：`github.com/ongridio/ongrid`。
- **HTTP 框架**：`go-chi/chi/v5` v5.1.0
- **ORM / 数据访问**：
  - `gorm.io/gorm` v1.31.1
  - `gorm.io/driver/mysql` v1.6.0
  - `gorm.io/plugin/soft_delete` v1.2.1（软删除）
  - `glebarez/sqlite` v1.11.0（CGO SQLite，本地开发可选）
  - `modernc.org/sqlite` v1.42.2（pure-Go SQLite，间接依赖）
  - `go-sql-driver/mysql` v1.9.3
  - `jackc/pgx/v5` v5.8.0（间接，Postgres 驱动）
  - `microsoft/go-mssqldb` v1.9.5（间接，SQL Server 驱动）
- **认证 / 授权**：
  - `golang-jwt/jwt/v5` v5.3.0（JWT）
  - `golang.org/x/crypto` v0.50.0（bcrypt / argon2id）
  - `casbin/casbin/v2` v2.103.0 + `casbin/gorm-adapter/v3` v3.32.0（RBAC）
- **RPC / 隧道**：
  - `singchia/frontier` v1.2.4（边端 ↔ 云端 tunnel broker，端口 40012 边端拨入 / 40011 服务端）
  - `singchia/geminio` v1.3.0-rc.2（RPC 库）
  - `google.golang.org/grpc` v1.80.0（间接，protobuf 生成的 gRPC stub）
  - `google.golang.org/protobuf` v1.36.11
  - `gorilla/websocket` v1.5.3（前端 SSE / WS 推流）
- **AI / LLM / RAG**：
  - `cloudwego/eino` v0.8.7（Agent 内核，graph 模式默认）
  - `eino-contrib/jsonschema` v1.0.3
  - `sashabaranov/go-openai` v1.41.2（OpenAI 兼容客户端，路由 Anthropic / GLM / DeepSeek / Gemini / Kimi 等）
  - `anush008/fastembed-go` v1.0.0（本地 ONNX 嵌入）
  - `yalue/onnxruntime_go` v1.7.0（间接，ONNX Runtime cgo 绑定；运行时 `libonnxruntime.so` v1.20.1）
  - `sugarme/tokenizer` v0.2.3-0.20230829214935（间接，HuggingFace tokenizer）
- **可观测性**：
  - `prometheus/client_golang` v1.20.5、`client_model` v0.6.1、`common` v0.55.0、`procfs` v0.15.1
  - `go.opentelemetry.io/otel` v1.43.0 + `sdk` v1.43.0 + `otelhttp` v0.68.0 + `otlptracehttp` v1.43.0
  - `shirou/gopsutil/v3` v3.23.6（主机指标采集；`v4` v4.26.3 间接）
- **IM 集成**：
  - `larksuite/oapi-sdk-go/v3` v3.5.4（飞书 / Lark）
  - `open-dingtalk/dingtalk-stream-sdk-go` v0.9.1（钉钉）
- **Markdown / 文档解析**：
  - `yuin/goldmark` v1.8.2
  - `ledongthuc/pdf` v0.0.0-20250511090121-5959a4027728
- **定时任务**：`robfig/cron/v3` v3.0.1
- **系统 / 杂项**：
  - `golang.org/x/sync` v0.20.0、`golang.org/x/time` v0.15.0、`golang.org/x/net` v0.52.0、`golang.org/x/sys` v0.43.0、`golang.org/x/text` v0.36.0
  - `golang/snappy` v0.0.4（压缩）
  - `google/uuid` v1.6.0
  - `gopkg.in/yaml.v3` v3.0.1
  - `klauspost/compress` v1.18.5、`k8s.io/klog/v2` v2.120.1（间接）
- **测试**：
  - `stretchr/testify` v1.11.1
  - `testcontainers/testcontainers-go` v0.42.0 + `modules/mysql` v0.42.0（集成测试）
  - `testcontainers` 链路依赖 `moby/moby` v0.4.0、`go-ole`、`tklauser` 等

### API / Proto

- **buf** v1.47.2（`.tool-versions`）。`api/buf.yaml` v2，lint `STANDARD`，breaking `FILE`。
- **代码生成插件**（`api/buf.gen.yaml`）：
  - `buf.build/protocolbuffers/go` v1.34.2
  - `buf.build/grpc/go` v1.5.1（`require_unimplemented_servers=true`，`paths=source_relative`）
- **proto 资产**：`api/iam/v1`、`api/manager/{aiops,alert,edge,k8s,metric,notification,setting}/v1`、`api/tunnel/v1`。
- 回退：未装 buf 时用 `protoc` + `protoc-gen-go` + `protoc-gen-go-grpc`（见 `Makefile` `proto` target）。

### 数据存储

- **MySQL** `8.0`（主存储；compose 镜像 `docker.cnb.cool/ongridio/ongrid/mysql:8.0`，`utf8mb4` / `utf8mb4_unicode_ci`）
- **SQLite**（可选，单机本地开发；`ONGRID_DB_DIALECT=sqlite`）
- **Qdrant** `v1.11.3`（向量库，RAG 知识库；HTTP 6333，仅集群内访问）
- **Prometheus** `v2.54.0`（指标 TSDB；90d / 20GB 保留；开启 `--web.enable-remote-write-receiver`）
- **Loki** `3.4.0`（日志后端，单二进制；HTTP 3100 仅集群内）
- **Tempo** `2.10.0`（追踪后端，单二进制；OTLP gRPC 4317 / HTTP 4318 不对外）
- **Grafana** `11.1.4`（`grafana-oss`，可视化 / 探索；嵌入式部署在 `/grafana` 子路径）
- **SearXNG** `latest`（自托管元搜索，`web_search` 工具默认后端；HTTP 8080 仅集群内）
- **Ollama** `latest`（可选，本地 LLM；GPU 直通，端口 11434）
- **数据库迁移**：`golang-migrate/migrate` v4.18.1（`.tool-versions`），迁移文件目录 `db/migrations`（`Makefile` `migrate-up` / `migrate-down`）。

### 前端（主 SPA `web/`）

- **框架**：React `18.3.1` + `react-dom` `18.3.1`
- **路由**：`react-router-dom` `6.27.0`
- **状态**：`zustand` `5.0.1`
- **图表**：`recharts` `2.13.3`（d3 / victory 系列，间接）
- **工作流图**：`@xyflow/react` `12.10.2` + `@dagrejs/dagre` `3.0.0`
- **图标**：`lucide-react` `0.460.0`
- **Markdown**：`react-markdown` `9.0.1` + `remark-gfm` `4.0.0`
- **终端**：`xterm` `5.3.0` + `xterm-addon-fit` `0.8.0` + `xterm-addon-web-links` `0.9.0`
- **构建 / 工具链**：
  - TypeScript `5.6.3`（`tsconfig.json` target `ES2020`，`strict: true`，`moduleResolution: bundler`）
  - Vite `5.4.10` + `@vitejs/plugin-react` `4.3.3`
  - TailwindCSS `3.4.14`（`darkMode: 'class'`，自定义 CSS 变量配色）+ PostCSS `8.4.47` + autoprefixer `10.4.20`
  - ESLint `8.57.1` + `@typescript-eslint/{eslint-plugin,parser}` `7.18.0` + `eslint-plugin-react-hooks` `4.6.2` + `eslint-plugin-react-refresh` `0.4.14`
  - `@types/node` `20.16.10`、`@types/react` `18.3.12`、`@types/react-dom` `18.3.1`
- **测试**：
  - Vitest `1.6.1` + jsdom `23.2.0`
  - `@testing-library/react` `14.3.1` + `@testing-library/jest-dom` `6.9.1` + `@testing-library/user-event` `14.6.1`
  - `msw` `2.14.4`（API mock）
  - `@playwright/test` `1.61.1`（E2E，`web/e2e/`）

### 前端（Ollama 子前端 `cmd/ollama/frontend/`）

> 独立小前端，仅用于本地 Ollama 聊天测试，不进生产镜像。

- React `19.2.8` + react-dom `19.2.8`
- Vite `8.2.0` + `@vitejs/plugin-react` `6.0.4`
- ESLint `10.8.0` + `@eslint/js` `10.0.1` + `eslint-plugin-react-hooks` `7.1.1` + `eslint-plugin-react-refresh` `0.5.3`
- `globals` `17.7.0`、`@types/react` `19.2.17`、`@types/react-dom` `19.2.3`

### 边端 Agent（`ongrid-edge`）内置插件

- **promtail** `3.4.0`（日志采集 → Loki）
- **node_exporter** `1.8.2`（主机指标）
- **process-exporter** `0.8.4`（进程指标）
- **otelcol-contrib** `0.118.0`（OpenTelemetry Collector，traces）
- **数据库 exporter（可选，linux-only）**：
  - `mysqld_exporter` `0.19.0`
  - `postgres_exporter` `0.19.1`
  - `redis_exporter` `1.86.0`
  - `mongodb_exporter` `0.51.0`

### 部署 / 容器

- **Docker** 多阶段构建（`syntax=docker/dockerfile:1.7`）：
  - `deploy/Dockerfile.ongrid`：builder `golang:1.25-bookworm`（CGO=1，链接 libonnxruntime），runtime `debian:bookworm-slim`，非 root（uid 65532），内嵌 `ONNX Runtime` `1.20.1`、Python3 / pip（cloud_bash 工具运行时安装）
  - `deploy/Dockerfile.ongrid-edge`：builder `golang:1.25-alpine`（CGO=0），runtime `gcr.io/distroless/base-debian12:nonroot`，5 个独立 stage 下载 promtail / node_exporter / process_exporter / otelcol-contrib
  - `deploy/Dockerfile.web`：builder `node:20-alpine`，runtime `nginx:1.27-alpine`，SPA 内嵌 + `deploy/nginx/nginx.conf`
  - `deploy/Dockerfile.frontier`：本地构建上游 `singchia/frontier`（仅 dev；生产用 CNB 镜像）
- **Docker Compose**：`deploy/docker-compose.yml`（本地开发）+ `deploy/install/docker-compose.yml`（生产安装包）。运行镜像统一拉自 `docker.cnb.cool/ongridio/ongrid/...`。
- **Kubernetes**：`deploy/kubernetes/ongrid-edge/`（Helm Chart `apiVersion: v2`，`ongrid-edge`，多架构 `linux/amd64,linux/arm64`）。Chart 发布到 OCI 制品库 `helm.cnb.cool/ongridio`。
- **NGINX** `1.27-alpine`：TLS 终止 + SPA 静态托管 + `/api`、`/prometheus`、`/loki`、`/v1/traces`、`/grafana` 反向代理（`auth_request` → manager `edgeauth`）。
- **CNB 制品库**：`docker.cnb.cool/ongridio/ongrid`（容器镜像）、`helm.cnb.cool/ongridio`（Helm OCI）。

### CI / CD

- **GitHub Actions**：
  - `actions/checkout@v4`、`actions/setup-go@v5`（Go `1.25.x`）、`actions/setup-node@v4`（Node `22`）
  - `bufbuild/buf-setup-action@v1`
  - `azure/setup-helm@v4`
  - `docker/setup-qemu-action@v3`、`docker/setup-buildx-action@v3`、`docker/login-action@v3`
  - `actions/upload-artifact@v4` / `actions/download-artifact@v4`
- **Runners**：`ubuntu-24.04`（amd64）+ `ubuntu-24.04-arm`（arm64）
- **工作流**：`.github/workflows/ci.yml`（push / PR：`go build` + `go vet` + `go test -race` + `buf lint` + `web test` + `web build` + 部署清单校验 + Helm chart 校验）、`.github/workflows/release.yml`（tag `v*.*.*` 触发：发布多架构镜像 + Helm chart + amd64/arm64 release tarball）、`.github/workflows/contribution-terms.yml`。

### Lint / 静态检查

- **golangci-lint** `1.61.0`（`.tool-versions`；`.golangci.yml` 启用 `gofmt` / `goimports` / `govet` / `staticcheck` / `errcheck` / `revive` / `gosec` / `sqlclosecheck` / `misspell` / `ineffassign` / `unused` / `gocyclo`，`gocyclo.min-complexity: 15`，本地前缀 `github.com/ongridio/ongrid`）
- **go-arch-lint**（校验分层边界，`Makefile` `arch-lint`，`.go-arch-lint.yml`）
- **buf lint**（STANDARD，例外 `PACKAGE_DIRECTORY_MATCH`）

### LLM / Embedding Provider（运行时配置）

- **LLM**（`ONGRID_LLM_DEFAULT_PROVIDER` 切换）：OpenAI（默认 `gpt-4o`）、Anthropic、Google Gemini、智谱 GLM、DeepSeek、Kimi（Moonshot）、Ollama（本地）
- **Embedding**：
  - `ONGRID_EMBEDDING_PROVIDER=openai`：`text-embedding-3-small`（1536 维，OpenAI 兼容 / GLM / Qwen 通用）
  - `ONGRID_EMBEDDING_PROVIDER=local`：BGE-small-zh-v1.5 ONNX（512 维，离线 RAG，fastembed-go 加载）

### 通知渠道（`ONGRID_NOTIFY_*`）

- 内置 log（dry-run）
- 通用 Webhook（带 `X-Ongrid-Signature` HMAC 签名）
- Slack / 飞书 / 钉钉（incoming webhook）

### 架构边界

- **monorepo**：`cmd/{ongrid,ongrid-edge,chi,mytest,ollama}` + `internal/{edgeagent,iam,manager,pkg,skill}` + `api/` + `web/` + `deploy/` + `docs/` + `tests/e2e` + `agents/` + `skills/`。
- **云端** `ongrid`：`cmd → web → controlplane → repo → model`，禁止跨层调用。
- **边端** `ongrid-edge`：拨号 `frontier:40012` 出站，不开任何入站服务端口（仅本地 `:9101/metrics`）。
- **域间通信**：`internal/<domain>` 之间禁止直接 import，必须通过 API / 事件 / `internal/shared/`。

<!-- OPENWIKI:START -->

## OpenWiki

本仓库使用 OpenWiki 维护面向 Agent 的代码文档。请先阅读 `openwiki/quickstart.md`，再按其中链接进入架构、工作流、领域概念、运维、集成、测试与源码地图。

需要仓库上下文时，优先读 OpenWiki 页面，而不是全库扫描。除非用户明确要求，不要手改生成页；应改源码后重新生成文档。

<!-- OPENWIKI:END -->
