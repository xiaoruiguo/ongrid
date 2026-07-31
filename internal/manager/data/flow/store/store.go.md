# `store.go` 技术实现文档

> 源文件：`internal/manager/data/flow/store/store.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/flow/store`

## 1. 概述

本文件实现 `biz/flow.Repo` 与 `biz/flow.RunRepo` 的 GORM 落地，覆盖 flow 定义 CRUD、run 执行实例管理、node 节点记录、retention prune 与 stale sweep。核心设计：`PruneRuns` 删 finished run 前先删 node 行避免孤儿；`SweepStaleRunning` 把 pending/running run 标 failed 处理 manager 重启后的僵尸 run；`ListEnabled` 单查询全量启用 flow 供 scheduler 调度。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/flow`
- **依赖方向**：被 `internal/manager/biz/flow` 装配；依赖 `internal/manager/biz/flow`（接口）、`internal/manager/model/flow`、`internal/pkg/errs`、`gorm.io/gorm`。

## 3. 关键类型与接口

```go
// Repo implements biz/flow.Repo.
type Repo struct{ db *gorm.DB }

var _ biz.Repo = (*Repo)(nil)

// RunRepo implements biz/flow.RunRepo.
type RunRepo struct{ db *gorm.DB }

var _ biz.RunRepo = (*RunRepo)(nil)
```

## 4. 关键函数与流程

### Repo（flow 定义）

#### `NewRepo`
- **签名**：`func NewRepo(db *gorm.DB) *Repo`

#### `Create` / `Update` / `Get`
- `Create`：Create。
- `Update`：`Save(f)` 全字段更新。
- `Get`：按 PK 取；`gorm.ErrRecordNotFound` → `ErrNotFound`。

#### `List`
- **签名**：`func (r *Repo) List(ctx, limit, offset int) ([]*model.Flow, int64, error)`
- **职责**：返回 flows + total count，`id DESC` 排序。

#### `ListEnabled`
- **签名**：`func (r *Repo) ListEnabled(ctx) ([]*model.Flow, error)`
- **职责**：返回全部 `enabled = true` 的 flow，`id ASC` 排序。供 scheduler 调度。

#### `Delete`
- 软删 by id。`RowsAffected == 0` → `ErrNotFound`。

### RunRepo（执行实例）

#### `NewRunRepo`
- **签名**：`func NewRunRepo(db *gorm.DB) *RunRepo`

#### `CreateRun` / `UpdateRun` / `GetRun`
- `CreateRun`：Create。
- `UpdateRun`：`Save(run)` 全字段更新。
- `GetRun`：按 id（string）取；`gorm.ErrRecordNotFound` → `ErrNotFound`。

#### `ListRuns`
- **签名**：`func (r *RunRepo) ListRuns(ctx, flowID uint64, limit int) ([]*model.FlowRun, error)`
- **职责**：按 flow_id 列 run，`created_at DESC` 排序。`flowID > 0` 才加 Where，0 表示列全部。

#### `CreateNode` / `UpdateNode` / `ListNodes`
- `CreateNode`：Create。
- `UpdateNode`：`Save(n)` 全字段更新。
- `ListNodes`：按 run_id 列 node，`id ASC` 排序。

#### `PruneRuns`
- **签名**：`func (r *RunRepo) PruneRuns(ctx, before time.Time) (int64, error)`
- **职责**：删除 `created_at < before` 且 status NOT IN (Pending, Running) 的 run 及其 node 行，限制 flow_runs / flow_run_nodes 无限增长。返回删除 run 数。
- **流程**：
  1. Pluck 符合条件 run id 列表
  2. 空 → 返回 0
  3. 删 `flow_run_nodes WHERE run_id IN ?`
  4. 删 `flow_runs WHERE id IN ?`
- **关键约束**：**pending/running run 永不 prune**（可能仍在执行）；node 先删避免孤儿。

#### `SweepStaleRunning`
- **签名**：`func (r *RunRepo) SweepStaleRunning(ctx, reason string) (int64, error)`
- **职责**：把 `status IN (Pending, Running)` 的 run 标 `Failed` + error=reason。处理 manager 重启后的僵尸 run。
- **返回**：heal 行数。

## 5. 依赖关系

- **内部包**：`internal/manager/biz/flow`（接口）、`internal/manager/model/flow`、`internal/pkg/errs`
- **外部库**：`gorm.io/gorm`、`context`、`errors`、`time`
- **被调用方**：`internal/manager/biz/flow` usecase；scheduler（ListEnabled）；retention goroutine（PruneRuns）；启动期 SweepStaleRunning

## 6. 并发与资源管理

- **无显式锁**：依赖 gorm。
- **ctx 透传**：所有方法首参 ctx。
- **PruneRuns 非事务**：先删 node 再删 run，无事务包裹；崩溃 mid-prune 可能留下孤儿 run（无 node），但下次 prune 会清理。
- **SweepStaleRunning 启动期调用**：处理 manager 重启后僵尸 run。

## 7. 设计模式与亮点

- **PruneRuns 先删 node**：避免孤儿 node；非事务但幂等。
- **SweepStaleRunning 处理僵尸 run**：manager 重启后 pending/running run 永远不会完成，标 failed 让 UI 状态终态化。
- **ListEnabled 单查询全量**：scheduler 一次拉全部启用 flow，避免 N 次 per-flow 查询。
- **ListRuns flowID=0 列全部**：caller 不传 flowID 时列全部 run，灵活。
- **List + total 同返回**：避免分页 total 与实际行数不一致。

## 8. 注意事项

- **PruneRuns 非事务**：崩溃 mid-prune 可能留孤儿 run；下次 prune 清理。如需强一致需加事务。
- **PruneRuns 不删 pending/running**：可能仍在执行；retention 仅清 finished run。
- **SweepStaleRunning 启动期调用**：漏调用导致 UI 永久 spinner（与 alert investigation FailOrphaned 对称）。
- **UpdateRun / UpdateNode 用 Save**：全字段更新；caller 需传完整 row，避免部分更新丢字段。
- **ListRuns limit 由 caller 控制**：repo 不兜底默认值；caller 不传 limit 时 gorm 不加 LIMIT，可能拉全表。
- **GetRun id 是 string**：FlowRun.ID 是 string（可能是 UUID），与 Flow.ID（uint64）不同。
