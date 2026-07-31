# `usecase.go` 技术实现文档

> 源文件：`internal/manager/biz/audit/usecase.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/audit`

## 1. 概述

本文件是 HLD-010 审计日志的 BC 级门面。暴露单一 `Emit` 方法供 middleware / handler / retention goroutine 记录观察事件。核心红线：**写入失败仅 Warn-log，绝不阻塞业务**（HLD-010 "audit write failure must never block business"）。同时承担每日保留期清理（03:00 本地时间扫表）和面向 RCA 的变更查询便利方法。

## 2. 包信息

- **包名**：`audit`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 middleware、HTTP handler、retention goroutine、aiops 工具（query_change_events）调用；依赖 `data/audit/store`（ListFilters 别名）、`model/audit`

## 3. 关键类型与接口

```go
type Repo interface {
    Insert(ctx, log *model.Log) error
    List(ctx, f ListFilters) ([]model.Log, int64, error)
    DeleteOlderThan(ctx, cutoff time.Time) (int64, error)
}

type ListFilters = store.ListFilters  // alias 避免 handler 依赖 data 层

type Event struct {
    // Actor — middleware 从 JWT claims 填，handler 填 anon/失败 auth 路径
    UserID *uint64
    UserEmail, Role, IP, UserAgent, RequestID string
    // Action
    Action, ResourceType, ResourceID, ResourceName string
    // Outcome
    Status, ErrorCode, ErrorMessage string  // status: success|failure|denied
    // Free-form 结构化详情；调用方负责 redact 密钥/密码/token
    Payload any
}

type Usecase struct {
    repo Repo
    log  *slog.Logger
}
```

Sentinel：`ErrorMessage` 截断阈值 `512` 字符。

## 4. 关键函数与流程

### `New`
- **签名**：`func New(repo Repo, log *slog.Logger) *Usecase`
- **职责**：构造 Usecase；log 必填（warn-on-failure 路径依赖）
- **流程**：log nil → `slog.Default()`；返回 `&Usecase{repo, log}`

### `Emit`
- **签名**：`func (u *Usecase) Emit(ctx, ev Event)`
- **职责**：持久化一条 Event；返回 nothing；失败仅 Warn
- **流程**：
  1. u / u.repo nil → 直接 return
  2. Action 或 Status 空 → Warn "dropped event with empty action or status" + return
  3. 构造 `model.Log` 行；`OccurredAt = time.Now().UTC()`；`ErrorMessage = truncate(ev.ErrorMessage, 512)`
  4. Payload 非 nil → `json.Marshal`；失败 Warn "payload marshal failed; storing empty"（row.PayloadJSON 留空）
  5. `repo.Insert`；失败 Warn "insert failed; observation lost"（含 action / resource_type / resource_id / err）
- **错误处理**：全程不返回 error；空字段丢弃 + Warn；marshal 失败存空；Insert 失败丢弃 + Warn

### `List`
- **签名**：`func (u *Usecase) List(ctx, f ListFilters) ([]model.Log, int64, error)`
- **职责**：admin UI 读路径
- **流程**：u/u.repo nil → 返回 nil,0,nil；否则透传 repo.List

### `ListChanges`
- **签名**：`func (u *Usecase) ListChanges(ctx, from, to time.Time, resourceType, action string, limit int) ([]model.Log, error)`
- **职责**：RCA 便利方法（HLD-013 Phase 2）；支撑 aiops `query_change_events` 工具的"incident 附近变了什么"步骤
- **流程**：
  1. u/u.repo nil → 返回 nil,nil
  2. limit<=0 → 默认 50
  3. 调 `u.List` 带过滤器
  4. 返回 logs（丢弃 total）
- **关键设计**：**有意包含 status=failure/denied** —— "有人在症状之前尝试改 X" 本身就是根因线索
- **错误处理**：透传 List 错误

### `RunRetention`
- **签名**：`func (u *Usecase) RunRetention(ctx, retentionDays int) error`
- **职责**：每日 03:00 本地时间扫表删除超过保留期的行；阻塞直到 ctx 取消
- **流程**：
  1. u/u.repo nil 或 retentionDays<=0 → `<-ctx.Done()` 阻塞（操作员可能用外部归档管理保留期）
  2. 循环：
     - 计算下一个 03:00 本地时间（已过则 +24h）
     - `time.NewTimer`；`select` ctx.Done / timer.C
     - cutoff = now - retentionDays×24h
     - `repo.DeleteOlderThan(ctx, cutoff)`；失败 Warn + continue
     - 成功 Info log 含 retention_days + rows_removed
- **错误处理**：单次删除失败 Warn 后 continue；ctx 取消返回 nil

### `truncate`
- **签名**：`func truncate(s string, n int) string`
- **职责**：硬截断 ErrorMessage 到 n 字节；超长截前 n 字符

## 5. 依赖关系

- **内部包**：`data/audit/store`（ListFilters alias 来源）、`model/audit`
- **被调用方**：HTTP middleware（JWT 解码后填 Actor）、HTTP handler（anon/失败 auth 路径）、retention goroutine（main 启动）、aiops `query_change_events` 工具
- **不依赖**：业务包；保持审计层的独立性

## 6. 并发与资源管理

- **无共享状态**：Usecase 仅持有不可变 repo + log
- **无锁**：所有状态在 DB
- **RunRetention 单 goroutine**：main 启动一个 goroutine 跑；不需额外同步
- **time.NewTimer 显式 Stop**：ctx 取消时停 timer 防泄漏
- **ctx 透传**：所有 IO 第一参 context

## 7. 设计模式与亮点

- **fire-and-forget**：Emit 返回 void；写入失败 Warn 不阻塞 —— 审计层绝不拖累业务路径
- **空字段丢弃 + Warn**：Action/Status 空说明调用方 bug，丢弃并记录但不返回 error
- **Payload marshal 失败存空**：宁可丢 payload 也要写入 row（核心 actor/action 不能丢）
- **ListFilters alias**：handler 仅依赖 biz/audit，不直接 import data/audit/store —— 分层清洁
- **ListChanges 有意含失败动作**：根因分析视角 —— 失败尝试本身是线索
- **RunRetention 简单算术**：03:00 + 24h 循环；注释明示"不需要 cron lib for once-a-day"
- **retentionDays<=0 禁用 sweep**：操作员可选外部归档方案
- **ErrorMessage 截断 512**：防 MySQL column 溢出

## 8. 注意事项

- **观察可能丢失**：Insert 失败时 row 不可恢复；HLD 接受这个权衡（不阻塞业务优先）
- **调用方必须 redact secrets**：Payload 字段注释明示"caller is responsible for redacting secrets BEFORE passing in (LLM keys, passwords, tokens)"
- **ErrorMessage 截断 512**：是字节截断不是 rune 截断，中文可能截半；写入前调用方应自我截断
- **OccurredAt = UTC**：所有时间 UTC 化；ListFilters.From/To 由调用方负责
- **RunRetention 03:00 本地时间**：依赖服务器时区；跨时区部署需注意
- **ListChanges limit 默认 50**：调用方可覆盖；aiops 工具调用时显式传 limit
- **status=failure/denied 不被过滤**：ListChanges 默认包含；若需仅成功需调用方自己 List 后过滤
- **Payload 是 any**：marshal 失败时 row 仍写入（PayloadJSON 留空），不阻塞
