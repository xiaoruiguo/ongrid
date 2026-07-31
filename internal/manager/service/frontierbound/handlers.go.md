# `handlers.go` 技术实现文档

> 源文件：`internal/manager/service/frontierbound/handlers.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/service/frontierbound`

## 1. 概述

本文件实现 manager 侧所有反向调用 handler 的注册（`Install`）以及边缘 → 云的数据流处理。核心红线：(1) `resolveDeviceID` 永不回退到 edge_id —— edge/device 实体拆分后二者是独立 auto-increment 序列，回退会把错误的 device_id 标签写入不可变 Prometheus TSDB（issue #96）；(2) `canonicalizeEdgeID==0` 时静默 drop 而非让 raw transport ID 泄露为 edge_id label（v0.7.39 fix）；(3) edge_id mismatch（body 中 in.EdgeID 与 canonical 不符）直接拒绝；(4) Prom disabled 时 push_prom_samples 静默 accept=n 而非报错（edge 不应感知 cloud Prom 状态）；(5) heartbeat 携带的 plugin health 永不 fail heartbeat（best-effort in-memory）。

## 2. 包信息

- **包名**：`frontierbound`（与 client.go 同包）
- **所属模块**：`internal/manager/service/frontierbound`
- **依赖方向**：被 `cmd/ongrid/main.go` 调用 Install；依赖 `biz/edge`、`biz/metric`、`internal/pkg/tunnel`、`log/slog`、`net`、`time`

## 3. 关键类型与接口

```go
type PromwriteIngester interface {
    Push(ctx, deviceID uint64, source string, samples []tunnel.PromSample) error
    PushKubernetes(ctx, clusterID uint64, source string, samples []tunnel.PromSample) error
}

type DeviceResolver interface {
    LookupHostDevice(ctx, edgeID uint64) (uint64, error)
}

type KubernetesRegistry interface {
    HandleRegister(ctx, edgeID uint64, deviceID *uint64, info tunnel.KubernetesInfo) error
    HandleControllerHeartbeat(ctx, edgeID uint64) error
    LookupControllerCluster(ctx, edgeID uint64) (uint64, error)
}

type KubernetesInventoryIngester interface {
    IngestInventory(ctx, edgeID uint64, in tunnel.KubernetesInventoryRequest) (acceptedNodes, acceptedWorkloads, acceptedPods, acceptedEvents int, err error)
}

type EdgeAuthenticator interface {
    Authenticate(ctx, accessKey, secretKey string) (tunnel.Session, error)
}

type WebshellRouter interface {
    DispatchOutput(sid string, data []byte) error
    DispatchExit(sid string, exitCode int, errMsg string)
}

type PluginConfigFetcher interface {
    FetchForEdge(ctx, edgeID uint64) (*edgebiz.WireSnapshot, error)
}

type Wiring struct {
    EdgeAuthn      EdgeAuthenticator
    EdgeUC         *edgebiz.Usecase
    MetricIngester metricbiz.IngestService
    PromIngester   PromwriteIngester            // 可 nil（Prom disabled）
    PluginConfigUC PluginConfigFetcher          // 可 nil
    WebshellRouter WebshellRouter               // 可 nil
    DeviceResolver DeviceResolver               // 可 nil（回退 edge_id==device_id 假设，仅 pre-launch 正确）
    K8sRegistry    KubernetesRegistry
    K8sInventory   KubernetesInventoryIngester
    Log            *slog.Logger
}
```

## 4. 关键函数与流程

### `Install(ctx, c *Client, w Wiring) error`

- **职责**：注册所有 reverse-call handler + 三个 lifecycle 回调；disabled client（svc==nil）直接返回 nil 让 main.go 继续。
- **流程**：
  1. log nil → `slog.Default()`；svc==nil → Info "Install skipped" + return nil。
  2. EdgeAuthn / EdgeUC / MetricIngester 必填，缺一报错。
  3. 定义 `authenticateEdge(authCtx, accessKey, secretKey) (uint64, error)` —— 调 `w.EdgeAuthn.Authenticate`；注释指出 AccessKeyAuthenticator 已把所有失败路径塌缩为 `errs.ErrUnauthorized` 防枚举。
  4. 定义 `resolveEdgeID(meta []byte) (uint64, error)` —— unmarshal `tunnel.Meta`；调 authenticateEdge。
  5. **RegisterGetEdgeID**：resolveEdgeID；失败返回 0+err 让 frontier 拒绝 dial（manager 永不分配匿名 ID）。
  6. **RegisterEdgeOnline**：resolveEdgeID(meta) → `c.bindEdgeTransportAt(edgeID, canonicalEdgeID, safeAddr(addr))`；Info 日志；注释提及 real-time edge_offline alerting 已移除，改由 PipelineEvaluator 的 metric_raw 规则在下个 tick 触发。
  7. **RegisterEdgeOffline**：`canonicalizeEdgeID(edgeID)` → `unbindEdgeTransport(edgeID, canonicalEdgeID, addr)`；若返回 false（stale）Debug 日志；否则 Info + 调 `EdgeUC.HandleOffline(canonicalEdgeID, now)`。
  8. **register_edge**：unmarshal `RegisterEdgeRequest`；canonicalEdgeID 来自 frontier 认证（req.ClientID()）；若是 K8s controller role → ClearHostDeviceLink + K8sRegistry.HandleRegister + HandleHeartbeat + setKubernetesController(true)；否则 HandleRegister(HostInfo, AgentVersion) + 若有 Kubernetes info 则 K8sRegistry.HandleRegister(deviceID) + setKubernetesController(false)。返回 `RegisterEdgeResponse{EdgeID, ServerTime}`。
  9. **heartbeat**：unmarshal；canonicalizeEdgeID==0 → "edge binding not ready; re-register required"；in.EdgeID mismatch → 拒绝；ts==0 用 now；HandleHeartbeat；refreshKubernetesControllerHeartbeat（失败仅 Warn）；plugin health（in.Plugins）→ RecordPluginHealth（best-effort，不 fail heartbeat）。
  10. **push_k8s_inventory**：unmarshal；canonicalizeEdgeID==0 → 空 response；K8sInventory nil → 回显 Accepted=length；否则 IngestInventory。
  11. **push_host_metrics**：unmarshal；mismatch → 拒绝；canonical==0 → silent drop + Accepted=0；`resolveDeviceID`==0 → Warn + Accepted=0（不写错误 device_id label）；否则 MetricIngester.Push(deviceID, points)；返回 Accepted=len。
  12. **push_prom_samples**：unmarshal；mismatch → 拒绝；canonical==0 → silent drop + Accepted=n；PromIngester nil → Debug "prom disabled" + Accepted=n；若 `isKubernetesPromSource(source)`（前缀 `k8s:`）→ lookupK8sControllerCluster + PushKubernetes；否则 resolveDeviceID → 0 时回退查 clusterID → 0 时 Warn + Accepted=n；否则 Push(deviceID, source, samples)。
  13. **get_plugin_configs**（仅 PluginConfigUC 非 nil 注册）：FetchForEdge → 翻译为 tunnel.GetPluginConfigsResponse（labelDeviceID 经 DeviceResolver 解析）。
  14. **shell_output / shell_exit**（仅 WebshellRouter 非 nil 注册）：DispatchOutput / DispatchExit。
  15. 末尾 Info "handlers installed"。

### 辅助函数

- **`isKubernetesControllerRole(role)`**：role=="controller" → true。
- **`safeAddr(a net.Addr)`**：nil 安全返回 ""。
- **`resolveDeviceID(ctx, dr, edgeID)`**：dr nil 或 edgeID==0 → 0；LookupHostDevice err 或 id==0 → 0（注释：MUST NOT 回退 edge_id，避免 issue #96 污染 TSDB）。
- **`isKubernetesPromSource(source)`**：strings.HasPrefix("k8s:")。
- **`lookupK8sControllerCluster(ctx, reg, edgeID, log)`**：reg nil 或 edgeID==0 → 0；LookupControllerCluster err → Warn + 0。
- **`refreshKubernetesControllerHeartbeat(ctx, c, reg, edgeID)`**：reg nil → nil；若 controllerState 已知用缓存；否则 LookupControllerCluster 决定 + setKubernetesController；isController=true 才调 HandleControllerHeartbeat。
- **`unixOrZero(sec)`**：0 → time.Time{}；否则 time.Unix(sec, 0).UTC()。

### `Client.NotifyPluginConfigsChanged`

- cloud→edge RPC；body=`"{}"`；注释明示 fire-and-forget（caller log 失败；edge 60s poll 兜底）。

## 5. 依赖关系

- **内部包**：`biz/edge`、`biz/metric`、`internal/pkg/tunnel`
- **外部库**：`log/slog`、`encoding/json`、`net`、`strings`、`time`、`fmt`
- **被调用方**：`cmd/ongrid/main.go`（调 Install）；frontier 反向调用触发 handler

## 6. 并发与资源管理

- **handler 闭包捕获 `w` 与 `c`**：Client 内部有锁保护 map；Wiring 字段在 Install 后只读。
- **plugin health 不加锁**：RecordPluginHealth 是 biz 层 in-memory 写入，biz 内部自锁。
- **ctx 透传**：reverse-call handler 的 rpcCtx 由 frontier 注入；lifecycle 用 Install 的 ctx（启动期）。
- **CanonicalizeEdgeID 加 RLock**：handlers 调用时与 bind/unbind 竞争，RWMutex 保证读一致。

## 7. 设计模式与亮点

- **接口在消费方定义**：PromwriteIngester / DeviceResolver / KubernetesRegistry 等都在本文件定义，让 Wiring 不耦合具体 biz 实现，测试可注入 fake。
- **optional 依赖 nil 降级**：PromIngester / PluginConfigUC / WebshellRouter / DeviceResolver / K8sRegistry 都可 nil；对应 handler 不注册或静默 accept。
- **device_id 永不回退 edge_id**：注释详述 issue #96 —— edge/device 拆分后回退会污染 Prom TSDB，调用方必须 drop 而非 persist。
- **canonicalizeEdgeID==0 静默 drop**：v0.7.39 fix —— 避免 raw transport ID 泄露为 ghost Prom series。
- **edge_id mismatch 拒绝**：防止 edge 篡改 body 伪造他人数据。
- **K8s controller vs node 分流**：register_edge 按 in.Kubernetes.Role 分流；controller 清 host device link，node 走正常 host 路径 + 可选 K8sRegistry.HandleRegister(deviceID)。
- **k8s: source 前缀路由 PushKubernetes**：cluster 级 metric 走 PushKubernetes 而非 host Push。
- **heartbeat plugin health best-effort**：注释明示"Never fail the heartbeat on this" —— UI 显示"logs: crashed"是辅助信息，不应阻断主链路。
- **real-time edge_offline alerting 移除**：改由 metric_raw 规则在下个 tick 自动 fire，简化架构。
- **stale offline 防御**：unbindEdgeTransport 三重校验 + canonicalizeEdgeID 翻译，防止误删新 binding。

## 8. 注意事项

- **DeviceResolver nil 回退 edge_id==device_id**：仅 pre-launch 数据正确（迁移时整数复用）；post-launch 必须注入 DeviceResolver。
- **resolveDeviceID==0 时 Accepted=0**：edge 会重试；register_edge 完成后 link 建立，下次 push 成功。
- **PromIngester nil 静默 accept=n**：edge 不重试；注释明示"edge 不应感知 cloud Prom 状态"。
- **push_k8s_inventory 在 K8sInventory nil 时回显 Accepted=length**：让 edge 不重试；用于 Prom/Inventory 未 wire 的部署。
- **canonicalizeEdgeID==0 时 push_k8s_inventory 返回空 response**：edge 不报错但数据丢失；register_edge 后恢复。
- **heartbeat 的 in.EdgeID 校验**：edge 端发送自己的 ID；mismatch 拒绝防伪造。
- **register_edge 的 canonicalEdgeID 来自 req.ClientID()**：frontier Meta 认证后注入；不信任 body 中的 legacy credentials（当前 edge 留空）。
- **K8s controller register 失败返回 error**：阻断 register_edge，让 edge 知道 K8s 注册失败。
- **get_plugin_configs 的 labelDeviceID 经 DeviceResolver**：返回给 edge 的 EdgeID 字段实际是 host device_id，用于 edge 端 metric label。
- **shell_output / shell_exit 仅 WebshellRouter 非 nil 注册**：未 wire 时 webshell 完全禁用。
- **`NotifyPluginConfigsChanged` body=`"{}"`**：edge 收到后重新 fetch，避免 push payload 与 pull response wire-format 耦合。
