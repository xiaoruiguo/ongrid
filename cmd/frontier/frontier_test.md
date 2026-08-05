# frontier 二进制验证记录

> 本文件记录 `cmd/frontier/{server,client}` 二进制对的编译、vet、单元测试验证过程与结果。验证日期:2026-08-05。

## 环境

| 项 | 值 |
|---|---|
| 操作系统 | Linux (WSL2) |
| Go 工具链 | `go1.25.0 linux/amd64`(由 `go.mod` 要求 `go 1.25.0` 触发自动下载) |
| 本地基础 Go | `go1.24.5`(系统自带,不满足 go.mod,需通过 `GOTOOLCHAIN` 拉取 1.25) |
| GOPROXY | `https://goproxy.cn`(默认 `proxy.golang.org` 在该网络环境超时) |
| GOTOOLCHAIN | `go1.25.0` |
| 工作目录 | `/mnt/d/claude/ongrid` |
| 验证范围 | `./cmd/frontier/...` + 全仓库 `./...` 回归 |

## 验证步骤

### 1. 编译两个二进制

```bash
GOPROXY=https://goproxy.cn GOTOOLCHAIN=go1.25.0 \
  go build ./cmd/frontier/server/ ./cmd/frontier/client/
```

**结果**:`EXIT=0`,无任何编译错误或警告。

首次执行触发依赖下载(`github.com/singchia/geminio v1.3.0-rc.2`、`github.com/singchia/frontier v1.2.4`、`github.com/prometheus/client_golang v1.20.5` 等),二次执行命中缓存即时返回。

### 2. 静态检查 `go vet`

```bash
GOPROXY=https://goproxy.cn GOTOOLCHAIN=go1.25.0 \
  go vet ./cmd/frontier/...
```

**结果**:`EXIT=0`,无 vet 告警。

### 3. 单元测试(单线程)

```bash
GOPROXY=https://goproxy.cn GOTOOLCHAIN=go1.25.0 \
  go test -count=1 ./cmd/frontier/...
```

**结果**:`EXIT=0`,两个包均 `ok`:

```
ok  	github.com/ongridio/ongrid/cmd/frontier/client	0.088s
ok  	github.com/ongridio/ongrid/cmd/frontier/server	0.075s
```

### 4. 单元测试 + race 检测

```bash
GOPROXY=https://goproxy.cn GOTOOLCHAIN=go1.25.0 \
  go test -race -count=1 ./cmd/frontier/...
```

**结果**:`EXIT=0`,无数据竞争:

```
ok  	github.com/ongridio/ongrid/cmd/frontier/client	1.090s
ok  	github.com/ongridio/ongrid/cmd/frontier/server	1.089s
```

### 5. 单元测试 verbose(逐用例确认)

```bash
GOPROXY=https://goproxy.cn GOTOOLCHAIN=go1.25.0 \
  go test -v -count=1 ./cmd/frontier/...
```

**结果**:`EXIT=0`,33 个用例全部 `PASS`。

#### Client 包(`cmd/frontier/client`,17 用例)

| # | 用例 | 耗时 | 状态 |
|---|---|---|---|
| 1 | `TestLoadClientConfig_RequiresCloudAddr` | 0.00s | PASS |
| 2 | `TestLoadClientConfig_RequiresAccessKey` | 0.00s | PASS |
| 3 | `TestLoadClientConfig_RequiresSecretKey` | 0.00s | PASS |
| 4 | `TestLoadClientConfig_DefaultHeartbeatInterval` | 0.00s | PASS |
| 5 | `TestLoadClientConfig_CustomHeartbeatInterval` | 0.00s | PASS |
| 6 | `TestLoadClientConfig_InvalidHeartbeatIntervalFallsBackToDefault` | 0.00s | PASS |
| 7 | `TestLocalHostInfo_PopulatesBasicFields` | 0.00s | PASS |
| 8 | `TestRegisterHandlers_NilClientReturnsError` | 0.00s | PASS |
| 9 | `TestRegisterHandlers_NilLoggerFallsBackToDefault` | 0.00s | PASS |
| 10 | `TestRegisterHandlers_RegistersEchoHandler` | 0.00s | PASS |
| 11 | `TestRegisterHandlers_EchoHandlerEchoesBody` | 0.00s | PASS |
| 12 | `TestRegisterHandlers_RegistersOneReconnectCallback` | 0.00s | PASS |
| 13 | `TestRegisterHandlers_RegisterEdgeCallsClientCall` | 0.00s | PASS |
| 14 | `TestRegisterHandlers_RegisterEdgePropagatesCallError` | 0.00s | PASS |
| 15 | `TestRegisterHandlers_ReconnectCallbackReRegisters` | 0.00s | PASS |
| 16 | `TestStartHeartbeatTicker_NegativeIntervalDoesNothing` | 0.02s | PASS |
| 17 | `TestStartHeartbeatTicker_SendsHeartbeatOnTick` | 0.05s | PASS |

汇总:`ok github.com/ongridio/ongrid/cmd/frontier/client 0.097s`

#### Server 包(`cmd/frontier/server`,16 用例)

| # | 用例 | 耗时 | 状态 |
|---|---|---|---|
| 1 | `TestLoadServerConfig_Defaults` | 0.00s | PASS |
| 2 | `TestLoadServerConfig_ExplicitEnvOverridesDefaults` | 0.00s | PASS |
| 3 | `TestLoadServerConfig_MissingAddrWhenNotDisabled` | 0.00s | PASS |
| 4 | `TestLoadServerConfig_DisabledAllowsEmptyAddr` | 0.00s | PASS |
| 5 | `TestLoadServerConfig_DisabledAcceptsOne` | 0.00s | PASS |
| 6 | `TestDeriveEdgeID_IsDeterministic` | 0.00s | PASS |
| 7 | `TestDeriveEdgeID_DifferentKeysYieldDifferentIDs` | 0.00s | PASS |
| 8 | `TestDeriveEdgeID_EmptyAccessKeyReturnsZero` | 0.00s | PASS |
| 9 | `TestDeriveEdgeID_NeverReturnsZeroForNonEmptyKey` | 0.00s | PASS |
| 10 | `TestAddrString_Nil` | 0.00s | PASS |
| 11 | `TestAddrString_TCPAddr` | 0.00s | PASS |
| 12 | `TestInstallHandlers_NilClientReturnsError` | 0.00s | PASS |
| 13 | `TestInstallHandlers_DisabledClientRegistersWithoutError` | 0.00s | PASS |
| 14 | `TestInstallHandlers_DisabledClientCallReturnsErrDisabled` | 0.00s | PASS |
| 15 | `TestInstallHandlers_NilLoggerFallsBackToDefault` | 0.00s | PASS |
| 16 | `TestRunServer_DisabledModeReturnsOnContextCancel` | 0.05s | PASS |

汇总:`ok github.com/ongridio/ongrid/cmd/frontier/server 0.068s`

`TestRunServer_DisabledModeReturnsOnContextCancel` 的日志输出(slog 默认 text handler):

```
2026/08/05 15:03:34 WARN frontier-server: disabled (ONGRID_FRONTIER_DISABLED=true) — all calls will return ErrDisabled
2026/08/05 15:03:34 INFO frontier-server ready addr="" service_name=ongrid-manager disabled=true version=dev
2026/08/05 15:03:34 INFO frontier-server shutting down
```

### 6. 全仓库构建回归

```bash
GOPROXY=https://goproxy.cn GOTOOLCHAIN=go1.25.0 \
  go build ./...
```

**结果**:`EXIT=0`,新增的 `cmd/frontier/{server,client}` 未破坏其它任何包的构建。

## 测试覆盖范围说明

### 已覆盖

| 函数 | 覆盖维度 |
|---|---|
| `loadServerConfig` | 默认值 / env 显式覆盖 / 缺 Addr 报错 / Disabled 模式允许空 Addr / Disabled 接受 `"1"` |
| `loadClientConfig` | 缺 CloudAddr/AccessKey/SecretKey 分别报错 / 默认心跳 30s / 自定义心跳 / 非法 duration 回退默认 |
| `deriveEdgeID` | 确定性(同 key 同 id) / 不同 key 不同 id / 空 access_key 返回 0 / 非 empty key 永不返回 0 |
| `addrString` | nil 安全 / `*net.TCPAddr` 格式化 |
| `installHandlers` | nil 客户端报错 / nil logger 回退默认 / Disabled 客户端注册无错 / Disabled 客户端 Call 返回 `ErrDisabled` |
| `runServer` | Disabled 模式 ctx cancel 后干净退出(含 metrics HTTP 服务优雅关闭) |
| `localHostInfo` | OS/Arch 非空 / Hostname 与 `os.Hostname()` 一致 |
| `registerHandlers` | nil 客户端报错 / nil logger 回退 / echo handler 注册成功 / echo 行为(原样返回 body) / 注册 1 个 OnReconnect 回调 / register_edge 调用 `Client.Call` 且 req 字段正确 / Call 错误正确包装 / 重连触发重发 |
| `startHeartbeatTicker` | 负 interval 不启动 goroutine / 正常 interval 周期触发 `MethodHeartbeat` Call |

### 未覆盖(原因与缓解)

| 项 | 原因 | 缓解 |
|---|---|---|
| 真实 frontier broker 端到端 dial | 上游 `github.com/singchia/frontier` 不暴露 in-process broker 构造 API;仓库现有测试(`tests/e2e`)也用 `ONGRID_FRONTIER_DISABLED=true` 跳过 broker | 沿用 `frontierbound/client_test.go:70-148` 的 `fakeService` 模式 + `NewDisabled` 退化模式覆盖单元层;e2e 层由 `tests/e2e/testenv` 统一管理,新增 cmd 不应自己拉起 broker |
| `runClient` 全流程 | 涉及真实网络 dial,与 frontier broker 耦合 | `registerHandlers`/`loadClientConfig`/`startHeartbeatTicker`/`localHostInfo` 全部分别单元覆盖;`runClient` 仅做装配串联,逻辑薄 |
| `frontierbound.Client.Register` 的真实注册路径 | `*frontierbound.Client` 不导出测试 seam(`newWithService` unexported) | 用 `NewDisabled`(production 退化模式)间接验证 `Register*` 不 panic、`Call` 返回 `ErrDisabled`;真实注册路径由 `internal/manager/service/frontierbound/client_test.go` 已覆盖 |

## 工具链备注

- `go.mod` 声明 `go 1.25.0`;系统自带 `/usr/local/go/bin/go` 为 `1.24.5`,直接 `go build` 会报 `go.mod requires go >= 1.25.0 (running go 1.24.5; GOTOOLCHAIN=local)`。需用 `GOTOOLCHAIN=go1.25.0`(或 `auto` + 可达 `proxy.golang.org`)触发工具链自动下载。
- 默认 `GOPROXY=https://proxy.golang.org` 在该网络环境 i/o timeout,改用 `https://goproxy.cn` 后工具链与依赖下载均正常。
- 验证命令统一前缀:`GOPROXY=https://goproxy.cn GOTOOLCHAIN=go1.25.0`。

## 结论

- ✅ `go build ./cmd/frontier/{server,client}/` 通过
- ✅ `go vet ./cmd/frontier/...` 通过
- ✅ `go test -count=1 ./cmd/frontier/...` 通过(33/33)
- ✅ `go test -race -count=1 ./cmd/frontier/...` 通过(无数据竞争)
- ✅ `go build ./...` 全仓库构建未受影响

`cmd/frontier/server` 与 `cmd/frontier/client` 二进制对实现完整、测试覆盖到位,可进入 review。

---

## 附录:运行 `cmd/frontier/server` 的配置与命令

> 本节给出 standalone `frontier-server` 二进制正确运行所需的完整配置文件、环境变量与启动命令。二进制本身只是 `frontierbound` 客户端的薄壳,真正的运行依赖是上游 `frontier` broker 容器(参见 `cmd/frontier/server/main.go:1-25` 的拓扑图)。

### A.1 拓扑回顾

```
edge (frontier-client) ──▶ frontier:40012 (edgebound)
                                │
                                ▼
frontier-server ──▶ frontier:40011 (servicebound)
```

`frontier-server` 只拨 `40011` servicebound 端口;`40012` edgebound 端口给 `frontier-client`(或生产 `ongrid-edge`)使用。两个端口都由同一个 frontier broker 容器提供。

### A.2 必需的 frontier broker 配置

broker 配置文件路径(被 docker compose 只读挂载到容器内 `/usr/conf/frontier.yaml`):`deploy/install/frontier.yaml`。最小内容(已存在,无需新建):

```yaml
# deploy/install/frontier.yaml
edgebound:
  listen:
    network: tcp
    addr: 0.0.0.0:40012
  # GetEdgeID 服务不可用时拒绝分配临时 ID(fail closed)
  edgeid_alloc_when_no_idservice_on: false

servicebound:
  listen:
    network: tcp
    addr: 0.0.0.0:40011
```

`edgeid_alloc_when_no_idservice_on: false` 是关键红线:确保 `frontier-server` 未上线期间,边缘端不会拿到无主临时 ID,否则 `frontier-server` 恢复后 ID 仍挂在已断的连接上(参见 `cmd/frontier/server/main.go:128-131` 的 deriveEdgeID 注释)。

### A.3 `frontier-server` 二进制的环境变量

| 变量 | 必需 | 默认 | 说明 |
|---|---|---|---|
| `ONGRID_FRONTIER_ADDR` | 是(除非 Disabled) | — | frontier service-bound 拨号地址,如 `frontier:40011` 或 `127.0.0.1:40011` |
| `ONGRID_FRONTIER_SERVICE_NAME` | 否 | `ongrid-manager` | broker 上识别本服务的名字;与 `cmd/ongrid` 默认值对齐 |
| `ONGRID_FRONTIER_DISABLED` | 否 | — | `true` / `1` 跳过拨号(退化模式,所有 Call/OpenStream/NotifyX 返回 `ErrDisabled`) |
| `ONGRID_FRONTIER_SERVER_METRICS_ADDR` | 否 | `:9102` | 本地 debug HTTP 监听地址,暴露 `/healthz` `/readyz` `/metrics` |

> 端口选择:默认 `:9102` 故意避开 `:9100`(由 `cmd/ongrid` 占用)。同一主机上同时跑 `ongrid` 与 `frontier-server` 时,二者不冲突。

### A.4 构建二进制

```bash
# 与本文件"验证步骤"统一前缀
GOPROXY=https://goproxy.cn GOTOOLCHAIN=go1.25.0 \
  go build -o bin/frontier-server ./cmd/frontier/server/
```

或带版本注入:

```bash
VERSION=v0.10.2
GOPROXY=https://goproxy.cn GOTOOLCHAIN=go1.25.0 \
  go build -ldflags "-X main.version=$VERSION" \
  -o bin/frontier-server ./cmd/frontier/server/
```

### A.5 启动方式 1:配合 docker-compose 起的 frontier broker

适用于本机开发或冒烟测试,完整链路:`docker compose` 起 frontier broker → 本机 `frontier-server` 拨入 → 本机 `frontier-client` 模拟边缘端。

**步骤 1:仅启动 frontier broker**(避免起 ongrid / mysql / nginx 等其它服务)

```bash
cd deploy
docker compose up -d frontier
# 验证 broker 已监听
docker compose ps frontier
# 期望:端口 40012 已发布到 host,40011 仅容器内可达
```

**步骤 2:本机启动 `frontier-server`**

由于 docker compose 仅把 `40012` 映射到 host,`40011` 未发布,`frontier-server` 必须在容器网络内访问。两种方案:

- 方案 A(推荐):临时把 `40011` 也发布到 host —— 在 `deploy/docker-compose.yml` 的 `frontier.ports` 加一行 `"40011:40011"`,然后:
  ```bash
  ONGRID_FRONTIER_ADDR=127.0.0.1:40011 \
  ONGRID_FRONTIER_SERVICE_NAME=ongrid-manager \
  ONGRID_FRONTIER_SERVER_METRICS_ADDR=:9102 \
    ../bin/frontier-server
  ```
- 方案 B:把 `frontier-server` 也跑成 docker 容器加入 `ongrid_net`,直接用服务名 `frontier:40011`:
  ```bash
  ONGRID_FRONTIER_ADDR=frontier:40011 \
  ONGRID_FRONTIER_SERVICE_NAME=ongrid-manager \
  ONGRID_FRONTIER_SERVER_METRICS_ADDR=:9102 \
    bin/frontier-server
  ```

**步骤 3(可选):启动 `frontier-client` 模拟边缘端**

```bash
ONGRID_EDGE_CLOUD_ADDR=127.0.0.1:40012 \
ONGRID_EDGE_ACCESS_KEY=demo-ak \
ONGRID_EDGE_SECRET_KEY=demo-sk \
  bin/frontier-client
```

成功时 `frontier-server` 日志应出现:

```
{"level":"INFO","msg":"edge online","edge_id":<fnv64 of demo-ak:demo-sk>,"addr":"..."}
{"level":"INFO","msg":"register_edge","edge_id":<id>,"hostname":"...","agent_version":"dev","body_bytes":...}
```

`frontier-client` 日志应出现:

```
{"level":"INFO","msg":"register_edge ok","edge_id":<id>,"server_time":...}
{"level":"INFO","msg":"frontier-client ready",...}
```

### A.6 启动方式 2:Disabled 退化模式(无需 broker)

适用于 e2e 测试或本机无 docker 环境,仅验证 HTTP debug 接口与 handler 装配路径。

```bash
ONGRID_FRONTIER_DISABLED=true \
ONGRID_FRONTIER_SERVER_METRICS_ADDR=:9102 \
  bin/frontier-server
```

预期日志:

```
{"level":"WARN","msg":"frontier-server: disabled (ONGRID_FRONTIER_DISABLED=true) — all calls will return ErrDisabled"}
{"level":"INFO","msg":"frontier-server ready","addr":"","service_name":"ongrid-manager","disabled":true,"version":"dev"}
```

健康检查:

```bash
curl http://localhost:9102/healthz  # → 200 ok
curl http://localhost:9102/readyz   # → 200 ready (installHandlers 完成后 ready.Store(true))
curl http://localhost:9102/metrics  # → Prometheus exposition
```

退出:`SIGINT` / `SIGTERM`(`Ctrl+C` 或 `kill <pid>`),metrics HTTP 服务 5 秒优雅关闭后进程退出。

### A.7 `--help` / `--version` 自检

```bash
bin/frontier-server --version    # → frontier-server dev
bin/frontier-server --help       # → 打印环境变量清单(与 A.3 表一致)
```

### A.8 常见错误

| 现象 | 原因 | 解决 |
|---|---|---|
| `config: frontier-server: ONGRID_FRONTIER_ADDR is required ...` | 未设置 `ONGRID_FRONTIER_ADDR` 且未 Disabled | 设置环境变量,或加 `ONGRID_FRONTIER_DISABLED=true` |
| `frontierbound: new client: dial tcp ...:40011: connection refused` | broker 容器未起 / 40011 端口未发布到 host | `docker compose up -d frontier` + 检查 `ports` 映射 |
| `frontier-server: RegisterGetEdgeID: ...` | broker 已起但 `edgeid_alloc_when_no_idservice_on: false` 触发 fail closed,边缘端先于 server 上线 | 先起 `frontier-server`,再起 `frontier-client` |
| `bind :9102: address already in use` | `:9102` 端口被占(常见于上一次进程未干净退出) | 改 `ONGRID_FRONTIER_SERVER_METRICS_ADDR=:9103` 或杀掉占用进程 |
| `frontier-client` 日志 `register_edge: ... EOF` | `frontier-server` 已退出 / broker 重启 | 重启 `frontier-server`,`frontier-client` 的 `OnReconnect` 会自动重注册(见 `cmd/frontier/client/main.go:150-155`) |

