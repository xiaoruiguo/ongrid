# `draft_store_memory.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/alertconfig/draft_store_memory.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/alertconfig`

## 1. 概述

本文件实现 `memoryAlertRuleDraftStore` —— 告警规则草案的**进程内内存存储 + apply lease（租赁）机制**。配合 `alert_rule_manager.go` 的两阶段流程（draft→apply），保证只有合法签发的草案、且只有持有者本人才能被 apply；通过 `applying` 标记位防并发重复 apply；通过 TTL 过期清理防止内存无限增长。lease 模式支持 commit（成功提交，删除草案）/ rollback（失败回滚，释放 applying 标记，允许重试）。

## 2. 包信息

- **包名**：`alertconfig`
- **所属模块**：`internal/manager/biz/aiops/alertconfig`
- **依赖方向**：被 `alert_rule_manager.go` 持有调用；依赖 `aiops/tools`（`ConfigCaller`、`AlertRuleConfigInput`、`AlertRuleConfigDraftHashForID`）、`internal/pkg/errs`

## 3. 关键类型与接口

```go
type memoryAlertRuleDraftStore struct {
    mu       sync.Mutex
    records  map[string]alertRuleDraftRecord   // draftID → 记录
    applying map[string]bool                    // draftID → 是否正在 apply
    nowFn    func() time.Time                   // 时间注入（测试可控）
}

type memoryAlertRuleDraftApplyLease struct {
    store   *memoryAlertRuleDraftStore
    draftID string
}
```

- `memoryAlertRuleDraftApplyLease` 实现租赁接口（外部由 `alert_rule_manager.go` 定义 lease interface，提供 `commit()` / `rollback()` 方法）。
- `alertRuleDraftRecord` 类型由 `alert_rule_manager.go` 定义（含 ID/Hash/Action/UserID/ExpiresAt）。

## 4. 关键函数与流程

### `newMemoryAlertRuleDraftStore`
- **签名**：`func newMemoryAlertRuleDraftStore(nowFn func() time.Time) *memoryAlertRuleDraftStore`
- **职责**：构造函数；nowFn nil → 回退 `time.Now`
- **流程**：初始化 records + applying 两个 map + nowFn

### `put`
- **签名**：`func (s *memoryAlertRuleDraftStore) put(rec alertRuleDraftRecord)`
- **职责**：保存/覆盖一条草案记录；同时清除该 ID 上的 applying 标记（允许重新 beginApply）
- **流程**：
  1. nil receiver → no-op
  2. 加锁 → 先 `cleanupExpiredLocked`（懒清理过期项）→ 写入 records[id] → delete(applying, id)

### `beginApply`
- **签名**：`func (s *memoryAlertRuleDraftStore) beginApply(caller, action, rule, draftID, draftHash string) (alertRuleDraftApplyLease, error)`
- **职责**：申请草案 apply 租赁；多重校验通过后置 applying 标记
- **流程**：
  1. nil store → `errs.ErrInvalid`
  2. draftID 空 → `errs.ErrInvalid: draft_id from config_draft payload is required`
  3. draftHash 空 → `errs.ErrInvalid: draft_hash from config_draft is required`
  4. **hash 重算校验**：`AlertRuleConfigDraftHashForID(action, rule, draftID)` 重算并 EqualFold 比对；不匹配 → `errs.ErrInvalid: draft_hash does not match config_draft payload`（防 LLM 篡改 payload）
  5. 加锁查 records[draftID]：
     - 不存在 → `errs.ErrInvalid: config_draft was not issued by this server or was already applied`（防止跨实例 / 已 apply 后重复）
     - 已过期 → 删除 + `errs.ErrInvalid: config_draft expired`
     - UserID != caller.UserID → `errs.ErrForbidden: config_draft belongs to a different user`（多租户隔离红线）
     - Action 或 Hash 不匹配 → `errs.ErrInvalid: config_draft does not match the issued payload`
     - 已在 applying → `errs.ErrInvalid: config_draft is already being applied`（并发保护）
  6. 置 `applying[draftID]=true`，返回 lease

### `memoryAlertRuleDraftApplyLease.commit`
- **签名**：`func (l memoryAlertRuleDraftApplyLease) commit()`
- **职责**：apply 成功后调用；删除 records + applying 标记（一次性消费）

### `memoryAlertRuleDraftApplyLease.rollback`
- **签名**：`func (l memoryAlertRuleDraftApplyLease) rollback()`
- **职责**：apply 失败后调用；只清除 applying 标记，保留 records（允许重试 beginApply）

### `cleanupExpiredLocked`
- **签名**：`func (s *memoryAlertRuleDraftStore) cleanupExpiredLocked(now time.Time)`
- **职责**：在持锁状态下扫描所有 records，删除 ExpiresAt <= now 的项（同步删除对应 applying 标记）

### `now / expiresAt`
- 时间访问器：`now()` 走 nowFn（缺省 `time.Now`）；`expiresAt(ttl)` = now + ttl

## 5. 依赖关系

- **内部包**：
  - `internal/manager/biz/aiops/tools`（`ConfigCaller`、`AlertRuleConfigInput`、`AlertRuleConfigDraftHashForID`）
  - `internal/pkg/errs`（`ErrInvalid`、`ErrForbidden`）
- **外部库**：仅标准库 `sync`、`time`、`strings`、`fmt`

## 6. 并发与资源管理

- **`mu` sync.Mutex**：保护 records + applying 两个 map；所有读写路径均持锁
- **lease 不持有 mu**：lease.commit/rollback 通过 store.mu 自行加锁，调用方无需嵌套
- **懒清理策略**：每次 put 触发一次 cleanupExpiredLocked，避免后台 goroutine
- **TTL 过期**：草案记录由 alert_rule_manager 写入时设 ExpiresAt（draft TTL=30min，见 alert_rule_manager.go.md）
- **无 ctx 参数**：所有方法不接受 context（纯内存操作，无 IO）

## 7. 设计模式与亮点

- **租赁（Lease）模式**：beginApply 申请 → commit/rollback 释放，对应分布式两阶段提交语义，但纯进程内实现简化
- **三重校验**：draftID 存在性 + UserID 匹配 + Hash 重算，保证只有签发方本人 + payload 未被篡改才能 apply
- **Hash 重算防御**：即使 LLM 试图伪造 draftID + draftHash 对，重算 `AlertRuleConfigDraftHashForID(action, rule, draftID)` 会因 rule 内容不匹配而失败
- **applying 标记防并发**：同一个 draftID 在 apply 进行中不能被二次 beginApply
- **rollback 不删 records**：失败后允许重试；只有 commit 才真正消费草案
- **EqualFold 比较 Hash**：容忍 hash 字符串大小写差异（hex 编码可能大小写不一）
- **nowFn 注入**：测试可注入虚拟时钟验证 TTL 过期逻辑

## 8. 注意事项

- **进程内存储**：重启丢失所有草案；多实例部署时草案不可跨实例 apply（beginApply 会因 records[draftID] 不存在报 "not issued by this server"）
- **无持久化**：仅适用单实例 / sticky-session 场景；分布式场景需替换为 Redis 实现
- **懒清理不保证及时**：若长时间无 put 调用，过期草案仍驻留内存；高负载下可考虑加后台 ticker
- **UserID 比对是租户隔离红线**：违反此检查会导致跨用户 apply 草案，AGENTS.md 多租户强制条款
- **commit/rollback 均幂等**：store 为 nil 或 draftID 不存在均安全 no-op
- **`AlertRuleConfigDraftHashForID` 是核心信任锚**：哈希算法变更会 invalidate 所有现存草案（需配套版本号）
