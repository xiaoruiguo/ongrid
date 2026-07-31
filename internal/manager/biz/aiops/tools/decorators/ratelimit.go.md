# `ratelimit.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/decorators/ratelimit.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools/decorators`

## 1. 概述

本文件实现 ratelimit 装饰器：在 `InvokableRun` 前先问 `Limiter.Allow(ctx, toolName, userID)`，被拒直接返回 `ErrRateLimited` 而不调 inner。生产实现 `TokenBucketLimiter` 基于 `golang.org/x/time/rate`，按 `(tool, user)` 维度独立桶；burst 等于 rate，idle 用户首调总能放行。默认 10/min。

## 2. 包信息

- **包名**：`decorators`
- **所属模块**：`internal/manager/biz/aiops/tools/decorators`
- **依赖方向**：被 `chain.go` 调用；依赖 `basetool`、`golang.org/x/time/rate`

## 3. 关键类型与接口

```go
const DefaultRateLimit = 10 // calls per minute

var ErrRateLimited = errors.New("tool rate limit exceeded")

type Limiter interface {
    Allow(ctx context.Context, toolName string, userID uint64) bool
}

type NoopLimiter struct{}  // 永远 true

type TokenBucketLimiter struct {
    ratePerMinute int
    burst         int
    mu            sync.Mutex
    limiters      map[limiterKey]*rate.Limiter
}

type limiterKey struct {
    tool string
    user uint64
}

type RateLimitTool struct {
    inner   basetool.BaseTool
    limiter Limiter
}
```

## 4. 关键函数与流程

### `NewTokenBucketLimiter`
- **签名**：`func NewTokenBucketLimiter(ratePerMinute int) *TokenBucketLimiter`
- **职责**：构造 token bucket limiter
- **流程**：rate <= 0 → DefaultRateLimit；burst = rate（idle 用户首调总放行）

### `TokenBucketLimiter.Allow`
- **签名**：`func (l *TokenBucketLimiter) Allow(_ context.Context, toolName string, userID uint64) bool`
- **职责**：查 per-(tool,user) bucket，有 token 消费并返回 true
- **流程**：
  1. 加锁查 map；未命中用 `rate.NewLimiter(rate.Limit(ratePerMinute)/60.0, burst)` 创建（rate 转换 per-minute → per-second）
  2. `lim.Allow()` 非阻塞消费
- **错误处理**：无错误返回（bool）
- **说明**：user 0 走 "anonymous" 共享 bucket，prod 由 upstream auth 锁住

### `WithRateLimit`
- **签名**：`func WithRateLimit(inner basetool.BaseTool, limiter Limiter) basetool.BaseTool`
- **职责**：包装 inner；limiter nil → pass-through
- **流程**：limiter nil 返回 inner；否则 `&RateLimitTool{inner, limiter}`

### `RateLimitTool.Info`
- **签名**：`func (r *RateLimitTool) Info(ctx) (*basetool.ToolInfo, error)`
- **职责**：透传 inner.Info（schema 不变）

### `RateLimitTool.InvokableRun`
- **签名**：`func (r *RateLimitTool) InvokableRun(ctx, argsJSON string, opts ...basetool.InvokeOption) (string, error)`
- **职责**：先查 limiter，被拒返回 `ErrRateLimited` wrap 错误
- **流程**：
  1. `ResolveOptions(opts)` 取 userID
  2. `inner.Info(ctx)` 取 name
  3. `limiter.Allow(ctx, name, userID)` false → `fmt.Errorf("%w: tool=%s user=%d", ErrRateLimited, name, userID)`
  4. true → `inner.InvokableRun(ctx, argsJSON, opts...)`

## 5. 依赖关系

- **内部包**：`basetool`
- **外部库**：`golang.org/x/time/rate`、标准库 `context`、`errors`、`fmt`、`sync`
- **被调用方**：`chain.go` 的 `Wrap`

## 6. 并发与资源管理

- `TokenBucketLimiter.mu`（Mutex）：保护 `limiters` map
- 每个 (tool, user) 一个 `*rate.Limiter`，懒创建；map 永不清理（实际部署规模下条目数有界）
- `rate.Limiter` 自身线程安全，但 `Allow` 调用前需取引用，故外层 mu 保护 map 读改

## 7. 设计模式与亮点

- **per-(tool, user) 维度**：两用户不共享预算；单用户不能用一个 tool 拖死其他 tool
- **burst = rate**：idle 用户首调总放行（避免冷启动惩罚）
- **非阻塞**：拒绝即返 `ErrRateLimited`，agent loop 看到快速失败而非 stalled 调用
- **nil limiter pass-through**：prod 可在 config 时关闭限流而无需 conditional 构造
- **ErrRateLimited sentinel**：`errors.Is` 让 agent loop 把它分类为 `status=error` 写入 chat_tool_calls

## 8. 注意事项

- **DefaultRateLimit = 10/min**：经验值，让 correlate_incident burst fire query_promql + query_logql 而不熔 Prom；可通过注入自定义 Limiter 覆盖
- **anonymous bucket**：user 0 共享一桶，prod 由 upstream auth 锁住；in-process 测试可接受
- **map 不清理**：长期运行下 (tool, user) 条目有界（user 数 × tool 数），实践中不会膨胀
- **rate 转换**：`rate.Limit(ratePerMinute)/60.0` 把 per-minute 转 per-second；rate.Limit 单位是 events/sec
