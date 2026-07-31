# `usecase.go` 技术实现文档

> 源文件：`internal/manager/biz/device/usecase.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/device`

## 1. 概述

本文件是 manager/device biz 层门面。封装 `Repo` + `EdgeDeviceRepo` + 可选 `TopologyMirror`，让 HTTP handler 不用串两个依赖。核心职责：device CRUD、roles 编辑、删除（含级联 edge + topology node）、presence 自愈 reconciler、edge↔device 双向 lookup。同时承担历史遗留孤儿 device 清理。

## 2. 包信息

- **包名**：`device`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 HTTP handler / edge biz / aiops 工具调用；依赖 `model/device`、`pkg/errs`，通过接口接入 `TopologyMirror`（避免 device→topology 包依赖）

## 3. 关键类型与接口

```go
type Usecase struct {
    repo     Repo
    links    EdgeDeviceRepo  // 可 nil
    topology TopologyMirror  // 可 nil
    log      *slog.Logger
}

type TopologyMirror interface {
    DeleteNodeForDevice(ctx, deviceID, nodeID uint64) error
}

type deletedTopologyDeviceLister interface {
    ListDeletedWithNodeID(ctx, limit int) ([]*model.Device, error)
}

type orphanDeviceLister interface {
    ListWithoutLiveEdges(ctx, limit int) ([]*model.Device, error)
}
```

注：`deletedTopologyDeviceLister` / `orphanDeviceLister` 是未导出的可选能力探测接口 —— Usecase 通过类型断言检查 Repo 是否实现，不强制所有 Repo 实现。

## 4. 关键函数与流程

### `NewUsecase`
- **签名**：`func NewUsecase(repo Repo, links EdgeDeviceRepo, log *slog.Logger) *Usecase`
- **职责**：构造 Usecase；links 可 nil（junction-aware 方法返回 `ErrNotWiredYet`）；log 可 nil

### `SetTopologyMirror`
- **签名**：`func (u *Usecase) SetTopologyMirror(m TopologyMirror)`
- **职责**：注入 device→topology 清理桥；topology Usecase 实现；接口在此避免 device→topology 包依赖（循环）

### `ReconcilePresence`
- **签名**：`func (u *Usecase) ReconcilePresence(ctx) (int64, error)`
- **职责**：把孤儿"幽灵" device（online=true 但无在线关联 edge）翻回 offline
- **流程**：
  1. repo nil → ErrNotWiredYet
  2. `repo.ReconcileOfflineOrphans`
  3. n>0 → Info log
  4. 返回 n
- **调用时机**：boot 一次 + ticker；manager 重启或硬删 edge 后 device presence 也能收敛

### `UpdateRoles`
- **签名**：`func (u *Usecase) UpdateRoles(ctx, id uint64, names []string) error`
- **职责**：写 device-roles 位集（侧边栏分组 + AI prompt 路由用）
- **流程**：
  1. repo nil → ErrNotWiredYet
  2. 遍历 names：TrimSpace；空跳过；`model.IsValidRoleName` 校验 → ErrInvalid
  3. `model.EncodeRoles(names)`
  4. `model.IsValidRoles(roles)` 二次校验 → ErrInvalid
  5. `repo.UpdateRoles`
  6. Info log 含 roles + decoded names
- **关键约束**：canonical wire 形状（"server"/"storage"/"network"/"database"）；"unknown" 或空列表清空位集；canonical 之外的 name 拒绝（防 typo 进幻影 bucket）

### `Delete`
- **签名**：`func (u *Usecase) Delete(ctx, id uint64) error`
- **职责**：删 offline device + 关联 Edge 身份；拒绝 online device
- **流程**：
  1. repo nil → ErrNotWiredYet
  2. `repo.Get` 取 device
  3. `repo.DeleteOfflineWithLinkedEdges`（事务，拒绝 online 返回 ErrConflict）
  4. `deleteTopologyNode`（best-effort，topology 镜像）
- **错误处理**：Get/Delete 失败透传；topology 删除失败仅返回错误（DB 已提交）

### `ReconcileDeletedTopology`
- **签名**：`func (u *Usecase) ReconcileDeletedTopology(ctx) (int, error)`
- **职责**：清理"topology cleanup hook 出现之前"被删 device 留下的 topology node
- **流程**：
  1. repo nil → ErrNotWiredYet；topology nil → 返回 0,nil
  2. 类型断言 `repo.(deletedTopologyDeviceLister)`；不支持则 0,nil
  3. `ListDeletedWithNodeID(ctx, 5000)`
  4. 逐行 `deleteTopologyNode`；err 返回 cleaned, err
  5. cleaned>0 → Info log
- **关键设计**：类型断言探测可选能力；不强制 Repo 实现

### `ReconcileOrphanDevices`
- **签名**：`func (u *Usecase) ReconcileOrphanDevices(ctx) (int, error)`
- **职责**：清理"无任何 live edge 关联"的 live device 行；治愈老 DELETE /edges 路径遗留
- **流程**：
  1. repo nil → ErrNotWiredYet
  2. 类型断言 `repo.(orphanDeviceLister)`；不支持则 0,nil
  3. `ReconcilePresence`（先把 ghost 翻 offline）
  4. `ListWithoutLiveEdges(ctx, 5000)`
  5. 逐行 `u.Delete`；err 返回 cleaned, err
  6. cleaned>0 → Info log
- **关键设计**：复用 `u.Delete`（含 topology 清理）；5000 上限防一次性扫爆

### `deleteTopologyNode`
- **签名**：`func (u *Usecase) deleteTopologyNode(ctx, d *model.Device) error`
- **职责**：删除 device-owned topology node；best-effort
- **流程**：
  1. topology nil / d nil / NodeID nil / *NodeID==0 → return nil
  2. `topology.DeleteNodeForDevice(ctx, d.ID, *d.NodeID)`
  3. err 是 ErrNotFound → 吞掉（已删）
  4. 其他 err → `%w` 包装

### `LookupHostDevice / LookupEdgeForDevice`
- **签名**：edge↔device 双向 lookup
- **职责**：push/pull 路径核心；links nil 返回 ErrNotWiredYet
- **流程**：透传到 links.LookupHostDevice / links.LookupEdgeForDevice(type=Host)

### `LinkHost`
- **签名**：`func (u *Usecase) LinkHost(ctx, edgeID, deviceID uint64) error`
- **职责**：upsert (edge, device, type=host) junction；edge register 流程调用
- **流程**：links nil → ErrNotWiredYet；否则透传 links.Link(type=Host)

## 5. 依赖关系

- **内部包**：`model/device`、`pkg/errs`
- **桥接接口**：`TopologyMirror`（topology Usecase 实现）、`deletedTopologyDeviceLister`/`orphanDeviceLister`（data 层可选实现）
- **被调用方**：HTTP handler（list/get/update_roles/delete）、edge biz（HandleRegister 调 FindOrCreateByFingerprint + UpdateHostFacts + MarkOnline + LinkHost）、aiops 工具（device lookup）

## 6. 并发与资源管理

- **无共享状态**：Usecase 仅持有不可变 repo + links + topology + log
- **无锁**：所有状态在 DB
- **ReconcilePresence 单调调用**：boot + ticker；不并发
- **ctx 透传**：所有 IO 第一参 context

## 7. 设计模式与亮点

- **接口隔离**：Usecase 只暴露 HTTP/edge 需要的方法；`Repo()` / `Links()` 直接返回底层 repo 供 edge handler hydrate host_info
- **TopologyMirror 反向依赖**：接口在 device 包定义，topology 包实现 —— 避免 device→topology 包循环
- **类型断言探测可选能力**：`deletedTopologyDeviceLister` / `orphanDeviceLister` 未导出；Repo 不实现就跳过 reconcile，向后兼容
- **ReconcilePresence 自愈**：注释明示"per-event MarkOnline/MarkOffline 路径看不到已不存在的 edge" —— reconciler 是收敛保证
- **UpdateRoles canonical 校验**：拒绝 canonical 之外的 name 防 typo 进幻影 bucket
- **Delete 先 DB 后 topology**：topology 失败时 DB 已提交；下次 ReconcileDeletedTopology 兜底
- **ReconcileOrphanDevices 先 reconcile presence**：先把 ghost 翻 offline 避免误删 live

## 8. 注意事项

- **Delete 拒绝 online**：`DeleteOfflineWithLinkedEdges` 内置 ErrConflict；调用方应先检查 Online 或捕获 ErrConflict
- **UpdateRoles "unknown" 清空**：传 ["unknown"] 或 [] 等价清空位集
- **ReconcileDeletedTopology 5000 上限**：单次扫 5000 行；超过需多次调用
- **ReconcileOrphanDevices 调 u.Delete**：会触发 topology 删除；失败时部分清理结果保留
- **links nil 时 junction 方法返回 ErrNotWiredYet**：调用方需优雅降级
- **NewUsecase 不强制 log**：log nil 时不 panic，但部分 Info log 不输出
- **Repo() / Links() 直接暴露**：edge handler 用 Repo() 直接 hydrate host_info；这是受控的 escape hatch
