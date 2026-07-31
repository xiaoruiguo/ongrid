# `alert_rule_manager.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/alertconfig/alert_rule_manager.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/alertconfig`

## 1. 概述

本文件实现 `AlertRuleManager`：协调告警规则的"草案 → 预览 → 应用"两阶段流程。LLM 调 `DraftAlertRuleConfig` 生成可校验草案（30 分钟 TTL，内存存储），用户在前端确认后调 `ApplyAlertRuleConfig` 落库。`AlertRulePort` 抽象底层告警子系统（database/scope/host），让 biz 层与具体告警后端解耦。

## 2. 包信息

- **包名**：`alertconfig`
- **所属模块**：`internal/manager/biz/aiops/alertconfig`
- **依赖方向**：被 chatruntime（draft_config_change tool handler）调用；依赖 alertdraft 编译器、`AlertRulePort` 接口实现

## 3. 关键类型与接口

```go
type DraftAlertRuleConfig struct {
    // LLM 提供的原始输入（rule kind/conditions/spec + scope + action）
    // ...
}

type ApplyAlertRuleConfig struct {
    DraftID string
    UserID  uint64
    // ...
}

type AlertRulePort interface {
    // 创建/更新/删除/获取规则的底层接口
    // 由 alert biz 层实现
}

type AlertRuleManager struct {
    port  AlertRulePort
    store alertRuleDraftStore  // memory or future persistent store
    log   *slog.Logger
    // ...
}

const draftTTL = 30 * time.Minute
```

`alertRuleDraftStore` 接口在 `draft_store_memory.go` 实现，定义 `beginApply` / `commit` / `rollback` lease 语义。

## 4. 关键函数与流程

### `DraftAlertRuleConfig`
- **职责**：编译 LLM 输入为可校验草案，存入内存 store，返回 draft_id + 草案摘要供前端展示
- **流程**：
  1. 调 `alertdraft.CompileDraft(input)` 编译为 `CompiledDraft`
  2. 校验（参考 `draft_validation.go` 的 `validateAlertRuleDraft`）
  3. `store.Save(draft, draftTTL)` 写入内存
  4. 返回 `{draft_id, draft_hash, summary, preview_blocks}` 给 LLM

### `ApplyAlertRuleConfig`
- **职责**：用户确认后落库
- **流程**：
  1. `store.beginApply(draftID)` 取 lease（防止并发 apply）
  2. 调 `port.CreateRule` / `UpdateRule` 写入告警子系统
  3. 成功 → `store.commit(draftID)`；失败 → `store.rollback(draftID)` 释放 lease
  4. 返回落库结果

### `GetDraft / ListDrafts`
- 辅助查询接口，供前端轮询草案状态

## 5. 依赖关系

- **内部包**：`internal/manager/biz/aiops/alertdraft`（编译器）、`internal/manager/biz/alert`（`AlertRulePort` 实现方）
- **外部库**：无外部依赖

## 6. 并发与资源管理

- **draftTTL=30min**：草案在内存中 30 分钟自动过期，避免 LLM 长时间未确认导致内存泄漏
- **lease 模式**：`beginApply` 取 lease 防止用户双击 apply 导致重复落库；`commit`/`rollback` 释放
- **store 内部 sync.Mutex**：保护内存 map（详见 `draft_store_memory.go`）
- **AlertRuleManager 实例无状态**：所有 per-request 状态在 store + port 实现侧

## 7. 设计模式与亮点

- **两阶段提交**：draft + apply 分离，让用户在落库前看到 LLM 生成的精确规则预览，避免 LLM 误判直接造成生产事故
- **Port 抽象**：`AlertRulePort` 屏蔽底层告警子系统差异（database scope / host scope / global scope），方便切换后端
- **TTL 自动清理**：30 分钟过期，无需后台 GC goroutine，访问时惰性清理
- **Lease 防并发**：beginApply/commit/rollback 三段式，防止前端重复点击或 LLM 重复 apply
- **draft_hash 一致性**：apply 时校验 hash 与 draft 阶段一致，防止前端篡改 payload

## 8. 注意事项

- **草案仅存内存**：服务重启会丢失；当前为 MVP，未来可考虑 Redis 持久化（gospec Redis 红线：必须设 TTL）
- **30min TTL 需与前端协同**：前端应在草案生成后 30 分钟内引导用户确认
- **lease 超时未释放**：当前实现依赖 commit/rollback 显式释放；若 apply 中途 panic 可能死锁，未来应考虑 lease 自带 TTL
- **`AlertRulePort` 实现侧需多租户过滤**：tenant_id 由 port 实现注入，本层不感知
- **draft_id 应使用密码学随机**：防止可猜测 id 攻击他人草案
