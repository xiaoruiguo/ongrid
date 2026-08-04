# OnGrid Quickstart

> Agent 入口页。需要仓库上下文时先读本页，再按链接深入。

OnGrid 是一个云边架构的 AIOps 平台：边端 agent 采集主机/K8s 指标并通过反向隧道上报，云端 manager 提供 AI Agent 对话、根因分析、告警、监控、知识库等能力，用户通过 React SPA 或 IM 渠道（Slack/Telegram/钉钉/飞书）交互。

## 构建与运行

所有操作通过 Makefile 入口（gospec 红线：禁止裸 `go build` / `docker build`）。

```bash
make help              # 列出全部 target
make build             # 构建 ongrid + ongrid-edge 二进制到 bin/
make run-ongrid        # 本地直接运行云端（go run ./cmd/ongrid）
make run-ongrid-edge   # 本地直接运行边端
make compose-up        # Docker Compose 启动全套（manager + edge + frontier + nginx + prometheus + loki + tempo + grafana）
make compose-down      # 停止 Compose
make test              # 单元测试
make test-race         # 单元测试 + 竞态检测
make test-e2e          # E2E 测试（fakes 模式）
make lint              # golangci-lint
make arch-lint         # BC 边界校验
```

前端开发：

```bash
cd web && npm ci && npm run dev     # Vite 开发服务器
cd web && npm run build             # 编译 SPA 到 web/dist/
cd web && npx vitest run            # 前端单元测试
```

## "Start here when…" 路由表

| 任务 | 去哪里 |
|------|--------|
| 理解整体架构与模块边界 | [architecture.md](architecture.md) |
| 追踪一个请求从前端到后端 | [workflows.md](workflows.md) |
| 理解领域概念（BC、tenant、edge） | [domain.md](domain.md) |
| 构建/部署/调试 | [operations.md](operations.md) |
| 外部服务集成（LLM、IM、Grafana） | [integrations.md](integrations.md) |
| 测试策略与 E2E | [testing.md](testing.md) |
| 找某个功能的源码位置 | [source-map.md](source-map.md) |
| API / Proto 定义 | [api.md](api.md) |

## 技术栈速览

- **后端**: Go 1.25, chi router, GORM, Casbin, OpenTelemetry, Frontier (gRPC 边端通信)
- **前端**: React 18 + TypeScript, Vite, Tailwind CSS, Zustand, React Router, xterm
- **数据存储**: MySQL (主库), Redis (缓存), Qdrant (向量), ClickHouse (可选)
- **可观测性**: Prometheus + Loki + Tempo + Grafana
- **AI**: CloudWeGo Eino 框架, 多 LLM 路由 (OpenAI/Anthropic/GLM/DeepSeek/Gemini/Kimi)
