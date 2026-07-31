# `scheduler.go` 技术实现文档

> 源文件：`internal/manager/biz\flow/scheduler.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/flow`

## 1. 概述

本文件实现 `trigger.cron` driver。一个 ticker 扫描 enabled flows 的 trigger.cron 节点，fire 那些 next-fire time 已过的。Schedule 在内存（boot/首次见到时从 cron spec 重新派生），与 report scheduler 同模型 —— **in-flight run 不跨 restart 存活**。Cron spec 用 UTC 求值。piggyback 一个 hourly run-retention sweep 避免第二个 goroutine。

## 2. 包信息

- **包名**：`flow`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 `cmd/ongrid` 启动调 `Start`；依赖 `github.com/robfig/cron/v3`、`log/slog`、`sync`、`time`

## 3. 关键类型与接口

```go
type cronTriggerConfig struct {
    Cron string `json:"cron"`  // 标准 5 段 cron, UTC, e.g. "0 8 * * *"
}

type Scheduler struct {
    uc        *Usecase
    log       *slog.Logger
    interval  time.Duration
    mu        sync.Mutex
    next      map[string]time.Time  // "flowID:nodeID" → next fire (UTC)
    lastPrune time.Time             // last run-retention sweep (UTC)
}
```

Sentinel：`interval = 30 * time.Second`（tick 间隔）；retention sweep 至多 hourly。

## 4. 关键函数与流程

### `NewScheduler`
- **签名**：`func NewScheduler(uc *Usecase, log *slog.Logger) *Scheduler`
- **职责**：构造 cron driver；30s tick
- **流程**：log nil → Default；返回 `&Scheduler{uc, log, 30s, map[string]time.Time{}, zero}`

### `Start`
- **签名**：`func (s *Scheduler) Start(ctx context.Context)`
- **职责**：启动 tick loop 直到 ctx 取消
- **流程**：s nil 或 uc nil → return；`go s.loop(ctx)`

### `loop`
- **签名**：`func (s *Scheduler) loop(ctx context.Context)`
- **职责**：tick 循环
- **流程**：
  1. `time.NewTicker(s.interval)`；defer Stop
  2. select ctx.Done / t.C：
     - now = UTC
     - `s.tick(ctx, now)`
     - **piggyback retention sweep**：now - lastPrune >= 1h → lastPrune = now；`uc.PruneOldRuns(ctx)`
- **关键设计**：retention sweep 搭 cron tick 便车；无第二个 goroutine

### `tick`
- **签名**：`func (s *Scheduler) tick(ctx, now time.Time)`
- **职责**：扫描 enabled flows；fire 到期的 cron trigger
- **流程**：
  1. `uc.ListEnabledFlows(ctx)`；失败 Warn + return
  2. 构建 live map（key = "flowID:nodeID"）
  3. 遍历 flows：
     - `ParseGraph(f.GraphJSON)`；失败 continue
     - 遍历 `g.Triggers()`：仅 NodeTriggerCron
     - `parseCronSpec(node.Config)`；空 continue
     - `cron.ParseStandard(spec)`；失败 continue（UI 校验；silent skip）
     - key = "flowID:nodeID"；live[key] = true
     - **mu.Lock**：
       - 首次见到 → `s.next[key] = sched.Next(now)`；Unlock；continue（不立即 fire）
       - now.Before(nf) → Unlock；continue
       - `s.next[key] = sched.Next(now)`；Unlock
     - payload = `{fired_at: RFC3339, cron: spec}`
     - `uc.TriggerEvent(ctx, f.ID, NodeTriggerCron, payload)`；失败 Warn，成功 Info
  4. **清理 vanished schedule**：mu.Lock；遍历 s.next，不在 live → delete；Unlock
- **关键设计**：首次见到不 fire（arm for next occurrence）；vanished flow/cron 清理防 stale schedule

### `parseCronSpec`
- **签名**：`func parseCronSpec(cfgRaw json.RawMessage) string`
- **职责**：从 trigger config 提取 cron 字段
- **流程**：len>0 unmarshal；TrimSpace cfg.Cron

## 5. 依赖关系

- **外部库**：`github.com/robfig/cron/v3`（ParseStandard + sched.Next）、`context`、`encoding/json`、`fmt`、`log/slog`、`strings`、`sync`、`time`
- **协作**：`Usecase.ListEnabledFlows` + `Usecase.TriggerEvent` + `Usecase.PruneOldRuns`
- **被调用方**：`cmd/ongrid` 启动调 `Start(ctx)`

## 6. 并发与资源管理

- **`mu sync.Mutex`**：保护 `next map` 和 `lastPrune`
- **单 goroutine loop**：Start 启动一个 goroutine；不需额外同步
- **`time.NewTicker` 显式 Stop**：defer t.Stop() 防泄漏
- **next map 内存 only**：不持久化；restart 后重新 arm

## 7. 设计模式与亮点

- **30s tick + cron.Next 求值**：不依赖外部 cron daemon；自调度
- **首次见到不 fire**：arm for next occurrence；防 boot 时所有 cron 立即触发
- **vanished schedule 清理**：disabled/deleted/edited 的 flow/cron 从 next map 删；edited cron 重新 arm
- **piggyback retention sweep**：搭 cron tick 便车；hourly；无第二个 goroutine
- **UTC 求值**：`time.Now().UTC()` + `cron.ParseStandard`（标准 5 段 cron UTC）
- **silent skip invalid cron**：UI 应校验；此处失败 continue 不 Warn
- **in-flight run 不跨 restart**：next map 内存 only；restart 后重新 arm（已 running 的 run 被 SweepStaleRunning 清理）

## 8. 注意事项

- **30s tick 精度**：cron fire 最迟 30s；对秒级精度场景不够（cron 本身分钟级）
- **首次见到不 fire**：boot 后第一次见到 cron trigger 不会立即 fire；arm for next occurrence
- **in-flight run 不跨 restart**：restart 后 next map 空；running run 被 SweepStaleRunning 清理
- **invalid cron silent skip**：UI 应校验；此处失败 continue
- **UTC 求值**：cron spec 应按 UTC 写；操作员需注意时区
- **piggyback retention hourly**：`PruneOldRuns` 至多每小时；高频调用无意义
- **next map 无上限**：enabled flows × cron triggers 数量有限；可控
- **live map 清理**：disabled/deleted/edited 的 flow/cron 从 next map 删；防 stale schedule 累积
- **ParseGraph 失败 silent skip**：坏 graph 不 Warn（操作员应在保存时校验）
