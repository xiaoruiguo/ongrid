# service.go

## 1. 概述

`service.go` 是 monitor 包的 biz 层编排器 —— 操作员通过 SPA 创建/编辑/删除监控面板，`Service` 持久化到 DB 后异步镜像到单个 ongrid 管理的 Grafana dashboard。

镜像单向（ongrid → Grafana），Grafana 内的直接编辑会被下次镜像推送覆盖。这是有意的：单一数据源、无 merge 冲突、匹配操作员心智模型（"在 ongrid 建，在 Grafana 看"）。

API 契约：API 调用在行持久化后立即返回 200；Grafana 镜像 fire-and-forget 后台 goroutine；失败记到 `last_sync_error` 列让 UI 显示"synced/failed"，永不阻塞操作员动作。

## 2. 包信息

- 包名：`monitor`
- 路径：`internal/manager/biz/monitor`
- 包注释：明确单向镜像 + fire-and-forget + last_sync_error 三个不变量

## 3. 关键类型与接口

### Repo 接口

```go
type Repo interface {
    List(ctx) ([]*model.Panel, error)
    Get(ctx, id) (*model.Panel, error)
    MaxOrdinal(ctx) (int, error)
    Create(ctx, p *model.Panel) (*model.Panel, error)
    Update(ctx, id uint64, fields map[string]any) (*model.Panel, error)
    SetSyncResult(ctx, id, errMsg string) error
    Delete(ctx, id) error
}
```

### GrafanaSyncer 接口

```go
type GrafanaSyncer interface {
    SyncMonitorPanels(ctx, panels []*model.Panel) error
}
```

`*biz/grafana.Service` 通过 `SyncMonitorPanels` 方法结构性满足。可选：nil 时降级为"仅持久化不镜像"（测试 + 不部署 Grafana 的场景）。

### Service

```go
type Service struct {
    repo   Repo
    syncer GrafanaSyncer
    log    *slog.Logger
    syncTO time.Duration  // 默认 30s
}
```

### CreateInput / UpdateInput

```go
type CreateInput struct {
    Title, Type, PromQL, Legend, Unit string
    Ordinal *int  // 空 = max(ordinal)+1
}

type UpdateInput struct {
    Title, Type, PromQL, Legend, Unit *string  // nil = 不改
    Ordinal *int
}
```

UpdateInput 用指针字段区分"设为空串"与"不动"。注释明确这是 plain string 做不到的。

## 4. 关键函数与流程

### New

构造 + syncTO = 30s。注释：30s 给单次 Grafana POST /api/dashboards/db 足够时间；超出几乎必是 Grafana 挂了，不应让慢上游 pin goroutine 永远。

### List / Get

直接委派 repo。

### Create

1. trim 所有字段
2. 校验：`title` 非空、`promql` 非空、`type` 默认 `Timeseries` + `ValidPanelType` 检查
3. `Ordinal`：传值用传值；空则 `MaxOrdinal + 1`（新面板落末尾）
4. `repo.Create(ctx, row)`
5. `kickSync("create", saved.ID)` 异步镜像

### Update

1. `id == 0` 校验
2. 遍历 UpdateInput 字段，非 nil 的字段：
   - trim 后校验（title/promql 非空、type 合法）
   - 放入 `fields map[string]any`
3. `repo.Update(ctx, id, fields)`
4. `kickSync("update", id)`

### Delete

1. `repo.Delete(ctx, id)`
2. `kickSync("delete", id)` —— goroutine 内重新 List 取 post-delete 状态

### SyncNow

同步镜像（非异步）：`repo.List` + `syncer.SyncMonitorPanels`。boot 时调用，让 ongrid-monitor dashboard 在操作员第一次编辑前就存在（至少含核心 fleet panels）。

### kickSync

```go
func (s *Service) kickSync(op string, panelID uint64)
```

detached goroutine：
1. `syncer == nil` 直接返回（degraded 模式）
2. `context.WithTimeout(context.Background(), syncTO)` —— 用 fresh context 不继承请求 ctx
3. `repo.List(ctx)` 取最新全量 panels
4. `syncer.SyncMonitorPanels(ctx, panels)`：
   - 失败 → warn log + `repo.SetSyncResult(ctx, panelID, truncateErr(err))` 持久化错误到触发面板
   - 成功 → 遍历所有 panel 清空 `last_sync_error`（"all green" 信号）

### truncateErr

`err.Error()` 截断到 480 字符 + "…"，适配 `last_sync_error` 列的 512 字符上限。

## 5. 依赖关系

### 外部包

- `context` / `errors` / `fmt` / `log/slog` / `strings` / `time`

### 内部包

- `model "github.com/ongridio/ongrid/internal/manager/model/monitor"` —— `Panel` / `PanelTypeTimeseries` / `ValidPanelType`
- `github.com/ongridio/ongrid/internal/pkg/errs` —— `ErrInvalid`

### 被谁实现

- `Repo`：`internal/manager/data/monitor/store`
- `GrafanaSyncer`：`biz/grafana.Service` 的 `SyncMonitorPanels` 方法

### 被谁调用

- HTTP handler（`/v1/monitor/panels`）调 CRUD
- `cmd/ongrid` boot 调 `SyncNow` 初始化 Grafana dashboard

## 6. 并发与资源管理

- **detached goroutine**：`kickSync` 起 goroutine，不阻塞 API
- **fresh context with timeout**：goroutine 用 `context.WithTimeout(context.Background(), syncTO)`，不继承请求 ctx（请求返回后 ctx 取消不能杀镜像）
- **无锁**：Service 无共享可变状态；并发安全由 repo 保证
- **`syncTO` 30s 上限**：防 stuck Grafana pin goroutine 永远

## 7. 设计模式与亮点

### Fire-and-forget 镜像

API 在行持久化后立即返回，镜像在 detached goroutine 跑。操作员动作永不因 Grafana 慢/挂而阻塞。失败记 `last_sync_error` 让 UI 显示。

### 单向镜像

ongrid → Grafana 单向。Grafana 内编辑被下次推送覆盖。注释明确：单一数据源、无 merge 冲突、匹配操作员心智模型。

### 指针字段区分 nil vs 空串

`UpdateInput` 用 `*string` 字段：nil = 不动，非 nil = 改（含设为空串）。注释说明这是 plain string 做不到的。这是 PATCH 语义的正确实现。

### MaxOrdinal 自动落末尾

`Create` 时 `Ordinal` 空 → `MaxOrdinal + 1`。新面板自动落到列表末尾，操作员不必手填序号。

### 成功时清空所有 last_sync_error

镜像成功后遍历所有 panel 清空 `last_sync_error`。注释："cheap (one UPDATE per row, but the table is tiny) and gives the UI a deterministic 'all green' signal"。

### SyncNow boot 初始化

boot 时调 `SyncNow` 让 dashboard 在第一次编辑前就存在（至少含核心 fleet panels）。best-effort：返回 syncer error 让调用方 log，boot 时 Grafana hiccup 非致命。

### truncateErr 列宽适配

`last_sync_error` 列 512 字符上限，`truncateErr` 截到 480 + "…"。留余量给"…"和编码。

## 8. 注意事项

- **Grafana 编辑被覆盖**：操作员若在 Grafana 内直接改 dashboard，下次 ongrid 镜像推送会覆盖。这是有意的单向模型
- **`kickSync` 用 fresh context**：不继承请求 ctx，否则请求返回后 ctx 取消会杀镜像。注释明确
- **`syncTO` 30s 硬编码**：不可配。若 Grafana 慢应调大，但太大会让 goroutine 堆积
- **`SetSyncResult` 失败只 debug log**：持久化 sync 错误本身失败不阻断，只 log。这是合理的 —— 已无法再做什么
- **`kickSync` goroutine 无 limit**：每次 Create/Update/Delete 都起 goroutine。高频编辑会起多个 goroutine，但每个有 30s timeout，最坏堆积有限
- **`Update` 空更新触发 re-sync**：所有字段 nil 的 Update 仍会 kickSync。注释："harmless"
- **`SyncNow` 不持久化错误**：与 `kickSync` 不同，`SyncNow` 失败只返回 error 给调用方 log，不写 `last_sync_error`。boot 时 panel 可能还没创建
- **`compile-time guard`**：文件末尾 `var _ = errors.Is` 是注释提到的"keep standard sentinels importable"。看似无用但确保 `errors` import 不被 goimports 删
- **`UpdateInput` 不含 ID**：ID 是 URL path 参数，handler 传给 `Update(ctx, id, in)`
