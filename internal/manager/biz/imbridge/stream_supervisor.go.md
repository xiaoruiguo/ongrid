# `stream_supervisor.go` 技术实现文档

> 源文件：`internal/manager/biz/imbridge/stream_supervisor.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/imbridge`

## 1. 概述

本文件是 IM 桥接器的流式客户端监管器：每个 (enabled, stream-mode) ImApp 一个长跑 goroutine。30s reconcile 间隔 diff DB 状态 vs 运行客户端：新 app → spawn；禁用 → cancel；secret 轮换 → cancel+respawn。设计要点：客户端 ctx 父级是 supervisor ctx，manager 关闭时优雅传播 Stop；runClientWithRestart 包 exponential backoff 兜底 SDK 不自愈的终端错误。红线：凭据轮换通过比较 AppID+AppSecret tail 检测，不依赖 updated_at。

## 2. 包信息

- **包名**：`imbridge`
- **所属模块**：`internal/manager/biz/imbridge`
- **依赖方向**：被 main.go 调用（RegisterFactory + Run）；依赖 `model/imbridge`（ImApp）

## 3. 关键类型与接口

```go
// StreamClient 是每个 provider 长连接客户端实现的接口
type StreamClient interface {
    ProviderName() string
    Run(ctx context.Context) error // 阻塞直到 ctx 取消或致命错误
}

// StreamClientFactory 为一个 ImApp 构造 StreamClient
type StreamClientFactory func(app *model.ImApp, bridge *Bridge) (StreamClient, error)

// StreamRepo 是 supervisor 需要的窄数据层接口（仅 list enabled stream apps）
type StreamRepo interface {
    ListEnabledStreamApps(ctx context.Context) ([]*model.ImApp, error)
}

type StreamSupervisor struct {
    repo       StreamRepo
    bridge     *Bridge
    factories  map[string]StreamClientFactory // provider -> factory
    log        *slog.Logger
    mu         sync.Mutex
    running    map[uint64]*streamSlot // im_app.id -> 运行 slot
    reconcile  time.Duration
}

type streamSlot struct {
    app    *model.ImApp
    cancel context.CancelFunc
    done   chan struct{}
}
```

## 4. 关键函数与流程

### `NewStreamSupervisor`
- **签名**：`func NewStreamSupervisor(repo, bridge, log) *StreamSupervisor`
- **职责**：构造 supervisor，默认 reconcile=30s
- **流程**：log nil → Default；log 加 `comp=imbridge.stream`；factories/running 初始化空 map

### `RegisterFactory`
- **签名**：`func (s *StreamSupervisor) RegisterFactory(provider string, f StreamClientFactory)`
- **职责**：注册 provider 实现（Feishu/DingTalk/Slack/Telegram），boot 前 Run 前调用

### `Run`
- **签名**：`func (s *StreamSupervisor) Run(ctx context.Context)`
- **职责**：启动 reconcile 循环，ctx done 时优雅关闭
- **流程**：
  1. `s.reconcileOnce(ctx)` 初始 apply
  2. `time.NewTicker(s.reconcile)` 30s
  3. select：ctx.Done → `s.shutdown()` 返回；tick → `s.reconcileOnce(ctx)`

### `reconcileOnce`
- **签名**：`func (s *StreamSupervisor) reconcileOnce(ctx context.Context)`
- **职责**：diff DB vs 运行客户端，spawn/stop/refresh
- **流程**：
  1. `repo.ListEnabledStreamApps(ctx)`；失败 Warn return
  2. 构建 wantByID map
  3. `mu.Lock`；遍历 running：
     - app 消失/禁用/凭据轮换（`nxt.AppID != slot.app.AppID || nxt.AppSecret != slot.app.AppSecret`）→ `slot.cancel()`；Unlock 等待 `<-slot.done`；重新 Lock 删除
     - 否则刷新 `slot.app = nxt`（更新显示标签等，不重启客户端）
  4. 遍历 wantByID：
     - 已 running → 跳过
     - 无 factory → Warn "no factory for provider"
     - factory 失败 → Warn
     - 成功：`runCtx, cancel := context.WithCancel(context.Background())`；创建 slot；`go s.runClientWithRestart(runCtx, slot, client)`
- **注释**：凭据轮换检测用 AppID+AppSecret 比较，不依赖 updated_at

### `runClientWithRestart`
- **签名**：`func (s *StreamSupervisor) runClientWithRestart(ctx, slot, client)`
- **职责**：包 client.Run 的 exponential backoff
- **流程**：
  1. `defer close(slot.done)`
  2. backoff=1s，maxBackoff=60s
  3. 循环：
     - `client.Run(ctx)`；err 非 nil 且 ctx 未取消 → Warn + backoff
     - ctx 取消 → return
     - `select { ctx.Done | time.After(backoff) }`
     - backoff *= 2，cap 60s
- **注释**：SDK 自身处理瞬态重连；Run 返回即终端错误，supervisor 加 backoff

### `shutdown`
- **签名**：`func (s *StreamSupervisor) shutdown()`
- **职责**：cancel 所有 slot 并等待 done
- **流程**：`mu.Lock`；遍历 running：`slot.cancel()` + `<-slot.done` + delete

## 5. 依赖关系

- **内部包**：`model/imbridge`（ImApp）、同包 `Bridge`
- **外部库**：仅标准库
- **被调用方**：main.go（RegisterFactory + Run）

## 6. 并发与资源管理

- **`mu`（Mutex）**：保护 running map；reconcileOnce 中持锁分两段（stop 时临时 Unlock 等 done）
- **per-slot ctx**：`context.WithCancel(context.Background())`，父级不是 supervisor ctx——manager 关闭通过 shutdown 显式 cancel 所有 slot
- **done channel**：每个 slot 一个 `chan struct{}`，runClientWithRestart defer close，shutdown 等 done 保证优雅退出
- **backoff 无 jitter**：1s/2s/4s.../60s cap

## 7. 设计模式与亮点

- **reconcile loop 模式**：30s tick diff DB vs 运行态，类似 Kubernetes controller
- **凭据轮换检测**：比较 AppID+AppSecret tail，不依赖 updated_at（更可靠）
- **runClientWithRestart 外层 backoff**：SDK 自身重连兜底，supervisor 处理 SDK 不自愈的终端错误
- **shutdown 优雅退出**：cancel + 等 done，保证所有 goroutine 干净退出
- **StreamRepo 窄接口**：仅 list enabled stream apps，不拖入完整 CRUD
- **factory 注册制**：main.go boot 时注册各 provider，supervisor 不硬编码 provider

## 8. 注意事项

- **reconcile=30s**：默认；admin 改设置后最长 30s 生效
- **凭据轮换检测**：AppID+AppSecret 比较，secret 变化即重启客户端
- **per-slot ctx 父级是 Background**：不是 supervisor ctx，manager 关闭通过 shutdown 显式 cancel
- **backoff cap 60s**：1s/2s/4s.../60s，无 jitter
- **factory 失败不重试**：Warn 后跳过，下个 reconcile 再尝试
- **refresh app 不重启**：仅显示标签变化时不重启客户端，避免无谓断连
- **SDK 自身重连**：飞书/Slack SDK 有 auto-reconnect，supervisor backoff 是兜底
