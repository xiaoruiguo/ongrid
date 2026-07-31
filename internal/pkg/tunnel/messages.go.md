# `messages.go` 技术实现文档

> 源文件：`internal/pkg/tunnel/messages.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/tunnel`

## 1. 概述

本文件手动镜像 `api/tunnel/v1/tunnel.proto` 的 message 形状为 Go 结构（带 JSON tag）。当前 MVP wire 格式是 JSON，刻意避免生成 protobuf Go 类型以保持 `internal/pkg/tunnel` 无依赖（无 protobuf import、无 generated-code 目录）。所有 RPC method 名作为常量集中定义，覆盖 register_edge / heartbeat / metrics push / k8s / WebSSH / agent upgrade / plugin config 等十余类协议。

## 2. 包信息

- **包名**：`tunnel`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被 manager 侧 frontierbound 与 edge 侧 tunnel client 共享；仅依赖标准库 `encoding/json`

## 3. 关键类型与接口

### Method 名常量（节选）
```go
const (
    MethodRegisterEdge              = "register_edge"
    MethodHeartbeat                 = "heartbeat"
    MethodPushHostMetrics           = "push_host_metrics"
    MethodPushPromSamples           = "push_prom_samples"
    MethodPushK8sInventory          = "push_k8s_inventory"
    MethodDescribeK8sResource       = "describe_k8s_resource"
    MethodQueryK8sLogs              = "query_k8s_logs"
    MethodExecuteK8sAction          = "execute_k8s_action"
    MethodGetHostLoad               = "get_host_load"
    MethodGetProcessList            = "get_process_list"
    MethodGetNetstat                = "get_netstat"
    MethodExecuteSkill              = "execute_skill"
    MethodGetPluginConfigs          = "get_plugin_configs"
    MethodPluginConfigsChanged      = "plugin_configs_changed"
    MethodWriteDatabaseMetricsSecret = "write_database_metrics_secret"
    MethodShellOpen/Input/Resize/Close/Output/Exit  // WebSSH
    MethodAgentUpgrade              = "agent_upgrade"
    MethodFetchPackage              = "fetch_package"
    MethodApplyPackage              = "apply_package"
)
```

### 关键 wire 结构
```go
type HostInfo struct {
    Hostname, OS, Arch, KernelVersion string
    CPUCount int
    MemTotalBytes uint64
    Fingerprint        string // /etc/machine-id 等
    HardwareFingerprint string // 物理 NIC MAC + CPU + disk serial（克隆抗性）
    IPAddress          string
}

type KubernetesInfo struct { Mode, ClusterID, ClusterUID, ClusterName, Role, NodeName, NodeUID, Namespace, PodName string }

type RegisterEdgeRequest struct {
    AccessKey, SecretKey string
    HostInfo HostInfo
    AgentVersion string
    Kubernetes *KubernetesInfo
}

type HeartbeatRequest struct {
    EdgeID uint64
    Ts int64
    StatusFlags map[string]string
    Plugins []PluginHealthWire
}

type PromSample struct {
    Name string
    Labels map[string]string
    Value float64
    TsMs int64
}

type KubernetesInventoryRequest struct {
    ClusterID uint64
    Mode, Role, Scope, Namespace string
    Ts int64
    ResourceVersion string
    Nodes, Workloads, Pods, Events []...
    DeletedNodes, DeletedWorkloads, DeletedPods, DeletedEvents []...
    SnapshotID, ChunkIndex, ChunkCount int  // 分块快照
}

type AgentUpgradeRequest struct {
    URL, SHA256 string
}

type Meta struct {
    AccessKey, SecretKey string
}
```

## 4. 关键函数与流程

无函数定义（纯 wire 协议常量与结构）。

文档要点：
- **Wire 格式**：JSON；字段名匹配 `tunnel.proto` 的 `json_name` 注解，必须保持同步
- **协议方向**：
  - edge→cloud：`register_edge`、`heartbeat`、`push_host_metrics`、`push_prom_samples`、`push_k8s_inventory`、`get_plugin_configs`
  - cloud→edge：`describe_k8s_resource`、`query_k8s_logs`、`execute_k8s_action`、`get_host_load`、`get_process_list`、`get_netstat`、`execute_skill`、`plugin_configs_changed`、`write_database_metrics_secret`、WebSSH（shell_*）、`agent_upgrade`、`fetch_package`、`apply_package`
- **分块快照**：`KubernetesInventoryRequest` 含 `SnapshotID` / `ChunkIndex` / `ChunkCount`，支持大集群分批推送
- **PluginHealthWire**：edge 把 `edgeagent/plugins.PluginHealth` 映射为 wire 形态，保持 plugin runtime 与 tunnel 协议解耦；`State` 为 stopped/starting/running/crashed；`LastError` 把静默失败变成 operator-visible 原因
- **WebSSH 双向**：shell_open/input/resize/close 是 cloud→edge；shell_output/exit 是 edge→cloud；stderr 与 stdout PTY 合并
- **`Meta` 是握手 blob**：序列化进 geminio Meta bytes，server 在 AuthFunc 前解码；不是 RPC body

## 5. 依赖关系

- **内部包**：无
- **外部库**：无（仅 `encoding/json`）
- **被调用方**：manager 侧 frontierbound handlers、edge 侧 tunnel client 与各 handler

## 6. 并发与资源管理

无并发控制（纯类型定义）。Wire 结构是值类型，可安全并发传递。

## 7. 设计模式与亮点

- **手镜像 proto 而非生成**：注释明示"deliberately avoid generating protobuf Go types ... so internal/pkg/tunnel/ stays dependency-free"。Phase 2 切 protobuf binary 时本文件是替换 seam
- **method 名常量化**：所有 RPC method 名集中定义，调用方 spell-safe
- **`HardwareFingerprint` 克隆抗性**：注释解释物理 NIC MAC + CPU model + disk serials 不被克隆 Linux VM 折叠（issue #96），cloud 优先用它，回退到 `Fingerprint`
- **`PluginHealthWire` 解耦**：edge 在 plugin runtime 与 wire 间映射，协议变化不污染 runtime
- **`KubernetesInventoryRequest` 分块**：大集群快照分批推送，`SnapshotID` 关联同一次快照
- **`LastError` 字段价值**：注释明示"turns a silent failure into an operator-visible reason"
- **`DiskUsedPct` 加入 `GetHostLoadResponse`**：注释提到修复 LLM 误读"disk usage = mem_pct"的真实 session 案例
- **`Meta` 与 RPC body 分离**：Meta 是握手 blob 非 RPC body

## 8. 注意事项

- **字段同步**：注释要求"Field names MUST stay in sync with tunnel.proto's json_name annotations"，proto 改动需同步本文件
- **JSON 弱类型**：`map[string]any` / `json.RawMessage` 用得多，编译期不检查类型；proto 切换后需重新评估
- **`SSHPass` 敏感**：`ShellOpenRequest.SSHPass` 注释明示"one-shot and wiped from edge memory after Dial; never logged, never stored"，但 wire 仍明文传输，必须配 TLS
- **`WriteDatabaseMetricsSecretRequest.Content` 敏感**：注释明示"do not log it and do not persist it on the manager side"
- **分块快照顺序**：`ChunkIndex` / `ChunkCount` 需 caller 处理乱序；edge 不保证按序到达
- **`KubernetesActionRequest` 是写操作**：注释明示"Class=write so ReviewGate must approve before this request is dispatched"
- **`AgentUpgradeRequest.URL` 无认证**：注释提到"revisit if we ever expose a CDN"，当前假设 artifact 在可信 nginx 后
- **大 InventoryRequest body**：分块缓解但仍可能大；JSON 序列化/反序列化开销需评估
