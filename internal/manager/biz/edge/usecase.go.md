# `usecase.go` 技术实现文档

> 源文件：`internal/manager/biz/edge/usecase.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/edge`

## 1. 概述

本文件是 manager/edge biz 层门面。核心职责：注册新 edge（生成 AccessKey + argon2id hash SecretKey + 种默认插件配置）、HandleRegister（device 指纹 upsert + host facts 刷新 + junction link + topology 镜像 + status online）、HandleHeartbeat / HandleOffline、RotateSecret、Delete（含 last-edge 检测级联删 device + topology node）、设备指纹 v3 迁移（legacy HostID-derived → hardware fingerprint）。

## 2. 包信息

- **包名**：`edge`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 HTTP handler / tunnel RPC / aiops 工具调用；依赖 `biz/device`、`model/device`、`model/edge`、`pkg/errs`、`pkg/passwd`、`pkg/tunnel`，通过接口接入 `NodeMirror`（topology）/ `PluginConfigSeeder`（plugin_config）

## 3. 关键类型与接口

```go
const (
    accessKeyEntropyBytes = 18  // → 24 chars base64url
    secretKeyEntropyBytes = 24  // → 32 chars base64url
)

type NodeMirror interface {
    EnsureNodeForDevice(ctx, deviceID uint64, deviceName string) (uint64, error)
    DeleteNodeForDevice(ctx, deviceID, nodeID uint64) error
}

type PluginConfigSeeder interface {
    UpsertSpec(ctx, edgeID uint64, plugin string, enabled bool, specJSON string) error
}

type Usecase struct {
    repo    Repo
    devices devicebiz.Repo
    links   devicebiz.EdgeDeviceRepo
    mirror  NodeMirror       // 可 nil
    plugins PluginConfigSeeder  // 可 nil
    log     *slog.Logger
    phMu         sync.RWMutex
    pluginHealth map[uint64][]PluginHealth  // lazy init
}

type CreateResult struct {
    Edge      *model.Edge
    AccessKey string
    SecretKey string  // 明文；仅此一次暴露；仅存 hash
}
```

## 4. 关键函数与流程

### `NewUsecase`
- **签名**：`func NewUsecase(repo Repo, devices devicebiz.Repo, links devicebiz.EdgeDeviceRepo, log *slog.Logger) *Usecase`
- **职责**：构造 Usecase；devices 必非 nil（register 流程依赖）；links 可 nil（回退 legacy 1:1 Edge.DeviceID-only）；log 可 nil

### `SetNodeMirror / SetPluginSeeder`
- **签名**：post-construction 注入
- **职责**：cmd/ongrid 先构造 Usecase，等 topology / plugin_config 就绪后回填

### `Create`
- **签名**：`func (u *Usecase) Create(ctx, name string, createdBy *uint64) (*CreateResult, error)`
- **职责**：注册新 edge；生成凭据；argon2id hash；插入 row；种默认插件
- **流程**：
  1. repo nil → ErrNotWiredYet
  2. name TrimSpace（空允许，HandleRegister 回填）
  3. `randomURLSafe(accessKeyEntropyBytes)` → 24-char AccessKeyID
  4. `randomURLSafe(secretKeyEntropyBytes)` → 32-char SecretKey
  5. `passwd.Hash(sk)` → argon2id hash
  6. 构造 `model.Edge{Name, AccessKeyID, SecretKeyHash: hash, Status: Offline, CreatedBy}`
  7. `repo.Create`（ID 回填）
  8. Info log
  9. `plugins != nil` → `seedDefaultPlugins`
  10. 返回 `CreateResult{Edge, AccessKey, SecretKey}`（**SecretKey 仅此一次暴露**）
- **错误处理**：rand/hash/Create 失败 `%w` 包装

### `seedDefaultPlugins`
- **签名**：`func (u *Usecase) seedDefaultPlugins(ctx, edgeID uint64)`
- **职责**：种 5 个默认启用插件（logs/traces/metrics/hostmetrics/procmetrics）
- **流程**：遍历 defaults；`plugins.UpsertSpec(ctx, edgeID, name, true, spec)`；失败 Warn（不阻塞 create）
- **关键设计**：失败仅 Warn；操作员可 UI 手动 toggle 兜底

### `HandleRegister`
- **签名**：`func (u *Usecase) HandleRegister(ctx, edgeID uint64, info tunnel.HostInfo, agentVersion string) error`
- **职责**：处理 register_edge RPC；upsert device + link + topology mirror + status online
- **流程**：
  1. repo/devices nil → ErrNotWiredYet
  2. `repo.GetByID(edgeID)` 取 edge
  3. `fp = deviceFingerprint(info)`（v3 硬件指纹优先）
  4. **v3 迁移**：oldFP != fp → `devices.RebindFingerprint(oldFP, fp)`（best-effort，失败 Warn）
  5. 构造 device seed（Fingerprint + UserID + Hostname/OS/Arch/...）
  6. `devices.FindOrCreateByFingerprint(seed)` → dev
  7. `devices.UpdateHostFacts(dev.ID, ...)` 刷新最新 facts
  8. `devices.MarkOnline(dev.ID)`
  9. **topology mirror**：mirror 非 nil 且 dev.NodeID==nil → `mirror.EnsureNodeForDevice` + `devices.SetNodeID`（best-effort，失败 Warn）
  10. **junction link**：links 非 nil → `links.Link(edgeID, dev.ID, EdgeDeviceRelationHost)`
  11. edge.DeviceID nil 或 != dev.ID → `repo.SetDeviceID`（convenience pointer 同步）
  12. edge.Name 空且 hostname 非空 → `repo.UpdateName`（首次回填）
  13. `repo.UpdateStatus(edgeID, Online, now)`
  14. agentVersion 非空且 != edge.AgentVersion → `repo.SetAgentVersion`
- **错误处理**：每步 `%w` 包装；topology mirror 失败 Warn 不阻塞

### `deviceFingerprint / deviceFingerprintLegacy / hashFingerprint`
- **签名**：指纹生成
- **职责**：生成稳定 per-host id 存 `devices.fingerprint`
- **流程**：
  - `deviceFingerprint`：`info.HardwareFingerprint` 非空 → `hashFingerprint("hw:" + hw)`；否则 fallback legacy
  - `deviceFingerprintLegacy`：`info.Fingerprint`（gopsutil HostID）非空 → `hashFingerprint("machine-id:" + fp)`；否则 `hashFingerprint("hostname:" + lower(hostname))`
  - `hashFingerprint`：`"fp_" + hex(sha256(seed)[:16])`
- **关键设计**：v3 硬件指纹优先（hypervisor 重生成 NIC MAC，克隆 VM 保持独立）；legacy 用于老 agent 或无物理 NIC 主机；hash+prefix 统一列形状 + 防枚举

### `RotateSecret`
- **签名**：`func (u *Usecase) RotateSecret(ctx, id uint64) (string, error)`
- **职责**：生成新 SecretKey 替换 hash；返回明文（仅此一次）
- **流程**：
  1. repo nil → ErrNotWiredYet
  2. `randomURLSafe(secretKeyEntropyBytes)` → 新 sk
  3. `passwd.Hash(sk)`
  4. `repo.UpdateSecretHash(id, hash)`
  5. Info log
  6. 返回 sk
- **关键约束**：旧 secret 立即失效；运行中 tunnel session 不受影响（geminio 仅握手校验）；runbook 应指导操作员 kick edge

### `HandleHeartbeat`
- **签名**：`func (u *Usecase) HandleHeartbeat(ctx, edgeID uint64, ts time.Time) error`
- **职责**：翻 status=online + last_seen_at；同步 device last_seen
- **流程**：
  1. `repo.UpdateStatus(edgeID, Online, ts)`
  2. devices 非 nil → `repo.GetByID(edgeID)` 取 edge；edge.DeviceID 非 nil → `devices.MarkOnline(*edge.DeviceID)`（best-effort，失败 Warn）
- **关键设计**：device last_seen 必须同步，否则 device 列表显示"last seen hours ago"而 edge 心跳秒级新鲜

### `HandleOffline`
- **签名**：`func (u *Usecase) HandleOffline(ctx, edgeID uint64, at time.Time) error`
- **职责**：翻 status=offline；device 也翻 offline
- **流程**：
  1. `repo.UpdateStatus(edgeID, Offline, at)`
  2. devices 非 nil → edge.DeviceID 非 nil → `devices.MarkOffline(*edge.DeviceID)`（best-effort，忽略 ErrNotFound）

### `Delete`
- **签名**：`func (u *Usecase) Delete(ctx, id uint64) error`
- **职责**：软删 edge；若 last live edge for host device → 级联删 device + topology node；否则仅 device mark offline
- **流程**：
  1. `repo.GetByID(id)` 取 edge
  2. devices 非 nil 且 edge.DeviceID 非 nil → `deleteDeviceIfLastEdge(id, *edge.DeviceID)`
  3. 未删 device 且未 handled → `devices.MarkOffline`（best-effort Warn）
  4. `repo.Delete(id)` 软删
- **错误处理**：Get/Delete 失败透传；device mark offline 失败 Warn

### `deleteDeviceIfLastEdge`
- **签名**：`func (u *Usecase) deleteDeviceIfLastEdge(ctx, edgeID, deviceID uint64) (removed bool, handled bool, err error)`
- **职责**：检测当前 edge 是否是 device 的 last live edge；是则删 device + topology + junction；否则仅 unlink 当前 edge 的 junction
- **流程**：
  1. links nil → false, false, nil（回退 legacy）
  2. `links.ListEdgesForDevice(deviceID)` 取所有 junction
  3. 遍历其他 edge：任一仍存在 → `unlinkCurrentEdgeDeviceLinks` + return false, true, nil
  4. 无其他 live edge：
     - `devices.Get(deviceID)`；ErrNotFound → false, true, nil
     - mirror 非 nil 且 dev.NodeID 非 nil → `mirror.DeleteNodeForDevice`（best-effort，ErrNotFound 吞）
     - 遍历 junction → `links.Unlink`
     - `devices.Delete(deviceID)`
     - return true, true, nil
- **错误处理**：每步 `%w` 包装

### `ClearHostDeviceLink`
- **签名**：`func (u *Usecase) ClearHostDeviceLink(ctx, edgeID uint64) error`
- **职责**：清 edge 的 host Device 关联；Kubernetes controller edge 自愈旧误注册
- **流程**：
  1. `repo.GetByID(edgeID)` 取 edge
  2. 收集 deviceIDs（edge.DeviceID + links.LookupHostDevice）
  3. 遍历 deviceIDs → `deleteDeviceIfLastEdge`；handled 且 removed → Info log
  4. 否则 `links.Unlink` + `devices.MarkOffline`（best-effort Warn）
  5. `repo.ClearDeviceID(edgeID)`

### `randomURLSafe`
- **签名**：`func randomURLSafe(nBytes int) (string, error)`
- **职责**：`base64.RawURLEncoding.EncodeToString(rand.Read(nBytes))`

## 5. 依赖关系

- **内部包**：`biz/device`（Repo + EdgeDeviceRepo）、`model/device`、`model/edge`、`pkg/errs`、`pkg/passwd`（argon2id Hash）、`pkg/tunnel`（HostInfo）
- **桥接接口**：`NodeMirror`（topology Usecase）、`PluginConfigSeeder`（PluginConfigUC）
- **被调用方**：HTTP handler（Create/List/Delete/RotateSecret）、tunnel RPC（HandleRegister/HandleHeartbeat/HandleOffline）

## 6. 并发与资源管理

- **`phMu sync.RWMutex`**：保护 `pluginHealth map`（见 plugin_health.go）
- **无其他共享状态**：Usecase 持有不可变 repo/devices/links/mirror/plugins/log
- **无锁 register/heartbeat/offline**：所有状态在 DB
- **ctx 透传**：所有 IO 第一参 context

## 7. 设计模式与亮点

- **设备指纹 v3 优先**：`HardwareFingerprint`（MAC|CPU|disk hash）优先于 legacy HostID；hypervisor 重生成 NIC MAC 让克隆 VM 保持独立（issue #96）
- **v3 迁移 in-place**：`RebindFingerprint(oldFP, fp)` 保留 device.ID 和历史；新 agent 首次 re-register 时迁移
- **hash+prefix 统一列形状**：`"fp_" + hex(sha256(seed)[:16])`；防 raw id 枚举 + 跨平台统一
- **SecretKey 仅此一次暴露**：Create/RotateSecret 返回明文；DB 仅存 argon2id hash
- **种默认插件**：Create 后 `seedDefaultPlugins` 让 SPA Monitor/Logs/Traces 页面"开箱即用"
- **topology mirror best-effort**：mirror 失败不阻塞 register；topology Migrate boot 时兜底
- **device last_seen 同步**：HandleHeartbeat 不忘 `devices.MarkOnline` 同步 device last_seen
- **last-edge 检测级联删**：Delete 时检测当前 edge 是否是 device 的 last live edge；是则删 device + topology + junction
- **ClearHostDeviceLink 自愈**：Kubernetes controller edge 每次注册调此清旧误注册的 host 链接
- **NodeMirror / PluginConfigSeeder 接口隔离**：避免 edge→topology / edge→plugin_config 包循环
- **post-construction 注入**：SetNodeMirror / SetPluginSeeder 让 cmd/ongrid 先构造 Usecase 再回填依赖

## 8. 注意事项

- **SecretKey 仅此一次暴露**：Create/RotateSecret 返回明文；调用方必须持久化给操作员；DB 不存明文
- **RotateSecret 旧 session 不踢**：geminio 仅握手校验；运行中 session 继续有效；runbook 应指导操作员 kick edge
- **HandleRegister v3 迁移 best-effort**：RebindFingerprint 失败 Warn 不阻塞；device 会在新 fp 下重新创建（ghost row 由 ReconcilePresence 清理）
- **HandleRegister topology mirror best-effort**：失败 Warn；topology Migrate boot 时兜底
- **HandleRegister 每步 UpdateHostFacts**：保持最新 facts；不依赖 FindOrCreateByFingerprint 的 seed 字段（仅初次创建写入）
- **HandleHeartbeat 必须同步 device last_seen**：否则 device 列表 staleness 误报
- **Delete 级联删 device**：仅当 last live edge；否则 device mark offline 保留
- **ClearHostDeviceLink 用于 controller edge**：host edge 不应调；会误删 host device 关联
- **accessKeyEntropyBytes=18 / secretKeyEntropyBytes=24**：base64url 后 24/32 chars；熵足够
- **name 空允许**：HandleRegister 用 hostname 回填；SPA 显示"(待主机上线)"占位
- **agentVersion 空不写**：保留最后已知好版本；操作员 audit drift 用
- **pluginHealth lazy init**：零值 Usecase 可用；首次 RecordPluginHealth 时 init map
