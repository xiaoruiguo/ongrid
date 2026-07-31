# `dedup.go` 技术实现文档

> 源文件：`internal/manager/biz/imbridge/dedup.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/imbridge`

## 1. 概述

本文件实现 `dedupSet`——一个有界、并发安全的 "近期已见 event key" 集合。它存在的根本原因是 long-poll provider（如 Telegram getUpdates）会重新投递尚未 ack 的 update：poll offset 存在于 `StreamClient` 中，supervisor 每次 reconnect 都从 offset 0 重建，于是 tunnel 抖动后刚到的 batch 会被 Telegram 重新发回，导致 agent 被运行两次、回复被发两遍。bridge（进程级 singleton）通过 event id 跨 reconnect 去重。去重采用两代 map（cur + prev）实现有界容量，O(1) 插入、无 per-entry timestamp；仅内存，进程重启后重置（与 Telegram offset 一起，unacked backlog 可能 reprocess，但 restart 比 reconnect 罕见得多）。

## 2. 包信息

- **包名**：`imbridge`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 `bridge.go`（`Bridge.seen` 字段，`HandleInbound` 中调用 `seenOrAdd`）使用；仅依赖标准库 `sync`

## 3. 关键类型与接口

```go
// dedupSet 是有界、并发安全的近期已见 event key 集合。
// 两代 map（cur + prev）实现有界：cur 满 → cur 变 prev，新 cur 起步。
// 保留 cap 到 2*cap 最近 key，O(1) 插入、无 per-entry timestamp。
// 仅内存——进程重启重置（与 Telegram offset 一起）。
type dedupSet struct {
    mu   sync.Mutex
    cap  int
    cur  map[string]struct{}
    prev map[string]struct{}
}
```

Sentinel：`newDedupSet(capacity int)` 中 `capacity < 1` 强制为 1；bridge 中实例化容量为 `2048`（见 `bridge.go` `newDedupSet(2048)`）。

## 4. 关键函数与流程

### `newDedupSet`
- **签名**：`func newDedupSet(capacity int) *dedupSet`
- **职责**：构造 dedupSet；capacity 下限保护
- **流程**：
  1. `capacity < 1` → `capacity = 1`（防 0 或负数导致 cur 永远满、每次插入都 rotate 的退化行为）
  2. 返回 `&dedupSet{cap: capacity, cur: make(map[string]struct{}, capacity), prev: make(map[string]struct{})}`
- **设计意图**：`cur` 预分配 capacity 桶；`prev` 空分配（首次 rotate 后才有内容）

### `seenOrAdd`
- **签名**：`func (d *dedupSet) seenOrAdd(key string) bool`
- **职责**：key 已记录返回 true（caller 视为 "duplicate, skip"）；否则记录 key 返回 false
- **流程**：
  1. `d.mu.Lock()` + `defer Unlock()`
  2. `_, ok := d.cur[key]`；ok → return true（cur 命中）
  3. `_, ok := d.prev[key]`；ok → return true（prev 命中）
  4. `len(d.cur) >= d.cap` → **rotate**：`d.prev = d.cur`；`d.cur = make(map[string]struct{}, d.cap)`（旧 cur 整体变 prev，旧 prev 被丢弃）
  5. `d.cur[key] = struct{}{}` 插入
  6. return false
- **错误处理**：无错误返回；纯内存操作
- **语义**：注释明示 "The caller treats a true result as 'duplicate, skip'"

## 5. 依赖关系

- **标准库**：`sync`（`sync.Mutex`）
- **被调用方**：`bridge.go` 的 `Bridge.seen` 字段；`HandleInbound` 中 `b.seen.seenOrAdd(key)` 调用

## 6. 并发与资源管理

- **`sync.Mutex`**：保护 cur + prev 两代 map；`seenOrAdd` 全程持锁（O(1) 操作，持锁时间极短）
- **无 per-entry timestamp**：用两代 map 隐式表达 "最近"——cur 是当前代，prev 是上一代；rotate 时 prev 被整体丢弃
- **有界容量**：cur 容量 = cap；prev 容量无上限但实际 ≤ cap（因为 prev 是上一次的 cur）；总体保留 cap 到 2*cap 个 key
- **无 goroutine**：纯同步结构，无后台清理
- **进程级生命周期**：dedupSet 随 Bridge singleton 存活；进程重启后重置

## 7. 设计模式与亮点

- **两代 map 有界去重**：注释明示 "two-generation map: when cur fills, it becomes prev and a fresh cur starts"——O(1) 插入、无 per-entry timestamp、保留 cap 到 2*cap 最近 key
- **隐式时间语义**：通过代际替换表达 "最近"，无需 timestamp；rotate 时旧 prev 丢弃相当于 GC
- **mark-on-entry 语义**：caller（bridge）在 entry 时调用 `seenOrAdd` 而非 success 时——duplicate reply 比漏 run 一条已开始的消息更糟（见 bridge.go 注释）
- **capacity 下限保护**：`capacity < 1` 强制为 1，避免 0 或负数导致 cur 永远满、每次插入都 rotate 的退化行为
- **`cur` 预分配 capacity 桶**：`make(map[string]struct{}, capacity)` 减少首次填充时的 rehash
- **`prev` 初始空分配**：首次 rotate 后才有内容，避免预分配浪费
- **纯内存、进程级**：注释明示 "in-memory only — a full manager restart resets it (and the Telegram offset)"；restart 比 reconnect 罕见得多，reprocess unacked backlog 可接受
- **key 由 caller 构造**：dedupSet 不关心 key 语义；bridge 用 `provider:appID:eventID` 三段 key，覆盖同一 event 在不同 app/provider 下的独立性
- **无后台 goroutine**：与 LRU/TTL cache 不同，无需后台清理；rotate 在插入时惰性触发
- **`struct{}{}` value**：零字节 value，内存占用仅 key 字符串本身

## 8. 注意事项

- **仅内存、进程级**：进程重启后 dedupSet 重置，与 Telegram offset 一起；unacked backlog 可能 reprocess——注释明示 "a restart can still reprocess the unacked backlog; that's a far rarer event than a reconnect"
- **容量 2048（bridge 中实例化）**：见 `bridge.go` `newDedupSet(2048)`；保留 2048-4096 最近 event_id；高流量群组可能需要调大
- **两代 map 语义**：cur 满 → cur 变 prev，新 cur 起步；**旧 prev 被整体丢弃**，相当于 GC 一代；最旧 1/2 key 可能被重新视为新（如果 cap 刚好满且 key 在 prev 中已被丢弃）——但概率极低，且 reconnect batch 通常 < cap
- **无 per-entry timestamp**：用代际替换表达 "最近"，无法精确控制保留时长；适合 "近期去重" 语义，不适合 "TTL 缓存" 语义
- **`capacity < 1` 强制为 1**：防退化；caller 不应传 0 或负数
- **`seenOrAdd` 全程持锁**：O(1) 操作持锁时间极短；高频调用下无竞争压力
- **key 由 caller 构造**：dedupSet 不解析 key；bridge 用 `provider:appID:eventID`，避免不同 app 的同名 event_id 误判 duplicate
- **mark-on-entry 语义**：caller 在 entry 时调用，**非 success 时**；duplicate reply 比漏 run 更糟
- **无持久化**：不写 DB；进程重启即丢失；与 Telegram offset 一致，reconnect 时 offset 也从 0 开始
- **`struct{}{}` value 零字节**：内存占用仅 key 字符串；2048 key 约几十 KB，可忽略
- **rotate 是惰性的**：cur 满时插入才触发 rotate；无后台定时清理
- **`prev` 容量无上限**：实际 ≤ cap（prev 是上一次的 cur）；不会无限增长
