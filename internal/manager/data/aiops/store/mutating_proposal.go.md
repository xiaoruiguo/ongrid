# `mutating_proposal.go` 技术实现文档

> 源文件：`internal/manager/data/aiops/store/mutating_proposal.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/aiops/store`

## 1. 概述

本文件实现 `biz.MutatingProposalRepo` 接口的 GORM 落地，是 reviewer（人工审批）审计行的持久化层。每次 mutating 工具调用在拦截点写一行 pending proposal，reviewer 返回时再翻牌为 approve / reject，工具真正派发后再 stamp `ExecutedAt`。两条写入是独立事务，因为 reviewer 往返可能跨多个 HTTP 请求生命周期。红线：decorator 包通过 narrow 接口 `decorators.MutatingProposalSink` 消费此 repo，便于测试注入 in-memory fake。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/aiops`
- **依赖方向**：被 `internal/manager/biz/aiops` 装配；依赖 `internal/manager/biz/aiops`（接口与 filter 类型）、`internal/manager/model/aiops`（GORM 模型）、`internal/pkg/errs`、`gorm.io/gorm`。

## 3. 关键类型与接口

```go
// MutatingProposalRepo 是 reviewer 审计行的 GORM 持久化层。
// decorator 包通过 narrow interface decorators.MutatingProposalSink 消费，
// 测试可注入 in-memory fake 而无需起 SQLite。
type MutatingProposalRepo struct {
    db *gorm.DB
}

// 编译期接口断言
var _ biz.MutatingProposalRepo = (*MutatingProposalRepo)(nil)
```

## 4. 关键函数与流程

### `NewMutatingProposalRepo`
- **签名**：`func NewMutatingProposalRepo(db *gorm.DB) *MutatingProposalRepo`
- **职责**：构造 repo；直接持有 `*gorm.DB`，与 SessionRepo 共享连接池。

### `Insert`
- **签名**：`func (r *MutatingProposalRepo) Insert(ctx context.Context, p *model.MutatingProposal) error`
- **职责**：写一条 fresh proposal 行，状态默认 pending。
- **流程**：
  1. `p == nil` → `errs.ErrInvalid`
  2. `Decision == ""` → 填 `model.DecisionPending`
  3. `CreatedAt.IsZero()` → `time.Now().UTC()`
  4. `db.WithContext(ctx).Create(p)`
- **错误处理**：nil 校验 + 默认值兜底；Create 错误直接返回。

### `UpdateDecision`
- **签名**：`func (r *MutatingProposalRepo) UpdateDecision(ctx context.Context, id, decision string, reason *string) error`
- **职责**：把行从 pending 翻牌为 approve / reject，stamp `DecidedAt`。`ExecutedAt` 由 `MarkExecuted` 懒填（reject 永远不填）。
- **流程**：
  1. `id == ""` → `ErrInvalid`
  2. `decision` 必须是 `DecisionApprove` 或 `DecisionReject`，否则 `ErrInvalid`
  3. `time.Now().UTC()` → Updates map（decision / decision_reason / decided_at）
  4. `RowsAffected == 0` → `errs.ErrNotFound`
- **错误处理**：枚举校验 + 行存在性校验。

### `MarkExecuted`
- **签名**：`func (r *MutatingProposalRepo) MarkExecuted(ctx context.Context, id string, t time.Time) error`
- **职责**：工具真正派发后 stamp `ExecutedAt`。best-effort —— 缺行不应失败工具执行。
- **流程**：`id == ""` → `ErrInvalid`；否则单字段 Update。
- **错误处理**：不校验 RowsAffected，故意容忍"行已不存在"。

### `Get`
- **签名**：`func (r *MutatingProposalRepo) Get(ctx context.Context, id string) (*model.MutatingProposal, error)`
- **职责**：按 id 取 proposal。
- **错误处理**：`gorm.ErrRecordNotFound` → `errs.ErrNotFound`；其余错误透传。

### `ListMutatingProposals`
- **签名**：`func (r *MutatingProposalRepo) ListMutatingProposals(ctx context.Context, f biz.MutatingProposalFilter) ([]*model.MutatingProposal, error)`
- **职责**：按 filter 列出审计行，最新优先。caller 负责 limit 规范化；非正值默认 100（防御性）。
- **流程**：`Limit <= 0` → 100；`applyMutatingProposalFilter` 拼 Where；`Offset > 0` 加 Offset；`Order("created_at DESC").Limit().Find()`。

### `CountMutatingProposals`
- **签名**：`func (r *MutatingProposalRepo) CountMutatingProposals(ctx context.Context, f biz.MutatingProposalFilter) (int64, error)`
- **职责**：按 filter 计数，用于分页总数。

### `applyMutatingProposalFilter`
- **签名**：`func applyMutatingProposalFilter(tx *gorm.DB, f biz.MutatingProposalFilter) *gorm.DB`
- **职责**：把 filter 转为 Where 链。当前支持 `ToolName`、`Decision` 两个等值过滤；空值跳过。

## 5. 依赖关系

- **内部包**：`internal/manager/biz/aiops`（`MutatingProposalRepo` 接口、`MutatingProposalFilter`）、`internal/manager/model/aiops`（`MutatingProposal` 模型及 Decision 常量）、`internal/pkg/errs`
- **外部库**：`gorm.io/gorm`
- **被调用方**：`internal/manager/biz/aiops` 的 usecase；decorator 包通过 narrow interface 注入测试 fake。

## 6. 并发与资源管理

- **无显式锁**：每个方法在自身 DB context 中运行；Insert + UpdateDecision 是两条独立事务，因 reviewer 往返可跨 HTTP 请求。
- **ctx 透传**：所有方法首参 `context.Context`，通过 `db.WithContext(ctx)` 透传到 GORM。
- **连接共享**：与 `SessionRepo` 共享同一 `*gorm.DB`，复用连接池。

## 7. 设计模式与亮点

- **Narrow interface 测试注入**：decorator 包通过 `MutatingProposalSink` 窄接口消费，测试用 in-memory fake 替身，避免起 SQLite。
- **独立事务写对**：Insert 与 UpdateDecision 分离，匹配 reviewer 异步审批语义。
- **MarkExecuted best-effort**：注释明示"missing row should not fail the tool execution"，工具执行优先于审计完整性。
- **filter 抽象**：`applyMutatingProposalFilter` 集中处理 Where 链，List / Count 复用，避免分页总数与列表 filter 漂移。
- **limit 防御性默认**：caller 责任规范化，repo 兜底 100，防止直接调用 repo 的测试 / 防御性代码意外拉全表。

## 8. 注意事项

- **Decision 枚举**：仅允许 `DecisionApprove` / `DecisionReject`；`DecisionPending` 是初始态，不可通过 `UpdateDecision` 写回。
- **CreatedAt 由 repo 兜底**：caller 未填时由 repo 补 UTC now；但 BeforeCreate hook（model 层）也会处理 ID，两者协作。
- **RowsAffected 检查策略不一致**：`UpdateDecision` / `Get` 严格校验存在性；`MarkExecuted` 故意宽松。新增方法需明确策略。
- **filter 当前仅 2 字段**：扩展需同步更新 biz 层 `MutatingProposalFilter` 与此处 Where 链。
- **时区**：所有时间戳统一 UTC，避免跨时区漂移。
