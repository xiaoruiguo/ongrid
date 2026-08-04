# Operations

> 构建、部署、调试操作指南。所有操作通过 Makefile 入口。

## 构建

```bash
make build               # 构建 ongrid + ongrid-edge 到 bin/
make build-ongrid        # 仅云端: bin/ongrid ← cmd/ongrid/
make build-ongrid-edge   # 仅边端: bin/ongrid-edge ← cmd/ongrid-edge/
make build-linux         # 交叉编译 linux/amd64 (CGO_ENABLED=0, -s -w)
make build-edge-all      # 交叉编译 edge 全部 4 目标 (linux/darwin × amd64/arm64)
make build-web           # 编译前端 SPA: cd web && npm ci && npm run build
```

## Docker 镜像

```bash
make docker              # 构建 ongrid + ongrid-edge 镜像
make docker-ongrid       # ongrid:VERSION ← deploy/Dockerfile.ongrid
make docker-ongrid-edge  # ongrid-edge:VERSION ← deploy/Dockerfile.ongrid-edge
make docker-build-web    # ongrid-web:VERSION ← deploy/Dockerfile.web (SPA + nginx)
make docker-build-broker # singchia/frontier:VERSION ← deploy/Dockerfile.frontier (本地开发)
```

## 本地运行

```bash
make run-ongrid          # go run ./cmd/ongrid (云端)
make run-ongrid-edge     # go run ./cmd/ongrid-edge (边端)
make compose-up          # Docker Compose 全套启动
make compose-down        # 停止 Compose

# 前端开发
cd web && npm run dev    # Vite 开发服务器 (HMR)
```

## 测试

```bash
make test                # 单元测试
make test-race           # 单元测试 + 竞态检测 (-race)
make test-integration    # 集成测试 (build tag: integration)
make test-e2e            # E2E (fakes 模式, 无外部凭证)
make test-e2e-live       # E2E live (需 tests/e2e/secrets.local.env)
```

前端测试：`cd web && npx vitest run` 或 `npx playwright test`

## 代码检查

```bash
make lint                # golangci-lint run
make arch-lint           # go-arch-lint check (BC 边界校验)
```

## 数据库迁移

```bash
make migrate-up          # 迁移 up (DB_DSN 可覆盖)
make migrate-down        # 回滚 1 步
# DB_DSN 默认: root:root@tcp(127.0.0.1:3306)/ongrid?charset=utf8mb4&parseTime=true
```

## Proto 生成

```bash
make proto               # buf generate (优先) 或 protoc (回退)
```

## Release 打包

```bash
make package             # 单架构 release tarball → dist/out/ongrid-VERSION-linux-ARCH.tar.xz
make package-all         # amd64 + arm64 两个包
make dist-clean          # 清理 release 产物
make version-print       # 打印 VERSION
```

## 发布到 CNB

```bash
make docker-push-release-images   # 发布全部镜像 (manager + web + edge)
make publish-k8s-chart            # 发布 Helm chart 到 CNB OCI
make verify-release-images        # 校验 manifest 包含 amd64 + arm64
make verify-compose-images        # 校验 Compose 镜像指向 CNB
```

## 调试技巧

- **中间件链**：4 层全局（OTel → Metrics → Audit）+ Auth + 路由级 Authz，详见 [ongrid_middleware.md](../ongrid_middleware.md)
- **tenantctx**：双层存储（不可变 context.WithValue + 可变 *slot 指针），解决跨中间件可见性
- **LLM 路由**：MultiClient 60s TTL 缓存，配置变更后最多 60s 生效
- **Ollama 冷启动**：30-90s，可能超过 20s LLM probe 超时；预热 `ollama run --keepalive`
- **SSE 调试**：前端用 fetch + ReadableStream（非 EventSource），\n\n 帧分割
- **JWT 验证**：无 DB 查询，仅 HMAC + 过期检查

## 配置优先级

三级配置优先级（来自 project_memory）：
1. **运行时可变**：UI → system_settings（DB）
2. **启动固定**：env → Config
3. **首次引导**：env → DB seed

关键环境变量（红线）：
- `JWT_SECRET`：无默认值，未设置则 fatal 启动失败
- `SECRET_KEY`：无回退配置则告警
