# `chain.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/decorators/chain.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools/decorators`

## 1. 概述

本文件定义装饰器链的统一装配点 `Wrap`：把 `basetool.BaseTool` 按固定顺序包装为 `tenant_bind → review_gate → timeout → audit → ratelimit → metric`。所有装饰器跨切关注点（tenant 注入、SOP 二审、超时、审计、限流、监控）集中在一处可文档化、可 review。零值 `Deps` 回退安全默认（DefaultTimeout、NoopLimiter、DefaultRegisterer、无 audit、不装 ReviewGate）。

## 2. 包信息

- **包名**：`decorators`
- **所属模块**：`internal/manager/biz/aiops/tools/decorators`
- **依赖方向**：被 `cmd/ongrid/main.go` 装配点和各 tool 注册器调用；依赖 `basetool`、`prometheus`

## 3. 关键类型与接口

```go
type Deps struct {
    Timeout        time.Duration              // 0 → DefaultTimeout (15s)
    Audit          AuditSink                  // nil → audit disabled
    Limiter        Limiter                    // nil → NoopLimiter
    Registerer     prometheus.Registerer      // nil → prometheus.DefaultRegisterer
    ReviewSpawner  ReviewSpawner              // nil → ReviewGate 不装
    ReviewSink     MutatingProposalSink       // nil-safe
    ReviewerAgent  string                     // 空 → DefaultReviewerAgent
    ReviewerTimeout time.Duration             // 0 → DefaultReviewerTimeout
}
```

## 4. 关键函数与流程

### `Wrap`
- **签名**：`func Wrap(inner basetool.BaseTool, deps Deps) basetool.BaseTool`
- **职责**：按标准顺序包装 inner
- **流程**：
  1. `inner == nil` → 返回 nil
  2. limiter nil → NoopLimiter
  3. 从内到外逐层包装：`WithMetric` → `WithRateLimit` → `WithAudit`（nil-safe）→ `WithTimeout` → （条件）`WithReviewGate` → `WithTenantBind`
  4. ReviewSpawner 非 nil 才装 ReviewGate；nil 时 mutating 工具无 SOP 门控（cmd/main 必须正确 wire）
- **错误处理**：无错误返回

### 链顺序的"为什么"
- **tenant_bind 最外**：rewrite 后的 args + opts 流入所有下游（audit 记 rewritten args、ratelimit 按 resolved user_id、review_gate 在 proposal 上看到 operator user_id）
- **review_gate 在 timeout 外**：reviewer worker 自身是 graph.Invoke 跑 LLM，受自己的多轮预算约束，套在 inner 15s 内不现实；ReviewGate 携带独立 Timeout（默认 60s），approve 后 inner 走 timeout 的 15s
- **review_gate 在 audit 外**：rejected proposal 没有执行可审计（chat_tool_calls 是 execution log，chat_mutating_proposals 是 decision log）；放进 audit 内会污染 execution 表
- **review_gate 对 read 类无操作**：Class != write|destructive 时 pass-through，只多一次 Info() 调用
- **timeout 在 audit 外**：超时时 audit-pending 行仍要写入（否则丢失 timeout 分类）
- **audit 在 ratelimit 外**：被限流的拒绝仍要审计为失败尝试
- **ratelimit 在 metric 外**：拒绝不计入 duration histogram（避免把 limiter 开销算成近零 tool call）
- **metric 最内**：histogram 跟踪 inner tool 真实延迟，不含装饰器开销

## 5. 依赖关系

- **内部包**：`basetool`
- **外部库**：`github.com/prometheus/client_golang`
- **被调用方**：`cmd/ongrid/main.go`、tool 注册器

## 6. 并发与资源管理

- `Wrap` 返回的链是 immutable 结构，多 goroutine 共享安全
- 各装饰器的内部状态（如 Limiter 的 map、metric 的 collectors）自行管理并发

## 7. 设计模式与亮点

- **统一装配点**：所有跨切关注点集中一处文档化，便于 review
- **零值安全默认**：`Deps{}` 仍能产生 well-behaved 工具（NoopLimiter、DefaultTimeout、DefaultRegisterer）
- **条件装饰器**：ReviewSpawner nil 时不装 ReviewGate；其他装饰器 nil-safe（WithAudit nil pass-through、WithRateLimit nil pass-through）
- **顺序的可调性**：注释明示"Any of these can be flipped if the SLO story changes"

## 8. 注意事项

- **ReviewSpawner nil 风险**：mutating 工具（Class=write|destructive）会无 SOP 门控直接跑；cmd/main 必须在部署包含 reviewer persona 时 wire 此项
- **ReviewSink nil-safe**：nil 时跳过持久化，内存中仍走 approve/reject 流程
- **顺序变更影响**：改顺序会影响 audit 行为、metric 准确性；改前先看注释的"为什么"
- **nil inner 返回 nil**：caller 应处理 nil 防止 nil tool 进入 bag
