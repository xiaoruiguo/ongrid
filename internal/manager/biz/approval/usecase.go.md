# `usecase.go` 技术实现文档

> 源文件：`internal/manager/biz/approval/usecase.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/approval`

## 1. 概述

本文件是人工审批收件箱（HLD-017）的 biz 层门面。生产者（agent cloud-shell、restart_service、flow 审批节点）通过 `Propose` 排入待审批的危险动作；人工在 UI 中 `Approve`/`Reject`；`Approve` 时按 Kind 查找注册的 Executor 执行动作并回写结果。红线：严格 additive —— 任何动作只有显式 propose 后才会执行；Executor 缺失时仅置 approved 而不执行（safe）。

## 2. 包信息

- **包名**：`approval`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 cloud-shell / flow 审批节点 / HTTP handler 调用；依赖 `model/approval`、`pkg/errs`

## 3. 关键类型与接口

```go
type Repo interface {
    Create(ctx, a *model.Approval) error
    Get(ctx, id string) (*model.Approval, error)
    List(ctx, status string, limit int) ([]*model.Approval, error)
    CountPending(ctx) (int64, error)
    Decide(ctx, id string, fields map[string]any) error
    SetResult(ctx, id, status, resultJSON string, executedAt time.Time) error
}

type Executor func(ctx context.Context, payloadJSON string) (resultJSON string, err error)

type Usecase struct {
    repo      Repo
    log       *slog.Logger
    executors map[string]Executor
}

type ProposeInput struct {
    Kind, Title, Summary string
    Payload any
    Source, SessionID string
    ProposedBy uint64
}
```

## 4. 关键函数与流程

### `NewUsecase`
- **签名**：`func NewUsecase(repo Repo, log *slog.Logger) *Usecase`
- **职责**：构造 Usecase；log nil → `slog.Default()`；初始化空 executors map
- **流程**：log 兜底；返回 `&Usecase{repo, log, map[string]Executor{}}`

### `RegisterExecutor`
- **签名**：`func (u *Usecase) RegisterExecutor(kind string, fn Executor)`
- **职责**：按 Kind 注册 execute-on-approve 处理器；启动时由 producer 子系统调用
- **流程**：直接 `u.executors[kind] = fn`；幂等（覆盖写）

### `Propose`
- **签名**：`func (u *Usecase) Propose(ctx, in ProposeInput) (*model.Approval, error)`
- **职责**：记录一条 pending 动作（producer-facing，非 admin-gated）
- **流程**：
  1. 校验 Kind + Title 非空 → `errs.ErrInvalid`
  2. `json.Marshal(in.Payload)`
  3. Source 空回退 `model.SourceAgent`
  4. 构造 `model.Approval{Status: StatusPending, ...}`
  5. `repo.Create`
  6. Info log
- **错误处理**：空 Kind/Title → ErrInvalid；marshal 失败直接返回；Create 失败透传

### `Approve`
- **签名**：`func (u *Usecase) Approve(ctx, approverID uint64, id string) (*model.Approval, error)`
- **职责**：标记 approved；若注册了 Executor 则执行动作并回写结果
- **流程**：
  1. `repo.Decide` 置 status=approved + approved_by + decided_at（repo 守护防双决定）
  2. `repo.Get` 取最新行
  3. 查 `executors[a.Kind]`；无则 Warn 并返回（safe，不执行）
  4. 有则 `exec(ctx, a.PayloadJSON)`；err 决定 status=StatusFailed / StatusExecuted
  5. runErr 非 nil → resultJSON 写 `{"error":...}`
  6. `repo.SetResult` 回写 status + result + executedAt；失败仅 Warn
  7. 再次 `repo.Get` 返回最新行
- **错误处理**：Decide 失败透传；Executor 失败不返回 error，写入 row 的 Status=Failed；SetResult 失败仅 Warn

### `Reject`
- **签名**：`func (u *Usecase) Reject(ctx, approverID uint64, id, reason string) error`
- **职责**：标记 rejected + reason；不执行任何动作
- **流程**：`repo.Decide` 写 status=rejected + approved_by + 修剪后 reason + decided_at

### `List / Get / CountPending`
- **签名**：thin passthroughs
- **职责**：UI 读路径

## 5. 依赖关系

- **内部包**：`model/approval`（数据模型 + 状态常量）、`pkg/errs`
- **被调用方**：agent cloud-shell、flow 审批节点、HTTP handler
- **Executor 注入方**：producer 子系统启动时注册（cloud-shell 注册 `shell_command` 等）

## 6. 并发与资源管理

- **无共享状态**：Usecase 仅持有不可变 repo + log + executors map；executors map 启动期注册后只读
- **无锁**：所有状态在 DB row 上（Decide 守护防双决定）
- **ctx 透传**：所有 IO 第一参 context

## 7. 设计模式与亮点

- **per-Kind Executor 注册表**：策略模式；Executor 由 producer 注入，biz 不感知具体动作语义
- **strict additive**：任何动作必须显式 propose；Executor 缺失仅 approved 不执行（safe）
- **双决定守护下沉 Repo**：biz 不重复检查 pending 状态，由 Repo.Decide 在 SQL 层防双决定
- **Executor 失败记录在 row**：err 不向调用方透传，而是写入 status=Failed + resultJSON，操作员看到失败原因
- **SetResult 失败仅 Warn**：执行已经发生（side effect 不可逆），仅记录不阻塞
- **ProposeInput.Payload any**：producer 自定义形状，biz 不解码；Executor 持有 schema 知识

## 8. 注意事项

- **Executor 必须幂等或安全可重试**：Approve 失败重试可能已执行了 side effect（SetResult 之后调用方失败再重试会再次 Decide → Repo 拒绝双决定）
- **kind+title 必填**：Propose 校验；title 为空 → ErrInvalid
- **Source 空回退 SourceAgent**：兼容老 producer
- **executors map 启动期注册**：运行期不动态增删；如需热加载需加锁
- **repo.SetResult 失败不返回 error**：执行已经发生，仅 Warn；调用方拿到的 *Approval 是 SetResult 之前的状态（但 DB 是最终态，需重查）
- **无并发上限**：Executor 同步执行，HTTP handler 调用方自行控制并发
