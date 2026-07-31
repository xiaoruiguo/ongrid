# `k8s_data_plane.go` 技术实现文档

> 源文件：`cmd/ongrid-edge/k8s_data_plane.go`
> 包路径：`github.com/ongridio/ongrid/cmd/ongrid-edge`

## 1. 概述

本文件实现 `ongrid-edge` 在 Kubernetes 数据平面（Data Plane）模式下的两种运行模式：`k8s-telemetry-gateway`（OpenTelemetry Collector 网关，接收集群内 traces/logs/metrics 并转发到外部后端）与 `k8s-metrics-scraper`（抓取集群内 Prometheus 指标并 remote_write 到外部）。这两种模式不建立到 manager 的隧道连接，专注于纯数据转发，由 `ONGRID_EDGE_MODE` 环境变量触发。

## 2. 包信息

- **包名**：`main`
- **所属模块**：`cmd/ongrid-edge`（命令入口层，与同包 `main.go`、`k8s_credentials.go` 共享类型与工具函数）
- **依赖方向**：被同包 `main.go` 的 `main()` 调用；依赖 `internal/edgeagent/k8s`、`internal/edgeagent/plugins`、`internal/pkg/*`

## 3. 关键类型与接口

```go
// telemetry 网关模式下的 plugin 配置 fetcher，从挂载的 Secret 文件读取配置
type k8sTelemetryGatewayFetcher struct {
    dir string // telemetry Secret 挂载目录
}

// 从 Secret 文件读取出的全部 telemetry 配置
type telemetryFiles struct {
    clusterID              uint64
    accessKey, secretKey   string
    tracesEndpoint         string
    tracesAuthUser, tracesAuthPass string
    tracesTLSInsecure      bool
    logsEndpoint           string
    logsAuthUser, logsAuthPass    string
    logsTLSInsecure        bool
    remoteWriteEndpoint    string
    remoteWriteBearer      string
    remoteWriteBasicUser   string
    remoteWriteBasicPass   string
    remoteWriteTLSInsecure bool
    remoteWriteCAPath      string
    remoteWriteCAHash      [32]byte
}

// 单个 telemetry 信号（traces/logs）的认证信息
type telemetrySignalAuth struct {
    user, password string
    tlsInsecure    bool
}

// remote_write 客户端封装，支持基于文件变更的热重载
type telemetryRemoteWriteWriter struct {
    dir     string
    timeout time.Duration
    log     *slog.Logger
    mu          sync.Mutex
    configured  bool
    fingerprint [32]byte          // 配置指纹，用于检测变更
    writer      *pkgpromwrite.Client
    httpClient  *http.Client
}
```

## 4. 关键函数与流程

### 模式分发

#### `runK8sDataPlaneMode`
- **签名**：`func runK8sDataPlaneMode(ctx context.Context, mode string, log *slog.Logger) (bool, error)`
- **职责**：根据 `ONGRID_EDGE_MODE` 分发到对应数据平面模式
- **流程**：
  - `k8s-telemetry-gateway` → `runK8sTelemetryGateway`
  - `k8s-metrics-scraper` → `runK8sMetricsScraper`
  - 其他 → `(false, nil)` 表示非数据平面模式，由 `main()` 走正常 agent 启动

### Telemetry Gateway 模式

#### `runK8sTelemetryGateway`
- **签名**：`func runK8sTelemetryGateway(ctx context.Context, log *slog.Logger) error`
- **职责**：启动 OpenTelemetry Collector 子进程作为集群内 telemetry 网关
- **流程**：
  1. 构造 `edgeplugintraces` 插件（otelcol-contrib 子进程）
  2. 构造 `edgeplugins.Supervisor`，Fetcher 用 `k8sTelemetryGatewayFetcher{dir: telemetrySecretDir()}`，ReloadInterval 默认 10s
  3. 注册 traces 插件到 supervisor
  4. 启动两个 goroutine（errgroup）：
     - `supervisor.Run`：管理 otelcol 子进程生命周期
     - `runDataPlaneDiagnostics`：暴露 `/metrics` `/healthz` `/readyz`，readiness 检查 supervisor 健康快照 + collector 13133 端口健康
  5. `group.Wait()` 阻塞直到任一 goroutine 退出
- **错误处理**：errgroup 任一失败则整体退出；`main()` 据此 `os.Exit(1)`

#### `k8sTelemetryGatewayFetcher.Fetch`
- **签名**：`func (f *k8sTelemetryGatewayFetcher) Fetch(ctx context.Context) (map[string]edgeplugins.PluginConfig, error)`
- **职责**：从挂载的 telemetry Secret 文件构造 otelcol 插件配置
- **流程**：
  1. `readTelemetryFiles` 读取全部 Secret 文件
  2. 校验三个 endpoint（traces/logs/remote_write）非空，否则返回 "endpoints not ready" 错误（触发 supervisor 等待重试）
  3. 构造 spec map：包含 grpc/http 监听地址（4317/4318）、k8sattributes、logs/metrics 转发配置、内存限制、批量大小、队列大小、额外属性（cluster_id 等）
  4. 若有 CA 文件，加入 `metrics_remote_write_ca_file` + checksum
  5. 返回 `map[pluginName]PluginConfig`，Enabled=true，含完整 spec

#### `collectorHealthReady`
- **签名**：`func collectorHealthReady(ctx context.Context, client *http.Client) bool`
- **职责**：探活 otelcol 健康检查端点 `http://127.0.0.1:13133/`

### Metrics Scraper 模式

#### `runK8sMetricsScraper`
- **签名**：`func runK8sMetricsScraper(ctx context.Context, log *slog.Logger) error`
- **职责**：启动集群内 Prometheus 指标抓取 + remote_write 转发
- **流程**：
  1. `waitForRemoteWriteFiles` 轮询等待 telemetry Secret 文件就绪（2s 间隔）
  2. 构造 `telemetryRemoteWriteWriter`，预构建客户端验证配置可用
  3. 构造 `edgek8s.RemoteWriteScraper`，配置参数全部来自环境变量（`ONGRID_K8S_METRICS_*`）
  4. 启动两个 goroutine：`scraper.Run` + `runDataPlaneDiagnostics`（readiness 用 `scraper.Ready`）
- **环境变量**：`ONGRID_K8S_METRICS_ENDPOINT`（抓取目标）、`ONGRID_K8S_METRICS_INTERVAL`（默认 30s）、`SAMPLE_LIMIT`（25万）、`BATCH_SAMPLE_LIMIT`（1万）、`BATCH_BYTE_LIMIT`（4MB）、`MAX_RETRIES`（3）、`RETRY_BACKOFF`（500ms）

#### `telemetryRemoteWriteWriter.Write` / `clientFor`
- **职责**：写入指标到 remote_write 端点，支持配置热重载
- **流程**：
  1. `Write`：每次先 `readRemoteWriteFiles` 读最新配置 → `clientFor` 获取客户端 → `writer.Write`
  2. `clientFor`：计算配置指纹（sha256 拼接 endpoint+auth+TLS+CAHash），加锁比对；未变 → 复用；已变 → 重建客户端 + 关闭旧客户端 idle 连接
- **并发控制**：`sync.Mutex` 保护 writer/fingerprint/httpClient 三字段

### 配置文件读取

#### `readTelemetryFiles` / `readRemoteWriteFiles`
- **职责**：从 `dir` 目录读取 telemetry Secret 投影的文件
- **区别**：`readTelemetryFiles` 读取全部字段（traces+logs+remote_write），用于 gateway 模式；`readRemoteWriteFiles` 只读取 remote_write 相关字段，用于 scraper 模式（scraper 不需要 traces/logs 配置）
- **校验**：`validateTelemetryEndpoint` 强制 endpoint 必须是绝对 HTTP(S) URL；basic auth 要求 user/pass 同时存在；CA 文件不存在或空 → 清空路径，存在 → 计算 sha256 hash

#### `readTelemetrySignalAuth`
- **签名**：`func readTelemetrySignalAuth(ctx context.Context, dir, signal, accessKey, secretKey string) (telemetrySignalAuth, error)`
- **职责**：读取单个信号（traces/logs）的认证配置
- **逻辑**：
  - `auth-mode` 缺失 → 视为 `telemetry`（旧版兼容，用共享 telemetry 凭据）
  - `telemetry` 模式 → user=accessKey, password=secretKey
  - `backend` 模式 → 用独立的 basic-user/basic-pass，要求同时存在
  - TLS 设置优先读文件，否则回退到 `ONGRID_K8S_ENROLL_TLS_INSECURE` 环境变量

#### `readTelemetryFile`
- **签名**：`func readTelemetryFile(ctx context.Context, dir, name string, required bool) (string, error)`
- **职责**：读取单个 Secret 文件，`required=false` 时文件不存在返回空串而非错误

#### `waitForRemoteWriteFiles`
- **签名**：`func waitForRemoteWriteFiles(ctx context.Context, dir string) (telemetryFiles, error)`
- **职责**：轮询等待 remote_write 配置文件就绪（Pod 启动时 Secret 可能尚未挂载）
- **流程**：2s ticker 循环 `readRemoteWriteFiles`；成功立即返回；ctx 取消则返回 `ctx.Err()`

### 诊断端点

#### `runDataPlaneDiagnostics`
- **签名**：`func runDataPlaneDiagnostics(ctx context.Context, registry *prometheus.Registry, ready func() bool, log *slog.Logger) error`
- **职责**：启动 HTTP 服务暴露 `/metrics` `/healthz` `/readyz`
- **流程**：chi router 注册三个端点；`readyz` 调用传入的 `ready` 函数，未就绪返回 503；监听地址由 `ONGRID_EDGE_METRICS_ADDR` 控制，默认 `:9101`
- **错误处理**：`httpserver.Start(ctx)` 在 ctx 取消时优雅关闭

### 辅助函数

- `telemetrySecretDir()`：返回 telemetry Secret 挂载目录，默认 `/var/run/ongrid-telemetry`，可由 `ONGRID_K8S_TELEMETRY_SECRET_DIR` 覆盖
- `remoteWriteFingerprint(files)`：把 remote_write 配置关键字段拼接后 sha256，用于热重载检测
- `validateTelemetryEndpoint(name, raw)`：校验 endpoint 是绝对 HTTP(S) URL

## 5. 依赖关系

- **内部包**：
  - `github.com/ongridio/ongrid/internal/edgeagent/k8s`（`edgek8s`）：`NewRemoteWriteScraper`、`RemoteWriteScraperConfig`
  - `github.com/ongridio/ongrid/internal/edgeagent/plugins`（`edgeplugins`）：`Supervisor`、`PluginConfig`、`NewSupervisor`
  - `github.com/ongridio/ongrid/internal/edgeagent/plugins/traces`（`edgeplugintraces`）：otelcol 插件
  - `github.com/ongridio/ongrid/internal/pkg/httpserver`：HTTP 服务封装
  - `github.com/ongridio/ongrid/internal/pkg/prom`：Prometheus registry
  - `github.com/ongridio/ongrid/internal/pkg/promauth`：remote_write 认证 + TLS 客户端构造
  - `github.com/ongridio/ongrid/internal/pkg/promwrite`：remote_write 客户端
- **外部库**：
  - `github.com/go-chi/chi/v5`：HTTP router
  - `github.com/prometheus/client_golang/prometheus`：registry
  - `golang.org/x/sync/errgroup`：goroutine 编排
  - `crypto/sha256`、`net/http`、`net/url`、`os`、`path/filepath`、`strconv`、`strings`、`sync`、`time`：标准库
- **被调用方**：`main.go` 的 `main()`（在 K8s 注册流程之前）

## 6. 并发与资源管理

- **errgroup 编排**：两种模式都用 `errgroup.WithContext(ctx)` 启动主 goroutine + 诊断 goroutine，任一失败则整体退出
- **`telemetryRemoteWriteWriter` 互斥锁**：`sync.Mutex` 保护 writer/fingerprint/httpClient 三字段，`clientFor` 在锁内重建客户端并关闭旧客户端 idle 连接——避免连接泄漏
- **HTTP 客户端生命周期**：`clientFor` 在配置变更时 `oldHTTPClient.CloseIdleConnections()`，防止旧 endpoint 的连接堆积
- **Context 传播**：所有 IO 操作接受 ctx，支持优雅关闭
- **无全局可变状态**：所有状态封装在结构体内，通过参数传递

## 7. 设计模式与亮点

- **策略模式（运行模式）**：`runK8sDataPlaneMode` 根据 `mode` 字符串选择执行策略，与正常 agent 模式互斥——单一二进制多种用途
- **配置热重载**：`telemetryRemoteWriteWriter` 通过指纹比对检测 Secret 文件变更，无需重启即可切换 remote_write 目标——K8s Secret 投影文件更新会自动反映
- **就绪探针分离**：`runDataPlaneDiagnostics` 的 `ready` 回调由各模式提供，gateway 模式检查 supervisor + collector 健康，scraper 模式检查 scraper 状态——精确反映数据平面就绪状态
- **文件轮询等待**：`waitForRemoteWriteFiles` 处理 Pod 启动时 Secret 尚未挂载的竞态，2s 间隔平衡响应速度与 API 压力
- **信号级认证**：`readTelemetrySignalAuth` 区分 `telemetry`（共享凭据）与 `backend`（独立凭据）两种认证模式，向后兼容旧版 Secret
- **最小依赖**：scraper 模式不依赖 traces/logs 配置文件，Secret 投影可只包含 remote_write 字段

## 8. 注意事项

- **与正常 agent 模式互斥**：数据平面模式不建立到 manager 的隧道，因此不接收 RPC 调用、不推送 inventory、不执行 host 命令——仅做数据转发
- **Secret 文件投影**：依赖 K8s Secret 通过 volume 投影到 `telemetrySecretDir()`，部署时需在 Pod spec 中配置 `volumes` + `volumeMounts`
- **`readTelemetrySignalAuth` 的 legacy 兼容**：`auth-mode` 缺失时默认用共享 telemetry 凭据，这是为旧版 Secret 设计的；新部署建议显式设置 `auth-mode=backend` 使用独立凭据
- **CA 文件 hash**：`remoteWriteCAHash` 用于指纹计算，确保 CA 内容变更也能触发客户端重建
- **`collectorHealthReady` 超时**：500ms 超时，避免 readiness 探针因 collector 卡住而超时
- **资源限制**：gateway 模式的 otelcol 内存限制默认 768MB + 128MB spike，可通过 `ONGRID_K8S_GATEWAY_MEMORY_LIMIT_MIB` 调整；scraper 模式的批量大小限制防止 OOM
- **扩展新模式**：新增数据平面模式应在 `runK8sDataPlaneMode` 的 switch 中添加分支，并复用 `runDataPlaneDiagnostics` 暴露诊断端点
- **与 `k8s_credentials.go` 的关系**：数据平面模式不调用 `ensureK8sEnrollment`，但 `telemetryRemoteWriteWriter` 读取的 Secret 可能由 controller 注册时通过 `storeK8sTelemetryConfig` 写入——两者共享 Secret key 命名约定
