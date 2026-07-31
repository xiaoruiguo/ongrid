# `client.go` 技术实现文档

> 源文件：`internal/manager/service/frontierbound/client.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/service/frontierbound`

## 1. 概述

本文件是 manager 侧对上游 `github.com/singchia/frontier` service-end SDK 的封装。维持与 frontier broker 的长连接 geminio 服务连接，注册生命周期回调（GetEdgeID / EdgeOnline / EdgeOffline），并暴露 `Caller` 表面（Call / Register）让 biz 代码无需学习 geminio 类型。核心红线：(1) `canonicalizeEdgeID` 在 binding 未建立时返回 0，防止把 raw geminio transport ID（opaque 64-bit）泄露为 Prom `edge_id` label 产生 ghost series（v0.7.39 fix）；(2) `unbindEdgeTransport` 用 canonicalEdgeID + addr 双重校验，防止 frontier 在替换连接已 online 后仍投递旧连接的 offline 事件误删新 binding；(3) `NewDisabled` 提供 e2e / degraded-broker 路径，所有出站调用返回 `ErrDisabled`，反向注册 no-op。

## 2. 包信息

- **包名**：`frontierbound`
- **所属模块**：`internal/manager/service/frontierbound`
- **依赖方向**：被 `cmd/ongrid/main.go` 构造、`service/frontierbound/handlers.go` Install 调用、`service/edge` 通过 EdgeCaller 接口调用；依赖 `internal/pkg/tunnel`、`github.com/singchia/frontier/api/dataplane/v1/service`、`github.com/singchia/geminio`

## 3. 关键类型与接口

```go
type Config struct {
    Addr        string  // "frontier:40011"
    ServiceName string  // fbsvc.OptionServiceName 路由标识
}

type Handler func(ctx, edgeID uint64, body []byte) ([]byte, error)

// service 是 fbsvc.Service 的窄接口切片，让测试可注入 fake
type service interface {
    NewRequest(data []byte) geminio.Request
    Call(ctx, edgeID uint64, method string, req geminio.Request) (geminio.Response, error)
    Register(ctx, method string, rpc geminio.RPC) error
    RegisterGetEdgeID(ctx, fn fbsvc.GetEdgeID) error
    RegisterEdgeOnline(ctx, fn fbsvc.EdgeOnline) error
    RegisterEdgeOffline(ctx, fn fbsvc.EdgeOffline) error
    OpenStream(ctx, edgeID uint64) (geminio.Stream, error)
    Close() error
}

var _ service = (fbsvc.Service)(nil)  // 编译期检查

type Client struct {
    svc service
    log *slog.Logger

    mu                sync.RWMutex
    transportToEdgeID map[uint64]uint64
    edgeIDToTransport map[uint64]uint64
    transportAddrs    map[uint64]string
    k8sControllers    map[uint64]bool
}

var ErrDisabled = errors.New("frontierbound: disabled")
```

## 4. 关键函数与流程

### 构造

- **`New(cfg, log)`**：
  1. log nil → `slog.Default()`；cfg.Addr 空 → error。
  2. dialer 闭包 `net.Dial("tcp", cfg.Addr)`。
  3. opts 附加 `OptionServiceName`（非空时）。
  4. `fbsvc.NewService(dialer, opts...)` 失败 `%w`。
  5. 初始化四个 map；Info 日志记录 addr + service_name。
- **`newWithService(svc, log)`**：测试 seam，注入 fake service。
- **`NewDisabled(log)`**：svc=nil；出站调用返 `ErrDisabled`；Register* 全 no-op；用于 `ONGRID_FRONTIER_DISABLED=true` 路径（e2e / degraded-broker）。

### 出站调用

- **`Call(ctx, edgeID, method, body)`**：
  1. svc nil → `ErrDisabled`。
  2. `resolveTransportID(edgeID)` 查 edgeID→transport 映射；未命中返回 edgeID 本身（注释：直连场景 transportID==edgeID）。
  3. `svc.NewRequest(body)`；`svc.Call(ctx, transportID, method, req)`。
  4. err → `%w`（含 method/edgeID/transportID 上下文）。
  5. `rsp.Error()` 非 nil → `%w`（remote error）。
  6. 返回 `rsp.Data()`。
- **`WriteDatabaseMetricsSecrets(ctx, edgeID, reqs)`**：批量写托管 DB exporter 凭据；marshal → Call(`MethodWriteDatabaseMetricsSecret`) → unmarshal；`!resp.OK` → error。注释明示"secret material 不被 manager 持久化"。
- **`OpenStream(ctx, edgeID)`**：开双向字节流（WebSSH 用 —— `ssh.NewClientConn(stream, "127.0.0.1:22", cfg)`）；resolveTransportID 后 `svc.OpenStream`；err `%w`。注释解释 Meta blob（如 `{"target":"127.0.0.1:22"}`）让 edge 解码后 dial 本地 socket，保持 tunnel 层通用。
- **`NotifyPluginConfigsChanged(ctx, edgeID)`**：fire-and-forget 推送（`MethodPluginConfigsChanged`，body=`"{}"`）；注释明示 edge 60s safety-net poll 兜底；实现 `edgebiz.EdgeReloadNotifier`。

### 反向注册

- **`Register(ctx, method, h)`**：
  1. h nil → error。
  2. svc nil → no-op return nil（disabled client）。
  3. wrap 闭包：从 `req.ClientID()` 取 edgeID（frontier 通过 custom-byte tail 注入）；调 `h(rpcCtx, edgeID, req.Data())`；err → `rsp.SetError(err)`；否则 `rsp.SetData(out)`。
  4. `svc.Register(ctx, method, wrap)`。
- **`RegisterGetEdgeID`** / **`RegisterEdgeOnline`** / **`RegisterEdgeOffline`**：thin 透传到 svc；svc nil 时 no-op。
- **`Close()`**：svc nil no-op；否则 `svc.Close()`。

### Binding 管理（核心）

- **`bindEdgeTransport(transportID, edgeID)`** / **`bindEdgeTransportAt(transportID, edgeID, addr)`**：
  - 0 值直接返回。
  - 加锁；若 transportID 已绑定不同 edgeID，清除旧 edgeID 映射 + 旧 addr；若 edgeID 已绑定不同 transportID，同理清除。
  - 写入双向映射；addr 非空时记录。
- **`unbindTransport(transportID)`**：委托 `unbindEdgeTransport(transportID, 0, "")`。
- **`unbindEdgeTransport(transportID, canonicalEdgeID, addr) bool`**：
  - transportID==0 → false。
  - 加锁；查 transportToEdgeID；若 mapped 且 canonicalEdgeID!=0 且 != mappedEdgeID → false（canonical 不匹配，stale event）。
  - 查 edgeIDToTransport；若 active 且 != transportID → false（已被新连接替换）。
  - 查 transportAddrs；若 activeAddr!="" 且 addr!="" 且 != activeAddr → false（addr 不匹配，stale）。
  - 全部通过才 delete 三个 map + k8sControllers；返回 true。
- **`setKubernetesController(edgeID, enabled)`** / **`isKubernetesController(edgeID)`** / **`kubernetesControllerState(edgeID) (isController, known)`**：管理 K8s controller 标记。
- **`canonicalizeEdgeID(edgeID)`**：
  - edgeID==0 → 0。
  - 加锁查 transportToEdgeID；命中返回 canonical；未命中返回 **0**（注释：防止 raw transport ID 泄露为 Prom label 产生 ghost series，v0.7.39 fix）。
- **`resolveTransportID(edgeID)`**：edgeID==0 → 0；查 edgeIDToTransport；未命中返回 edgeID 本身（直连场景）。

## 5. 依赖关系

- **内部包**：`internal/pkg/tunnel`
- **外部库**：`github.com/singchia/frontier/api/dataplane/v1/service`、`github.com/singchia/geminio`、`log/slog`、`sync`、`net`、`encoding/json`、`errors`、`fmt`
- **被调用方**：`cmd/ongrid/main.go`（构造）、`service/frontierbound/handlers.go`（Install）、`service/edge`（EdgeCaller）

## 6. 并发与资源管理

- **`mu`（sync.RWMutex）**：保护四个 map。bind/unbind/setK8sController 用 Lock；canonicalizeEdgeID / resolveTransportID / kubernetesControllerState 用 RLock。
- **map 永不 shrink**：delete 后容量不变；binding 数量受 edge 总数限制，可控。
- **disabled client 安全**：svc nil 时所有调用短路，无锁竞争。
- **geminio.Stream 生命周期**：OpenStream 返回的 stream 由 caller 管理（WebSSH 用 `io.Copy` + `Close`）。

## 7. 设计模式与亮点

- **窄接口切片**：`service` interface 仅暴露本包实际用到的方法子集；编译期 `var _ service = (fbsvc.Service)(nil)` 检查上游类型满足；测试用 fake 注入。
- **transport ID ↔ canonical edgeID 双向映射**：frontier 的 transportID 是 opaque 64-bit，与业务 edgeID 解耦；双向 map 让 Call 和 reverse-call 都能翻译。
- **stale offline 事件防御**：`unbindEdgeTransport` 三重校验（canonicalEdgeID + active transport + addr）防止 frontier 投递的旧连接 offline 事件误删新 binding 或误标 edge offline。
- **canonicalizeEdgeID 返回 0 而非 raw ID**：注释详述 v0.7.39 fix —— 让 raw transport ID 泄露为 `edge_id="7634732871700095575"` 会产生 ghost series 污染 Grafana dropdown 直到 tsdb retention 清理。
- **NewDisabled 优雅降级**：e2e / degraded-broker 路径不依赖真 frontier；Register* no-op 让 Install 不报错。
- **Handler adapter 抽取 edgeID**：reverse-call handler 不感知 geminio.Request，只处理 `(ctx, edgeID, body)`。
- **OpenStream Meta blob 通用化**：注释明示"添加未来 stream-based 协议（port forwarding / file copy）只动 Meta"。

## 8. 注意事项

- **`resolveTransportID` 未命中返回 edgeID 本身**：直连场景 transportID==edgeID；但若 binding 未建立（race on first connect），caller 用 raw transport ID 调 Call 会被 frontier 拒绝（transport not found）—— handlers.go 用 `canonicalizeEdgeID` 先校验。
- **`canonicalizeEdgeID` 返回 0 时 caller 应 drop**：handlers.go 的 push_prom_samples / push_host_metrics 都遵循此约定。
- **disabled client 的 Register* no-op**：handler 不会感知 frontier 缺失；e2e 测试需自行 mock edge reverse call。
- **`unbindEdgeTransport` 的 addr 校验仅当 activeAddr!="" 且 addr!=""**：任一为空则跳过 addr 检查（向后兼容旧 frontier 不带 addr 的场景）。
- **`k8sControllers` map 与 binding 解耦**：setKubernetesController 独立于 bind/unbind；unbind 时显式 delete k8sControllers[canonicalEdgeID]。
- **`WriteDatabaseMetricsSecrets` 不持久化 secret**：注释明示仅透传给 edge。
- **`NotifyPluginConfigsChanged` 是 fire-and-forget**：caller 自行 log 失败；edge 60s poll 兜底。
- **`New` 的 dialer 是同步 `net.Dial`**：若 frontier 暂时不可达会阻塞；caller 应在启动期处理。
