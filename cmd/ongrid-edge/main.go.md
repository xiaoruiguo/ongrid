# `main.go`（ongrid-edge）技术实现文档

> 源文件：`cmd/ongrid-edge/main.go`
> 包路径：`github.com/ongridio/ongrid/cmd/ongrid-edge`

## 1. 概述

本文件是 `ongrid-edge` 边端二进制的入口。它负责：识别并分发 K8s 主机运行时/升级准备子命令；处理 `--version` / `--help` 等打印即退出标志；加载配置；完成 K8s 注册（首次 bootstrap 或加载已存储凭据）；构造 collector、tunnel 客户端、各类 host capability handler（host_files / restart_service / bash / webshell）；启动 plugin supervisor、K8s inventory/metrics pusher、本地 metrics HTTP 服务；通过 `errgroup` 编排所有长生命周期 goroutine 并等待退出。

## 2. 包信息

- **包名**：`main`
- **所属模块**：`cmd/ongrid-edge`（命令入口层，cmd → ... 分层最顶层）
- **依赖方向**：被操作系统启动；调用 `internal/pkg/*`（config / httpserver / logger / prom / tunnel）与 `internal/edgeagent/*`（biz / collector / k8s / plugins / host_files / restart_service / bash / webshell / service）

## 3. 关键类型与接口

```go
// K8s 注册请求体
type k8sEnrollRequest struct {
    ClusterID    uint64
    ClusterUID   string
    Role         string
    NodeName     string
    NodeUID      string
    ProviderID   string
    Namespace    string
    AgentVersion string
    Capabilities []string
}

// K8s 注册响应体（含 edge 凭据 + 可选 telemetry 配置）
type k8sEnrollResponse struct {
    ClusterID        uint64
    Role, Mode       string
    EdgeID           uint64
    AccessKey        string
    SecretKey        string
    CloudAddr        string
    ManagerPublicURL string
    Telemetry        *k8sTelemetryConfig
}

// Telemetry 配置（traces/logs/remote_write 端点 + 认证 + TLS）
type k8sTelemetryConfig struct {
    ClusterID              uint64
    AccessKey, SecretKey   string
    TracesEndpoint         string
    TracesAuthMode         string
    TracesBasicUser        string
    TracesBasicPass        string
    TracesTLSInsecure      bool
    LogsEndpoint           string
    LogsAuthMode           string
    LogsBasicUser          string
    LogsBasicPass          string
    LogsTLSInsecure        bool
    RemoteWriteEndpoint    string
    RemoteWriteBearer      string
    RemoteWriteBasicUser   string
    RemoteWriteBasicPass   string
    RemoteWriteTLSInsecure bool
    RemoteWriteTLSCAPEM    string
}

// 适配器：把 collector 包的 Collector 接口桥接到 biz 包的同形接口
// 存在原因：避免 biz/agent.go 直接 import collector 包（collector 依赖 tunnel 类型，
// 直接 import 会形成循环）
type collectorAdapter struct {
    c edgecollector.Collector
}
```

## 4. 关键函数与流程

### `main`
- **职责**：edge 进程入口，编排整个启动流程
- **流程**：
  1. **子命令分发**：依次尝试 `runK8sHostCommand`（install/enter 主机运行时）、`runK8sUpgradeCommand`（升级准备）；任一 handled=true 则处理后直接 return
  2. **打印即退出标志**：遍历 `os.Args` 识别 `--version/-v`、`--help/-h`，输出后 return——必须在可能失败的配置加载之前，确保 install.sh 等脚本能稳定获取版本
  3. **配置加载**：`config.Load()` 失败则 `os.Exit(1)`
  4. **日志初始化**：`logger.WithService(logger.New(LevelInfo), "ongrid-edge")`
  5. **根上下文**：`signal.NotifyContext(SIGINT, SIGTERM)` 支持优雅关闭
  6. **数据平面模式检查**：`runK8sDataPlaneMode` 根据 `ONGRID_EDGE_MODE` 进入 gateway/scraper 模式；若进入则 return
  7. **K8s 注册**：`ensureK8sEnrollment` 完成首次 bootstrap 或加载已存储凭据；失败 `os.Exit(1)`
  8. **Prometheus registry**：`prom.NewRegistry()`
  9. **Tunnel 客户端**：`tunnel.NewClient` 用 cfg.Edge 配置构造到 manager 的长连接客户端
  10. **errgroup 编排**：
      - 若 controller 且配置了 telemetry secret → `runK8sTelemetryConfigSync` goroutine（周期刷新 telemetry 配置）
      - `buildCollector` 构造 collector（off/auto/scrape/embedded 四种模式）
      - `edgesvc.RegisterWithCollector` 注册 on-demand RPC handler
      - **Host capability 注册**（仅 node 角色或非 K8s 模式）：
        - `edgehostfiles.Register`（find_large_files / du_summary / stat_file）
        - `edgerestartservice.Register`（重启 systemd 服务，首个 mutating skill）
        - `edgebash.Register`（通用只读 shell，受 cmdpolicy 约束）
        - `edgewebshell.Register`（WebSSH 字节转发）
        - 任一注册失败仅 Warn 不退出——capability 降级而非崩溃
      - controller 角色则禁用 host handler 并记录日志
      - **Agent 构造**：`edgebiz.NewAgent` 含 metrics interval、version、k8sInfo、upgrade stage dir
      - **K8s controller 专属**：
        - `edgek8s.NewInventoryPusher` + `RegisterHandlers` + `Run` goroutine（周期推送集群资源清单）
        - 可选 `edgek8s.NewMetricsPusher` + `Run` goroutine（推送集群指标）
      - **本地 metrics HTTP 服务**：`:9101`，暴露 `/metrics` `/healthz`
      - **Agent 主循环**：`agent.Run` goroutine，defer cancel() 确保 agent 退出时关闭所有兄弟 goroutine
      - **Plugin supervisor**（非 controller 或开启 telemetry gateway 时）：
        - 注册 logs/traces/metrics/custommetrics/databasemetrics/hostmetrics/procmetrics 等插件
        - `agent.SetPluginHealthFn` 把 supervisor 健康状态桥接到 tunnel 上报
        - 注册 `MethodPluginConfigsChanged` handler，收到通知时 `supervisor.TriggerReload`
        - `supervisor.Run` goroutine
  12. **等待退出**：`eg.Wait()`；非 `context.Canceled` 的错误 `os.Exit(1)`
- **错误处理**：配置加载、K8s 注册、collector 构造失败立即 `os.Exit(1)`；capability 注册失败仅 Warn；errgroup 错误统一在 wait 后处理

### `buildCollector`
- **签名**：`func buildCollector(ctx context.Context, cfg *config.Config, log *slog.Logger, eg *errgroup.Group) (edgebiz.Collector, *edgecollector.Scraper, error)`
- **职责**：根据 `cfg.Edge.CollectorMode` 构造 collector
- **模式**：
  - `off/none/""`：默认，不周期推送；用 `NewNoopPush` 包装 embedded collector，on-demand RPC 仍可用
  - `auto`：legacy，embedded + scraper 组合
  - `scrape`：仅 scraper，需 scrape config 文件
  - `embedded`：仅 embedded push
- **scraper 生命周期**：scrape 模式下 `scraper.Run` goroutine 加入 errgroup，共享 agent 生命周期

### `ensureK8sEnrollment`
- **签名**：`func ensureK8sEnrollment(ctx context.Context, cfg *config.Config, log *slog.Logger) (*tunnel.KubernetesInfo, error)`
- **职责**：完成 K8s 模式下的 edge 注册
- **流程**：
  1. 检查环境变量（`ONGRID_K8S_CLUSTER_ID` / `ONGRID_K8S_BOOTSTRAP_TOKEN` / `ONGRID_EDGE_MODE`），都不设置 → 返回 nil（非 K8s 模式）
  2. 校验 `ONGRID_K8S_CLUSTER_ID` 非空且为正数
  3. 确定 role（环境变量或根据 edge_mode 推断）
  4. `edgek8s.DiscoverClusterUID` 发现集群 UID（10s 超时）
  5. `loadStoredK8sCredential` 尝试加载已存储凭据；加载成功且为 controller → 刷新 telemetry 配置
  6. 加载失败但有 bootstrap token → 调用 manager `/internal/k8s/enroll` 注册
  7. 注册响应含 AccessKey/SecretKey → 写入 cfg + `storeK8sCredential` 持久化
- **错误处理**：cluster id 非法、cluster UID 发现失败、enroll HTTP 失败、响应缺字段 → 明确错误

### `refreshK8sTelemetryConfig` / `refreshAndStoreK8sTelemetryConfig`
- **职责**：向 manager 请求最新 telemetry 配置并存储
- **流程**：POST `/internal/k8s/telemetry-config`，basic auth 用 edge 凭据；响应校验 ClusterID/AccessKey/SecretKey 非空；`applyManagerTelemetryTLS` 处理 manager 同源 TLS；`storeK8sTelemetryConfig` 持久化

### `runK8sTelemetryConfigSync`
- **签名**：`func runK8sTelemetryConfigSync(ctx context.Context, cfg *config.Config, info *tunnel.KubernetesInfo, log *slog.Logger) error`
- **职责**：周期性刷新 telemetry 配置（默认 1 分钟）
- **流程**：ticker 循环；刷新失败仅 Warn 保留旧配置；ctx 取消返回 nil

### `collectorAdapter` 方法
- `CollectAll`：把 `edgecollector.CollectorOutput` 转换为 `edgebiz.CollectorOutput`（字段逐个复制）
- `HostInfo` / `GetHostLoad` / `GetProcessList`：直接转发

### 辅助函数
- `k8sManagerHTTPClient()`：构造 manager HTTP 客户端，支持 `ONGRID_K8S_ENROLL_TLS_INSECURE` 跳过 TLS
- `defaultK8sRole(edgeMode)`：根据 edge_mode 推断 role
- `k8sCapabilities(role)`：返回角色能力列表（controller→`k8s_inventory`，node→`node_edge`）
- `isK8sController` / `isK8sNode`：角色判断
- `parseDurationEnv` / `parseIntEnv` / `parseBoolEnv` / `envOr`：环境变量解析工具，均带 fallback 默认值

## 5. 依赖关系

- **内部包**（主要）：
  - `internal/pkg/config`：配置加载
  - `internal/pkg/httpserver`：HTTP 服务封装
  - `internal/pkg/logger`：结构化日志
  - `internal/pkg/prom`：Prometheus registry
  - `internal/pkg/tunnel`：到 manager 的隧道客户端
  - `internal/edgeagent/biz`：Agent 主体
  - `internal/edgeagent/collector`：指标采集器（embedded/scrape）
  - `internal/edgeagent/k8s`：K8s inventory/metrics pusher
  - `internal/edgeagent/plugins`：插件 supervisor + 各类插件
  - `internal/edgeagent/host_files` / `restart_service` / `bash` / `webshell`：host capability
  - `internal/edgeagent/service`：tunnel RPC 注册
  - `internal/skill/builtin`：_ import 触发 init() 注册 builtin skill executor
- **外部库**：
  - `github.com/go-chi/chi/v5`：HTTP router
  - `golang.org/x/sync/errgroup`：goroutine 编排
  - `crypto/tls`、`encoding/json`、`errors`、`fmt`、`io`、`log/slog`、`net/http`、`net/url`、`os`、`os/signal`、`strconv`、`strings`、`syscall`、`time`：标准库
- **被调用方**：操作系统启动；systemd 服务

## 6. 并发与资源管理

- **errgroup 编排**：所有长生命周期 goroutine 通过 `errgroup.WithContext(rootCtx)` 管理，任一失败取消 egCtx 触发兄弟 goroutine 退出
- **根 context**：`signal.NotifyContext(SIGINT, SIGTERM)` 捕获信号优雅关闭
- **Agent 退出触发全局关闭**：`agent.Run` goroutine 内 `defer cancel()`——agent 退出（升级 swap 或 ctx 取消）时取消 rootCtx，所有兄弟 goroutine 解除阻塞，systemd 收到 EXIT 后替换 staged bundle
- **无显式锁**：本文件不直接使用 mutex；并发安全由各子包保证（如 supervisor 内部有锁）
- **资源释放**：tunnel 客户端、HTTP 服务的关闭由各自包在 ctx 取消时处理

## 7. 设计模式与亮点

- **子命令分发链**：`runK8sHostCommand` → `runK8sUpgradeCommand` → `--version/--help` → `runK8sDataPlaneMode` → 正常 agent 启动，用顺序 `if` 链 + `(handled, err)` 返回值实现，避免在 main 里堆叠 flag 解析
- **capability 降级**：host capability 注册失败仅 Warn 不退出，确保 edge 至少能跑基础功能（scrape metrics / 读文件）——符合"克制"设计哲学
- **接口适配器模式**：`collectorAdapter` 解决 `collector` 与 `biz` 两个包同形接口的循环依赖问题——这是 Go 中常见的解耦手法
- **角色分支**：controller 与 node 角色启用的功能集不同（controller 跑 inventory/metrics pusher，node 跑 host capability），通过 `isK8sController`/`isK8sNode` 分支控制
- **plugin supervisor 热重载**：`MethodPluginConfigsChanged` handler 调用 `supervisor.TriggerReload`，使 manager 端配置变更能实时下发到 edge
- **环境变量驱动配置**：大量 `ONGRID_K8S_*` / `ONGRID_EDGE_*` 环境变量控制行为，便于 K8s ConfigMap/Secret 注入
- **build tag 平台分支**：依赖 `k8s_host_runtime_linux.go` / `k8s_host_runtime_other.go` 的 build tag 实现 `enterK8sHost` 跨平台

## 8. 注意事项

- **`--version` 必须最先处理**：install.sh 和运维脚本依赖 `ongrid-edge --version` 在任何可能失败的操作之前输出版本——本文件在配置加载前处理，符合要求
- **capability 注册顺序**：host_files → restart_service → bash → webshell，顺序反映从只读到 mutating 的能力递进；任一失败不影响后续
- **controller 不跑 host capability**：controller 角色运行在控制平面节点，不应直接操作主机——通过 `else` 分支禁用并记录日志
- **telemetry config 刷新**：`runK8sTelemetryConfigSync` 默认 1 分钟刷新，失败保留旧配置——避免 manager 短暂不可用导致 controller 失能
- **agent.Run 的 defer cancel**：这是升级流程的关键——agent 升级时 `Run` 返回 nil，必须 cancel rootCtx 才能让 systemd 收到 EXIT 信号替换 staged bundle；注释明确说明 "Without this, eg.Wait() blocks forever"
- **collector mode 默认 off**：新安装默认不推送指标，依赖 hostmetrics/procmetrics 插件 + manager 端 Prom 抓取；legacy `auto` 模式保留向后兼容
- **edgeMetricsAddr 分离**：edge metrics 端口 `:9101` 与 cloud metrics `:9100` 分开，便于同主机调试
- **plugin 健康上报**：`agent.SetPluginHealthFn` 把 supervisor 健康快照转换为 `tunnel.PluginHealthWire`，使 manager 能监控各插件状态——字段映射在 main.go 中完成，避免 tunnel 包依赖 plugins 包
- **升级 stage dir**：默认 `/var/lib/ongrid-edge/.upgrade`，空则禁用 agent_upgrade——dev 环境可设 `ONGRID_EDGE_UPGRADE_STAGE_DIR` 重定位
