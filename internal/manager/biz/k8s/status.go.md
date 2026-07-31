# `status.go` 技术实现文档

> 源文件：`internal/manager/biz/k8s/status.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/k8s`

## 1. 概述

本文件计算 K8s cluster 的有效状态：基于 LastSeenAt + InventorySyncedAt 判定 online/offline。设计要点：online 状态有 90s TTL（超时降级 offline）；last activity 取 LastSeenAt 和 InventorySyncedAt 的较新者。红线：非 online 状态原样返回，不做 TTL 降级。

## 2. 包信息

- **包名**：`k8s`
- **所属模块**：`internal/manager/biz/k8s`
- **依赖方向**：被同包 usecase.go 多处调用；依赖 `model/k8s`

## 3. 关键类型与接口

```go
const ClusterOnlineTTL = 90 * time.Second
```

## 4. 关键函数与流程

### `EffectiveClusterStatus`
- **签名**：`func EffectiveClusterStatus(c *model.Cluster, now time.Time) string`
- **职责**：计算 cluster 有效状态
- **流程**：
  1. c nil → 返回 ""
  2. `status := TrimSpace(c.Status)`；status != online → 原样返回（非 online 不降级）
  3. `last := ClusterLastActivityAt(c)`；last nil → offline（无活动记录）
  4. `now.Sub(last.UTC()) > ClusterOnlineTTL` → offline（90s 超时）
  5. 否则返回 online
- **错误处理**：无 error，纯计算

### `ClusterLastActivityAt`
- **签名**：`func ClusterLastActivityAt(c *model.Cluster) *time.Time`
- **职责**：返回最近活动时间（LastSeenAt 和 InventorySyncedAt 的较新者）
- **流程**：
  1. c nil → nil
  2. out = LastSeenAt（可能 nil）
  3. InventorySyncedAt 非 nil 且（out nil 或 InventorySyncedAt.After(out)）→ out = InventorySyncedAt
  4. 返回 out

## 5. 依赖关系

- **内部包**：`model/k8s`（Cluster/ClusterStatusOnline/ClusterStatusOffline）
- **外部库**：仅标准库
- **被调用方**：usecase.go（CreateCluster/DeleteCluster/ReconcileTopology/IngestInventory 等）

## 6. 并发与资源管理

- **纯函数**：无共享状态，线程安全
- **无 ctx**：纯计算

## 7. 设计模式与亮点

- **TTL 降级**：online 状态 90s 超时降级 offline，避免心跳中断后状态卡 online
- **双时间源**：LastSeenAt（controller heartbeat）+ InventorySyncedAt（inventory 同步），取较新者
- **非 online 不降级**：offline/error 等状态原样返回，不做 TTL 处理
- **nil 安全**：c nil / 时间 nil 均处理

## 8. 注意事项

- **ClusterOnlineTTL=90s**：online 超时阈值；controller heartbeat 间隔应远小于此
- **LastSeenAt**：controller 心跳更新（HandleControllerHeartbeat）
- **InventorySyncedAt**：inventory 同步更新（IngestInventory）
- **UTC 统一**：时间比较前转 UTC，防时区问题
- **非 online 不降级**：offline/error 状态原样返回，仅 online 走 TTL
