# `env.go` 技术实现文档

> 源文件：`tests/e2e/testenv/env.go`
> 包路径：`github.com/ongridio/ongrid/tests/e2e/testenv`

## 1. 概述

本文件是 e2e 测试环境的核心入口，为单个测试用例拉起一个完整 manager 进程 + 周边fake（LLM / Slack / Telegram / Prom）+ 共享 MySQL 容器。核心红线：(1) `//go:build e2e` 构建标签隔离，普通 `go test` 不编译；(2) AdminEmail/Password 跨所有 Start 共享（BootstrapAdmin 仅首次 seed，后续 Starts 见 users>0 跳过，所以首测试设的密码必须全测试通用）；(3) sharedMySQL 每 Start 前 DROP 跨测试易泄漏表（system_settings / alert_rules / chat_sessions 等），避免 E1 的 fake-prom 随机端口 URL 持久化污染 F1；(4) `startMu` 串行化 Start 防并行测试 race DB schema；(5) `ONGRID_FRONTIER_DISABLED=true` 跳过 geminio dial，让 manager 无 broker 也能起来。

## 2. 包信息

- **包名**：`testenv`
- **所属模块**：`tests/e2e/testenv`
- **依赖方向**：被 e2e 测试用例调用；依赖 `testcontainers-go/modules/mysql`、`go-sql-driver/mysql`

## 3. 关键类型与接口

```go
type Env struct {
    t   *testing.T
    cfg envConfig

    httpBase string
    cmd      *exec.Cmd
    logBuf   *bytes.Buffer

    llm      *FakeLLM
    slack    *FakeSlack
    telegram *FakeTelegram
    prom     *FakeProm

    AdminEmail    string  // "admin@ongrid.local"
    AdminPassword string  // "E2E!Admin-pass-do-not-reuse"

    stopOnce sync.Once
}

type envConfig struct{ extraEnv map[string]string }
type Option func(*envConfig)
func WithEnv(k, v string) Option  // 叠加 ONGRID_* 环境变量

var (
    mysqlOnce      sync.Once
    mysqlHostPort  string
    mysqlContainer *tcmysql.MySQLContainer
    mysqlErr       error

    binaryOnce sync.Once
    binaryPath string
    binaryErr  error

    startMu sync.Mutex  // 串行化 Start 防 schema race
)
```

## 4. 关键函数与流程

### `Start(t, opts...) *Env`

- **职责**：拉起一个完整测试环境。
- **流程**：
  1. 应用 opts 到 cfg。
  2. `sharedMySQL(t)` 取 DSN。
  3. `managerBinary(t)` 取编译好的二进制路径。
  4. 构造 Env，初始化 4 个 fake，设置 AdminEmail/Password。
  5. `t.Cleanup(env.Stop)` 注册清理。
  6. `freePort()` 取随机 HTTP + metrics 端口。
  7. 构造 managerEnv map（ONGRID_HTTP_ADDR / DB_DSN / JWT_SECRET / Admin 凭据 / Prom URL / LLM URL / 各 provider fake URL / ALERT_EVAL_INTERVAL=30s / AGENT_KERNEL=graph / BUILTIN_AGENTS_ROOT / SKILLS_ROOT / FRONTIER_DISABLED=true）。
  8. 叠加 cfg.extraEnv。
  9. `startManager(binary, managerEnv)`；失败 dumpLogs + t.Fatal。
  10. `waitReady(20s)`；失败 dumpLogs + t.Fatal。
- **错误处理**：任何启动失败 t.Fatal（测试无法继续）；logBuf 经 dumpLogs 写入 t.Logf。

### `Stop()`

- **职责**：idempotent 关闭（t.Cleanup 可能调用，deferred Stop 也会调用）。
- **流程**：`stopOnce.Do`：
  1. cmd 进程发 SIGTERM；等 5s；超时 SIGKILL。
  2. 4 个 fake.Close()。
- **错误处理**：忽略所有关闭错误（_ =）。

### HTTP helpers

- **`DoJSON(method, path, body, bearer) (status, body, err)`**：构造请求；body 非 nil 设 Content-Type: application/json；bearer 非空设 Authorization；读响应并 unmarshal。
- **`LoginAdmin() LoginResult`**：用 AdminEmail/Password 调 Login。
- **`Login(email, password) LoginResult`**：POST /api/v1/auth/login；非 200 t.Fatal；提取 access_token / refresh_token。

### Fake accessors

- `FakeLLM()` / `FakeSlack()` / `FakeTelegram()` / `FakeProm()` / `BaseURL()` / `ManagerLogs()`。

### `sharedMySQL(t) string`

- **职责**：包级共享一个 MySQL 容器。
- **流程**：
  1. `mysqlOnce.Do`：禁用 ryuk（mac flakiness + 慢）；5min timeout；`tcmysql.Run("mysql:8.0", ...)`；记录 host + mapped port。
  2. 构造 DSN（parseTime / charset / loc / multiStatements）。
  3. **关键**：DROP 跨测试易泄漏表（system_settings / alert_rules / alert_incidents / alert_events / investigation_reports / notification_channels / notification_dispatches / chat_sessions / chat_messages / chat_tool_calls / audit_events / users / orgs / memberships / user_agents）。
  4. DROP 失败容忍（首次 Start 无表，"Unknown table" 错误忽略；其他错误仅 Logf 不 Fatal）。
- **错误处理**：mysqlErr t.Fatal；DROP 错误容忍。

### `managerBinary(t) string`

- **职责**：包级共享一个编译好的 manager 二进制。
- **流程**：`binaryOnce.Do`：repoRoot 找仓库根；MkdirTemp；`go build -o <tmp>/ongrid-manager ./cmd/ongrid`；记录路径。
- **错误处理**：binaryErr t.Fatal（含 build stderr）。

### `startManager(binary, envMap)`

- **职责**：spawn manager 进程。
- **流程**：`startMu.Lock`（串行化）；logBuf = bytes.Buffer；exec.Command(binary)；cmd.Env = mergedEnv(envMap)；Stdout/Stderr = logBuf；cmd.Start()。
- **错误处理**：Start err 返回（caller t.Fatal + dumpLogs）。

### `waitReady(d) error`

- **职责**：轮询 /healthz 直到 200 或超时。
- **流程**：deadline；循环：若 cmd.ProcessState.Exited() → 提前返回"manager exited before ready"；GET /healthz；200 → nil；150ms sleep。
- **错误处理**：超时返回 error。

### `dumpLogs()`

- 把 logBuf 写入 `t.Logf("=== manager logs ===\n%s\n=== end ===")`，便于失败诊断。

### `mergedEnv(envMap) []string`

- **职责**：合并 parent env 与测试 envMap。
- **流程**：os.Environ() → 剥离所有 `ONGRID_*` 前缀（让测试完全控制 config）→ append envMap 的 `K=V`。
- **关键**：剥离 parent ONGRID_* 防测试 runner 环境污染。

### `repoRoot() string`

- 从本文件源码位置向上找 go.mod（最多 8 层）。

### `randomSuffix() string` / `readFull(b)`

- 8 hex 字符随机后缀（JWT secret 用）；读 /dev/urandom，失败降级为 time-based 低质量随机。

### `TerminateSharedMySQL()`

- 供 TestMain 在进程退出时调用，释放容器（防 ~500MB/容器泄漏）；ryuk 已禁用，此为唯一 reap 途径。

## 5. 依赖关系

- **内部包**：无（testenv 是测试基础设施）
- **外部库**：`testcontainers-go/modules/mysql`、`go-sql-driver/mysql`、标准库（os/exec / database/sql / net/http / encoding/json / syscall / runtime / sync / time / bufio / io / fmt / strings / bytes / context / errors / filepath）
- **被调用方**：所有 e2e 测试用例（`tests/e2e/**`）

## 6. 并发与资源管理

- **`mysqlOnce` / `binaryOnce`（sync.Once）**：包级单例；首次调用构造，后续复用。
- **`startMu`（sync.Mutex）**：串行化 Start —— 并行测试若同时 spawn manager 会 race DB schema（DROP + AutoMigrate）。
- **`stopOnce`（sync.Once）**：保证 Stop idempotent；t.Cleanup + deferred Stop 都安全。
- **logBuf 非并发安全**：但 manager 进程单写 + 测试单读，无竞争。
- **fake server 各自独立**：httptest.Server 自带并发安全。
- **TerminateSharedMySQL 需 TestMain 调用**：否则容器泄漏（ryuk 已禁用）。

## 7. 设计模式与亮点

- **共享容器 + 独立进程**：MySQL 容器跨测试共享（省 ~500MB + 启动时间）；每测试独立 manager 进程（隔离状态）。
- **DROP 而非 TRUNCATE**：注释解释为何 DROP —— AutoMigrate 在 manager 启动时重建；DROP 比 TRUNCATE 更彻底（清索引 / 外键）。
- **startMu 串行化**：注释明示"two parallel tests don't race the schema"；牺牲并行度换正确性。
- **AdminEmail/Password 共享**：注释详述 BootstrapAdmin 仅首次 seed，后续 Starts 见 users>0 跳过；首测试设的密码必须全测试通用。
- **剥离 parent ONGRID_***：防测试 runner 环境变量污染；注释明示"we want a clean slate so the test fully controls config"。
- **waitReady 早检测崩溃**：`cmd.ProcessState.Exited()` 防止"poll for nothing" —— 进程已死时立即失败而非等超时。
- **dumpLogs 失败诊断**：启动失败 / ready 失败都 dumpLogs 到 t.Logf，便于 CI 诊断。
- **FrontierDisabled=true**：e2e 不依赖真 frontier broker；edge-tunnel-only 功能用 RequireSecret 风格 gate 跳过。
- **AGENT_KERNEL=graph**：注释明示 graph kernel 是生产 runtime；legacy 不构建 chatruntime，investigator/agent 路径会"not wired"。
- **BUILTIN_AGENTS_ROOT / SKILLS_ROOT**：manager 在 tempdir 启动，相对路径不指向 repo；显式指向 repo 的 agents/ skills/ 目录。
- **readFull /dev/urandom 降级**：无 /dev/urandom 时用 time-based 低质量随机（注释明示"deterministic non-zero"）。

## 8. 注意事项

- **`//go:build e2e` 标签**：普通 `go test ./...` 不编译本包；需 `go test -tags e2e ./tests/e2e/...`。
- **AdminPassword 是已知凭据**：在 ephemeral test MySQL 容器内，"known credentials" 表面受限于本进程；注释明示"do-not-reuse"。
- **sharedMySQL DROP 17 张表**：新增表若跨测试泄漏需加入 DROP 列表。
- **managerBinary 缓存**：首次 build 后复用；若代码变更需 `go clean -testcache` 或删除 tmpdir。
- **freePort 竞态**：注释明示 Close 与 manager Listen 间有微小 race；waitReady 20s 内兜底。
- **ONGRID_TUNNEL_ADDR=127.0.0.1:0**：disabled in practice；e2e 从不 dial。
- **Prom URL 用 fake URL**：ONGRID_PROM_URL / PROM_QUERY_URL 都指向 FakeProm。
- **Loki / Tempo 默认空 URL**：ONGRID_LOG_QUERY_URL / TRACE_QUERY_URL 空，Loki/Tempo disabled。
- **Anthropic 用不同 base path**：ONGRID_ANTHROPIC_BASE_URL 不带 /v1（Anthropic 用 /v1/messages）；OpenAI / Zhipu 带 /v1。
- **TerminateSharedMySQL 需 TestMain**：未注册 TestMain 的包会泄漏容器（ryuk 已禁用）。
- **randomSuffix 用 /dev/urandom**：Windows 无此文件会降级；注释提及。
- **repoRoot 向上找 go.mod**：最多 8 层；超过返回 ""（binaryErr）。
