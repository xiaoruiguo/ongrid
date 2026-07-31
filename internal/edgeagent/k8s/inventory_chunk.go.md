# `inventory_chunk.go` 技术实现文档

> 源文件：`internal/edgeagent/k8s/inventory_chunk.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/k8s`

## 1. 概述

本文件负责把一次完整采集的 `inventorySnapshot` 切分为多个满足字节与样本约束的 chunk，每个 chunk 都是一个完整的 `tunnel.KubernetesInventoryRequest`，可独立通过 tunnel 推送。它是 full sync 推送路径的核心组件，确保单次 RPC 不会超过上游的体积上限。

## 2. 包信息

- **包名**：`k8s`
- **所属模块**：`internal/edgeagent/k8s`（edgeagent 域的 Kubernetes 适配层）
- **依赖方向**：被 `inventory.go` 的 `pushOnceWithSnapshot` 调用；依赖 `internal/pkg/tunnel` 的请求类型与 snapshot 模型（在 `inventory.go` 中定义）。

## 3. 关键类型与接口

```go
const (
    targetInventoryChunkBytes = 4 << 20  // 4 MiB 目标 chunk 大小（触发切分阈值）
    maxInventoryChunkBytes    = 8 << 20  // 8 MiB 硬上限（单 chunk 序列化后超过即报错）
)
```

无显著类型定义。仅使用闭包 `appendTo func(*tunnel.KubernetesInventoryRequest)` 完成对当前 chunk 字段的追加。

## 4. 关键函数与流程

### `buildInventorySnapshotChunks`
- **签名**：`func buildInventorySnapshotChunks(base tunnel.KubernetesInventoryRequest, snap *inventorySnapshot) ([]tunnel.KubernetesInventoryRequest, error)`
- **职责**：把 snapshot 切成多个可独立推送的 chunk。
- **流程**：
  1. 校验 `snap != nil`。
  2. `newInventorySnapshotID()` 生成 16 字节随机 hex 作为 snapshotID（同一批 chunk 共享）。
  3. 清空 base 的 `Nodes/Workloads/Pods/Events` 与 `ChunkIndex/ChunkCount`，设置 `SnapshotID`。
  4. `json.Marshal(base)` 计算 base（header）的编码字节数 `baseJSON`。
  5. 初始化 `chunks=[base]` 与 `sizes=[len(baseJSON)]`。
  6. 定义 `appendItem(encodedSize, appendTo)` 闭包：若当前 chunk 已有 items 且 `sizes[i]+encodedSize+1 > targetInventoryChunkBytes`，则新建一个 chunk（追加 base 副本，记录 size）；然后追加 item 到当前 chunk，累加 `encodedSize+1`（+1 是 JSON 数组逗号）。
  7. 依次遍历 `snap.nodes/workloads/pods/events`，对每个 item 调 `inventoryItemJSONSize`（即 `json.Marshal` 后取长度）后 `appendItem`。
  8. 最终回填每个 chunk 的 `ChunkIndex` 与 `ChunkCount`，并 `json.Marshal` 校验每个 chunk 编码后不超过 `maxInventoryChunkBytes`。
- **错误处理**：snapshot 为 nil、item marshal 失败、chunk 超硬上限都返回明确错误。

### `inventoryRequestHasItems`
- **签名**：`func inventoryRequestHasItems(req tunnel.KubernetesInventoryRequest) bool`
- **职责**：判断 chunk 是否已经包含任意 items，用于切分决策（避免产生空 chunk）。

### `inventoryItemJSONSize`
- **签名**：`func inventoryItemJSONSize(item any) (int, error)`
- **职责**：marshal 单个 item 取字节数，用于切分预算。每个 item 会被 marshal 两次（这里一次，最终 chunk marshal 时一次），是性能与正确性的权衡。

### `newInventorySnapshotID`
- **签名**：`func newInventorySnapshotID() (string, error)`
- **职责**：用 `crypto/rand` 生成 16 字节随机数，hex 编码为 32 字符字符串，作为一批 chunk 的唯一标识。
- **错误处理**：`rand.Read` 失败返回错误。

## 5. 依赖关系

- **内部包**：`github.com/ongridio/ongrid/internal/pkg/tunnel`（`KubernetesInventoryRequest` 及其内嵌的 `Kubernetes*Snapshot` 类型）。
- **外部库**：标准库 `crypto/rand`、`encoding/hex`、`encoding/json`、`fmt`。
- **被调用方**：`inventory.go` 的 `pushOnceWithSnapshot`。

## 6. 并发与资源管理

无并发控制。函数为纯函数式，无共享状态。`appendItem` 闭包捕获 `chunks` 与 `sizes` 切片，但仅在单 goroutine 内顺序调用。

## 7. 设计模式与亮点

- **预算式切分**：通过预计算每个 item 的 marshal 字节数，在追加前判断是否超出目标阈值，避免事后切分带来的复杂回退。
- **共享 header**：所有 chunk 共享同一份 base（含 edgeID/clusterID/scope/resourceVersion/snapshotID 等），上游可通过 `SnapshotID` 把多个 chunk 关联为同一批。
- **闭包封装切分逻辑**：`appendItem` 闭包隐藏了 chunk 切换的细节，主流程只需按资源类型顺序追加。
- **逗号预算**：`encodedSize+1` 的 `+1` 是 JSON 数组的逗号分隔符，体现了对 JSON 编码细节的精确 accounting。
- **双阈值保护**：`targetInventoryChunkBytes(4MiB)` 是软切分点，`maxInventoryChunkBytes(8MiB)` 是硬上限，防止超大 item 导致单 chunk 超限。

## 8. 注意事项

- **item 双重 marshal 的性能开销**：`inventoryItemJSONSize` 对每个 item 做一次 marshal 仅用于计数，最终 chunk marshal 时会再次 marshal。大集群下（数万 pod）这会带来一定 CPU 开销，可通过缓存 marshal 结果优化，但当前实现优先正确性与简洁性。
- **`maxInventoryChunkBytes` 硬上限**：若单个 item 编码后超过 8MiB（极少见，但理论上可能存在超大 ConfigMap-derived 数据），整个切分会失败返回错误。
- **`targetInventoryChunkBytes` 与 `maxInventoryChunkBytes` 未配置化**：常量硬编码，若上游调整了 chunk 大小限制，需要改代码重新编译。
- **snapshotID 随机性**：依赖 `crypto/rand`，在熵不足的容器环境下理论上可能阻塞，但 Linux 上 `/dev/urandom` 不会阻塞。
- **chunk 顺序**：按 nodes → workloads → pods → events 顺序填充，单个 chunk 内可能混合多种资源类型，上游需按 `ChunkIndex` 顺序处理。
