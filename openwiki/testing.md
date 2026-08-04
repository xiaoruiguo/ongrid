# Testing

> 测试策略与 E2E 指南。

## 测试分层

| 层级 | 命令 | 说明 |
|------|------|------|
| 单元测试 | `make test` | `go test ./...`，全仓库 |
| 竞态检测 | `make test-race` | `go test -race ./...`（AGENTS.md 红线） |
| 集成测试 | `make test-integration` | `-tags=integration`，需外部依赖 |
| E2E (fakes) | `make test-e2e` | `-tags=e2e`，无外部凭证 |
| E2E (live) | `make test-e2e-live` | `E2E_LIVE_ALL=1`，真实外部服务，15m 超时 |
| 前端单元 | `cd web && npx vitest run` | Vitest + jsdom + MSW |
| 前端 E2E | `cd web && npx playwright test` | Playwright |
| 前端类型检查 | `cd web && npm run typecheck` | `tsc -b --noEmit` |

## E2E 测试目录

`tests/e2e/` — Go E2E 测试，使用 build tag `e2e`。

### 测试环境

| 文件 | 作用 |
|------|------|
| `tests/e2e/testenv/env.go` | 测试环境启动/ teardown |
| `tests/e2e/testenv/fakes.go` | Fake 后端（LLM/外部服务） |
| `tests/e2e/testenv/http.go` | HTTP 测试服务器 |
| `tests/e2e/testenv/secrets.go` | 凭证管理 |
| `tests/e2e/secrets.example.env` | 凭证模板 |

### 测试覆盖

| 测试文件 | 覆盖场景 |
|----------|----------|
| `auth_login_test.go` | 登录流程 |
| `auth_rbac_test.go` | RBAC 授权 |
| `auth_refresh_test.go` | Token 刷新 |
| `alert_evaluator_test.go` | 告警评估 |
| `rca_pipeline_test.go` | RCA 管道 |
| `notify_channel_crud_test.go` | 通知渠道 CRUD |
| `notify_signed_test.go` | 签名通知 |
| `notify_slack_test.go` | Slack 集成 |
| `credentials_test.go` | 凭证管理 |
| `flow_mcp_node_test.go` | Flow MCP 节点 |
| `flow_serve_page_test.go` | Flow 页面服务 |
| `mcp_test.go` | MCP |
| `settings_reveal_test.go` | 设置脱敏 |
| `skills_registry_test.go` | 技能注册 |
| `tasks_test.go` | 任务 |
| `workflow_generate_test.go` | 工作流生成 |
| `workflow_test.go` | 工作流 |

E2E 目录：`docs/test/e2e-catalog.md`

## 前端测试

| 类型 | 框架 | 配置 |
|------|------|------|
| 单元测试 | Vitest + Testing Library | `web/vitest.config.ts` |
| E2E | Playwright | `web/playwright.config.ts` |
| Mock | MSW (Mock Service Worker) | `web/src/test/msw-server.ts` |

前端测试 fixtures：`web/test/fixtures/`

## CI

CI 配置在 `.github/workflows/`：
- `ci.yml` — 主 CI 流水线
- `release.yml` — 发布流水线

CI 强制要求（AGENTS.md 红线）：
- `-race` 竞态检测
- `govulncheck` 漏洞扫描
- 依赖/镜像漏洞扫描

## 测试红线

- 新功能必须有单元测试
- E2E 测试必须清理数据
- 共享状态必须加锁，测试必须带 `-race`
- 禁止 `_ = fn()` 忽略错误
