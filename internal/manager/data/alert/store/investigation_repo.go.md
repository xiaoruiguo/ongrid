# `investigation_repo.go` 技术实现文档

> 源文件：`internal/manager/data/alert/store/investigation_repo.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/alert/store`

## 1. 概述

本文件实现 `investigation_reports` 表的持久化层，支撑 alert RCA（根因分析）worker 的生命周期：创建报告 → 绑定 worker → 标记 ready / failed → 删除重触发。同时实现 `investigator.RelatedAlertQuerier` 接口，为 RCA 提供同设备时间窗内的关联 incident 列表。核心红线：worker 仅存活于 manager 进程内，启动时必须 `FailOrphaned` 把上一进程残留的 pending/running 报告全部标 failed，否则 SPA 永远停在"Spawning root-cause analysis worker…"。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/alert`
- **依赖方向**：被 `internal/manager/biz/alert/investigator` 装配；依赖 `internal/manager/biz/alert/investigator`（接口与 `ReadyFields`）、`internal/manager/model/alert`、`internal/pkg/errs`、`gorm.io/gorm`。

## 3. 关键类型与接口

```go
// InvestigationRepo 是 investigation_reports 的存储层。
// Operations match the biz interface in internal/manager/biz/alert/investigator.
type InvestigationRepo struct {
    db *gorm.DB
}
```

实现 `investigator.RelatedAlertQuerier`（通过 `RelatedToIncident`）以及 investigator repo 接口的其余方法。

## 4. 关键函数与流程

### `NewInvestigationRepo`
- **签名**：`func NewInvestigationRepo(db *gorm.DB) *InvestigationRepo`

### `RelatedToIncident`
- **签名**：`func (r *InvestigationRepo) RelatedToIncident(ctx, target *model.Incident, halfWindow time.Duration, limit int) ([]*model.Incident, error)`
- **职责**：实现 `investigator.RelatedAlertQuerier`，返回与 target 同设备、`last_fired_at` 落在 `[target.LastFiredAt ± halfWindow]` 内的 incident（排除自身与软删行），按 `last_fired_at DESC` 排序。
- **流程**：
  1. target nil → 返回 nil, nil
  2. halfWindow ≤ 0 → 默认 5 分钟
  3. limit ≤ 0 → 默认 10
  4. Where `id != target.ID` + `last_fired_at BETWEEN from AND to`
  5. target.DeviceID 非空 → 同设备过滤；为空 → `device_id IS NULL`（避免集群级噪音）
  6. Order + Limit + Find
- **限制**：当前仅同设备；topology-aware fan-out（depends_on / member_of 边）为 follow-up。

### `Create`
- **签名**：`func (r *InvestigationRepo) Create(ctx, rep *model.InvestigationReport) error`
- **职责**：插入报告。`incident_id` 唯一索引冲突 → 翻译为 `errs.ErrConflict`，caller 视为"已入队，跳过"。
- **错误处理**：`isDuplicateKey(err)` 检测 MySQL 1062 / SQLite UNIQUE constraint / "duplicate key" 三种字符串标记。

### `UpdateStatus`
- **签名**：`func (r *InvestigationRepo) UpdateStatus(ctx, id, status, reason string) error`
- **职责**：移动报告生命周期状态，可选 `status_reason` 记录"为什么 skipped / failed"。`RowsAffected == 0` → `ErrNotFound`。

### `FailOrphaned`
- **签名**：`func (r *InvestigationRepo) FailOrphaned(ctx, reason string) (int64, error)`
- **职责**：把所有 pending / running 报告标 failed。**必须在 manager 启动时调用一次**，因为 worker 仅存于 manager 进程内，前一进程的残留行永远不会被完成。返回 heal 行数。
- **类比**：与 edge 端"stale-online"启动回填对称。

### `ListIncidentsWithoutReport`
- **签名**：`func (r *InvestigationRepo) ListIncidentsWithoutReport(ctx, since time.Time, limit int) ([]uint64, error)`
- **职责**：返回 `[since, now)` 内触发但无任何报告的 incident id。支撑启动补偿：若 manager 曾在无 LLM provider 配置下运行，incident 自动调查被静默跳过（无报告行 → API 永远 status=not_started）；provider 配置后重启，此 list 让 backfill 通过正常 Enqueue 门控（severity / 并发上限 / dedup）重新入队。
- **实现**：LEFT JOIN `alert_incidents` 与 `investigation_reports`，`r.id IS NULL` 过滤；按 `first_fired_at DESC` 取最新，limit 限制 LLM 成本。

### `AttachWorker`
- **签名**：`func (r *InvestigationRepo) AttachWorker(ctx, id, workerID, auditSessionID string) error`
- **职责**：记录已 spawn 的 worker + 审计 session，让 SPA 在 worker 运行时即可深链到底层 transcript。同时把 status 翻为 running、清空 status_reason。

### `MarkReady`
- **签名**：`func (r *InvestigationRepo) MarkReady(ctx, id string, fields investigator.ReadyFields) error`
- **职责**：以报告生成器产出的所有结构化字段终结报告，`ready_at = now`。字段包括 root_cause / affected_window / pinpointed_target / related_alerts / evidence / suggested_actions / findings_md / confidence / confidence_factors / tool_call_count。

### `GetByIncident` / `Get`
- 按 incident_id 或 自身 id 取报告；`gorm.ErrRecordNotFound` → `errs.ErrNotFound`。`GetByIncident` 取最新一条（按 `created_at DESC`）。

### `DeleteByIncident`
- **签名**：`func (r *InvestigationRepo) DeleteByIncident(ctx, incidentID uint64) error`
- **职责**：删除绑定到 incident 的报告行。手动重触发路径使用，让新调查覆盖之前 failed / ready / stuck 的报告而不撞 `uniq_invreports_incident` 唯一索引。
- **关键约束**：必须 `Unscoped()` 强制硬 DELETE；否则 gorm 走软删（设 `deleted_at`），行仍对唯一索引可见，下次 Enqueue 的 INSERT 撞 Error 1062。注释明示"destructive by design"。

### `RecentlySpawnedFor`
- **签名**：`func (r *InvestigationRepo) RecentlySpawnedFor(ctx, ruleName string, deviceID *uint64, window time.Duration) (bool, error)`
- **职责**：检查 `(rule, device)` 对在 dedup 窗口内是否已有调查行。用于 enqueue 门控抑制 alert-storm 重复 spawn。
- **实现**：JOIN `alert_incidents`，按 `created_at >= cutoff` + rule + device 过滤后 Count。

### `isDuplicateKey` / `contains`
- **签名**：`func isDuplicateKey(err error) bool` + `func contains(s, sub string) bool`
- **职责**：跨方言检测唯一键冲突。匹配 `Error 1062` / `UNIQUE constraint failed` / `duplicate key` 三种字符串。
- **限制**：字符串匹配脆弱，但跨 MySQL/SQLite 双方言的最简方案。

## 5. 依赖关系

- **内部包**：`internal/manager/biz/alert/investigator`（接口、`ReadyFields`）、`internal/manager/model/alert`、`internal/pkg/errs`
- **外部库**：`gorm.io/gorm`
- **被调用方**：`internal/manager/biz/alert/investigator` usecase；`cmd/ongrid` 启动序列（FailOrphaned + ListIncidentsWithoutReport 补偿）

## 6. 并发与资源管理

- **无显式锁**：所有读写通过 GORM；incident_id 唯一索引 + isDuplicateKey 翻译保证 enqueue 幂等。
- **ctx 透传**：所有方法首参 ctx。
- **进程边界假设**：worker 仅存于 manager 进程内，故 FailOrphaned 启动期清扫是必须的。

## 7. 设计模式与亮点

- **启动期孤儿清扫**：`FailOrphaned` 把跨进程残留行终态化，避免 SPA 永久 spinner。
- **启动期补偿 backfill**：`ListIncidentsWithoutReport` 处理"manager 曾无 LLM provider"的静默跳过场景，provider 配置后重启自动补偿。
- **同设备时间窗关联**：`RelatedToIncident` 用 `±halfWindow` 简单窗口捕获多数操作上有用的共现（disk_high + swap_high、scrape_down 跟随 node_down）。
- **硬删 vs 软删策略分离**：`DeleteByIncident` 显式 `Unscoped()` 强制硬删以绕过唯一索引；其余删除走 gorm 默认软删保留审计。
- **跨方言 duplicate key 检测**：`isDuplicateKey` 字符串匹配 MySQL + SQLite，让 biz 层保持 storage-agnostic。
- **device_id NULL 处理**：`RelatedToIncident` 与 `RecentlySpawnedFor` 都显式处理 `device_id IS NULL`，避免集群级 incident 噪音。

## 8. 注意事项

- **FailOrphaned 必须启动期调用**：注释明示"Run once at manager startup"。漏调用会导致 SPA 永久 spinner。
- **`Unscoped()` 仅 DeleteByIncident 使用**：手动重触发路径专用；其余路径保留软删。
- **incident_id 唯一索引**：`DeleteByIncident` 是绕过唯一索引的唯一手段；新调查必须先删旧报告。
- **topology fan-out 未实现**：`RelatedToIncident` 当前仅同设备；依赖 depends_on / member_of 边的跨设备关联是 follow-up。
- **`isDuplicateKey` 字符串匹配**：新 DB 方言需扩展匹配字符串。
- **`RecentlySpawnedFor` 时间基准**：`time.Now().UTC()` 计算 cutoff；时钟漂移可能让 dedup 窗口略短或略长。
- **`MarkReady` 字段繁多**：扩展 ReadyFields 需同步更新此方法 updates map。
- **`ListIncidentsWithoutReport` limit 必填**：limit ≤ 0 返回 nil，限制 LLM backfill burst 成本。
