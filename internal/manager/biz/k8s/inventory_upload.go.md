# `inventory_upload.go` 技术实现文档

> 源文件：`internal/manager/biz/k8s/inventory_upload.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/k8s`

## 1. 概述

本文件管理 K8s inventory 上传的分片状态机：处理 delta / snapshot（单 chunk / 多 chunk）三种同步模式。设计要点：每个 cluster 同时只能有一个活跃 snapshot 上传，按 ChunkIndex 严格顺序校验；时间戳按毫秒截断统一精度。红线：delta 不能携带 snapshot 字段；chunk 乱序或 snapshot 切换返回 `ErrConflict`。

## 2. 包信息

- **包名**：`k8s`
- **所属模块**：`internal/manager/biz/k8s`
- **依赖方向**：被同包 `usecase.go` 的 `IngestInventory` 调用；依赖 `pkg/errs`、`pkg/tunnel`

## 3. 关键类型与接口

```go
const (
    maxInventorySnapshotIDLength = 128
    maxInventoryChunkCount       = 10000
    inventoryTimestampPrecision  = time.Millisecond
)

type inventoryUploadState struct {
    snapshotID string
    chunkCount int
    nextChunk  int
    startedAt  time.Time
}
```

## 4. 关键函数与流程

### `Usecase.prepareInventoryChunk`
- **签名**：`func (u *Usecase) prepareInventoryChunk(in tunnel.KubernetesInventoryRequest, receivedAt time.Time) (time.Time, bool, error)`
- **职责**：校验 chunk + 推进分片状态机；返回 (startedAt, isFinalChunk, err)
- **流程**：
  1. `receivedAt = receivedAt.UTC().Truncate(inventoryTimestampPrecision)` 统一毫秒精度
  2. **delta 模式**（`normalizeInventorySyncType(in.SyncType) == inventorySyncDelta`）：
     - 携带 snapshot 字段（SnapshotID/ChunkIndex/ChunkCount 非 0）→ `ErrInvalid`
     - 否则返回 (receivedAt, true, nil)
  3. **单 chunk snapshot**（SnapshotID 空）：
     - 携带 ChunkIndex/ChunkCount 非 0 → `ErrInvalid`
     - 否则返回 (receivedAt, true, nil)
  4. **多 chunk snapshot**：
     - SnapshotID 长度 > 128 → `ErrInvalid`
     - ChunkCount ∈ [1, 10000] + ChunkIndex ∈ [0, ChunkCount-1] 校验
     - `inventoryUploadsMu.Lock`；ChunkIndex==0 → 初始化 state
     - snapshotID/chunkCount/nextChunk 不匹配 → `ErrConflict`（stale 或乱序）
     - 返回 (state.startedAt, ChunkIndex == ChunkCount-1, nil)
- **错误处理**：delta/单 chunk 不持锁；多 chunk 持锁校验顺序

### `Usecase.completeInventoryChunk`
- **签名**：`func (u *Usecase) completeInventoryChunk(in tunnel.KubernetesInventoryRequest) error`
- **职责**：chunk 处理完成后推进 nextChunk 或清理 state
- **流程**：
  1. SnapshotID 空（delta/单 chunk）→ 直接返回 nil
  2. `inventoryUploadsMu.Lock`
  3. state 不存在 / snapshotID 不匹配 / nextChunk 不匹配 → `ErrConflict`（snapshot 切换）
  4. **最后 chunk**（ChunkIndex == ChunkCount-1）→ `delete(u.inventoryUploads, ClusterID)`
  5. 否则 `state.nextChunk++` 写回
- **错误处理**：snapshot 中途切换 → ErrConflict，让 caller 决定如何处理

## 5. 依赖关系

- **内部包**：`pkg/errs`（ErrInvalid/ErrConflict）、`pkg/tunnel`（KubernetesInventoryRequest）
- **外部库**：仅标准库
- **被调用方**：`usecase.go` 的 `IngestInventory`

## 6. 并发与资源管理

- **`inventoryUploadsMu`（Mutex）**：保护 `inventoryUploads` map；定义在 usecase.go 的 Usecase 结构
- **per-cluster state**：每个 cluster 一个 inventoryUploadState，互不干扰
- **锁粒度**：仅多 chunk 模式持锁；delta/单 chunk 无锁
- **state 生命周期**：ChunkIndex==0 创建，最后 chunk 删除；中途 snapshot 切换则残留（下次 ChunkIndex==0 覆盖）

## 7. 设计模式与亮点

- **三态分派**：delta / 单 chunk snapshot / 多 chunk snapshot，各有独立校验路径
- **严格顺序校验**：nextChunk 必须等于 ChunkIndex，防乱序/重放
- **snapshot 切换检测**：snapshotID 变化即 ErrConflict，避免新旧 snapshot chunk 混淆
- **时间戳统一精度**：毫秒截断，避免不同 chunk 时间戳不一致
- **delta 不持锁**：delta 无状态，直接处理
- **最后 chunk 清理**：避免 state 泄漏

## 8. 注意事项

- **maxInventorySnapshotIDLength=128**：snapshot ID 长度上限
- **maxInventoryChunkCount=10000**：单 snapshot chunk 数上限
- **inventoryTimestampPrecision=ms**：时间戳精度
- **delta 不能携带 snapshot 字段**：校验防配置错误
- **chunk 乱序/snapshot 切换返回 ErrConflict**：caller 决定是否重试
- **state 残留风险**：snapshot 中途切换不清理旧 state，下次 ChunkIndex==0 覆盖（可接受）
- **锁仅多 chunk**：delta/单 chunk 性能不受影响
