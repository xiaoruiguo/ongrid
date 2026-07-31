# `review_gate.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/decorators/review_gate.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools/decorators`

## 1. 概述

本文件实现 SOP-double-sign 装饰器 `ReviewGate`：拦截 `Class="write"|"destructive"` 的工具调用，狭义的 `apply_config_change` 走确定性策略放行，其他 spawn reviewer worker（agents/reviewer.md），仅在 reviewer 输出 `Decision: approve` 时转发给 inner。reject 返回 wrap 错误让 coordinator 解释给用户。关键红线：spawner nil 时 mutating 工具直接拒绝（safe default）；reviewer 输出无 decision 行视为 reject；sink 错误 best-effort 吞掉。

## 2. 包信息

- **包名**：`decorators`
- **所属模块**：`internal/manager/biz/aiops/tools/decorators`
- **依赖方向**：被 `chain.go` 的 `Wrap` 调用；依赖 `basetool`、标准库

## 3. 关键类型与接口

```go
const DefaultReviewerAgent = "reviewer"
const DefaultReviewerTimeout = 60 * time.Second

var (
    ErrReviewRejected  = errors.New("review rejected")
    ErrReviewUndecided = errors.New("reviewer returned no decision")
    ErrReviewerSpawn   = errors.New("reviewer spawn failed")
)

type ReviewSpawner interface {
    SpawnReviewer(ctx context.Context, req ReviewSpawnRequest) (*ReviewSpawnResult, error)
}

type MutatingProposalSink interface {
    Insert(ctx context.Context, ev MutatingProposalEvent) (id string, err error)
    UpdateDecision(ctx context.Context, id, decision, reason string) error
    MarkExecuted(ctx context.Context, id string, t time.Time) error
}

type ReviewGate struct {
    inner         basetool.BaseTool
    reviewerAgent string
    spawner       ReviewSpawner
    sink          MutatingProposalSink
    timeout       time.Duration
    nowFn         func() time.Time
}
```

## 4. 关键函数与流程

### `WithReviewGate`
- **签名**：`func WithReviewGate(inner, spawner, cfg ReviewGateConfig) basetool.BaseTool`
- **职责**：构造 ReviewGate 装饰器
- **流程**：inner nil → 返回 nil；agent 空 → DefaultReviewerAgent；timeout <=0 → DefaultReviewerTimeout；nowFn 默认 time.Now
- **说明**：nil spawner 仍构造，InvokableRun 时返回 ErrReviewerSpawn（safe default）

### `ReviewGate.Info`
- **签名**：`func (g *ReviewGate) Info(ctx) (*basetool.ToolInfo, error)`
- **职责**：透传 inner.Info；Class 字段是 ReviewGate 决定是否 gate 的依据

### `ReviewGate.InvokableRun`
- **签名**：`func (g *ReviewGate) InvokableRun(ctx, argsJSON string, opts ...basetool.InvokeOption) (string, error)`
- **职责**：gate mutating 工具调用
- **流程**：
  1. `inner.Info(ctx)`；err/nil → pass-through（Info-broken 工具直接跑，让 inner 自己 surface error）
  2. `!isMutatingClass(info.Class)` → pass-through
  3. `deterministicReviewApproval(name, argsJSON)` 命中 → `runWithDeterministicApproval`
  4. spawner nil → `ErrReviewerSpawn` wrap "spawner not wired"
  5. `ResolveOptions(opts)` 拿 operator user id
  6. sink 非 nil → `Insert(MutatingProposalEvent{...})` best-effort（err 不阻塞，proposalID 仅在 err==nil 时填）
  7. `buildReviewerPrompt` 构造 markdown brief
  8. `context.WithTimeout(ctx, g.timeout)` spawnCtx
  9. `spawner.SpawnReviewer(spawnCtx, ReviewSpawnRequest{AgentName, Prompt})`
  10. spawnErr / res nil / res.Err 非空 → `recordDecision(reject)` + 返回 `ErrReviewerSpawn` wrap
  11. `parseReviewerDecision(res.Result)` 解析 `Decision: approve|reject` 行
  12. approve → `recordDecision(approve)` → `inner.InvokableRun` → `markExecuted` → 返回 out/runErr
  13. reject → `recordDecision(reject)` + 返回 `ErrReviewRejected: <reason>`
  14. undecided → `recordDecision(reject)` + 返回 `ErrReviewUndecided`

### `deterministicReviewApproval`
- **签名**：`func deterministicReviewApproval(toolName, argsJSON string) (string, bool)`
- **职责**：对狭义 `apply_config_change` + alert_rule domain + create action 走确定性放行
- **流程**：toolName != "apply_config_change" → false；unmarshal args 取 domain/action/payload；非 alert_rule domain → false；action 非空且非 create → false；payload.action 兜底解析；命中返回 `alertRuleCreateDeterministicApprovalReason`
- **理由**：apply_config_change 自身已校验 confirmed/admin/payload/draft_hash 等，reviewer 重复审无价值

### `buildReviewerPrompt`
- **签名**：`func buildReviewerPrompt(toolName, toolClass, argsJSON string, operatorUID uint64) string`
- **职责**：构造 reviewer 的 markdown brief
- **流程**：`indentJSON` 美化 args；写"# 二审 proposal" 标题 + Tool/Class/Operator 元信息 + Args 代码块 + 输出格式硬性要求（必须含 `Decision: approve|reject` 行）+ SOP 提示

### `parseReviewerDecision`
- **签名**：`func parseReviewerDecision(text string) (decision, reason string)`
- **职责**：扫描 reviewer 输出找 `Decision:` 行
- **流程**：
  1. `bufio.Scanner` 逐行扫，buffer 64KB..1MB
  2. 行首 strip `*_# ` markdown emphasis，`HasPrefix("decision:")` 匹配
  3. `rest` 取 `approve`/`reject`（前缀匹配，case-insensitive）；其他 continue 找下一行
  4. matched 后累计非空行作为 reason，cap 8 行
  5. 未匹配 → 返回 `("", "")`（caller 转 reject）
  6. reason `truncate(reason, 1024)`

### `recordDecision / markExecuted`
- **签名**：`func (g *ReviewGate) recordDecision(ctx, id, decision, reason string)` / `func (g *ReviewGate) markExecuted(ctx, id string)`
- **职责**：best-effort 更新 sink
- **流程**：sink nil 或 id 空 → no-op；调 sink 方法，err 吞掉（注释明示 audit 不影响 decision）

### `isMutatingClass`
- **签名**：`func isMutatingClass(class string) bool`
- **职责**：返回 true 当 class="write"|"destructive"；空视为 "read"

## 5. 依赖关系

- **内部包**：`basetool`
- **外部库**：标准库 `bufio`、`context`、`encoding/json`、`errors`、`fmt`、`strings`、`time`
- **被调用方**：`chain.go` 的 `Wrap`；生产 binding 由 cmd/main.go 把 `*chatruntime.Runtime` 包成 `ReviewSpawner` shim

## 6. 并发与资源管理

- `ReviewGate` 结构 immutable（除 nowFn），多 goroutine 共享安全
- `context.WithTimeout(spawnCtx, g.timeout)` + `defer cancel()` 限定 reviewer round-trip
- sink 调用 best-effort，错误不阻塞主流程

## 7. 设计模式与亮点

- **Class-driven dispatch**：ReviewGate 看 Class 字段决定是否 gate，agent loop 不需 special-case mutating 工具
- **位置在 chain 外**：review_gate 在 timeout/audit 外——reviewer 是独立 graph.Invoke 不能套 inner 15s；reject 不写 execution 表
- **确定性放行狭义**：apply_config_change + alert_rule/create 自身已校验，避免 reviewer 重复劳动
- **safe default 姿态**：spawner nil / spawn err / undecided 全部视为 reject
- **行扫描而非 LLM re-parse**：避免双倍 LLM 成本和二次失败模式；reviewer.md prompt 强制 decision 行
- **best-effort sink**：audit 失败不让 review decision 失败
- **常量值 mirror model**：`model_DecisionApprove/Reject` 本地常量避免 import manager/model/aiops

## 8. 注意事项

- **spawner nil = mutating 工具拒绝**：cmd/main 必须 wire ReviewSpawner，否则部署无 SOP 门控
- **reviewer.md 强制 decision 行**：parser 严格匹配 `Decision: approve|reject`，reviewer 必须按格式输出
- **reason cap 1024**：审计行长度控制
- **reviewer timeout 60s**：reviewer 最多 5 轮 LLM+tool；超出独立于 inner tool 的 15s
- **`nowFn` 注入**：测试可注入 deterministic clock
- **decision 行多个时取首个**：reviewer 写 "Decision: pending" 后再 "Decision: approve" 时取后者——parser continue 找首个匹配的 approve/reject
