# `k8s_credentials.go` 技术实现文档

> 源文件：`cmd/ongrid-edge/k8s_credentials.go`
> 包路径：`github.com/ongridio/ongrid/cmd/ongrid-edge`

## 1. 概述

本文件负责 Kubernetes 部署模式下 edge 进程的凭据管理：把首次 bootstrap 注册得到的 edge AccessKey/SecretKey 持久化到 K8s Secret 或本地文件，后续重启时直接加载，避免重复 bootstrap；同时管理 controller 角色的 telemetry 凭据（traces/logs/remote_write 端点 + 认证信息）的存储与刷新。这是"controller 注册一次、Pod 重启不丢凭据"机制的核心实现。

## 2. 包信息

- **包名**：`main`
- **所属模块**：`cmd/ongrid-edge`（命令入口层，与同包 `main.go` 共享类型如 `k8sEnrollResponse`）
- **依赖方向**：被同包 `main.go` 的 `ensureK8sEnrollment` / `runK8sTelemetryConfigSync` 调用；依赖 `internal/pkg/config`、`internal/pkg/tunnel`

## 3. 关键类型与接口

```go
// 持久化到 Secret / 文件的 edge 凭据快照
type k8sStoredCredential struct {
    ClusterID        uint64
    Role             string
    NodeName         string
    NodeUID          string
    EdgeID           uint64
    AccessKey        string
    SecretKey        string
    CloudAddr        string
    ManagerPublicURL string
    StoredAt         string // RFC3339
}

// K8s API server 的轻量 HTTP 客户端，专门用于操作单个 Secret
type k8sSecretClient struct {
    baseURL    string        // https://<host>:<port>
    namespace  string
    secretName string
    token      string        // serviceaccount bearer token
    client     *http.Client  // 10s 超时，带 CA 校验的 transport
}
```

## 4. 关键函数与流程

### 凭据加载与存储

#### `loadStoredK8sCredential`
- **签名**：`func loadStoredK8sCredential(ctx context.Context, cfg *config.Config, info *tunnel.KubernetesInfo, log *slog.Logger) (bool, error)`
- **职责**：从 Secret 或文件加载已存储的 edge 凭据
- **流程**：
  1. 若 `ONGRID_K8S_CREDENTIAL_FILE` 环境变量设置 → 走文件路径 `loadStoredK8sCredentialFile`
  2. 否则 `newK8sSecretClient` 创建 Secret 客户端；客户端为 nil（环境变量未配置）→ `(false, nil)`
  3. `getDataKey(ctx, k8sCredentialKey(info))` 读取对应 key 的数据
  4. JSON 反序列化为 `k8sStoredCredential`，校验 `ClusterID/Role` 一致性 + AccessKey/SecretKey 非空
  5. 把凭据写入 `cfg.Edge.AccessKey/SecretKey/CloudAddr`
- **错误处理**：Secret 不存在或 key 缺失 → `(false, nil)`（视为未注册，调用方走 bootstrap）；ClusterID/Role 不匹配 → 明确错误

#### `storeK8sCredential`
- **签名**：`func storeK8sCredential(ctx context.Context, info *tunnel.KubernetesInfo, out k8sEnrollResponse, cfg *config.Config) error`
- **职责**：注册成功后持久化凭据
- **流程**：
  1. 文件模式 → `storeK8sCredentialFile`
  2. Secret 模式 → 构造 `k8sStoredCredential`（含 `StoredAt` 时间戳），JSON 序列化
  3. 若 role=controller 且响应含 `Telemetry`，额外把 telemetry 凭据写入独立的 telemetry Secret（由 `ONGRID_K8S_TELEMETRY_SECRET` 指定，默认复用 credential secret）
  4. `patchDataKey` 把 edge 凭据 JSON 写入 credential Secret 的 `k8sCredentialKey(info)` key
- **错误处理**：telemetry 凭据的 ClusterID/AccessKey/SecretKey 校验失败 → 立即返回错误，不写入

#### `loadStoredK8sCredentialFile` / `storeK8sCredentialFile`
- 文件模式实现，使用**原子写**（临时文件 + fsync + rename，权限 0600，目录 0700）保证凭据文件不会半写
- 文件不存在 → `(false, nil)`；JSON 损坏 → 错误
- 校验比 Secret 模式更严格：`NodeName/NodeUID` 也必须匹配

### Telemetry 凭据管理

#### `storeK8sTelemetryConfig`
- **签名**：`func storeK8sTelemetryConfig(ctx context.Context, info *tunnel.KubernetesInfo, telemetry k8sTelemetryConfig) error`
- **职责**：把 manager 返回的 telemetry 配置写入 telemetry Secret
- **流程**：
  1. 校验 `info.Role == "controller"` 且 ClusterID 一致
  2. `telemetrySecretData` 把 `k8sTelemetryConfig` 展平为 18 个 `[]byte` key-value（每字段独立 key，便于 Pod 通过 `secretKeyRef` 单独挂载）
  3. 读当前 Secret，若已包含相同值 → 跳过（幂等，避免无谓 patch）
  4. 否则 `patchDataKeys` 一次性写入所有 key

#### `loadK8sTelemetryCredential`
- **签名**：`func loadK8sTelemetryCredential(ctx context.Context, info *tunnel.KubernetesInfo) (string, string, bool, error)`
- **职责**：从 telemetry Secret 读取 access-key + secret-key（用于刷新 telemetry 配置时认证）
- **返回**：`(accessKey, secretKey, found, err)`；`found=false` 表示尚未首次写入

### K8s Secret 客户端

#### `newK8sSecretClient`
- **签名**：`func newK8sSecretClient(info *tunnel.KubernetesInfo) (*k8sSecretClient, error)`
- **职责**：基于 in-cluster serviceaccount 构造 K8s API 客户端
- **流程**：
  1. 读 `ONGRID_K8S_CREDENTIAL_SECRET` 环境变量；空 → 返回 `(nil, nil)`（禁用 Secret 模式）
  2. 读 `KUBERNETES_SERVICE_HOST` / `KUBERNETES_SERVICE_PORT_HTTPS` 环境变量；空 → `(nil, nil)`（非集群内运行）
  3. namespace 优先取 `info.Namespace`，否则读 serviceaccount 目录下的 `namespace` 文件
  4. 读 `token` 文件作为 bearer
  5. 读 `ca.crt` 构造 `tls.Config{RootCAs: pool}`，MinVersion 强制 TLS 1.2+
  6. 构造 `http.Client{Timeout: 10s, Transport}`
- **错误处理**：任一必需文件缺失 → 明确错误；CA 文件读取失败不致命（跳过 RootCAs 配置，但 transport 仍生效）

#### `getData` / `getDataKey` / `patchDataKey` / `patchDataKeys`
- 标准 K8s API：`GET /api/v1/namespaces/{ns}/secrets/{name}` 与 `PATCH`（`application/merge-patch+json`）
- `getData` 把 Secret 的 base64 编码 `data` 字段解码为 `map[string][]byte`
- 404 → `(nil, false, nil)`（Secret 不存在）；其他非 2xx → 错误（读 body 前 2048 字节）

### 辅助函数

- `k8sCredentialKey(info)`：根据 role 生成 Secret 内的 key——controller 用 `controller`；node 用 `node-` + sha256(NodeUID/NodeName)[:18] 的 base64url，避免不同节点冲突
- `telemetrySecretData(telemetry)`：把 `k8sTelemetryConfig` 展平为 18 个独立 key 的 map
- `dataContainsValues(current, desired)`：逐 key 比对，用于幂等写入判断
- `applyManagerTelemetryTLS(in, managerURL)`：当 `ONGRID_K8S_ENROLL_TLS_INSECURE=true` 且 telemetry endpoint 与 manager 同源时，把对应信号的 `TLSInsecure` 置 true——仅对 manager 自身签发的 endpoint 生效，外部后端保留各自 TLS 策略

## 5. 依赖关系

- **内部包**：
  - `github.com/ongridio/ongrid/internal/pkg/config`：读取 `cfg.Edge.AccessKey/SecretKey/CloudAddr` 与 `cfg.PublicURL`
  - `github.com/ongridio/ongrid/internal/pkg/tunnel`：`tunnel.KubernetesInfo` 类型
- **外部库**：
  - `crypto/sha256`、`crypto/tls`、`crypto/x509`：哈希与 TLS 证书校验
  - `encoding/base64`、`encoding/json`：Secret data 编解码
  - `net/http`、`net/url`：K8s API 调用
  - `bytes`、`io`、`os`、`path`、`path/filepath`、`strconv`、`strings`、`time`：标准辅助
- **被调用方**：
  - `main.go`：`ensureK8sEnrollment`（注册流程）、`refreshAndStoreK8sTelemetryConfig`（telemetry 刷新）、`loadK8sTelemetryCredential`

## 6. 并发与资源管理

- **无显式锁**：本文件函数均为同步调用，由调用方（`main.go`）控制并发——telemetry 刷新 goroutine 与主流程不会同时调用同一 Secret 客户端
- **HTTP 客户端复用**：`k8sSecretClient.client` 在客户端生命周期内复用，连接池由 `http.Transport` 管理
- **文件句柄**：`storeK8sCredentialFile` 用 `defer` 清理临时文件；`loadStoredK8sCredentialFile` 由 `os.ReadFile` 自动管理
- **Context**：所有 K8s API 调用与文件 IO 都接受 `context.Context`，支持超时与取消

## 7. 设计模式与亮点

- **策略模式（存储后端）**：通过 `ONGRID_K8S_CREDENTIAL_FILE` 环境变量在"K8s Secret"与"本地文件"两种存储后端间切换，对外接口相同——适合集群内（Secret）与集群外/测试（文件）两种部署形态
- **原子写模式**：`storeK8sCredentialFile` 采用临时文件 + fsync + rename，与 `k8s_host_runtime.go` 的 `copyFileAtomic` 一致
- **幂等写入**：`storeK8sTelemetryConfig` 在写入前比对当前值，避免无谓 patch——减少 K8s API 调用与 Secret 版本号抖动
- **最小化 K8s 客户端**：没有引入 `client-go`（重量级依赖），而是用标准 `net/http` 直接调用 K8s REST API——对于只操作单个 Secret 的场景足够轻量
- **Key 命名设计**：`k8sCredentialKey` 对 node 角色用 sha256 哈希避免 NodeUID 直接暴露在 Secret key 名中；controller 用固定 key 简化查询

## 8. 注意事项

- **凭据敏感**：edge AccessKey/SecretKey 与 telemetry 凭据均明文存入 K8s Secret——K8s Secret 默认 base64 编码而非加密，生产环境应启用 etcd 加密或对接外部 KMS
- **文件模式权限**：凭据文件 0600、目录 0700，仅 edge uid 可读；但仍建议优先使用 K8s Secret（RBAC 可控）
- **serviceaccount 依赖**：Secret 模式要求 edge Pod 的 serviceaccount 对目标 Secret 有 `get/patch` 权限，部署时需配 `Role/RoleBinding`
- **`applyManagerTelemetryTLS` 的安全含义**：仅在显式设置 `ONGRID_K8S_ENROLL_TLS_INSECURE=true` 时生效，且只对 manager 同源 endpoint 生效——避免对真实外部后端（Loki/Tempo/Prometheus）意外关闭 TLS 校验
- **`StoredAt` 字段**：仅用于诊断，不参与校验；可作为凭据是否过期的参考
- **扩展存储字段**：若未来需要新增凭据字段（如 edge 证书），应同时更新 `k8sStoredCredential`、`telemetrySecretData`、`loadStoredK8sCredentialFile` 的校验逻辑，三者需保持一致
- **并发刷新**：`runK8sTelemetryConfigSync` 周期性调用 `refreshAndStoreK8sTelemetryConfig`，若与注册流程并发可能产生 Secret patch 冲突——当前设计是注册完成后才启动刷新 goroutine，但未来若调整时序需注意 K8s 的乐观锁（resourceVersion）
