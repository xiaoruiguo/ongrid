# `service.go` 技术实现文档

> 源文件：`internal/manager/service/edge/service.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/service/edge`

## 1. 概述

本文件是 edge 领域的 manager 应用服务层：HTTP 请求校验 + 错误映射 + 委托 `biz/edge.Usecase`。永不 import `data/`。核心红线：(1) SecretKey 明文仅在 Create / RotateSecret 返回一次，不存储可读形态；(2) Kubernetes-managed edge 拒绝通过普通 mutation 路径操作（Delete / RotateSecret / UpgradeAgent / FetchPackage / ApplyPackage 全部经 `rejectManagedMutation` guard），必须用对应的 Managed 变体或 Kubernetes cluster 操作；(3) caller 后置注入（frontierbound.Client 构造晚于 edgeSvc）。

## 2. 包信息

- **包名**：`edge`
- **所属模块**：`internal/manager/service/edge`
- **依赖方向**：被 HTTP handler 调用；依赖 `biz/edge`、`model/edge`、`internal/pkg/errs`、`internal/pkg/tunnel`

## 3. 关键类型与接口

```go
type EdgeCaller interface {
    Call(ctx, edgeID uint64, method string, body []byte) ([]byte, error)
}

type ManagedEdgeGuard interface {
    ManagedClusterIDForEdge(ctx, edgeID uint64) (clusterID uint64, managed bool, err error)
}

type Service struct {
    uc     *biz.Usecase
    caller EdgeCaller  // nil 禁用 UpgradeAgent / GetProcessList 等
    guard  ManagedEdgeGuard
    log    *slog.Logger
}
```

`EdgeCaller` 由 `frontierbound.Client.Call` 实现；`ManagedEdgeGuard` 由 `service/k8s` 实现（同 service/k8s 的 `ManagedClusterIDForEdge`）。

## 4. 关键函数与流程

### 构造与注入

- **`New(uc, caller, log)`**：caller 可 nil —— 依赖它的 handler（UpgradeAgent）会返回 `ErrNotWiredYet` 类错误直到 `SetEdgeCaller` back-fill。
- **`SetEdgeCaller(c)`** / **`SetManagedEdgeGuard(g)`**：后置注入；注释明示"wiring happens during startup before HTTP servers listen"，启动期单线程调用安全。

### Edge CRUD

- **`Create(ctx, name, createdBy)`**：委托 uc.Create；返回 `*biz.CreateResult` 含明文 SecretKey —— 注释明示"必须一次性回显给 caller，API 无法再次获取"。
- **`List(ctx, f biz.ListFilter)`** / **`Get(ctx, id)`**：纯委托。
- **`Delete(ctx, id)`**：先 `rejectManagedMutation` guard；委托 `uc.Delete`（soft-delete）。
- **`DeleteManaged(ctx, id)`**：无 guard，直接委托 —— 供 Kubernetes cluster 操作路径调用。
- **`RotateSecret(ctx, id)`**：先 guard；委托 `uc.RotateSecret`，返回新明文 secret（一次性）。
- **`RotateManagedSecret(ctx, id)`**：无 guard，直接委托。

### Tunnel 回调

- **`HandleRegister(ctx, edgeID, info, agentVersion)`** / **`HandleHeartbeat(ctx, edgeID, ts)`**：thin passthrough 到 biz.Usecase，供 frontierbound handlers 调用。
- **`PluginHealth(edgeID)`**：返回 in-memory 的 per-plugin 运行时健康（由 heartbeat 路径喂入）；无数据返回 nil。

### Cloud→Edge RPC

- **`GetProcessList(ctx, edgeID, topN, sortBy)`**：
  1. caller nil → 错误"processes not wired: no edge caller configured"。
  2. marshal `tunnel.GetProcessListRequest`；caller.Call(`MethodGetProcessList`)。
  3. unmarshal `tunnel.GetProcessListResponse`；失败 `%w`。
- **`UpgradeAgent(ctx, edgeID, url, sha256)`**：
  1. `rejectManagedMutation` guard。
  2. caller nil → "upgrade not wired"。
  3. marshal `tunnel.AgentUpgradeRequest{URL, SHA256}`；Call(`MethodAgentUpgrade`)。
  4. unmarshal response（含 StagedPath + Bytes）。
  5. 注释明示 edge stages binary + sha256 校验，实际 swap 在下次 systemd ExecStartPre 重启时发生。
- **`FetchPackage(ctx, edgeID, url, sha256, version)`** / **`ApplyPackage(ctx, edgeID)`**：integer-bundle 升级两半；URL+sha 由 HTTP handler 自动解析（admin 一键点）；均经 guard + caller nil 检查。

### `rejectManagedMutation(ctx, edgeID)`

- **职责**：拒绝 Kubernetes-managed edge 走普通 mutation 路径。
- **流程**：guard nil → 直接 nil（无 guard 时放行）；调 `guard.ManagedClusterIDForEdge`；err → `%w`；managed=true → `ErrConflict` 包装（含 clusterID 提示用 Kubernetes 操作）。

## 5. 依赖关系

- **内部包**：`biz/edge`、`model/edge`、`internal/pkg/errs`、`internal/pkg/tunnel`
- **外部库**：`log/slog`、`encoding/json`、`fmt`、`time`
- **被调用方**：HTTP handler（edge CRUD / upgrade / processes）；frontierbound handlers（HandleRegister / HandleHeartbeat / PluginHealth）

## 6. 并发与资源管理

- **无共享可变状态**：Service 字段在启动期注入后只读；并发安全依赖 biz.Usecase。
- **caller / guard 后置注入**：注释明示启动期单线程调用；HTTP 流量到达前完成。
- **ctx 透传**：所有 IO 函数首参 `context.Context`。

## 7. 设计模式与亮点

- **Managed / 普通 mutation 双路径**：Delete / RotateSecret / UpgradeAgent / FetchPackage / ApplyPackage 都有 `*Managed` 变体绕过 guard，供 Kubernetes cluster 操作路径调用。普通路径经 `rejectManagedMutation` 防止误操作 K8s 管理的 edge。
- **SecretKey 一次性回显**：Create / RotateSecret 返回明文，API 无法再获取 —— 强制客户端立即保存。
- **caller 后置注入**：与 pluginConfigUC 同 pattern，解决 frontierbound.Client 构造晚于 edgeSvc 的依赖顺序。
- **thin shim 设计**：注释明示"validation + DTO translation in HTTP layer, business logic in biz" —— Service 层不承载业务规则。
- **错误信息含上下文**：caller nil 时返回"not wired: no edge caller configured"而非通用 ErrNotWiredYet —— 便于运维诊断。

## 8. 注意事项

- **Managed 变体无 guard**：`DeleteManaged` / `RotateManagedSecret` 直接调 uc，调用方（K8s cluster 操作）必须自行确保权限。
- **caller nil 时返回非 sentinel 错误**：`fmt.Errorf("processes not wired: ...")` 不是 `errs.ErrNotWiredYet` —— 上层 handler 需用字符串匹配或接受此差异。
- **UpgradeAgent 无 retry**：edge 端 sha256 mismatch / bad URL 等错误直接 wrap 返回。
- **GetProcessList / UpgradeAgent 等无 guard 检查 caller nil 顺序**：先 guard（如有）再 caller nil 检查 —— managed edge 优先拒绝。
- **PluginHealth 无 ctx**：in-memory 读取，不涉及 IO；返回值 nil 表示无数据。
- **HandleRegister / HandleHeartbeat 是 tunnel-side entrypoint**：被 frontierbound handlers 调用，非 HTTP handler。
- **`rejectManagedMutation` guard nil 时放行**：未注入 guard 的部署下不强制 K8s 隔离 —— 部署方需自行确保 guard 已 wire。
