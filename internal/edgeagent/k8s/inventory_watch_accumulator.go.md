# `inventory_watch_accumulator.go` 技术实现文档

> 源文件：`internal/edgeagent/k8s/inventory_watch_accumulator.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/k8s`

## 1. 概述

本文件实现 watch 事件的累积器与防抖逻辑：`inventoryWatchAccumulator` 把多个 watch goroutine 产出的 trigger 合并成单个待推送 trigger；`mergeInventoryWatchTrigger` 负责合并语义（含 fullResync 优先、upsert/delete 抵消、RV 取新、reason 累计 batch 计数）；`waitForWatchDebounce` 在主循环中做时间窗口防抖，避免高频事件打爆推送通道。

## 2. 包信息

- **包名**：`k8s`
- **所属模块**：`internal/edgeagent/k8s`（edgeagent 域的 Kubernetes 适配层）
- **依赖方向**：被 `inventory.go` 的 `Run` 主循环与 `watchResourceLoop` 调用；依赖本包 `inventory_delta.go` 的 `inventoryWatchTrigger` 类型与 key 函数。

## 3. 关键类型与接口

```go
// inventoryWatchAccumulator 累积待处理的 watch trigger
type inventoryWatchAccumulator struct {
    mu      sync.Mutex
    pending inventoryWatchTrigger
    wake    chan struct{} // 容量 1 的非阻塞通知 channel
}
```

## 4. 关键函数与流程

### `newInventoryAccumulator`
- **签名**：`func newInventoryWatchAccumulator() *inventoryWatchAccumulator`
- **职责**：构造 accumulator，`wake` channel 容量为 1（保证通知最多一次，多余的 add 不会堆积）。

### `add`
- **签名**：`func (a *inventoryWatchAccumulator) add(trigger inventoryWatchTrigger)`
- **职责**：把新 trigger 合并到 pending，并通知主循环。
- **流程**：
  1. trigger 为空（`isEmpty()`）直接返回。
  2. 加锁 `pending = mergeInventoryWatchTrigger(pending, trigger)`。
  3. 非阻塞 send `wake <- struct{}{}`（`default` 分支丢弃多余通知，因为 pending 已包含全部信息）。
- **并发**：锁内合并，锁外通知；通知 channel 容量 1 保证不会因 channel 满而阻塞 watch goroutine。

### `take`
- **签名**：`func (a *inventoryWatchAccumulator) take() inventoryWatchTrigger`
- **职责**：取出并清空 pending。加锁，返回 pending 后置零。

### `notifications`
- **签名**：`func (a *inventoryWatchAccumulator) notifications() <-chan struct{}`
- **职责**：暴露 wake channel 给主循环 select。

### `mergeInventoryWatchTrigger`
- **签名**：`func mergeInventoryWatchTrigger(current, next inventoryWatchTrigger) inventoryWatchTrigger`
- **职责**：合并两个 trigger 成一个。
- **流程**：
  1. **reason**：调 `mergeWatchReason` 合并，并带上累计 count。
  2. **observedAt**：取更早的（保留首次观察时间）。
  3. **count**：累加。
  4. **resourceVersion**：取更新的（`newerResourceVersion`）。
  5. **resourceVersions**：按 key 逐个取更新的 RV。
  6. **fullResync 短路**：若任一为 fullResync，结果强制 fullResync，清空所有具体 items/deleted*（full sync 不需要增量数据）。
  7. 否则 syncType=delta，对四种资源分别调 `mergeFinalInventoryOperations` 合并 upserts 与 deletes。
- **错误处理**：无错误返回；合并是纯函数式。

### `mergeWatchReason`
- **签名**：`func mergeWatchReason(current, next string, count int) string`
- **职责**：生成合并后的 reason。
- **流程**：取 current/next 中第一个不含 ` batch=N` 后缀的部分作为基名；count>1 时追加 ` batch=<count>`。
- **设计**：reason 仅用于日志可读性，不参与逻辑判断。

### `mergeFinalInventoryOperations[T, R]`
- **签名**：`func mergeFinalInventoryOperations[T any, R any](currentUpserts, nextUpserts []T, currentDeletes, nextDeletes []R, upsertKey func(T) string, deleteKey func(R) string) ([]T, []R)`
- **职责**：泛型合并同类资源的 upserts 与 deletes，处理抵消语义。
- **流程**：
  1. 维护 `upserts map[string]T` 与 `deletes map[string]R`。
  2. 顺序应用：currentUpserts → currentDeletes → nextUpserts → nextDeletes。
  3. **抵消规则**：upsert 时若该 key 在 deletes 中则从 deletes 删除；delete 时若该 key 在 upserts 中则从 upserts 删除。
  4. 最终按 key 排序输出 upserts 与 deletes 切片（排序保证输出稳定，便于测试与日志比对）。
- **泛型约束**：用两个类型参数 T（snapshot）与 R（ref），通过 key 函数解耦具体类型。

### `waitForWatchDebounce`
- **签名**：`func waitForWatchDebounce(ctx, accumulator, delay) (inventoryWatchTrigger, bool)`
- **职责**：在主循环中做防抖。
- **流程**：
  1. `take()` 取当前 pending 作为初始 trigger。
  2. `delay<=0` 直接返回。
  3. 启动 `time.NewTimer(delay)`。
  4. select 循环：
     - ctx.Done → 返回 (trigger, false)。
     - wake 到达 → 合并新 pending，重置 timer（重新计时 delay）。
     - timer.C → 返回 (trigger, true)。
- **资源管理**：defer 中 `timer.Stop()` 并 drain `timer.C` 防止泄漏；重置 timer 前也 Stop+drain。
- **语义**：自首次 take 后，若 delay 窗口内又有新事件，则继续合并并重新计时；直到 delay 窗口内无新事件才返回。

## 5. 依赖关系

- **内部包**：
  - 本包 `inventory_delta.go`：`inventoryWatchTrigger`、`inventorySyncFull`/`inventorySyncDelta`、`nodeSnapshotKey`/`workloadSnapshotKey`/`podSnapshotKey`/`eventSnapshotKey` 与对应 refKey 函数。
  - 本包 `inventory.go`：`newerResourceVersion`。
- **外部库**：标准库 `context`、`sort`、`strconv`、`strings`、`sync`、`time`。
- **被调用方**：`inventory.go` 的 `Run`（消费 `notifications()` + `waitForWatchDebounce`）与 `watchResourceLoop`（`add`）。

## 6. 并发与资源管理

- **多 producer 单 consumer**：多个 watch goroutine 调 `add`（producer），主循环单 goroutine 调 `take`/`waitForWatchDebounce`（consumer）。
- **`sync.Mutex` 保护 pending**：`add` 与 `take` 都加锁；`mergeInventoryWatchTrigger` 是纯函数，在锁内调用。
- **channel 容量 1**：`wake` channel 容量 1，`add` 用非阻塞 send（`select default`），保证 watch goroutine 不会因主循环未及时消费而阻塞。
- **timer 资源管理**：`waitForWatchDebounce` 中每次重置 timer 前都 Stop+drain，defer 也 Stop+drain，避免 timer 泄漏与重复触发。

## 7. 设计模式与亮点

- **accumulator + debounce 经典模式**：高频事件先入 accumulator 合并，主循环做时间窗口防抖，把 N 次事件压缩成 1 次推送。
- **泛型合并**：`mergeFinalInventoryOperations[T, R]` 用 Go 泛型统一四种资源的合并逻辑，消除重复代码。
- **抵消语义**：upsert 与 delete 互相抵消（先 upsert 后 delete 则最终为 delete；先 delete 后 upsert 则最终为 upsert），保证推送内容最小化。
- **排序输出**：合并后按 key 排序，使输出稳定可预测，便于测试断言与日志比对。
- **fullResync 短路**：任一 trigger 为 fullResync 时直接清空所有具体数据，因为 full sync 会重新拉全量，增量数据无意义。
- **非阻塞通知**：channel 容量 1 + 非阻塞 send 是经典的生产者-消费者解耦模式，producer 永不阻塞。
- **reason 可读性**：reason 带 `batch=N` 后缀，让日志能直观看到一次推送合并了多少个事件。

## 8. 注意事项

- **`waitForWatchDebounce` 的初始 take**：进入函数立即 take 一次，这意味着即使 delay=0 也会取走 pending；若主循环在 take 与 select 之间有新 add，会在 wake 分支合并，不会丢失。
- **debounce 延迟上限**：`waitForWatchDebounce` 没有最大延迟上限，若 watch 事件持续高频到达，timer 会不断重置，trigger 可能长时间无法返回。实际场景下 K8s watch 事件不会持续高频，且 `Run` 的 ticker 会兜底 full sync。但极端情况下可能延迟较久，可考虑加最大延迟保护。
- **`mergeFinalInventoryOperations` 的 map 分配**：每次合并都新建两个 map，高频合并下有一定 GC 压力；可考虑复用 map 优化，但当前实现优先正确性。
- **`take` 与 `notifications` 的竞态**：`take` 后 channel 中可能仍有未消费的 wake 信号（之前 add 时发送的），主循环下次 select 会立即触发，但 `take` 返回空 trigger——`waitForWatchDebounce` 的循环会处理这种情况（合并空 trigger 不影响结果）。这是有意设计，非 bug。
- **排序开销**：每次合并都对 upserts/deletes 排序，O(N log N)；单次推送的 items 数通常不大（watch 事件批量），可接受。
- **`mergeWatchReason` 的 reason 截断**：用 `strings.SplitN(current, " batch=", 2)[0]` 截掉旧 batch 后缀，假设 reason 本身不含 ` batch=` 字符串，实际 reason 由 `<spec.name>:<eventType>` 构成，不含该子串，安全。
